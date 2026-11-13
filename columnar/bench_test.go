/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package columnar_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/SpareCores/sc-keeper-go/columnar"
)

// priceRow has the columns of a server_price file.
type priceRow struct {
	VendorID        string  `json:"vendor_id"`
	RegionID        string  `json:"region_id"`
	ZoneID          string  `json:"zone_id"`
	ServerID        string  `json:"server_id"`
	OperatingSystem string  `json:"operating_system"`
	Allocation      string  `json:"allocation"`
	Unit            string  `json:"unit"`
	Price           float64 `json:"price"`
	PriceUpfront    float64 `json:"price_upfront"`
	Currency        string  `json:"currency"`
	Status          string  `json:"status"`
}

const benchRows = 20000

// benchBodies builds the same prices as a columnar file and as an array of
// row objects, the shape json.Unmarshal of a row slice reads.
func benchBodies() (columnarBody, rowBody []byte) {
	cols := map[string][]any{}
	rows := make([]map[string]any, 0, benchRows)
	for i := range benchRows {
		r := map[string]any{
			"vendor_id": "aws", "region_id": fmt.Sprintf("region-%d", i%30),
			"zone_id": fmt.Sprintf("region-%d%c", i%30, 'a'+rune(i%3)), "server_id": fmt.Sprintf("m%d.large", i%500),
			"operating_system": "Linux", "allocation": []string{"ondemand", "spot"}[i%2], "unit": "hour",
			"price": 0.01 + float64(i)/1e5, "price_upfront": 0.0, "currency": "USD", "status": "active",
		}
		rows = append(rows, r)
		for k, v := range r {
			cols[k] = append(cols[k], v)
		}
	}
	columnarBody, _ = json.Marshal(cols)
	rowBody, _ = json.Marshal(rows)
	return columnarBody, rowBody
}

func BenchmarkDecodeRows(b *testing.B) {
	body, _ := benchBodies()
	b.ReportAllocs()
	for b.Loop() {
		rows, err := columnar.DecodeRows[priceRow](bytes.NewReader(body), benchRows)
		if err != nil || len(rows) != benchRows {
			b.Fatal(err)
		}
	}
}

// BenchmarkUnmarshalRows is the baseline: the same data as a JSON array of
// row objects through json.Unmarshal.
func BenchmarkUnmarshalRows(b *testing.B) {
	_, body := benchBodies()
	b.ReportAllocs()
	for b.Loop() {
		var rows []priceRow
		if err := json.Unmarshal(body, &rows); err != nil || len(rows) != benchRows {
			b.Fatal(err)
		}
	}
}
