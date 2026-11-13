/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package selection

import (
	"context"
	"maps"
	"slices"
	"testing"

	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/SpareCores/sc-keeper-go/selection/api"
)

func TestAcceptPrice(t *testing.T) {
	const od, sp = keeper.PriceAllocationOnDemand, keeper.PriceAllocationSpot
	tests := []struct {
		name                  string
		status, unit, cur, os string
		alloc                 keeper.PriceAllocation
		price                 float64
		want                  keeper.Allocation
		wantOK                bool
	}{
		{name: "on-demand", status: "active", unit: "hour", cur: "USD", os: "Linux", alloc: od, price: 1, want: keeper.AllocationOnDemand, wantOK: true},
		{name: "spot, blank OS and currency", status: "active", unit: "hour", alloc: sp, price: 0.2, want: keeper.AllocationSpot, wantOK: true},
		{name: "OS case", status: "active", unit: "hour", os: "linux", alloc: od, price: 1, want: keeper.AllocationOnDemand, wantOK: true},
		{name: "inactive", status: "inactive", unit: "hour", alloc: od, price: 1},
		{name: "monthly", status: "active", unit: "month", alloc: od, price: 70},
		{name: "other currency", status: "active", unit: "hour", cur: "EUR", alloc: od, price: 1},
		{name: "Windows", status: "active", unit: "hour", os: "Windows", alloc: od, price: 1},
		{name: "zero", status: "active", unit: "hour", alloc: od, price: 0},
		{name: "reserved", status: "active", unit: "hour", alloc: keeper.PriceAllocationReserved, price: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := AcceptPrice(tc.status, tc.unit, tc.cur, tc.os, tc.alloc, tc.price)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("AcceptPrice = %q, %v; want %q, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func priceRow(server, region, zone string, alloc keeper.PriceAllocation, price float64) CatalogPrice {
	return CatalogPrice{ServerID: server, RegionID: region, ZoneID: zone, Allocation: alloc,
		Unit: "hour", Currency: "USD", Status: "active", OperatingSystem: "Linux", Price: price}
}

func prices(rows ...CatalogPrice) func(func(CatalogPrice) bool) {
	return func(yield func(CatalogPrice) bool) {
		for _, r := range rows {
			if !yield(r) {
				return
			}
		}
	}
}

// The rules of /servers, one by one.
func TestBuildCatalogCandidatesRules(t *testing.T) {
	const od, sp = keeper.PriceAllocationOnDemand, keeper.PriceAllocationSpot
	srv := func(id, name string, vcpus int, status string) keeper.Server {
		// The id differs from the name, as on GCP and Hetzner.
		return keeper.Server{ServerID: id, APIReference: name, VCPUs: vcpus, Status: status, CPUArchitecture: "x86_64", Family: "f"}
	}
	servers := []keeper.Server{
		srv("id-a", "a", 2, "active"),
		srv("id-retiring", "retiring", 2, "planned-for-retirement"),
		srv("id-retired", "retired", 2, "inactive"),
		srv("id-nostatus", "nostatus", 2, ""),
		srv("id-novcpu", "novcpu", 0, "active"),
		srv("id-spotonly", "spotonly", 2, "active"),
		srv("id-unpriced", "unpriced", 2, "active"),
		srv("id-elsewhere", "elsewhere", 2, "active"),
		srv("id-win", "win", 2, "active"),
		srv("id-dup", "a", 2, "active"),
	}
	win := priceRow("id-win", "r1", "z1", od, 0.2)
	win.OperatingSystem = "Windows"
	zero := priceRow("id-win", "r1", "z2", od, 0)
	monthly := priceRow("id-win", "r1", "z3", od, 0.1)
	monthly.Unit = "month"
	inactive := priceRow("id-a", "r1", "z9", od, 0.01)
	inactive.Status = "inactive"
	rows := []CatalogPrice{
		win, zero, monthly, inactive,
		priceRow("id-a", "r1", "z1", od, 0.30000049),
		priceRow("id-a", "r1", "z2", od, 0.25),
		priceRow("id-a", "r2", "z1", od, 0.10),
		priceRow("id-a", "r1", "z1", sp, 0.05),
		priceRow("id-retiring", "r1", "", od, 0.4),
		priceRow("id-retired", "r1", "", od, 0.4),
		priceRow("id-nostatus", "r1", "", od, 0.4),
		priceRow("id-novcpu", "r1", "", od, 0.4),
		priceRow("id-spotonly", "r1", "", sp, 0.4),
		priceRow("id-elsewhere", "r3", "", od, 0.4),
		priceRow("id-dup", "r1", "", od, 0.9),
	}
	scores := map[keeper.BenchmarkKey]map[string]float64{testKey: {"id-a": 1000, "a": 1, "id-retiring": 500}}

	build := func(basis keeper.Allocation, regions ...string) map[string]*Candidate {
		cs := BuildCatalogCandidates(CatalogInput{Servers: servers, Prices: prices(rows...), Regions: regions, Scores: scores}, basis)
		if !slices.IsSortedFunc(cs, func(a, b *Candidate) int {
			switch {
			case a.Name() < b.Name():
				return -1
			case a.Name() > b.Name():
				return 1
			}
			return 0
		}) {
			t.Error("candidates are not sorted by name")
		}
		return byName(cs)
	}
	names := func(m map[string]*Candidate) []string { return slices.Sorted(maps.Keys(m)) }

	got := build(keeper.AllocationOnDemand, "r1", "r2")
	// orderable and with a vCPU and an on-demand price (Windows, zero, monthly
	// and inactive rows are no price; the duplicate name keeps the first).
	if want := []string{"a", "nostatus", "retiring"}; !slices.Equal(names(got), want) {
		t.Fatalf("on-demand candidates in r1, r2 = %v, want %v", names(got), want)
	}
	a := got["a"]
	// The lowest over the zones and the regions, rounded to 4 digits.
	if p := a.Prices[keeper.AllocationOnDemand]; p != 0.1 {
		t.Errorf("price of a = %v, want 0.1", p)
	}
	if _, ok := a.Prices[keeper.AllocationSpot]; ok {
		t.Error("Prices holds the basis only")
	}
	if a.Scores[testKey] != 1000 || got["retiring"].Scores[testKey] != 500 || len(got["nostatus"].Scores) != 0 {
		t.Errorf("scores are those of the server id: a %v, retiring %v, nostatus %v", a.Scores, got["retiring"].Scores, got["nostatus"].Scores)
	}
	if a.Server.ServerID != "id-a" || a.Architecture != api.ArchitectureAMD64 {
		t.Errorf("a = %+v", a)
	}

	if p := build(keeper.AllocationOnDemand, "r1")["a"].Prices[keeper.AllocationOnDemand]; p != 0.25 {
		t.Errorf("price of a in r1 = %v, want 0.25", p)
	}
	if got := names(build(keeper.AllocationOnDemand)); !slices.Contains(got, "elsewhere") {
		t.Errorf("no regions means all regions: %v", got)
	}
	if got := names(build(keeper.AllocationOnDemand, "r3")); !slices.Equal(got, []string{"elsewhere"}) {
		t.Errorf("r3 = %v", got)
	}
	spot := build(keeper.AllocationSpot, "r1")
	if got := names(spot); !slices.Equal(got, []string{"a", "spotonly"}) {
		t.Errorf("spot candidates = %v", got)
	}
	if p := spot["a"].Prices[keeper.AllocationSpot]; p != 0.05 {
		t.Errorf("spot price of a = %v", p)
	}
	// Rounding to 4 digits, as Keeper does.
	rounded := BuildCatalogCandidates(CatalogInput{Servers: servers[:1], Prices: prices(priceRow("id-a", "r1", "", od, 0.30000049))},
		keeper.AllocationOnDemand)
	if rounded[0].Prices[keeper.AllocationOnDemand] != 0.3 {
		t.Errorf("price = %v, want 0.3", rounded[0].Prices[keeper.AllocationOnDemand])
	}
	if cs := BuildCatalogCandidates(CatalogInput{Servers: servers}, keeper.AllocationOnDemand); len(cs) != 0 {
		t.Errorf("no prices: %d candidates", len(cs))
	}
}

// The candidates and the selection of the local builder equal those of the
// /servers path (PlanQueries, BuildCandidates) when both see the same data:
// here the rows of the snapshot's /servers responses, taken apart into specs,
// prices and scores as a catalog holds them.
func TestCatalogCandidatesMatchTheServersPath(t *testing.T) {
	type tc struct {
		name   string
		vendor string
		spec   *api.Spec
	}
	var cases []tc
	for _, c := range selectorCases() {
		sel := c.sel
		cases = append(cases, tc{c.name, c.vendor, specOf(&sel)})
	}
	ranked := func(sel *api.Selector, r api.Ranking) *api.Spec { return rankingSpec(sel, r) }
	bench := &api.BenchmarkRef{ID: "geekbench:score", Config: cfg(`{"cores": "single"}`)}
	two := int32(2)
	three, six := int32(3), int32(6)
	for _, vendor := range []string{"aws", "gcp", "azure", "hcloud"} {
		cases = append(cases,
			tc{"ranked/" + vendor, vendor, ranked(&api.Selector{VCPUs: &api.IntRange{Min: &two}}, api.Ranking{
				Score: api.RankingScore{Benchmark: bench}, Objective: api.ObjectiveScore,
				Band: &api.RankingBand{WithinPercent: 12}, Families: &api.RankingFamilies{Min: &three, Max: &six}})},
			tc{"cheapest/" + vendor, vendor, ranked(nil, api.Ranking{
				Score: api.RankingScore{Benchmark: bench}, Objective: api.ObjectivePrice,
				Band: &api.RankingBand{WithinPercent: 50}})},
			tc{"per-price/" + vendor, vendor, ranked(&api.Selector{Unbenchmarked: api.UnbenchmarkedInclude}, api.Ranking{
				Score: api.RankingScore{Benchmark: bench}, Objective: api.ObjectiveScorePerPrice,
				Band: &api.RankingBand{WithinPercent: 20}})},
			tc{"empty/" + vendor, vendor, specOf(&api.Selector{})},
		)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := Options{Catalog: catalog}
			plan, err := PlanQueries(c.spec, c.vendor, []string{region(t, c.vendor)}, opts)
			if err != nil {
				t.Fatal(err)
			}
			results, err := fetcher.ServersAll(context.Background(), plan)
			if err != nil {
				t.Fatal(err)
			}
			basis := AllocationOf(ResolveBasis(c.spec, opts.DefaultBasis))
			in := catalogInputOf(region(t, c.vendor), results)
			old := BuildCandidates(results)
			local := BuildCatalogCandidates(in, basis)

			if !slices.EqualFunc(old, local, func(a, b *Candidate) bool {
				return a.Name() == b.Name() && a.Prices[basis] == b.Prices[basis] && a.Family() == b.Family() &&
					a.Architecture == b.Architecture && a.AcceleratorType == b.AcceleratorType
			}) {
				t.Fatalf("candidates differ: /servers %d, local %d", len(old), len(local))
			}
			wantRes, err := Evaluate(c.spec, old, opts)
			if err != nil {
				t.Fatal(err)
			}
			gotRes, err := Evaluate(c.spec, local, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(wantRes.Names(), gotRes.Names()) {
				t.Errorf("selected types differ:\n/servers %v\nlocal    %v", wantRes.Names(), gotRes.Names())
			}
			for i, w := range wantRes.InstanceTypes {
				g := gotRes.InstanceTypes[i]
				if w.Price != g.Price || (w.Score == nil) != (g.Score == nil) || (w.Score != nil && *w.Score != *g.Score) {
					t.Errorf("%s: /servers %+v, local %+v", w.Name, w, g)
				}
			}
		})
	}
}

// catalogInputOf takes the rows of the queries apart: the specs of the base
// rows, their prices as price rows of one region, the scores by server id.
func catalogInputOf(regionID string, results []keeper.QueryResult) CatalogInput {
	in := CatalogInput{Scores: map[keeper.BenchmarkKey]map[string]float64{}}
	seen := map[string]bool{}
	var all []CatalogPrice
	for _, r := range results {
		alloc := keeper.PriceAllocationOnDemand
		if r.Query.Allocation == keeper.AllocationSpot {
			alloc = keeper.PriceAllocationSpot
		}
		for _, s := range r.Servers {
			if !seen[s.ServerID] {
				seen[s.ServerID] = true
				spec := s
				spec.Price, spec.SelectedBenchmarkScore = nil, nil
				in.Servers = append(in.Servers, spec)
			}
			if s.Price != nil {
				all = append(all, CatalogPrice{ServerID: s.ServerID, RegionID: regionID, ZoneID: "z", Allocation: alloc,
					OperatingSystem: "Linux", Unit: "hour", Currency: "USD", Status: "active", Price: *s.Price})
			}
			if r.Query.Benchmark != nil && s.SelectedBenchmarkScore != nil {
				if in.Scores[*r.Query.Benchmark] == nil {
					in.Scores[*r.Query.Benchmark] = map[string]float64{}
				}
				in.Scores[*r.Query.Benchmark][s.ServerID] = *s.SelectedBenchmarkScore
			}
		}
	}
	in.Prices = prices(all...)
	in.Regions = []string{regionID}
	return in
}

func TestBenchmarkKeys(t *testing.T) {
	two := int32(2)
	bench := &api.BenchmarkRef{ID: "geekbench:score", Config: cfg(`{"cores": "single"}`)}
	spec := rankingSpec(&api.Selector{
		Benchmarks: []api.BenchmarkFilter{{BenchmarkRef: *bench, Min: new(100.0)}},
		VCPUs:      &api.IntRange{Min: &two},
	}, api.Ranking{Score: api.RankingScore{Benchmark: bench}, Objective: api.ObjectiveScore,
		Band: &api.RankingBand{WithinPercent: 10}})
	keys, err := BenchmarkKeys(spec, Options{Catalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	if want := []keeper.BenchmarkKey{testKey}; !slices.Equal(keys, want) {
		t.Errorf("keys = %v, want the one benchmark of filter and ranking: %v", keys, want)
	}
	if keys, err := BenchmarkKeys(specOf(&api.Selector{}), Options{}); err != nil || len(keys) != 0 {
		t.Errorf("no benchmarks: %v, %v", keys, err)
	}
	if _, err := BenchmarkKeys(rankingSpec(nil, api.Ranking{Score: api.RankingScore{Benchmark: &api.BenchmarkRef{ID: "no:such"}}}),
		Options{Catalog: catalog}); err == nil {
		t.Error("an unknown benchmark is invalid")
	}
}

func TestCatalogPriceOf(t *testing.T) {
	row := keeper.ServerPrice{
		VendorID: "aws", RegionID: "r1", ZoneID: "z1", ServerID: "s1", OperatingSystem: "Linux",
		Allocation: keeper.PriceAllocationSpot, Unit: "hour", Price: 0.5, Currency: "USD", Status: "active",
	}
	got := CatalogPriceOf(&row)
	if got != priceRow("s1", "r1", "z1", keeper.PriceAllocationSpot, 0.5) {
		t.Errorf("CatalogPriceOf = %+v", got)
	}
	if alloc, ok := got.Accept(); !ok || alloc != keeper.AllocationSpot {
		t.Errorf("Accept = %q, %v", alloc, ok)
	}
}
