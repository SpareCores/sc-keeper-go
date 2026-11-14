//go:build unix && !solaris && !aix

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package snapshot_test

import (
	"context"
	"io/fs"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/SpareCores/sc-keeper-go/keepertest"
	"github.com/SpareCores/sc-keeper-go/snapshot"
)

// Recorded files are world-readable (0644), whatever the umask, so another
// user, for example in a container, can read them.
func TestRecordedFilesAreWorldReadable(t *testing.T) {
	old := syscall.Umask(0o077) // process-wide: this test must not run in parallel
	t.Cleanup(func() { syscall.Umask(old) })

	srv := keepertest.New(t)
	list, err := snapshot.LoadQueryList(queriesFile)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "snapshot")
	opts := snapshot.Options{
		Dir: out, KeeperURL: srv.URL, Sleep: noSleep, Now: func() time.Time { return time.Unix(0, 0) },
	}
	if _, err := snapshot.Fetch(context.Background(), list, opts); err != nil {
		t.Fatal(err)
	}

	files := 0
	err = filepath.WalkDir(out, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		info, err := d.Info()
		if err != nil {
			return err
		}
		if mode := info.Mode().Perm(); mode != 0o644 {
			t.Errorf("%s has mode %o, want 644", path, mode)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("no recorded files")
	}
}
