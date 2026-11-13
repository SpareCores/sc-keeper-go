/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package columnar_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"
	"unsafe"

	"github.com/SpareCores/sc-keeper-go/columnar"
)

type tier struct {
	Lower float64 `json:"lower"`
	Upper any     `json:"upper"`
	Price float64 `json:"price"`
}

type row struct {
	VendorID string            `json:"vendor_id"`
	Name     string            `json:"name"`
	Score    *float64          `json:"score"`
	Count    int               `json:"count"`
	Flag     *bool             `json:"flag"`
	Config   json.RawMessage   `json:"config"`
	Tiers    []tier            `json:"price_tiered"`
	Aliases  []string          `json:"aliases"`
	Extra    map[string]string `json:"extra"`
	Skipped  string            `json:"-"`
}

const sample = `{
 "vendor_id":["aws","aws","hcloud"],
 "name":["a","bé\"c",null],
 "unknown":[{"x":[1,2]},null,"s"],
 "score":[1.5,null,3],
 "count":[1,2,3],
 "flag":[true,null,false],
 "config":[{"cores": "single"},{},null],
 "price_tiered":[[{"lower":0.0,"upper":"Infinity","price":0.5}],[],null],
 "aliases":[["x","y"],[],["z"]],
 "extra":[{"k":"v"},null,{}]
}`

func TestDecodeRows(t *testing.T) {
	rows, err := columnar.DecodeRows[row](strings.NewReader(sample), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("%d rows", len(rows))
	}
	if rows[0].VendorID != "aws" || rows[2].VendorID != "hcloud" {
		t.Errorf("vendor_id: %+v", rows)
	}
	if rows[1].Name != "bé\"c" || rows[2].Name != "" {
		t.Errorf("name: %q, %q", rows[1].Name, rows[2].Name)
	}
	if rows[0].Score == nil || *rows[0].Score != 1.5 || rows[1].Score != nil || *rows[2].Score != 3 {
		t.Errorf("score: %v %v %v", rows[0].Score, rows[1].Score, rows[2].Score)
	}
	if rows[2].Flag == nil || *rows[2].Flag || rows[1].Flag != nil {
		t.Errorf("flag: %v", rows[1].Flag)
	}
	// The element bytes are verbatim, inner whitespace included.
	if got := string(rows[0].Config); got != `{"cores": "single"}` {
		t.Errorf("config = %s", got)
	}
	if got := string(rows[2].Config); got != "null" && got != "" {
		t.Errorf("null config = %q", got)
	}
	if len(rows[0].Tiers) != 1 || rows[0].Tiers[0].Upper != "Infinity" || rows[0].Tiers[0].Price != 0.5 {
		t.Errorf("price_tiered = %+v", rows[0].Tiers)
	}
	if rows[1].Tiers == nil || len(rows[1].Tiers) != 0 || rows[2].Tiers != nil {
		t.Errorf("empty and null tiers: %#v %#v", rows[1].Tiers, rows[2].Tiers)
	}
	if len(rows[0].Aliases) != 2 || rows[2].Aliases[0] != "z" || rows[0].Extra["k"] != "v" {
		t.Errorf("aliases/extra: %+v", rows[0])
	}
}

func TestDecodeRowsUnknownRows(t *testing.T) {
	for _, tc := range []struct {
		name, body string
	}{
		{"known first", `{"count":[1,2],"unknown":[1,2]}`},
		{"unknown first", `{"unknown":[1,2],"count":[1,2]}`},
	} {
		rows, err := columnar.DecodeRows[row](strings.NewReader(tc.body), -1)
		if err != nil || len(rows) != 2 || rows[1].Count != 2 {
			t.Errorf("%s: %+v, %v", tc.name, rows, err)
		}
	}
}

func TestDecodeRowsEmpty(t *testing.T) {
	// A score file of an unknown benchmark: every column is an empty array.
	body := `{"vendor_id":[],"name":[],"unknown":[],"score":[]}`
	for _, n := range []int{0, -1} {
		rows, err := columnar.DecodeRows[row](strings.NewReader(body), n)
		if err != nil || rows == nil || len(rows) != 0 {
			t.Errorf("rows=%d: %#v, %v", n, rows, err)
		}
	}
	rows, err := columnar.DecodeRows[row](strings.NewReader(`{}`), -1)
	if err != nil || len(rows) != 0 {
		t.Errorf("empty object: %v, %v", rows, err)
	}
}

func TestDecodeRowsMissingColumn(t *testing.T) {
	rows, err := columnar.DecodeRows[row](strings.NewReader(`{"vendor_id":["aws"]}`), 1)
	if err != nil || rows[0].VendorID != "aws" || rows[0].Name != "" || rows[0].Score != nil {
		t.Errorf("%+v, %v", rows, err)
	}
}

func TestDecodeRowsErrors(t *testing.T) {
	tests := []struct {
		name, body string
		rows       int
		want       string
	}{
		{"short column", `{"vendor_id":["a","b"],"count":[1]}`, -1, `column "count" has 1 rows, want 2`},
		{"long column", `{"vendor_id":["a"],"count":[1,2]}`, -1, `column "count" has more than 1 rows`},
		{"short unknown column", `{"vendor_id":["a","b"],"unknown":[1]}`, -1, `column "unknown" has 1 rows, want 2`},
		{"long unknown column", `{"vendor_id":["a"],"unknown":[1,2]}`, -1, `column "unknown" has more than 1 rows`},
		{"manifest says more", `{"vendor_id":["a","b"]}`, 3, `column "vendor_id" has 2 rows, want 3`},
		{"manifest says fewer", `{"vendor_id":["a","b"]}`, 1, `more than 1 rows`},
		{"not an object", `[1]`, -1, "not a JSON object"},
		{"not an array", `{"vendor_id":"aws"}`, -1, `column "vendor_id" is not an array`},
		{"wrong type", `{"count":["x"]}`, -1, `column "count" row 0`},
		{"wrong interned type", `{"vendor_id":[1]}`, -1, `column "vendor_id" row 0`},
		{"trailing data", `{"count":[1]} {}`, -1, "trailing"},
		{"truncated in a column", `{"count":[1,2`, -1, "unexpected EOF"},
		{"truncated in an element", `{"name":["abc`, -1, "unexpected EOF"},
		{"truncated after a column", `{"count":[1,2]`, -1, "unexpected EOF"},
		{"truncated unknown", `{"unknown":[1,`, -1, "unexpected EOF"},
		{"empty", ``, -1, "unexpected EOF"},
		{"trailing comma in a column", `{"count":[1,2,]}`, -1, "trailing comma"},
		{"trailing comma in an unknown column", `{"unknown":[1,2,]}`, -1, "trailing comma"},
		{"trailing comma in the object", `{"count":[1],}`, -1, "trailing comma"},
		{"empty object for rows", `{}`, 3, "none of the 0 columns"},
		{"only foreign columns for rows", `{"unknown":[1,2,3]}`, 3, "none of the 1 columns"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := columnar.DecodeRows[row](strings.NewReader(tc.body), tc.rows)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	if _, err := columnar.DecodeRows[int](strings.NewReader(`{}`), -1); err == nil {
		t.Error("a non-struct row type is accepted")
	}
	boom := errors.New("boom")
	r := io.MultiReader(strings.NewReader(`{"count":[1,`), iotest.ErrReader(boom))
	if _, err := columnar.DecodeRows[row](r, -1); !errors.Is(err, boom) {
		t.Errorf("reader error: %v", err)
	}
}

func TestDecodeRowsInterns(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"vendor_id":[`)
	for i := range 100 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"aws"`)
	}
	b.WriteString(`],"name":[`)
	for i := range 100 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"same"`)
	}
	b.WriteString(`]}`)
	rows, err := columnar.DecodeRows[row](strings.NewReader(b.String()), 100)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(rows); i++ {
		if unsafe.StringData(rows[i].VendorID) != unsafe.StringData(rows[0].VendorID) {
			t.Fatalf("row %d has its own vendor_id string", i)
		}
	}
	// name is not an identifier column: nothing promises sharing, only equality.
	if rows[99].Name != "same" {
		t.Errorf("name = %q", rows[99].Name)
	}
}

func TestDecodeRowsInternedEscapes(t *testing.T) {
	rows, err := columnar.DecodeRows[row](strings.NewReader(`{"vendor_id":["abc","abc",null]}`), 3)
	if err != nil || rows[0].VendorID != "abc" || rows[1].VendorID != "abc" || rows[2].VendorID != "" {
		t.Errorf("%+v, %v", rows, err)
	}
}

// The memory is the rows plus one element: a 20 MB column of an unknown
// name is skipped without being held.
func TestDecodeRowsStreams(t *testing.T) {
	const n = 200000
	pad := strings.Repeat("x", 100)
	var sent int
	pr, pw := io.Pipe()
	go func() {
		w := bufio.NewWriter(pw)
		_, _ = w.WriteString(`{"big":[`)
		for i := range n {
			if i > 0 {
				_ = w.WriteByte(',')
			}
			_, _ = w.WriteString(`"`)
			_, _ = w.WriteString(pad)
			_, _ = w.WriteString(`"`)
			sent += len(pad) + 3
		}
		_, _ = w.WriteString(`],"count":[`)
		for i := range n {
			if i > 0 {
				_ = w.WriteByte(',')
			}
			_, _ = w.WriteString("7")
		}
		_, _ = w.WriteString(`]}`)
		pw.CloseWithError(w.Flush())
	}()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	rows, err := columnar.DecodeRows[row](pr, n)
	runtime.ReadMemStats(&after)
	if err != nil || len(rows) != n || rows[n-1].Count != 7 {
		t.Fatalf("%d rows, %v", len(rows), err)
	}
	// The rows themselves are n*sizeof(row); the body was over 20 MB.
	rowBytes := uint64(n) * uint64(unsafe.Sizeof(row{}))
	if used := after.TotalAlloc - before.TotalAlloc; used > rowBytes+8<<20 {
		t.Errorf("allocated %d MB for %d MB of rows and a %d MB body", used>>20, rowBytes>>20, sent>>20)
	}
}

// A manifest's row count is not trusted for the allocation.
func TestDecodeRowsHugeCount(t *testing.T) {
	_, err := columnar.DecodeRows[row](strings.NewReader(`{"count":[1,2]}`), 1<<62)
	if err == nil || !strings.Contains(err.Error(), "has 2 rows, want") {
		t.Errorf("err = %v", err)
	}
	rows, err := columnar.DecodeRows[row](strings.NewReader(`{"count":[1,2]}`), 2)
	if err != nil || len(rows) != 2 {
		t.Errorf("%v, %v", rows, err)
	}
}

func TestDecodeRowsBeyondPrealloc(t *testing.T) {
	const n = 1<<20 + 5
	body := `{"count":[` + strings.Repeat("1,", n-1) + `1]}`
	rows, err := columnar.DecodeRows[row](strings.NewReader(body), n)
	if err != nil || len(rows) != n || rows[n-1].Count != 1 {
		t.Fatalf("%d rows, %v", len(rows), err)
	}
	if _, err := columnar.DecodeRows[row](strings.NewReader(body), n+1); err == nil {
		t.Error("a count one too high is accepted")
	}
}
