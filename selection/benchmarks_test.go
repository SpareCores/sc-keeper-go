/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package selection

import (
	"errors"
	"math"
	"testing"

	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/SpareCores/sc-keeper-go/selection/api"
)

func key(t *testing.T, id, config string) keeper.BenchmarkKey {
	t.Helper()
	k, err := catalog.Resolve(id, []byte(config))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// checkBenchmark asserts, for every eligible candidate, the documented
// outcome of one benchmark filter given its (normalised) value.
func checkBenchmark(t *testing.T, res Result, cands []*Candidate, k keeper.BenchmarkKey, field string,
	value func(c *Candidate, score float64) float64, passes func(v float64) bool) {
	t.Helper()
	selected := selectedSet(res)
	var in, out, miss int
	for _, c := range cands {
		if c.Architecture == "" {
			continue
		}
		score, ok := c.Scores[k]
		_, isSelected := selected[c.Name()]
		switch {
		case !ok:
			miss++
			if got := res.Misses[c.Name()].Reason; got != ReasonMissingData {
				t.Errorf("%s has no score: want missingData, got %q", c.Name(), got)
			}
		case passes(value(c, score)):
			in++
			if !isSelected {
				t.Errorf("%s (%.4g) should pass, excluded as %+v", c.Name(), value(c, score), res.Misses[c.Name()])
			}
		default:
			out++
			if got := res.Misses[c.Name()].Reason; got != ReasonFilterPrefix+field {
				t.Errorf("%s (%.4g) should fail %s, got %q", c.Name(), value(c, score), field, got)
			}
		}
	}
	if in == 0 || out == 0 || miss == 0 {
		t.Errorf("want passing, failing and unscored rows; got %d / %d / %d", in, out, miss)
	}
}

func raw(_ *Candidate, s float64) float64 { return s }

func TestBenchmarkFilters(t *testing.T) {
	tests := []struct {
		name   string
		vendor string
		bench  api.BenchmarkFilter
		value  func(c *Candidate, s float64) float64
		passes func(v float64) bool
	}{
		{name: "geekbench single min", vendor: "aws",
			bench: api.BenchmarkFilter{BenchmarkRef: api.BenchmarkRef{ID: "geekbench:score", Config: cfg(`{"cores":"single"}`)}, Min: new(2000.0)},
			value: raw, passes: func(v float64) bool { return v >= 2000 }},
		{name: "membench latency max (lower is better)", vendor: "aws",
			bench: api.BenchmarkFilter{BenchmarkRef: api.BenchmarkRef{ID: "membench:latency", Config: cfg(`{"scope":"RAM"}`)}, Max: new(100.0)},
			value: raw, passes: func(v float64) bool { return v <= 100 }},
		{name: "stress-ng per vCPU", vendor: "azure",
			bench:  api.BenchmarkFilter{BenchmarkRef: api.BenchmarkRef{ID: "stress_ng:bestn"}, Min: new(1500.0), Normalize: api.NormalizePerVcpu},
			value:  func(c *Candidate, s float64) float64 { return s / float64(c.VCPUs()) },
			passes: func(v float64) bool { return v >= 1500 }},
		{name: "passmark single per price", vendor: "gcp",
			bench: api.BenchmarkFilter{BenchmarkRef: api.BenchmarkRef{ID: "passmark:cpu_single_threaded_test"}, Min: new(20000.0), Normalize: api.NormalizePerPrice},
			value: func(c *Candidate, s float64) float64 {
				p, _ := c.Price(keeper.AllocationOnDemand, 0)
				return s / p
			},
			passes: func(v float64) bool { return v >= 20000 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := tt.bench
			res, cands := run(t, tt.vendor, specOf(&api.Selector{Benchmarks: []api.BenchmarkFilter{b}}))
			var cfgRaw string
			if b.Config != nil {
				cfgRaw = string(b.Config.Raw)
			}
			checkBenchmark(t, res, cands, key(t, b.ID, cfgRaw), "benchmarks", tt.value, tt.passes)
		})
	}
}

func TestWorkloadProfiles(t *testing.T) {
	for _, vendor := range []string{"gcp", "hcloud"} {
		t.Run(vendor, func(t *testing.T) {
			sel := &api.Selector{WorkloadProfiles: []api.WorkloadProfileFilter{{Name: "web", Min: 0.3}}}
			res, cands := run(t, vendor, specOf(sel))
			if vendor == "hcloud" {
				// Fully benchmarked vendor: nothing lacks a score.
				if res.Excluded.MissingData != 0 || res.Excluded.Filters["workloadProfiles"] == 0 {
					t.Errorf("excluded = %+v", res.Excluded)
				}
				return
			}
			checkBenchmark(t, res, cands, key(t, "workload_profile:web", ""), "workloadProfiles", raw,
				func(v float64) bool { return v >= 0.3 })
			if _, ok := selectedSet(res)["c3-standard-4"]; !ok {
				t.Error("c3-standard-4 (web 0.34) not selected")
			}
		})
	}
}

func TestBenchmarkValidation(t *testing.T) {
	gb := func(c string) api.BenchmarkRef {
		return api.BenchmarkRef{ID: "geekbench:score", Config: cfg(c)}
	}
	tests := []struct {
		name    string
		bench   api.BenchmarkFilter
		unknown bool
	}{
		{name: "min on lower-is-better", bench: api.BenchmarkFilter{BenchmarkRef: api.BenchmarkRef{ID: "membench:latency", Config: cfg(`{"scope":"RAM"}`)}, Min: new(1.0)}},
		{name: "max on higher-is-better", bench: api.BenchmarkFilter{BenchmarkRef: gb(`{"cores":"single"}`), Max: new(1.0)}},
		{name: "neither min nor max", bench: api.BenchmarkFilter{BenchmarkRef: gb(`{"cores":"single"}`)}},
		{name: "bad normalize", bench: api.BenchmarkFilter{BenchmarkRef: gb(`{"cores":"single"}`), Min: new(1.0), Normalize: "PerGiB"}},
		{name: "unknown config", bench: api.BenchmarkFilter{BenchmarkRef: gb(`{"cores":"all"}`), Min: new(1.0)}, unknown: true},
		{name: "config required", bench: api.BenchmarkFilter{BenchmarkRef: api.BenchmarkRef{ID: "geekbench:score"}, Min: new(1.0)}, unknown: true},
		{name: "bogomips rejected", bench: api.BenchmarkFilter{BenchmarkRef: api.BenchmarkRef{ID: "bogomips"}, Min: new(1.0)}, unknown: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := specOf(&api.Selector{Benchmarks: []api.BenchmarkFilter{tt.bench}})
			_, err := Evaluate(spec, nil, Options{Catalog: catalog})
			want := ErrInvalidSpec
			if tt.unknown {
				want = keeper.ErrUnknownBenchmarkConfig
			}
			if !errors.Is(err, want) {
				t.Fatalf("want %v, got %v", want, err)
			}
			if _, err := PlanQueries(spec, "aws", []string{"us-east-1"}, Options{Catalog: catalog}); !errors.Is(err, want) {
				t.Errorf("PlanQueries: want %v, got %v", want, err)
			}
		})
	}
	wp := specOf(&api.Selector{WorkloadProfiles: []api.WorkloadProfileFilter{{Name: "web", Min: 1, Normalize: "Nope"}}})
	if err := Validate(wp, Options{Catalog: catalog}); !errors.Is(err, ErrInvalidSpec) {
		t.Errorf("workload profile normalize: %v", err)
	}
	wp = specOf(&api.Selector{WorkloadProfiles: []api.WorkloadProfileFilter{{Name: "nope", Min: 1}}})
	if err := Validate(wp, Options{Catalog: catalog}); !errors.Is(err, keeper.ErrUnknownBenchmarkConfig) {
		t.Errorf("unknown workload profile: %v", err)
	}
	if err := Validate(specOf(&api.Selector{WorkloadProfiles: []api.WorkloadProfileFilter{{Name: "web", Min: 1}}}), Options{}); err == nil {
		t.Error("benchmarks without a catalog accepted")
	}
}

// IncludeIfFamilyQualifies keeps an unscored size when a scored size of its
// family passes every benchmark filter; Include keeps every unscored size.
func TestUnbenchmarkedPolicies(t *testing.T) {
	k := key(t, "geekbench:score", `{"cores": "single"}`)
	bench := api.BenchmarkFilter{BenchmarkRef: api.BenchmarkRef{ID: k.ID, Config: cfg(k.Config)}, Min: new(2000.0)}
	for _, policy := range []api.UnbenchmarkedPolicy{api.UnbenchmarkedIncludeIfFamilyQualifies, api.UnbenchmarkedInclude} {
		t.Run(string(policy), func(t *testing.T) {
			sel := &api.Selector{Benchmarks: []api.BenchmarkFilter{bench}, Unbenchmarked: policy}
			res, cands := run(t, "aws", specOf(sel))
			qualifies := map[string]bool{}
			for _, c := range cands {
				if s, ok := c.Scores[k]; ok && s >= 2000 && c.Architecture != "" {
					qualifies[c.Family()] = true
				}
			}
			selected := selectedSet(res)
			var keptUnscored, droppedUnscored int
			for _, c := range cands {
				if c.Architecture == "" {
					continue
				}
				s, scored := c.Scores[k]
				_, isSel := selected[c.Name()]
				var want bool
				switch {
				case scored:
					want = s >= 2000
				case policy == api.UnbenchmarkedInclude:
					want = true
				default:
					want = qualifies[c.Family()]
				}
				if want != isSel {
					t.Errorf("%s (scored %v, family qualifies %v): selected %v, want %v (%+v)",
						c.Name(), scored, qualifies[c.Family()], isSel, want, res.Misses[c.Name()])
				}
				if !scored {
					if isSel {
						keptUnscored++
					} else {
						droppedUnscored++
					}
				}
			}
			if keptUnscored == 0 {
				t.Error("no unscored size kept")
			}
			if policy == api.UnbenchmarkedIncludeIfFamilyQualifies && droppedUnscored == 0 {
				t.Error("no unscored size dropped")
			}
		})
	}
}

// Without per-type data the noneOf flag filter passes only with Include.
func TestFlagsNoneOfInclude(t *testing.T) {
	sel := &api.Selector{
		CPU:           &api.CPUSelector{Flags: &api.CPUFlags{NoneOf: []string{"avx512f"}}},
		Unbenchmarked: api.UnbenchmarkedInclude,
	}
	res, cands := run(t, "azure", specOf(sel))
	selected := selectedSet(res)
	n := 0
	for _, c := range cands {
		if len(c.Server.CPUFlags) == 0 && c.Architecture != "" {
			n++
			if _, ok := selected[c.Name()]; !ok {
				t.Errorf("%s without flags excluded: %+v", c.Name(), res.Misses[c.Name()])
			}
		}
	}
	if n == 0 || res.Excluded.MissingData != 0 {
		t.Errorf("%d rows without flags, missingData %d", n, res.Excluded.MissingData)
	}
	// allOf still needs data.
	sel.CPU.Flags = &api.CPUFlags{AllOf: []string{"avx2"}}
	res, _ = run(t, "azure", specOf(sel))
	if res.Excluded.MissingData < n {
		t.Errorf("allOf: missingData %d < %d", res.Excluded.MissingData, n)
	}
}

func TestMissRelative(t *testing.T) {
	res, _ := run(t, "aws", specOf(&api.Selector{
		VCPUs: &api.IntRange{Min: new(int32(4))},
		Price: &api.PriceSelector{MaxHourly: "0.10"},
	}))
	// m7i.large: 2 vCPUs (50% below 4) and 0.1008 USD (0.8% above 0.10).
	m := res.Misses["m7i.large"]
	if m.Reason != "filters.vcpus" || math.Abs(m.Relative-0.5) > 1e-9 || m.NotOffered {
		t.Errorf("m7i.large miss = %+v", m)
	}
	m = res.Misses["m7i.xlarge"]
	if m.Reason != "filters.price.maxHourly" || math.Abs(m.Relative-(0.2016-0.10)/0.10) > 1e-6 {
		t.Errorf("m7i.xlarge miss = %+v", m)
	}
	if m := res.Misses["t2.micro"]; m.Reason != ReasonProviderIneligible || !m.NotOffered {
		t.Errorf("i386 type: %+v", m)
	}
}
