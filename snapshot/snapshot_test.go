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
	"sync"
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
	out := filepath.Join(t.TempDir(), "snapshot")
	opts := snapshot.Options{
		Dir: out, KeeperURL: srv.URL, Sleep: noSleep, Now: func() time.Time { return time.Unix(0, 0) },
	}
	snap, err := snapshot.Fetch(context.Background(), list, opts)
	if err != nil {
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
	if len(snap.Manifest.Queries) != len(m.Queries) {
		t.Errorf("%d queries for %d committed queries", len(snap.Manifest.Queries), len(m.Queries))
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
	for _, leftover := range []string{"progress.json", out + ".tmp"} {
		if _, err := os.Stat(leftover); err == nil {
			t.Errorf("%s left behind", leftover)
		}
	}

	// Identical data gives identical bytes: a second fresh fetch, and a
	// resume into the finished output, which reuses every query.
	again := filepath.Join(t.TempDir(), "again")
	o := opts
	o.Dir = again
	if _, err := snapshot.Fetch(context.Background(), list, o); err != nil {
		t.Fatal(err)
	}
	assertSameTree(t, out, again)
	o = opts
	o.Resume = true
	resumed, err := snapshot.Fetch(context.Background(), list, o)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Reused != len(m.Queries) {
		t.Errorf("resume reused %d of %d queries", resumed.Reused, len(m.Queries))
	}
	assertSameTree(t, again, out)
}

// tree maps the slash-separated path of every file below dir to its content.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		files[filepath.ToSlash(rel)] = string(readFile(t, path))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// assertSameTree compares the file lists and the bytes of two directories.
func assertSameTree(t *testing.T, a, b string) {
	t.Helper()
	ta, tb := tree(t, a), tree(t, b)
	for name, content := range ta {
		other, ok := tb[name]
		switch {
		case !ok:
			t.Errorf("%s only in %s", name, a)
		case other != content:
			t.Errorf("%s differs", name)
		}
	}
	for name := range tb {
		if _, ok := ta[name]; !ok {
			t.Errorf("%s only in %s", name, b)
		}
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestWriteReplacesTheDirectory(t *testing.T) {
	srv := keepertest.New(t)
	list := &snapshot.QueryList{Queries: []snapshot.Entry{{Name: "tables/region", Path: "/table/region"}}}
	dir := filepath.Join(t.TempDir(), "snap")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stale"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := snapshot.Fetch(context.Background(), list, snapshot.Options{Dir: dir, KeeperURL: srv.URL, Sleep: noSleep})
	if err != nil {
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

// A run that fails midway leaves its finished queries; -resume takes them over
// and fetches only the rest, and the result equals an uninterrupted run.
func TestResume(t *testing.T) {
	srv := keepertest.New(t)
	var failZone atomic.Bool
	var hits sync.Map
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/table/zone" && failZone.Load() {
			http.Error(w, "boom", http.StatusForbidden)
			return
		}
		n, _ := hits.LoadOrStore(r.URL.Path, new(atomic.Int32))
		n.(*atomic.Int32).Add(1)
		srv.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	base := map[string]int32{}
	count := func(path string) int32 {
		if n, ok := hits.Load(path); ok {
			return n.(*atomic.Int32).Load() - base[path]
		}
		return 0
	}
	list := &snapshot.QueryList{Queries: []snapshot.Entry{
		{Name: "benchmark_configs", Path: "/benchmark_configs"},
		{Name: "tables/zone", Path: "/table/zone"},
	}}
	dir := filepath.Join(t.TempDir(), "snap")
	opts := snapshot.Options{
		Dir: dir, KeeperURL: proxy.URL, Sleep: noSleep, Resume: true, Now: func() time.Time { return time.Unix(0, 0) },
	}
	// The reference: one uninterrupted run.
	uninterrupted := filepath.Join(t.TempDir(), "whole")
	whole := opts
	whole.Dir, whole.Resume = uninterrupted, false
	if _, err := snapshot.Fetch(context.Background(), list, whole); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/benchmark_configs", "/table/zone"} {
		base[path] = count(path)
	}
	failZone.Store(true)
	if _, err := snapshot.Fetch(context.Background(), list, opts); err == nil {
		t.Fatal("expected the zone query to fail")
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("a failed run replaced the output directory")
	}
	failZone.Store(false)
	// Leftovers of killed runs and of removed queries must not reach the output.
	stage := dir + ".tmp"
	for _, name := range []string{"tables/zone.json.gz.part", "removed.json.gz", "old/dir/removed.json.gz"} {
		path := filepath.Join(stage, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("junk"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := snapshot.Fetch(context.Background(), list, opts)
	if err != nil {
		t.Fatal(err)
	}
	assertSameTree(t, uninterrupted, dir)
	if snap.Reused != 1 || count("/benchmark_configs") != 1 {
		t.Errorf("reused %d, /benchmark_configs fetched %d times", snap.Reused, count("/benchmark_configs"))
	}
	// A complete snapshot is reused as a whole; a damaged file is fetched again.
	if err := os.WriteFile(filepath.Join(dir, "tables", "zone.json.gz"), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err = snapshot.Fetch(context.Background(), list, opts)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Reused != 1 || count("/benchmark_configs") != 1 || count("/table/zone") != 2 {
		t.Errorf("reused %d, configs %d, zone %d", snap.Reused, count("/benchmark_configs"), count("/table/zone"))
	}
	assertSameTree(t, uninterrupted, dir)
	snap, err = snapshot.Fetch(context.Background(), list, opts)
	if err != nil || snap.Reused != 2 {
		t.Errorf("reused %d, %v", snap.Reused, err)
	}
	// Another database hash: nothing is reused.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthcheck" {
			_, _ = w.Write([]byte(`{"database_hash": "other", "database_last_updated": 1}`))
			return
		}
		srv.ServeHTTP(w, r)
	}))
	defer other.Close()
	opts.KeeperURL = other.URL
	snap, err = snapshot.Fetch(context.Background(), list, opts)
	if err != nil || snap.Reused != 0 {
		t.Errorf("reused %d, %v", snap.Reused, err)
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
	_, err := snapshot.Fetch(context.Background(), list, snapshot.Options{Dir: filepath.Join(t.TempDir(), "s"), KeeperURL: proxy.URL, Sleep: noSleep})
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
	opts := snapshot.Options{Dir: filepath.Join(t.TempDir(), "s"), KeeperURL: proxy.URL, Sleep: func(_ context.Context, d time.Duration) error {
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
	opts := snapshot.Options{Dir: filepath.Join(t.TempDir(), "s"), KeeperURL: srv.URL, Sleep: noSleep}
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
	snap, err := snapshot.Fetch(context.Background(), list, snapshot.Options{Dir: filepath.Join(t.TempDir(), "s"), KeeperURL: srv.URL, Sleep: noSleep})
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
	snap, err := snapshot.Fetch(context.Background(), list, snapshot.Options{Dir: filepath.Join(t.TempDir(), "s"), KeeperURL: srv.URL, Sleep: noSleep})
	if err != nil {
		t.Fatal(err)
	}
	q := snap.Manifest.Queries[0]
	got := string(gunzip(t, readFile(t, filepath.Join(snap.Dir, q.File))))
	if got != `[{"operating_system":"Linux","price":0.25,"vendor_id":"aws","zone_id":"z"}]` || q.Projection != "flat" || len(q.Fields) == 0 {
		t.Errorf("stored %s, projection %q %v", got, q.Projection, q.Fields)
	}
}

// fakeKeeper serves a region table with a retired and an active hcloud region
// and counts the requests per path.
func fakeKeeper(t *testing.T, hits *sync.Map) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := hits.LoadOrStore(r.URL.Path, new(atomic.Int32))
		n.(*atomic.Int32).Add(1)
		switch r.URL.Path {
		case "/healthcheck":
			_, _ = w.Write([]byte(`{"database_hash": "h", "database_last_updated": 1}`))
		case "/table/region":
			_, _ = w.Write([]byte(`[
 {"vendor_id":"hcloud","region_id":"4","api_reference":"fsn1-dc14","aliases":["fsn1"],"status":"inactive"},
 {"vendor_id":"hcloud","region_id":"fsn1","api_reference":"fsn1","aliases":["4"],"status":"active"},
 {"vendor_id":"hcloud","region_id":"2","api_reference":"nbg1-dc3","aliases":["nbg1"],"status":"inactive"}]`))
		case "/benchmark_configs":
			_, _ = w.Write([]byte(`[{"benchmark_id":"geekbench:score","config":"{\"cores\": \"multi\"}"},{"benchmark_id":"geekbench:score","config":"{\"cores\": \"single\"}"}]`))
		default:
			_, _ = w.Write([]byte(`[{"a":1}]`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestExpansionPrefersActiveRegionsAndFailsFast(t *testing.T) {
	var hits sync.Map
	url := fakeKeeper(t, &hits)
	servers := snapshot.Entry{Name: "{vendor}/{region_id}", Path: "/servers"}
	fetch := func(region string, queries ...snapshot.Entry) (*snapshot.Snapshot, error) {
		return snapshot.Fetch(context.Background(), &snapshot.QueryList{
			Vendors:       []snapshot.Vendor{{Name: "hcloud", Region: region}},
			VendorQueries: []snapshot.Entry{servers},
			Queries:       queries,
		}, snapshot.Options{Dir: filepath.Join(t.TempDir(), "s"), KeeperURL: url, Sleep: noSleep})
	}
	// fsn1 is an alias of the retired region 4 and the id of an active one.
	snap, err := fetch("fsn1")
	if err != nil || snap.Manifest.Queries[0].Name != "hcloud/fsn1" {
		t.Fatalf("fsn1: %+v, %v", snap, err)
	}
	// Only retired regions match: an error naming them, before any data request.
	hits = sync.Map{}
	_, err = fetch("nbg1", snapshot.Entry{Name: "big", Path: "/server_prices"})
	if err == nil || !strings.Contains(err.Error(), "retired") || !strings.Contains(err.Error(), "nbg1") {
		t.Errorf("retired region: %v", err)
	}
	if _, ok := hits.Load("/server_prices"); ok {
		t.Error("data was fetched before the query list was validated")
	}
}

func TestBenchmarksOfOneIDGetUniqueFileNames(t *testing.T) {
	var hits sync.Map
	url := fakeKeeper(t, &hits)
	list := func(benchmarks ...snapshot.Benchmark) *snapshot.QueryList {
		return &snapshot.QueryList{
			Vendors:       []snapshot.Vendor{{Name: "hcloud", Region: "fsn1"}},
			Benchmarks:    benchmarks,
			VendorQueries: []snapshot.Entry{{Name: "{vendor}/benchmark-{benchmark_slug}", Path: "/servers", Benchmarks: true}},
		}
	}
	names := func(l *snapshot.QueryList) []string {
		snap, err := snapshot.Fetch(context.Background(), l, snapshot.Options{Dir: filepath.Join(t.TempDir(), "s"), KeeperURL: url, Sleep: noSleep})
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(snap.Manifest.Queries))
		for _, q := range snap.Manifest.Queries {
			out = append(out, q.File)
		}
		return out
	}
	single := snapshot.Benchmark{ID: "geekbench:score", Config: map[string]any{"cores": "single"}}
	multi := snapshot.Benchmark{ID: "geekbench:score", Config: map[string]any{"cores": "multi"}}
	if got := names(list(single)); len(got) != 1 || got[0] != "hcloud/benchmark-geekbench_score.json.gz" {
		t.Errorf("a lone config keeps the plain name: %v", got)
	}
	a, b := names(list(single, multi)), names(list(multi, single))
	if len(a) != 2 || a[0] == a[1] || !strings.HasPrefix(a[0], "hcloud/benchmark-geekbench_score-") {
		t.Fatalf("names %v", a)
	}
	// Deterministic: the same config has the same name in any order.
	if a[0] != b[1] || a[1] != b[0] {
		t.Errorf("names depend on the order: %v vs %v", a, b)
	}
	// Same id and same config twice is refused.
	_, err := snapshot.Fetch(context.Background(), list(single, single), snapshot.Options{Dir: filepath.Join(t.TempDir(), "s"), KeeperURL: url, Sleep: noSleep})
	if err == nil || !strings.Contains(err.Error(), "two queries write") {
		t.Errorf("duplicate config: %v", err)
	}
}

func TestConcurrentRunsOnOneOutputAreRefused(t *testing.T) {
	srv := keepertest.New(t)
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/table/zone" {
			started <- struct{}{}
			<-release
		}
		srv.ServeHTTP(w, r)
	}))
	defer slow.Close()
	dir := filepath.Join(t.TempDir(), "snap")
	list := &snapshot.QueryList{Queries: []snapshot.Entry{{Name: "tables/zone", Path: "/table/zone"}}}
	done := make(chan error, 1)
	go func() {
		_, err := snapshot.Fetch(context.Background(), list, snapshot.Options{Dir: dir, KeeperURL: slow.URL, Sleep: noSleep})
		done <- err
	}()
	<-started
	_, err := snapshot.Fetch(context.Background(), list, snapshot.Options{Dir: dir, KeeperURL: srv.URL, Sleep: noSleep})
	if err == nil || !strings.Contains(err.Error(), "another sc-snapshot run") {
		t.Errorf("second run: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A run killed between the two renames of the swap leaves no <out>; the next
// run completes or undoes the swap and resumes from it.
func TestInterruptedSwapIsRecovered(t *testing.T) {
	srv := keepertest.New(t)
	list := &snapshot.QueryList{Queries: []snapshot.Entry{{Name: "tables/zone", Path: "/table/zone"}}}
	dir := filepath.Join(t.TempDir(), "snap")
	opts := snapshot.Options{Dir: dir, KeeperURL: srv.URL, Sleep: noSleep, Resume: true}
	if _, err := snapshot.Fetch(context.Background(), list, opts); err != nil {
		t.Fatal(err)
	}
	want := tree(t, dir)
	for name, stageFinished := range map[string]bool{"before the second rename": false, "after the stage was finished": true} {
		if err := os.Rename(dir, dir+".old"); err != nil {
			t.Fatal(err)
		}
		if stageFinished {
			if err := os.MkdirAll(dir+".tmp", 0o750); err != nil {
				t.Fatal(err)
			}
			for n, c := range want {
				p := filepath.Join(dir+".tmp", filepath.FromSlash(n))
				if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		snap, err := snapshot.Fetch(context.Background(), list, opts)
		if err != nil || snap.Reused != 1 {
			t.Fatalf("%s: reused %v, %v", name, snap, err)
		}
		if got := tree(t, dir); len(got) != len(want) {
			t.Errorf("%s: %d files, want %d", name, len(got), len(want))
		}
		if _, err := os.Stat(dir + ".old"); err == nil {
			t.Errorf("%s: .old left", name)
		}
	}
}
