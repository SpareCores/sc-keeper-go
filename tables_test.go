/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package keeper_test

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/SpareCores/sc-keeper-go/keepertest"
)

// tableServerSample is four rows of the real /table/server response.
func tableServerSample(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("keepertest", "testdata", "table_server_sample.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// priceRows are hand-written rows in the schema of Keeper's ServerPrice (the
// endpoint needs an API key, so no real response is committed).
const priceRows = `[
 {"vendor_id":"aws","region_id":"us-east-1","zone_id":"use1-az1","server_id":"a1.2xlarge","operating_system":"Linux",
  "allocation":"ondemand","unit":"hour","price":0.204,"price_upfront":0,"price_tiered":[],"currency":"USD",
  "status":"active","observed_at":"2026-11-05T18:44:41.054216","future_field":1},
 {"vendor_id":"aws","region_id":"us-east-1","zone_id":"use1-az2","server_id":"a1.2xlarge","operating_system":"Linux",
  "allocation":"spot","unit":"hour","price":0.0612,"price_upfront":0,"price_tiered":[],"currency":"USD",
  "status":"inactive","observed_at":"2026-11-05T18:44:41.054216"}
]`

func TestTableServers(t *testing.T) {
	var path atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.String())
		_, _ = w.Write(tableServerSample(t))
	}))
	defer srv.Close()

	rows, err := keeper.New(srv.URL).TableServers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := path.Load(); got != "/table/server" {
		t.Errorf("requested %v", got)
	}
	if len(rows) != 4 {
		t.Fatalf("%d rows", len(rows))
	}
	a1 := rows[0]
	if a1.VendorID != "aws" || a1.APIReference != "a1.2xlarge" || a1.VCPUs != 8 || a1.CPUArchitecture != "arm64" ||
		a1.MemoryAmount != 16384 || a1.CPUManufacturer == nil || a1.Status != "active" || a1.CPUCores == nil {
		t.Errorf("unexpected row: %+v", a1)
	}
	if dl1 := rows[3]; dl1.AcceleratorCount != 8 || dl1.AcceleratorType == nil || *dl1.AcceleratorType != "GPU" ||
		dl1.AcceleratorMemoryTotal == nil {
		t.Errorf("accelerator columns not decoded: %+v", dl1)
	}
}

func TestTableServerPrices(t *testing.T) {
	var urls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		urls = append(urls, r.URL.String())
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, `{"detail":"Authentication required"}`, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(priceRows))
	}))
	defer srv.Close()
	q := keeper.PriceQuery{
		Vendor: "aws", Regions: []string{"us-east-1", "us-west-2", "eu-west-1", "ap-south-1"},
		Allocation: keeper.PriceAllocationOnDemand, Currency: "USD", IncludeInactive: true,
	}

	_, err := keeper.New(srv.URL).TableServerPrices(context.Background(), q)
	var he *keeper.HTTPError
	if !errors.As(err, &he) || he.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous request: %v", err)
	}

	urls = nil
	rows, err := keeper.New(srv.URL, keeper.WithAPIKey("secret")).TableServerPrices(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	// 4 regions in chunks of 3: two requests, rows concatenated.
	if len(urls) != 2 || len(rows) != 4 {
		t.Fatalf("%d requests %v, %d rows", len(urls), urls, len(rows))
	}
	first := urls[0]
	for _, want := range []string{
		"/table/server_prices?", "vendor=aws", "allocation=ondemand", "currency=USD", "only_active=false",
		"vendor_regions=aws~ap-south-1", "vendor_regions=aws~eu-west-1", "vendor_regions=aws~us-east-1",
	} {
		if !strings.Contains(strings.ReplaceAll(first, "%7E", "~"), want) {
			t.Errorf("first request %s lacks %s", first, want)
		}
	}
	p := rows[0]
	if p.ZoneID != "use1-az1" || p.ServerID != "a1.2xlarge" || p.Allocation != keeper.PriceAllocationOnDemand ||
		p.Price != 0.204 || p.Currency != "USD" || p.Unit != "hour" {
		t.Errorf("unexpected price: %+v", p)
	}
	if rows[1].Status != "inactive" {
		t.Errorf("unexpected spot price: %+v", rows[1])
	}

	// No regions: one request for the whole vendor.
	urls = nil
	all := keeper.New(srv.URL, keeper.WithAPIKey("secret"))
	if _, err := all.TableServerPrices(context.Background(), keeper.PriceQuery{Vendor: "aws"}); err != nil {
		t.Fatal(err)
	}
	if len(urls) != 1 || strings.Contains(urls[0], "vendor_regions") || strings.Contains(urls[0], "allocation") ||
		strings.Contains(urls[0], "only_active") {
		t.Errorf("whole-vendor request: %v", urls)
	}
	if _, err := all.TableServerPrices(context.Background(), keeper.PriceQuery{}); err == nil {
		t.Error("query without vendor accepted")
	}
}

// writeSnapshot writes a snapshot directory with the given gzipped responses.
func writeSnapshot(t *testing.T, queries string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		zw := gzip.NewWriter(f)
		_, _ = zw.Write([]byte(body))
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	manifest := `{"keeper": {"database_hash": "h1", "database_last_updated": "2026-01-01T00:00:00+00:00"}, "queries": ` + queries + `}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestTableEndpointsReplay(t *testing.T) {
	dir := writeSnapshot(t, `[
	  {"path": "/table/server", "params": {}, "file": "server.json.gz"},
	  {"path": "/table/server_prices", "params": {"vendor": "aws", "vendor_regions": "aws~us-east-1", "allocation": "ondemand", "currency": "USD"}, "file": "od.json.gz"},
	  {"path": "/table/server_prices", "params": {"vendor": "aws", "vendor_regions": "aws~us-east-1", "allocation": "spot", "currency": "USD"}, "file": "spot.json.gz"},
	  {"path": "/table/server_prices", "params": {"vendor": "aws", "vendor_regions": ["aws~us-east-1", "aws~us-west-2"], "allocation": "ondemand"}, "file": "multi.json.gz"}]`,
		map[string]string{
			"server.json.gz": string(tableServerSample(t)),
			"od.json.gz":     `[{"server_id": "od", "price": 1, "zone_id": "z"}]`,
			"spot.json.gz":   `[{"server_id": "spot", "price": 1, "zone_id": "z"}]`,
			"multi.json.gz":  `[{"server_id": "multi", "price": 1, "zone_id": "z"}]`,
		})
	srv := keepertest.NewFromDir(t, dir)
	f := keeper.NewFetcher(keeper.New(srv.URL))
	ctx := context.Background()

	servers, err := f.TableServers(ctx)
	if err != nil || len(servers) != 4 {
		t.Fatalf("TableServers: %d rows, %v", len(servers), err)
	}
	for alloc, want := range map[keeper.PriceAllocation]string{keeper.PriceAllocationOnDemand: "od", keeper.PriceAllocationSpot: "spot"} {
		rows, err := f.TableServerPrices(ctx, keeper.PriceQuery{
			Vendor: "aws", Regions: []string{"us-east-1"}, Allocation: alloc, Currency: "USD",
		})
		if err != nil || len(rows) != 1 || rows[0].ServerID != want {
			t.Errorf("%s: %+v, %v", alloc, rows, err)
		}
	}
	// A request is matched on allocation and currency: the recorded query
	// without currency is another one, and unknown ones are 404.
	var he *keeper.HTTPError
	if _, err := f.TableServerPrices(ctx, keeper.PriceQuery{Vendor: "aws", Regions: []string{"us-east-1"}, Allocation: keeper.PriceAllocationReserved, Currency: "USD"}); !errors.As(err, &he) || he.StatusCode != http.StatusNotFound {
		t.Errorf("unrecorded allocation: %v", err)
	}
	if _, err := f.TableServerPrices(ctx, keeper.PriceQuery{Vendor: "aws", Regions: []string{"us-east-1"}, Allocation: keeper.PriceAllocationOnDemand}); !errors.As(err, &he) {
		t.Errorf("unrecorded currency: %v", err)
	}
	// Repeated parameters of the manifest are a set of regions; a client
	// that chunks them by 2 matches the recorded query.
	multi := keeper.New(srv.URL, keeper.WithRegionsPerRequest(2))
	rows, err := multi.TableServerPrices(ctx, keeper.PriceQuery{Vendor: "aws", Regions: []string{"us-west-2", "us-east-1"}, Allocation: keeper.PriceAllocationOnDemand})
	if err != nil || len(rows) != 1 || rows[0].ServerID != "multi" {
		t.Errorf("multi-region query: %+v, %v", rows, err)
	}

	// The fetcher memoises (one request per distinct query, plus the
	// healthcheck).
	before := len(srv.Requests())
	for range 3 {
		if _, err := f.TableServers(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(srv.Requests()) - before; n != 0 {
		t.Errorf("%d requests for memoised TableServers", n)
	}
	var tables []string
	for _, u := range srv.Requests() {
		if strings.HasPrefix(u.Path, "/table/") {
			tables = append(tables, u.Path+" ")
		}
	}
	if n := strings.Count(strings.Join(tables, " "), "/table/server "); n != 1 {
		t.Errorf("/table/server fetched twice: %v", tables)
	}
}

func TestTableRowsCarryTheKeysOfTheJoin(t *testing.T) {
	// Zones and regions expose what a join with the price table needs.
	srv := keepertest.New(t)
	c := keeper.New(srv.URL)
	zones, err := c.Zones(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(zones, func(z keeper.Zone) bool { return z.VendorID == "aws" && z.APIReference != "" })
	if i < 0 {
		t.Fatal("no aws zone")
	}
	z := zones[i]
	if z.ZoneID == "" || z.Name == "" || z.APIReference == "" || z.RegionID == "" || z.Status == "" {
		t.Errorf("zone lacks join keys: %+v", z)
	}
	regions, err := c.Regions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range regions {
		if r.VendorID == "aws" && r.APIReference == "us-east-1" {
			if r.RegionID == "" || r.Status != "active" {
				t.Errorf("region: %+v", r)
			}
			return
		}
	}
	t.Error("aws us-east-1 missing")
}

func TestRegionLocationAndGreenEnergy(t *testing.T) {
	srv := keepertest.New(t)
	regions, err := keeper.New(srv.URL).Regions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var green, notGreen, unknown, withCity, withState, withCoords, withYear int
	for _, r := range regions {
		switch {
		case r.GreenEnergy == nil:
			unknown++
		case *r.GreenEnergy:
			green++
		default:
			notGreen++
		}
		if r.City != nil && *r.City != "" {
			withCity++
		}
		if r.State != nil && *r.State != "" {
			withState++
		}
		if r.Lat != nil && r.Lon != nil {
			withCoords++
		}
		if r.FoundingYear != nil {
			withYear++
		}
	}
	// The three states of green_energy all occur in the snapshot.
	if green == 0 || notGreen == 0 || unknown == 0 || withCity == 0 || withState == 0 || withCoords == 0 || withYear == 0 {
		t.Errorf("green %d, not green %d, unknown %d, city %d, state %d, coordinates %d, year %d",
			green, notGreen, unknown, withCity, withState, withCoords, withYear)
	}
}

func TestRegionDecodesNullableFieldsAndIgnoresUnknownOnes(t *testing.T) {
	var r keeper.Region
	raw := `{"vendor_id":"gcp","region_id":"1","city":"Council Bluffs","state":null,"green_energy":false,"lat":41.5,"lon":-95.8,"founding_year":2009,"new_field":1}`
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	if r.City == nil || *r.City != "Council Bluffs" || r.State != nil || r.GreenEnergy == nil || *r.GreenEnergy ||
		*r.Lat != 41.5 || *r.Lon != -95.8 || *r.FoundingYear != 2009 {
		t.Errorf("%+v", r)
	}
}

func TestUserAgent(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("User-Agent"))
		_ = json.NewEncoder(w).Encode([]string{})
	}))
	defer srv.Close()
	_, _ = keeper.New(srv.URL).Regions(context.Background())
	_, _ = keeper.New(srv.URL, keeper.WithUserAgent("my-tool/1.0")).Regions(context.Background())
	if !slices.Equal(got, []string{keeper.DefaultUserAgent, "my-tool/1.0"}) {
		t.Errorf("User-Agent headers: %v", got)
	}
}

func tableVendorSample(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("keepertest", "testdata", "table_vendor_sample.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestVendors(t *testing.T) {
	var urls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		urls = append(urls, r.URL.String())
		if r.URL.Path == "/healthcheck" {
			_, _ = w.Write([]byte(`{"database_hash":"h1","database_last_updated":1.0}`))
			return
		}
		_, _ = w.Write(tableVendorSample(t))
	}))
	defer srv.Close()

	rows, err := keeper.New(srv.URL).Vendors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 8 || urls[0] != "/table/vendor" {
		t.Fatalf("rows %d, urls %v", len(rows), urls)
	}
	by := map[string]keeper.Vendor{}
	for _, v := range rows {
		by[v.VendorID] = v
	}
	aws, ali := by["aws"], by["alicloud"]
	if aws.Name != "Amazon Web Services" || aws.CountryID != "US" || *aws.FoundingYear != 2002 {
		t.Errorf("aws %+v", aws)
	}
	if *aws.BillingIncrementSeconds != 1 || *aws.MinimumBillingSeconds != 60 || *aws.StoppedServerCharged || aws.BillingComment != nil {
		t.Errorf("aws billing %+v", aws)
	}
	if !*ali.StoppedServerCharged || *ali.MinimumBillingSeconds != 600 || ali.BillingComment == nil {
		t.Errorf("alicloud billing %+v", ali)
	}

	// Through the Fetcher the request is pinned to the data version.
	urls = nil
	if _, err := keeper.NewFetcher(keeper.New(srv.URL)).Vendors(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(urls, "/table/vendor?data_version=h1") {
		t.Errorf("not pinned: %v", urls)
	}

	// The Fetcher memoises the table for the cycle.
	urls = nil
	f := keeper.NewFetcher(keeper.New(srv.URL))
	for range 2 {
		if _, err := f.Vendors(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	vendorCalls := 0
	for _, u := range urls {
		if strings.HasPrefix(u, "/table/vendor") {
			vendorCalls++
		}
	}
	if vendorCalls != 1 {
		t.Errorf("/table/vendor fetched %d times: %v", vendorCalls, urls)
	}
}

func TestVendorUnknownAndIntBooleans(t *testing.T) {
	var rows []keeper.Vendor
	body := `[{"vendor_id":"a","stopped_server_charged":1,"billing_increment_seconds":1},
	 {"vendor_id":"b","stopped_server_charged":0},
	 {"vendor_id":"c","stopped_server_charged":null,"billing_increment_seconds":null},
	 {"vendor_id":"d"}]`
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatal(err)
	}
	if !*rows[0].StoppedServerCharged || *rows[1].StoppedServerCharged {
		t.Errorf("0/1: %+v %+v", rows[0], rows[1])
	}
	if rows[0].BillingIncrementSeconds == nil || rows[0].VendorID != "a" {
		t.Errorf("plain fields lost: %+v", rows[0])
	}
	for _, v := range rows[2:] {
		if v.StoppedServerCharged != nil || v.BillingIncrementSeconds != nil || v.MinimumBillingSeconds != nil || v.BillingComment != nil {
			t.Errorf("%s: unknown must be nil: %+v", v.VendorID, v)
		}
	}
	if err := json.Unmarshal([]byte(`[{"stopped_server_charged":"yes"}]`), &rows); err == nil {
		t.Error("a non-boolean must fail")
	}
}

func TestPriceTiered(t *testing.T) {
	const body = `[{"vendor_id":"v","price":1,"price_tiered":[
	 {"lower":0,"upper":"Infinity","price":0.5},
	 {"lower":0,"upper":730,"price":0.4},
	 {"lower":730.5,"upper":"Infinity","price":0.2}]},
	 {"vendor_id":"v","price":2,"price_tiered":[]}]`
	var rows []keeper.ServerPrice
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatal(err)
	}
	tiers := rows[0].PriceTiered
	if len(tiers) != 3 || !math.IsInf(tiers[0].Upper, 1) || tiers[1].Upper != 730 || tiers[2].Lower != 730.5 || tiers[2].Price != 0.2 {
		t.Errorf("tiers %+v", tiers)
	}
	if len(rows[1].PriceTiered) != 0 {
		t.Errorf("flat price has tiers: %+v", rows[1])
	}
	var zp []keeper.ZonePrice
	if err := json.Unmarshal([]byte(body), &zp); err != nil || len(zp[0].PriceTiered) != 3 {
		t.Errorf("zone price: %v %+v", err, zp)
	}
	if err := json.Unmarshal([]byte(`[{"price_tiered":[{"upper":"bogus"}]}]`), &rows); err == nil {
		t.Error("bad tier must fail")
	}

	var odd keeper.PriceTier
	if err := json.Unmarshal([]byte(`{"lower":"-Infinity","upper":null,"price":"NaN"}`), &odd); err != nil {
		t.Fatal(err)
	}
	if !math.IsInf(odd.Lower, -1) || odd.Upper != 0 || !math.IsNaN(odd.Price) {
		t.Errorf("odd tier %+v", odd)
	}

	// A decoded row encodes again and round-trips.
	raw, err := json.Marshal(tiers)
	if err != nil {
		t.Fatal(err)
	}
	var again []keeper.PriceTier
	if err := json.Unmarshal(raw, &again); err != nil || !slices.Equal(again, tiers) {
		t.Errorf("round trip %s: %v %+v", raw, err, again)
	}
}
