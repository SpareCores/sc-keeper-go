/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package snapshot fetches a fixed set of Keeper API responses, described by a
// query list, into a directory of gzipped files plus a manifest.json that the
// replay server of package keepertest serves. It backs cmd/sc-snapshot.
package snapshot

import (
	"errors"
	"fmt"
	"math"
	"os"
	"slices"

	"sigs.k8s.io/yaml"
)

// QueryList says what to fetch. In YAML:
//
//	vendors:                      # vendor expansions, in this order
//	  - {name: aws, region: us-east-1, spot: true}
//	benchmarks:                   # benchmark expansions
//	  - {id: "geekbench:score", config: {cores: single}}
//	queries:                      # fetched once
//	  - {name: tables/region, path: /table/region}
//	vendorQueries:                # fetched once per vendor
//	  - name: "{vendor}/servers-ondemand"
//	    path: /servers
//	    params:
//	      vendor: "{vendor}"
//	      vendor_regions: "{vendor}~{region_id}"
//	      limit: -1
//	      best_price_allocation: ONDEMAND_ONLY
//
// Names and string parameter values may use {vendor}, {region} (the region
// of the vendor entry), {region_id} (its Keeper region id, found in
// /table/region by region_id, api_reference or alias; active regions are
// preferred and retired ones are an error), and, with
// benchmarks, {benchmark_id}, {benchmark_slug} (the id with ":" as "_", plus "-" and
// a short hash of the config when the id is listed more than once) and
// {benchmark_config} (the canonical config string of /benchmark_configs).
type QueryList struct {
	Vendors       []Vendor      `json:"vendors,omitempty"`
	Benchmarks    []Benchmark   `json:"benchmarks,omitempty"`
	Queries       []Entry       `json:"queries,omitempty"`
	VendorQueries []Entry       `json:"vendorQueries,omitempty"`
	Snapshot      *SnapshotSpec `json:"snapshot,omitempty"`
}

// SnapshotSpec says which of Keeper's snapshot files (GET /snapshot/{hash}/...)
// to record, in the query list as
//
//	vendors:                     # the vendors of the list; without vendorQueries
//	  - {name: hcloud}           # a list with a snapshot block needs no region
//	snapshot:
//	  vendors: [region, zone, server, server_price, storage_price, ipv4_price]
//	  global: [vendor]
//	  benchmarks: ["stress_ng:bestn"]
//
// vendors names the per-vendor tables, recorded for each vendor of the list;
// global the global tables; benchmarks the benchmark ids, one score file per
// vendor of the list and id. The files are those of the database hash
// /healthcheck reports when the run starts, and the snapshot routes need an
// API key (Options.APIKey).
type SnapshotSpec struct {
	Vendors    []string `json:"vendors,omitempty"`
	Global     []string `json:"global,omitempty"`
	Benchmarks []string `json:"benchmarks,omitempty"`
}

// vendorTables and globalTables are the tables Keeper's snapshot routes know
// (src/sc_keeper/snapshots.py, VENDOR_TABLES and GLOBAL_TABLES).
var (
	vendorTables = []string{
		"region", "zone", "server", "server_description", "server_price", "storage", "storage_price",
		"traffic_price", "ipv4_price", "database", "database_price", "database_storage",
		"database_storage_price", "vendor_compliance_link",
	}
	globalTables = []string{"vendor", "country", "compliance_framework", "benchmark", "currency"}
)

func (s *SnapshotSpec) empty() bool {
	return s == nil || len(s.Vendors)+len(s.Global)+len(s.Benchmarks) == 0
}

// Vendor is one expansion of vendorQueries.
type Vendor struct {
	Name string `json:"name"`
	// Region is the region of the vendor to snapshot (a region_id,
	// api_reference or alias of /table/region).
	Region string `json:"region"`
	// Spot marks vendors with spot prices; entries that require spot skip
	// the others.
	Spot bool `json:"spot,omitempty"`
}

// Benchmark is one benchmark config of the benchmark expansion.
type Benchmark struct {
	ID     string         `json:"id"`
	Config map[string]any `json:"config,omitempty"`
}

// Entry is one request (or a family of requests, with an expansion).
type Entry struct {
	// Name is the file (without .json.gz) and the manifest name.
	Name string `json:"name"`
	// Path is the Keeper path, e.g. /servers.
	Path string `json:"path"`
	// Params are the query parameters: scalars, or lists of scalars for a
	// repeated parameter.
	Params map[string]any `json:"params,omitempty"`
	// Benchmarks expands the entry over the benchmarks of the list.
	Benchmarks bool `json:"benchmarks,omitempty"`
	// RequiresSpot skips the vendors without spot prices.
	RequiresSpot bool `json:"requiresSpot,omitempty"`
}

// LoadQueryList reads a query list file.
func LoadQueryList(path string) (*QueryList, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading query list: %w", err)
	}
	var l QueryList
	if err := yaml.UnmarshalStrict(raw, &l); err != nil {
		return nil, fmt.Errorf("parsing query list %s: %w", path, err)
	}
	l.normalise()
	return &l, l.validate()
}

// normalise turns the whole numbers the YAML-to-JSON conversion made floats
// back into integers, so that limit: -1 stays -1 in the manifest.
func (l *QueryList) normalise() {
	for _, entries := range [][]Entry{l.Queries, l.VendorQueries} {
		for _, e := range entries {
			for k, v := range e.Params {
				e.Params[k] = normaliseValue(v)
			}
		}
	}
}

func normaliseValue(v any) any {
	switch v := v.(type) {
	case float64:
		if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
			return int64(v)
		}
	case []any:
		for i := range v {
			v[i] = normaliseValue(v[i])
		}
	}
	return v
}

func (l *QueryList) validate() error {
	var errs []error
	names := map[string]bool{}
	check := func(e Entry, scope string) {
		if e.Name == "" || e.Path == "" {
			errs = append(errs, fmt.Errorf("%s entry %q needs a name and a path", scope, e.Name))
		}
	}
	for _, e := range l.Queries {
		check(e, "queries")
		if e.Benchmarks || e.RequiresSpot {
			errs = append(errs, fmt.Errorf("queries entry %q: benchmarks and requiresSpot need vendorQueries", e.Name))
		}
	}
	for _, e := range l.VendorQueries {
		check(e, "vendorQueries")
	}
	if len(l.VendorQueries) > 0 && len(l.Vendors) == 0 {
		errs = append(errs, errors.New("vendorQueries without vendors"))
	}
	for _, v := range l.Vendors {
		if v.Name == "" || (v.Region == "" && (len(l.VendorQueries) > 0 || l.Snapshot == nil)) {
			errs = append(errs, fmt.Errorf("vendor %q needs a name and a region", v.Name))
		}
		if names[v.Name] {
			errs = append(errs, fmt.Errorf("vendor %q listed twice", v.Name))
		}
		names[v.Name] = true
	}
	errs = append(errs, l.Snapshot.validate(len(l.Vendors) > 0)...)
	if !slices.ContainsFunc(l.VendorQueries, func(e Entry) bool { return e.Benchmarks }) && len(l.Benchmarks) > 0 {
		errs = append(errs, errors.New("benchmarks listed but no vendorQueries entry expands them"))
	}
	return errors.Join(errs...)
}

func (s *SnapshotSpec) validate(haveVendors bool) []error {
	if s == nil {
		return nil
	}
	var errs []error
	for _, t := range s.Vendors {
		if !slices.Contains(vendorTables, t) {
			errs = append(errs, fmt.Errorf("snapshot.vendors: %q is not a per-vendor table (%v)", t, vendorTables))
		}
	}
	for _, t := range s.Global {
		if !slices.Contains(globalTables, t) {
			errs = append(errs, fmt.Errorf("snapshot.global: %q is not a global table (%v)", t, globalTables))
		}
	}
	if len(s.Vendors)+len(s.Benchmarks) > 0 && !haveVendors {
		errs = append(errs, errors.New("snapshot.vendors and snapshot.benchmarks need vendors"))
	}
	return errs
}
