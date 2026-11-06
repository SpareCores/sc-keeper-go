/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package snapshot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
)

// The snapshot files are written the way Python's json.dumps(sort_keys=True,
// ensure_ascii=True) writes them, which the first (Python) fetcher did: the
// SHA-256 of a response and the committed files stay comparable across the two
// implementations, and the bytes depend on the data only.

// format selects the separators of the encoder.
type format struct {
	item, key string
	// indent is the indentation per level; empty writes one line.
	indent string
}

var (
	// compactFormat is json.dumps(separators=(",", ":")): the data files.
	compactFormat = format{item: ",", key: ":"}
	// sortKeyFormat is json.dumps' default separators; row order is the order
	// of the rows encoded this way.
	sortKeyFormat = format{item: ", ", key: ": "}
	// manifestFormat is json.dump(indent=2).
	manifestFormat = format{item: ",", key: ": ", indent: "  "}
)

// decode parses JSON into a tree of nil, bool, json.Number, string, []any and
// map[string]any, keeping the number tokens.
func decode(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("trailing data after the JSON value")
	}
	return v, nil
}

// encode writes v with sorted keys and ASCII-only strings.
func encode(v any, f format) ([]byte, error) {
	var b bytes.Buffer
	if err := write(&b, v, f, 0); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func write(b *bytes.Buffer, v any, f format, level int) error {
	switch v := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(v))
	case string:
		writeString(b, v)
	case json.Number:
		s, err := pyNumber(v)
		if err != nil {
			return err
		}
		b.WriteString(s)
	case int:
		b.WriteString(strconv.Itoa(v))
	case int64:
		b.WriteString(strconv.FormatInt(v, 10))
	case float64:
		b.WriteString(pyFloat(v))
	case []any:
		return writeContainer(b, f, level, '[', ']', len(v), func(i int) error {
			return write(b, v[i], f, level+1)
		})
	case []string:
		return writeContainer(b, f, level, '[', ']', len(v), func(i int) error {
			writeString(b, v[i])
			return nil
		})
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		return writeContainer(b, f, level, '{', '}', len(keys), func(i int) error {
			writeString(b, keys[i])
			b.WriteString(f.key)
			return write(b, v[keys[i]], f, level+1)
		})
	default:
		// Anything else (the manifest structs) goes through encoding/json
		// first and is then written from the generic tree.
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		tree, err := decode(raw)
		if err != nil {
			return err
		}
		return write(b, tree, f, level)
	}
	return nil
}

func writeContainer(b *bytes.Buffer, f format, level int, open, closing byte, n int, item func(int) error) error {
	b.WriteByte(open)
	if n == 0 {
		b.WriteByte(closing)
		return nil
	}
	for i := range n {
		if i > 0 {
			b.WriteString(f.item)
		}
		if f.indent != "" {
			b.WriteString("\n" + strings.Repeat(f.indent, level+1))
		}
		if err := item(i); err != nil {
			return err
		}
	}
	if f.indent != "" {
		b.WriteString("\n" + strings.Repeat(f.indent, level))
	}
	b.WriteByte(closing)
	return nil
}

// writeString writes a JSON string like Python with ensure_ascii.
func writeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20:
			fmt.Fprintf(b, `\u%04x`, r)
		case r < 0x7f:
			b.WriteRune(r)
		case r < 0x10000:
			fmt.Fprintf(b, `\u%04x`, r)
		default:
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(b, `\u%04x\u%04x`, hi, lo)
		}
	}
	b.WriteByte('"')
}

// pyNumber re-writes a JSON number token as Python does after json.loads:
// integers stay verbatim, everything with a fraction or an exponent becomes
// the repr of a float.
func pyNumber(n json.Number) (string, error) {
	s := n.String()
	if !strings.ContainsAny(s, ".eE") {
		if s == "-0" {
			return "0", nil
		}
		return s, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return "", fmt.Errorf("number %q: %w", s, err)
	}
	return pyFloat(f), nil
}

// pyFloat is Python's repr(float): the shortest digits that round-trip,
// positional notation for decimal exponents -4 to 15, scientific otherwise.
func pyFloat(f float64) string {
	if f == 0 {
		if 1/f < 0 {
			return "-0.0"
		}
		return "0.0"
	}
	sign := ""
	if f < 0 {
		sign = "-"
		f = -f
	}
	// d.ddde±XX with the fewest digits.
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expStr, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expStr)
	digits := strings.Replace(mant, ".", "", 1)
	if exp < -4 || exp >= 16 {
		out := digits[:1]
		if len(digits) > 1 {
			out += "." + digits[1:]
		}
		esign := "+"
		if exp < 0 {
			esign = "-"
			exp = -exp
		}
		return fmt.Sprintf("%s%se%s%02d", sign, out, esign, exp)
	}
	pointAt := exp + 1
	switch {
	case pointAt <= 0:
		return sign + "0." + strings.Repeat("0", -pointAt) + digits
	case pointAt >= len(digits):
		return sign + digits + strings.Repeat("0", pointAt-len(digits)) + ".0"
	default:
		return sign + digits[:pointAt] + "." + digits[pointAt:]
	}
}
