/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package selection

import (
	"math"

	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/SpareCores/sc-keeper-go/selection/api"
)

// keeperVCPUsLimit is the largest vcpus_min / vcpus_max Keeper accepts.
const keeperVCPUsLimit = 256

// PlanQueries returns the Keeper /servers queries of one evaluation: a base
// query and one query per benchmark used by the selector or the ranking.
//
// The base query carries no push-down filters, so it lists every type the
// vendor offers in the regions; churn control relies on that to tell a type
// that is no longer offered from one that no longer qualifies. The benchmark
// queries carry the push-down filters, loosened by the churn removal margin,
// so every type that might be kept by the margin still gets its scores.
// Benchmark thresholds are pushed down only with unbenchmarked: Exclude
// (Keeper drops rows without a score when one is set).
//
// With exactly one benchmark, the base query and the benchmark query are one
// query: the benchmark query without push-down, which lists every offered
// type as the base query does (Keeper outer-joins the scores) and carries
// the score. One request instead of two, and no mix of data versions.
func PlanQueries(spec *api.Spec, vendor string, regions []string, opts Options) ([]keeper.Query, error) {
	cs, err := compile(spec, opts)
	if err != nil {
		return nil, err
	}
	base := keeper.Query{Vendor: vendor, Regions: regions, Allocation: cs.allocation}
	out := []keeper.Query{base}

	m := math.Max(cs.marginPercent, 0) / 100
	common := cs.pushDown(m)
	var keys []keeper.BenchmarkKey
	byKey := map[keeper.BenchmarkKey]*keeper.Query{}
	need := func(k keeper.BenchmarkKey) *keeper.Query {
		if q, ok := byKey[k]; ok {
			return q
		}
		key := k
		q := base
		q.Benchmark = &key
		q.PushDown = common
		byKey[k] = &q
		keys = append(keys, k)
		return &q
	}
	for _, b := range cs.benches {
		q := need(b.key)
		if cs.policy == api.UnbenchmarkedExclude {
			pushBenchmark(&q.PushDown, b, m)
		}
	}
	if cs.ranking != nil {
		need(cs.ranking.key)
	}
	if len(keys) == 1 {
		key := keys[0]
		base.Benchmark = &key
		return []keeper.Query{base}, nil
	}
	for _, k := range keys {
		out = append(out, *byKey[k])
	}
	return out, nil
}

// loosen widens a lower (dir -1) or upper (dir +1) bound by the margin m,
// plus a relative epsilon so that rounding never makes Keeper stricter than
// the client. A lower bound that becomes <= 0 is dropped (nil).
func loosen(v float64, m float64, dir int) *float64 {
	const eps = 1e-9
	var out float64
	if dir < 0 {
		out = v * (1 - m - eps)
		if out <= 0 {
			return nil
		}
	} else {
		out = v * (1 + m + eps)
	}
	return &out
}

// floorInt rounds a loosened integer bound down, ignoring the epsilon of
// loosen (2 loosened by 0% stays 2).
func floorInt(v float64) int {
	return int(math.Floor(v + 1e-6))
}

// positive drops an upper bound of zero or less: Keeper rejects it (HTTP 422,
// vcpus_max >= 1, memory_max, memory_per_vcpu_max and price_max > 0). Such a
// bound admits nothing, which the client-side filter applies anyway.
func positive(v *float64) *float64 {
	if v == nil || *v <= 0 {
		return nil
	}
	return v
}

func loosenPtr(v *float64, m float64, dir int) *float64 {
	if v == nil {
		return nil
	}
	return loosen(*v, m, dir)
}

// pushDown translates the selector fields with an exact Keeper equivalent.
func (cs *compiled) pushDown(m float64) keeper.PushDown {
	s := &cs.sel
	var p keeper.PushDown
	if s.VCPUs != nil {
		if s.VCPUs.Min != nil {
			if lo := loosen(float64(*s.VCPUs.Min), m, -1); lo != nil && *lo >= 1 {
				if v := floorInt(*lo); v <= keeperVCPUsLimit {
					p.VCPUsMin = &v
				}
			}
		}
		if s.VCPUs.Max != nil {
			hi := int(math.Ceil(*loosen(float64(*s.VCPUs.Max), m, 1)))
			if hi >= 1 && hi <= keeperVCPUsLimit {
				p.VCPUsMax = &hi
			}
		}
	}
	for _, a := range s.Architectures {
		p.Architectures = append(p.Architectures, scArchitecture(a))
	}
	if mem := s.Memory; mem != nil {
		p.MemoryMin = loosenPtr(mem.MinGiB, m, -1)
		p.MemoryMax = positive(loosenPtr(mem.MaxGiB, m, 1))
		if mem.PerVCPU != nil {
			p.MemoryPerVCPUMin = loosenPtr(mem.PerVCPU.MinGiB, m, -1)
			p.MemoryPerVCPUMax = positive(loosenPtr(mem.PerVCPU.MaxGiB, m, 1))
		}
	}
	if cpu := s.CPU; cpu != nil {
		p.CPUSpeedMin = loosenPtr(cpu.ClockSpeedMinGHz, m, -1)
		p.CPUHyperthreading = cpu.Hyperthreading
		if k := cpu.Caches; k != nil {
			p.CPUL1dCacheMin = loosenPtr(k.L1dPerCoreMinKiB, m, -1)
			p.CPUL1iCacheMin = loosenPtr(k.L1iPerCoreMinKiB, m, -1)
			p.CPUL2CacheMin = loosenPtr(k.L2PerCoreMinKiB, m, -1)
			p.CPUL3CacheMin = loosenPtr(k.L3MinMiB, m, -1)
			p.CPUL3CacheTotalMin = loosenPtr(k.L3TotalMinMiB, m, -1)
		}
	}
	if n := s.Network; n != nil {
		p.NetworkSpeedBaselineMin = loosenPtr(n.BaselineMinGbps, m, -1)
		p.NetworkSpeedMaxMin = loosenPtr(n.PeakMinGbps, m, -1)
	}
	if s.Storage != nil && s.Storage.Local != nil {
		pushStorage(&p, s.Storage.Local, m)
	}
	if a := s.Accelerator; a != nil {
		if a.Count != nil {
			p.GPUMin = loosenPtr(a.Count.Min, m, -1)
			p.GPUMax = loosenPtr(a.Count.Max, m, 1)
		}
		if a.MemoryPerDevice != nil {
			p.GPUMemoryMin = loosenPtr(a.MemoryPerDevice.MinGiB, m, -1)
		}
		if a.MemoryTotal != nil {
			p.GPUMemoryTotal = loosenPtr(a.MemoryTotal.MinGiB, m, -1)
		}
	}
	// Keeper's price excludes the managed fee, so this is looser than the
	// client-side check whenever the fee is included.
	p.PriceMax = positive(loosenPtr(cs.maxHourly, m, 1))
	return p
}

// pushStorage: countMin 1 is sent as storage_size >= 1 GB, as Keeper counts
// per-disk entries, which four vendors lack.
func pushStorage(p *keeper.PushDown, l *api.LocalStorageSelector, m float64) {
	p.StorageSizeMin = loosenPtr(l.MinGB, m, -1)
	if l.CountMin == nil || *l.CountMin <= 0 {
		return
	}
	lo := loosen(float64(*l.CountMin), m, -1)
	if lo == nil {
		return
	}
	n := max(floorInt(*lo), 1)
	if n >= 2 {
		p.StorageCountMin = &n
		return
	}
	if p.StorageSizeMin == nil || *p.StorageSizeMin < 1 {
		one := 1.0
		p.StorageSizeMin = &one
	}
}

// pushBenchmark adds one benchmark filter's threshold to its query. Several
// filters on the same benchmark combine into the strictest bound, which
// still equals their conjunction.
func pushBenchmark(p *keeper.PushDown, b benchFilter, m float64) {
	tighterMin := func(cur, v *float64) *float64 {
		if v == nil || (cur != nil && *cur >= *v) {
			return cur
		}
		return v
	}
	lo := loosenPtr(b.min, m, -1)
	switch b.normalize {
	case api.NormalizePerVcpu:
		p.BenchmarkScorePerVCPUMin = tighterMin(p.BenchmarkScorePerVCPUMin, lo)
	case api.NormalizePerPrice:
		p.BenchmarkScorePerPriceMin = tighterMin(p.BenchmarkScorePerPriceMin, lo)
	default:
		p.BenchmarkScoreMin = tighterMin(p.BenchmarkScoreMin, lo)
		if hi := loosenPtr(b.max, m, 1); hi != nil && (p.BenchmarkScoreMax == nil || *hi < *p.BenchmarkScoreMax) {
			p.BenchmarkScoreMax = hi
		}
	}
}
