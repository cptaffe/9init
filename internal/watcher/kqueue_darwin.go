package watcher

import (
	"fmt"
	"os"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// Watcher watches a directory for socket creation and deletion using kqueue.
type Watcher struct {
	dir  string
	kq   int
	f    *os.File // keeps the directory fd open
	done chan struct{}
	wg   sync.WaitGroup

	mu       sync.Mutex
	existing map[string]bool // sockets present at last scan

	events chan Event
}

// New creates a Watcher for dir and starts the background watch loop.
// Call Close to stop it.
func New(dir string) (*Watcher, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, fmt.Errorf("kqueue: %w", err)
	}

	f, err := os.Open(dir)
	if err != nil {
		unix.Close(kq)
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}

	// Register EVFILT_VNODE | NOTE_WRITE on the directory fd.
	// EV_CLEAR resets the event after each delivery so we get one event
	// per directory modification rather than a level-triggered flood.
	change := unix.Kevent_t{
		Ident:  uint64(f.Fd()),
		Filter: unix.EVFILT_VNODE,
		Flags:  unix.EV_ADD | unix.EV_CLEAR,
		Fflags: unix.NOTE_WRITE,
	}
	if _, err := unix.Kevent(kq, []unix.Kevent_t{change}, nil, nil); err != nil {
		f.Close()
		unix.Close(kq)
		return nil, fmt.Errorf("kevent register: %w", err)
	}

	w := &Watcher{
		dir:      dir,
		kq:       kq,
		f:        f,
		done:     make(chan struct{}),
		events:   make(chan Event, 64),
		existing: scanSockets(dir),
	}

	w.wg.Add(1)
	go w.loop()
	return w, nil
}

// Events returns the channel on which socket events are delivered.
// The channel is closed when the Watcher is closed.
func (w *Watcher) Events() <-chan Event {
	return w.events
}

// Snapshot returns the set of socket names currently present in the directory.
func (w *Watcher) Snapshot() map[string]bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]bool, len(w.existing))
	for k, v := range w.existing {
		out[k] = v
	}
	return out
}

// Close stops the watcher. It is safe to call more than once.
func (w *Watcher) Close() {
	select {
	case <-w.done:
	default:
		close(w.done)
	}
	// Closing the kqueue fd unblocks the kevent(2) call in the loop.
	unix.Close(w.kq)
	w.wg.Wait()
	w.f.Close()
	close(w.events)
}

func (w *Watcher) loop() {
	defer w.wg.Done()

	buf := make([]unix.Kevent_t, 8)
	// 1-second timeout lets us poll `done` regularly even if no events arrive.
	timeout := unix.Timespec{Sec: 1}

	for {
		select {
		case <-w.done:
			return
		default:
		}

		n, err := unix.Kevent(w.kq, nil, buf, &timeout)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			// EBADF is expected when Close() shuts down the kq.
			return
		}
		if n == 0 {
			// Timeout: no directory change, but check `done` and continue.
			continue
		}

		// At least one directory-change event arrived. Diff the current
		// socket set against the previous snapshot.
		w.mu.Lock()
		current := scanSockets(w.dir)
		for name := range current {
			if !w.existing[name] {
				select {
				case w.events <- Event{Name: name, Exists: true}:
				default:
				}
			}
		}
		for name := range w.existing {
			if !current[name] {
				select {
				case w.events <- Event{Name: name, Exists: false}:
				default:
				}
			}
		}
		w.existing = current
		w.mu.Unlock()
	}
}

// scanSockets returns the set of socket basenames currently in dir.
func scanSockets(dir string) map[string]bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return map[string]bool{}
	}
	sockets := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.Type()&os.ModeSocket != 0 {
			sockets[e.Name()] = true
		}
	}
	return sockets
}
