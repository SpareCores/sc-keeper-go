/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package selection

import (
	"errors"
	"math"
	"slices"
	"testing"

	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/SpareCores/sc-keeper-go/selection/api"
)

func geekbenchSingle() *api.BenchmarkRef {
	return &api.BenchmarkRef{ID: "geekbench:score", Config: cfg(`{"cores": "single"}`)}
}

func rankingSpec(sel *api.Selector, r api.Ranking) *api.Spec {
	s := specOf(sel)
	s.Ranking = &r
	return s
}

// familyMedians recomputes the documented ranking input independently: the
// median objective of the qualifying, scored sizes of each family.
func familyMedians(cands []*Candidate, qualifies func(*Candidate) bool, value func(*Candidate) (float64, bool)) map[string]float64 {
	values := map[string][]float64{}
	for _, c := range cands {
		if !qualifies(c) {
			continue
		}
		if v, ok := value(c); ok {
			values[c.Family()] = append(values[c.Family()], v)
		}
	}
	out := map[string]float64{}
	for f, vs := range values {
		out[f] = median(vs)
	}
	return out
}

// The worked example of selectors.md: "the fastest single-core machines, but
// not too few for capacity fallback, and nothing above $0.50/h". Properties
// only, so a snapshot refresh does not break it.
func TestRankingWorkedExample(t *testing.T) {
	k := key(t, "geekbench:score", `{"cores": "single"}`)
	spec := rankingSpec(&api.Selector{Price: &api.PriceSelector{MaxHourly: "0.50"}}, api.Ranking{
		Score:     api.RankingScore{Benchmark: geekbenchSingle()},
		Objective: api.ObjectiveScore,
		Band:      &api.RankingBand{WithinPercent: 12},
		Families:  &api.RankingFamilies{Min: new(int32(3)), Max: new(int32(6))},
	})
	res, cands := run(t, "aws", spec)

	affordable := func(c *Candidate) bool {
		p, ok := c.Price(keeper.AllocationOnDemand, 0)
		return c.Architecture != "" && ok && p <= 0.50
	}
	medians := familyMedians(cands, affordable, func(c *Candidate) (float64, bool) {
		s, ok := c.Scores[k]
		return s, ok
	})
	best, bestFamily := 0.0, ""
	for f, v := range medians {
		if v > best || (v == best && f < bestFamily) {
			best, bestFamily = v, f
		}
	}
	b := res.Band
	if b == nil || b.Reference != bestFamily || b.ReferenceValue != best {
		t.Fatalf("band = %+v, want reference %s (%v)", b, bestFamily, best)
	}
	for _, f := range res.Families {
		t.Logf("family %d %s: median %v, %d sizes", f.Rank, f.Name, *f.Objective, f.Sizes)
	}
	t.Logf("band %+v, %d types", *res.Band, len(res.InstanceTypes))
	if n := len(res.Families); n < 3 || n > 6 {
		t.Fatalf("%d families kept, want 3..6", n)
	}
	kept := checkKeptFamilies(t, res, medians, best)
	// Every qualifying, scored size of a kept family is selected; nothing
	// else is.
	selected := selectedSet(res)
	for _, c := range cands {
		_, scored := c.Scores[k]
		want := kept[c.Family()] && affordable(c) && scored
		if _, got := selected[c.Name()]; got != want {
			t.Errorf("%s: selected %v, want %v (%+v)", c.Name(), got, want, res.Misses[c.Name()])
		}
	}
	for _, it := range selected {
		if it.Score == nil || it.Price > 0.50 {
			t.Errorf("%s: score %v, price %v", it.Name, it.Score, it.Price)
		}
	}
	lowest := res.Families[len(res.Families)-1]
	if b.LowestKept != lowest.Name || math.Abs(b.LowestKeptDeltaPercent-(best-*lowest.Objective)/best*100) > 1e-9 {
		t.Errorf("band lowest = %s %v, want %s", b.LowestKept, b.LowestKeptDeltaPercent, lowest.Name)
	}
	if res.Excluded.Ranking == 0 || res.Excluded.Filters["price.maxHourly"] == 0 {
		t.Errorf("excluded = %+v", res.Excluded)
	}
	st := b.Status()
	if st.Reference != b.Reference || st.ReferenceValue == nil || st.LowestKeptDeltaPercent == nil {
		t.Errorf("band status = %+v", st)
	}
}

// checkKeptFamilies: kept families are in rank order with their medians,
// inside the 12% band unless families.min widened it, and no family inside
// the band is dropped unless families.max trimmed it.
func checkKeptFamilies(t *testing.T, res Result, medians map[string]float64, best float64) map[string]bool {
	t.Helper()
	b := res.Band
	kept := map[string]bool{}
	for i, f := range res.Families {
		kept[f.Name] = true
		if f.Rank != i+1 || f.Objective == nil || *f.Objective != medians[f.Name] {
			t.Errorf("family %+v: want rank %d, median %v", f, i+1, medians[f.Name])
		}
		if medians[f.Name] < best*0.88 && !b.Widened {
			t.Errorf("family %s (%v) outside the band without widening", f.Name, medians[f.Name])
		}
		if i > 0 && medians[f.Name] > medians[res.Families[i-1].Name] {
			t.Errorf("families not in rank order at %s", f.Name)
		}
	}
	for f, v := range medians {
		if !kept[f] && v >= best*0.88 && !b.Trimmed {
			t.Errorf("family %s (%v) is inside the band but not kept", f, v)
		}
	}
	return kept
}

func TestRankingWidenTrim(t *testing.T) {
	base := api.Ranking{Score: api.RankingScore{Benchmark: geekbenchSingle()}}
	sel := &api.Selector{Price: &api.PriceSelector{MaxHourly: "0.50"}}
	tests := []struct {
		name        string
		band        *float64
		min, max    *int32
		want        int
		widen, trim bool
	}{
		{name: "widened by families.min", band: new(0.0), min: new(int32(4)), want: 4, widen: true},
		{name: "trimmed by families.max", band: new(60.0), max: new(int32(2)), want: 2, trim: true},
		{name: "families.max without band", max: new(int32(3)), want: 3, trim: true},
		{name: "band only", band: new(5.0)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := base
			if tt.band != nil {
				r.Band = &api.RankingBand{WithinPercent: *tt.band}
			}
			r.Families = &api.RankingFamilies{Min: tt.min, Max: tt.max}
			res, _ := run(t, "aws", rankingSpec(sel, r))
			if tt.want > 0 && len(res.Families) != tt.want {
				t.Errorf("%d families, want %d", len(res.Families), tt.want)
			}
			if res.Band.Widened != tt.widen || res.Band.Trimmed != tt.trim {
				t.Errorf("band = %+v", res.Band)
			}
			n := 0
			for _, m := range res.Misses {
				if m.Reason == ReasonRanking {
					n++
					if m.Relative < 0 || math.IsInf(m.Relative, 1) {
						t.Errorf("ranking miss %+v", m)
					}
				}
			}
			if n == 0 {
				t.Error("nothing dropped by ranking")
			}
		})
	}
}

func TestRankingObjectives(t *testing.T) {
	tests := []struct {
		name       string
		ranking    api.Ranking
		lowerFirst bool
	}{
		{name: "price", lowerFirst: true, ranking: api.Ranking{
			Score: api.RankingScore{WorkloadProfile: "web"}, Objective: api.ObjectivePrice,
			Band: &api.RankingBand{WithinPercent: 20}}},
		{name: "score per price", ranking: api.Ranking{
			Score: api.RankingScore{WorkloadProfile: "web"}, Objective: api.ObjectiveScorePerPrice,
			Band: &api.RankingBand{WithinPercent: 20}}},
		{name: "per vCPU", ranking: api.Ranking{
			Score: api.RankingScore{Benchmark: &api.BenchmarkRef{ID: "stress_ng:bestn"}}, Normalize: api.RankingNormalizePerVcpu,
			Band: &api.RankingBand{WithinPercent: 10}, Families: &api.RankingFamilies{Max: new(int32(4))}}},
		{name: "lower-is-better score", lowerFirst: true, ranking: api.Ranking{
			Score: api.RankingScore{Benchmark: &api.BenchmarkRef{ID: "membench:latency", Config: cfg(`{"scope": "RAM"}`)}},
			Band:  &api.RankingBand{WithinPercent: 10}, Families: &api.RankingFamilies{Min: new(int32(2))}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, _ := run(t, "aws", rankingSpec(nil, tt.ranking))
			if len(res.Families) == 0 {
				t.Fatal("nothing kept")
			}
			var values []float64
			for _, f := range res.Families {
				values = append(values, *f.Objective)
			}
			sorted := slices.IsSorted(values)
			if !tt.lowerFirst {
				r := slices.Clone(values)
				slices.Reverse(r)
				sorted = slices.IsSorted(r)
			}
			if !sorted {
				t.Errorf("families not in rank order: %v", values)
			}
			b := res.Band
			p := tt.ranking.Band.WithinPercent / 100
			for _, v := range values {
				if !b.Widened && ((tt.lowerFirst && v > b.ReferenceValue*(1+p)*(1+1e-9)) ||
					(!tt.lowerFirst && v < b.ReferenceValue*(1-p)*(1-1e-9))) {
					t.Errorf("value %v outside the band around %v", v, b.ReferenceValue)
				}
			}
		})
	}
}

func TestRankingValidation(t *testing.T) {
	gb := api.RankingScore{Benchmark: geekbenchSingle()}
	tests := []struct {
		name string
		r    api.Ranking
	}{
		{name: "no band, no max", r: api.Ranking{Score: gb}},
		{name: "min > max", r: api.Ranking{Score: gb, Families: &api.RankingFamilies{Min: new(int32(3)), Max: new(int32(2))}}},
		{name: "min 0", r: api.Ranking{Score: gb, Families: &api.RankingFamilies{Min: new(int32(0)), Max: new(int32(2))}}},
		{name: "both scores", r: api.Ranking{Score: api.RankingScore{Benchmark: geekbenchSingle(), WorkloadProfile: "web"}, Band: &api.RankingBand{WithinPercent: 5}}},
		{name: "no score", r: api.Ranking{Band: &api.RankingBand{WithinPercent: 5}}},
		{name: "band > 100", r: api.Ranking{Score: gb, Band: &api.RankingBand{WithinPercent: 120}}},
		{name: "bad objective", r: api.Ranking{Score: gb, Objective: "Fastest", Band: &api.RankingBand{WithinPercent: 5}}},
		{name: "bad normalize", r: api.Ranking{Score: gb, Normalize: "PerPrice", Band: &api.RankingBand{WithinPercent: 5}}},
		{name: "score per price of a latency", r: api.Ranking{
			Score:     api.RankingScore{Benchmark: &api.BenchmarkRef{ID: "membench:latency", Config: cfg(`{"scope":"RAM"}`)}},
			Objective: api.ObjectiveScorePerPrice, Band: &api.RankingBand{WithinPercent: 5}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Validate(rankingSpec(nil, tt.r), Options{Catalog: catalog}); !errors.Is(err, ErrInvalidSpec) {
				t.Errorf("want ErrInvalidSpec, got %v", err)
			}
		})
	}
	bad := rankingSpec(nil, api.Ranking{Score: api.RankingScore{Benchmark: &api.BenchmarkRef{ID: "geekbench:score"}},
		Band: &api.RankingBand{WithinPercent: 5}})
	if err := Validate(bad, Options{Catalog: catalog}); !errors.Is(err, keeper.ErrUnknownBenchmarkConfig) {
		t.Errorf("missing config: %v", err)
	}
}

// With ranking, unscored sizes follow their family under both lenient
// policies; families without any scored size cannot be ranked.
func TestRankingUnbenchmarked(t *testing.T) {
	k := key(t, "geekbench:score", `{"cores": "single"}`)
	for _, policy := range []api.UnbenchmarkedPolicy{api.UnbenchmarkedIncludeIfFamilyQualifies, api.UnbenchmarkedInclude} {
		t.Run(string(policy), func(t *testing.T) {
			spec := rankingSpec(&api.Selector{Unbenchmarked: policy}, api.Ranking{
				Score: api.RankingScore{Benchmark: geekbenchSingle()},
				Band:  &api.RankingBand{WithinPercent: 100},
			})
			res, cands := run(t, "aws", spec)
			scoredFamily := map[string]bool{}
			for _, c := range cands {
				if _, ok := c.Scores[k]; ok && c.Architecture != "" {
					scoredFamily[c.Family()] = true
				}
			}
			selected := selectedSet(res)
			unscoredKept := 0
			for _, c := range cands {
				if c.Architecture == "" {
					continue
				}
				_, isSel := selected[c.Name()]
				// A 100% band keeps every rankable family.
				if want := scoredFamily[c.Family()]; isSel != want {
					t.Errorf("%s: selected %v, want %v (%+v)", c.Name(), isSel, want, res.Misses[c.Name()])
				}
				if _, ok := c.Scores[k]; !ok && isSel {
					unscoredKept++
					if selected[c.Name()].Score != nil {
						t.Errorf("%s: unscored type reports a score", c.Name())
					}
				}
			}
			if unscoredKept == 0 {
				t.Error("no unscored size kept")
			}
			wantReason := ReasonRanking
			if policy == api.UnbenchmarkedIncludeIfFamilyQualifies {
				wantReason = ReasonMissingData
			}
			for _, c := range cands {
				if c.Architecture != "" && !scoredFamily[c.Family()] {
					if got := res.Misses[c.Name()].Reason; got != wantReason {
						t.Errorf("%s (family without scores): reason %q, want %q", c.Name(), got, wantReason)
					}
				}
			}
		})
	}
}
