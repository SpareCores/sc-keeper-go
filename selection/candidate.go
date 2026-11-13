/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package selection turns a selector, a ranking and Keeper data into a set of
// instance types: candidates, selectors, the data-quality policy, ranking and
// churn control (subpackage churn). The semantics are specified in
// docs/reference/selectors.md of sc-karpenter-nodepool. It has no Kubernetes
// client; provider-specific inputs come in as plain values.
package selection

import (
	"slices"
	"strings"

	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/SpareCores/sc-keeper-go/selection/api"
)

// Candidate is one instance type of a vendor, joined over every Keeper query
// of one evaluation on api_reference.
type Candidate struct {
	// Server is the row of the base query (or of the first query that
	// returned the type). Its price and benchmark score fields are those of
	// that query only; use Price and Scores.
	Server keeper.Server

	// Prices are the cheapest hourly USD prices in the target regions, per
	// allocation.
	Prices map[keeper.Allocation]float64
	// Scores are the benchmark scores the queries returned (absent: no score).
	Scores map[keeper.BenchmarkKey]float64
	// KeeperFiltered marks benchmarks whose query pushed a threshold down and
	// did not return this type: it either has no score or misses the
	// (margin-loosened) threshold.
	KeeperFiltered map[keeper.BenchmarkKey]bool

	// Architecture is the Kubernetes architecture; empty when Spare Cores'
	// architecture has no Kubernetes equivalent (i386, *_mac).
	Architecture api.Architecture
	// AcceleratorType is the derived type of the primary accelerator (GPU,
	// TPU), empty when unknown or when there is no accelerator.
	AcceleratorType api.AcceleratorType
}

// Name is the instance type (api_reference).
func (c *Candidate) Name() string { return c.Server.APIReference }

// Family is the Spare Cores family.
func (c *Candidate) Family() string { return c.Server.Family }

// VCPUs is the number of vCPUs.
func (c *Candidate) VCPUs() int { return c.Server.VCPUs }

// MemoryGiB is the memory in GiB.
func (c *Candidate) MemoryGiB() float64 { return c.Server.MemoryAmount / 1024 }

// MemoryPerVCPU is the memory in GiB per vCPU (0 without vCPUs).
func (c *Candidate) MemoryPerVCPU() float64 {
	if c.Server.VCPUs <= 0 {
		return 0
	}
	return c.MemoryGiB() / float64(c.Server.VCPUs)
}

// Hyperthreading reports whether there are more vCPUs than physical cores;
// ok is false when the core count is unknown.
func (c *Candidate) Hyperthreading() (on, ok bool) {
	if c.Server.CPUCores == nil || *c.Server.CPUCores <= 0 {
		return false, false
	}
	return c.Server.VCPUs > *c.Server.CPUCores, true
}

// Price returns the hourly USD price on the basis plus the managed fee
// (fee 0 when not applicable); ok is false without a positive price.
func (c *Candidate) Price(basis keeper.Allocation, fee float64) (float64, bool) {
	p, ok := c.Prices[basis]
	if !ok || p <= 0 {
		return 0, false
	}
	return p + fee, true
}

// k8sArchitecture maps Spare Cores' cpu_architecture.
func k8sArchitecture(sc string) api.Architecture {
	switch sc {
	case "x86_64":
		return api.ArchitectureAMD64
	case "arm64":
		return api.ArchitectureARM64
	}
	return ""
}

// scArchitecture is the inverse of k8sArchitecture.
func scArchitecture(a api.Architecture) string {
	if a == api.ArchitectureAMD64 {
		return "x86_64"
	}
	return string(a)
}

// deriveAcceleratorType implements the derivation rule of selectors.md
// ("Accelerators"): accelerator_type if set; TPU when the accelerator family
// is TPU; GPU when the manufacturer is NVIDIA or AMD; otherwise unknown.
func deriveAcceleratorType(s *keeper.Server) api.AcceleratorType {
	if s.AcceleratorCount <= 0 {
		return ""
	}
	if s.AcceleratorType != nil && *s.AcceleratorType != "" {
		return api.AcceleratorType(strings.ToUpper(*s.AcceleratorType))
	}
	if s.AcceleratorFamily != nil && strings.EqualFold(*s.AcceleratorFamily, "TPU") {
		return api.AcceleratorTypeTPU
	}
	if s.AcceleratorManufacturer != nil {
		switch *s.AcceleratorManufacturer {
		case "NVIDIA", "AMD":
			return api.AcceleratorTypeGPU
		}
	}
	return ""
}

// newCandidate is a candidate of a server row with no prices and scores yet.
func newCandidate(s *keeper.Server) *Candidate {
	return &Candidate{
		Server:          *s,
		Prices:          map[keeper.Allocation]float64{},
		Scores:          map[keeper.BenchmarkKey]float64{},
		KeeperFiltered:  map[keeper.BenchmarkKey]bool{},
		Architecture:    k8sArchitecture(s.CPUArchitecture),
		AcceleratorType: deriveAcceleratorType(s),
	}
}

// BuildCandidates joins query results on api_reference. When results contain
// a query without a benchmark (the base query), only its rows are
// candidates and the other queries contribute scores; otherwise every row of
// every query is. The result is sorted by name.
func BuildCandidates(results []keeper.QueryResult) []*Candidate {
	hasBase := slices.ContainsFunc(results, func(r keeper.QueryResult) bool { return r.Query.Benchmark == nil })
	byName := map[string]*Candidate{}
	add := func(s *keeper.Server) *Candidate {
		c := newCandidate(s)
		byName[s.APIReference] = c
		return c
	}
	// Base rows first, so Candidate.Server is the base row.
	for _, base := range []bool{true, false} {
		for _, r := range results {
			if (r.Query.Benchmark == nil) != base {
				continue
			}
			for i := range r.Servers {
				s := &r.Servers[i]
				c, ok := byName[s.APIReference]
				if !ok {
					if hasBase && !base {
						continue
					}
					c = add(s)
				}
				if s.Price != nil {
					if old, seen := c.Prices[r.Query.Allocation]; !seen || *s.Price < old {
						c.Prices[r.Query.Allocation] = *s.Price
					}
				}
				if r.Query.Benchmark != nil && s.SelectedBenchmarkScore != nil {
					c.Scores[*r.Query.Benchmark] = *s.SelectedBenchmarkScore
				}
			}
		}
	}
	for _, r := range results {
		if r.Query.Benchmark == nil || !r.Query.PushDown.HasBenchmarkThreshold() {
			continue
		}
		returned := map[string]bool{}
		for i := range r.Servers {
			returned[r.Servers[i].APIReference] = true
		}
		for name, c := range byName {
			if !returned[name] {
				c.KeeperFiltered[*r.Query.Benchmark] = true
			}
		}
	}
	out := make([]*Candidate, 0, len(byName))
	for _, c := range byName {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b *Candidate) int { return strings.Compare(a.Name(), b.Name()) })
	return out
}
