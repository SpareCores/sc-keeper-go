/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package keepertest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/SpareCores/sc-keeper-go/keepertest"
)

const recordedHash = "40ecd194b481b86a3237f51c9be6f590abcd53df"

var ctx = context.Background()

func newClient(s *keepertest.Server, opts ...keeper.Option) *keeper.Client {
	return keeper.New(s.URL, opts...)
}

func rawGet(t *testing.T, s *keepertest.Server, path string, header http.Header) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	maps.Copy(req.Header, header)
	// Without this the transport adds Accept-Encoding: gzip and unzips.
	if req.Header.Get("Accept-Encoding") == "" {
		req.Header.Set("Accept-Encoding", "identity")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func TestCatalogHealthcheck(t *testing.T) {
	s := keepertest.NewCatalog(t)
	h, err := newClient(s).Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if h.DatabaseHash != recordedHash || !h.DatabasePublishedAt.Equal(time.Date(2026, 11, 13, 19, 50, 14, 0, time.UTC)) ||
		!slices.Equal(h.SnapshotHashes, []string{recordedHash}) {
		t.Errorf("Health = %+v", h)
	}
	if h.DatabaseLastUpdated.IsZero() {
		t.Error("database_last_updated is missing")
	}
}

// Every recorded file is served as the bytes Keeper sent, with the digest of
// the manifest, and decodes into its row type.
func TestCatalogFiles(t *testing.T) {
	s := keepertest.NewCatalog(t)
	c := newClient(s)
	manifest, version, err := c.SnapshotManifest(ctx, recordedHash)
	if err != nil {
		t.Fatal(err)
	}
	if version.Hash != recordedHash || !version.PublishedAt.Equal(time.Date(2026, 11, 13, 19, 50, 14, 0, time.UTC)) {
		t.Errorf("version = %+v", version)
	}
	if len(manifest.Files) < 1000 {
		t.Errorf("the manifest lists %d files, Keeper's has more than a thousand", len(manifest.Files))
	}
	if !slices.IsSortedFunc(manifest.Files, func(a, b keeper.ManifestFile) int { return strings.Compare(a.Path, b.Path) }) {
		t.Error("the manifest is not sorted by path")
	}

	rec := s.Manifest().Snapshot
	if rec == nil || len(rec.Files) != 8 {
		t.Fatalf("recording: %+v", rec)
	}
	for _, f := range rec.Files {
		entry, ok := manifest.Lookup(f.Path)
		if !ok {
			t.Errorf("%s is not in the manifest", f.Path)
			continue
		}
		body, err := c.SnapshotFile(ctx, recordedHash, f.Path)
		if err != nil {
			t.Errorf("%s: %v", f.Path, err)
			continue
		}
		stored, err := os.ReadFile(filepath.Join(keepertest.CatalogDir(), f.File))
		if err != nil || !bytes.Equal(stored, body.Gzip) {
			t.Errorf("%s: the gzip bytes are not the stored ones (%v)", f.Path, err)
		}
		if err := body.Verify(entry.SHA256); err != nil {
			t.Errorf("%s: %v", f.Path, err)
		}
		if body.Version.Hash != recordedHash || body.Header.Get("Cache-Control") != "private, max-age=86400, immutable" ||
			body.Header.Get("Content-Type") != "application/json" || body.Header.Get("Content-Encoding") != "gzip" {
			t.Errorf("%s: headers %v, version %+v", f.Path, body.Header, body.Version)
		}
	}

}

// catalogRows decodes a recorded file into its row type, with the row count of
// Keeper's manifest.
func catalogRows[T any](t *testing.T, path string) []T {
	t.Helper()
	c := newClient(keepertest.NewCatalog(t))
	manifest, _, err := c.SnapshotManifest(ctx, recordedHash)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := manifest.Lookup(path)
	if !ok {
		t.Fatalf("%s is not in the manifest", path)
	}
	body, err := c.SnapshotFile(ctx, recordedHash, path)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := keeper.DecodeSnapshotRows[T](body, entry.Rows)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return rows
}

// vendor rows decode into their row types.
func TestCatalogRowsVendor(t *testing.T) {
	vendors := catalogRows[keeper.Vendor](t, "vendor")
	if len(vendors) != 8 {
		t.Fatalf("%d vendors", len(vendors))
	}
	if v := vendors[0]; v.VendorID != "alicloud" || v.BillingIncrementSeconds == nil || *v.BillingIncrementSeconds != 1 ||
		v.StoppedServerCharged == nil || !*v.StoppedServerCharged || v.BillingComment == nil {
		t.Errorf("vendor = %+v", v)
	}
}

// region and zone rows decode into their row types.
func TestCatalogRowsRegionZone(t *testing.T) {
	regions := catalogRows[keeper.Region](t, "region/hcloud")
	if len(regions) != 6 || regions[1].RegionID != "fsn1" || regions[1].Aliases[0] != "4" || regions[0].State == nil || *regions[0].State != "VA" || regions[1].State != nil {
		t.Errorf("regions: %+v", regions)
	}
	zones := catalogRows[keeper.Zone](t, "zone/hcloud")
	if len(zones) != 6 || zones[0].ZoneID != "ash" {
		t.Errorf("zones: %+v", zones)
	}
}

// server rows decode into their row types.
func TestCatalogRowsServer(t *testing.T) {
	servers := catalogRows[keeper.TableServer](t, "server/hcloud")
	if len(servers) != 26 {
		t.Fatalf("%d servers", len(servers))
	}
	// The server_id differs from the api_reference at Hetzner.
	if sv := servers[0]; sv.ServerID != "100" || sv.APIReference != "ccx53" || sv.VCPUs != 32 || sv.CPUArchitecture != "x86_64" ||
		sv.Family == nil || *sv.Family != "General Purpose" || !slices.Contains(sv.CPUFlags, "avx2") || sv.MemoryAmount != 128000 {
		t.Errorf("server = %+v", sv)
	}
	if conv := servers[0].Server(); conv.Family != "General Purpose" || conv.APIReference != "ccx53" || conv.VCPUs != 32 {
		t.Errorf("Server() = %+v", conv)
	}
}

// server_price rows decode into their row types.
func TestCatalogRowsServerPrice(t *testing.T) {
	prices := catalogRows[keeper.ServerPrice](t, "server_price/hcloud")
	if len(prices) != 79 {
		t.Fatalf("%d prices", len(prices))
	}
	p := prices[0]
	if p.ServerID != "100" || p.Allocation != keeper.PriceAllocationOnDemand || p.Unit != "hour" || p.Currency != "USD" || p.Price != 0.9537876 ||
		p.PriceMonthly == nil || *p.PriceMonthly != 594.21078 || len(p.PriceTiered) != 2 || !isInf(p.PriceTiered[1].Upper) {
		t.Errorf("price = %+v", p)
	}
}

// storage_price and ipv4_price rows decode into their row types.
func TestCatalogRowsStorageIPv4(t *testing.T) {
	storage := catalogRows[keeper.StoragePrice](t, "storage_price/hcloud")
	if len(storage) != 6 || storage[0].Price != 0.04862879999999999 || storage[0].Unit != "GB/month" || storage[0].StorageID != "block" {
		t.Errorf("storage prices: %+v", storage)
	}
	ipv4 := catalogRows[keeper.IPv4Price](t, "ipv4_price/hcloud")
	if len(ipv4) != 6 || ipv4[0].Unit != "month" || ipv4[0].Price != 0.5526 {
		t.Errorf("ipv4 prices: %+v", ipv4)
	}
}

// benchmark_score rows decode into their row types.
func TestCatalogRowsScore(t *testing.T) {
	scores := catalogRows[keeper.BenchmarkScore](t, keeper.ScorePath("hcloud", "stress_ng:bestn"))
	if len(scores) != 26 {
		t.Fatalf("%d scores", len(scores))
	}
	if sc := scores[0]; sc.ResourceType != "server" || sc.ResourceID != "100" || string(sc.Config) != "{}" || sc.Score == nil || *sc.Score != 48807.7 || sc.BenchmarkID != "stress_ng:bestn" {
		t.Errorf("score = %+v", sc)
	}
}

func isInf(f float64) bool { return f > 1e308 }

func TestCatalogGzipOnly(t *testing.T) {
	s := keepertest.NewCatalog(t)
	for name, ae := range map[string]string{"none": "", "identity": "identity", "gzip refused": "gzip;q=0", "other": "br, deflate"} {
		resp, body := rawGet(t, s, "/snapshot/"+recordedHash+"/vendor", http.Header{"Accept-Encoding": {ae}})
		if name == "none" {
			// rawGet sets identity for an empty header; send the request bare.
			resp, body = bareGet(t, s, "/snapshot/"+recordedHash+"/vendor")
		}
		if resp.StatusCode != http.StatusNotAcceptable || resp.Header.Get("Content-Type") != "application/problem+json" ||
			resp.Header.Get("Cache-Control") != "private, no-store" || !strings.Contains(string(body), `"about:blank"`) {
			t.Errorf("%s: %d %v %s", name, resp.StatusCode, resp.Header, body)
		}
		if resp.Header.Get("SC-Data-Hash") != recordedHash {
			t.Errorf("%s: SC-Data-Hash %q", name, resp.Header.Get("SC-Data-Hash"))
		}
	}
	for _, ae := range []string{"gzip", "gzip, deflate", "br, gzip;q=0.5", "*", "GZIP"} {
		if resp, _ := rawGet(t, s, "/snapshot/"+recordedHash+"/vendor", http.Header{"Accept-Encoding": {ae}}); resp.StatusCode != http.StatusOK {
			t.Errorf("Accept-Encoding %q: %d", ae, resp.StatusCode)
		}
	}
}

// bareGet sends a request without an Accept-Encoding header at all.
func bareGet(t *testing.T, s *keepertest.Server, path string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{DisableCompression: true}
	defer tr.CloseIdleConnections()
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

func TestCatalogErrors(t *testing.T) {
	s := keepertest.NewCatalog(t)
	c := newClient(s, keeper.WithRetries(0, 0, 0))

	// Another hash, and a malformed one: snapshot-not-available.
	for _, hash := range []string{"0000000000000000000000000000000000000000", "foo"} {
		_, err := c.SnapshotFile(ctx, hash, "vendor")
		var pe *keeper.ProblemError
		if !keeper.IsSnapshotNotAvailable(err) || !errors.As(err, &pe) || pe.Status != 404 ||
			pe.Requested != hash || pe.Current != recordedHash || !slices.Equal(pe.Available, []string{recordedHash}) {
			t.Errorf("hash %q: %v (%+v)", hash, err, pe)
		}
		if _, _, err := c.SnapshotManifest(ctx, hash); !keeper.IsSnapshotNotAvailable(err) {
			t.Errorf("manifest of %q: %v", hash, err)
		}
	}

	for _, path := range []string{
		"vendor/hcloud",       // vendor is a global table
		"nosuchtable/hcloud",  // unknown table
		"nosuchtable",         //
		"server/hcloud/extra", // too deep
		"server/bad%20vendor", // not a valid name
		"benchmark_score/hcloud/bad%20id",
		"benchmark_score/nosuchvendor/stress_ng:bestn", // a vendor that is not in the recording
	} {
		if _, err := c.SnapshotFile(ctx, recordedHash, path); !keeper.IsNotFound(err) {
			t.Errorf("%s: %v, want not-found", path, err)
		}
	}
	// Keeper has no trailing-slash manifest.
	if resp, body := rawGet(t, s, "/snapshot/"+recordedHash+"/", http.Header{"Accept-Encoding": {"gzip"}}); resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "/problems/not-found") {
		t.Errorf("trailing slash: %d %s", resp.StatusCode, body)
	}

	// In Keeper's manifest, not recorded: a test bug, named in the title.
	_, err := c.SnapshotFile(ctx, recordedHash, "server/aws")
	var pe *keeper.ProblemError
	if !keeper.IsNotFound(err) || !errors.As(err, &pe) || !strings.Contains(pe.Title, "server/aws") || !strings.Contains(pe.Title, "not recorded") {
		t.Errorf("an unrecorded path: %v", err)
	}
}

func TestCatalogUnknownBenchmarkIsEmpty(t *testing.T) {
	s := keepertest.NewCatalog(t)
	c := newClient(s)
	path := keeper.ScorePath("hcloud", "nosuch:benchmark")
	body, err := c.SnapshotFile(ctx, recordedHash, path)
	if err != nil {
		t.Fatalf("an unknown benchmark of a recorded vendor: %v", err)
	}
	rows, err := keeper.DecodeSnapshotRows[keeper.BenchmarkScore](body, 0)
	if err != nil || len(rows) != 0 {
		t.Errorf("%d rows, %v", len(rows), err)
	}
	zr, _ := body.Open()
	raw, _ := io.ReadAll(zr)
	var cols map[string][]any
	if err := json.Unmarshal(raw, &cols); err != nil || len(cols) < 8 {
		t.Fatalf("columns: %v, %v", cols, err)
	}
	recorded, _ := c.SnapshotFile(ctx, recordedHash, keeper.ScorePath("hcloud", "stress_ng:bestn"))
	zr2, _ := recorded.Open()
	raw2, _ := io.ReadAll(zr2)
	var cols2 map[string][]any
	_ = json.Unmarshal(raw2, &cols2)
	for name, v := range cols {
		if len(v) != 0 {
			t.Errorf("column %s has rows", name)
		}
		if _, ok := cols2[name]; !ok {
			t.Errorf("column %s is not one of the recorded file", name)
		}
	}
	if len(cols) != len(cols2) {
		t.Errorf("%d columns, the recorded file has %d", len(cols), len(cols2))
	}
	// The columns come in the order of the recorded file.
	if !bytes.HasPrefix(raw, []byte(`{"vendor_id":[],"benchmark_id":[],"resource_type":[]`)) {
		t.Errorf("column order: %.80s", raw)
	}
}

func TestRequireAPIKey(t *testing.T) {
	s := keepertest.NewCatalog(t)
	s.RequireAPIKey("test-key")

	for name, tc := range map[string]struct {
		key        string
		detail     string
		keyWasSent bool
	}{
		"no key":    {"", "Authentication required", false},
		"wrong key": {"other", "Invalid or expired token", true},
	} {
		opts := []keeper.Option{keeper.WithRetries(0, 0, 0)}
		if tc.key != "" {
			opts = append(opts, keeper.WithAPIKey(tc.key))
		}
		_, err := newClient(s, opts...).SnapshotFile(ctx, recordedHash, "vendor")
		var ue *keeper.UnauthorizedError
		if !errors.As(err, &ue) || ue.Detail != tc.detail || ue.KeyConfigured != tc.keyWasSent {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Auth comes before the gzip check; both 401s are plain JSON.
	resp, body := bareGet(t, s, "/snapshot/"+recordedHash+"/vendor")
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("Content-Type") != "application/json" ||
		resp.Header.Get("WWW-Authenticate") != "Bearer" || resp.Header.Get("Cache-Control") != "private, no-store" ||
		strings.TrimSpace(string(body)) != `{"detail":"Authentication required"}` {
		t.Errorf("401: %d %v %s", resp.StatusCode, resp.Header, body)
	}
	if resp, _ := rawGet(t, s, "/snapshot/"+recordedHash+"/vendor", http.Header{"Authorization": {"Basic test-key"}, "Accept-Encoding": {"gzip"}}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a key that is not a bearer token: %d", resp.StatusCode)
	}

	good := newClient(s, keeper.WithAPIKey("test-key"))
	if _, err := good.SnapshotFile(ctx, recordedHash, "vendor"); err != nil {
		t.Errorf("the right key: %v", err)
	}
	if _, _, err := good.SnapshotManifest(ctx, recordedHash); err != nil {
		t.Errorf("the manifest with the right key: %v", err)
	}
	// /healthcheck stays anonymous.
	if _, err := newClient(s).Health(ctx); err != nil {
		t.Errorf("healthcheck without a key: %v", err)
	}
}

func TestPreparing(t *testing.T) {
	s := keepertest.NewCatalog(t)
	s.Preparing(recordedHash, 2)
	var sleeps []time.Duration
	c := newClient(s, keeper.WithSleep(func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}))
	if _, err := c.SnapshotFile(ctx, recordedHash, "vendor"); err != nil {
		t.Fatal(err)
	}
	if len(sleeps) != 2 || sleeps[0] != 0 {
		t.Errorf("waits = %v, want two of Retry-After: 0", sleeps)
	}
	if _, err := c.SnapshotFile(ctx, recordedHash, "vendor"); err != nil {
		t.Errorf("after the preparing requests: %v", err)
	}

	s.Preparing(recordedHash, 1)
	resp, body := rawGet(t, s, "/snapshot/"+recordedHash, http.Header{"Accept-Encoding": {"gzip"}})
	var problem struct{ Type, Requested, Current string }
	_ = json.Unmarshal(body, &problem)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "0" || problem.Type != "/problems/snapshot-preparing" ||
		problem.Requested != recordedHash || problem.Current != recordedHash {
		t.Errorf("503: %d %v %s", resp.StatusCode, resp.Header, body)
	}
}

func TestAddHash(t *testing.T) {
	const second = "2b4bd195ff4897951e174b9b59adf704a002502b"
	s := keepertest.NewCatalog(t)
	replaced := `{"vendor_id":["hcloud"],"region_id":["xyz"],"name":["xyz"],"api_reference":["xyz"],"status":["active"]}`
	added := `{"vendor_id":["hcloud"],"benchmark_id":["new:bench"],"resource_type":["server"],"resource_id":["100"],"config":[{}],"score":[1.5],"status":["active"]}`
	if err := s.AddHash(second, map[string][]byte{
		"region/hcloud": []byte(replaced), "benchmark_score/hcloud/new:bench": []byte(added),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddHash(second, nil); err == nil {
		t.Error("a hash added twice")
	}
	if err := s.AddHash("not-a-hash", nil); err == nil {
		t.Error("a malformed hash")
	}
	c := newClient(s, keeper.WithRetries(0, 0, 0))

	// The recorded hash stays current, both are available, newest first.
	h, _ := c.Health(ctx)
	if h.DatabaseHash != recordedHash || !slices.Equal(h.SnapshotHashes, []string{second, recordedHash}) {
		t.Errorf("Health = %+v", h)
	}
	if err := s.SetCurrent("1111111111111111111111111111111111111111"); err == nil {
		t.Error("SetCurrent of a hash that is not served")
	}
	if err := s.SetCurrent(second); err != nil {
		t.Fatal(err)
	}
	if h, _ = c.Health(ctx); h.DatabaseHash != second || !h.DatabasePublishedAt.Equal(time.Date(2026, 11, 13, 20, 50, 14, 0, time.UTC)) {
		t.Errorf("Health after SetCurrent = %+v", h)
	}

	m1, _, err := c.SnapshotManifest(ctx, recordedHash)
	if err != nil {
		t.Fatal(err)
	}
	m2, v2, err := c.SnapshotManifest(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Hash != second || !v2.PublishedAt.Equal(h.DatabasePublishedAt) {
		t.Errorf("version = %+v", v2)
	}
	if !slices.IsSortedFunc(m2.Files, func(a, b keeper.ManifestFile) int { return strings.Compare(a.Path, b.Path) }) || len(m2.Files) != len(m1.Files)+1 {
		t.Errorf("%d files in the new manifest, %d in the old", len(m2.Files), len(m1.Files))
	}
	// Only the replaced and the added path differ.
	for _, f := range m2.Files {
		old, ok := m1.Lookup(f.Path)
		changed := f.Path == "region/hcloud" || f.Path == "benchmark_score/hcloud/new:bench"
		same := ok && old.SHA256 == f.SHA256
		if changed == same {
			t.Errorf("%s: sha256 unchanged = %v, want %v", f.Path, same, !changed)
		}
	}
	if f, _ := m2.Lookup("region/hcloud"); f.Rows != 1 {
		t.Errorf("rows of the replaced file = %d", f.Rows)
	}
	for _, path := range []string{"region/hcloud", "benchmark_score/hcloud/new:bench", "server/hcloud"} {
		body, err := c.SnapshotFile(ctx, second, path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		entry, _ := m2.Lookup(path)
		if err := body.Verify(entry.SHA256); err != nil {
			t.Errorf("%s: %v", path, err)
		}
		if body.Version.Hash != second {
			t.Errorf("%s: version %+v", path, body.Version)
		}
	}
	// An unchanged file is the same bytes under both hashes.
	a, _ := c.SnapshotFile(ctx, recordedHash, "server/hcloud")
	b, _ := c.SnapshotFile(ctx, second, "server/hcloud")
	if !bytes.Equal(a.Gzip, b.Gzip) {
		t.Error("an unchanged file differs between the hashes")
	}

	// The new hash knows the added benchmark; the recorded one answers empty.
	if _, err := c.SnapshotFile(ctx, recordedHash, "benchmark_score/hcloud/new:bench"); err != nil {
		t.Errorf("an unknown benchmark of the recorded hash: %v", err)
	}

}

// Expire: the old hash is gone, and the 404 names the new current one.
func TestExpire(t *testing.T) {
	const second = "2b4bd195ff4897951e174b9b59adf704a002502b"
	s := keepertest.NewCatalog(t)
	if err := s.AddHash(second, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCurrent(second); err != nil {
		t.Fatal(err)
	}
	c := newClient(s, keeper.WithRetries(0, 0, 0))
	if err := s.Expire(second); err == nil {
		t.Error("the current hash expired")
	}
	if err := s.Expire(recordedHash); err != nil {
		t.Fatal(err)
	}
	_, err := c.SnapshotFile(ctx, recordedHash, "vendor")
	var pe *keeper.ProblemError
	if !keeper.IsSnapshotNotAvailable(err) || !errors.As(err, &pe) || pe.Current != second || !slices.Equal(pe.Available, []string{second}) {
		t.Errorf("an expired hash: %v", err)
	}
	if err := s.Expire(recordedHash); err == nil {
		t.Error("an expired hash expired twice")
	}
}

func TestRequestsAreRecorded(t *testing.T) {
	s := keepertest.NewCatalog(t)
	c := newClient(s)
	_, _ = c.Health(ctx)
	_, _ = c.SnapshotFile(ctx, recordedHash, "vendor")
	reqs := s.Requests()
	if len(reqs) != 2 || reqs[0].Path != "/healthcheck" || reqs[1].Path != "/snapshot/"+recordedHash+"/vendor" {
		t.Errorf("requests = %v", reqs)
	}
}

// A recording without a snapshot block has no snapshot routes, and the
// existing routes keep working next to the new ones.
func TestLegacyRecordingHasNoSnapshotRoutes(t *testing.T) {
	s := keepertest.New(t)
	_, err := newClient(s, keeper.WithRetries(0, 0, 0)).SnapshotFile(ctx, recordedHash, "vendor")
	var he *keeper.HTTPError
	if !errors.As(err, &he) || he.StatusCode != http.StatusNotFound {
		t.Errorf("err = %v", err)
	}
	if err := (&keepertest.Handler{}).AddHash(recordedHash, nil); err == nil {
		t.Error("AddHash without a snapshot block")
	}
	if regions, err := newClient(s).Regions(ctx); err != nil || len(regions) < 100 {
		t.Errorf("the /table/region of the legacy recording: %d, %v", len(regions), err)
	}
}

// AuthGuardMiddleware: a wrong or non-Bearer Authorization header is a 401 on
// every route; no header is a 401 on the snapshot routes only.
func TestRequireAPIKeyGuardsEveryRoute(t *testing.T) {
	s := keepertest.NewCatalog(t)
	s.RequireAPIKey("test-key")
	for _, path := range []string{"/healthcheck", "/benchmark_configs", "/table/region", "/snapshot/" + recordedHash + "/vendor"} {
		for _, auth := range []string{"Bearer other", "Basic test-key", "test-key"} {
			resp, body := rawGet(t, s, path, http.Header{"Authorization": {auth}, "Accept-Encoding": {"gzip"}})
			if resp.StatusCode != http.StatusUnauthorized || strings.TrimSpace(string(body)) != `{"detail":"Invalid or expired token"}` ||
				resp.Header.Get("WWW-Authenticate") != "Bearer" {
				t.Errorf("%s with %q: %d %s", path, auth, resp.StatusCode, body)
			}
		}
		resp, _ := rawGet(t, s, path, http.Header{"Authorization": {"Bearer test-key"}, "Accept-Encoding": {"gzip"}})
		if resp.StatusCode == http.StatusUnauthorized {
			t.Errorf("%s with the key: 401", path)
		}
	}
	if resp, _ := rawGet(t, s, "/healthcheck", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("healthcheck without a header: %d", resp.StatusCode)
	}
	resp, body := rawGet(t, s, "/snapshot/"+recordedHash+"/vendor", http.Header{"Accept-Encoding": {"gzip"}})
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), "Authentication required") {
		t.Errorf("snapshot without a header: %d %s", resp.StatusCode, body)
	}
}

// Keeper's order: an unknown route or invalid name is a not-found before the
// gzip check and under any hash; Preparing only counts real file requests.
func TestCatalogCheckOrder(t *testing.T) {
	s := keepertest.NewCatalog(t)
	unknown := "0000000000000000000000000000000000000000"
	for _, path := range []string{"/snapshot/" + unknown + "/nosuchtable/hcloud", "/snapshot/" + recordedHash + "/server/bad%20vendor"} {
		resp, body := bareGet(t, s, path)
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "/problems/not-found") {
			t.Errorf("%s: %d %s", path, resp.StatusCode, body)
		}
	}
	// A known route under an unknown hash, without gzip: the gzip check first.
	if resp, _ := bareGet(t, s, "/snapshot/"+unknown+"/vendor"); resp.StatusCode != http.StatusNotAcceptable {
		t.Errorf("gzip check: %d", resp.StatusCode)
	}
	s.Preparing(recordedHash, 1)
	if resp, _ := rawGet(t, s, "/snapshot/"+recordedHash+"/nosuchtable", http.Header{"Accept-Encoding": {"gzip"}}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown table while preparing: %d", resp.StatusCode)
	}
	if resp, _ := rawGet(t, s, "/snapshot/"+recordedHash+"/vendor", http.Header{"Accept-Encoding": {"gzip"}}); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("the preparing request was used up: %d", resp.StatusCode)
	}
}

// The vendors Keeper knows come from the recorded vendor table.
func TestCatalogUnknownBenchmarkAtAnyKnownVendor(t *testing.T) {
	c := newClient(keepertest.NewCatalog(t))
	if _, err := c.SnapshotFile(ctx, recordedHash, keeper.ScorePath("aws", "nosuch:bench")); err != nil {
		t.Errorf("a known vendor with no recorded files: %v", err)
	}
}
