/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package keeper_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/SpareCores/sc-keeper-go/keepertest"
)

func awsQuery() keeper.Query {
	return keeper.Query{Vendor: "aws", Regions: []string{"us-east-1"}, Allocation: keeper.AllocationOnDemand}
}

func TestServersReplay(t *testing.T) {
	srv := keepertest.New(t)
	c := keeper.New(srv.URL)
	rows, err := c.Servers(context.Background(), awsQuery())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 1000 {
		t.Fatalf("got %d aws rows, want > 1000", len(rows))
	}
	var m7i *keeper.Server
	for i := range rows {
		if rows[i].APIReference == "m7i.large" {
			m7i = &rows[i]
		}
	}
	if m7i == nil {
		t.Fatal("m7i.large missing")
	}
	if m7i.VCPUs != 2 || m7i.Family != "m7i" || m7i.CPUArchitecture != "x86_64" || m7i.MemoryAmount != 8192 {
		t.Errorf("unexpected m7i.large: %+v", m7i)
	}
	if m7i.Price == nil || *m7i.Price <= 0 || m7i.CPUCores == nil || !slices.Contains(m7i.CPUFlags, "avx2") {
		t.Errorf("m7i.large lacks price, cores or flags: %+v", m7i)
	}
}

func TestServersPushDownIgnoredByReplay(t *testing.T) {
	srv := keepertest.New(t)
	c := keeper.New(srv.URL)
	q := awsQuery()
	q.PushDown = keeper.PushDown{
		VCPUsMin: new(2), VCPUsMax: new(16), Architectures: []string{"x86_64"},
		MemoryMin: new(4.0), PriceMax: new(0.5), CPUHyperthreading: new(false),
		StorageCountMin: new(2), BenchmarkScoreMin: new(1.0),
	}
	if !q.PushDown.HasBenchmarkThreshold() {
		t.Error("HasBenchmarkThreshold = false")
	}
	if _, err := c.Servers(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	u := srv.Requests()[0]
	for k, want := range map[string]string{
		"vcpus_min": "2", "vcpus_max": "16", "architecture": "x86_64", "memory_min": "4",
		"price_max": "0.5", "currency": "USD", "cpu_hyperthreading": "false", "storage_count_min": "2",
		"benchmark_score_min": "1", "limit": "-1", "vendor_regions": "aws~us-east-1",
	} {
		if got := u.Query().Get(k); got != want {
			t.Errorf("param %s = %q, want %q", k, got, want)
		}
	}
}

func TestServersUnknownQuery(t *testing.T) {
	srv := keepertest.New(t)
	c := keeper.New(srv.URL, keeper.WithRetries(0, 0, 0))
	q := awsQuery()
	q.Regions = []string{"eu-west-1"}
	_, err := c.Servers(context.Background(), q)
	var he *keeper.HTTPError
	if !errors.As(err, &he) || he.StatusCode != http.StatusNotFound {
		t.Fatalf("want HTTP 404, got %v", err)
	}
	if !strings.Contains(he.Body, "no recorded response") {
		t.Errorf("404 body lacks a clear message: %q", he.Body)
	}
	if _, err := keeper.New("http://unused").Servers(context.Background(), keeper.Query{}); err == nil {
		t.Error("query without vendor accepted")
	}
}

// fakeKeeper answers /servers with one row per requested region, priced by
// the region's position, and records every request.
type fakeKeeper struct {
	mu   sync.Mutex
	reqs [][]string
}

func (f *fakeKeeper) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	regions := r.URL.Query()["vendor_regions"]
	f.mu.Lock()
	f.reqs = append(f.reqs, regions)
	f.mu.Unlock()
	rows := make([]map[string]any, 0, 2*len(regions))
	for _, vr := range regions {
		price := map[string]float64{"aws~r1": 0.5, "aws~r2": 0.4, "aws~r3": 0.6, "aws~r4": 0.3, "aws~r5": 0.7}[vr]
		rows = append(rows,
			map[string]any{"api_reference": "shared", "price": price, "vcpus": 2},
			map[string]any{"api_reference": "only-" + vr, "price": price, "vcpus": 2, "future_field": []int{1}})
	}
	_ = json.NewEncoder(w).Encode(rows)
}

func TestServersChunking(t *testing.T) {
	f := &fakeKeeper{}
	srv := httptest.NewServer(f)
	defer srv.Close()

	c := keeper.New(srv.URL)
	q := keeper.Query{Vendor: "aws", Regions: []string{"r5", "r1", "r3", "r2", "r4", "r1"}}
	rows, err := c.Servers(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.reqs) != 2 || len(f.reqs[0]) != 3 || len(f.reqs[1]) != 2 {
		t.Fatalf("want chunks of 3+2 regions, got %v", f.reqs)
	}
	if len(rows) != 6 {
		t.Fatalf("want 6 merged rows (shared + 5 region-only), got %d", len(rows))
	}
	for _, r := range rows {
		if r.APIReference == "shared" && *r.Price != 0.3 {
			t.Errorf("shared row: want the cheapest price 0.3, got %v", *r.Price)
		}
	}

	f.reqs = nil
	unlimited := keeper.New(srv.URL, keeper.WithRegionsPerRequest(0))
	if _, err := unlimited.Servers(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if len(f.reqs) != 1 || len(f.reqs[0]) != 5 {
		t.Fatalf("want one request with 5 regions, got %v", f.reqs)
	}
}

// flaky fails the first n requests with the given status (and Retry-After),
// then delegates.
type flaky struct {
	n          atomic.Int32
	status     int
	retryAfter string
	next       http.Handler
	auth       atomic.Value
}

func (f *flaky) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.auth.Store(r.Header.Get("Authorization"))
	if f.n.Add(-1) >= 0 {
		if f.retryAfter != "" {
			w.Header().Set("Retry-After", f.retryAfter)
		}
		http.Error(w, "slow down", f.status)
		return
	}
	f.next.ServeHTTP(w, r)
}

func TestRetries(t *testing.T) {
	h, err := keepertest.NewHandler(keepertest.SnapshotDir())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		failures   int32
		status     int
		retryAfter string
		retries    int
		wantErr    bool
		wantSleeps []time.Duration
	}{
		{name: "503 then ok", failures: 2, status: 503, retries: 4,
			wantSleeps: []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}},
		{name: "429 honours Retry-After", failures: 1, status: 429, retryAfter: "7", retries: 4,
			wantSleeps: []time.Duration{7 * time.Second}},
		{name: "Retry-After capped", failures: 1, status: 429, retryAfter: "3600", retries: 4,
			wantSleeps: []time.Duration{time.Minute}},
		{name: "retries exhausted", failures: 5, status: 502, retries: 2, wantErr: true,
			wantSleeps: []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}},
		{name: "422 is not retried", failures: 1, status: 422, retries: 4, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &flaky{status: tt.status, retryAfter: tt.retryAfter, next: h}
			f.n.Store(tt.failures)
			srv := httptest.NewServer(f)
			defer srv.Close()
			var sleeps []time.Duration
			c := keeper.New(srv.URL, keeper.WithRetries(tt.retries, 10*time.Millisecond, time.Minute),
				keeper.WithAPIKey("test-key"),
				keeper.WithSleep(func(_ context.Context, d time.Duration) error {
					sleeps = append(sleeps, d)
					return nil
				}))
			_, err := c.Servers(context.Background(), awsQuery())
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !slices.Equal(sleeps, tt.wantSleeps) {
				t.Errorf("sleeps = %v, want %v", sleeps, tt.wantSleeps)
			}
			if got := f.auth.Load(); got != "Bearer test-key" {
				t.Errorf("Authorization = %q", got)
			}
		})
	}
}

func TestRetryCancelled(t *testing.T) {
	f := &flaky{status: 503, next: http.NotFoundHandler()}
	f.n.Store(100)
	srv := httptest.NewServer(f)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	c := keeper.New(srv.URL, keeper.WithRetries(10, time.Hour, time.Hour))
	start := time.Now()
	_, err := c.Servers(ctx, awsQuery())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("backoff ignored the context")
	}
}

func TestTransportErrorRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = fmt.Fprint(w, `[{"api_reference": "trunc`)
			return
		}
		_, _ = fmt.Fprint(w, `[{"api_reference": "ok", "vcpus": 1}]`)
	}))
	defer srv.Close()
	c := keeper.New(srv.URL, keeper.WithRetries(2, time.Millisecond, time.Millisecond))
	rows, err := c.Servers(context.Background(), awsQuery())
	if err != nil || len(rows) != 1 || calls.Load() != 2 {
		t.Fatalf("rows=%v err=%v calls=%d", rows, err, calls.Load())
	}

	srv.Close()
	_, err = c.Servers(context.Background(), awsQuery())
	if err == nil {
		t.Fatal("want a connection error")
	}
}

func TestTablesAndHealth(t *testing.T) {
	srv := keepertest.New(t)
	c := keeper.New(srv.URL)
	ctx := context.Background()

	regions, err := c.Regions(ctx)
	if err != nil || len(regions) < 100 {
		t.Fatalf("regions: %d, %v", len(regions), err)
	}
	found := false
	for _, r := range regions {
		if r.VendorID == "gcp" && r.APIReference == "us-central1" && r.RegionID == "1000" {
			found = true
		}
	}
	if !found {
		t.Error("gcp us-central1 (region id 1000) not in the region table")
	}
	zones, err := c.Zones(ctx)
	if err != nil || len(zones) < 100 {
		t.Fatalf("zones: %d, %v", len(zones), err)
	}
	h, err := c.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if h.DatabaseHash != srv.Manifest().Keeper.DatabaseHash || h.DatabaseLastUpdated.Year() < 2026 {
		t.Errorf("health = %+v", h)
	}
}

func TestFetcherMemoises(t *testing.T) {
	srv := keepertest.New(t)
	f := keeper.NewFetcher(keeper.New(srv.URL))
	ctx := context.Background()

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			q := awsQuery()
			if _, err := f.Servers(ctx, q); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	for range 2 {
		if _, err := f.BenchmarkCatalog(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Regions(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Zones(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Health(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Region order does not matter for the cache key.
	q := awsQuery()
	q.Regions = []string{"us-east-1", "us-east-1"}
	results, err := f.ServersAll(ctx, []keeper.Query{q, awsQuery()})
	if err != nil || len(results) != 2 {
		t.Fatalf("ServersAll: %v", err)
	}

	if got := len(srv.Requests()); got != 5 {
		t.Errorf("want 5 requests (servers, benchmark_configs, region, zone, healthcheck), got %d", got)
	}

	// Errors are memoised as well.
	bad := awsQuery()
	bad.Regions = []string{"nowhere"}
	fb := keeper.NewFetcher(keeper.New(srv.URL, keeper.WithRetries(0, 0, 0)))
	for range 2 {
		if _, err := fb.Servers(ctx, bad); err == nil {
			t.Fatal("want an error")
		}
	}
	if _, err := fb.ServersAll(ctx, []keeper.Query{bad}); err == nil {
		t.Fatal("want an error")
	}
	failed := 0
	for _, u := range srv.Requests()[5:] {
		if u.Path == "/servers" {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("failed query requested %d times, want once", failed)
	}
}

// Every cacheable request of a cycle carries the database hash, so that the
// CDN serves the whole cycle from one data version; /healthcheck does not.
func TestFetcherDataVersion(t *testing.T) {
	srv := keepertest.New(t)
	f := keeper.NewFetcher(keeper.New(srv.URL))
	ctx := context.Background()
	if _, err := f.Servers(ctx, awsQuery()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.BenchmarkCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Regions(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Zones(ctx); err != nil {
		t.Fatal(err)
	}
	want := srv.Manifest().Keeper.DatabaseHash
	if want == "" {
		t.Fatal("the snapshot has no database hash")
	}
	paths := map[string]bool{}
	for _, u := range srv.Requests() {
		paths[u.Path] = true
		got := u.Query().Get(keeper.DataVersionParam)
		if u.Path == "/healthcheck" {
			if got != "" {
				t.Errorf("/healthcheck carries %s=%q", keeper.DataVersionParam, got)
			}
			continue
		}
		if got != want {
			t.Errorf("%s: %s=%q, want %q", u.Path, keeper.DataVersionParam, got, want)
		}
	}
	for _, p := range []string{"/healthcheck", "/servers", "/benchmark_configs", "/table/region", "/table/zone"} {
		if !paths[p] {
			t.Errorf("no request to %s", p)
		}
	}
}

// Without a hash (healthcheck failing) the requests go out unversioned.
func TestFetcherWithoutDataVersion(t *testing.T) {
	h, err := keepertest.NewHandler(keepertest.SnapshotDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthcheck" {
			http.Error(w, "down", http.StatusNotFound)
			return
		}
		if r.URL.Query().Has(keeper.DataVersionParam) {
			t.Errorf("%s carries %s without a healthcheck", r.URL.Path, keeper.DataVersionParam)
		}
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()
	f := keeper.NewFetcher(keeper.New(srv.URL, keeper.WithRetries(0, 0, 0)))
	if _, err := f.Servers(context.Background(), awsQuery()); err != nil {
		t.Fatal(err)
	}
}

func TestFetcherErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusBadRequest)
	}))
	defer srv.Close()
	f := keeper.NewFetcher(keeper.New(srv.URL))
	ctx := context.Background()
	if _, err := f.BenchmarkCatalog(ctx); err == nil {
		t.Error("catalog: want error")
	}
	if _, err := f.Regions(ctx); err == nil {
		t.Error("regions: want error")
	}
	if _, err := f.Zones(ctx); err == nil {
		t.Error("zones: want error")
	}
	if _, err := f.Health(ctx); err == nil {
		t.Error("health: want error")
	}
}

func TestFetcherCancelledCallerDoesNotPoison(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthcheck" {
			_, _ = fmt.Fprint(w, `{"database_hash": "h", "database_last_updated": 0}`)
			return
		}
		if calls.Add(1) == 1 {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		_, _ = fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()
	defer close(release)

	f := keeper.NewFetcher(keeper.New(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := f.Servers(ctx, awsQuery())
		errc <- err
	}()
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	// A waiter with its own short deadline gives up without a result.
	wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer wcancel()
	if _, err := f.Servers(wctx, awsQuery()); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiter: want deadline exceeded, got %v", err)
	}
	cancel()
	if err := <-errc; err == nil {
		t.Fatal("cancelled caller: want an error")
	}
	if _, err := f.Servers(context.Background(), awsQuery()); err != nil {
		t.Fatalf("after cancellation: %v", err)
	}
}
