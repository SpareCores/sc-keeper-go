/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package keepertest

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SpareCores/sc-keeper-go/columnar"
)

// The snapshot routes of Keeper (GET /snapshot/{hash}, .../{table},
// .../{table}/{vendor}, .../benchmark_score/{vendor}/{benchmark_id}) are
// served from the "snapshot" block of the recording: the manifest and the
// files as the gzip bytes Keeper sent, so that a client sees what it sees at
// Keeper. The block is made by cmd/sc-snapshot from a "snapshot:" query list
// entry.

// ManifestSnapshot is the "snapshot" block of manifest.json.
type ManifestSnapshot struct {
	// Hash and PublishedAt are the data version of the recording.
	Hash        string `json:"hash"`
	PublishedAt string `json:"published_at"`
	// Healthcheck is /healthcheck as recorded.
	Healthcheck map[string]any `json:"healthcheck"`
	// Manifest is Keeper's manifest of the hash (all of its files, also the
	// ones that were not recorded).
	Manifest SnapshotFile `json:"manifest"`
	// Files are the files that were recorded.
	Files []SnapshotFile `json:"files"`
}

// SnapshotFile is a recorded response: the gzip bytes in File (relative to
// the recording), the path below /snapshot/{hash}/ (empty for the manifest),
// the manifest's row count and SHA-256 of the uncompressed JSON, and the
// headers of the response that the recorder kept (SC-Data-*, Content-Type,
// Cache-Control).
type SnapshotFile struct {
	Path    string            `json:"path,omitempty"`
	File    string            `json:"file"`
	Rows    *int              `json:"rows,omitempty"`
	SHA256  string            `json:"sha256,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

const (
	// Cache-Control of a snapshot 200, and of an error.
	cacheSnapshot = "private, max-age=86400, immutable"
	cacheError    = "private, no-store"

	problemJSON = "application/problem+json"

	// Problem types, as Keeper writes them.
	ProblemSnapshotNotAvailable = "/problems/snapshot-not-available"
	ProblemSnapshotPreparing    = "/problems/snapshot-preparing"
)

var (
	hashPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)
	// namePattern is what Keeper accepts as a vendor or benchmark id.
	namePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_:.-]*$`)
)

// snapHash is one data version the server serves.
type snapHash struct {
	publishedAt time.Time
	manifest    []byte // gzip
	listed      map[string]bool
	files       map[string][]byte // gzip, by path
}

type snapshotState struct {
	hashes    map[string]*snapHash
	current   string
	key       string
	needKey   bool
	preparing map[string]int
	// vendors are the vendors with at least one recorded file; scoreColumns
	// are the columns of a recorded score file, in order.
	vendors      map[string]bool
	scoreColumns []string
}

func loadSnapshot(dir string, m *Manifest) (*snapshotState, error) {
	rec := m.Snapshot
	published, err := time.Parse(time.RFC3339, rec.PublishedAt)
	if err != nil {
		return nil, fmt.Errorf("keepertest: snapshot published_at: %w", err)
	}
	read := func(rel string) ([]byte, error) {
		b, err := os.ReadFile(filepath.Join(dir, rel)) //nolint:gosec // the recording directory
		if err != nil {
			return nil, fmt.Errorf("keepertest: %w", err)
		}
		return b, nil
	}
	h := &snapHash{publishedAt: published.UTC(), listed: map[string]bool{}, files: map[string][]byte{}}
	if h.manifest, err = read(rec.Manifest.File); err != nil {
		return nil, err
	}
	listed, err := manifestFiles(h.manifest)
	if err != nil {
		return nil, err
	}
	for _, f := range listed {
		h.listed[f.Path] = true
	}
	s := &snapshotState{
		hashes: map[string]*snapHash{rec.Hash: h}, current: rec.Hash,
		preparing: map[string]int{}, vendors: map[string]bool{},
	}
	for _, f := range rec.Files {
		if h.files[f.Path], err = read(f.File); err != nil {
			return nil, err
		}
		if f.Path == tableVendor {
			if err := s.addVendorIDs(h.files[f.Path]); err != nil {
				return nil, err
			}
		}
		if vendor := vendorOf(f.Path); vendor != "" {
			s.vendors[vendor] = true
		}
		if strings.HasPrefix(f.Path, "benchmark_score/") && s.scoreColumns == nil {
			if s.scoreColumns, err = columnNames(h.files[f.Path]); err != nil {
				return nil, fmt.Errorf("keepertest: %s: %w", f.Path, err)
			}
		}
	}
	return s, nil
}

// addVendorIDs takes the vendors of the recorded vendor table: those are the
// vendors Keeper knows, whether or not their files were recorded.
func (s *snapshotState) addVendorIDs(gz []byte) error {
	raw, err := gunzip(gz)
	if err != nil {
		return fmt.Errorf("keepertest: vendor table: %w", err)
	}
	rows, err := columnar.DecodeRows[struct {
		VendorID string `json:"vendor_id"`
	}](bytes.NewReader(raw), -1)
	if err != nil {
		return fmt.Errorf("keepertest: vendor table: %w", err)
	}
	for _, r := range rows {
		s.vendors[r.VendorID] = true
	}
	return nil
}

// vendorOf is the vendor of a per-vendor path ("" for a global table).
func vendorOf(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

type manifestRow struct {
	Path   string `json:"path"`
	Rows   int    `json:"rows"`
	SHA256 string `json:"sha256"`
}

func gunzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(zr)
}

func gzipBytes(b []byte) []byte {
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return out.Bytes()
}

func manifestFiles(gz []byte) ([]manifestRow, error) {
	raw, err := gunzip(gz)
	if err != nil {
		return nil, fmt.Errorf("keepertest: snapshot manifest: %w", err)
	}
	rows, err := columnar.DecodeRows[manifestRow](bytes.NewReader(raw), -1)
	if err != nil {
		return nil, fmt.Errorf("keepertest: snapshot manifest: %w", err)
	}
	return rows, nil
}

func encodeManifest(rows []manifestRow) []byte {
	path, nrows, sum := make([]any, 0, len(rows)), make([]any, 0, len(rows)), make([]any, 0, len(rows))
	for _, r := range rows {
		path, nrows, sum = append(path, r.Path), append(nrows, r.Rows), append(sum, r.SHA256)
	}
	raw := []byte(`{"path":` + mustJSON(path) + `,"rows":` + mustJSON(nrows) + `,"sha256":` + mustJSON(sum) + `}`)
	return gzipBytes(raw)
}

func mustJSON(v any) string {
	if v == nil {
		return "[]"
	}
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// columnNames returns the keys of a columnar file, in order.
func columnNames(gz []byte) ([]string, error) {
	raw, err := gunzip(gz)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	var names []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		names = append(names, tok.(string)) //nolint:forcetypeassert // an object key
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return names, nil
}

// RequireAPIKey makes the snapshot routes need "Authorization: Bearer <key>":
// a snapshot request without the header gets Keeper's 401 "Authentication
// required". As at Keeper (AuthGuardMiddleware), an Authorization header that
// is not the Bearer key gets 401 "Invalid or expired token" on every route,
// /healthcheck included; without the header the other routes stay open. Both
// 401s are plain application/json.
func (h *Handler) RequireAPIKey(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.snap != nil {
		h.snap.key, h.snap.needKey = key, true
	}
}

// Preparing makes the first n requests that name the hash answer 503
// snapshot-preparing with "Retry-After: 0", as a replica still downloading
// the snapshot does.
func (h *Handler) Preparing(hash string, n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.snap != nil {
		h.snap.preparing[hash] = n
	}
}

// AddHash makes a second data version available: the files of the recording
// with the paths of overrides replaced by the given (uncompressed, columnar
// JSON) bytes, and a manifest with the row counts and SHA-256 of those
// recomputed. A path that the recording does not list is added to the
// manifest. The hash is published an hour after the newest one so far. It is
// served next to the others; /healthcheck keeps pointing at the first until
// SetCurrent.
func (h *Handler) AddHash(hash string, overrides map[string][]byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.snap
	if s == nil {
		return errors.New("keepertest: the recording has no snapshot block")
	}
	if !hashPattern.MatchString(hash) {
		return fmt.Errorf("keepertest: %q is not a snapshot hash", hash)
	}
	if _, dup := s.hashes[hash]; dup {
		return fmt.Errorf("keepertest: hash %s is already served", hash)
	}
	base := s.hashes[s.current]
	rows, err := manifestFiles(base.manifest)
	if err != nil {
		return err
	}
	nh := &snapHash{listed: map[string]bool{}, files: map[string][]byte{}}
	for _, other := range s.hashes {
		if other.publishedAt.After(nh.publishedAt) {
			nh.publishedAt = other.publishedAt
		}
	}
	nh.publishedAt = nh.publishedAt.Add(time.Hour)
	maps.Copy(nh.files, base.files)
	for p, body := range overrides {
		n, err := countRows(body)
		if err != nil {
			return fmt.Errorf("keepertest: override %s: %w", p, err)
		}
		sum := sha256.Sum256(body)
		entry := manifestRow{Path: p, Rows: n, SHA256: hex.EncodeToString(sum[:])}
		if i := slices.IndexFunc(rows, func(r manifestRow) bool { return r.Path == p }); i >= 0 {
			rows[i] = entry
		} else {
			rows = append(rows, entry)
		}
		nh.files[p] = gzipBytes(body)
		if vendor := vendorOf(p); vendor != "" {
			s.vendors[vendor] = true
		}
	}
	slices.SortFunc(rows, func(a, b manifestRow) int { return strings.Compare(a.Path, b.Path) })
	for _, r := range rows {
		nh.listed[r.Path] = true
	}
	nh.manifest = encodeManifest(rows)
	s.hashes[hash] = nh
	return nil
}

// countRows is the number of rows of a columnar file: the length of its first
// column.
func countRows(body []byte) (int, error) {
	var cols map[string][]json.RawMessage
	if err := json.Unmarshal(body, &cols); err != nil {
		return 0, err
	}
	n := -1
	for name, c := range cols {
		if n >= 0 && len(c) != n {
			return 0, fmt.Errorf("column %s has %d rows, the others %d", name, len(c), n)
		}
		n = len(c)
	}
	return max(n, 0), nil
}

// SetCurrent makes a hash the one /healthcheck reports and the 404s name as
// current, as when Keeper loads a new database.
func (h *Handler) SetCurrent(hash string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.snap == nil || h.snap.hashes[hash] == nil {
		return fmt.Errorf("keepertest: hash %q is not served", hash)
	}
	h.snap.current = hash
	return nil
}

// Expire makes a hash unavailable: snapshot-not-available, as when a replica
// has dropped its files. The current hash cannot expire: SetCurrent another
// first.
func (h *Handler) Expire(hash string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.snap == nil || h.snap.hashes[hash] == nil:
		return fmt.Errorf("keepertest: hash %q is not served", hash)
	case h.snap.current == hash:
		return fmt.Errorf("keepertest: hash %s is the current one", hash)
	}
	delete(h.snap.hashes, hash)
	return nil
}

// available lists the served hashes, newest first.
func (s *snapshotState) available() []string {
	out := make([]string, 0, len(s.hashes))
	for hash := range s.hashes {
		out = append(out, hash)
	}
	slices.SortFunc(out, func(a, b string) int {
		if c := s.hashes[b].publishedAt.Compare(s.hashes[a].publishedAt); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	return out
}

func acceptsGzip(header string) bool {
	star, explicit := false, false
	gz := false
	for part := range strings.SplitSeq(header, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		q := 1.0
		if _, v, ok := strings.Cut(strings.ReplaceAll(params, " ", ""), "q="); ok {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				q = f
			}
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "gzip":
			explicit, gz = true, q > 0
		case "*":
			star = q > 0
		}
	}
	if explicit {
		return gz
	}
	return star
}

// guardAuth answers 401 "Invalid or expired token" to a request whose
// Authorization header is not the required Bearer key, on any route, and
// reports whether it did.
func (h *Handler) guardAuth(w http.ResponseWriter, r *http.Request) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.snap
	auth := r.Header.Get("Authorization")
	if s == nil || !s.needKey || auth == "" {
		return false
	}
	if got, ok := strings.CutPrefix(auth, "Bearer "); ok && got == s.key {
		return false
	}
	s.unauthorized(w, "Invalid or expired token")
	return true
}

func (s *snapshotState) unauthorized(w http.ResponseWriter, detail string) {
	w.Header().Set("Cache-Control", cacheError)
	if cur := s.hashes[s.current]; cur != nil {
		w.Header().Set("SC-Data-Hash", s.current)
		w.Header().Set("SC-Data-Published-At", cur.publishedAt.Format(time.RFC3339))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("WWW-Authenticate", "Bearer")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"detail": detail})
}

const (
	tableVendor   = "vendor"
	tableCurrency = "currency"
)

// known are the tables of Keeper's snapshot routes.
var (
	globalTables = []string{tableVendor, "country", "compliance_framework", "benchmark", tableCurrency}
	vendorTables = []string{
		"region", "zone", "server", "server_description", "server_price", "storage", "storage_price",
		"traffic_price", "ipv4_price", "database", "database_price", "database_storage",
		"database_storage_price", "vendor_compliance_link",
	}
)

// routeKnown says whether a path below the hash is one of Keeper's routes with
// valid names (vendor and benchmark id): anything else is a plain not-found,
// whatever the hash and the headers.
func routeKnown(path string) bool {
	parts := strings.Split(path, "/")
	switch {
	case len(parts) == 1:
		return slices.Contains(globalTables, parts[0])
	case len(parts) == 2:
		return slices.Contains(vendorTables, parts[0]) && namePattern.MatchString(parts[1])
	case len(parts) == 3:
		return parts[0] == "benchmark_score" && namePattern.MatchString(parts[1]) && namePattern.MatchString(parts[2])
	}
	return false
}

func (h *Handler) serveSnapshot(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/snapshot/")
	hash, path, slash := strings.Cut(rest, "/")

	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.snap
	if s == nil {
		http.Error(w, "keepertest: the recording has no snapshot block", http.StatusNotFound)
		return
	}
	cur := s.hashes[s.current]
	errHeaders := func() {
		w.Header().Set("Cache-Control", cacheError)
		if cur != nil {
			w.Header().Set("SC-Data-Hash", s.current)
			w.Header().Set("SC-Data-Published-At", cur.publishedAt.Format(time.RFC3339))
		}
	}
	problem := func(status int, typ, title string, extra map[string]any) {
		body := map[string]any{"type": typ, "title": title, "status": status}
		maps.Copy(body, extra)
		errHeaders()
		w.Header().Set("Content-Type", problemJSON)
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	notFound := func(title string) {
		problem(http.StatusNotFound, "/problems/not-found", title, nil)
	}

	// Auth comes before everything else, the gzip check included. A wrong
	// Authorization header was answered by guardAuth already.
	if s.needKey && r.Header.Get("Authorization") == "" {
		s.unauthorized(w, "Authentication required")
		return
	}
	// A path that is none of Keeper's routes is a not-found under any hash.
	if slash && !routeKnown(path) {
		notFound("Unknown snapshot")
		return
	}
	if !acceptsGzip(r.Header.Get("Accept-Encoding")) {
		problem(http.StatusNotAcceptable, "about:blank", "Send Accept-Encoding: gzip", nil)
		return
	}
	data := s.hashes[hash]
	if !hashPattern.MatchString(hash) || data == nil {
		problem(http.StatusNotFound, ProblemSnapshotNotAvailable, "Snapshot not available",
			map[string]any{"requested": hash, "current": s.current, "available": s.available()})
		return
	}
	if n := s.preparing[hash]; n > 0 {
		s.preparing[hash] = n - 1
		w.Header().Set("Retry-After", "0")
		problem(http.StatusServiceUnavailable, ProblemSnapshotPreparing, "Snapshot being prepared",
			map[string]any{"requested": hash, "current": s.current})
		return
	}

	var body []byte
	switch {
	case !slash:
		body = data.manifest
	case path == "":
		notFound("Unknown snapshot")
		return
	default:
		var ok bool
		if body, ok = s.file(data, path); !ok {
			notFound(s.missingTitle(data, path))
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Vary", "Accept-Encoding")
	w.Header().Set("Cache-Control", cacheSnapshot)
	w.Header().Set("SC-Data-Hash", hash)
	w.Header().Set("SC-Data-Published-At", data.publishedAt.Format(time.RFC3339))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}

// file finds the gzip bytes of a path: a recorded file, or the empty arrays
// Keeper answers for a benchmark it has no scores of at a vendor it knows.
func (s *snapshotState) file(data *snapHash, path string) ([]byte, bool) {
	if gz, ok := data.files[path]; ok {
		return gz, true
	}
	if data.listed[path] {
		return nil, false
	}
	parts := strings.Split(path, "/")
	if len(parts) == 3 && parts[0] == "benchmark_score" && s.vendors[parts[1]] && s.scoreColumns != nil {
		cols := make([]string, len(s.scoreColumns))
		for i, c := range s.scoreColumns {
			cols[i] = strconv.Quote(c) + ":[]"
		}
		return gzipBytes([]byte("{" + strings.Join(cols, ",") + "}")), true
	}
	return nil, false
}

// missingTitle explains a 404 of a valid path: a path the manifest lists but
// the recording lacks names itself (a bug of the test, not a case of Keeper).
func (*snapshotState) missingTitle(data *snapHash, path string) string {
	if data.listed[path] {
		return fmt.Sprintf("keepertest: %s is in the manifest but was not recorded", path)
	}
	return "Unknown snapshot"
}

// health is the /healthcheck of a recording with a snapshot block.
func (s *snapshotState) health(m *Manifest, rec *ManifestSnapshot) map[string]any {
	out := map[string]any{}
	maps.Copy(out, rec.Healthcheck)
	if _, ok := out["packages"]; !ok {
		out["packages"] = m.Keeper.Packages
	}
	out["database_hash"] = s.current
	out["database_published_at"] = s.hashes[s.current].publishedAt.Format(time.RFC3339)
	out["snapshot_hashes"] = s.available()
	return out
}
