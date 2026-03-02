// Package watcher watches a directory for Unix socket creation and deletion,
// emitting Events when sockets appear or disappear. It uses kernel-level
// notifications (kqueue on Darwin, inotify on Linux) rather than polling.
package watcher

// Event is emitted when a socket is created or deleted in the watched directory.
type Event struct {
	// Name is the socket's basename within the watched directory.
	Name string
	// Exists is true when the socket was created, false when deleted.
	Exists bool
}
