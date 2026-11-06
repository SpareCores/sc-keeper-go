/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package selection

import (
	"testing"

	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/SpareCores/sc-keeper-go/selection/api"
)

// One case per selector field: types that must pass (in), types that must
// fail this filter (out), and a predicate for rows lacking the data (each
// such row must be excluded as missing data, and at least one must exist).
// Instance types are long-lived ones, so snapshot refreshes keep the cases
// valid.
type selectorCase struct {
	name    string
	vendor  string
	sel     api.Selector
	field   string
	in      []string
	out     []string
	missing func(*keeper.Server) bool
}

func selectorCases() []selectorCase {
	return []selectorCase{
		{name: "instanceTypes globs", vendor: "aws", field: "instanceTypes",
			sel: api.Selector{InstanceTypes: &api.GlobFilter{Include: []string{"m7i.*", "c7?.large"}, Exclude: []string{"*.metal*", "*.48xlarge"}}},
			in:  []string{"m7i.large", "c7a.large"}, out: []string{"m7i.48xlarge", "m7i.metal-24xl", "m8g.large", "c7a.xlarge"}},
		{name: "families exclude", vendor: "azure", field: "families",
			sel: api.Selector{Families: &api.GlobFilter{Exclude: []string{"DSv5", "[BE]*"}}},
			in:  []string{"Standard_D2as_v5"}, out: []string{"Standard_D2s_v5", "Standard_B2s", "Standard_E2s_v5"}},
		{name: "vcpus inclusive", vendor: "gcp", field: "vcpus",
			sel: api.Selector{VCPUs: &api.IntRange{Min: new(int32(2)), Max: new(int32(4))}},
			in:  []string{"n2-standard-2", "c3-standard-4"}, out: []string{"t2a-standard-1", "g2-standard-8"}},
		{name: "architectures", vendor: "hcloud", field: "architectures",
			sel: api.Selector{Architectures: []api.Architecture{api.ArchitectureARM64}},
			in:  []string{"cax11", "cax41"}, out: []string{"ccx13", "cpx22"}},
		{name: "cpu.manufacturers", vendor: "azure", field: "cpu.manufacturers",
			sel: api.Selector{CPU: &api.CPUSelector{Manufacturers: []string{"AMD"}}},
			in:  []string{"Standard_D2as_v5"}, out: []string{"Standard_D2s_v5", "Standard_D2ps_v5"},
			missing: func(s *keeper.Server) bool { return s.CPUManufacturer == nil }},
		{name: "cpu.allocations", vendor: "aws", field: "cpu.allocations",
			sel: api.Selector{CPU: &api.CPUSelector{Allocations: []api.CPUAllocation{api.CPUAllocationBurstable}}},
			in:  []string{"t3.micro"}, out: []string{"m7i.large"}},
		{name: "cpu.clockSpeedMinGHz", vendor: "gcp", field: "cpu.clockSpeedMinGHz",
			sel: api.Selector{CPU: &api.CPUSelector{ClockSpeedMinGHz: new(2.5)}},
			in:  []string{"n2-standard-2"}, out: []string{"e2-standard-2"},
			missing: func(s *keeper.Server) bool { return s.CPUSpeed == nil }},
		{name: "cpu.hyperthreading on", vendor: "aws", field: "cpu.hyperthreading",
			sel: api.Selector{CPU: &api.CPUSelector{Hyperthreading: new(true)}},
			in:  []string{"m7i.large", "i4i.large"}, out: []string{"m8g.large", "c7a.large"}},
		{name: "cpu.hyperthreading off", vendor: "gcp", field: "cpu.hyperthreading",
			sel: api.Selector{CPU: &api.CPUSelector{Hyperthreading: new(false)}},
			in:  []string{"t2a-standard-1"}, out: []string{"n2-standard-2"},
			missing: func(s *keeper.Server) bool { return s.CPUCores == nil }},
		{name: "cpu.flags allOf", vendor: "aws", field: "cpu.flags",
			sel: api.Selector{CPU: &api.CPUSelector{Flags: &api.CPUFlags{AllOf: []string{"avx512f", "amx_tile"}}}},
			in:  []string{"m7i.large"}, out: []string{"c7a.large", "m8g.large"},
			missing: func(s *keeper.Server) bool { return len(s.CPUFlags) == 0 }},
		{name: "cpu.flags anyOf", vendor: "aws", field: "cpu.flags",
			sel: api.Selector{CPU: &api.CPUSelector{Flags: &api.CPUFlags{AnyOf: []string{"sve", "amx_tile"}}}},
			in:  []string{"m7i.large", "m8g.large"}, out: []string{"c7a.large"},
			missing: func(s *keeper.Server) bool { return len(s.CPUFlags) == 0 }},
		{name: "cpu.flags noneOf", vendor: "azure", field: "cpu.flags",
			sel: api.Selector{CPU: &api.CPUSelector{Flags: &api.CPUFlags{NoneOf: []string{"avx512f"}}}},
			in:  []string{"Standard_D2ps_v5"}, out: []string{"Standard_D2s_v5"},
			missing: func(s *keeper.Server) bool { return len(s.CPUFlags) == 0 }},
		{name: "cpu.caches L2 per core", vendor: "hcloud", field: "cpu.caches",
			sel: api.Selector{CPU: &api.CPUSelector{Caches: &api.CPUCaches{L2PerCoreMinKiB: new(1024.0)}}},
			in:  []string{"cpx22"}, out: []string{"ccx13"},
			missing: func(s *keeper.Server) bool { return s.CPUL2Cache == nil }},
		{name: "cpu.caches L1 and L3", vendor: "aws", field: "cpu.caches",
			sel: api.Selector{CPU: &api.CPUSelector{Caches: &api.CPUCaches{
				L1dPerCoreMinKiB: new(48.0), L1iPerCoreMinKiB: new(32.0), L3MinMiB: new(64.0), L3TotalMinMiB: new(100.0)}}},
			in: []string{"m7i.large"}, out: []string{"m8g.large", "c7a.large"},
			missing: func(s *keeper.Server) bool { return s.CPUL3Cache == nil }},
		{name: "memory GiB (Hetzner GB)", vendor: "hcloud", field: "memory",
			sel: api.Selector{Memory: &api.MemorySelector{MinGiB: new(3.8), MaxGiB: new(8.0)}},
			in:  []string{"cax11", "cax21", "cpx22"}, out: []string{"cpx12", "cax31"}},
		{name: "memory per vCPU", vendor: "aws", field: "memory.perVcpu",
			sel: api.Selector{Memory: &api.MemorySelector{PerVCPU: &api.GiBRange{MinGiB: new(4.0), MaxGiB: new(4.0)}}},
			in:  []string{"m7i.large", "m8g.large"}, out: []string{"c7a.large", "i4i.large"}},
		{name: "accelerator.count none", vendor: "aws", field: "accelerator.count",
			sel: api.Selector{Accelerator: &api.AcceleratorSelector{Count: &api.FloatRange{Max: new(0.0)}}},
			in:  []string{"m7i.large"}, out: []string{"g6.xlarge"}},
		{name: "accelerator.types TPU (derived)", vendor: "gcp", field: "accelerator.types",
			sel: api.Selector{Accelerator: &api.AcceleratorSelector{Types: []api.AcceleratorType{api.AcceleratorTypeTPU}}},
			in:  []string{"ct5l-hightpu-1t"}, out: []string{"g2-standard-4", "n2-standard-2"}},
		{name: "accelerator.types GPU (derived)", vendor: "gcp", field: "accelerator.types",
			sel: api.Selector{Accelerator: &api.AcceleratorSelector{Types: []api.AcceleratorType{api.AcceleratorTypeGPU}}},
			in:  []string{"g2-standard-4"}, out: []string{"ct5l-hightpu-1t"}},
		{name: "accelerator.memoryPerDevice", vendor: "azure", field: "accelerator.memoryPerDevice",
			sel: api.Selector{Accelerator: &api.AcceleratorSelector{MemoryPerDevice: &api.GiBMin{MinGiB: new(20.0)}}},
			in:  []string{"Standard_NC16ads_A10_v4", "Standard_NC24ads_A100_v4"}, out: []string{"Standard_D2s_v5", "Standard_NV6ads_A10_v5"},
			missing: func(s *keeper.Server) bool {
				return s.AcceleratorCount > 0 && (s.AcceleratorMemoryMin == nil || *s.AcceleratorMemoryMin == 0)
			}},
		{name: "accelerator.memoryTotal", vendor: "azure", field: "accelerator.memoryTotal",
			sel: api.Selector{Accelerator: &api.AcceleratorSelector{MemoryTotal: &api.GiBMin{MinGiB: new(40.0)}}},
			in:  []string{"Standard_NV72ads_A10_v5", "Standard_NC24ads_A100_v4"}, out: []string{"Standard_NC16ads_A10_v4", "Standard_D2s_v5"},
			missing: func(s *keeper.Server) bool {
				return s.AcceleratorCount > 0 && (s.AcceleratorMemoryTotal == nil || *s.AcceleratorMemoryTotal == 0)
			}},
		{name: "accelerator.manufacturers", vendor: "azure", field: "accelerator.manufacturers",
			sel: api.Selector{Accelerator: &api.AcceleratorSelector{Manufacturers: []string{"NVIDIA"}}},
			in:  []string{"Standard_NC24ads_A100_v4"}, out: []string{"Standard_D2s_v5"},
			missing: func(s *keeper.Server) bool { return s.AcceleratorCount > 0 && s.AcceleratorManufacturer == nil }},
		{name: "accelerator.models", vendor: "gcp", field: "accelerator.models",
			sel: api.Selector{Accelerator: &api.AcceleratorSelector{Models: []string{"L4"}}},
			in:  []string{"g2-standard-4"}, out: []string{"ct5l-hightpu-1t", "n2-standard-2"}},
		{name: "accelerator.models missing", vendor: "azure", field: "accelerator.models",
			sel: api.Selector{Accelerator: &api.AcceleratorSelector{Models: []string{"A100"}}},
			in:  []string{"Standard_NC24ads_A100_v4"}, out: []string{"Standard_NC16ads_A10_v4"},
			missing: func(s *keeper.Server) bool { return s.AcceleratorCount > 0 && s.AcceleratorModel == nil }},
		{name: "network baseline", vendor: "aws", field: "network",
			sel: api.Selector{Network: &api.NetworkSelector{BaselineMinGbps: new(10.0)}},
			in:  []string{"i4i.32xlarge"}, out: []string{"m7i.large"}},
		{name: "network peak inclusive", vendor: "aws", field: "network",
			sel: api.Selector{Network: &api.NetworkSelector{PeakMinGbps: new(12.5)}},
			in:  []string{"m7i.large"}, out: []string{"t3.micro"}},
		{name: "network missing everywhere", vendor: "gcp", field: "network",
			sel:     api.Selector{Network: &api.NetworkSelector{BaselineMinGbps: new(1.0)}},
			missing: func(s *keeper.Server) bool { return s.NetworkSpeedBaseline == nil }},
		{name: "storage.local minGB", vendor: "aws", field: "storage.local",
			sel: api.Selector{Storage: &api.StorageSelector{Local: &api.LocalStorageSelector{MinGB: new(200.0)}}},
			in:  []string{"i4i.large", "i4i.32xlarge"}, out: []string{"m7i.large"}},
		{name: "storage.local countMin 2", vendor: "aws", field: "storage.local",
			sel: api.Selector{Storage: &api.StorageSelector{Local: &api.LocalStorageSelector{CountMin: new(int32(2))}}},
			in:  []string{"i4i.32xlarge"}, out: []string{"i4i.large", "m7i.large"}},
		{name: "storage.local countMin 1 without per-disk data", vendor: "hcloud", field: "storage.local",
			sel: api.Selector{Storage: &api.StorageSelector{Local: &api.LocalStorageSelector{CountMin: new(int32(1))}}},
			in:  []string{"cax11", "ccx13"}},
		{name: "storage.local countMin 2 without per-disk data", vendor: "hcloud", field: "storage.local",
			sel:     api.Selector{Storage: &api.StorageSelector{Local: &api.LocalStorageSelector{CountMin: new(int32(2))}}},
			missing: func(s *keeper.Server) bool { return s.StorageSize > 0 && len(s.Storages) == 0 }},
		{name: "price.maxHourly inclusive", vendor: "aws", field: "price.maxHourly",
			sel: api.Selector{Price: &api.PriceSelector{MaxHourly: "0.1008"}},
			in:  []string{"m7i.large", "t3.micro"}, out: []string{"i4i.large"}},
		{name: "price.maxHourlyPerVcpu", vendor: "hcloud", field: "price.maxHourlyPerVcpu",
			sel: api.Selector{Price: &api.PriceSelector{MaxHourlyPerVCPU: "0.02"}},
			in:  []string{"cax11", "cpx22"}, out: []string{"ccx13"}},
	}
}

func TestSelectors(t *testing.T) {
	for _, tc := range selectorCases() {
		t.Run(tc.name, func(t *testing.T) {
			sel := tc.sel
			res, cands := run(t, tc.vendor, specOf(&sel))
			all := byName(cands)
			selected := selectedSet(res)
			for _, n := range append(append([]string{}, tc.in...), tc.out...) {
				if all[n] == nil {
					t.Fatalf("snapshot lacks %s/%s", tc.vendor, n)
				}
			}
			for _, n := range tc.in {
				if _, ok := selected[n]; !ok {
					t.Errorf("%s: want selected, excluded as %+v", n, res.Misses[n])
				}
			}
			for _, n := range tc.out {
				if got := res.Misses[n].Reason; got != ReasonFilterPrefix+tc.field {
					t.Errorf("%s: want excluded by %s, got %q", n, tc.field, got)
				}
			}
			if tc.missing != nil {
				n := 0
				for _, c := range cands {
					if c.Architecture == "" || !tc.missing(&c.Server) {
						continue
					}
					n++
					if got := res.Misses[c.Name()].Reason; got != ReasonMissingData {
						t.Errorf("%s lacks data: want missingData, got %q", c.Name(), got)
					}
				}
				if n == 0 {
					t.Errorf("no %s row lacks the data; pick another vendor", tc.vendor)
				}
				if res.Excluded.MissingData < n {
					t.Errorf("missingData = %d, want >= %d", res.Excluded.MissingData, n)
				}
			}
			if len(tc.out) > 0 && res.Excluded.Filters[tc.field] < len(tc.out) {
				t.Errorf("filters.%s = %d, want >= %d", tc.field, res.Excluded.Filters[tc.field], len(tc.out))
			}
		})
	}
}

// Every selector at once still works and AND-s the filters (spot basis on
// the aws snapshot).
func TestSelectorsCombined(t *testing.T) {
	sel := &api.Selector{
		InstanceTypes: &api.GlobFilter{Exclude: []string{"*.metal*"}},
		Families:      &api.GlobFilter{Include: []string{"m*", "c*", "r*"}},
		VCPUs:         &api.IntRange{Min: new(int32(2)), Max: new(int32(16))},
		Architectures: []api.Architecture{api.ArchitectureAMD64, api.ArchitectureARM64},
		CPU: &api.CPUSelector{
			Manufacturers: []string{"AMD", "AWS", "Intel"},
			Allocations:   []api.CPUAllocation{api.CPUAllocationDedicated},
			Flags:         &api.CPUFlags{NoneOf: []string{"amx_tile"}},
		},
		Memory:      &api.MemorySelector{MinGiB: new(4.0), MaxGiB: new(64.0), PerVCPU: &api.GiBRange{MinGiB: new(2.0), MaxGiB: new(8.0)}},
		Accelerator: &api.AcceleratorSelector{Count: &api.FloatRange{Min: new(0.0), Max: new(0.0)}},
		Price:       &api.PriceSelector{Basis: api.PriceBasisSpot, MaxHourly: "0.40", MaxHourlyPerVCPU: "0.05"},
	}
	res, cands := run(t, "aws", specOf(sel))
	if res.Basis != api.PriceBasisSpot || len(res.InstanceTypes) == 0 {
		t.Fatalf("basis %s, %d types", res.Basis, len(res.InstanceTypes))
	}
	all := byName(cands)
	for _, it := range res.InstanceTypes {
		c := all[it.Name]
		p, _ := c.Price(keeper.AllocationSpot, 0)
		if c.VCPUs() < 2 || c.VCPUs() > 16 || p > 0.40 || p/float64(c.VCPUs()) > 0.05 || it.Price != p ||
			c.Server.AcceleratorCount != 0 || c.Server.CPUAllocation != "Dedicated" {
			t.Errorf("%s violates the selector: %+v", it.Name, c.Server)
		}
	}
}
