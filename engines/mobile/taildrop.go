package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FileTarget is a device on a tailnet that can be sent files (Taildrop).
type FileTarget struct {
	Name    string `json:"name"`
	DNSName string `json:"dns_name"`
	// ID is the device's stable id; pass it back as send_file's target.
	ID string `json:"id"`
}

// WaitingFile is a file another device sent to this one, waiting to be saved.
type WaitingFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// FileNode is a Node that can send and receive Taildrop files. The real node
// can (when the engine is built with Taildrop, see scripts/build-engine.sh);
// fakes implement it for tests.
type FileNode interface {
	FileTargets(ctx context.Context) ([]FileTarget, error)
	SendFile(ctx context.Context, target, name string, size int64, r io.Reader) error
	WaitingFiles(ctx context.Context) ([]WaitingFile, error)
	// ReceiveFile streams a waiting file to w.
	ReceiveFile(ctx context.Context, name string, w io.Writer) error
	DeleteWaitingFile(ctx context.Context, name string) error
}

// fileNodeFor is the running node of a connected tailnet, as a FileNode.
func (e *Engine) fileNodeFor(name string) (FileNode, error) {
	e.mu.RLock()
	up := e.up
	_, exists := e.cfg.Profiles[name]
	var node Node
	running := false
	if ne := e.nodes[name]; ne != nil {
		node, running = ne.node, ne.status.State == StateRunning
	}
	e.mu.RUnlock()
	if !up {
		return nil, errOff
	}
	if !exists {
		return nil, fmt.Errorf("there is no tailnet called %q", name)
	}
	if node == nil || !running {
		return nil, fmt.Errorf("tailnet %q is not connected", name)
	}
	fn, ok := node.(FileNode)
	if !ok {
		return nil, errors.New("file sending is not available on this tailnet")
	}
	return fn, nil
}

// FileTargets lists the devices on a tailnet that can receive files from this one.
func (e *Engine) FileTargets(name string) ([]FileTarget, error) {
	fn, err := e.fileNodeFor(name)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t, err := fn.FileTargets(ctx)
	if t == nil {
		t = []FileTarget{}
	}
	return t, err
}

// SendFile sends the file at path (in the app's own storage) to a device.
func (e *Engine) SendFile(name, target, path string) error {
	if strings.TrimSpace(target) == "" {
		return errors.New("bad arguments: target is required")
	}
	fn, err := e.fileNodeFor(name)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("could not open the file: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.IsDir() {
		return errors.New("that is a folder; pick a file")
	}
	// A large file takes a while; bound it by the amount, not a fixed minute.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute+time.Duration(st.Size()/(64<<10))*time.Second)
	defer cancel()
	return fn.SendFile(ctx, target, filepath.Base(path), st.Size(), f)
}

// WaitingFiles lists the files other devices sent to this one.
func (e *Engine) WaitingFiles(name string) ([]WaitingFile, error) {
	fn, err := e.fileNodeFor(name)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w, err := fn.WaitingFiles(ctx)
	if w == nil {
		w = []WaitingFile{}
	}
	return w, err
}

// ReceiveFile saves a waiting file into destDir (never overwriting: "a.txt"
// becomes "a 2.txt"), then removes it from the waiting list. It returns the
// path it wrote.
func (e *Engine) ReceiveFile(name, file, destDir string) (string, error) {
	base := filepath.Base(strings.TrimSpace(file))
	if base == "." || base == string(filepath.Separator) || base == "" {
		return "", errors.New("bad arguments: file is required")
	}
	if destDir == "" {
		return "", errors.New("bad arguments: dest_dir is required")
	}
	fn, err := e.fileNodeFor(name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return "", err
	}
	out, path, err := createUnique(destDir, base)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := fn.ReceiveFile(ctx, base, out); err != nil {
		out.Close()
		os.Remove(path)
		return "", err
	}
	if err := out.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	_ = fn.DeleteWaitingFile(ctx, base) // saved; a failure here only leaves it listed
	return path, nil
}

// createUnique creates dir/base, or dir/"stem 2.ext" and so on when it exists.
func createUnique(dir, base string) (*os.File, string, error) {
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for n := 1; n < 1000; n++ {
		name := base
		if n > 1 {
			name = fmt.Sprintf("%s %d%s", stem, n, ext)
		}
		p := filepath.Join(dir, name)
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return f, p, nil
		}
		if !os.IsExist(err) {
			return nil, "", err
		}
	}
	return nil, "", errors.New("too many files with that name")
}
