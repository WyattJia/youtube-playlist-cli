package fsutil

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestAtomicWriteLongFilenameAndPrivateMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, strings.Repeat("歌", 82)+".json")
	if err := AtomicWrite(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "new" {
		t.Fatalf("file = %q, %v", got, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("permissions = %o", got)
	}
	directoryTarget := filepath.Join(dir, "directory")
	if err := os.Mkdir(directoryTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(directoryTarget, []byte("data"), 0o600); err == nil {
		t.Fatal("write unexpectedly replaced a directory")
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("unexpected leftover temporary files: %v", files)
	}
}

func TestFlockConflictAndKernelReleaseOnClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := WithFileLock(path, func() error {
		t.Error("competing callback ran")
		return nil
	}); !errors.Is(err, ErrLocked) {
		t.Fatalf("competing lock = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := WithFileLockContext(ctx, path, func() error {
		t.Error("competing callback ran")
		return nil
	}); !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrLocked) {
		t.Fatalf("lock wait = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// The lock file still exists. Closing the descriptor released the flock.
	if err := WithFileLock(path, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}
