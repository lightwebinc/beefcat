//go:build unix

package seen

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ErrLocked is a seen-output file another beefcat command is using.
var ErrLocked = errors.New("the seen-output file is in use by another beefcat command")

// lock takes the lock file at path. The returned function releases it.
func lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // a file descriptor
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // a file descriptor
		f.Close()
	}, nil
}
