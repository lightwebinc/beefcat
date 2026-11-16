//go:build !unix

package home

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrLocked is a home another beefcat command is using.
var ErrLocked = errors.New("the home is in use by another beefcat command")

// Lock takes the home's lock by creating its lock file, which the release
// removes. A command that was killed leaves the file behind; remove it by
// hand once no beefcat command is running.
func Lock(dir string) (func(), error) {
	path := filepath.Join(dir, "lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("%w: %s (remove %s if no beefcat command is running)", ErrLocked, dir, path)
	}
	if err != nil {
		return nil, err
	}
	f.Close()
	return func() { _ = os.Remove(path) }, nil
}
