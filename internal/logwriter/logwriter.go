// Package logwriter provides an io.Writer that simultaneously:
//   - writes to a size-rotating log file on disk,
//   - maintains an in-memory ring buffer of recent output for fast tail reads,
//   - broadcasts new data to active streaming subscribers via a context-aware
//     channel mechanism (compatible with srv9p's flush/cancel semantics).
package logwriter

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	defaultRingCap  = 8 * 1024        // 8 KiB in-memory tail
	defaultMaxSize  = 10 * 1024 * 1024 // 10 MiB per log file before rotation
	defaultMaxFiles = 5
)

// Writer is an io.Writer that fans writes to disk and an in-memory ring buffer.
// It is safe for concurrent use.
type Writer struct {
	path     string
	maxSize  int64
	maxFiles int
	ringCap  int

	mu sync.Mutex

	// Disk file state.
	f    *os.File
	size int64

	// Ring buffer: fixed-capacity circular byte buffer.
	// head is the index of the oldest valid byte; used is the count of
	// valid bytes. When used == ringCap, the oldest byte is overwritten.
	ring []byte
	head int
	used int

	// total is the count of bytes ever written to the ring (never decrements).
	// Subscribers use this to track their read position.
	total int64

	// notify is closed and replaced each time new data is written.
	// Subscribers select on it to wake without holding the lock.
	notify chan struct{}

	closed bool
}

// New creates a Writer that writes to path with the given rotation parameters.
// The directory for path is created automatically.
func New(path string, maxSize int64, maxFiles int) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("logwriter mkdir: %w", err)
	}
	f, err := openAppend(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	w := &Writer{
		path:     path,
		maxSize:  maxSize,
		maxFiles: maxFiles,
		ringCap:  defaultRingCap,
		f:        f,
		size:     info.Size(),
		ring:     make([]byte, defaultRingCap),
		notify:   make(chan struct{}),
	}
	return w, nil
}

// Write implements io.Writer. It writes p to the disk file and ring buffer,
// rotating the file if necessary, and wakes all waiting subscribers.
func (w *Writer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	if err := w.writeFile(p); err != nil {
		w.mu.Unlock()
		return 0, err
	}
	w.appendRing(p)
	// Swap the notify channel: close the old one (waking subscribers),
	// replace it with a fresh one for the next write.
	old := w.notify
	w.notify = make(chan struct{})
	w.mu.Unlock()

	close(old)
	return len(p), nil
}

// Tail returns a copy of the current ring buffer contents in order
// (oldest byte first). Safe to call concurrently.
func (w *Writer) Tail() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ringSnapshot()
}

// Subscribe returns a Subscription positioned at the start of the current
// ring buffer tail. Use Read on the Subscription to stream new data;
// reads block until data arrives, the context is cancelled, or Close is called.
func (w *Writer) Subscribe() *Subscription {
	w.mu.Lock()
	startAt := w.total - int64(w.used)
	if startAt < 0 {
		startAt = 0
	}
	w.mu.Unlock()
	return &Subscription{w: w, pos: startAt}
}

// Close flushes, closes the underlying file, and unblocks all subscribers
// (who will receive io.EOF after draining buffered data).
func (w *Writer) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	old := w.notify
	w.notify = make(chan struct{})
	w.mu.Unlock()
	close(old)
	return w.f.Close()
}

// ---- internal helpers (must be called with mu held) ----

func (w *Writer) writeFile(p []byte) error {
	if w.size+int64(len(p)) > w.maxSize {
		if err := w.rotate(); err != nil {
			return err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return err
}

func (w *Writer) rotate() error {
	w.f.Close()
	for i := w.maxFiles - 1; i >= 1; i-- {
		old := fmt.Sprintf("%s.%d", w.path, i)
		newp := fmt.Sprintf("%s.%d", w.path, i+1)
		os.Rename(old, newp) //nolint:errcheck
	}
	os.Rename(w.path, w.path+".1") //nolint:errcheck
	var err error
	w.f, err = openAppend(w.path)
	if err != nil {
		return fmt.Errorf("rotate: reopen %s: %w", w.path, err)
	}
	w.size = 0
	return nil
}

func (w *Writer) appendRing(p []byte) {
	if len(p) > w.ringCap {
		p = p[len(p)-w.ringCap:]
	}
	for _, b := range p {
		pos := (w.head + w.used) % w.ringCap
		w.ring[pos] = b
		if w.used < w.ringCap {
			w.used++
		} else {
			w.head = (w.head + 1) % w.ringCap
		}
	}
	w.total += int64(len(p))
}

func (w *Writer) ringSnapshot() []byte {
	if w.used == 0 {
		return nil
	}
	out := make([]byte, w.used)
	for i := 0; i < w.used; i++ {
		out[i] = w.ring[(w.head+i)%w.ringCap]
	}
	return out
}

func openAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// Subscription is a streaming reader over a Writer's output.
// Reads block until data arrives, the context is cancelled, or the Writer
// is closed. Each Subscription is not safe for concurrent use.
type Subscription struct {
	w   *Writer
	pos int64 // total bytes consumed from the writer
}

// Read fills p with the next available bytes.
// It blocks if no data is available, returning when:
//   - data arrives,
//   - ctx is cancelled (returns ctx.Err()),
//   - the Writer is closed and all buffered data is consumed (returns io.EOF).
func (s *Subscription) Read(ctx context.Context, p []byte) (int, error) {
	w := s.w
	for {
		w.mu.Lock()
		oldest := w.total - int64(w.used)
		if s.pos < oldest {
			s.pos = oldest // ring overwrote data we hadn't read; skip ahead
		}
		available := w.total - s.pos
		if available > 0 {
			n := int(available)
			if n > len(p) {
				n = len(p)
			}
			// Copy from the ring starting at s.pos.
			ringOff := int(s.pos-oldest) + w.head
			for i := 0; i < n; i++ {
				p[i] = w.ring[(ringOff+i)%w.ringCap]
			}
			s.pos += int64(n)
			w.mu.Unlock()
			return n, nil
		}
		if w.closed {
			w.mu.Unlock()
			return 0, io.EOF
		}
		// No data yet; grab the current notify channel before releasing the lock.
		notifyCh := w.notify
		w.mu.Unlock()

		// Wait for new data, context cancellation, or writer close.
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-notifyCh:
			// New data written (or writer closed); loop to check.
		}
	}
}

// Close releases the subscription. It does not close the underlying Writer.
func (s *Subscription) Close() {}

// Timestamper wraps an io.Writer and prefixes each output line with the
// current time in the format "2006/01/02 15:04:05 ". It is not safe for
// concurrent use; the supervisor creates one per process and uses it only
// from the process's stdout/stderr goroutines.
type Timestamper struct {
	w   io.Writer
	buf []byte // bytes of an incomplete line not yet written
}

// NewTimestamper returns a Timestamper that writes timestamped lines to w.
func NewTimestamper(w io.Writer) *Timestamper {
	return &Timestamper{w: w}
}

// Write buffers p and flushes each complete line to the underlying writer
// with a timestamp prefix.
func (t *Timestamper) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			t.buf = append(t.buf, p...)
			break
		}
		t.buf = append(t.buf, p[:i+1]...)
		if err := t.flush(); err != nil {
			return 0, err
		}
		p = p[i+1:]
	}
	return total, nil
}

// Flush writes any buffered partial line (without a trailing newline) to the
// underlying writer. Call it when the process exits to avoid losing the last
// line of output.
func (t *Timestamper) Flush() error {
	if len(t.buf) == 0 {
		return nil
	}
	t.buf = append(t.buf, '\n')
	return t.flush()
}

func (t *Timestamper) flush() error {
	ts := time.Now().Format("2006/01/02 15:04:05 ")
	if _, err := io.WriteString(t.w, ts); err != nil {
		t.buf = t.buf[:0]
		return err
	}
	_, err := t.w.Write(t.buf)
	t.buf = t.buf[:0]
	return err
}
