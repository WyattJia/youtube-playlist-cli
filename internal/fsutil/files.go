package fsutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

var ErrLocked = errors.New("另一个进程正在使用此状态")

// AtomicWrite replaces path with a complete file. The temporary file's name is
// independent of the destination's length and its permissions apply from creation.
func AtomicWrite(path string, data []byte, mode fs.FileMode) (err error) {
	dir := filepath.Dir(path)
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Errorf("生成临时文件名: %w", err)
	}
	temp := filepath.Join(dir, ".ytpl-tmp-"+hex.EncodeToString(random[:]))
	f, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("创建临时文件: %w", err)
	}
	renamed := false
	defer func() {
		if !renamed {
			if cleanupErr := os.Remove(temp); cleanupErr != nil && !errors.Is(cleanupErr, fs.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("删除临时文件: %w", cleanupErr))
			}
		}
	}()

	_, writeErr := f.Write(data)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return fmt.Errorf("写入临时文件: %w", err)
	}
	if err := os.Rename(temp, path); err != nil {
		return fmt.Errorf("替换文件: %w", err)
	}
	renamed = true

	parent, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("打开文件所在目录以同步: %w", err)
	}
	if err := errors.Join(parent.Sync(), parent.Close()); err != nil {
		return fmt.Errorf("同步文件所在目录: %w", err)
	}
	return nil
}

func WriteJSON(path string, value any, mode fs.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("编码 JSON: %w", err)
	}
	return AtomicWrite(path, append(data, '\n'), mode)
}

// WithFileLock tries once. Leave the lock file in place so all contenders lock
// the same inode. Closing the descriptor releases the lock, including on crash.
func WithFileLock(path string, fn func() error) error {
	return withFileLock(nil, path, fn)
}

// WithFileLockContext waits for a short-lived lock until ctx expires.
func WithFileLockContext(ctx context.Context, path string, fn func() error) error {
	return withFileLock(ctx, path, fn)
}

func withFileLock(ctx context.Context, path string, fn func() error) (err error) {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("打开锁文件: %w", err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭锁文件: %w", closeErr))
		}
	}()

	for {
		lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if lockErr == nil {
			return fn()
		}
		if !errors.Is(lockErr, syscall.EWOULDBLOCK) && !errors.Is(lockErr, syscall.EINTR) {
			return fmt.Errorf("锁定状态文件: %w", lockErr)
		}
		if ctx == nil {
			return fmt.Errorf("%w (%s)", ErrLocked, filepath.Base(path))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (%s): %w", ErrLocked, filepath.Base(path), ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
