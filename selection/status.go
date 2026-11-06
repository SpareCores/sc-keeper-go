/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package selection

import (
	"math"

	"github.com/SpareCores/sc-keeper-go/selection/api"
)

// Status converts the counters for status.excluded.
func (e Excluded) Status() *api.ExcludedStatus {
	out := &api.ExcludedStatus{
		ProviderIneligible:  int32(e.ProviderIneligible),
		TemplateConstraints: int32(e.TemplateConstraints),
		MissingData:         int32(e.MissingData),
		Ranking:             int32(e.Ranking),
	}
	if len(e.Filters) > 0 {
		out.Filters = make(map[string]int32, len(e.Filters))
		for k, v := range e.Filters {
			out.Filters[k] = int32(v)
		}
	}
	return out
}

// Status converts the band report for status.selected.band (values rounded
// to 4 decimals, as they are only informative).
func (b *Band) Status() *api.BandStatus {
	if b == nil {
		return nil
	}
	ref, delta := round4(b.ReferenceValue), round4(b.LowestKeptDeltaPercent)
	return &api.BandStatus{
		Reference:              b.Reference,
		ReferenceValue:         &ref,
		LowestKept:             b.LowestKept,
		LowestKeptDeltaPercent: &delta,
		Widened:                b.Widened,
		Trimmed:                b.Trimmed,
	}
}

func round4(v float64) float64 {
	return math.Round(v*1e4) / 1e4
}
