/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package selection

import (
	"cmp"
	"math"
	"slices"
	"strings"

	"github.com/SpareCores/sc-keeper-go/selection/api"
)

// rankScore is the item's ranking score, normalised per vCPU if asked; nil
// when the type has no score.
func (cs *compiled) rankScore(it *item) *float64 {
	score, ok := it.c.Scores[cs.ranking.key]
	if !ok {
		return nil
	}
	if cs.ranking.normalize == api.RankingNormalizePerVcpu {
		if it.c.VCPUs() <= 0 {
			return nil
		}
		score /= float64(it.c.VCPUs())
	}
	return &score
}

// objective is the item's ranking value; ok is false when it cannot be
// computed (no score).
func (cs *compiled) objective(it *item) (float64, bool) {
	switch cs.ranking.objective {
	case api.ObjectivePrice:
		return it.price, true
	case api.ObjectiveScorePerPrice:
		if it.rankScore == nil {
			return 0, false
		}
		return *it.rankScore / it.price, true
	}
	if it.rankScore == nil {
		return 0, false
	}
	return *it.rankScore, true
}

// lowerFirst reports whether lower objective values rank first.
func (cs *compiled) lowerFirst() bool {
	return cs.ranking.objective == api.ObjectivePrice ||
		(cs.ranking.objective == api.ObjectiveScore && cs.ranking.lowerIsBetter)
}

type familyRank struct {
	name  string
	value float64
	items []*item
}

// rank groups the items by family, ranks families by their median objective
// and keeps the families inside the band, widened to families.min and
// trimmed to families.max (selectors.md, "Ranking"). Every item of a kept
// family is returned.
func (cs *compiled) rank(items []*item, res *Result) []*item {
	byName := map[string]*familyRank{}
	var families []*familyRank
	for _, it := range items {
		f, ok := byName[it.c.Family()]
		if !ok {
			f = &familyRank{name: it.c.Family()}
			byName[f.name] = f
			families = append(families, f)
		}
		f.items = append(f.items, it)
	}
	var ranked []*familyRank
	for _, f := range families {
		var values []float64
		for _, it := range f.items {
			if v, ok := cs.objective(it); ok {
				values = append(values, v)
			}
		}
		if len(values) > 0 {
			f.value = median(values)
			ranked = append(ranked, f)
		} else {
			// No scored size: the family cannot be placed in the ranking.
			cs.dropFamily(f, math.Inf(1), res)
		}
	}
	lowerFirst := cs.lowerFirst()
	slices.SortFunc(ranked, func(a, b *familyRank) int {
		if c := cmp.Compare(a.value, b.value); c != 0 {
			if lowerFirst {
				return c
			}
			return -c
		}
		return strings.Compare(a.name, b.name)
	})
	if len(ranked) == 0 {
		return nil
	}

	best := ranked[0].value
	keep := len(ranked)
	boundary := 0.0
	if cs.ranking.band != nil {
		p := *cs.ranking.band / 100
		boundary = best * (1 - p)
		if lowerFirst {
			boundary = best * (1 + p)
		}
		keep = 0
		for _, f := range ranked {
			if !inBand(f.value, boundary, lowerFirst) {
				break
			}
			keep++
		}
	}
	band := &Band{Reference: ranked[0].name, ReferenceValue: best}
	if keep < cs.ranking.familiesMin {
		n := min(cs.ranking.familiesMin, len(ranked))
		band.Widened = n > keep
		keep = n
	}
	if cs.ranking.familiesMax > 0 && keep > cs.ranking.familiesMax {
		keep = cs.ranking.familiesMax
		band.Trimmed = true
	}
	lowest := ranked[keep-1]
	band.LowestKept = lowest.name
	band.LowestKeptDeltaPercent = relMiss(lowest.value, best) * 100
	if best == 0 {
		band.LowestKeptDeltaPercent = 0
	}
	res.Band = band

	// The threshold a dropped family misses: the band boundary, unless the
	// family count moved the cut-off to the lowest kept family.
	cutoff := boundary
	if cs.ranking.band == nil || band.Widened || band.Trimmed {
		cutoff = lowest.value
	}
	var selected []*item
	for i, f := range ranked {
		if i >= keep {
			cs.dropFamily(f, relMiss(f.value, cutoff), res)
			continue
		}
		v := f.value
		res.Families = append(res.Families, Family{Name: f.name, Rank: i + 1, Objective: &v})
		selected = append(selected, f.items...)
	}
	return selected
}

func inBand(v, boundary float64, lowerFirst bool) bool {
	if lowerFirst {
		return v <= boundary+tolerance*math.Abs(boundary)
	}
	return v >= boundary-tolerance*math.Abs(boundary)
}

func (cs *compiled) dropFamily(f *familyRank, miss float64, res *Result) {
	for _, it := range f.items {
		res.Excluded.Ranking++
		res.Misses[it.c.Name()] = Miss{Reason: ReasonRanking, Relative: miss}
	}
}

func median(values []float64) float64 {
	s := slices.Clone(values)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
