/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package snapshot

import (
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

// Snapshot is a fetched snapshot, not yet written.
type Snapshot struct {
	Manifest Manifest
	// Files maps manifest file names to their (gzipped) content.
	Files map[string][]byte
}

type fetcher struct {
	opts   Options
	client *http.Client
	cache  map[string][]byte
}

// Fetch runs every query of the list against Keeper. It fails if the Keeper
// database changes while it runs. The result depends only on the data, except
// for fetched_at.
func Fetch(ctx context.Context, list *QueryList, opts Options) (*Snapshot, error) {
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
	f := &fetcher{opts: opts, client: opts.HTTPClient, cache: map[string][]byte{}}
	if f.client == nil {
		f.client = &http.Client{Timeout: 5 * time.Minute}
	}

	health, err := f.health(ctx)
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{Files: map[string][]byte{}}
	m := &snap.Manifest
	m.KeeperURL = opts.KeeperURL
	m.Vendors = map[string]ManifestVendor{}
	for _, v := range list.Vendors {
		m.Vendors[v.Name] = ManifestVendor{Region: v.Region, Spot: v.Spot}
	}

	record := func(e Entry, name string, params map[string]any) error {
		raw, err := f.get(ctx, e.Path, params)
		if err != nil {
			return err
		}
		q, file, err := store(name, e.Path, params, raw)
		if err != nil {
			return err
		}
		if _, dup := snap.Files[q.File]; dup {
			return fmt.Errorf("two queries write %s", q.File)
		}
		snap.Files[q.File] = file
		m.Queries = append(m.Queries, q)
		return nil
	}

	for _, e := range list.Queries {
		if err := record(e, e.Name, e.Params); err != nil {
			return nil, err
		}
	}
	for _, v := range list.Vendors {
		for _, e := range list.VendorQueries {
			if e.RequiresSpot && !v.Spot {
				continue
			}
			vars, err := f.vendorVars(ctx, v)
			if err != nil {
				return nil, err
			}
			if !e.Benchmarks {
				if err := record(e, expandString(e.Name, vars), expandParams(e.Params, vars)); err != nil {
					return nil, err
				}
				continue
			}
			for _, b := range list.Benchmarks {
				config, err := f.canonicalConfig(ctx, b)
				if err != nil {
					return nil, err
				}
				bv := maps.Clone(vars)
				bv["benchmark_id"] = b.ID
				bv["benchmark_slug"] = strings.ReplaceAll(b.ID, ":", "_")
				bv["benchmark_config"] = config
				if err := record(e, expandString(e.Name, bv), expandParams(e.Params, bv)); err != nil {
					return nil, err
				}
			}
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
	m.Keeper = ManifestKeeper{
		Packages:            health.Packages,
		DatabaseHash:        health.DatabaseHash,
		DatabaseLastUpdated: time.Unix(int64(health.DatabaseLastUpdated), 0).UTC().Format("2006-01-02T15:04:05-07:00"),
	}
	return snap, nil
}

// Write replaces dir with the snapshot: it is written next to dir and moved
// into place, so a failed fetch or write leaves the old snapshot (only the
// last two steps, removing dir and renaming, are not atomic).
func (s *Snapshot) Write(dir string) error {
	dir = filepath.Clean(dir)
	tmp := dir + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("clearing %s: %w", tmp, err)
	}
	for name, content := range s.Files {
		path := filepath.Join(tmp, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return fmt.Errorf("writing snapshot: %w", err)
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			return fmt.Errorf("writing snapshot: %w", err)
		}
	}
	manifest, err := encode(s.Manifest, manifestFormat)
	if err != nil {
		return fmt.Errorf("encoding manifest: %w", err)
	}
	if err := os.MkdirAll(tmp, 0o750); err != nil {
		return fmt.Errorf("writing snapshot: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "manifest.json"), append(manifest, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing manifest: %w", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("replacing %s: %w", dir, err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		return fmt.Errorf("replacing %s: %w", dir, err)
	}
	return nil
}

// store canonicalises a response and gzips it. Lists are sorted (Keeper's
// order among equally priced rows is not stable), keys are sorted, and the
// gzip header carries no time, so identical data gives identical files.
func store(name, path string, params map[string]any, raw []byte) (ManifestQuery, []byte, error) {
	q := ManifestQuery{Name: name, Path: path, Params: params, File: name + ".json.gz"}
	var tree any
	var err error
	if path == serverPricesPath {
		q.Projection, q.Fields = projectionFlat, flatServerPriceFields
		tree, err = projectFlat(raw, flatServerPriceFields)
	} else {
		tree, err = decode(raw)
	}
	if err != nil {
		return ManifestQuery{}, nil, fmt.Errorf("%s: %w", name, err)
	}
	if q.Params == nil {
		q.Params = map[string]any{}
	}
	if list, ok := tree.([]any); ok {
		keys := make(map[int]string, len(list))
		idx := make([]int, len(list))
		for i, row := range list {
			b, err := encode(row, sortKeyFormat)
			if err != nil {
				return ManifestQuery{}, nil, fmt.Errorf("%s: %w", name, err)
			}
			keys[i], idx[i] = string(b), i
		}
		slices.SortStableFunc(idx, func(a, b int) int { return strings.Compare(keys[a], keys[b]) })
		sorted := make([]any, len(list))
		for i, j := range idx {
			sorted[i] = list[j]
		}
		tree = sorted
		n := len(sorted)
		q.Rows = &n
	}
	canonical, err := encode(tree, compactFormat)
	if err != nil {
		return ManifestQuery{}, nil, fmt.Errorf("%s: %w", name, err)
	}
	sum := sha256.Sum256(canonical)
	q.SHA256 = hex.EncodeToString(sum[:])
	var gz bytes.Buffer
	zw, err := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	if err != nil {
		return ManifestQuery{}, nil, err
	}
	if _, err := zw.Write(canonical); err != nil {
		return ManifestQuery{}, nil, err
	}
	if err := zw.Close(); err != nil {
		return ManifestQuery{}, nil, err
	}
	return q, gz.Bytes(), nil
}

type health struct {
	DatabaseHash        string            `json:"database_hash"`
	DatabaseLastUpdated float64           `json:"database_last_updated"`
	Packages            map[string]string `json:"packages"`
}

func (f *fetcher) health(ctx context.Context) (health, error) {
	// Never cached: it is read before and after to detect a changing database.
	raw, err := f.fetch(ctx, "/healthcheck", nil)
	if err != nil {
		return health{}, err
	}
	var h health
	if err := json.Unmarshal(raw, &h); err != nil {
		return health{}, fmt.Errorf("/healthcheck: %w", err)
	}
	return h, nil
}

func (f *fetcher) get(ctx context.Context, path string, params map[string]any) ([]byte, error) {
	key := path + "?" + encodeParams(params)
	if raw, ok := f.cache[key]; ok {
		return raw, nil
	}
	raw, err := f.fetch(ctx, path, params)
	if err != nil {
		return nil, err
	}
	f.cache[key] = raw
	return raw, nil
}

func (f *fetcher) vendorVars(ctx context.Context, v Vendor) (map[string]string, error) {
	raw, err := f.get(ctx, "/table/region", nil)
	if err != nil {
		return nil, err
	}
	var regions []struct {
		VendorID     string   `json:"vendor_id"`
		RegionID     string   `json:"region_id"`
		APIReference string   `json:"api_reference"`
		Aliases      []string `json:"aliases"`
	}
	if err := json.Unmarshal(raw, &regions); err != nil {
		return nil, fmt.Errorf("/table/region: %w", err)
	}
	for _, r := range regions {
		if r.VendorID == v.Name && (v.Region == r.RegionID || v.Region == r.APIReference || slices.Contains(r.Aliases, v.Region)) {
			return map[string]string{"vendor": v.Name, "region": v.Region, "region_id": r.RegionID}, nil
		}
	}
	return nil, fmt.Errorf("region %s of %s not found in /table/region", v.Region, v.Name)
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
	want := b.Config
	if want == nil {
		want = map[string]any{}
	}
	wantJSON, err := encode(want, compactFormat)
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

// fetch performs one GET with retries; a rate-limit window nearly used up is
// waited out (the anonymous limit is 300 credits per minute).
func (f *fetcher) fetch(ctx context.Context, path string, params map[string]any) ([]byte, error) {
	u := f.opts.KeeperURL + path
	if len(params) > 0 {
		u += "?" + encodeParams(params)
	}
	var lastErr error
	for attempt := 0; attempt <= f.opts.Retries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "sc-keeper-go-snapshot")
		req.Header.Set("Accept", "application/json")
		if f.opts.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+f.opts.APIKey)
		}
		resp, err := f.client.Do(req)
		var delay time.Duration
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			switch {
			case readErr != nil:
				err = readErr
			case resp.StatusCode >= 200 && resp.StatusCode <= 299:
				if rem, convErr := strconv.Atoi(resp.Header.Get("X-Ratelimit-Remaining")); convErr == nil && rem < 30 {
					if err := f.opts.Sleep(ctx, 30*time.Second); err != nil {
						return nil, err
					}
				}
				return body, nil
			default:
				err = fmt.Errorf("GET %s: HTTP %d: %.200s", u, resp.StatusCode, bytes.TrimSpace(body))
				if !slices.Contains([]int{429, 500, 502, 503, 504}, resp.StatusCode) {
					return nil, err
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
				return nil, err
			}
		}
	}
	return nil, lastErr
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

// projectFlat keeps the given fields of every element of a JSON array of
// objects. The array is walked element by element, so the nested objects of a
// large response are never held as a tree.
func projectFlat(raw []byte, fields []string) ([]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		if err == nil {
			err = errors.New("response is not a JSON array")
		}
		return nil, err
	}
	rows := []any{}
	for dec.More() {
		var row map[string]json.RawMessage
		if err := dec.Decode(&row); err != nil {
			return nil, err
		}
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
		rows = append(rows, flat)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return rows, nil
}
