package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// runningHome starts the engine with the "home" tailnet signed in and Running.
func runningHome(t *testing.T) (*Engine, *fakeNode) {
	t.Helper()
	e, dir, fn := domainEngine(t)
	if err := e.Start(dir); err != nil {
		t.Fatal(err)
	}
	n := fn.node("home")
	n.signIn(fakeState{Suffix: "home.ts.net", Account: "a@b.example"})
	eventually(t, "home online", func() bool {
		var st State
		json.Unmarshal(mustOK(t, e, "state", nil), &st)
		for _, p := range st.Profiles {
			if p.Name == "home" {
				return p.Online
			}
		}
		return false
	})
	return e, n
}

func TestTaildropTargetsAndSend(t *testing.T) {
	e, n := runningHome(t)
	n.mu.Lock()
	n.files().targets = []FileTarget{{Name: "macbook", DNSName: "macbook.home.ts.net", ID: "nABC"}}
	n.mu.Unlock()

	var targets []FileTarget
	json.Unmarshal(mustOK(t, e, "file_targets", map[string]any{"name": "home"}), &targets)
	if !reflect.DeepEqual(targets, []FileTarget{{Name: "macbook", DNSName: "macbook.home.ts.net", ID: "nABC"}}) {
		t.Fatalf("targets %+v", targets)
	}

	src := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(src, []byte("hello taildrop"), 0o600)
	mustOK(t, e, "send_file", map[string]any{"name": "home", "target": "nABC", "path": src})
	n.mu.Lock()
	got := string(n.files().sent["nABC/notes.txt"])
	n.mu.Unlock()
	if got != "hello taildrop" {
		t.Fatalf("sent %q", got)
	}

	mustFail(t, e, "send_file", map[string]any{"name": "home", "target": "nABC", "path": filepath.Join(t.TempDir(), "missing")}, "could not open the file")
	mustFail(t, e, "send_file", map[string]any{"name": "home", "target": "nABC", "path": t.TempDir()}, "pick a file")
	mustFail(t, e, "send_file", map[string]any{"name": "home", "path": src}, "target is required")
}

func TestTaildropReceiveNeverOverwritesAndClearsTheQueue(t *testing.T) {
	e, n := runningHome(t)
	n.mu.Lock()
	n.files().waiting["pic.png"] = []byte("one")
	n.mu.Unlock()
	dest := filepath.Join(t.TempDir(), "Taildrop")

	var w []WaitingFile
	json.Unmarshal(mustOK(t, e, "waiting_files", map[string]any{"name": "home"}), &w)
	if !reflect.DeepEqual(w, []WaitingFile{{Name: "pic.png", Size: 3}}) {
		t.Fatalf("waiting %+v", w)
	}

	var r struct{ Path string }
	json.Unmarshal(mustOK(t, e, "receive_file", map[string]any{"name": "home", "file": "pic.png", "dest_dir": dest}), &r)
	if r.Path != filepath.Join(dest, "pic.png") {
		t.Fatalf("path %q", r.Path)
	}
	// The same name arrives again: the first copy is kept.
	n.mu.Lock()
	n.files().waiting["pic.png"] = []byte("two")
	n.mu.Unlock()
	json.Unmarshal(mustOK(t, e, "receive_file", map[string]any{"name": "home", "file": "pic.png", "dest_dir": dest}), &r)
	if r.Path != filepath.Join(dest, "pic 2.png") {
		t.Fatalf("second path %q", r.Path)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "pic.png")); string(b) != "one" {
		t.Fatalf("first copy overwritten: %q", b)
	}
	if b, _ := os.ReadFile(r.Path); string(b) != "two" {
		t.Fatalf("second copy: %q", b)
	}
	json.Unmarshal(mustOK(t, e, "waiting_files", map[string]any{"name": "home"}), &w)
	if len(w) != 0 {
		t.Fatalf("still waiting: %+v", w)
	}
}

func TestTaildropReceiveStaysInsideTheFolder(t *testing.T) {
	e, n := runningHome(t)
	n.mu.Lock()
	n.files().waiting["passwd"] = []byte("x")
	n.mu.Unlock()
	dest := filepath.Join(t.TempDir(), "Taildrop")
	var r struct{ Path string }
	json.Unmarshal(mustOK(t, e, "receive_file", map[string]any{"name": "home", "file": "../../etc/passwd", "dest_dir": dest}), &r)
	if filepath.Dir(r.Path) != dest {
		t.Fatalf("wrote outside the folder: %q", r.Path)
	}
}

func TestTaildropFailsClearlyWhenOffOrUnknown(t *testing.T) {
	e, _, _ := domainEngine(t)
	mustFail(t, e, "file_targets", map[string]any{"name": "home"}, "tailfuse is off")
	e2, _ := runningHome(t)
	mustFail(t, e2, "file_targets", map[string]any{"name": "nowhere"}, "there is no tailnet called")
}
