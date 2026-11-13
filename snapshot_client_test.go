/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package keeper_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	keeper "github.com/SpareCores/sc-keeper-go"
)

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// recorder counts the requests of a fake Keeper and the waits of a client.
type recorder struct {
	mu     sync.Mutex
	reqs   []*http.Request
	sleeps []time.Duration
}

func (r *recorder) record(req *http.Request) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req.Clone(context.Background()))
	return len(r.reqs)
}

func (r *recorder) sleep(_ context.Context, d time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sleeps = append(r.sleeps, d)
	return nil
}

func (r *recorder) total() (n int, waited time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range r.sleeps {
		waited += d
	}
	return len(r.reqs), waited
}

const (
	testHash = "40ecd194b481b86a3237f51c9be6f590abcd53df"
	otherHex = "2b4bd195ff4897951e174b9b59adf704a002502b"
)

func serveSnapshotFile(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Cache-Control", "private, max-age=86400, immutable")
	w.Header().Set("SC-Data-Hash", testHash)
	w.Header().Set("SC-Data-Published-At", "2026-11-13T19:50:14Z")
	_, _ = w.Write(body)
}

func writeProblem(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("SC-Data-Hash", testHash)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func TestHealthSnapshotFields(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want keeper.Health
	}{
		"with snapshots": {
			`{"database_hash":"` + testHash + `","database_last_updated":1794600598.5,` +
				`"database_published_at":"2026-11-13T19:50:14Z","snapshot_hashes":["` + testHash + `","` + otherHex + `"]}`,
			keeper.Health{
				DatabaseHash:        testHash,
				DatabaseLastUpdated: time.Unix(1794600598, 500_000_000).UTC(),
				DatabasePublishedAt: time.Date(2026, 11, 13, 19, 50, 14, 0, time.UTC),
				SnapshotHashes:      []string{testHash, otherHex},
			},
		},
		"no published database": {
			`{"database_hash":null,"database_last_updated":0,"snapshot_hashes":[]}`,
			keeper.Health{DatabaseLastUpdated: time.Unix(0, 0).UTC(), SnapshotHashes: []string{}},
		},
		"an older Keeper": {
			`{"database_hash":"` + testHash + `","database_last_updated":10}`,
			keeper.Health{DatabaseHash: testHash, DatabaseLastUpdated: time.Unix(10, 0).UTC()},
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			got, err := keeper.New(srv.URL).Health(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got.DatabaseHash != tc.want.DatabaseHash || !got.DatabaseLastUpdated.Equal(tc.want.DatabaseLastUpdated) ||
				!got.DatabasePublishedAt.Equal(tc.want.DatabasePublishedAt) || strings.Join(got.SnapshotHashes, ",") != strings.Join(tc.want.SnapshotHashes, ",") {
				t.Errorf("Health = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestSnapshotFileRequestAndBody(t *testing.T) {
	const file = `{"vendor_id":["aws","aws"],"score":[1.5,null]}`
	var rec recorder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		serveSnapshotFile(w, gz(t, file))
	}))
	defer srv.Close()
	c := keeper.New(srv.URL, keeper.WithAPIKey("secret"), keeper.WithUserAgent("test-agent"))

	body, err := c.SnapshotFile(context.Background(), testHash, keeper.ScorePath("aws", "stress_ng:bestn"))
	if err != nil {
		t.Fatal(err)
	}
	r := rec.reqs[0]
	if r.URL.Path != "/snapshot/"+testHash+"/benchmark_score/aws/stress_ng:bestn" || r.URL.RawQuery != "" {
		t.Errorf("URL = %s", r.URL)
	}
	if strings.Contains(r.URL.RawPath, "%3A") {
		t.Errorf("the colon of the benchmark id is escaped: %s", r.URL.EscapedPath())
	}
	if r.Header.Get("Accept-Encoding") != "gzip" || r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("User-Agent") != "test-agent" {
		t.Errorf("headers = %v", r.Header)
	}
	if body.Version.Hash != testHash || !body.Version.PublishedAt.Equal(time.Date(2026, 11, 13, 19, 50, 14, 0, time.UTC)) {
		t.Errorf("Version = %+v", body.Version)
	}
	if !bytes.Equal(body.Gzip, gz(t, file)) || body.Header.Get("Cache-Control") == "" {
		t.Error("the gzip bytes or the header are not as received")
	}
	zr, err := body.Open()
	if err != nil {
		t.Fatal(err)
	}
	var plain bytes.Buffer
	if _, err := plain.ReadFrom(zr); err != nil || plain.String() != file {
		t.Errorf("Open: %q, %v", plain.String(), err)
	}
	_ = zr.Close()

	if err := body.Verify(sha(file)); err != nil {
		t.Errorf("Verify: %v", err)
	}
	if err := body.Verify(strings.ToUpper(sha(file))); err != nil {
		t.Errorf("Verify of an upper-case digest: %v", err)
	}
	// The digest is of the uncompressed bytes, not of the gzip body.
	sum := sha256.Sum256(body.Gzip)
	for _, bad := range []string{hex.EncodeToString(sum[:]), sha("other")} {
		if err := body.Verify(bad); !errors.Is(err, keeper.ErrChecksum) {
			t.Errorf("Verify(%.8s...) = %v, want ErrChecksum", bad, err)
		}
	}
	if err := (&keeper.SnapshotBody{Gzip: []byte("not gzip")}).Verify(sha(file)); err == nil {
		t.Error("Verify of a body that is not gzip passed")
	}

	if _, err := c.SnapshotFile(context.Background(), testHash, ""); err == nil {
		t.Error("a file without a path is accepted")
	}
}

func TestSnapshotManifest(t *testing.T) {
	manifest := `{"path":["benchmark","region/aws","vendor"],"rows":[162,33,8],"sha256":["a","b","c"]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/snapshot/"+testHash {
			t.Errorf("path = %s", r.URL.Path)
		}
		serveSnapshotFile(w, gz(t, manifest))
	}))
	defer srv.Close()
	m, v, err := keeper.New(srv.URL).SnapshotManifest(context.Background(), testHash)
	if err != nil {
		t.Fatal(err)
	}
	if v.Hash != testHash || len(m.Files) != 3 {
		t.Fatalf("manifest = %+v, version %+v", m, v)
	}
	if f, ok := m.Lookup(keeper.VendorTablePath("region", "aws")); !ok || f.Rows != 33 || f.SHA256 != "b" {
		t.Errorf("Lookup = %+v, %v", f, ok)
	}
	if f, ok := m.Lookup(keeper.TablePath("vendor")); !ok || f.Rows != 8 {
		t.Errorf("Lookup(vendor) = %+v, %v", f, ok)
	}
	if _, ok := m.Lookup("region/nope"); ok {
		t.Error("an unlisted path was found")
	}
}

func TestPathBuilders(t *testing.T) {
	if got := keeper.TablePath("vendor"); got != "vendor" {
		t.Errorf("TablePath = %q", got)
	}
	if got := keeper.VendorTablePath("server_price", "aws"); got != "server_price/aws" {
		t.Errorf("VendorTablePath = %q", got)
	}
	if got := keeper.ScorePath("hcloud", "stress_ng:bestn"); got != "benchmark_score/hcloud/stress_ng:bestn" {
		t.Errorf("ScorePath = %q", got)
	}
}

// Keeper charges a 401 11 credits: it is returned at once, never retried.
func TestUnauthorizedIsNeverRetried(t *testing.T) {
	for name, tc := range map[string]struct {
		key, detail string
		wantConfig  bool
	}{
		"no key":       {"", "Authentication required", false},
		"rejected key": {"wrong", "Invalid or expired token", true},
	} {
		t.Run(name, func(t *testing.T) {
			var rec recorder
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec.record(r)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "1") // would be waited for if it were retried
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"detail":"` + tc.detail + `"}`))
			}))
			defer srv.Close()
			opts := []keeper.Option{keeper.WithSleep(rec.sleep), keeper.WithRetries(5, time.Second, time.Minute)}
			if tc.key != "" {
				opts = append(opts, keeper.WithAPIKey(tc.key))
			}
			c := keeper.New(srv.URL, opts...)
			ctx := context.Background()
			for name, call := range map[string]func() error{
				"file":     func() error { _, err := c.SnapshotFile(ctx, testHash, "vendor"); return err },
				"manifest": func() error { _, _, err := c.SnapshotManifest(ctx, testHash); return err },
				"table":    func() error { _, err := c.Vendors(ctx); return err },
			} {
				n0, _ := rec.total()
				err := call()
				var ue *keeper.UnauthorizedError
				if !errors.As(err, &ue) {
					t.Fatalf("%s: err = %v, want *UnauthorizedError", name, err)
				}
				if ue.KeyConfigured != tc.wantConfig || ue.Detail != tc.detail {
					t.Errorf("%s: %+v", name, ue)
				}
				var he *keeper.HTTPError
				if !errors.As(err, &he) || he.StatusCode != http.StatusUnauthorized {
					t.Errorf("%s: the HTTPError is not reachable: %v", name, err)
				}
				if n, waited := rec.total(); n != n0+1 || waited != 0 {
					t.Errorf("%s: %d requests and %v waited, want 1 and 0", name, n-n0, waited)
				}
			}
		})
	}
}

func TestProblemErrors(t *testing.T) {
	notAvailable := `{"type":"/problems/snapshot-not-available","title":"Snapshot not available","status":404,` +
		`"requested":"` + otherHex + `","current":"` + testHash + `","available":["` + testHash + `"]}`
	for name, tc := range map[string]struct {
		status int
		body   string
		want   keeper.ProblemError
		is     func(error) bool
	}{
		"not available": {
			404, notAvailable,
			keeper.ProblemError{Status: 404, Type: keeper.ProblemSnapshotNotAvailable, Title: "Snapshot not available", Requested: otherHex, Current: testHash, Available: []string{testHash}},
			keeper.IsSnapshotNotAvailable,
		},
		"not found": {
			404, `{"type":"/problems/not-found","title":"Unknown snapshot","status":404}`,
			keeper.ProblemError{Status: 404, Type: keeper.ProblemNotFound, Title: "Unknown snapshot"},
			keeper.IsNotFound,
		},
		"406": {
			406, `{"type":"about:blank","title":"Send Accept-Encoding: gzip","status":406}`,
			keeper.ProblemError{Status: 406, Type: "about:blank", Title: "Send Accept-Encoding: gzip"},
			func(error) bool { return true },
		},
	} {
		t.Run(name, func(t *testing.T) {
			var rec recorder
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec.record(r)
				writeProblem(w, tc.status, tc.body)
			}))
			defer srv.Close()
			_, err := keeper.New(srv.URL, keeper.WithSleep(rec.sleep)).SnapshotFile(context.Background(), otherHex, "region/aws")
			var pe *keeper.ProblemError
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v, want *ProblemError", err)
			}
			got := *pe
			if got.Status != tc.want.Status || got.Type != tc.want.Type || got.Title != tc.want.Title ||
				got.Requested != tc.want.Requested || got.Current != tc.want.Current || strings.Join(got.Available, ",") != strings.Join(tc.want.Available, ",") {
				t.Errorf("ProblemError = %+v, want %+v", got, tc.want)
			}
			if !tc.is(err) {
				t.Error("the Is helper does not match")
			}
			if n, _ := rec.total(); n != 1 {
				t.Errorf("%d requests, want 1: a 404 or 406 is not retried", n)
			}
			// Only the one problem matches its helper.
			matches := 0
			for _, is := range []func(error) bool{keeper.IsSnapshotNotAvailable, keeper.IsSnapshotPreparing, keeper.IsNotFound} {
				if is(err) {
					matches++
				}
			}
			if name != "406" && matches != 1 || name == "406" && matches != 0 {
				t.Errorf("%d helpers match", matches)
			}
		})
	}
	if keeper.IsNotFound(errors.New("x")) || keeper.IsSnapshotNotAvailable(nil) {
		t.Error("a helper matches an unrelated error")
	}
}

func preparing(w http.ResponseWriter, retryAfter string) {
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	writeProblem(w, http.StatusServiceUnavailable,
		`{"type":"/problems/snapshot-preparing","title":"Snapshot is being prepared","status":503,"requested":"`+testHash+`","current":"`+testHash+`"}`)
}

func TestSnapshotPreparingIsRetried(t *testing.T) {
	var rec recorder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rec.record(r) <= 3 {
			preparing(w, "5")
			return
		}
		serveSnapshotFile(w, gz(t, `{}`))
	}))
	defer srv.Close()
	// No retries at all: the preparing waits are not counted as retries.
	c := keeper.New(srv.URL, keeper.WithSleep(rec.sleep), keeper.WithRetries(0, 0, 0))
	if _, err := c.SnapshotFile(context.Background(), testHash, "vendor"); err != nil {
		t.Fatal(err)
	}
	if n, waited := rec.total(); n != 4 || waited != 15*time.Second {
		t.Errorf("%d requests, %v waited; want 4 and 15s", n, waited)
	}
	for _, r := range rec.reqs[1:] {
		if r.URL.Path != rec.reqs[0].URL.Path {
			t.Errorf("a retry went to %s, not the same path", r.URL.Path)
		}
	}
}

func TestSnapshotPreparingBudget(t *testing.T) {
	for name, tc := range map[string]struct {
		retryAfter string
		opts       []keeper.Option
		wantReqs   int
		wantWait   time.Duration
	}{
		"default 5 s for about a minute": {"", nil, 13, 60 * time.Second},
		"Retry-After is honoured":        {"20", nil, 4, 60 * time.Second},
		"a Retry-After over the budget":  {"90", nil, 1, 0},
		"own budget":                     {"5", []keeper.Option{keeper.WithPreparingBudget(12 * time.Second)}, 3, 10 * time.Second},
		"no budget":                      {"5", []keeper.Option{keeper.WithPreparingBudget(0)}, 1, 0},
	} {
		t.Run(name, func(t *testing.T) {
			var rec recorder
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec.record(r)
				preparing(w, tc.retryAfter)
			}))
			defer srv.Close()
			opts := append([]keeper.Option{keeper.WithSleep(rec.sleep)}, tc.opts...)
			_, err := keeper.New(srv.URL, opts...).SnapshotFile(context.Background(), testHash, "vendor")
			if !keeper.IsSnapshotPreparing(err) {
				t.Fatalf("err = %v, want the snapshot-preparing problem", err)
			}
			if n, waited := rec.total(); n != tc.wantReqs || waited != tc.wantWait {
				t.Errorf("%d requests, %v waited; want %d and %v", n, waited, tc.wantReqs, tc.wantWait)
			}
		})
	}
}

// Retry-After: 0 (keepertest's) retries at once, and still ends.
func TestSnapshotPreparingZeroRetryAfter(t *testing.T) {
	var rec recorder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rec.record(r) <= 2 {
			preparing(w, "0")
			return
		}
		serveSnapshotFile(w, gz(t, `{}`))
	}))
	defer srv.Close()
	c := keeper.New(srv.URL, keeper.WithSleep(rec.sleep))
	if _, err := c.SnapshotFile(context.Background(), testHash, "vendor"); err != nil {
		t.Fatal(err)
	}
	if n, waited := rec.total(); n != 3 || waited != 0 {
		t.Errorf("%d requests, %v waited", n, waited)
	}

	always := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { preparing(w, "0") }))
	defer always.Close()
	if _, err := keeper.New(always.URL, keeper.WithSleep(rec.sleep)).SnapshotFile(context.Background(), testHash, "vendor"); !keeper.IsSnapshotPreparing(err) {
		t.Errorf("a Keeper that never finishes: %v", err)
	}
}

// 429 and other 5xx keep the existing retry, whatever the body says.
func TestSnapshotOtherRetries(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable} {
		var rec recorder
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if rec.record(r) <= 2 {
				w.WriteHeader(status)
				return
			}
			serveSnapshotFile(w, gz(t, `{}`))
		}))
		c := keeper.New(srv.URL, keeper.WithSleep(rec.sleep), keeper.WithRetries(4, time.Second, time.Minute))
		if _, err := c.SnapshotFile(context.Background(), testHash, "vendor"); err != nil {
			t.Errorf("status %d: %v", status, err)
		}
		if n, _ := rec.total(); n != 3 {
			t.Errorf("status %d: %d requests, want 3", status, n)
		}
		srv.Close()
	}
}

func TestSnapshotNotGzip(t *testing.T) {
	var rec recorder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	_, err := keeper.New(srv.URL, keeper.WithSleep(rec.sleep)).SnapshotFile(context.Background(), testHash, "vendor")
	if err == nil || !strings.Contains(err.Error(), "not gzip") {
		t.Errorf("err = %v", err)
	}
	if n, _ := rec.total(); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

func TestDecodeSnapshotRows(t *testing.T) {
	file := `{"vendor_id":["hcloud","hcloud"],"region_id":["fsn1","nbg1"],"storage_id":["block","block"],` +
		`"unit":["GB/month","GB/month"],"price":[0.04862879999999999,0.05],"currency":["USD","USD"],"status":["active","active"],"price_upfront":[0.0,0.0]}`
	body := &keeper.SnapshotBody{Gzip: gz(t, file)}
	rows, err := keeper.DecodeSnapshotRows[keeper.StoragePrice](body, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Price != 0.04862879999999999 || rows[1].RegionID != "nbg1" || rows[0].Unit != "GB/month" {
		t.Errorf("rows = %+v", rows)
	}
	if _, err := keeper.DecodeSnapshotRows[keeper.StoragePrice](body, 3); err == nil {
		t.Error("a wrong row count is accepted")
	}
	corrupt := &keeper.SnapshotBody{Gzip: body.Gzip[:len(body.Gzip)-6]}
	if _, err := keeper.DecodeSnapshotRows[keeper.StoragePrice](corrupt, 2); err == nil {
		t.Error("a truncated gzip body is accepted")
	}
}

// Error bodies are read through the gzip encoding too.
func TestGzippedErrorBodies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		if r.URL.Path == "/healthcheck" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write(gz(t, `{"detail":"Invalid or expired token"}`))
			return
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(gz(t, `{"type":"/problems/not-found","title":"Unknown snapshot","status":404}`))
	}))
	defer srv.Close()
	c := keeper.New(srv.URL, keeper.WithAPIKey("k"))
	var ue *keeper.UnauthorizedError
	if _, err := c.Health(context.Background()); !errors.As(err, &ue) || ue.Detail != "Invalid or expired token" {
		t.Errorf("401: %v", err)
	}
	if _, err := c.SnapshotFile(context.Background(), testHash, "vendor"); !keeper.IsNotFound(err) {
		t.Errorf("404: %v", err)
	}
}
