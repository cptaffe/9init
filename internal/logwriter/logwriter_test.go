package logwriter

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"
)

func newTestWriter(t *testing.T) *Writer {
	t.Helper()
	w, err := New(filepath.Join(t.TempDir(), "test.log"), 1024, 3)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	return w
}

func TestWriteAndTail(t *testing.T) {
	w := newTestWriter(t)
	w.Write([]byte("hello "))  //nolint:errcheck
	w.Write([]byte("world\n")) //nolint:errcheck

	tail := w.Tail()
	if string(tail) != "hello world\n" {
		t.Errorf("Tail = %q, want %q", tail, "hello world\n")
	}
}

func TestSubscriptionReceivesExistingTail(t *testing.T) {
	w := newTestWriter(t)
	w.Write([]byte("existing\n")) //nolint:errcheck

	sub := w.Subscribe()
	defer sub.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	buf := make([]byte, 64)
	n, err := sub.Read(ctx, buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf[:n]) != "existing\n" {
		t.Errorf("got %q, want %q", buf[:n], "existing\n")
	}
}

func TestSubscriptionBlocksAndReceivesNewData(t *testing.T) {
	w := newTestWriter(t)
	sub := w.Subscribe()
	defer sub.Close()

	// Write in a goroutine after a small delay.
	go func() {
		time.Sleep(50 * time.Millisecond)
		w.Write([]byte("late\n")) //nolint:errcheck
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	buf := make([]byte, 64)
	n, err := sub.Read(ctx, buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf[:n]) != "late\n" {
		t.Errorf("got %q, want %q", buf[:n], "late\n")
	}
}

func TestSubscriptionContextCancelled(t *testing.T) {
	w := newTestWriter(t)
	sub := w.Subscribe()
	defer sub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	buf := make([]byte, 64)
	_, err := sub.Read(ctx, buf)
	if err == nil {
		t.Error("expected error from cancelled context, got nil")
	}
}

func TestSubscriptionEOFOnClose(t *testing.T) {
	w := newTestWriter(t)
	sub := w.Subscribe()

	go func() {
		time.Sleep(50 * time.Millisecond)
		w.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	buf := make([]byte, 64)
	_, err := sub.Read(ctx, buf)
	if err != io.EOF {
		t.Errorf("expected io.EOF after writer close, got %v", err)
	}
}

func TestRotation(t *testing.T) {
	dir := t.TempDir()
	// Very small max size to force rotation.
	w, err := New(filepath.Join(dir, "svc.log"), 10, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// Write enough to trigger two rotations.
	for i := 0; i < 5; i++ {
		w.Write([]byte("0123456789")) //nolint:errcheck
	}

	// Current file plus up to 3 numbered files.
	for _, name := range []string{"svc.log", "svc.log.1", "svc.log.2"} {
		if _, err := filepath.Glob(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected %s to exist after rotation", name)
		}
	}
}

func TestRingOverflow(t *testing.T) {
	w := newTestWriter(t)
	w.ringCap = 5 // shrink ring for testing

	w.Write([]byte("ABCDE")) //nolint:errcheck
	w.Write([]byte("FG"))    //nolint:errcheck  overwrites AB

	tail := w.Tail()
	if string(tail) != "CDEFG" {
		t.Errorf("Tail after overflow = %q, want %q", tail, "CDEFG")
	}
}
