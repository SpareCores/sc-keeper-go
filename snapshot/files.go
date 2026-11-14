/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package snapshot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	keeper "github.com/SpareCores/sc-keeper-go"
)

// ErrAPIKeyRequired is the failure of a query list with a snapshot block run
// without an API key; nothing is requested then.
var ErrAPIKeyRequired = errors.New("the snapshot routes need a Keeper API key")

// ManifestSnapshot is the "snapshot" block of manifest.json: Keeper's
// snapshot of one database hash as far as the query list asked for it. The
// files are the gzip bytes as Keeper sent them; package keepertest serves
// them.
type ManifestSnapshot struct {
	// Hash and PublishedAt are the data version (SC-Data-Hash and
	// SC-Data-Published-At) of every file.
	Hash        string `json:"hash"`
	PublishedAt string `json:"published_at"`
	// Healthcheck is /healthcheck at the start of the run.
	Healthcheck map[string]any `json:"healthcheck"`
	// Manifest is Keeper's manifest of the hash: every file of the hash, not
	// only the recorded ones.
	Manifest SnapshotFile `json:"manifest"`
	// Files are the recorded files, in the order of the query list.
	Files []SnapshotFile `json:"files"`
}

// SnapshotFile is one recorded snapshot response.
type SnapshotFile struct {
	// Path is the route below /snapshot/{hash}/ (empty for the manifest);
	// File is the gzip file, relative to the output directory.
	Path string `json:"path,omitempty"`
	File string `json:"file"`
	// Rows and SHA256 are Keeper's manifest entry for the path: the number of
	// rows and the SHA-256 of the uncompressed JSON.
	Rows   *int   `json:"rows,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	// Headers are the response headers worth keeping: SC-Data-*,
	// Content-Type and Cache-Control. Nothing else is recorded, so that no
	// request header (the Authorization of the key) or secret can end up in
	// the recording.
	Headers map[string]string `json:"headers,omitempty"`
}

const snapshotDir = "snapshot"

type snapshotPath struct {
	path string
	// optional paths may be absent from Keeper's manifest (a benchmark
	// without scores at a vendor): they are not recorded then.
	optional bool
}

// file is the name of the recorded gzip file: colons of benchmark ids become
// underscores.
func (p snapshotPath) file() string {
	return snapshotDir + "/" + strings.ReplaceAll(p.path, ":", "_") + ".json.gz"
}

// checkSnapshotKey fails a list with a snapshot block when there is no key,
// before anything is requested.
func checkSnapshotKey(list *QueryList, key string) error {
	if !list.Snapshot.empty() && key == "" {
		return ErrAPIKeyRequired
	}
	return nil
}

// planSnapshot expands the snapshot block into the paths to fetch (none
// without a block).
func planSnapshot(list *QueryList, pin health) ([]snapshotPath, error) {
	if list.Snapshot.empty() {
		return nil, nil
	}
	if pin.DatabaseHash == "" {
		return nil, errors.New("Keeper serves no data version (database_hash is null)") //nolint:staticcheck // user-facing message
	}
	var out []snapshotPath
	files := map[string]string{}
	add := func(p snapshotPath) error {
		if other, dup := files[p.file()]; dup {
			return fmt.Errorf("snapshot: %s and %s are recorded in one file", other, p.path)
		}
		files[p.file()] = p.path
		out = append(out, p)
		return nil
	}
	for _, t := range list.Snapshot.Global {
		if err := add(snapshotPath{path: keeper.TablePath(t)}); err != nil {
			return nil, err
		}
	}
	for _, v := range list.Vendors {
		for _, t := range list.Snapshot.Vendors {
			if err := add(snapshotPath{path: keeper.VendorTablePath(t, v.Name)}); err != nil {
				return nil, err
			}
		}
		for _, b := range list.Snapshot.Benchmarks {
			if err := add(snapshotPath{path: keeper.ScorePath(v.Name, b), optional: true}); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// keepHeader says whether the recording keeps a response header (see
// SnapshotFile.Headers): SC-Data-*, Content-Type and Cache-Control.
func keepHeader(name string) bool {
	name = http.CanonicalHeaderKey(name)
	return name == "Content-Type" || name == "Cache-Control" || strings.HasPrefix(name, "Sc-Data-")
}

func keptHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for name, values := range h {
		if keepHeader(name) && len(values) > 0 {
			out[http.CanonicalHeaderKey(name)] = values[0]
		}
	}
	return out
}

// recordSnapshot fetches the planned files of the pinned hash into
// stage/snapshot and returns the manifest block. Every file is checked
// against the sha256 of Keeper's manifest, and the run fails when a response
// is of another hash than the pin.
func (f *fetcher) recordSnapshot(ctx context.Context, plan []snapshotPath, pin health, stage string) (*ManifestSnapshot, error) {
	if plan == nil {
		return nil, nil
	}
	client := keeper.New(f.opts.KeeperURL,
		keeper.WithAPIKey(f.opts.APIKey),
		keeper.WithUserAgent("sc-keeper-go-snapshot"),
		keeper.WithHTTPClient(f.client),
		keeper.WithRetries(f.opts.Retries, 5*time.Second, 2*time.Minute),
		keeper.WithSleep(f.opts.Sleep))

	hash := pin.DatabaseHash
	checkVersion := func(what string, v keeper.DataVersion) error {
		if v.Hash != hash {
			return fmt.Errorf("snapshot %s is of %q, not of the pinned %s: Keeper data changed during the fetch, run again", what, v.Hash, hash)
		}
		return nil
	}
	f.logf("fetching the snapshot manifest of %s", hash)
	mbody, err := client.SnapshotManifestBody(ctx, hash)
	if err != nil {
		return nil, err
	}
	if err := checkVersion("manifest", mbody.Version); err != nil {
		return nil, err
	}
	manifest, err := keeper.DecodeManifest(mbody)
	if err != nil {
		return nil, err
	}
	rec := &ManifestSnapshot{
		Hash: hash, PublishedAt: mbody.Version.PublishedAt.Format(time.RFC3339), Healthcheck: pin.raw,
		Manifest: SnapshotFile{File: snapshotDir + "/manifest.json.gz", Headers: keptHeaders(mbody.Header)},
	}
	if err := writeFile(stage, rec.Manifest.File, mbody.Gzip); err != nil {
		return nil, err
	}

	// A table that Keeper does not list is a mistake of the query list.
	var todo []snapshotPath
	var errs []error
	for _, p := range plan {
		if _, ok := manifest.Lookup(p.path); ok {
			todo = append(todo, p)
		} else if !p.optional {
			errs = append(errs, fmt.Errorf("snapshot: %s is not in Keeper's manifest of %s", p.path, hash))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	for i, p := range todo {
		f.logf("[%d/%d] fetching snapshot %s", i+1, len(todo), p.path)
		body, err := client.SnapshotFile(ctx, hash, p.path)
		if err != nil {
			return nil, err
		}
		if err := checkVersion(p.path, body.Version); err != nil {
			return nil, err
		}
		entry, _ := manifest.Lookup(p.path)
		if err := body.Verify(entry.SHA256); err != nil {
			return nil, fmt.Errorf("snapshot %s: %w", p.path, err)
		}
		if err := writeFile(stage, p.file(), body.Gzip); err != nil {
			return nil, err
		}
		rows := entry.Rows
		rec.Files = append(rec.Files, SnapshotFile{
			Path: p.path, File: p.file(), Rows: &rows, SHA256: entry.SHA256, Headers: keptHeaders(body.Header),
		})
		// The window of the rate limit is per minute: leave it some room.
		if rem, err := strconv.Atoi(body.Header.Get("X-Ratelimit-Remaining")); err == nil && rem < 30 {
			if err := f.opts.Sleep(ctx, 30*time.Second); err != nil {
				return nil, err
			}
		}
	}
	return rec, nil
}

func writeFile(dir, rel string, data []byte) error {
	dest := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("writing snapshot: %w", err)
	}
	if err := writeRecording(dest, data); err != nil {
		return fmt.Errorf("writing snapshot: %w", err)
	}
	return nil
}

// recordingMode is the mode of recorded files: world-readable, so the
// recordings can be read by other users (for example inside containers).
const recordingMode os.FileMode = 0o644

// openRecording creates or truncates path for writing and sets its mode to
// recordingMode regardless of the umask.
func openRecording(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, recordingMode) //nolint:gosec // recorded data, meant to be world-readable
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(recordingMode); err != nil { //nolint:gosec // as above
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// writeRecording writes data to path with mode recordingMode regardless of
// the umask.
func writeRecording(path string, data []byte) error {
	if err := os.WriteFile(path, data, recordingMode); err != nil { //nolint:gosec // recorded data, meant to be world-readable
		return err
	}
	return os.Chmod(path, recordingMode) //nolint:gosec // as above
}
