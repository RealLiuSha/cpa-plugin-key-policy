package persist

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrLocked reports a data directory another plugin instance owns.
var ErrLocked = errors.New("data directory is owned by another plugin instance")

// Lock is an exclusive advisory lock on a file in the data directory. It is
// a flock held by an open file description, so it also keeps out a second
// plugin instance inside the same process, which a POSIX record lock would
// not. The kernel drops it when the process exits.
type Lock struct {
	file *os.File
}

// LockPath is the ownership lock next to the state file.
func LockPath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), "cpa-key-policy.lock")
}

// TryLock takes the lock without waiting.
func TryLock(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		return nil, err
	}
	return &Lock{file: file}, nil
}

// Release gives the lock up. Releasing a nil lock does nothing.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	return err
}
