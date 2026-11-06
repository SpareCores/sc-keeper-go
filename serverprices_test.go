/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package keeper_test

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/SpareCores/sc-keeper-go/keepertest"
)

func TestServerPricesReplay(t *testing.T) {
	srv := keepertest.New(t)
	f := keeper.NewFetcher(keeper.New(srv.URL))
	ctx := context.Background()
	q := keeper.ServerPricesQuery{
		Vendor: "aws", Regions: []string{"us-west-2", "us-east-1"}, Allocation: keeper.PriceAllocationOnDemand,
	}
	rows, err := f.ServerPrices(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 1000 {
		t.Fatalf("%d rows", len(rows))
	}
	r := rows[0]
	if r.VendorID != "aws" || r.ZoneID == "" || r.ServerID == "" || r.OperatingSystem == "" || r.Price <= 0 || r.Currency != "USD" ||
		r.Allocation != keeper.PriceAllocationOnDemand || r.Unit != "hour" || r.Status == "" || r.ObservedAt == "" {
		t.Errorf("unexpected row: %+v", r)
	}
	spot, err := f.ServerPrices(ctx, keeper.ServerPricesQuery{
		Vendor: "aws", Regions: []string{"us-east-1", "us-west-2"}, Allocation: keeper.PriceAllocationSpot,
	})
	if err != nil || len(spot) == 0 || spot[0].Allocation != keeper.PriceAllocationSpot {
		t.Fatalf("spot: %d rows, %v", len(spot), err)
	}
	before := len(srv.Requests())
	if _, err := f.ServerPrices(ctx, q); err != nil {
		t.Fatal(err)
	}
	if len(srv.Requests()) != before {
		t.Error("second identical query hit the server")
	}
	last := srv.Requests()[len(srv.Requests())-1]
	if last.Path != "/server_prices" || last.Query().Get("limit") != "-1" || last.Query().Get("data_version") == "" {
		t.Errorf("request %s", last)
	}
	// An unrecorded query is a 404.
	if _, err := f.ServerPrices(ctx, keeper.ServerPricesQuery{Vendor: "aws", Regions: []string{"eu-west-1"}}); err == nil {
		t.Error("unrecorded query succeeded")
	}
	if _, err := keeper.New(srv.URL).ServerPrices(ctx, keeper.ServerPricesQuery{}); err == nil {
		t.Error("query without vendor accepted")
	}
}

func TestServerPricesRequestAndGzip(t *testing.T) {
	var urls []string
	var accept []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		urls = append(urls, r.URL.RawQuery)
		accept = append(accept, r.Header.Get("Accept-Encoding"))
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		_, _ = zw.Write([]byte(`[{"vendor_id":"aws","zone_id":"z","server_id":"s","price":1.5,"server":{"vcpus":2}}]`))
		_ = zw.Close()
	}))
	defer srv.Close()
	c := keeper.New(srv.URL, keeper.WithServerPricesRegionsPerRequest(2))
	rows, err := c.ServerPrices(context.Background(), keeper.ServerPricesQuery{
		Vendor: "aws", Regions: []string{"a", "b", "c"}, Currency: "EUR", IncludeInactive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 3 regions in chunks of 2: two requests, one row each.
	if len(urls) != 2 || len(rows) != 2 || rows[0].Price != 1.5 {
		t.Fatalf("%d requests, rows %+v", len(urls), rows)
	}
	for _, a := range accept {
		if a != "gzip" {
			t.Errorf("Accept-Encoding %q", a)
		}
	}
	for _, want := range []string{"currency=EUR", "only_active=false", "limit=-1", "vendor=aws"} {
		if !strings.Contains(urls[0], want) {
			t.Errorf("%s lacks %s", urls[0], want)
		}
	}
}

// bigBody writes n rows with a nested server object of about 4 KB each, like
// Keeper's /server_prices. The rows are prebuilt and reused, so that the
// server side allocates (almost) nothing and the test measures the client.
type bigBody struct {
	n, i  int
	rows  [3][]byte
	buf   []byte
	state int
	comma bool
}

var comma = []byte(",")

func newBigBody(n int) *bigBody {
	b := &bigBody{n: n}
	for z := range b.rows {
		b.rows[z] = fmt.Appendf(nil, `{"vendor_id":"aws","region_id":"r","zone_id":"z%d","server_id":"s","allocation":"spot",`+
			`"unit":"hour","price":0.5,"currency":"USD","status":"active","observed_at":"t","price_tiered":[],`+
			`"server":{"description":"%s","cpus":[{"x":1}]},"vendor":{"a":"b"},"region":{"c":"d"},"zone":{"e":"f"}}`,
			z, strings.Repeat("x", 4000))
	}
	return b
}

func (b *bigBody) Read(p []byte) (int, error) {
	for len(b.buf) == 0 {
		switch {
		case b.state == 0:
			b.buf, b.state = []byte("["), 1
		case b.i < b.n:
			if b.i > 0 && !b.comma {
				b.buf, b.comma = comma, true
				continue
			}
			b.buf, b.comma = b.rows[b.i%3], false
			b.i++
		case b.state == 1:
			b.buf, b.state = []byte("]"), 2
		default:
			return 0, io.EOF
		}
	}
	n := copy(p, b.buf)
	b.buf = b.buf[n:]
	return n, nil
}

// The nested objects are skipped and the body is never held: decoding a body
// of about 80 MB allocates a small fraction of it.
func TestServerPricesStreamsTheBody(t *testing.T) {
	const rows = 20000
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body := newBigBody(rows)
		n, _ := io.Copy(w, body)
		served.Store(n)
	}))
	defer srv.Close()
	c := keeper.New(srv.URL)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, err := c.ServerPrices(context.Background(), keeper.ServerPricesQuery{Vendor: "aws"})
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != rows || got[rows-1].ServerID != "s" || got[5].ZoneID != "z2" {
		t.Fatalf("%d rows, last %+v", len(got), got[len(got)-1])
	}
	body := served.Load()
	alloc := int64(after.TotalAlloc - before.TotalAlloc)
	t.Logf("body %d MB, allocated %d MB", body>>20, alloc>>20)
	if body < 70<<20 {
		t.Fatalf("test body is only %d bytes", body)
	}
	// Buffering the body (or decoding the nested objects) would allocate more
	// than its size; the kept flat rows are about a quarter of it here.
	if alloc > body/2 {
		t.Errorf("allocated %d bytes for a %d byte body", alloc, body)
	}
}
