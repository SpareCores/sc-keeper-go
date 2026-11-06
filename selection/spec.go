/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package selection

import (
	"errors"
	"fmt"
	"path"
	"strconv"

	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/SpareCores/sc-keeper-go/selection/api"
)

// ErrInvalidSpec is matched (errors.Is) by every validation error of the
// selector and ranking that the CRD schema cannot express. Unknown benchmark
// configs match keeper.ErrUnknownBenchmarkConfig instead.
var ErrInvalidSpec = errors.New("invalid selector")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidSpec, fmt.Sprintf(format, args...))
}

// Defaults of spec.churn.
const (
	DefaultRemoveMarginPercent   = 5.0
	DefaultRemoveAfterRefreshes  = 2
	DefaultMaxRemovalsPerRefresh = 3
)

// benchFilter is one resolved entry of selector.benchmarks or
// selector.workloadProfiles.
type benchFilter struct {
	// field is the counter name: "benchmarks" or "workloadProfiles".
	field         string
	key           keeper.BenchmarkKey
	min, max      *float64
	normalize     api.Normalize
	lowerIsBetter bool
}

// rankSpec is the resolved spec.ranking.
type rankSpec struct {
	key           keeper.BenchmarkKey
	lowerIsBetter bool
	objective     api.Objective
	normalize     api.RankingNormalize
	band          *float64
	familiesMin   int
	familiesMax   int // 0: no limit
}

// compiled is a validated spec with defaults applied and benchmarks resolved.
type compiled struct {
	sel              api.Selector
	basis            api.PriceBasis
	allocation       keeper.Allocation
	includeFee       bool
	maxHourly        *float64
	maxHourlyPerVCPU *float64
	benches          []benchFilter
	ranking          *rankSpec
	policy           api.UnbenchmarkedPolicy
	marginPercent    float64
}

// ResolveBasis returns the price basis of a spec: selector.price.basis if
// set, else the default (from the template's capacity-type requirement),
// else OnDemand.
func ResolveBasis(spec *api.Spec, defaultBasis api.PriceBasis) api.PriceBasis {
	if spec.Selector != nil && spec.Selector.Price != nil && spec.Selector.Price.Basis != "" {
		return spec.Selector.Price.Basis
	}
	if defaultBasis != "" {
		return defaultBasis
	}
	return api.PriceBasisOnDemand
}

// AllocationOf maps a price basis to Keeper's best_price_allocation.
func AllocationOf(b api.PriceBasis) keeper.Allocation {
	if b == api.PriceBasisSpot {
		return keeper.AllocationSpot
	}
	return keeper.AllocationOnDemand
}

// Validate checks the parts of the selector and ranking the CRD schema cannot
// (benchmark configs against the catalog, benchmark directions, price
// strings, globs).
func Validate(spec *api.Spec, opts Options) error {
	_, err := compile(spec, opts)
	return err
}

func compile(spec *api.Spec, opts Options) (*compiled, error) {
	c := &compiled{policy: api.UnbenchmarkedExclude, marginPercent: DefaultRemoveMarginPercent}
	if spec.Selector != nil {
		c.sel = *spec.Selector
	}
	if c.sel.Unbenchmarked != "" {
		c.policy = c.sel.Unbenchmarked
	}
	if spec.Churn.RemoveMarginPercent != nil {
		c.marginPercent = *spec.Churn.RemoveMarginPercent
	}
	c.basis = ResolveBasis(spec, opts.DefaultBasis)
	c.allocation = AllocationOf(c.basis)
	c.includeFee = opts.ManagedFees != nil
	if p := c.sel.Price; p != nil {
		if opts.ManagedFees != nil && p.IncludeManagedFee != nil {
			c.includeFee = *p.IncludeManagedFee
		}
		var err error
		if c.maxHourly, err = parseMoney("price.maxHourly", p.MaxHourly); err != nil {
			return nil, err
		}
		if c.maxHourlyPerVCPU, err = parseMoney("price.maxHourlyPerVcpu", p.MaxHourlyPerVCPU); err != nil {
			return nil, err
		}
	}
	for _, g := range []*api.GlobFilter{c.sel.InstanceTypes, c.sel.Families} {
		if err := validateGlobs(g); err != nil {
			return nil, err
		}
	}
	if err := c.compileBenchmarks(opts.Catalog); err != nil {
		return nil, err
	}
	if spec.Ranking != nil {
		r, err := compileRanking(spec.Ranking, opts.Catalog)
		if err != nil {
			return nil, err
		}
		c.ranking = r
	}
	return c, nil
}

func (c *compiled) compileBenchmarks(catalog *keeper.BenchmarkCatalog) error {
	for i, b := range c.sel.Benchmarks {
		key, err := resolve(catalog, b.BenchmarkRef)
		if err != nil {
			return fmt.Errorf("selector.benchmarks[%d]: %w", i, err)
		}
		lower := keeper.LowerIsBetter(key.ID)
		switch {
		case b.Min == nil && b.Max == nil:
			return invalid("selector.benchmarks[%d] (%s): set min or max", i, key.ID)
		case lower && b.Min != nil:
			return invalid("selector.benchmarks[%d]: %s is lower-is-better, use max instead of min", i, key.ID)
		case !lower && b.Max != nil:
			return invalid("selector.benchmarks[%d]: %s is higher-is-better, use min instead of max", i, key.ID)
		}
		norm, err := normalizeOf(b.Normalize)
		if err != nil {
			return fmt.Errorf("selector.benchmarks[%d]: %w", i, err)
		}
		c.benches = append(c.benches, benchFilter{
			field: "benchmarks", key: key, min: b.Min, max: b.Max, normalize: norm, lowerIsBetter: lower,
		})
	}
	for i, w := range c.sel.WorkloadProfiles {
		key, err := resolve(catalog, api.BenchmarkRef{ID: keeper.WorkloadProfileID(string(w.Name))})
		if err != nil {
			return fmt.Errorf("selector.workloadProfiles[%d]: %w", i, err)
		}
		norm, err := normalizeOf(w.Normalize)
		if err != nil {
			return fmt.Errorf("selector.workloadProfiles[%d]: %w", i, err)
		}
		c.benches = append(c.benches, benchFilter{field: "workloadProfiles", key: key, min: &w.Min, normalize: norm})
	}
	return nil
}

func compileRanking(r *api.Ranking, catalog *keeper.BenchmarkCatalog) (*rankSpec, error) {
	var ref api.BenchmarkRef
	switch {
	case r.Score.Benchmark != nil && r.Score.WorkloadProfile == "":
		ref = *r.Score.Benchmark
	case r.Score.Benchmark == nil && r.Score.WorkloadProfile != "":
		ref = api.BenchmarkRef{ID: keeper.WorkloadProfileID(string(r.Score.WorkloadProfile))}
	default:
		return nil, invalid("ranking.score: set exactly one of benchmark and workloadProfile")
	}
	key, err := resolve(catalog, ref)
	if err != nil {
		return nil, fmt.Errorf("ranking.score: %w", err)
	}
	rs := &rankSpec{
		key:           key,
		lowerIsBetter: keeper.LowerIsBetter(key.ID),
		objective:     r.Objective,
		normalize:     r.Normalize,
		familiesMin:   1,
	}
	if rs.objective == "" {
		rs.objective = api.ObjectiveScore
	}
	if rs.normalize == "" {
		rs.normalize = api.RankingNormalizeNone
	}
	switch rs.objective {
	case api.ObjectiveScore, api.ObjectivePrice:
	case api.ObjectiveScorePerPrice:
		if rs.lowerIsBetter {
			// "Score per dollar" has no meaning for latencies: dividing by the
			// price would favour expensive types.
			return nil, invalid("ranking: objective ScorePerPrice needs a higher-is-better score, %s is lower-is-better", key.ID)
		}
	default:
		return nil, invalid("ranking.objective: unknown value %q", rs.objective)
	}
	if rs.normalize != api.RankingNormalizeNone && rs.normalize != api.RankingNormalizePerVcpu {
		return nil, invalid("ranking.normalize: unknown value %q", rs.normalize)
	}
	if r.Band != nil {
		if r.Band.WithinPercent < 0 || r.Band.WithinPercent > 100 {
			return nil, invalid("ranking.band.withinPercent must be within 0..100")
		}
		rs.band = &r.Band.WithinPercent
	}
	if f := r.Families; f != nil {
		if f.Min != nil {
			rs.familiesMin = int(*f.Min)
		}
		if f.Max != nil {
			rs.familiesMax = int(*f.Max)
		}
	}
	switch {
	case rs.band == nil && rs.familiesMax == 0:
		return nil, invalid("ranking needs band or families.max")
	case rs.familiesMin < 1:
		return nil, invalid("ranking.families.min must be at least 1")
	case rs.familiesMax < 0, rs.familiesMax > 0 && rs.familiesMin > rs.familiesMax:
		return nil, invalid("ranking.families.min must not exceed families.max")
	}
	return rs, nil
}

func resolve(catalog *keeper.BenchmarkCatalog, ref api.BenchmarkRef) (keeper.BenchmarkKey, error) {
	if catalog == nil {
		return keeper.BenchmarkKey{}, errors.New("no benchmark catalog available")
	}
	var raw []byte
	if ref.Config != nil {
		raw = ref.Config.Raw
	}
	return catalog.Resolve(ref.ID, raw)
}

func normalizeOf(n api.Normalize) (api.Normalize, error) {
	switch n {
	case "":
		return api.NormalizeNone, nil
	case api.NormalizeNone, api.NormalizePerVcpu, api.NormalizePerPrice:
		return n, nil
	}
	return "", invalid("normalize: unknown value %q", n)
}

func parseMoney(field, s string) (*float64, error) {
	if s == "" {
		return nil, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return nil, invalid("%s: %q is not a non-negative decimal", field, s)
	}
	return &v, nil
}

func validateGlobs(g *api.GlobFilter) error {
	if g == nil {
		return nil
	}
	for _, list := range [][]string{g.Include, g.Exclude} {
		for _, p := range list {
			if _, err := path.Match(p, ""); err != nil {
				return invalid("glob %q: %v", p, err)
			}
		}
	}
	return nil
}
