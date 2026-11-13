/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package keeper is a client of the Spare Cores Keeper API
// (https://keeper.sparecores.net). It only reads: /servers,
// /benchmark_configs, /table/region, /table/zone, /table/vendor,
// /table/server, /table/server_prices and /healthcheck.
//
// Responses are decoded by field name and unknown fields are ignored, so
// additive schema changes of sc-crawler do not break the client. The decoded
// schema is sc-crawler 0.10 (accelerator_* instead of gpu_*).
package keeper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// Allocation is Keeper's best_price_allocation: which price a /servers query
// reports and filters on.
type Allocation string

const (
	// AllocationOnDemand reports on-demand prices only.
	AllocationOnDemand Allocation = "ONDEMAND_ONLY"
	// AllocationSpot reports spot prices only.
	AllocationSpot Allocation = "SPOT_ONLY"
)

// Server is one row of GET /servers. Nullable fields are pointers; units are
// Spare Cores' own (MiB for memory, KiB for caches, GB for local storage,
// GHz, Gbps, USD per hour).
type Server struct {
	APIReference string  `json:"api_reference"`
	ServerID     string  `json:"server_id"`
	VendorID     string  `json:"vendor_id"`
	Name         string  `json:"name"`
	DisplayName  string  `json:"display_name"`
	Description  string  `json:"description"`
	Family       string  `json:"family"`
	Series       *string `json:"series"`
	Status       string  `json:"status"`

	VCPUs           int      `json:"vcpus"`
	CPUCores        *int     `json:"cpu_cores"`
	CPUAllocation   string   `json:"cpu_allocation"`
	CPUArchitecture string   `json:"cpu_architecture"`
	CPUManufacturer *string  `json:"cpu_manufacturer"`
	CPUFamily       *string  `json:"cpu_family"`
	CPUModel        *string  `json:"cpu_model"`
	CPUSpeed        *float64 `json:"cpu_speed"`
	CPUFlags        []string `json:"cpu_flags"`
	CPUL1dCache     *float64 `json:"cpu_l1d_cache"`
	CPUL1iCache     *float64 `json:"cpu_l1i_cache"`
	CPUL2Cache      *float64 `json:"cpu_l2_cache"`
	CPUL3Cache      *float64 `json:"cpu_l3_cache"`
	CPUL3CacheTotal *float64 `json:"cpu_l3_cache_total"`

	// MemoryAmount is in MiB.
	MemoryAmount float64 `json:"memory_amount"`

	AcceleratorCount        float64  `json:"accelerator_count"`
	AcceleratorType         *string  `json:"accelerator_type"`
	AcceleratorFamily       *string  `json:"accelerator_family"`
	AcceleratorManufacturer *string  `json:"accelerator_manufacturer"`
	AcceleratorModel        *string  `json:"accelerator_model"`
	AcceleratorMemoryMin    *float64 `json:"accelerator_memory_min"`
	AcceleratorMemoryTotal  *float64 `json:"accelerator_memory_total"`

	NetworkSpeedBaseline *float64 `json:"network_speed_baseline"`
	NetworkSpeedMax      *float64 `json:"network_speed_max"`

	// StorageSize is the total bundled local disk in GB (0: none).
	StorageSize float64   `json:"storage_size"`
	Storages    []Storage `json:"storages"`

	// Price is the cheapest price of the query's allocation in the queried
	// regions (USD per hour).
	Price            *float64 `json:"price"`
	MinPrice         *float64 `json:"min_price"`
	MinPriceOnDemand *float64 `json:"min_price_ondemand"`
	MinPriceSpot     *float64 `json:"min_price_spot"`

	// SelectedBenchmarkScore is the score of the queried benchmark_id and
	// benchmark_config, nil when the server has none.
	SelectedBenchmarkScore *float64 `json:"selected_benchmark_score"`
}

// Storage is one local disk of a server.
type Storage struct {
	Size        float64 `json:"size"`
	StorageType string  `json:"storage_type"`
}

// BenchmarkConfig is one row of GET /benchmark_configs. Config is the
// canonical JSON string Keeper matches exactly.
type BenchmarkConfig struct {
	BenchmarkID string  `json:"benchmark_id"`
	Config      string  `json:"config"`
	Category    string  `json:"category"`
	Subcategory *string `json:"subcategory"`
}

// Region is one row of GET /table/region.
type Region struct {
	VendorID     string   `json:"vendor_id"`
	RegionID     string   `json:"region_id"`
	APIReference string   `json:"api_reference"`
	Name         string   `json:"name"`
	DisplayName  string   `json:"display_name"`
	Aliases      []string `json:"aliases"`
	CountryID    string   `json:"country_id"`
	// State, City, AddressLine and ZipCode are the optional location of the
	// region (null in Keeper when unknown).
	State       *string `json:"state"`
	City        *string `json:"city"`
	AddressLine *string `json:"address_line"`
	ZipCode     *string `json:"zip_code"`
	// Lat and Lon are the known or approximate coordinates.
	Lat *float64 `json:"lat"`
	Lon *float64 `json:"lon"`
	// FoundingYear is the 4-digit year the region was founded.
	FoundingYear *int `json:"founding_year"`
	// GreenEnergy says whether the region is 100% powered by renewable
	// energy: true, false, or nil when Keeper does not know.
	GreenEnergy *bool `json:"green_energy"`
	// Status is "active" or "inactive" (a retired region).
	Status string `json:"status"`
}

// Zone is one row of GET /table/zone.
type Zone struct {
	VendorID     string `json:"vendor_id"`
	RegionID     string `json:"region_id"`
	ZoneID       string `json:"zone_id"`
	APIReference string `json:"api_reference"`
	Name         string `json:"name"`
	DisplayName  string `json:"display_name"`
	Status       string `json:"status"`
}

// Health is the subset of GET /healthcheck the controller uses: the hash and
// update time of the Spare Cores database Keeper serves, and its snapshots.
type Health struct {
	// DatabaseHash is the data version to pin; "" when Keeper serves no
	// published database (it reports null).
	DatabaseHash        string
	DatabaseLastUpdated time.Time
	// DatabasePublishedAt is when the database of DatabaseHash was
	// published; zero when Keeper does not say.
	DatabasePublishedAt time.Time
	// SnapshotHashes are the hashes the replica has snapshots of locally,
	// newest first.
	SnapshotHashes []string
}

// TableServer is one row of GET /table/server: the Server table as is, one row
// per vendor server type, without prices or benchmark scores. Units are Spare
// Cores' own (MiB for memory, KiB for caches, GB for local storage, GHz, Gbps).
type TableServer struct {
	VendorID     string  `json:"vendor_id"`
	ServerID     string  `json:"server_id"`
	Name         string  `json:"name"`
	APIReference string  `json:"api_reference"`
	DisplayName  string  `json:"display_name"`
	Description  string  `json:"description"`
	Family       *string `json:"family"`
	Series       *string `json:"series"`
	Hypervisor   *string `json:"hypervisor"`
	Status       string  `json:"status"`

	VCPUs           int      `json:"vcpus"`
	CPUCores        *int     `json:"cpu_cores"`
	CPUAllocation   string   `json:"cpu_allocation"`
	CPUArchitecture string   `json:"cpu_architecture"`
	CPUManufacturer *string  `json:"cpu_manufacturer"`
	CPUFamily       *string  `json:"cpu_family"`
	CPUModel        *string  `json:"cpu_model"`
	CPUSpeed        *float64 `json:"cpu_speed"`
	CPUFlags        []string `json:"cpu_flags"`
	CPUL1dCache     *float64 `json:"cpu_l1d_cache"`
	CPUL1iCache     *float64 `json:"cpu_l1i_cache"`
	CPUL2Cache      *float64 `json:"cpu_l2_cache"`
	CPUL3Cache      *float64 `json:"cpu_l3_cache"`
	CPUL3CacheTotal *float64 `json:"cpu_l3_cache_total"`

	// MemoryAmount is in MiB.
	MemoryAmount float64 `json:"memory_amount"`

	AcceleratorCount        float64  `json:"accelerator_count"`
	AcceleratorType         *string  `json:"accelerator_type"`
	AcceleratorFamily       *string  `json:"accelerator_family"`
	AcceleratorManufacturer *string  `json:"accelerator_manufacturer"`
	AcceleratorModel        *string  `json:"accelerator_model"`
	AcceleratorMemoryMin    *float64 `json:"accelerator_memory_min"`
	AcceleratorMemoryTotal  *float64 `json:"accelerator_memory_total"`

	NetworkSpeedBaseline *float64 `json:"network_speed_baseline"`
	NetworkSpeedMax      *float64 `json:"network_speed_max"`

	// StorageSize is the total bundled local disk in GB (0: none).
	StorageSize float64   `json:"storage_size"`
	Storages    []Storage `json:"storages"`
}

// PriceAllocation is the allocation of GET /table/server_prices (a different
// vocabulary from /servers' best_price_allocation).
type PriceAllocation string

const (
	// PriceAllocationOnDemand selects on-demand prices.
	PriceAllocationOnDemand PriceAllocation = "ondemand"
	// PriceAllocationSpot selects spot prices.
	PriceAllocationSpot PriceAllocation = "spot"
	// PriceAllocationReserved selects reserved prices.
	PriceAllocationReserved PriceAllocation = "reserved"
)

// ServerPrice is one row of GET /table/server_prices: the price of one server
// in one zone (without the relationships resolved; join ServerID with
// TableServer.ServerID and ZoneID with Zone.ZoneID).
type ServerPrice struct {
	VendorID        string          `json:"vendor_id"`
	RegionID        string          `json:"region_id"`
	ZoneID          string          `json:"zone_id"`
	ServerID        string          `json:"server_id"`
	OperatingSystem string          `json:"operating_system"`
	Allocation      PriceAllocation `json:"allocation"`
	// Unit is the billing unit (hour for compute).
	Unit string `json:"unit"`
	// Price is per Unit, in Currency.
	Price        float64 `json:"price"`
	PriceUpfront float64 `json:"price_upfront"`
	// PriceMonthly is Keeper's own column of the snapshot files (the
	// monthly price of an on-demand row, nil for spot); /table/server_prices
	// does not have it.
	PriceMonthly *float64 `json:"price_monthly"`
	// PriceTiered is the tiered pricing (hours), empty for flat prices.
	PriceTiered []PriceTier `json:"price_tiered"`
	Currency    string      `json:"currency"`
	Status      string      `json:"status"`
	ObservedAt  string      `json:"observed_at"`
}

// ZonePrice is one row of GET /server_prices (the anonymous search endpoint),
// reduced to the flat fields: the price of one server in one zone. Keeper
// embeds the full server, vendor, region and zone objects in every row; they
// are not decoded. Join ServerID with TableServer.ServerID and ZoneID with
// Zone.ZoneID.
type ZonePrice struct {
	VendorID string `json:"vendor_id"`
	RegionID string `json:"region_id"`
	ZoneID   string `json:"zone_id"`
	ServerID string `json:"server_id"`
	// OperatingSystem is the OS the price is for (for example Linux).
	OperatingSystem string          `json:"operating_system"`
	Allocation      PriceAllocation `json:"allocation"`
	// Unit is the billing unit (hour for compute); Price is per Unit.
	Unit  string  `json:"unit"`
	Price float64 `json:"price"`
	// PriceTiered is the tiered pricing (hours), empty for flat prices.
	PriceTiered []PriceTier `json:"price_tiered"`
	Currency    string      `json:"currency"`
	Status      string      `json:"status"`
	ObservedAt  string      `json:"observed_at"`
}

// Vendor is one row of GET /table/vendor (anonymous). The billing fields are
// nil when Keeper does not know them.
type Vendor struct {
	VendorID     string  `json:"vendor_id"`
	Name         string  `json:"name"`
	Logo         *string `json:"logo"`
	Homepage     *string `json:"homepage"`
	StatusPage   *string `json:"status_page"`
	CountryID    string  `json:"country_id"`
	State        *string `json:"state"`
	City         *string `json:"city"`
	AddressLine  *string `json:"address_line"`
	ZipCode      *string `json:"zip_code"`
	FoundingYear *int    `json:"founding_year"`
	// Status is "active" or "inactive".
	Status     string `json:"status"`
	ObservedAt string `json:"observed_at"`

	// BillingIncrementSeconds is the granularity compute is billed in.
	BillingIncrementSeconds *int `json:"billing_increment_seconds"`
	// MinimumBillingSeconds is the minimum billed duration of a run.
	MinimumBillingSeconds *int `json:"minimum_billing_seconds"`
	// StoppedServerCharged says whether a stopped server is still charged.
	StoppedServerCharged *bool `json:"stopped_server_charged"`
	// BillingComment is free text on billing exceptions.
	BillingComment *string `json:"billing_comment"`
}

// UnmarshalJSON decodes stopped_server_charged from true/false as well as
// 0/1 (some Keeper snapshots serve the column as an integer).
func (v *Vendor) UnmarshalJSON(b []byte) error {
	type plain Vendor
	aux := struct {
		*plain
		StoppedServerCharged json.RawMessage `json:"stopped_server_charged"`
	}{plain: (*plain)(v)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	v.StoppedServerCharged = nil
	if isNull(aux.StoppedServerCharged) {
		return nil
	}
	raw := bytes.TrimSpace(aux.StoppedServerCharged)
	switch string(raw) {
	case "true", "1":
		t := true
		v.StoppedServerCharged = &t
	case "false", "0":
		f := false
		v.StoppedServerCharged = &f
	default:
		return fmt.Errorf("keeper: stopped_server_charged %s is not a boolean", raw)
	}
	return nil
}

// PriceTier is one entry of price_tiered: Price applies from Lower up to Upper
// (hours; Upper is +Inf for the last tier).
type PriceTier struct {
	Lower float64 `json:"lower"`
	Upper float64 `json:"upper"`
	Price float64 `json:"price"`
}

// UnmarshalJSON accepts numbers as well as the strings "Infinity",
// "-Infinity" and "NaN" that Keeper serves for non-finite floats.
func (t *PriceTier) UnmarshalJSON(b []byte) error {
	var w struct {
		Lower json.RawMessage `json:"lower"`
		Upper json.RawMessage `json:"upper"`
		Price json.RawMessage `json:"price"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	var err error
	if t.Lower, err = pythonFloat(w.Lower); err != nil {
		return fmt.Errorf("keeper: price tier lower: %w", err)
	}
	if t.Upper, err = pythonFloat(w.Upper); err != nil {
		return fmt.Errorf("keeper: price tier upper: %w", err)
	}
	if t.Price, err = pythonFloat(w.Price); err != nil {
		return fmt.Errorf("keeper: price tier price: %w", err)
	}
	return nil
}

// MarshalJSON writes infinite and NaN bounds as the strings Keeper serves, so
// a decoded row encodes again (encoding/json rejects them as numbers).
func (t PriceTier) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Lower any `json:"lower"`
		Upper any `json:"upper"`
		Price any `json:"price"`
	}{pythonFloatValue(t.Lower), pythonFloatValue(t.Upper), pythonFloatValue(t.Price)})
}

// Keeper (pydantic, ser_json_inf_nan="strings") serves non-finite floats as
// these strings.
const (
	posInfinity = "Infinity"
	negInfinity = "-Infinity"
	notANumber  = "NaN"
)

// pythonFloat decodes a JSON number or one of the strings Infinity, -Infinity,
// NaN. A missing or null value is 0.
func pythonFloat(raw json.RawMessage) (float64, error) {
	if isNull(raw) {
		return 0, nil
	}
	raw = bytes.TrimSpace(raw)
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, err
		}
		switch s {
		case posInfinity:
			return math.Inf(1), nil
		case negInfinity:
			return math.Inf(-1), nil
		case notANumber:
			return math.NaN(), nil
		}
		return 0, fmt.Errorf("unexpected string %q", s)
	}
	var f float64
	err := json.Unmarshal(raw, &f)
	return f, err
}

// pythonFloatValue is the inverse of pythonFloat.
func pythonFloatValue(f float64) any {
	switch {
	case math.IsInf(f, 1):
		return posInfinity
	case math.IsInf(f, -1):
		return negInfinity
	case math.IsNaN(f):
		return notANumber
	}
	return f
}

// isNull reports whether raw JSON is absent (empty) or the null literal.
func isNull(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 || string(raw) == "null"
}
