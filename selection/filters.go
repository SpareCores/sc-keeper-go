/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package selection

import (
	"math"
	"path"
	"slices"

	"github.com/SpareCores/sc-keeper-go/selection/api"
)

// verdict is the outcome of one filter on one candidate.
type verdict int

const (
	pass verdict = iota
	fail
	missing
)

// outcome is a verdict plus, for fail, the relative miss of the failing
// threshold (+Inf for non-numeric filters).
type outcome struct {
	v    verdict
	miss float64
}

var (
	passed    = outcome{v: pass}
	failed    = outcome{v: fail, miss: math.Inf(1)}
	noData    = outcome{v: missing, miss: math.Inf(1)}
	tolerance = 1e-9
)

// atLeast compares value >= minimum, with a relative tolerance for the
// rounding of derived values (GiB from MiB, prices plus fees).
func atLeast(value, minimum float64) outcome {
	if value >= minimum-tolerance*math.Abs(minimum) {
		return passed
	}
	return outcome{v: fail, miss: relMiss(value, minimum)}
}

// atMost compares value <= maximum.
func atMost(value, maximum float64) outcome {
	if value <= maximum+tolerance*math.Max(math.Abs(maximum), 1) {
		return passed
	}
	return outcome{v: fail, miss: relMiss(value, maximum)}
}

// relMiss is the distance of value from threshold relative to the threshold.
func relMiss(value, threshold float64) float64 {
	if threshold == 0 {
		return math.Inf(1)
	}
	return math.Abs(value-threshold) / math.Abs(threshold)
}

// worse combines two outcomes of one filter (several bounds): the first
// non-pass verdict wins, and the miss is the larger.
func worse(a, b outcome) outcome {
	switch {
	case a.v == pass:
		return b
	case b.v == pass:
		return a
	case a.v == missing || b.v == missing:
		return noData
	}
	return outcome{v: fail, miss: math.Max(a.miss, b.miss)}
}

func boolOutcome(ok bool) outcome {
	if ok {
		return passed
	}
	return failed
}

func floatBounds(value float64, lo, hi *float64) outcome {
	o := passed
	if lo != nil {
		o = worse(o, atLeast(value, *lo))
	}
	if hi != nil {
		o = worse(o, atMost(value, *hi))
	}
	return o
}

func optMin(value, minimum *float64, scale float64) outcome {
	if minimum == nil {
		return passed
	}
	if value == nil {
		return noData
	}
	return atLeast(*value*scale, *minimum)
}

// evalCtx carries per-evaluation inputs of the filters.
type evalCtx struct {
	policy api.UnbenchmarkedPolicy
}

// filter is one selector field.
type filter struct {
	name string
	eval func(c *Candidate, price float64, ctx *evalCtx) outcome
}

// globMatch implements GlobFilter: kept if it matches any include (or include
// is empty) and no exclude.
func globMatch(g *api.GlobFilter, name string) bool {
	match := func(patterns []string) bool {
		return slices.ContainsFunc(patterns, func(p string) bool {
			ok, _ := path.Match(p, name)
			return ok
		})
	}
	return (len(g.Include) == 0 || match(g.Include)) && !match(g.Exclude)
}

// filters returns the set selector fields as filters, in a fixed order (the
// order decides which counter a candidate failing several filters lands in).
func (c *compiled) filters() []filter {
	s := &c.sel
	var out []filter
	add := func(name string, set bool, eval func(*Candidate, float64, *evalCtx) outcome) {
		if set {
			out = append(out, filter{name: name, eval: eval})
		}
	}
	add("instanceTypes", s.InstanceTypes != nil, func(cd *Candidate, _ float64, _ *evalCtx) outcome {
		return boolOutcome(globMatch(s.InstanceTypes, cd.Name()))
	})
	add("families", s.Families != nil, func(cd *Candidate, _ float64, _ *evalCtx) outcome {
		return boolOutcome(globMatch(s.Families, cd.Family()))
	})
	add("vcpus", s.VCPUs != nil, func(cd *Candidate, _ float64, _ *evalCtx) outcome {
		return floatBounds(float64(cd.VCPUs()), int32Ptr(s.VCPUs.Min), int32Ptr(s.VCPUs.Max))
	})
	add("architectures", len(s.Architectures) > 0, func(cd *Candidate, _ float64, _ *evalCtx) outcome {
		return boolOutcome(slices.Contains(s.Architectures, cd.Architecture))
	})
	if s.CPU != nil {
		out = append(out, c.cpuFilters()...)
	}
	if m := s.Memory; m != nil {
		add("memory", m.MinGiB != nil || m.MaxGiB != nil, func(cd *Candidate, _ float64, _ *evalCtx) outcome {
			return floatBounds(cd.MemoryGiB(), m.MinGiB, m.MaxGiB)
		})
		add("memory.perVcpu", m.PerVCPU != nil, func(cd *Candidate, _ float64, _ *evalCtx) outcome {
			return floatBounds(cd.MemoryPerVCPU(), m.PerVCPU.MinGiB, m.PerVCPU.MaxGiB)
		})
	}
	if s.Accelerator != nil {
		out = append(out, c.acceleratorFilters()...)
	}
	if n := s.Network; n != nil {
		add("network", n.BaselineMinGbps != nil || n.PeakMinGbps != nil, func(cd *Candidate, _ float64, _ *evalCtx) outcome {
			return worse(optMin(cd.Server.NetworkSpeedBaseline, n.BaselineMinGbps, 1),
				optMin(cd.Server.NetworkSpeedMax, n.PeakMinGbps, 1))
		})
	}
	if s.Storage != nil && s.Storage.Local != nil {
		l := s.Storage.Local
		add("storage.local", l.MinGB != nil || l.CountMin != nil, func(cd *Candidate, _ float64, _ *evalCtx) outcome {
			return localStorage(cd, l)
		})
	}
	add("price.maxHourly", c.maxHourly != nil, func(_ *Candidate, price float64, _ *evalCtx) outcome {
		return atMost(price, *c.maxHourly)
	})
	add("price.maxHourlyPerVcpu", c.maxHourlyPerVCPU != nil, func(cd *Candidate, price float64, _ *evalCtx) outcome {
		if cd.VCPUs() <= 0 {
			return noData
		}
		return atMost(price/float64(cd.VCPUs()), *c.maxHourlyPerVCPU)
	})
	return out
}

func (c *compiled) cpuFilters() []filter {
	cpu := c.sel.CPU
	var out []filter
	if len(cpu.Manufacturers) > 0 {
		out = append(out, filter{"cpu.manufacturers", func(cd *Candidate, _ float64, _ *evalCtx) outcome {
			if cd.Server.CPUManufacturer == nil || *cd.Server.CPUManufacturer == "" {
				return noData
			}
			return boolOutcome(slices.Contains(cpu.Manufacturers, *cd.Server.CPUManufacturer))
		}})
	}
	if len(cpu.Allocations) > 0 {
		out = append(out, filter{"cpu.allocations", func(cd *Candidate, _ float64, _ *evalCtx) outcome {
			return boolOutcome(slices.Contains(cpu.Allocations, api.CPUAllocation(cd.Server.CPUAllocation)))
		}})
	}
	if cpu.ClockSpeedMinGHz != nil {
		out = append(out, filter{"cpu.clockSpeedMinGHz", func(cd *Candidate, _ float64, _ *evalCtx) outcome {
			return optMin(cd.Server.CPUSpeed, cpu.ClockSpeedMinGHz, 1)
		}})
	}
	if cpu.Hyperthreading != nil {
		out = append(out, filter{"cpu.hyperthreading", func(cd *Candidate, _ float64, _ *evalCtx) outcome {
			on, ok := cd.Hyperthreading()
			if !ok {
				return noData
			}
			return boolOutcome(on == *cpu.Hyperthreading)
		}})
	}
	if f := cpu.Flags; f != nil && len(f.AllOf)+len(f.AnyOf)+len(f.NoneOf) > 0 {
		out = append(out, filter{"cpu.flags", func(cd *Candidate, _ float64, ctx *evalCtx) outcome {
			return cpuFlags(f, cd.Server.CPUFlags, ctx.policy)
		}})
	}
	if k := cpu.Caches; k != nil {
		out = append(out, filter{"cpu.caches", func(cd *Candidate, _ float64, _ *evalCtx) outcome {
			s := &cd.Server
			o := optMin(s.CPUL1dCache, k.L1dPerCoreMinKiB, 1)
			o = worse(o, optMin(s.CPUL1iCache, k.L1iPerCoreMinKiB, 1))
			o = worse(o, optMin(s.CPUL2Cache, k.L2PerCoreMinKiB, 1))
			o = worse(o, optMin(s.CPUL3Cache, k.L3MinMiB, 1.0/1024))
			return worse(o, optMin(s.CPUL3CacheTotal, k.L3TotalMinMiB, 1.0/1024))
		}})
	}
	return out
}

// cpuFlags: an empty flag list means the SC inspector has not measured the
// type. It fails allOf/anyOf as missing data and passes a noneOf-only filter
// only with unbenchmarked: Include (selectors.md, "CPU features").
func cpuFlags(f *api.CPUFlags, flags []string, policy api.UnbenchmarkedPolicy) outcome {
	if len(flags) == 0 {
		if len(f.AllOf) == 0 && len(f.AnyOf) == 0 && policy == api.UnbenchmarkedInclude {
			return passed
		}
		return noData
	}
	for _, want := range f.AllOf {
		if !slices.Contains(flags, want) {
			return failed
		}
	}
	if len(f.AnyOf) > 0 && !slices.ContainsFunc(f.AnyOf, func(x string) bool { return slices.Contains(flags, x) }) {
		return failed
	}
	if slices.ContainsFunc(f.NoneOf, func(x string) bool { return slices.Contains(flags, x) }) {
		return failed
	}
	return passed
}

func (c *compiled) acceleratorFilters() []filter {
	a := c.sel.Accelerator
	var out []filter
	if a.Count != nil {
		out = append(out, filter{"accelerator.count", func(cd *Candidate, _ float64, _ *evalCtx) outcome {
			return floatBounds(cd.Server.AcceleratorCount, a.Count.Min, a.Count.Max)
		}})
	}
	if len(a.Types) > 0 {
		out = append(out, filter{"accelerator.types", func(cd *Candidate, _ float64, _ *evalCtx) outcome {
			if cd.Server.AcceleratorCount <= 0 {
				return failed
			}
			if cd.AcceleratorType == "" {
				return noData
			}
			return boolOutcome(slices.Contains(a.Types, cd.AcceleratorType))
		}})
	}
	if a.MemoryPerDevice != nil && a.MemoryPerDevice.MinGiB != nil {
		out = append(out, filter{"accelerator.memoryPerDevice", func(cd *Candidate, _ float64, _ *evalCtx) outcome {
			return acceleratorMemory(cd, cd.Server.AcceleratorMemoryMin, *a.MemoryPerDevice.MinGiB)
		}})
	}
	if a.MemoryTotal != nil && a.MemoryTotal.MinGiB != nil {
		out = append(out, filter{"accelerator.memoryTotal", func(cd *Candidate, _ float64, _ *evalCtx) outcome {
			return acceleratorMemory(cd, cd.Server.AcceleratorMemoryTotal, *a.MemoryTotal.MinGiB)
		}})
	}
	if len(a.Manufacturers) > 0 {
		out = append(out, filter{"accelerator.manufacturers", func(cd *Candidate, _ float64, _ *evalCtx) outcome {
			return acceleratorString(cd, cd.Server.AcceleratorManufacturer, a.Manufacturers)
		}})
	}
	if len(a.Models) > 0 {
		out = append(out, filter{"accelerator.models", func(cd *Candidate, _ float64, _ *evalCtx) outcome {
			return acceleratorString(cd, cd.Server.AcceleratorModel, a.Models)
		}})
	}
	return out
}

// acceleratorMemory: types without accelerators have 0 memory (a plain
// miss); accelerated types without a value lack data.
func acceleratorMemory(cd *Candidate, mib *float64, minGiB float64) outcome {
	if cd.Server.AcceleratorCount <= 0 {
		return atLeast(0, minGiB)
	}
	if mib == nil || *mib <= 0 {
		return noData
	}
	return atLeast(*mib/1024, minGiB)
}

func acceleratorString(cd *Candidate, value *string, allowed []string) outcome {
	if cd.Server.AcceleratorCount <= 0 {
		return failed
	}
	if value == nil || *value == "" {
		return noData
	}
	return boolOutcome(slices.Contains(allowed, *value))
}

// localStorage: countMin 1 means "has local storage" (storage_size > 0) on
// every vendor; 2 or more needs per-disk entries, which four vendors lack.
func localStorage(cd *Candidate, l *api.LocalStorageSelector) outcome {
	s := &cd.Server
	o := passed
	if l.MinGB != nil {
		o = atLeast(s.StorageSize, *l.MinGB)
	}
	if l.CountMin == nil || *l.CountMin <= 0 {
		return o
	}
	want := float64(*l.CountMin)
	switch {
	case s.StorageSize <= 0:
		return worse(o, atLeast(0, want))
	case want <= 1:
		return o
	case len(s.Storages) == 0:
		return worse(o, noData)
	}
	return worse(o, atLeast(float64(len(s.Storages)), want))
}

func int32Ptr(p *int32) *float64 {
	if p == nil {
		return nil
	}
	v := float64(*p)
	return &v
}
