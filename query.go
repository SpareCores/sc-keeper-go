/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package keeper

import (
	"math"
	"net/url"
	"slices"
	"strconv"
)

// Query is one logical GET /servers query. Regions are chunked into several
// HTTP requests by the client (see WithRegionsPerRequest).
type Query struct {
	// Vendor is the Keeper vendor id (aws, azure, gcp, ...).
	Vendor string
	// Regions are Keeper region ids of Vendor (not Kubernetes region names:
	// GCP and Hetzner use numeric ids).
	Regions []string
	// Allocation selects the reported price.
	Allocation Allocation
	// Benchmark, when set, adds benchmark_id and benchmark_config, and makes
	// Server.SelectedBenchmarkScore the score of that benchmark.
	Benchmark *BenchmarkKey
	// PushDown are server-side filters that only shrink the response. They
	// must be equal to or looser than the client-side checks, which are always
	// re-applied.
	PushDown PushDown
	// DataVersion, when set, is sent as data_version (see DataVersionParam).
	DataVersion string
}

// DataVersionParam carries Keeper's database hash on cacheable requests.
// Keeper ignores it, but the CDN in front of Keeper caches every URL on its
// own for up to an hour: with the hash in the URL, all requests of a refresh
// share one cache generation and see the same data version, and a new
// database version is a new set of URLs.
const DataVersionParam = "data_version"

// BenchmarkKey identifies one benchmark score: a benchmark_id and the
// canonical config string from /benchmark_configs.
type BenchmarkKey struct {
	ID     string
	Config string
}

// String formats the key for messages and logs.
func (k BenchmarkKey) String() string {
	return k.ID + " " + k.Config
}

// PushDown holds the /servers filter parameters with an exact client-side
// equivalent. Nil fields are not sent. Units are Keeper's:
// GiB for memory and accelerator memory, KiB for L1/L2 per core, MiB for L3,
// GB for local storage, USD per hour for prices.
type PushDown struct {
	VCPUsMin *int
	VCPUsMax *int
	// Architectures are Spare Cores names (x86_64, arm64).
	Architectures []string

	MemoryMin        *float64
	MemoryMax        *float64
	MemoryPerVCPUMin *float64
	MemoryPerVCPUMax *float64

	CPUSpeedMin        *float64
	CPUHyperthreading  *bool
	CPUL1dCacheMin     *float64
	CPUL1iCacheMin     *float64
	CPUL2CacheMin      *float64
	CPUL3CacheMin      *float64
	CPUL3CacheTotalMin *float64

	NetworkSpeedBaselineMin *float64
	NetworkSpeedMaxMin      *float64

	StorageSizeMin  *float64
	StorageCountMin *int

	GPUMin         *float64
	GPUMax         *float64
	GPUMemoryMin   *float64
	GPUMemoryTotal *float64

	PriceMax *float64

	// Benchmark thresholds apply to the query's Benchmark only.
	BenchmarkScoreMin         *float64
	BenchmarkScoreMax         *float64
	BenchmarkScorePerVCPUMin  *float64
	BenchmarkScorePerPriceMin *float64
}

// HasBenchmarkThreshold reports whether a benchmark threshold is pushed down,
// in which case Keeper drops rows without a score as well.
func (p PushDown) HasBenchmarkThreshold() bool {
	return p.BenchmarkScoreMin != nil || p.BenchmarkScoreMax != nil ||
		p.BenchmarkScorePerVCPUMin != nil || p.BenchmarkScorePerPriceMin != nil
}

// values encodes the query for the given regions (one chunk).
func (q Query) values(regions []string) url.Values {
	v := url.Values{}
	v.Set("vendor", q.Vendor)
	for _, r := range regions {
		v.Add("vendor_regions", q.Vendor+"~"+r)
	}
	if q.Allocation != "" {
		v.Set("best_price_allocation", string(q.Allocation))
	}
	v.Set("limit", "-1")
	if q.Benchmark != nil {
		v.Set("benchmark_id", q.Benchmark.ID)
		v.Set("benchmark_config", q.Benchmark.Config)
	}
	q.PushDown.encode(v)
	if q.DataVersion != "" {
		v.Set(DataVersionParam, q.DataVersion)
	}
	return v
}

func (p PushDown) encode(v url.Values) {
	setInt(v, "vcpus_min", p.VCPUsMin)
	setInt(v, "vcpus_max", p.VCPUsMax)
	for _, a := range p.Architectures {
		v.Add("architecture", a)
	}
	setFloat(v, "memory_min", p.MemoryMin)
	setFloat(v, "memory_max", p.MemoryMax)
	setFloat(v, "memory_per_vcpu_min", p.MemoryPerVCPUMin)
	setFloat(v, "memory_per_vcpu_max", p.MemoryPerVCPUMax)
	setFloat(v, "cpu_speed_min", p.CPUSpeedMin)
	if p.CPUHyperthreading != nil {
		v.Set("cpu_hyperthreading", strconv.FormatBool(*p.CPUHyperthreading))
	}
	// Keeper takes the cache sizes as integers (KiB); the bounds pushed down
	// are loosened by the removal margin and rarely whole. Rounding a lower
	// bound down only returns more rows, which the selection filters again.
	setFloor(v, "cpu_l1d_cache_min", p.CPUL1dCacheMin)
	setFloor(v, "cpu_l1i_cache_min", p.CPUL1iCacheMin)
	setFloor(v, "cpu_l2_cache_min", p.CPUL2CacheMin)
	setFloor(v, "cpu_l3_cache_min", p.CPUL3CacheMin)
	setFloor(v, "cpu_l3_cache_total_min", p.CPUL3CacheTotalMin)
	setFloat(v, "network_speed_baseline_min", p.NetworkSpeedBaselineMin)
	setFloat(v, "network_speed_max_min", p.NetworkSpeedMaxMin)
	setFloat(v, "storage_size", p.StorageSizeMin)
	setInt(v, "storage_count_min", p.StorageCountMin)
	setFloat(v, "gpu_min", p.GPUMin)
	setFloat(v, "gpu_max", p.GPUMax)
	setFloat(v, "gpu_memory_min", p.GPUMemoryMin)
	setFloat(v, "gpu_memory_total", p.GPUMemoryTotal)
	if p.PriceMax != nil {
		setFloat(v, "price_max", p.PriceMax)
		v.Set("currency", "USD")
	}
	setFloat(v, "benchmark_score_min", p.BenchmarkScoreMin)
	setFloat(v, "benchmark_score_max", p.BenchmarkScoreMax)
	setFloat(v, "benchmark_score_per_vcpu_min", p.BenchmarkScorePerVCPUMin)
	setFloat(v, "benchmark_score_per_price_min", p.BenchmarkScorePerPriceMin)
}

func setInt(v url.Values, key string, p *int) {
	if p != nil {
		v.Set(key, strconv.Itoa(*p))
	}
}

// setFloor sets an integer parameter from a lower bound, rounded down.
func setFloor(v url.Values, key string, p *float64) {
	if p != nil {
		v.Set(key, strconv.FormatFloat(math.Floor(*p), 'f', -1, 64))
	}
}

func setFloat(v url.Values, key string, p *float64) {
	if p != nil {
		v.Set(key, strconv.FormatFloat(*p, 'f', -1, 64))
	}
}

// cacheKey identifies the query independently of region order.
func (q Query) cacheKey() string {
	regions := slices.Clone(q.Regions)
	slices.Sort(regions)
	regions = slices.Compact(regions)
	return "/servers?" + q.values(regions).Encode()
}

// chunks splits the regions into groups of at most n (n <= 0: one group).
func chunks(regions []string, n int) [][]string {
	regions = slices.Clone(regions)
	slices.Sort(regions)
	regions = slices.Compact(regions)
	if n <= 0 || len(regions) <= n {
		return [][]string{regions}
	}
	var out [][]string
	for c := range slices.Chunk(regions, n) {
		out = append(out, c)
	}
	return out
}

// PriceQuery is one logical GET /table/server_prices query. Regions are
// chunked into several HTTP requests by the client, like those of Query.
type PriceQuery struct {
	// Vendor is the Keeper vendor id.
	Vendor string
	// Regions are Keeper region ids of Vendor; none selects every region.
	Regions []string
	// Allocation selects the price allocation; empty selects all.
	Allocation PriceAllocation
	// Currency asks for prices in that currency; empty keeps Keeper's default.
	Currency string
	// IncludeInactive sends only_active=false. Keeper's responses can contain
	// inactive prices either way, so filter on the rows' Status.
	IncludeInactive bool
	// DataVersion, when set, is sent as data_version (see DataVersionParam).
	DataVersion string
}

// values encodes the query for the given regions (one chunk).
func (q PriceQuery) values(regions []string) url.Values {
	v := url.Values{}
	v.Set("vendor", q.Vendor)
	for _, r := range regions {
		v.Add("vendor_regions", q.Vendor+"~"+r)
	}
	if q.Allocation != "" {
		v.Set("allocation", string(q.Allocation))
	}
	if q.Currency != "" {
		v.Set("currency", q.Currency)
	}
	if q.IncludeInactive {
		v.Set("only_active", "false")
	}
	if q.DataVersion != "" {
		v.Set(DataVersionParam, q.DataVersion)
	}
	return v
}

// cacheKey identifies the query independently of region order.
func (q PriceQuery) cacheKey() string {
	regions := slices.Clone(q.Regions)
	slices.Sort(regions)
	regions = slices.Compact(regions)
	return "/table/server_prices?" + q.values(regions).Encode()
}

// ServerPricesQuery is one logical GET /server_prices query (the anonymous
// search endpoint). Regions are chunked into several HTTP requests by the
// client (see WithServerPricesRegionsPerRequest).
type ServerPricesQuery struct {
	// Vendor is the Keeper vendor id.
	Vendor string
	// Regions are Keeper region ids of Vendor; none selects every region.
	Regions []string
	// Allocation selects the price allocation; empty selects all.
	Allocation PriceAllocation
	// Currency asks for prices in that currency; empty keeps Keeper's default
	// (USD).
	Currency string
	// IncludeInactive sends only_active=false. Keeper's responses can contain
	// inactive prices either way, so filter on the rows' Status.
	IncludeInactive bool
	// DataVersion, when set, is sent as data_version (see DataVersionParam).
	DataVersion string
}

// values encodes the query for the given regions (one chunk). limit=-1 turns
// the endpoint's default page of 25 rows off.
func (q ServerPricesQuery) values(regions []string) url.Values {
	v := url.Values{}
	v.Set("vendor", q.Vendor)
	for _, r := range regions {
		v.Add("vendor_regions", q.Vendor+"~"+r)
	}
	if q.Allocation != "" {
		v.Set("allocation", string(q.Allocation))
	}
	if q.Currency != "" {
		v.Set("currency", q.Currency)
	}
	if q.IncludeInactive {
		v.Set("only_active", "false")
	}
	v.Set("limit", "-1")
	if q.DataVersion != "" {
		v.Set(DataVersionParam, q.DataVersion)
	}
	return v
}

// cacheKey identifies the query independently of region order.
func (q ServerPricesQuery) cacheKey() string {
	regions := slices.Clone(q.Regions)
	slices.Sort(regions)
	regions = slices.Compact(regions)
	return "/server_prices?" + q.values(regions).Encode()
}
