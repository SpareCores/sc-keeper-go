/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package keepertest replays a snapshot of Keeper API responses (see
// cmd/sc-snapshot) as a fake Keeper API, so tests never touch the network.
// The module's own snapshot is committed under testdata/snapshot (SnapshotDir).
//
// Requests are matched on path and on the parameters that select data:
// vendor, vendor_regions (as a set), best_price_allocation, allocation,
// currency (of /table/ paths and /server_prices only; on /servers it is a
// push-down filter), benchmark_id and benchmark_config (an exact string, as
// Keeper matches it). Push-down filter parameters and data_version are ignored,
// since the client re-applies every filter. Unknown queries get HTTP 404 with
// a message naming the closest recorded queries. /healthcheck is synthesised
// from the manifest. Other handlers (another API the code under test calls)
// can be mounted next to it with Handler.Mount.
package keepertest

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Manifest is the manifest.json of a snapshot directory. Keys this type does
// not name (a consumer may add its own) are ignored.
type Manifest struct {
	FetchedAt string `json:"fetched_at"`
	Keeper    struct {
		DatabaseHash        string            `json:"database_hash"`
		DatabaseLastUpdated string            `json:"database_last_updated"`
		Packages            map[string]string `json:"packages"`
	} `json:"keeper"`
	Vendors map[string]struct {
		Region string `json:"region"`
		Spot   bool   `json:"spot"`
	} `json:"vendors"`
	Queries []ManifestQuery `json:"queries"`
}

// ManifestQuery is one recorded query.
type ManifestQuery struct {
	Name   string         `json:"name"`
	Path   string         `json:"path"`
	Params map[string]any `json:"params"`
	File   string         `json:"file"`
	Rows   int            `json:"rows"`
	// Projection is "flat" for /server_prices (only the flat fields of each
	// row are stored, see package snapshot).
	Projection string `json:"projection,omitempty"`
}

// matchParams are the parameters that select data; everything else is a
// push-down filter or paging and is ignored.
var matchParams = []string{
	"vendor", "vendor_regions", "best_price_allocation", "allocation", "currency", "benchmark_id", "benchmark_config",
}

// SnapshotDir is the absolute path of the snapshot committed with this module
// (testdata/snapshot). It resolves against the module's source tree, so it
// works from the module cache and through a replace directive as well.
func SnapshotDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "testdata", "snapshot")
}

// LoadManifest reads manifest.json of a snapshot directory.
func LoadManifest(dir string) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("reading snapshot manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parsing snapshot manifest: %w", err)
	}
	return &m, nil
}

// KeeperRegion returns the Keeper region id the snapshot recorded for a
// vendor (from the vendor_regions parameter of its /servers queries).
func (m *Manifest) KeeperRegion(vendor string) (string, bool) {
	for _, q := range m.Queries {
		if q.Path != "/servers" || q.Params["vendor"] != vendor {
			continue
		}
		vr, _ := q.Params["vendor_regions"].(string)
		if _, region, ok := strings.Cut(vr, "~"); ok {
			return region, true
		}
	}
	return "", false
}

// Handler serves the snapshot.
type Handler struct {
	dir      string
	manifest *Manifest
	routes   map[string]ManifestQuery
	mounts   []mount

	mu       sync.Mutex
	bodies   map[string][]byte
	requests []*url.URL
}

// NewHandler loads the manifest of the snapshot in dir.
func NewHandler(dir string) (*Handler, error) {
	m, err := LoadManifest(dir)
	if err != nil {
		return nil, err
	}
	h := &Handler{dir: dir, manifest: m, routes: map[string]ManifestQuery{}, bodies: map[string][]byte{}}
	for _, q := range m.Queries {
		values := url.Values{}
		for k, v := range q.Params {
			if list, ok := v.([]any); ok {
				for _, item := range list {
					values.Add(k, fmt.Sprint(item))
				}
				continue
			}
			values.Set(k, fmt.Sprint(v))
		}
		h.routes[routeKey(q.Path, values)] = q
	}
	return h, nil
}

type mount struct {
	prefix  string
	handler http.Handler
}

// Mount serves every request whose path starts with prefix by handler, for the
// requests of other APIs the code under test makes to the same server. A
// mount is checked before the snapshot routes. Mount before serving.
func (h *Handler) Mount(prefix string, handler http.Handler) {
	h.mounts = append(h.mounts, mount{prefix, handler})
}

// Manifest returns the loaded manifest.
func (h *Handler) Manifest() *Manifest { return h.manifest }

// Requests returns the URLs of all requests served so far.
func (h *Handler) Requests() []*url.URL {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.requests)
}

func routeKey(path string, values url.Values) string {
	var parts []string
	for _, k := range matchParams {
		// currency is a push-down filter of /servers (price_max) but selects
		// the data of the price tables.
		if k == "currency" && !strings.HasPrefix(path, "/table/") && path != "/server_prices" {
			continue
		}
		vs := slices.Clone(values[k])
		slices.Sort(vs)
		vs = slices.Compact(vs)
		if len(vs) > 0 {
			parts = append(parts, k+"="+strings.Join(vs, ","))
		}
	}
	return path + "?" + strings.Join(parts, "&")
}

// integerParams are the /servers parameters Keeper declares as integers.
var integerParams = []string{
	"vcpus_min", "vcpus_max", "storage_count_min",
	"cpu_l1d_cache_min", "cpu_l1i_cache_min", "cpu_l2_cache_min", "cpu_l3_cache_min", "cpu_l3_cache_total_min",
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.requests = append(h.requests, r.URL)
	h.mu.Unlock()

	if r.Method != http.MethodGet {
		http.Error(w, "keepertest: only GET is supported", http.StatusMethodNotAllowed)
		return
	}
	for _, m := range h.mounts {
		if strings.HasPrefix(r.URL.Path, m.prefix) {
			m.handler.ServeHTTP(w, r)
			return
		}
	}
	if r.URL.Path == "/healthcheck" {
		h.serveHealth(w)
		return
	}
	// Like Keeper (HTTP 422, int_parsing): these parameters are integers.
	for _, name := range integerParams {
		if s := r.URL.Query().Get(name); s != "" {
			if _, err := strconv.Atoi(s); err != nil {
				http.Error(w, fmt.Sprintf("keepertest: %s must be an integer, got %q", name, s), http.StatusUnprocessableEntity)
				return
			}
		}
	}
	key := routeKey(r.URL.Path, r.URL.Query())
	q, ok := h.routes[key]
	if !ok {
		http.Error(w, h.notFound(key), http.StatusNotFound)
		return
	}
	body, err := h.body(q.File)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (h *Handler) notFound(key string) string {
	path, _, _ := strings.Cut(key, "?")
	var known []string
	for k := range h.routes {
		if strings.HasPrefix(k, path+"?") {
			known = append(known, "  "+k)
		}
	}
	slices.Sort(known)
	if len(known) > 10 {
		known = append(known[:10], fmt.Sprintf("  ... and %d more", len(known)-10))
	}
	return fmt.Sprintf("keepertest: no recorded response for %s\nrecorded queries on %s:\n%s\n",
		key, path, strings.Join(known, "\n"))
}

func (h *Handler) serveHealth(w http.ResponseWriter) {
	updated, err := time.Parse(time.RFC3339, h.manifest.Keeper.DatabaseLastUpdated)
	if err != nil {
		http.Error(w, "keepertest: bad database_last_updated in manifest: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"database_hash":         h.manifest.Keeper.DatabaseHash,
		"database_last_updated": float64(updated.Unix()),
		"packages":              h.manifest.Keeper.Packages,
	})
}

func (h *Handler) body(file string) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if b, ok := h.bodies[file]; ok {
		return b, nil
	}
	raw, err := os.ReadFile(filepath.Join(h.dir, file))
	if err != nil {
		return nil, fmt.Errorf("keepertest: %w", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("keepertest: %s: %w", file, err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("keepertest: %s: %w", file, err)
	}
	h.bodies[file] = b
	return b, nil
}

// Server is a running replay server.
type Server struct {
	*httptest.Server
	*Handler
}

// Close shuts the server down.
func (s *Server) Close() { s.Server.Close() }

// TB is the subset of testing.TB the helpers need.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
	Cleanup(func())
}

// New starts a replay server of the snapshot committed with this module; it is
// closed when the test ends.
func New(t TB) *Server {
	t.Helper()
	return NewFromDir(t, SnapshotDir())
}

// NewFromDir starts a replay server of the snapshot in dir; it is closed when
// the test ends.
func NewFromDir(t TB, dir string) *Server {
	t.Helper()
	h, err := NewHandler(dir)
	if err != nil {
		t.Fatalf("keepertest: %v", err)
	}
	return NewServer(t, h)
}

// NewServer starts a server of h (for a handler with mounts); it is closed
// when the test ends.
func NewServer(t TB, h *Handler) *Server {
	t.Helper()
	s := &Server{Server: httptest.NewServer(h), Handler: h}
	t.Cleanup(s.Close)
	return s
}
