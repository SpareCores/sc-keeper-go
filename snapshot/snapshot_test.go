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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SpareCores/sc-keeper-go/keepertest"
	"github.com/SpareCores/sc-keeper-go/snapshot"
)

var (
	queriesFile = filepath.Join("..", "keepertest", "testdata", "queries.yaml")
	// fetched_at and keeper_url differ by construction (the replay server).
	volatile = regexp.MustCompile(`"(fetched_at|keeper_url)": "[^"]*"`)
)

func noSleep(context.Context, time.Duration) error { return nil }

func gunzip(t *testing.T, raw []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The committed snapshot was written by the Python fetcher of
// sc-karpenter-nodepool. Fetching the same data (replayed by keepertest) with
// the committed query list must reproduce its manifest byte for byte (except
// fetched_at and keeper_url) and the content of every file: the same query set, file names,
// parameters, row counts and SHA-256 sums.
func TestFetchReproducesTheCommittedSnapshot(t *testing.T) {
	srv := keepertest.New(t)
	list, err := snapshot.LoadQueryList(queriesFile)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := snapshot.Fetch(context.Background(), list, snapshot.Options{
		KeeperURL: srv.URL, Sleep: noSleep, Now: func() time.Time { return time.Unix(0, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "snapshot")
	if err := snap.Write(out); err != nil {
		t.Fatal(err)
	}
	committed := keepertest.SnapshotDir()
	want, err := os.ReadFile(filepath.Join(committed, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if a, b := volatile.ReplaceAllString(string(got), ""), volatile.ReplaceAllString(string(want), ""); a != b {
		t.Errorf("manifest differs from the committed one:\n%s\nwant:\n%s", a, b)
	}

	m, err := keepertest.LoadManifest(committed)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Files) != len(m.Queries) {
		t.Errorf("%d files for %d committed queries", len(snap.Files), len(m.Queries))
	}
	for _, q := range m.Queries {
		raw, err := os.ReadFile(filepath.Join(committed, q.File))
		if err != nil {
			t.Fatal(err)
		}
		regenerated, err := os.ReadFile(filepath.Join(out, q.File))
		if err != nil {
			t.Errorf("%s not written: %v", q.File, err)
			continue
		}
		if !bytes.Equal(gunzip(t, raw), gunzip(t, regenerated)) {
			t.Errorf("%s: content differs from the committed file", q.File)
		}
	}

	// Identical data gives identical bytes.
	again, err := snapshot.Fetch(context.Background(), list, snapshot.Options{
		KeeperURL: srv.URL, Sleep: noSleep, Now: func() time.Time { return time.Unix(0, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range snap.Files {
		if !bytes.Equal(content, again.Files[name]) {
			t.Errorf("%s: two fetches of the same data differ", name)
		}
	}
}

func TestWriteReplacesTheDirectory(t *testing.T) {
	srv := keepertest.New(t)
	list := &snapshot.QueryList{Queries: []snapshot.Entry{{Name: "tables/region", Path: "/table/region"}}}
	snap, err := snapshot.Fetch(context.Background(), list, snapshot.Options{KeeperURL: srv.URL, Sleep: noSleep})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "snap")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stale"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := snap.Write(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "stale")); err == nil {
		t.Error("stale file kept")
	}
	// The result is a snapshot keepertest can replay.
	replay := keepertest.NewFromDir(t, dir)
	if rows := snap.Manifest.Queries[0].Rows; rows == nil || *rows < 100 {
		t.Errorf("rows = %v", rows)
	}
	if replay.Manifest().Keeper.DatabaseHash != snap.Manifest.Keeper.DatabaseHash {
		t.Error("hash lost")
	}
}

func TestFetchFailsWhenTheDatabaseChanges(t *testing.T) {
	srv := keepertest.New(t)
	var health atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthcheck" && health.Add(1) > 1 {
			_, _ = w.Write([]byte(`{"database_hash": "other", "database_last_updated": 1}`))
			return
		}
		srv.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	list := &snapshot.QueryList{Queries: []snapshot.Entry{{Name: "tables/zone", Path: "/table/zone"}}}
	_, err := snapshot.Fetch(context.Background(), list, snapshot.Options{KeeperURL: proxy.URL, Sleep: noSleep})
	if err == nil || !strings.Contains(err.Error(), "changed during the fetch") {
		t.Errorf("err = %v", err)
	}
}

func TestFetchRetriesAndReportsErrors(t *testing.T) {
	srv := keepertest.New(t)
	var calls atomic.Int32
	var slept []time.Duration
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/table/zone" && calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		if r.URL.Path == "/table/region" {
			http.Error(w, "nope", http.StatusForbidden)
			return
		}
		srv.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	opts := snapshot.Options{KeeperURL: proxy.URL, Sleep: func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}}
	list := &snapshot.QueryList{Queries: []snapshot.Entry{{Name: "tables/zone", Path: "/table/zone"}}}
	if _, err := snapshot.Fetch(context.Background(), list, opts); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 1 || slept[0] != 7*time.Second {
		t.Errorf("slept %v, want the Retry-After of 7s", slept)
	}
	// A non-retryable status fails at once.
	list = &snapshot.QueryList{Queries: []snapshot.Entry{{Name: "tables/region", Path: "/table/region"}}}
	if _, err := snapshot.Fetch(context.Background(), list, opts); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("err = %v", err)
	}
}

func TestFetchExpansionErrors(t *testing.T) {
	srv := keepertest.New(t)
	opts := snapshot.Options{KeeperURL: srv.URL, Sleep: noSleep}
	servers := snapshot.Entry{Name: "{vendor}/s", Path: "/servers", Params: map[string]any{"vendor": "{vendor}"}}
	_, err := snapshot.Fetch(context.Background(), &snapshot.QueryList{
		Vendors:       []snapshot.Vendor{{Name: "aws", Region: "mars-1"}},
		VendorQueries: []snapshot.Entry{servers},
	}, opts)
	if err == nil || !strings.Contains(err.Error(), "region mars-1 of aws not found") {
		t.Errorf("unknown region: %v", err)
	}
	bench := snapshot.Entry{Name: "{vendor}/b-{benchmark_slug}", Path: "/servers", Benchmarks: true}
	_, err = snapshot.Fetch(context.Background(), &snapshot.QueryList{
		Vendors:       []snapshot.Vendor{{Name: "aws", Region: "us-east-1"}},
		Benchmarks:    []snapshot.Benchmark{{ID: "geekbench:score", Config: map[string]any{"cores": "nope"}}},
		VendorQueries: []snapshot.Entry{bench},
	}, opts)
	if err == nil || !strings.Contains(err.Error(), "not in /benchmark_configs") {
		t.Errorf("unknown benchmark config: %v", err)
	}
	// Two entries writing one file are refused.
	dup := snapshot.Entry{Name: "a", Path: "/table/zone"}
	_, err = snapshot.Fetch(context.Background(), &snapshot.QueryList{Queries: []snapshot.Entry{dup, dup}}, opts)
	if err == nil || !strings.Contains(err.Error(), "two queries write a.json.gz") {
		t.Errorf("duplicate names: %v", err)
	}
}

func TestSpotVendorsOnly(t *testing.T) {
	srv := keepertest.New(t)
	list := &snapshot.QueryList{
		Vendors: []snapshot.Vendor{{Name: "aws", Region: "us-east-1", Spot: true}, {Name: "hcloud", Region: "fsn1"}},
		VendorQueries: []snapshot.Entry{{
			Name: "{vendor}/servers-spot", Path: "/servers", RequiresSpot: true,
			Params: map[string]any{"vendor": "{vendor}", "vendor_regions": "{vendor}~{region_id}", "best_price_allocation": "SPOT_ONLY"},
		}},
	}
	snap, err := snapshot.Fetch(context.Background(), list, snapshot.Options{KeeperURL: srv.URL, Sleep: noSleep})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Manifest.Queries) != 1 || snap.Manifest.Queries[0].Name != "aws/servers-spot" {
		t.Errorf("queries: %+v", snap.Manifest.Queries)
	}
	if !snap.Manifest.Vendors["aws"].Spot || snap.Manifest.Vendors["hcloud"].Spot {
		t.Errorf("vendors: %+v", snap.Manifest.Vendors)
	}
}

func TestLoadQueryList(t *testing.T) {
	list, err := snapshot.LoadQueryList(queriesFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Vendors) != 8 || list.Vendors[0].Name != "aws" || len(list.Benchmarks) != 5 || len(list.VendorQueries) != 3 {
		t.Errorf("unexpected list: %+v", list)
	}
	// Whole numbers stay integers, so that limit: -1 is -1 in the manifest.
	if got := list.VendorQueries[0].Params["limit"]; got != int64(-1) {
		t.Errorf("limit = %#v", got)
	}

	bad := map[string]string{
		"unknown field":         "queries: [{name: a, path: /x, nope: 1}]",
		"no path":               "queries: [{name: a}]",
		"vendor expansion":      "queries: [{name: a, path: /x, requiresSpot: true}]",
		"no vendors":            "vendorQueries: [{name: a, path: /x}]",
		"duplicate vendor":      "vendors: [{name: a, region: r}, {name: a, region: r}]",
		"unused benchmarks":     "benchmarks: [{id: b}]",
		"vendor without region": "vendors: [{name: a}]",
	}
	for name, content := range bad {
		path := filepath.Join(t.TempDir(), "q.yaml")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := snapshot.LoadQueryList(path); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := snapshot.LoadQueryList(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestServerPricesAreStoredFlat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthcheck" {
			_, _ = w.Write([]byte(`{"database_hash": "h", "database_last_updated": 1}`))
			return
		}
		_, _ = w.Write([]byte(`[{"vendor_id":"aws","zone_id":"z","operating_system":"Linux","price":0.25,"price_tiered":[],"server":{"big":"x"},"zone":{"a":1}}]`))
	}))
	defer srv.Close()
	list := &snapshot.QueryList{Queries: []snapshot.Entry{{Name: "p", Path: "/server_prices", Params: map[string]any{"vendor": "aws"}}}}
	snap, err := snapshot.Fetch(context.Background(), list, snapshot.Options{KeeperURL: srv.URL, Sleep: noSleep})
	if err != nil {
		t.Fatal(err)
	}
	q := snap.Manifest.Queries[0]
	got := string(gunzip(t, snap.Files[q.File]))
	if got != `[{"operating_system":"Linux","price":0.25,"vendor_id":"aws","zone_id":"z"}]` || q.Projection != "flat" || len(q.Fields) == 0 {
		t.Errorf("stored %s, projection %q %v", got, q.Projection, q.Fields)
	}
}
