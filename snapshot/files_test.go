/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package snapshot_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/SpareCores/sc-keeper-go/keepertest"
	"github.com/SpareCores/sc-keeper-go/snapshot"
)

const (
	testKey      = "sc-test-key-0123456789abcdef"
	catalogHash  = "40ecd194b481b86a3237f51c9be6f590abcd53df"
	catalogQuery = "../keepertest/testdata/catalog-queries.yaml"
)

// keyedServer is the catalog recording behind an API key. Its responses carry
// headers that echo the request's Authorization, to show the recorder keeps
// only what it allows; it counts the requests that reached it.
type keyedServer struct {
	*httptest.Server
	inner *keepertest.Server
	mu    sync.Mutex
	paths []string
	// mutate, if set, may change a response of the inner server.
	mutate func(path string, w http.ResponseWriter, resp *http.Response, body []byte) bool
}

func newKeyedServer(t *testing.T) *keyedServer {
	t.Helper()
	k := &keyedServer{inner: keepertest.NewCatalog(t)}
	k.inner.RequireAPIKey(testKey)
	k.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		k.paths = append(k.paths, r.URL.Path)
		k.mu.Unlock()
		rec := httptest.NewRecorder()
		k.inner.ServeHTTP(rec, r)
		resp := rec.Result()
		body := rec.Body.Bytes()
		if k.mutate != nil && k.mutate(r.URL.Path, w, resp, body) {
			return
		}
		maps.Copy(w.Header(), resp.Header)
		w.Header().Set("Authorization", r.Header.Get("Authorization"))
		w.Header().Set("X-Echo-Authorization", r.Header.Get("Authorization"))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
	}))
	t.Cleanup(k.Close)
	return k
}

func (k *keyedServer) requests() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.paths)
}

func loadCatalogList(t *testing.T) *snapshot.QueryList {
	t.Helper()
	list, err := snapshot.LoadQueryList(catalogQuery)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func readTree(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		out[filepath.ToSlash(rel)], err = os.ReadFile(path)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func fetchCatalog(t *testing.T, k *keyedServer, key string) (string, error) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out")
	_, err := snapshot.Fetch(context.Background(), loadCatalogList(t), snapshot.Options{
		Dir: out, KeeperURL: k.URL, APIKey: key, Sleep: noSleep, Now: func() time.Time { return time.Unix(0, 0) },
	})
	return out, err
}

// Recording the committed recording's query list from a server that serves it
// gives the recording back: the same files, byte for byte, and the same
// "snapshot" block; and keepertest serves the new copy again.
func TestRecordSnapshotRoundTrip(t *testing.T) {
	k := newKeyedServer(t)
	out, err := fetchCatalog(t, k, testKey)
	if err != nil {
		t.Fatal(err)
	}

	got, want := readTree(t, out), readTree(t, keepertest.CatalogDir())
	for name, b := range want {
		if name == "manifest.json" {
			continue
		}
		if !bytes.Equal(got[name], b) {
			t.Errorf("%s differs from the committed recording", name)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok && name != "manifest.json" {
			t.Errorf("%s is not in the committed recording", name)
		}
	}
	var a, b struct {
		Keeper   any `json:"keeper"`
		Snapshot any `json:"snapshot"`
		Queries  []any
	}
	// keeper.packages and healthcheck of the committed recording are the live
	// Keeper's; the replay serves them back, so they are equal as well.
	if err := json.Unmarshal(got["manifest.json"], &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want["manifest.json"], &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.Snapshot, b.Snapshot) || !reflect.DeepEqual(a.Keeper, b.Keeper) || a.Queries == nil || len(a.Queries) != 0 {
		t.Errorf("manifest.json differs:\n%s\nwant:\n%s", got["manifest.json"], want["manifest.json"])
	}

	// The copy replays like the original.
	replay := keepertest.NewFromDir(t, out)
	c := keeper.New(replay.URL)
	h, err := c.Health(context.Background())
	if err != nil || h.DatabaseHash != catalogHash {
		t.Fatalf("Health: %+v, %v", h, err)
	}
	body, err := c.SnapshotFile(context.Background(), catalogHash, keeper.VendorTablePath("region", "hcloud"))
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := c.SnapshotManifest(context.Background(), catalogHash)
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := m.Lookup("region/hcloud")
	if err := body.Verify(entry.SHA256); err != nil {
		t.Error(err)
	}

	// healthcheck, manifest, eight files, healthcheck again.
	if n := len(k.requests()); n != 11 {
		t.Errorf("%d requests: %v", n, k.requests())
	}
}

// A recording never has the key or an Authorization header: only the
// allow-listed response headers are kept, whatever the server sends.
func TestRecordingHasNoKey(t *testing.T) {
	k := newKeyedServer(t)
	out, err := fetchCatalog(t, k, testKey)
	if err != nil {
		t.Fatal(err)
	}
	files := readTree(t, out)
	if len(files) < 10 {
		t.Fatalf("%d files", len(files))
	}
	for name, raw := range files {
		for _, content := range [][]byte{raw, ungzipped(raw)} {
			if bytes.Contains(content, []byte(testKey)) || bytes.Contains(bytes.ToLower(content), []byte("authorization")) || bytes.Contains(bytes.ToLower(content), []byte("bearer")) {
				t.Errorf("%s has the key or an Authorization header", name)
			}
		}
	}
	// The recorded headers are the allow-list and nothing else.
	var m snapshot.Manifest
	if err := json.Unmarshal(files["manifest.json"], &m); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot == nil {
		t.Fatal("no snapshot block")
	}
	for _, f := range append([]snapshot.SnapshotFile{m.Snapshot.Manifest}, m.Snapshot.Files...) {
		for name := range f.Headers {
			if name != "Content-Type" && name != "Cache-Control" && !strings.HasPrefix(name, "Sc-Data-") {
				t.Errorf("%s records the header %s", f.File, name)
			}
		}
		if f.Headers["Sc-Data-Hash"] != catalogHash {
			t.Errorf("%s: headers %v", f.File, f.Headers)
		}
	}
}

func ungzipped(raw []byte) []byte {
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	var b bytes.Buffer
	_, _ = b.ReadFrom(zr)
	return b.Bytes()
}

func TestSnapshotNeedsAKey(t *testing.T) {
	k := newKeyedServer(t)
	out, err := fetchCatalog(t, k, "")
	if !errors.Is(err, snapshot.ErrAPIKeyRequired) || err.Error() != "the snapshot routes need a Keeper API key" {
		t.Errorf("err = %v", err)
	}
	if n := len(k.requests()); n != 0 {
		t.Errorf("%d requests before the failure: %v", n, k.requests())
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("the output directory exists")
	}

	// A list without a snapshot block does not need one.
	list := &snapshot.QueryList{}
	if _, err := snapshot.Fetch(context.Background(), list, snapshot.Options{Dir: filepath.Join(t.TempDir(), "o"), KeeperURL: k.URL, Sleep: noSleep}); err != nil {
		t.Errorf("a list without a snapshot block: %v", err)
	}
}

func TestSnapshotRejectedKey(t *testing.T) {
	k := newKeyedServer(t)
	_, err := fetchCatalog(t, k, "wrong")
	var ue *keeper.UnauthorizedError
	if !errors.As(err, &ue) || !ue.KeyConfigured {
		t.Errorf("err = %v", err)
	}
	// /healthcheck is anonymous; the manifest is the one request that 401s,
	// and it is not retried (a 401 costs 11 credits).
	if got := k.requests(); len(got) != 2 || got[1] != "/snapshot/"+catalogHash {
		t.Errorf("requests = %v", got)
	}
}

func TestSnapshotFailures(t *testing.T) {
	otherGzip := func() []byte {
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		_, _ = zw.Write([]byte(`{"vendor_id":[]}`))
		_ = zw.Close()
		return b.Bytes()
	}()
	tests := []struct {
		name   string
		mutate func(path string, w http.ResponseWriter, resp *http.Response, body []byte) bool
		want   string
	}{
		{"a file of another hash", func(path string, w http.ResponseWriter, resp *http.Response, body []byte) bool {
			if !strings.HasSuffix(path, "/zone/hcloud") {
				return false
			}
			w.Header().Set("SC-Data-Hash", "2b4bd195ff4897951e174b9b59adf704a002502b")
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(body)
			return true
		}, "not of the pinned"},
		{"a file that is not the manifest's", func(path string, w http.ResponseWriter, resp *http.Response, body []byte) bool {
			if !strings.HasSuffix(path, "/vendor") {
				return false
			}
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("SC-Data-Hash", catalogHash)
			_, _ = w.Write(otherGzip)
			return true
		}, "checksum mismatch"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			k := newKeyedServer(t)
			k.mutate = tc.mutate
			out, err := fetchCatalog(t, k, testKey)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
			if _, statErr := os.Stat(out); statErr == nil {
				t.Error("a failed run left an output directory")
			}
		})
	}
}

func TestSnapshotPathsMustBeInKeepersManifest(t *testing.T) {
	k := newKeyedServer(t)
	list := loadCatalogList(t)
	list.Vendors = append(list.Vendors, snapshot.Vendor{Name: "nosuchvendor"})
	list.Snapshot.Benchmarks = append(list.Snapshot.Benchmarks, "nosuch:bench")
	_, err := snapshot.Fetch(context.Background(), list, snapshot.Options{
		Dir: filepath.Join(t.TempDir(), "o"), KeeperURL: k.URL, APIKey: testKey, Sleep: noSleep,
	})
	if err == nil || !strings.Contains(err.Error(), "region/nosuchvendor is not in Keeper's manifest") {
		t.Fatalf("err = %v", err)
	}
	// Reported together, before a single file was requested.
	if !strings.Contains(err.Error(), "server_price/nosuchvendor") {
		t.Errorf("only some of the missing tables are reported: %v", err)
	}
	if strings.Contains(err.Error(), "nosuch:bench") {
		t.Errorf("a benchmark without scores is an error: %v", err)
	}
	if got := k.requests(); len(got) != 2 {
		t.Errorf("requests = %v, want the healthcheck and the manifest", got)
	}
}

// A benchmark that has no scores at a vendor is not in Keeper's manifest: it
// is not recorded, and keepertest answers it with empty arrays.
func TestSnapshotUnknownBenchmarkIsNotRecorded(t *testing.T) {
	k := newKeyedServer(t)
	list := loadCatalogList(t)
	list.Snapshot.Benchmarks = []string{"nosuch:bench"}
	out := filepath.Join(t.TempDir(), "o")
	snap, err := snapshot.Fetch(context.Background(), list, snapshot.Options{Dir: out, KeeperURL: k.URL, APIKey: testKey, Sleep: noSleep})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range snap.Manifest.Snapshot.Files {
		if strings.HasPrefix(f.Path, "benchmark_score/") {
			t.Errorf("recorded %s", f.Path)
		}
	}
	if len(snap.Manifest.Snapshot.Files) != 7 {
		t.Errorf("%d files, want the 7 tables", len(snap.Manifest.Snapshot.Files))
	}
}

func TestSnapshotQueryListValidation(t *testing.T) {
	good := "vendors: [{name: hcloud}]\nsnapshot: {vendors: [region], global: [vendor], benchmarks: ['a:b']}\n"
	bad := map[string]string{
		"unknown table":        "vendors: [{name: a}]\nsnapshot: {vendors: [nope]}\n",
		"vendor as vendor tab": "vendors: [{name: a}]\nsnapshot: {vendors: [vendor]}\n",
		"unknown global":       "snapshot: {global: [server]}\n",
		"no vendors":           "snapshot: {vendors: [region]}\n",
		"benchmarks no vendor": "snapshot: {benchmarks: ['a:b']}\n",
		"unknown key":          "vendors: [{name: a}]\nsnapshot: {tables: [region]}\n",
		"region with queries":  "vendors: [{name: a}]\nvendorQueries: [{name: x, path: /x}]\nsnapshot: {global: [vendor]}\n",
	}
	write := func(content string) string {
		path := filepath.Join(t.TempDir(), "q.yaml")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	if _, err := snapshot.LoadQueryList(write(good)); err != nil {
		t.Errorf("a snapshot list without regions: %v", err)
	}
	if _, err := snapshot.LoadQueryList(write("snapshot: {global: [vendor, country]}\n")); err != nil {
		t.Errorf("global tables only: %v", err)
	}
	for name, content := range bad {
		if _, err := snapshot.LoadQueryList(write(content)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
