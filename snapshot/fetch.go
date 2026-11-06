/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package snapshot

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Options configures Fetch.
type Options struct {
	// Dir is the output directory. It is replaced as a whole when the fetch
	// succeeds; while it runs the files are written to Dir+".tmp".
	Dir string
	// Resume reuses the queries that an earlier run already wrote, in Dir or
	// in an interrupted run's Dir+".tmp": a query is skipped when its file
	// exists, is complete (its SHA-256 matches the one recorded), belongs to the
	// same query (name, path, parameters) and was fetched from the Keeper
	// database that is served now (same database hash). Everything else is
	// fetched again. Without Resume the whole snapshot is fetched.
	Resume bool
	// KeeperURL is the Keeper API (default https://keeper.sparecores.net).
	KeeperURL string
	// APIKey is sent as a bearer token; /table/server_prices needs one.
	APIKey string
	// HTTPClient replaces the default (5 minute timeout).
	HTTPClient *http.Client
	// Retries is the number of retries per request (default 5).
	Retries int
	// Sleep replaces time.Sleep-like waiting (tests).
	Sleep func(ctx context.Context, d time.Duration) error
	// Now replaces the clock of fetched_at (tests).
	Now func() time.Time
	// Log, if set, receives one progress line per query.
	Log func(format string, args ...any)
}

// Manifest is manifest.json: what was fetched, from which database.
type Manifest struct {
	FetchedAt string         `json:"fetched_at"`
	KeeperURL string         `json:"keeper_url"`
	Keeper    ManifestKeeper `json:"keeper"`
	// Vendors are the vendor expansions of the query list.
	Vendors map[string]ManifestVendor `json:"vendors"`
	Queries []ManifestQuery           `json:"queries"`
}

// ManifestKeeper identifies the Keeper database the data was read from.
type ManifestKeeper struct {
	Packages            map[string]string `json:"packages"`
	DatabaseHash        string            `json:"database_hash"`
	DatabaseLastUpdated string            `json:"database_last_updated"`
}

// ManifestVendor is a vendor of the query list.
type ManifestVendor struct {
	Region string `json:"region"`
	Spot   bool   `json:"spot"`
}

// ManifestQuery is one recorded query. SHA256 is of the canonical JSON of the
// response (sorted keys, compact), not of the gzipped file.
type ManifestQuery struct {
	Name   string         `json:"name"`
	Path   string         `json:"path"`
	Params map[string]any `json:"params"`
	File   string         `json:"file"`
	Rows   *int           `json:"rows"`
	SHA256 string         `json:"sha256"`
	// Projection is set when the stored rows are a reduced form of the
	// response: "flat" keeps only Fields of every row. /server_prices embeds
	// full server, vendor, region and zone objects in every row (about 4 KB),
	// so it is always stored flat.
	Projection string   `json:"projection,omitempty"`
	Fields     []string `json:"fields,omitempty"`
}

// Snapshot is a fetched snapshot, written to Dir.
type Snapshot struct {
	Manifest Manifest
	Dir      string
	// Reused is the number of queries Resume took over from an earlier run.
	Reused int
}

const progressFile = "progress.json"

type fetcher struct {
	opts   Options
	client *http.Client
	// cache holds the small responses expansion needs (/table/region,
	// /benchmark_configs), so that a query list asking for them too does not
	// fetch them twice.
	cache map[string][]byte
}

func (f *fetcher) logf(format string, args ...any) {
	if f.opts.Log != nil {
		f.opts.Log(format, args...)
	}
}

// planned is one request of the expanded query list.
type planned struct {
	name   string
	path   string
	params map[string]any
}

func (p planned) file() string { return p.name + ".json.gz" }

// Fetch runs every query of the list against Keeper and writes the result to
// opts.Dir. The whole list is expanded and validated first (unknown or retired
// regions, unknown benchmark configs, colliding file names) so that a bad list
// fails before any data is fetched. Each response is streamed to its file as
// it arrives; memory is bounded by the rows of one response, not by the
// snapshot. It fails if the Keeper database changes while it runs. The result
// depends only on the data, except for fetched_at.
func Fetch(ctx context.Context, list *QueryList, opts Options) (*Snapshot, error) {
	if opts.Dir == "" {
		return nil, errors.New("Options.Dir is required") //nolint:staticcheck // user-facing message
	}
	if opts.KeeperURL == "" {
		opts.KeeperURL = "https://keeper.sparecores.net"
	}
	opts.KeeperURL = strings.TrimRight(opts.KeeperURL, "/")
	if opts.Retries == 0 {
		opts.Retries = 5
	}
	if opts.Sleep == nil {
		opts.Sleep = sleep
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	opts.Dir = filepath.Clean(opts.Dir)
	f := &fetcher{opts: opts, client: opts.HTTPClient, cache: map[string][]byte{}}
	if f.client == nil {
		f.client = &http.Client{Timeout: 5 * time.Minute}
	}

	health, err := f.health(ctx)
	if err != nil {
		return nil, err
	}
	plan, err := f.plan(ctx, list)
	if err != nil {
		return nil, err
	}

	unlock, err := lockDir(opts.Dir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	stage := opts.Dir + ".tmp"
	if err := recoverReplace(opts.Dir, stage); err != nil {
		return nil, err
	}
	prior := map[string]ManifestQuery{}
	inDir := map[string]bool{} // prior queries whose file is in opts.Dir, not in the staging directory
	if opts.Resume {
		// The existing snapshot first, the interrupted run's files over it.
		for _, src := range []struct{ dir, file string }{{opts.Dir, "manifest.json"}, {stage, progressFile}} {
			m, err := loadManifest(filepath.Join(src.dir, src.file))
			if err != nil || m.Keeper.DatabaseHash != health.DatabaseHash {
				continue
			}
			for _, q := range m.Queries {
				if verify(filepath.Join(src.dir, filepath.FromSlash(q.File)), q) == nil {
					prior[q.File] = q
					inDir[q.File] = src.dir == opts.Dir
				}
			}
		}
	} else if err := os.RemoveAll(stage); err != nil {
		return nil, fmt.Errorf("clearing %s: %w", stage, err)
	}
	if err := os.MkdirAll(stage, 0o750); err != nil {
		return nil, fmt.Errorf("writing snapshot: %w", err)
	}

	snap := &Snapshot{Dir: opts.Dir}
	m := &snap.Manifest
	m.KeeperURL = opts.KeeperURL
	m.Vendors = map[string]ManifestVendor{}
	for _, v := range list.Vendors {
		m.Vendors[v.Name] = ManifestVendor{Region: v.Region, Spot: v.Spot}
	}
	m.Keeper = ManifestKeeper{
		Packages:            health.Packages,
		DatabaseHash:        health.DatabaseHash,
		DatabaseLastUpdated: time.Unix(int64(health.DatabaseLastUpdated), 0).UTC().Format("2006-01-02T15:04:05-07:00"),
	}

	for i, p := range plan {
		dest := filepath.Join(stage, filepath.FromSlash(p.file()))
		if q, ok := prior[p.file()]; ok && sameQuery(q, p) {
			if inDir[p.file()] {
				if err := copyFile(filepath.Join(opts.Dir, filepath.FromSlash(q.File)), dest); err != nil {
					return nil, err
				}
			}
			f.logf("[%d/%d] reusing %s", i+1, len(plan), p.name)
			m.Queries = append(m.Queries, q)
			snap.Reused++
			continue
		}
		f.logf("[%d/%d] fetching %s", i+1, len(plan), p.name)
		q, err := f.fetchQuery(ctx, p, dest)
		if err != nil {
			return nil, err
		}
		m.Queries = append(m.Queries, q)
		if err := writeProgress(stage, m); err != nil {
			return nil, err
		}
	}

	after, err := f.health(ctx)
	if err != nil {
		return nil, err
	}
	if after.DatabaseHash != health.DatabaseHash {
		return nil, errors.New("Keeper data changed during the fetch, run again") //nolint:staticcheck // user-facing message
	}
	m.FetchedAt = opts.Now().UTC().Format("2006-01-02T15:04:05-07:00")
	if err := finish(stage, opts.Dir, m, plan); err != nil {
		return nil, err
	}
	return snap, nil
}

// finish prunes the staging directory to the planned files (leftovers of
// killed runs and of queries since removed from the list must not reach the
// output), writes the manifest and swaps the directory into place: the old
// output is renamed to <dir>.old, the staging directory to <dir>, and the old
// output removed. At every step either <dir> is the old snapshot, or
// recoverReplace (the start of the next run) completes or undoes the swap.
func finish(stage, dir string, m *Manifest, plan []planned) error {
	keep := map[string]bool{"manifest.json": true}
	for _, p := range plan {
		keep[p.file()] = true
	}
	if err := pruneTo(stage, keep); err != nil {
		return fmt.Errorf("pruning %s: %w", stage, err)
	}
	manifest, err := encode(*m, manifestFormat)
	if err != nil {
		return fmt.Errorf("encoding manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stage, "manifest.json"), append(manifest, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing manifest: %w", err)
	}
	old := dir + ".old"
	if err := os.RemoveAll(old); err != nil {
		return fmt.Errorf("replacing %s: %w", dir, err)
	}
	if err := os.Rename(dir, old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("replacing %s: %w", dir, err)
	}
	if err := os.Rename(stage, dir); err != nil {
		return fmt.Errorf("replacing %s: %w", dir, err)
	}
	return os.RemoveAll(old)
}

// pruneTo removes every file below dir whose slash-separated relative path is
// not in keep, and the directories that end up empty.
func pruneTo(dir string, keep map[string]bool) error {
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if keep[filepath.ToSlash(rel)] {
			return nil
		}
		return os.Remove(path)
	})
	if err != nil {
		return err
	}
	var dirs []string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() && path != dir {
			dirs = append(dirs, path)
		}
		return nil
	})
	for _, d := range slices.Backward(dirs) {
		_ = os.Remove(d) // fails, and is kept, when not empty
	}
	return nil
}

// recoverReplace repairs what a run killed inside finish left: when <dir> is
// missing, a finished staging directory (it has its manifest) becomes <dir>,
// otherwise <dir>.old is put back; a leftover <dir>.old next to <dir> is
// removed. Resume then works as after any interruption.
func recoverReplace(dir, stage string) error {
	old := dir + ".old"
	if _, err := os.Stat(old); err != nil {
		return nil
	}
	if _, err := os.Stat(dir); err != nil {
		from := old
		if _, err := os.Stat(filepath.Join(stage, "manifest.json")); err == nil {
			from = stage
		}
		if err := os.Rename(from, dir); err != nil {
			return fmt.Errorf("recovering %s: %w", dir, err)
		}
	}
	return os.RemoveAll(old)
}

// writeProgress records the queries finished so far, for Resume.
func writeProgress(stage string, m *Manifest) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := filepath.Join(stage, progressFile+".part")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("writing progress: %w", err)
	}
	return os.Rename(tmp, filepath.Join(stage, progressFile))
}

func loadManifest(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the output directory chosen by the user
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // parameters compare as written, -1 stays -1
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// sameQuery says whether a recorded query is the planned one.
func sameQuery(q ManifestQuery, p planned) bool {
	if q.Name != p.name || q.Path != p.path || q.File != p.file() {
		return false
	}
	want, err1 := encode(orEmpty(p.params), compactFormat)
	have, err2 := encode(orEmpty(q.Params), compactFormat)
	return err1 == nil && err2 == nil && bytes.Equal(want, have)
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// verify checks that a query's file is complete and the recorded one: it
// decompresses to the canonical JSON the manifest has the SHA-256 of.
func verify(path string, q ManifestQuery) error {
	file, err := os.Open(path) //nolint:gosec // the output directory chosen by the user
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	zr, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(h, zr); err != nil { // also checks the gzip trailer
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != q.SHA256 {
		return errors.New("content differs from the manifest")
	}
	return nil
}

func copyFile(from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o750); err != nil {
		return fmt.Errorf("writing snapshot: %w", err)
	}
	in, err := os.Open(from) //nolint:gosec // the output directory chosen by the user
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // as above
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// plan expands the query list into the requests to make, in order, and checks
// all of it: regions and benchmark configs resolve, file names are unique. It
// reads only /table/region and /benchmark_configs. All problems are reported
// together.
func (f *fetcher) plan(ctx context.Context, list *QueryList) ([]planned, error) {
	var plan []planned
	var errs []error
	for _, e := range list.Queries {
		plan = append(plan, planned{e.Name, e.Path, e.Params})
	}

	// Benchmarks listed more than once under one id (Geekbench single- and
	// multi-core) would collide on {benchmark_slug}: their slugs carry a short
	// hash of the config. An id used once keeps its plain slug.
	perID := map[string]int{}
	for _, b := range list.Benchmarks {
		perID[b.ID]++
	}

	vendorVars := map[string]map[string]string{}
	benchVars := map[int]map[string]string{}
	var regions []regionRow
	haveRegions := false
	for _, v := range list.Vendors {
		for _, e := range list.VendorQueries {
			if e.RequiresSpot && !v.Spot {
				continue
			}
			vars, ok := vendorVars[v.Name]
			if !ok {
				if !haveRegions {
					raw, err := f.get(ctx, "/table/region", nil)
					if err != nil {
						return nil, err
					}
					if err := json.Unmarshal(raw, &regions); err != nil {
						return nil, fmt.Errorf("/table/region: %w", err)
					}
					haveRegions = true
				}
				id, err := resolveRegion(regions, v)
				if err != nil {
					errs = append(errs, err)
				} else {
					vars = map[string]string{"vendor": v.Name, "region": v.Region, "region_id": id}
				}
				vendorVars[v.Name] = vars
			}
			if vars == nil {
				continue
			}
			if !e.Benchmarks {
				plan = append(plan, planned{expandString(e.Name, vars), e.Path, expandParams(e.Params, vars)})
				continue
			}
			for i, b := range list.Benchmarks {
				bvars, ok := benchVars[i]
				if !ok {
					config, err := f.canonicalConfig(ctx, b)
					if err != nil {
						errs = append(errs, err)
					} else {
						slug := strings.ReplaceAll(b.ID, ":", "_")
						if perID[b.ID] > 1 {
							hash, err := configHash(b.Config)
							if err != nil {
								return nil, err
							}
							slug += "-" + hash
						}
						bvars = map[string]string{"benchmark_id": b.ID, "benchmark_slug": slug, "benchmark_config": config}
					}
					benchVars[i] = bvars
				}
				if bvars == nil {
					continue
				}
				bv := maps.Clone(vars)
				maps.Copy(bv, bvars)
				plan = append(plan, planned{expandString(e.Name, bv), e.Path, expandParams(e.Params, bv)})
			}
		}
	}

	files := map[string]string{}
	for _, p := range plan {
		if other, dup := files[p.file()]; dup {
			errs = append(errs, fmt.Errorf("two queries write %s (%s and %s)", p.file(), other, p.name))
			continue
		}
		files[p.file()] = p.name
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("query list: %w", err)
	}
	return plan, nil
}

// configHash is the first 8 hex digits of the SHA-256 of the canonical JSON of
// a benchmark config (sorted keys, compact), the same for the same config on
// every run and machine.
func configHash(config map[string]any) (string, error) {
	canonical, err := encode(orEmpty(config), compactFormat)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:4]), nil
}

type regionRow struct {
	VendorID     string   `json:"vendor_id"`
	RegionID     string   `json:"region_id"`
	APIReference string   `json:"api_reference"`
	Status       string   `json:"status"`
	Aliases      []string `json:"aliases"`
}

func (r regionRow) retired() bool { return r.Status == "inactive" }

// resolveRegion finds the Keeper region id of a vendor's region, given as
// region_id, api_reference or alias. Active regions win over retired ones, and
// a match by region_id over api_reference over alias; a name that only matches
// retired regions, or two active regions equally well, is an error.
func resolveRegion(regions []regionRow, v Vendor) (string, error) {
	type match struct {
		row  regionRow
		rank int
	}
	var matches []match
	for _, r := range regions {
		if r.VendorID != v.Name {
			continue
		}
		switch {
		case r.RegionID == v.Region:
			matches = append(matches, match{r, 0})
		case r.APIReference == v.Region:
			matches = append(matches, match{r, 1})
		case slices.Contains(r.Aliases, v.Region):
			matches = append(matches, match{r, 2})
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("region %s of %s not found in /table/region", v.Region, v.Name)
	}
	best := matches[0]
	for _, m := range matches[1:] {
		if m.row.retired() != best.row.retired() {
			if best.row.retired() {
				best = m
			}
			continue
		}
		if m.rank < best.rank {
			best = m
		}
	}
	if best.row.retired() {
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, m.row.RegionID)
		}
		return "", fmt.Errorf("region %s of %s only matches retired (inactive) regions: %s; use an active region",
			v.Region, v.Name, strings.Join(ids, ", "))
	}
	for _, m := range matches {
		if m.row.RegionID != best.row.RegionID && !m.row.retired() && m.rank == best.rank {
			return "", fmt.Errorf("region %s of %s is ambiguous: active regions %s and %s",
				v.Region, v.Name, best.row.RegionID, m.row.RegionID)
		}
	}
	return best.row.RegionID, nil
}

// store canonicalises a response read from r and writes it gzipped to dest.
// Lists are sorted (Keeper's order among equally priced rows is not stable),
// keys are sorted, and the gzip header carries no time, so identical data
// gives identical files. List rows are read one at a time and kept only in
// their encoded form, so nothing like the whole parsed response is held.
func store(r io.Reader, p planned, dest string) (ManifestQuery, error) {
	q := ManifestQuery{Name: p.name, Path: p.path, Params: p.params, File: p.file()}
	if q.Params == nil {
		q.Params = map[string]any{}
	}
	flat := p.path == serverPricesPath
	if flat {
		q.Projection, q.Fields = projectionFlat, flatServerPriceFields
	}
	fail := func(err error) (ManifestQuery, error) {
		return ManifestQuery{}, fmt.Errorf("%s: %w", p.name, err)
	}

	br := bufio.NewReaderSize(r, 1<<20)
	first, err := peekNonSpace(br)
	if err != nil {
		return fail(err)
	}
	dec := json.NewDecoder(br)
	dec.UseNumber()

	type row struct{ key, compact []byte }
	var rows []row
	var single []byte
	isList := first == '['
	switch {
	case isList:
		if _, err := dec.Token(); err != nil {
			return fail(err)
		}
		rows = []row{}
		for dec.More() {
			var v any
			if flat {
				var raw map[string]json.RawMessage
				if err := dec.Decode(&raw); err != nil {
					return fail(err)
				}
				if v, err = projectRow(raw, flatServerPriceFields); err != nil {
					return fail(err)
				}
			} else if err := dec.Decode(&v); err != nil {
				return fail(err)
			}
			key, err := encode(v, sortKeyFormat)
			if err != nil {
				return fail(err)
			}
			compact, err := encode(v, compactFormat)
			if err != nil {
				return fail(err)
			}
			rows = append(rows, row{key, compact})
		}
		if _, err := dec.Token(); err != nil {
			return fail(err)
		}
	case flat:
		return fail(errors.New("response is not a JSON array"))
	default:
		var v any
		if err := dec.Decode(&v); err != nil {
			return fail(err)
		}
		if single, err = encode(v, compactFormat); err != nil {
			return fail(err)
		}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fail(errors.New("trailing data after the JSON value"))
	}

	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // the output directory chosen by the user
	if err != nil {
		return fail(err)
	}
	defer func() { _ = out.Close() }()
	zw, err := gzip.NewWriterLevel(out, gzip.BestCompression)
	if err != nil {
		return fail(err)
	}
	h := sha256.New()
	w := io.MultiWriter(zw, h)
	if isList {
		slices.SortStableFunc(rows, func(a, b row) int { return bytes.Compare(a.key, b.key) })
		n := len(rows)
		q.Rows = &n
		_, err = io.WriteString(w, "[")
		for i := 0; err == nil && i < len(rows); i++ {
			if i > 0 {
				_, err = io.WriteString(w, ",")
			}
			if err == nil {
				_, err = w.Write(rows[i].compact)
			}
		}
		if err == nil {
			_, err = io.WriteString(w, "]")
		}
	} else {
		_, err = w.Write(single)
	}
	if err == nil {
		err = zw.Close()
	}
	if err == nil {
		err = out.Close()
	}
	if err != nil {
		return fail(err)
	}
	q.SHA256 = hex.EncodeToString(h.Sum(nil))
	return q, nil
}

// peekNonSpace returns the first byte of the JSON text without consuming it.
func peekNonSpace(br *bufio.Reader) (byte, error) {
	for {
		b, err := br.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		if b == ' ' || b == '\t' || b == '\n' || b == '\r' {
			continue
		}
		return b, br.UnreadByte()
	}
}

// projectRow keeps the given fields of one object.
func projectRow(row map[string]json.RawMessage, fields []string) (map[string]any, error) {
	flat := make(map[string]any, len(fields))
	for _, f := range fields {
		v, ok := row[f]
		if !ok {
			continue
		}
		tree, err := decode(v)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", f, err)
		}
		flat[f] = tree
	}
	return flat, nil
}

// fetchQuery streams one query to dest (through a .part file renamed when
// complete, so a file that exists is a whole one).
func (f *fetcher) fetchQuery(ctx context.Context, p planned, dest string) (ManifestQuery, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return ManifestQuery{}, fmt.Errorf("writing snapshot: %w", err)
	}
	part := dest + ".part"
	var q ManifestQuery
	sink := func(r io.Reader) (err error) {
		q, err = store(r, p, part)
		return err
	}
	var err error
	if raw, ok := f.cache[p.path+"?"+encodeParams(p.params)]; ok {
		err = sink(bytes.NewReader(raw))
	} else {
		err = f.fetch(ctx, p.path, p.params, sink)
	}
	if err != nil {
		_ = os.Remove(part)
		return ManifestQuery{}, err
	}
	if err := os.Rename(part, dest); err != nil {
		return ManifestQuery{}, fmt.Errorf("writing snapshot: %w", err)
	}
	return q, nil
}

type health struct {
	DatabaseHash        string            `json:"database_hash"`
	DatabaseLastUpdated float64           `json:"database_last_updated"`
	Packages            map[string]string `json:"packages"`
}

func readAll(dst *[]byte) func(io.Reader) error {
	return func(r io.Reader) (err error) {
		*dst, err = io.ReadAll(r)
		return err
	}
}

func (f *fetcher) health(ctx context.Context) (health, error) {
	// Never cached: it is read before and after to detect a changing database.
	var raw []byte
	if err := f.fetch(ctx, "/healthcheck", nil, readAll(&raw)); err != nil {
		return health{}, err
	}
	var h health
	if err := json.Unmarshal(raw, &h); err != nil {
		return health{}, fmt.Errorf("/healthcheck: %w", err)
	}
	return h, nil
}

// get fetches a small response into memory, once.
func (f *fetcher) get(ctx context.Context, path string, params map[string]any) ([]byte, error) {
	key := path + "?" + encodeParams(params)
	if raw, ok := f.cache[key]; ok {
		return raw, nil
	}
	var raw []byte
	if err := f.fetch(ctx, path, params, readAll(&raw)); err != nil {
		return nil, err
	}
	f.cache[key] = raw
	return raw, nil
}

// canonicalConfig finds the config string Keeper matches exactly: the entry
// of /benchmark_configs whose parsed config equals the one of the list.
func (f *fetcher) canonicalConfig(ctx context.Context, b Benchmark) (string, error) {
	raw, err := f.get(ctx, "/benchmark_configs", nil)
	if err != nil {
		return "", err
	}
	var configs []struct {
		BenchmarkID string `json:"benchmark_id"`
		Config      string `json:"config"`
	}
	if err := json.Unmarshal(raw, &configs); err != nil {
		return "", fmt.Errorf("/benchmark_configs: %w", err)
	}
	wantJSON, err := encode(orEmpty(b.Config), compactFormat)
	if err != nil {
		return "", err
	}
	for _, c := range configs {
		if c.BenchmarkID != b.ID {
			continue
		}
		have, err := decode([]byte(c.Config))
		if err != nil {
			continue
		}
		haveJSON, err := encode(have, compactFormat)
		if err == nil && bytes.Equal(haveJSON, wantJSON) {
			return c.Config, nil
		}
	}
	return "", fmt.Errorf("benchmark %s %s not in /benchmark_configs", b.ID, wantJSON)
}

func expandString(s string, vars map[string]string) string {
	pairs := make([]string, 0, 2*len(vars))
	for k, v := range vars {
		pairs = append(pairs, "{"+k+"}", v)
	}
	return strings.NewReplacer(pairs...).Replace(s)
}

func expandParams(params map[string]any, vars map[string]string) map[string]any {
	out := make(map[string]any, len(params))
	for k, v := range params {
		switch v := v.(type) {
		case string:
			out[k] = expandString(v, vars)
		case []any:
			list := make([]any, len(v))
			for i, item := range v {
				if s, ok := item.(string); ok {
					item = expandString(s, vars)
				}
				list[i] = item
			}
			out[k] = list
		default:
			out[k] = v
		}
	}
	return out
}

func encodeParams(params map[string]any) string {
	values := url.Values{}
	for k, v := range params {
		if list, ok := v.([]any); ok {
			for _, item := range list {
				values.Add(k, paramString(item))
			}
			continue
		}
		values.Set(k, paramString(v))
	}
	return values.Encode()
}

func paramString(v any) string {
	switch v := v.(type) {
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

// errReader remembers a failure of the underlying body, to tell a broken
// transfer (retry) from a response that is bad as delivered (fail).
type errReader struct {
	r   io.Reader
	err error
}

func (e *errReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		e.err = err
	}
	return n, err
}

// fetch performs one GET with retries and hands the body of a successful
// response to sink as a stream. A sink failing on a broken transfer is retried
// like a failed request (the sink is called again from the start); a sink
// failing on a complete body is not. A rate-limit window nearly used up is
// waited out (the anonymous limit is 300 credits per minute).
func (f *fetcher) fetch(ctx context.Context, path string, params map[string]any, sink func(io.Reader) error) error {
	u := f.opts.KeeperURL + path
	if len(params) > 0 {
		u += "?" + encodeParams(params)
	}
	var lastErr error
	for attempt := 0; attempt <= f.opts.Retries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "sc-keeper-go-snapshot")
		req.Header.Set("Accept", "application/json")
		if f.opts.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+f.opts.APIKey)
		}
		resp, err := f.client.Do(req)
		var delay time.Duration
		if err == nil {
			switch {
			case resp.StatusCode >= 200 && resp.StatusCode <= 299:
				body := &errReader{r: resp.Body}
				sinkErr := sink(body)
				if sinkErr == nil {
					// Drain, so the connection is reused and a transfer error shows.
					_, _ = io.Copy(io.Discard, body)
					sinkErr = body.err
				}
				_ = resp.Body.Close()
				if sinkErr == nil {
					if rem, convErr := strconv.Atoi(resp.Header.Get("X-Ratelimit-Remaining")); convErr == nil && rem < 30 {
						if err := f.opts.Sleep(ctx, 30*time.Second); err != nil {
							return err
						}
					}
					return nil
				}
				if body.err == nil {
					return sinkErr
				}
				err = fmt.Errorf("GET %s: %w", u, sinkErr)
			default:
				snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				_ = resp.Body.Close()
				err = fmt.Errorf("GET %s: HTTP %d: %.200s", u, resp.StatusCode, bytes.TrimSpace(snippet))
				if !slices.Contains([]int{429, 500, 502, 503, 504}, resp.StatusCode) {
					return err
				}
				if s, convErr := strconv.Atoi(resp.Header.Get("Retry-After")); convErr == nil && s >= 0 {
					delay = time.Duration(s) * time.Second
				}
			}
		}
		lastErr = err
		if delay == 0 {
			delay = time.Duration(1<<attempt) * 5 * time.Second
		}
		if attempt < f.opts.Retries {
			if err := f.opts.Sleep(ctx, delay); err != nil {
				return err
			}
		}
	}
	return lastErr
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

const (
	serverPricesPath = "/server_prices"
	projectionFlat   = "flat"
)

// flatServerPriceFields are the fields of a /server_prices row that survive
// the flat projection; keeper.ZonePrice decodes exactly these.
var flatServerPriceFields = []string{
	"vendor_id", "region_id", "zone_id", "server_id", "operating_system", "allocation", "unit", "price", "currency", "status", "observed_at",
}
