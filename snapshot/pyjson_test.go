/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package snapshot

import "testing"

// The expectations are what Python 3 prints for json.dumps(json.loads(in),
// sort_keys=True, separators=(",", ":")).
func TestEncodeLikePython(t *testing.T) {
	tests := []struct{ in, want string }{
		{`{"b": 1, "a": [1.0, 2.50, 1e-5, 1E5, 123456789012345678901234567890]}`,
			`{"a":[1.0,2.5,1e-05,100000.0,123456789012345678901234567890],"b":1}`},
		{`[0.0001, 0.00001, 1e15, 1e16, 1.5e16, 123456789.123456789, 0.1, 100, -0.0, -0, -2.5e-7]`,
			`[0.0001,1e-05,1000000000000000.0,1e+16,1.5e+16,123456789.12345679,0.1,100,-0.0,0,-2.5e-07]`},
		{`["é", "\u00e9", "😀", "a\"b\\c\n\t\u0001\u007f", "</script>&"]`,
			`["\u00e9","\u00e9","\ud83d\ude00","a\"b\\c\n\t\u0001\u007f","</script>&"]`},
		{`{"z": null, "y": true, "x": false, "w": {}, "v": []}`,
			`{"v":[],"w":{},"x":false,"y":true,"z":null}`},
		{`1.7976931348623157e308`, `1.7976931348623157e+308`},
		{`5e-324`, `5e-324`},
	}
	for _, tc := range tests {
		tree, err := decode([]byte(tc.in))
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		got, err := encode(tree, compactFormat)
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if string(got) != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.in, got, tc.want)
		}
	}
}

func TestEncodeFormats(t *testing.T) {
	tree, err := decode([]byte(`{"b": [1, 2], "a": {"c": {}}, "d": []}`))
	if err != nil {
		t.Fatal(err)
	}
	// json.dumps(..., sort_keys=True) and json.dumps(..., indent=2, sort_keys=True).
	for f, want := range map[format]string{
		sortKeyFormat:  `{"a": {"c": {}}, "b": [1, 2], "d": []}`,
		manifestFormat: "{\n  \"a\": {\n    \"c\": {}\n  },\n  \"b\": [\n    1,\n    2\n  ],\n  \"d\": []\n}",
	} {
		got, err := encode(tree, f)
		if err != nil || string(got) != want {
			t.Errorf("format %+v:\n got %s (%v)\nwant %s", f, got, err, want)
		}
	}
}

func TestDecodeRejectsTrailingData(t *testing.T) {
	if _, err := decode([]byte(`[1] x`)); err == nil {
		t.Error("trailing data accepted")
	}
	if _, err := decode([]byte(`[1,`)); err == nil {
		t.Error("truncated JSON accepted")
	}
}
