//go:build !unix || solaris || aix

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package snapshot

// lockDir does not lock on this platform: do not run two fetches into one
// directory.
func lockDir(string) (func(), error) { return func() {}, nil }
