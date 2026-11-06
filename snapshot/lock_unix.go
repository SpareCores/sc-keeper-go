//go:build unix && !solaris && !aix

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package snapshot

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// lockDir takes an exclusive lock for the output directory (a flock on
// <dir>.lock, released by the kernel when the process dies, so a crash never
// leaves a stale lock) and fails at once if another run holds it.
func lockDir(dir string) (unlock func(), err error) {
	path := dir + ".lock"
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // next to the output directory chosen by the user
	if err != nil {
		return nil, fmt.Errorf("locking %s: %w", dir, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // a file descriptor fits an int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another sc-snapshot run is writing %s (lock %s)", dir, path)
		}
		return nil, fmt.Errorf("locking %s: %w", dir, err)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil //nolint:gosec // as above
}
