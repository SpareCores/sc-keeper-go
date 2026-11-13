/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package columnar decodes the columnar JSON of Keeper's snapshot files: one
// object with one key per column, each an array with one element per row, all
// of the same length.
//
//	{"vendor_id":["aws","aws"],"region_id":["eu-west-1","us-east-1"]}
//
// DecodeRows turns it into a slice of structs without holding the body or a
// row-oriented copy of it.
package columnar

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"unsafe"
)

// internColumns are the columns whose strings repeat over the rows of a file
// (hundreds of thousands of price rows carry a few dozen distinct values);
// the rows of such a column share one string per distinct value.
var internColumns = map[string]bool{
	"vendor_id": true, "region_id": true, "zone_id": true, "server_id": true,
	"storage_id": true, "benchmark_id": true, "resource_type": true,
	"unit": true, "currency": true, "allocation": true, "operating_system": true,
	"status": true, "cpu_architecture": true, "cpu_allocation": true, "country_id": true,
}

// kind says how an element of the column is stored. The common field types
// have a fast path: json.Unmarshal allocates for every call, which is
// hundreds of thousands of calls for the numbers of a price file.
type kind uint8

const (
	kindOther  kind = iota // json.Unmarshal
	kindIntern             // a string shared by the rows with the same value
	kindFloat
	kindInt
	kindBool
	kindFloatPtr
	kindIntPtr
	kindBoolPtr
)

type column struct {
	index int
	kind  kind
}

var fastKinds = map[reflect.Type]kind{
	reflect.TypeFor[float64](): kindFloat, reflect.TypeFor[int](): kindInt, reflect.TypeFor[bool](): kindBool,
	reflect.TypeFor[*float64](): kindFloatPtr, reflect.TypeFor[*int](): kindIntPtr, reflect.TypeFor[*bool](): kindBoolPtr,
}

var plans sync.Map // reflect.Type -> map[string]column

// columnsOf maps the JSON names of the exported fields of struct type t to
// their index: the name of the json tag, or the field name without one.
// Embedded structs are not flattened.
func columnsOf(t reflect.Type) map[string]column {
	if p, ok := plans.Load(t); ok {
		return p.(map[string]column) //nolint:forcetypeassert // only this function stores
	}
	cols := map[string]column{}
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() || f.Anonymous {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		k := fastKinds[f.Type]
		if f.Type.Kind() == reflect.String && internColumns[name] {
			k = kindIntern
		}
		cols[name] = column{index: i, kind: k}
	}
	plans.Store(t, cols)
	return cols
}

// maxPrealloc bounds the rows allocated before they are read.
const maxPrealloc = 1 << 20

// DecodeRows reads one columnar object from r into rows of T.
//
// Each column is matched to a field of T by the name of its json tag and each
// element is decoded into that field as encoding/json would; unknown columns
// are skipped, a field without a column keeps its zero value, and every
// column must have the same number of elements. rows is the expected number
// of rows (the manifest has it) and sizes the result up front; a column of
// another length is an error. With rows < 0 the length is that of the first
// column and the result grows while it is read.
//
// The body is read one element at a time: memory is the rows plus one
// element, never the whole body. json.RawMessage fields keep the bytes of
// their element verbatim; a null element leaves a string, number or bool
// field at its zero value and a pointer field nil. Strings of the identifier
// columns (vendor_id, region_id, server_id, unit, currency, status, ...) are
// shared by the rows that carry the same value.
//
// With rows > 0 at least one column of T must be present, so that an empty or
// foreign object does not pass as rows of zero values. The elements of
// skipped (unknown) columns are only scanned for their extent (strings and
// brackets balanced), not validated as JSON.
func DecodeRows[T any](r io.Reader, rows int) ([]T, error) {
	t := reflect.TypeFor[T]()
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("columnar: row type %s is not a struct", t)
	}
	d := &decoder[T]{br: bufio.NewReaderSize(r, 64<<10), cols: columnsOf(t), n: -1}
	if rows >= 0 {
		// The count comes from a manifest: do not trust it for the allocation.
		d.out = make([]T, min(rows, maxPrealloc))
		d.n = rows
	}
	if err := d.object(); err != nil {
		return nil, err
	}
	if rows > 0 && d.known == 0 {
		return nil, fmt.Errorf("columnar: none of the %d columns of the body is a column of %s", d.columns, t)
	}
	if d.out == nil {
		d.out = []T{}
	}
	return d.out, nil
}

// The decoder finds the element boundaries itself: json.Decoder.Decode of one
// scalar after another allocates an error value per element (its scanner
// reports the byte after a top-level value), which more than doubles the
// allocations of a file of a few hundred thousand rows. Every element is then
// parsed, and validated, by json.Unmarshal.
type decoder[T any] struct {
	br   *bufio.Reader
	cols map[string]column
	out  []T
	n    int // rows of the file: -1 until known
	// known and columns count the columns of the body that are fields of T,
	// and all of them.
	known, columns int
	buf            []byte // the current element
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func (d *decoder[T]) readByte() (byte, error) {
	c, err := d.br.ReadByte()
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return c, err
}

// next returns the next byte that is not JSON whitespace.
func (d *decoder[T]) next() (byte, error) {
	for {
		c, err := d.readByte()
		if err != nil || !isSpace(c) {
			return c, err
		}
	}
}

func (d *decoder[T]) object() error {
	c, err := d.next()
	if err != nil {
		return fmt.Errorf("columnar: %w", err)
	}
	if c != '{' {
		return fmt.Errorf("columnar: body is not a JSON object (starts with %q)", c)
	}
	if c, err = d.next(); err != nil {
		return fmt.Errorf("columnar: %w", err)
	}
	for c != '}' {
		if c != '"' {
			return fmt.Errorf("columnar: object key starts with %q, want a string", c)
		}
		name, err := d.key()
		if err != nil {
			return fmt.Errorf("columnar: object key: %w", err)
		}
		if c, err = d.next(); err != nil {
			return fmt.Errorf("columnar: column %q: %w", name, err)
		} else if c != ':' {
			return fmt.Errorf("columnar: column %q: found %q, want ':'", name, c)
		}
		if err := d.column(name); err != nil {
			return err
		}
		switch c, err = d.next(); {
		case err != nil:
			return fmt.Errorf("columnar: after column %q: %w", name, err)
		case c == ',':
			if c, err = d.next(); err != nil {
				return fmt.Errorf("columnar: after column %q: %w", name, err)
			}
			if c == '}' {
				return errors.New("columnar: trailing comma after the last column")
			}
		case c != '}':
			return fmt.Errorf("columnar: after column %q: found %q, want ',' or '}'", name, c)
		}
	}
	// Only whitespace may follow.
	for {
		c, err := d.br.ReadByte()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return fmt.Errorf("columnar: %w", err)
		case !isSpace(c):
			return fmt.Errorf("columnar: trailing data (%q) after the object", c)
		}
	}
}

// key reads an object key whose opening quote is consumed.
func (d *decoder[T]) key() (string, error) {
	d.buf = append(d.buf[:0], '"')
	if err := d.str(true); err != nil {
		return "", err
	}
	var name string
	err := json.Unmarshal(d.buf, &name)
	return name, err
}

// str reads the rest of a string whose opening quote is consumed, appending
// it to d.buf when keep is set.
func (d *decoder[T]) str(keep bool) error {
	for {
		c, err := d.readByte()
		if err != nil {
			return err
		}
		if keep {
			d.buf = append(d.buf, c)
		}
		switch c {
		case '"':
			return nil
		case '\\':
			if c, err = d.readByte(); err != nil {
				return err
			}
			if keep {
				d.buf = append(d.buf, c)
			}
		}
	}
}

// value reads the rest of an array element that starts with first (consumed),
// into d.buf when keep is set.
func (d *decoder[T]) value(first byte, keep bool) error {
	if keep {
		d.buf = append(d.buf[:0], first)
	}
	switch first {
	case ',', ']', '}', ':':
		return fmt.Errorf("found %q, want a value", first)
	case '"':
		return d.str(keep)
	case '{', '[':
		return d.nested(keep)
	}
	// A number, true, false or null: up to the next delimiter.
	for {
		c, err := d.readByte()
		if err != nil {
			return err
		}
		if c == ',' || c == ']' || c == '}' || isSpace(c) {
			return d.br.UnreadByte()
		}
		if keep {
			d.buf = append(d.buf, c)
		}
	}
}

// nested reads the rest of an object or array whose first byte is consumed.
func (d *decoder[T]) nested(keep bool) error {
	for depth := 1; depth > 0; {
		c, err := d.readByte()
		if err != nil {
			return err
		}
		if keep {
			d.buf = append(d.buf, c)
		}
		switch c {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case '"':
			if err := d.str(keep); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *decoder[T]) column(name string) error {
	c, err := d.next()
	if err != nil {
		return fmt.Errorf("columnar: column %q: %w", name, err)
	}
	if c != '[' {
		return fmt.Errorf("columnar: column %q is not an array", name)
	}
	col, known := d.cols[name]
	d.columns++
	if known {
		d.known++
	}
	var interned map[string]string
	if known && col.kind == kindIntern {
		interned = map[string]string{}
	}
	if c, err = d.next(); err != nil {
		return fmt.Errorf("columnar: column %q: %w", name, err)
	}
	i := 0
	for ; c != ']'; i++ {
		switch {
		case d.n >= 0 && i >= d.n:
			return fmt.Errorf("columnar: column %q has more than %d rows", name, d.n)
		case known && i >= len(d.out): // the first column of a file of unknown length
			var zero T
			d.out = append(d.out, zero)
		}
		if err := d.value(c, known); err != nil {
			return fmt.Errorf("columnar: column %q row %d: %w", name, i, err)
		}
		if known {
			if err := d.set(&d.out[i], col, interned); err != nil {
				return fmt.Errorf("columnar: column %q row %d: %w", name, i, err)
			}
		}
		switch c, err = d.next(); {
		case err != nil:
			return fmt.Errorf("columnar: column %q row %d: %w", name, i, err)
		case c == ',':
			if c, err = d.next(); err != nil {
				return fmt.Errorf("columnar: column %q row %d: %w", name, i, err)
			}
			if c == ']' {
				return fmt.Errorf("columnar: column %q: trailing comma", name)
			}
		case c != ']':
			return fmt.Errorf("columnar: column %q row %d: found %q, want ',' or ']'", name, i, c)
		}
	}
	if d.n < 0 {
		d.n = i
		if !known {
			d.out = make([]T, i)
		}
	}
	if i != d.n {
		return fmt.Errorf("columnar: column %q has %d rows, want %d", name, i, d.n)
	}
	return nil
}

// set decodes the element in d.buf into the field of the row.
func (d *decoder[T]) set(row *T, col column, interned map[string]string) error {
	field := reflect.ValueOf(row).Elem().Field(col.index)
	if col.kind == kindOther {
		return json.Unmarshal(d.buf, field.Addr().Interface())
	}
	if string(d.buf) == "null" {
		return nil
	}
	if col.kind != kindIntern {
		if d.setScalar(field, col.kind) {
			return nil
		}
		// Not what the fast path takes (a bad value too): the error is
		// json.Unmarshal's.
		return json.Unmarshal(d.buf, field.Addr().Interface())
	}
	key := d.buf
	if n := len(key); n >= 2 && key[0] == '"' && key[n-1] == '"' && bytes.IndexByte(key, '\\') < 0 {
		key = key[1 : n-1]
	} else {
		var s string
		if err := json.Unmarshal(key, &s); err != nil {
			return err
		}
		key = []byte(s)
	}
	s, ok := interned[string(key)] // a lookup does not allocate
	if !ok {
		s = string(key)
		interned[s] = s
	}
	field.SetString(s)
	return nil
}

// setScalar stores d.buf in a float64, int or bool field (or the pointer to
// one) when it is a plain JSON number or boolean; it reports false for
// anything else.
func (d *decoder[T]) setScalar(field reflect.Value, k kind) bool {
	switch k {
	case kindBool, kindBoolPtr:
		var b bool
		switch string(d.buf) {
		case "true":
			b = true
		case "false":
		default:
			return false
		}
		if k == kindBool {
			field.SetBool(b)
		} else {
			field.Set(reflect.ValueOf(&b))
		}
	case kindFloat, kindFloatPtr:
		if !validNumber(d.buf) {
			return false
		}
		f, err := strconv.ParseFloat(unsafe.String(&d.buf[0], len(d.buf)), 64)
		if err != nil {
			return false
		}
		if k == kindFloat {
			field.SetFloat(f)
		} else {
			field.Set(reflect.ValueOf(&f))
		}
	case kindInt, kindIntPtr:
		if !validNumber(d.buf) || bytes.ContainsAny(d.buf, ".eE") {
			return false
		}
		n, err := strconv.ParseInt(unsafe.String(&d.buf[0], len(d.buf)), 10, strconv.IntSize)
		if err != nil {
			return false
		}
		if k == kindInt {
			field.SetInt(n)
		} else {
			v := int(n)
			field.Set(reflect.ValueOf(&v))
		}
	default:
		return false
	}
	return true
}

// The strings handed to strconv above alias d.buf: strconv does not keep its
// argument (its errors copy it), and the bytes are not touched meanwhile.

// validNumber reports whether b is a number in JSON's grammar.
func validNumber(b []byte) bool {
	i := 0
	if i < len(b) && b[i] == '-' {
		i++
	}
	digits := func() int {
		start := i
		for i < len(b) && b[i] >= '0' && b[i] <= '9' {
			i++
		}
		return i - start
	}
	switch {
	case i < len(b) && b[i] == '0':
		i++
	case digits() == 0:
		return false
	}
	if i < len(b) && b[i] == '.' {
		i++
		if digits() == 0 {
			return false
		}
	}
	if i < len(b) && (b[i] == 'e' || b[i] == 'E') {
		i++
		if i < len(b) && (b[i] == '+' || b[i] == '-') {
			i++
		}
		if digits() == 0 {
			return false
		}
	}
	return i == len(b)
}
