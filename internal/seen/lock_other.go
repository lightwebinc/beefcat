//go:build !unix

package seen

import (
	"errors"
	"fmt"
	"os"
)

// ErrLocked is a seen-output file another beefcat command is using.
var ErrLocked = errors.New("the seen-output file is in use by another beefcat command")

// lock takes the lock by creating the lock file, which the release
// removes. A command that was killed leaves the file behind; remove it by
// hand once no beefcat command is running.
func lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("%w: remove %s if no beefcat command is running", ErrLocked, path)
	}
	if err != nil {
		return nil, err
	}
	f.Close()
	return func() { _ = os.Remove(path) }, nil
}
