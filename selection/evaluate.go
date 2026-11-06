/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package selection

import (
	"math"
	"slices"
	"strings"

	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/SpareCores/sc-keeper-go/selection/api"
)

// Options are the provider-supplied inputs of an evaluation (the caller builds
// them from its provider adapter and pool template).
type Options struct {
	// Catalog resolves benchmark configs; required when the spec uses
	// benchmarks, workload profiles or ranking.
	Catalog *keeper.BenchmarkCatalog
	// Eligible is the provider's eligibility predicate (nil: every type).
	Eligible func(*Candidate) bool
	// ManagedFees are per-instance-type hourly USD fees the provider charges
	// on top of the instance price (EKS Auto Mode). Non-nil turns
	// price.includeManagedFee on by default; a type without an entry then
	// has no usable price.
	ManagedFees map[string]float64
	// DefaultBasis is the price basis when selector.price.basis is not set
	// (Spot if the template allows only spot capacity).
	DefaultBasis api.PriceBasis
	// Architectures is the template's kubernetes.io/arch constraint (nil:
	// none).
	Architectures []api.Architecture
	// InstanceTypeAllowed is the template's node.kubernetes.io/instance-type
	// constraint (nil: none). Types it rejects count as template
	// constraints.
	InstanceTypeAllowed func(name string) bool
}

// Result is the outcome of Evaluate.
type Result struct {
	// Basis is the price basis used.
	Basis api.PriceBasis
	// InstanceTypes are the selected types, sorted by name.
	InstanceTypes []InstanceType
	// Families are the families of the selected types: in rank order with
	// ranking, else sorted by name (Rank 0).
	Families []Family
	// Band reports the applied ranking band (nil without ranking).
	Band *Band
	// Excluded counts the candidates dropped per pipeline step.
	Excluded Excluded
	// Misses has an entry for every candidate that was not selected.
	Misses map[string]Miss
}

// Names returns the selected instance type names, sorted.
func (r *Result) Names() []string {
	out := make([]string, len(r.InstanceTypes))
	for i, t := range r.InstanceTypes {
		out[i] = t.Name
	}
	return out
}

// InstanceType is one selected type.
type InstanceType struct {
	Name         string
	Family       string
	Architecture api.Architecture
	// Score is the (normalised) ranking score, nil without ranking or score.
	Score *float64
	// Price is the hourly USD price on the basis, including the managed fee
	// where it applies.
	Price float64
}

// Family is one family of the selection.
type Family struct {
	Name string
	// Rank starts at 1; 0 without ranking.
	Rank int
	// Objective is the family's median objective value (nil without ranking).
	Objective *float64
	// Sizes is the number of selected types of the family.
	Sizes int
}

// Band reports what the ranking applied.
type Band struct {
	Reference      string
	ReferenceValue float64
	LowestKept     string
	// LowestKeptDeltaPercent is the distance of LowestKept from the reference
	// in percent of the reference value (always >= 0).
	LowestKeptDeltaPercent float64
	Widened                bool
	Trimmed                bool
}

// Excluded counts candidates dropped per pipeline step. Each candidate is
// counted once, in the first step (and the first filter) it fails.
type Excluded struct {
	ProviderIneligible  int
	TemplateConstraints int
	// Filters is keyed by selector field (vcpus, cpu.flags, benchmarks, ...).
	Filters     map[string]int
	MissingData int
	Ranking     int
}

// Exclusion reasons of Miss.Reason (filters are "filters.<field>").
const (
	ReasonProviderIneligible  = "providerIneligible"
	ReasonTemplateConstraints = "templateConstraints"
	ReasonMissingData         = "missingData"
	ReasonRanking             = "ranking"
	ReasonFilterPrefix        = "filters."
)

// Miss describes why a candidate was not selected, for churn control and
// troubleshooting.
type Miss struct {
	// Reason is the pipeline step (and filter) that excluded the type, as
	// counted in Excluded.
	Reason string
	// NotOffered: the provider cannot launch the type, or it has no price
	// on the basis in the target regions.
	NotOffered bool
	// Relative is the worst relative miss over the thresholds the type fails
	// (0.07 = 7% beyond the threshold); +Inf for non-numeric filters,
	// missing data and template constraints.
	Relative float64
}

// item is a candidate on its way through the pipeline.
type item struct {
	c     *Candidate
	price float64
	// unscored: lacks a benchmark score needed by a filter or the ranking,
	// and is kept by the data-quality policy so far.
	unscored bool
	// rankScore is the (normalised) ranking score, nil if missing.
	rankScore *float64
}

// Evaluate applies template constraints, selectors, the data-quality policy
// and ranking to the candidates (docs/reference/selectors.md, steps 3–7).
func Evaluate(spec *api.Spec, candidates []*Candidate, opts Options) (Result, error) {
	cs, err := compile(spec, opts)
	if err != nil {
		return Result{}, err
	}
	res := Result{
		Basis:    cs.basis,
		Excluded: Excluded{Filters: map[string]int{}},
		Misses:   map[string]Miss{},
	}
	ctx := &evalCtx{policy: cs.policy}
	filters := cs.filters()

	// Steps 3–4: eligibility, price availability, template constraints.
	var eligible []*item
	for _, c := range candidates {
		switch {
		case c.Architecture == "" || (opts.Eligible != nil && !opts.Eligible(c)):
			res.Excluded.ProviderIneligible++
			res.Misses[c.Name()] = Miss{Reason: ReasonProviderIneligible, NotOffered: true, Relative: math.Inf(1)}
			continue
		case opts.Architectures != nil && !slices.Contains(opts.Architectures, c.Architecture),
			opts.InstanceTypeAllowed != nil && !opts.InstanceTypeAllowed(c.Name()):
			res.Excluded.TemplateConstraints++
			res.Misses[c.Name()] = Miss{Reason: ReasonTemplateConstraints, Relative: math.Inf(1)}
			continue
		}
		price, ok := cs.price(c, opts.ManagedFees)
		if !ok {
			res.Excluded.MissingData++
			res.Misses[c.Name()] = Miss{Reason: ReasonMissingData, NotOffered: true, Relative: math.Inf(1)}
			continue
		}
		eligible = append(eligible, &item{c: c, price: price})
	}

	qualified := cs.qualifiedFamilies(eligible)

	// Steps 5–6: selectors and the data-quality policy.
	var survivors []*item
	for _, it := range eligible {
		bucket, miss := cs.evaluateItem(it, filters, ctx, qualified)
		if bucket == "" {
			survivors = append(survivors, it)
			continue
		}
		reason := ReasonMissingData
		if bucket == bucketMissing {
			res.Excluded.MissingData++
		} else {
			res.Excluded.Filters[bucket]++
			reason = ReasonFilterPrefix + bucket
		}
		res.Misses[it.c.Name()] = Miss{Reason: reason, Relative: miss}
	}

	// Step 7: ranking.
	selected := survivors
	if cs.ranking != nil {
		selected = cs.rank(survivors, &res)
	}
	res.InstanceTypes, res.Families = summarise(selected, cs.ranking != nil, res.Families)
	return res, nil
}

const bucketMissing = "\x00missing"

// price returns the candidate's price on the basis, including the managed
// fee when it applies.
func (cs *compiled) price(c *Candidate, fees map[string]float64) (float64, bool) {
	fee := 0.0
	if cs.includeFee {
		f, ok := fees[c.Name()]
		if !ok {
			return 0, false
		}
		fee = f
	}
	return c.Price(cs.allocation, fee)
}

// evaluateItem runs every filter on one item. It returns the counter the item
// is excluded under ("" if it survives, bucketMissing for missing data) and
// the worst relative miss over everything that excludes it.
func (cs *compiled) evaluateItem(it *item, filters []filter, ctx *evalCtx, qualified map[string]bool) (string, float64) {
	bucket := ""
	worst := 0.0
	note := func(name string, o outcome) {
		if o.v == pass {
			return
		}
		if bucket == "" {
			bucket = name
			if o.v == missing {
				bucket = bucketMissing
			}
		}
		worst = math.Max(worst, o.miss)
	}
	for _, f := range filters {
		note(f.name, f.eval(it.c, it.price, ctx))
	}

	scoresMissing := false
	for _, b := range cs.benches {
		o := cs.benchOutcome(it, b)
		if o.v == missing {
			scoresMissing = true
			continue
		}
		note(b.field, o)
	}
	if cs.ranking != nil {
		it.rankScore = cs.rankScore(it)
		if it.rankScore == nil && cs.ranking.objective != api.ObjectivePrice {
			scoresMissing = true
		}
	}
	if scoresMissing {
		switch cs.policy {
		case api.UnbenchmarkedInclude:
			it.unscored = true
		case api.UnbenchmarkedIncludeIfFamilyQualifies:
			if qualified[it.c.Family()] {
				it.unscored = true
			} else {
				note("", noData)
			}
		default:
			note("", noData)
		}
	}
	return bucket, worst
}

// benchOutcome evaluates one benchmark filter. Types a Keeper threshold
// filtered out fail with an unknown (infinite) miss: the threshold was pushed
// down loosened by the churn margin, so they miss it by more than that.
func (cs *compiled) benchOutcome(it *item, b benchFilter) outcome {
	score, ok := it.c.Scores[b.key]
	if !ok {
		if it.c.KeeperFiltered[b.key] {
			return failed
		}
		return noData
	}
	v, ok := normalise(score, b.normalize, it)
	if !ok {
		return noData
	}
	return floatBounds(v, b.min, b.max)
}

func normalise(score float64, n api.Normalize, it *item) (float64, bool) {
	switch n {
	case api.NormalizePerVcpu:
		if it.c.VCPUs() <= 0 {
			return 0, false
		}
		return score / float64(it.c.VCPUs()), true
	case api.NormalizePerPrice:
		return score / it.price, true
	}
	return score, true
}

// qualifiedFamilies returns the families with at least one fully scored type
// that passes every benchmark filter (unbenchmarked: IncludeIfFamilyQualifies).
func (cs *compiled) qualifiedFamilies(items []*item) map[string]bool {
	out := map[string]bool{}
	if cs.policy != api.UnbenchmarkedIncludeIfFamilyQualifies {
		return out
	}
	for _, it := range items {
		if out[it.c.Family()] {
			continue
		}
		ok := true
		for _, b := range cs.benches {
			if cs.benchOutcome(it, b).v != pass {
				ok = false
				break
			}
		}
		if ok && cs.ranking != nil && cs.ranking.objective != api.ObjectivePrice && cs.rankScore(it) == nil {
			ok = false
		}
		if ok {
			out[it.c.Family()] = true
		}
	}
	return out
}

// summarise builds the sorted instance types and the family list.
func summarise(selected []*item, ranked bool, rankedFamilies []Family) ([]InstanceType, []Family) {
	types := make([]InstanceType, 0, len(selected))
	sizes := map[string]int{}
	for _, it := range selected {
		types = append(types, InstanceType{
			Name:         it.c.Name(),
			Family:       it.c.Family(),
			Architecture: it.c.Architecture,
			Score:        it.rankScore,
			Price:        it.price,
		})
		sizes[it.c.Family()]++
	}
	slices.SortFunc(types, func(a, b InstanceType) int { return strings.Compare(a.Name, b.Name) })
	if ranked {
		for i := range rankedFamilies {
			rankedFamilies[i].Sizes = sizes[rankedFamilies[i].Name]
		}
		return types, rankedFamilies
	}
	families := make([]Family, 0, len(sizes))
	for name, n := range sizes {
		families = append(families, Family{Name: name, Sizes: n})
	}
	slices.SortFunc(families, func(a, b Family) int { return strings.Compare(a.Name, b.Name) })
	return types, families
}
