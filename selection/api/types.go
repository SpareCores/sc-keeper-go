/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package api holds the Kubernetes-style API types of the selection engine:
// the Selector and Ranking objects that sc-karpenter-nodepool (SCNodePool) and
// SCarpenter embed in their CRDs, plus the Churn settings and the status
// counters the engine produces. They carry kubebuilder markers, so
// controller-gen generates the same schema in every CRD that embeds them. The
// package depends on k8s.io/apimachinery types only (no client).
//
// +kubebuilder:object:generate=true
package api

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Numeric conventions of this API. A CRD that embeds these types must be
// generated with controller-gen's crd:allowDangerousTypes=true, because of the
// float64 fields:
//
//   - counts and ranks are integers;
//   - every other number (GiB, GB, GHz, Gbps, KiB, MiB, percent, benchmark
//     scores) is a float64, so YAML such as `minGiB: 3.8` or `min: 1.5` works
//     without quoting;
//   - money (USD per hour) is the exception: it is a decimal string such as
//     "0.40", so the value never goes through binary floating point.
//
// Memory sizes are GiB (2^30 bytes), prices are USD per hour. All selector
// semantics are specified in docs/reference/selectors.md of
// sc-karpenter-nodepool.

// Architecture is a Kubernetes CPU architecture name.
// +kubebuilder:validation:Enum=amd64;arm64
type Architecture string

// CPUAllocation is how Spare Cores classifies the CPU time a type gets.
// +kubebuilder:validation:Enum=Shared;Burstable;Dedicated
type CPUAllocation string

// AcceleratorType is the type of the primary accelerator of a server.
// +kubebuilder:validation:Enum=GPU;TPU
type AcceleratorType string

// PriceBasis selects the Spare Cores price allocation.
// +kubebuilder:validation:Enum=OnDemand;Spot
type PriceBasis string

// Normalize selects how a benchmark score is normalised before it is compared.
//   - None: the raw score;
//   - PerVcpu: score divided by the number of vCPUs;
//   - PerPrice: score divided by the hourly price on the price basis.
//
// +kubebuilder:validation:Enum=None;PerVcpu;PerPrice
type Normalize string

// RankingNormalize is the normalisation applied to the ranking score.
// +kubebuilder:validation:Enum=None;PerVcpu
type RankingNormalize string

// Objective is what the ranking optimises.
//   - Score: the best score first;
//   - ScorePerPrice: the best score per dollar first;
//   - Price: the cheapest first.
//
// +kubebuilder:validation:Enum=Score;ScorePerPrice;Price
type Objective string

// UnbenchmarkedPolicy decides what happens to candidates that lack a score
// needed by a benchmark filter, workload profile or the ranking score.
//   - Exclude: drop them;
//   - IncludeIfFamilyQualifies: keep an unscored size if a scored size of the
//     same family qualifies;
//   - Include: keep them, ignoring benchmark filters for that type.
//
// +kubebuilder:validation:Enum=Exclude;IncludeIfFamilyQualifies;Include
type UnbenchmarkedPolicy string

// WorkloadProfileName is one of the Spare Cores compound workload benchmarks.
// +kubebuilder:validation:Enum=web;compute;cache;data_analysis;llm;cicd
type WorkloadProfileName string

const (
	ArchitectureAMD64 Architecture = "amd64"
	ArchitectureARM64 Architecture = "arm64"

	CPUAllocationShared    CPUAllocation = "Shared"
	CPUAllocationBurstable CPUAllocation = "Burstable"
	CPUAllocationDedicated CPUAllocation = "Dedicated"

	AcceleratorTypeGPU AcceleratorType = "GPU"
	AcceleratorTypeTPU AcceleratorType = "TPU"

	PriceBasisOnDemand PriceBasis = "OnDemand"
	PriceBasisSpot     PriceBasis = "Spot"

	NormalizeNone     Normalize = "None"
	NormalizePerVcpu  Normalize = "PerVcpu"
	NormalizePerPrice Normalize = "PerPrice"

	RankingNormalizeNone    RankingNormalize = "None"
	RankingNormalizePerVcpu RankingNormalize = "PerVcpu"

	ObjectiveScore         Objective = "Score"
	ObjectiveScorePerPrice Objective = "ScorePerPrice"
	ObjectivePrice         Objective = "Price"

	UnbenchmarkedExclude                  UnbenchmarkedPolicy = "Exclude"
	UnbenchmarkedIncludeIfFamilyQualifies UnbenchmarkedPolicy = "IncludeIfFamilyQualifies"
	UnbenchmarkedInclude                  UnbenchmarkedPolicy = "Include"
)

// Churn controls hysteresis when instance types leave the selection
// (docs: churn control). Additions are always applied immediately.
type Churn struct {
	// removeMarginPercent: a type is removed only if it fails the selection
	// by more than this percentage of the failing threshold (or is no longer
	// offered at all).
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=0
	// +optional
	RemoveMarginPercent *float64 `json:"removeMarginPercent,omitempty"`

	// removeAfterRefreshes: the number of consecutive refreshes a type must
	// fail before it is removed.
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=0
	// +optional
	RemoveAfterRefreshes *int32 `json:"removeAfterRefreshes,omitempty"`

	// maxRemovalsPerRefresh caps the removals of one refresh (types the
	// provider no longer offers are exempt).
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxRemovalsPerRefresh *int32 `json:"maxRemovalsPerRefresh,omitempty"`
}

// ---------------------------------------------------------------------------
// Selector
// ---------------------------------------------------------------------------

// Selector filters instance types by Spare Cores data. Fields are AND-ed.
type Selector struct {
	// instanceTypes include/exclude globs on the instance type name
	// (server.api_reference).
	// +optional
	InstanceTypes *GlobFilter `json:"instanceTypes,omitempty"`

	// families include/exclude globs on the instance family: the Spare Cores series
	// (e.g. m7i) when set, otherwise the family (e.g. General Purpose).
	// +optional
	Families *GlobFilter `json:"families,omitempty"`

	// vcpus are inclusive bounds on the vCPU count.
	// +optional
	VCPUs *IntRange `json:"vcpus,omitempty"`

	// architectures restricts the CPU architecture; intersected with a
	// kubernetes.io/arch requirement of the template.
	// +kubebuilder:validation:MaxItems=2
	// +optional
	Architectures []Architecture `json:"architectures,omitempty"`

	// +optional
	CPU *CPUSelector `json:"cpu,omitempty"`
	// +optional
	Memory *MemorySelector `json:"memory,omitempty"`
	// +optional
	Accelerator *AcceleratorSelector `json:"accelerator,omitempty"`
	// +optional
	Network *NetworkSelector `json:"network,omitempty"`
	// +optional
	Storage *StorageSelector `json:"storage,omitempty"`
	// +optional
	Price *PriceSelector `json:"price,omitempty"`

	// benchmarks are filters on benchmark scores; entries are AND-ed.
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	// +optional
	Benchmarks []BenchmarkFilter `json:"benchmarks,omitempty"`

	// workloadProfiles are filters on the compound workload benchmarks;
	// entries are AND-ed with the benchmarks.
	// +kubebuilder:validation:MaxItems=8
	// +listType=atomic
	// +optional
	WorkloadProfiles []WorkloadProfileFilter `json:"workloadProfiles,omitempty"`

	// unbenchmarked decides what happens to candidates that lack a score
	// needed by a benchmark filter, workload profile or the ranking score.
	// +kubebuilder:default=Exclude
	// +optional
	Unbenchmarked UnbenchmarkedPolicy `json:"unbenchmarked,omitempty"`
}

// GlobFilter keeps a name if it matches any include glob (or include is
// empty) and no exclude glob. Globs are shell globs (*, ?, [...]),
// case-sensitive.
type GlobFilter struct {
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=128
	// +optional
	Include []string `json:"include,omitempty"`
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=128
	// +optional
	Exclude []string `json:"exclude,omitempty"`
}

// IntRange is an inclusive integer range; either bound may be omitted.
// +kubebuilder:validation:XValidation:rule="!has(self.min) || !has(self.max) || self.min <= self.max",message="min must not exceed max"
type IntRange struct {
	// +kubebuilder:validation:Minimum=0
	// +optional
	Min *int32 `json:"min,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	Max *int32 `json:"max,omitempty"`
}

// FloatRange is an inclusive range; either bound may be omitted.
// +kubebuilder:validation:XValidation:rule="!has(self.min) || !has(self.max) || self.min <= self.max",message="min must not exceed max"
type FloatRange struct {
	// +kubebuilder:validation:Minimum=0
	// +optional
	Min *float64 `json:"min,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	Max *float64 `json:"max,omitempty"`
}

// GiBRange is an inclusive range in GiB; either bound may be omitted.
// +kubebuilder:validation:XValidation:rule="!has(self.minGiB) || !has(self.maxGiB) || self.minGiB <= self.maxGiB",message="minGiB must not exceed maxGiB"
type GiBRange struct {
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinGiB *float64 `json:"minGiB,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxGiB *float64 `json:"maxGiB,omitempty"`
}

// GiBMin is a lower bound in GiB.
type GiBMin struct {
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinGiB *float64 `json:"minGiB,omitempty"`
}

// CPUSelector selects on CPU facts.
type CPUSelector struct {
	// manufacturers are exact, case-sensitive Spare Cores values (Intel, AMD,
	// AWS, Ampere, Alibaba, Microsoft, Hygon).
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=64
	// +optional
	Manufacturers []string `json:"manufacturers,omitempty"`

	// allocations restricts the CPU allocation class.
	// +kubebuilder:validation:MaxItems=3
	// +optional
	Allocations []CPUAllocation `json:"allocations,omitempty"`

	// clockSpeedMinGHz is a lower bound on the vendor-reported clock speed.
	// +kubebuilder:validation:Minimum=0
	// +optional
	ClockSpeedMinGHz *float64 `json:"clockSpeedMinGHz,omitempty"`

	// hyperthreading true selects types with more vCPUs than physical cores
	// (SMT siblings), false selects one vCPU per physical core. Unset: no
	// filter.
	// +optional
	Hyperthreading *bool `json:"hyperthreading,omitempty"`

	// flags filters on Linux /proc/cpuinfo flag names, lowercase.
	// +optional
	Flags *CPUFlags `json:"flags,omitempty"`

	// caches filters on cache sizes (lower bounds only).
	// +optional
	Caches *CPUCaches `json:"caches,omitempty"`
}

// CPUFlags filters on CPU flags.
type CPUFlags struct {
	// allOf: every listed flag must be present.
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=64
	// +optional
	AllOf []string `json:"allOf,omitempty"`
	// anyOf: at least one listed flag must be present.
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=64
	// +optional
	AnyOf []string `json:"anyOf,omitempty"`
	// noneOf: none of the listed flags may be present.
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=64
	// +optional
	NoneOf []string `json:"noneOf,omitempty"`
}

// CPUCaches are lower bounds on cache sizes. L1 and L2 are per physical core;
// L3 is the largest single domain (l3MinMiB) or the sum of all domains
// (l3TotalMinMiB) visible to the VM.
type CPUCaches struct {
	// +kubebuilder:validation:Minimum=0
	// +optional
	L1dPerCoreMinKiB *float64 `json:"l1dPerCoreMinKiB,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	L1iPerCoreMinKiB *float64 `json:"l1iPerCoreMinKiB,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	L2PerCoreMinKiB *float64 `json:"l2PerCoreMinKiB,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	L3MinMiB *float64 `json:"l3MinMiB,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	L3TotalMinMiB *float64 `json:"l3TotalMinMiB,omitempty"`
}

// MemorySelector selects on memory size, in GiB.
// +kubebuilder:validation:XValidation:rule="!has(self.minGiB) || !has(self.maxGiB) || self.minGiB <= self.maxGiB",message="minGiB must not exceed maxGiB"
type MemorySelector struct {
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinGiB *float64 `json:"minGiB,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxGiB *float64 `json:"maxGiB,omitempty"`
	// perVcpu bounds the GiB per vCPU (2 = compute-optimised, 4 = general
	// purpose, 8 = memory-optimised).
	// +optional
	PerVCPU *GiBRange `json:"perVcpu,omitempty"`
}

// AcceleratorSelector selects on GPUs and TPUs (Spare Cores "accelerators").
type AcceleratorSelector struct {
	// count is inclusive and may be fractional (vGPU slices). max: 0 selects
	// servers without any accelerator.
	// +optional
	Count *FloatRange `json:"count,omitempty"`

	// types restricts the type of the primary accelerator. Unknown types are
	// excluded when this is set.
	// +kubebuilder:validation:MaxItems=2
	// +optional
	Types []AcceleratorType `json:"types,omitempty"`

	// memoryPerDevice: memory of the smallest accelerator, lower bound.
	// +optional
	MemoryPerDevice *GiBMin `json:"memoryPerDevice,omitempty"`

	// memoryTotal: memory summed over all accelerators, lower bound.
	// +optional
	MemoryTotal *GiBMin `json:"memoryTotal,omitempty"`

	// manufacturers are Spare Cores values (NVIDIA, AMD, Habana, Google, ...).
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=64
	// +optional
	Manufacturers []string `json:"manufacturers,omitempty"`

	// models are exact Spare Cores model strings (L40S, A10G, ...); they are
	// not normalised across vendors.
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=64
	// +optional
	Models []string `json:"models,omitempty"`
}

// NetworkSelector selects on network bandwidth. Setting a field excludes
// every type without a value.
type NetworkSelector struct {
	// baselineMinGbps is a lower bound on the guaranteed bandwidth.
	// +kubebuilder:validation:Minimum=0
	// +optional
	BaselineMinGbps *float64 `json:"baselineMinGbps,omitempty"`
	// peakMinGbps is a lower bound on the burst bandwidth.
	// +kubebuilder:validation:Minimum=0
	// +optional
	PeakMinGbps *float64 `json:"peakMinGbps,omitempty"`
}

// StorageSelector selects on instance-local storage.
type StorageSelector struct {
	// +optional
	Local *LocalStorageSelector `json:"local,omitempty"`
}

// LocalStorageSelector selects on the disks bundled with the instance type.
type LocalStorageSelector struct {
	// minGB is a lower bound on the total local disk size in GB.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinGB *float64 `json:"minGB,omitempty"`
	// countMin is a lower bound on the number of local disks. 1 is evaluated
	// as "has local storage" on every vendor; 2 or more needs per-disk data.
	// +kubebuilder:validation:Minimum=0
	// +optional
	CountMin *int32 `json:"countMin,omitempty"`
}

// PriceSelector selects on the hourly price in USD.
type PriceSelector struct {
	// basis is the price allocation. Default: Spot if the template's
	// karpenter.sh/capacity-type requirement allows only spot, otherwise
	// OnDemand.
	// +optional
	Basis PriceBasis `json:"basis,omitempty"`

	// maxHourly is an inclusive upper bound on the hourly price of the type
	// in USD, as a decimal string (for example "0.40"). It also caps the node
	// size.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	// +kubebuilder:validation:MaxLength=16
	// +optional
	MaxHourly string `json:"maxHourly,omitempty"`

	// maxHourlyPerVcpu is an inclusive upper bound on the hourly price per
	// vCPU in USD, as a decimal string.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	// +kubebuilder:validation:MaxLength=16
	// +optional
	MaxHourlyPerVCPU string `json:"maxHourlyPerVcpu,omitempty"`

	// includeManagedFee adds the EKS Auto Mode management fee to prices
	// before filtering and ranking. Defaults to true on Auto Mode and is
	// ignored elsewhere.
	// +optional
	IncludeManagedFee *bool `json:"includeManagedFee,omitempty"`
}

// BenchmarkRef identifies one Spare Cores benchmark.
type BenchmarkRef struct {
	// id is a benchmark_id, for example geekbench:score.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +required
	ID string `json:"id"`

	// config is a JSON object. Required when the benchmark has more than one
	// config; validated against Keeper's benchmark_configs by comparing
	// parsed objects.
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Type=object
	// +optional
	Config *apiextensionsv1.JSON `json:"config,omitempty"`
}

// BenchmarkFilter is one filter on a benchmark score.
// +kubebuilder:validation:XValidation:rule="has(self.min) || has(self.max)",message="a benchmark entry needs min or max"
// +kubebuilder:validation:XValidation:rule="!has(self.min) || !has(self.max) || self.min <= self.max",message="min must not exceed max"
type BenchmarkFilter struct {
	BenchmarkRef `json:",inline"`

	// min is an inclusive lower bound on the (normalised) score, for
	// higher-is-better benchmarks.
	// +optional
	Min *float64 `json:"min,omitempty"`

	// max is an inclusive upper bound, for lower-is-better benchmarks
	// (latencies).
	// +optional
	Max *float64 `json:"max,omitempty"`

	// normalize selects the score normalisation.
	// +kubebuilder:default=None
	// +optional
	Normalize Normalize `json:"normalize,omitempty"`
}

// WorkloadProfileFilter is a filter on workload_profile:<name>. Scores are
// unitless; 1.0 is the fleet-median server.
type WorkloadProfileFilter struct {
	// +required
	Name WorkloadProfileName `json:"name"`

	// min is an inclusive lower bound on the (normalised) score.
	// +kubebuilder:validation:Minimum=0
	// +required
	Min float64 `json:"min"`

	// normalize selects the score normalisation.
	// +kubebuilder:default=None
	// +optional
	Normalize Normalize `json:"normalize,omitempty"`
}

// ---------------------------------------------------------------------------
// Ranking
// ---------------------------------------------------------------------------

// Ranking keeps the best families of the candidates. At least one of band and
// families.max must be set.
// +kubebuilder:validation:XValidation:rule="has(self.band) || (has(self.families) && has(self.families.max))",message="ranking needs band or families.max"
type Ranking struct {
	// score is what to rank by.
	// +required
	Score RankingScore `json:"score"`

	// objective is what the ranking optimises.
	// +kubebuilder:default=Score
	// +optional
	Objective Objective `json:"objective,omitempty"`

	// normalize is applied to the score before the objective.
	// +kubebuilder:default=None
	// +optional
	Normalize RankingNormalize `json:"normalize,omitempty"`

	// band keeps the families whose objective is within a percentage of the
	// best family's.
	// +optional
	Band *RankingBand `json:"band,omitempty"`

	// families bounds the number of kept families.
	// +optional
	Families *RankingFamilies `json:"families,omitempty"`
}

// RankingScore selects the ranking score: exactly one of benchmark and
// workloadProfile.
// +kubebuilder:validation:XValidation:rule="has(self.benchmark) != has(self.workloadProfile)",message="set exactly one of benchmark and workloadProfile"
type RankingScore struct {
	// +optional
	Benchmark *BenchmarkRef `json:"benchmark,omitempty"`
	// +optional
	WorkloadProfile WorkloadProfileName `json:"workloadProfile,omitempty"`
}

// RankingBand is the acceptance band around the best family.
type RankingBand struct {
	// withinPercent: keep families within this percentage of the best.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +required
	WithinPercent float64 `json:"withinPercent"`
}

// RankingFamilies bounds the number of kept families.
// +kubebuilder:validation:XValidation:rule="!has(self.min) || !has(self.max) || self.min <= self.max",message="min must not exceed max"
type RankingFamilies struct {
	// min widens the band until at least this many families are kept (the
	// capacity floor).
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	Min *int32 `json:"min,omitempty"`
	// max keeps at most this many families, even if more are inside the band.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Max *int32 `json:"max,omitempty"`
}

// BandStatus reports the ranking band that was applied.
type BandStatus struct {
	// reference is the best family.
	// +optional
	Reference string `json:"reference,omitempty"`
	// referenceValue is the objective value of the reference family.
	// +optional
	ReferenceValue *float64 `json:"referenceValue,omitempty"`
	// lowestKept is the lowest-ranked kept family.
	// +optional
	LowestKept string `json:"lowestKept,omitempty"`
	// lowestKeptDeltaPercent is the distance of lowestKept from the reference.
	// +optional
	LowestKeptDeltaPercent *float64 `json:"lowestKeptDeltaPercent,omitempty"`
	// widened: families.min widened the band.
	// +optional
	Widened bool `json:"widened,omitempty"`
	// trimmed: families.max trimmed the band.
	// +optional
	Trimmed bool `json:"trimmed,omitempty"`
}

// ExcludedStatus counts candidates dropped per pipeline step.
type ExcludedStatus struct {
	// +optional
	ProviderIneligible int32 `json:"providerIneligible,omitempty"`
	// +optional
	TemplateConstraints int32 `json:"templateConstraints,omitempty"`
	// filters counts the candidates dropped by each selector field.
	// +optional
	Filters map[string]int32 `json:"filters,omitempty"`
	// +optional
	MissingData int32 `json:"missingData,omitempty"`
	// +optional
	Ranking int32 `json:"ranking,omitempty"`
}

// PendingRemoval is an instance type waiting to be removed.
type PendingRemoval struct {
	Name string `json:"name"`
	// since is when the type first failed the selection.
	Since metav1.Time `json:"since"`
	// refreshes is the number of consecutive refreshes it has failed.
	Refreshes int32 `json:"refreshes"`
}

// Spec is the part of a pool spec the selection engine reads. The CRDs that
// embed the selection API build one from their own spec type.
type Spec struct {
	Selector *Selector `json:"selector,omitempty"`
	Ranking  *Ranking  `json:"ranking,omitempty"`
	Churn    Churn     `json:"churn,omitzero"`
}
