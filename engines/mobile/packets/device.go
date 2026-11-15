package packets

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sync"
)

// Device is the phone's tun: raw IP packets, one per call. WritePacket must
// be safe for concurrent use; ReadPacket is only called from Mux.Run.
type Device interface {
	// ReadPacket reads one packet into b and returns its length.
	ReadPacket(b []byte) (int, error)
	// WritePacket writes one packet. It must not keep pkt after returning.
	WritePacket(pkt []byte) error
	// Close unblocks ReadPacket.
	Close() error
}

// FileDevice is a tun file descriptor: Android's VpnService fd (plain IP
// packets) or the iOS packet tunnel's utun (each packet prefixed with a
// 4-byte protocol family, AF_INET or AF_INET6, big endian).
type FileDevice struct {
	f      *os.File
	utun   bool
	own    bool
	wbufs  sync.Pool
	closed sync.Once
}

// NewFileDevice wraps fd. utun selects the darwin 4-byte header. own: Close
// closes the fd (Android hands the fd over with detachFd); on iOS the system
// owns the utun fd and it must stay open after the engine lets go of it.
//
// A non-blocking fd (Android's default) is put on Go's poller, so reads park
// a goroutine instead of a thread.
func NewFileDevice(fd int, utun, own bool) (*FileDevice, error) {
	if fd < 0 {
		return nil, errors.New("packets: bad tun fd")
	}
	f := os.NewFile(uintptr(fd), "tun")
	if f == nil {
		return nil, errors.New("packets: bad tun fd")
	}
	return &FileDevice{f: f, utun: utun, own: own}, nil
}

// darwin's AF_INET and AF_INET6, as the utun header carries them.
const (
	afInet  = 2
	afInet6 = 30
)

func (d *FileDevice) ReadPacket(b []byte) (int, error) {
	if !d.utun {
		return d.f.Read(b)
	}
	// Read the header into the 4 bytes before the packet, then move the
	// packet down; reading straight into b avoids a second buffer.
	for {
		n, err := d.f.Read(b)
		if err != nil {
			return 0, err
		}
		if n <= 4 {
			continue
		}
		return copy(b, b[4:n]), nil
	}
}

func (d *FileDevice) WritePacket(pkt []byte) error {
	if !d.utun {
		_, err := d.f.Write(pkt)
		return err
	}
	if len(pkt) == 0 {
		return nil
	}
	bp, _ := d.wbufs.Get().(*[]byte)
	if bp == nil || cap(*bp) < len(pkt)+4 {
		b := make([]byte, len(pkt)+4, max(len(pkt)+4, 2048))
		bp = &b
	}
	b := (*bp)[:len(pkt)+4]
	fam := uint32(afInet)
	if pkt[0]>>4 == 6 {
		fam = afInet6
	}
	binary.BigEndian.PutUint32(b, fam)
	copy(b[4:], pkt)
	_, err := d.f.Write(b)
	d.wbufs.Put(bp)
	return err
}

func (d *FileDevice) Close() error {
	var err error
	d.closed.Do(func() {
		if d.own {
			err = d.f.Close()
			return
		}
		// Not ours to close: stop reading by setting a past deadline
		// (works for fds on Go's poller), and let the fd go without
		// closing it.
		_ = d.f.SetReadDeadline(aLongTimeAgo)
	})
	return err
}

// MemDevice is an in-memory Device: tests feed it packets and read what the
// engine writes. It also fits a shell that moves packets itself (iOS
// packetFlow through gomobile), though DESIGN.md recommends the fd.
type MemDevice struct {
	in     chan []byte
	out    chan []byte
	done   chan struct{}
	closed sync.Once
}

// NewMemDevice returns a device with queues of size n.
func NewMemDevice(n int) *MemDevice {
	return &MemDevice{in: make(chan []byte, n), out: make(chan []byte, n), done: make(chan struct{})}
}

// Inject queues a packet as if an app had sent it. It drops the packet when
// the queue is full, like a real tun.
func (d *MemDevice) Inject(pkt []byte) bool {
	select {
	case d.in <- append([]byte(nil), pkt...):
		return true
	case <-d.done:
		return false
	default:
		return false
	}
}

// Out delivers the packets the engine wrote (towards apps).
func (d *MemDevice) Out() <-chan []byte { return d.out }

func (d *MemDevice) ReadPacket(b []byte) (int, error) {
	select {
	case p := <-d.in:
		return copy(b, p), nil
	case <-d.done:
		return 0, io.EOF
	}
}

func (d *MemDevice) WritePacket(pkt []byte) error {
	select {
	case <-d.done:
		return io.ErrClosedPipe
	default:
	}
	select {
	case d.out <- append([]byte(nil), pkt...):
	default: // full: dropped, like a real tun
	}
	return nil
}

func (d *MemDevice) Close() error {
	d.closed.Do(func() { close(d.done) })
	return nil
}
