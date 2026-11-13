/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package selection

import (
	"iter"
	"math"
	"slices"
	"strings"

	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/SpareCores/sc-keeper-go/selection/api"
)

// Status values of Keeper's server and price rows.
const (
	StatusActive               = "active"
	StatusPlannedForRetirement = "planned-for-retirement"
)

// Orderable reports whether a server with the status can still be ordered.
// Keeper's /servers lists only such servers by default (only_orderable, with
// sc-crawler's Status.is_orderable: ACTIVE and PLANNED_FOR_RETIREMENT).
func Orderable(status string) bool {
	return status == StatusActive || status == StatusPlannedForRetirement
}

// priceNDigits is the precision Keeper rounds a price to, in USD, before it
// compares or reports it (sc_keeper.helpers._PRICE_NDIGITS; the SQL rounds
// the regional minimum to 4 digits as well).
const priceNDigits = 4

func roundPrice(p float64) float64 {
	f := math.Pow10(priceNDigits)
	return math.Round(p*f) / f
}

// CatalogPrice is the price of one server in one zone, with the columns the
// rules of Accept need. The caller converts the currency; a catalog that
// has applied Accept already sets the neutral values (Unit "hour", Currency
// "USD", Status "active", a blank OperatingSystem).
type CatalogPrice struct {
	ServerID string
	RegionID string
	ZoneID   string
	// Allocation is Keeper's allocation of the row.
	Allocation      keeper.PriceAllocation
	OperatingSystem string
	Unit            string
	Currency        string
	Status          string
	// Price is per Unit, in Currency.
	Price float64
}

// CatalogPriceOf is the CatalogPrice of a price row of a snapshot file or of
// /table/server_prices.
func CatalogPriceOf(p *keeper.ServerPrice) CatalogPrice {
	return CatalogPrice{
		ServerID: p.ServerID, RegionID: p.RegionID, ZoneID: p.ZoneID, Allocation: p.Allocation,
		OperatingSystem: p.OperatingSystem, Unit: p.Unit, Currency: p.Currency, Status: p.Status, Price: p.Price,
	}
}

// ScoresOf is the score per server_id of one benchmark config, from the rows
// of a benchmark_score snapshot file, with the rules /servers applies to its
// selected_benchmark_score (sc_keeper.queries.gen_benchmark_query):
//   - only rows of the benchmark (benchmarkID) with status active count;
//   - only resource_type server (a database's score is not a server's);
//   - the config is matched as text, byte for byte, against configText (the
//     string /benchmark_configs returns); an empty configText matches every
//     config, as an absent benchmark_config does;
//   - a server with several matching rows gets the highest score, and a row
//     without a score counts for nothing.
func ScoresOf(rows []keeper.BenchmarkScore, benchmarkID, configText string) map[string]float64 {
	out := map[string]float64{}
	for i := range rows {
		r := &rows[i]
		if r.Score == nil || r.Status != StatusActive || r.ResourceType != "server" || r.BenchmarkID != benchmarkID {
			continue
		}
		if configText != "" && string(r.Config) != configText {
			continue
		}
		if old, seen := out[r.ResourceID]; !seen || *r.Score > old {
			out[r.ResourceID] = *r.Score
		}
	}
	return out
}

// AcceptPrice says whether a price row counts as the price of a server in a
// selection, and for which allocation.
//
// This is what the catalog keeps of Keeper's prices; it is slightly stricter
// than /servers, which applies only the first and the last rule. A row counts
// when:
//   - its status is active (Keeper: ServerPrice.status == ACTIVE, in
//     gen_live_price_query and in ServerExtra);
//   - its unit is hour (Keeper does not check it; sc-crawler stores hourly
//     server prices only);
//   - its currency is USD or blank (Keeper converts every row to USD, a
//     catalog reads the prices in USD);
//   - its operating system is Linux or blank (Keeper does not check it);
//   - its price is above zero (Keeper does not check it, but a zero price is
//     no price: Candidate.Price rejects it);
//   - its allocation is on-demand or spot, the two price bases of Keeper's
//     best_price_allocation (reserved prices only count in Keeper's overall
//     minimum, which a selection never reads: this is the "last rule" above).
func AcceptPrice(status, unit, currency, operatingSystem string, allocation keeper.PriceAllocation,
	price float64) (keeper.Allocation, bool) {
	if status != StatusActive || unit != "hour" || (currency != "" && currency != "USD") || price <= 0 {
		return "", false
	}
	if operatingSystem != "" && !strings.EqualFold(operatingSystem, "Linux") {
		return "", false
	}
	switch allocation {
	case keeper.PriceAllocationOnDemand:
		return keeper.AllocationOnDemand, true
	case keeper.PriceAllocationSpot:
		return keeper.AllocationSpot, true
	}
	return "", false
}

// Accept applies AcceptPrice to the row.
func (p *CatalogPrice) Accept() (keeper.Allocation, bool) {
	return AcceptPrice(p.Status, p.Unit, p.Currency, p.OperatingSystem, p.Allocation, p.Price)
}

// CatalogInput is the data of one vendor as a catalog holds it, from which
// BuildCatalogCandidates builds the candidates of a selection without asking
// Keeper.
type CatalogInput struct {
	// Servers are the vendor's servers: the specification columns of
	// keeper.Server (ServerID, APIReference, Family, VCPUs, ...). A row
	// without Status is taken as orderable (the catalog read active and
	// retiring servers only). Price fields are ignored.
	Servers []keeper.Server
	// Prices are the vendor's prices, in any region and allocation.
	Prices iter.Seq[CatalogPrice]
	// Regions are the region ids the selection considers (empty: all).
	Regions []string
	// Scores are benchmark scores by server_id, per benchmark; ScoresOf
	// builds one map from score rows with the rules of /servers. Types without
	// a score are absent. The key is the server id, not api_reference: the
	// two differ for GCP and Hetzner, and prices refer to the id.
	Scores map[keeper.BenchmarkKey]map[string]float64
}

// BuildCatalogCandidates builds the candidates of a selection that prices on
// the basis from catalog data. It applies what Keeper's /servers applies, and
// the result is what BuildCandidates returns for the /servers queries of
// PlanQueries, except for the push-down bookkeeping (KeeperFiltered stays
// empty: nothing is filtered out beforehand):
//   - a server counts when it is orderable and has at least one vCPU
//     (/servers' default vcpus_min is 1);
//   - its price is the lowest price of the basis, over the zones of the
//     regions, rounded to 4 digits (as Keeper does in USD); a server without
//     one is not a candidate (/servers, with a best_price_allocation, hides
//     it), so Candidate.Prices holds the basis only;
//   - its scores are the input's, mapped from server_id to the server.
//
// The result is sorted by name, as BuildCandidates' is.
func BuildCatalogCandidates(in CatalogInput, basis keeper.Allocation) []*Candidate {
	var inRegion map[string]bool
	if len(in.Regions) > 0 {
		inRegion = make(map[string]bool, len(in.Regions))
		for _, r := range in.Regions {
			inRegion[r] = true
		}
	}
	lowest := map[string]float64{}
	if in.Prices != nil {
		for p := range in.Prices {
			if inRegion != nil && !inRegion[p.RegionID] {
				continue
			}
			alloc, ok := p.Accept()
			if !ok || alloc != basis {
				continue
			}
			if old, seen := lowest[p.ServerID]; !seen || p.Price < old {
				lowest[p.ServerID] = p.Price
			}
		}
	}

	byName := map[string]*Candidate{}
	for i := range in.Servers {
		s := &in.Servers[i]
		if (s.Status != "" && !Orderable(s.Status)) || s.VCPUs < 1 {
			continue
		}
		price, ok := lowest[s.ServerID]
		if !ok {
			continue
		}
		if _, dup := byName[s.APIReference]; dup {
			continue
		}
		c := newCandidate(s)
		c.Prices[basis] = roundPrice(price)
		for key, byID := range in.Scores {
			if score, ok := byID[s.ServerID]; ok {
				c.Scores[key] = score
			}
		}
		byName[s.APIReference] = c
	}
	out := make([]*Candidate, 0, len(byName))
	for _, c := range byName {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b *Candidate) int { return strings.Compare(a.Name(), b.Name()) })
	return out
}

// BenchmarkKeys returns the benchmarks a spec reads (its benchmark filters,
// workload profiles and the ranking score), without duplicates, after
// validating the spec as Evaluate does. The caller fetches their scores for
// CatalogInput.Scores.
func BenchmarkKeys(spec *api.Spec, opts Options) ([]keeper.BenchmarkKey, error) {
	cs, err := compile(spec, opts)
	if err != nil {
		return nil, err
	}
	var keys []keeper.BenchmarkKey
	add := func(k keeper.BenchmarkKey) {
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	for _, b := range cs.benches {
		add(b.key)
	}
	if cs.ranking != nil {
		add(cs.ranking.key)
	}
	return keys, nil
}
