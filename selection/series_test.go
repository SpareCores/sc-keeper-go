package selection

import (
	"slices"
	"testing"

	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/SpareCores/sc-keeper-go/selection/api"
)

// Since sc-crawler 0.10.4 server.family is the broad class and server.series
// the hardware line; the family of a candidate is the series, else the family.
func TestCandidateFamilyPrefersSeries(t *testing.T) {
	str := func(s string) *string { return &s }
	for _, tc := range []struct {
		name, family string
		series       *string
		want         string
	}{
		{"series wins", "General Purpose", str("m7i"), "m7i"},
		{"nil series falls back", "General Purpose", nil, "General Purpose"},
		{"empty series falls back", "cax", str(""), "cax"},
		{"pre-0.10.4 row", "m7i", nil, "m7i"},
	} {
		c := &Candidate{Server: keeper.Server{Family: tc.family, Series: tc.series}}
		if got := c.Family(); got != tc.want {
			t.Errorf("%s: Family() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSeriesDrivesFamilyConstraints(t *testing.T) {
	str := func(s string) *string { return &s }
	mk := func(name, series string, price float64) keeper.Server {
		r := row(name, "General Purpose", 2, price)
		r.Series = str(series)
		return r
	}
	rows := []keeper.Server{mk("m7i.large", "m7i", 0.1), mk("t3a.large", "t3a", 0.1), mk("n2-standard-2", "n2", 0.1)}
	scores := map[string]float64{"m7i.large": 100, "t3a.large": 90, "n2-standard-2": 80}

	names := func(r Result) []string {
		n := make([]string, 0, len(r.InstanceTypes))
		for _, it := range r.InstanceTypes {
			n = append(n, it.Name)
		}
		return n
	}

	// Families glob matches the series, not the class.
	spec := specOf(&api.Selector{Families: &api.GlobFilter{Include: []string{"t3*"}}})
	if got := names(evalSynth(t, spec, synth(rows, scores))); !slices.Equal(got, []string{"t3a.large"}) {
		t.Errorf("family include t3*: %v", got)
	}

	// Ranking counts series as families: max 2 keeps the two best series.
	two := int32(2)
	r := api.Ranking{Score: api.RankingScore{Benchmark: geekbenchSingle()}, Families: &api.RankingFamilies{Max: &two}}
	res := evalSynth(t, rankingSpec(nil, r), synth(rows, scores))
	kept := make([]string, 0, len(res.Families))
	for _, f := range res.Families {
		kept = append(kept, f.Name)
	}
	if !slices.Equal(kept, []string{"m7i", "t3a"}) {
		t.Errorf("ranked families %v, want [m7i t3a]", kept)
	}
}
