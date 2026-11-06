/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package keepertest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func get(t *testing.T, s *Server, method, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, s.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestReplay(t *testing.T) {
	s := New(t)
	for vendor, region := range map[string]string{"aws": "us-east-1", "gcp": "1000", "hcloud": "4"} {
		if got, ok := s.Manifest().KeeperRegion(vendor); !ok || got != region {
			t.Errorf("KeeperRegion(%s) = %q, %v", vendor, got, ok)
		}
	}
	if _, ok := s.Manifest().KeeperRegion("nope"); ok {
		t.Error("unknown vendor has a region")
	}

	// Push-down parameters and parameter order are ignored; the config string
	// must match exactly.
	code, body := get(t, s, http.MethodGet,
		`/servers?vcpus_min=2&vendor_regions=aws~us-east-1&vendor=aws&best_price_allocation=ONDEMAND_ONLY&benchmark_id=geekbench:score&benchmark_config={"cores":%20"single"}&limit=-1`)
	if code != http.StatusOK || !strings.HasPrefix(body, "[") {
		t.Fatalf("recorded query: %d %.100s", code, body)
	}
	code, body = get(t, s, http.MethodGet,
		`/servers?vendor_regions=aws~us-east-1&vendor=aws&best_price_allocation=ONDEMAND_ONLY&benchmark_id=geekbench:score&benchmark_config={"cores":"single"}`)
	if code != http.StatusNotFound || !strings.Contains(body, "no recorded response") || !strings.Contains(body, "more") {
		t.Errorf("non-canonical config: %d %s", code, body)
	}
	if code, body = get(t, s, http.MethodGet, "/healthcheck"); code != http.StatusOK || !strings.Contains(body, s.Manifest().Keeper.DatabaseHash) {
		t.Errorf("healthcheck: %d %s", code, body)
	}
	if code, _ = get(t, s, http.MethodPost, "/table/zone"); code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", code)
	}
	if n := len(s.Requests()); n != 4 {
		t.Errorf("%d requests recorded", n)
	}
}

func TestBrokenSnapshots(t *testing.T) {
	if _, err := NewHandler(t.TempDir()); err == nil {
		t.Error("missing manifest accepted")
	}
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.json", "{")
	if _, err := NewHandler(dir); err == nil {
		t.Error("broken manifest accepted")
	}
	write("manifest.json", `{"keeper": {"database_last_updated": "yesterday"}, "queries": [
		{"path": "/table/zone", "params": {}, "file": "zone.json.gz"},
		{"path": "/table/region", "params": {}, "file": "missing.json.gz"}]}`)
	write("zone.json.gz", "not gzip")
	h, err := NewHandler(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Handler: h}
	s.Server = httptest.NewServer(h)
	defer s.Close()
	for _, path := range []string{"/table/zone", "/table/region", "/healthcheck"} {
		if code, _ := get(t, s, http.MethodGet, path); code != http.StatusInternalServerError {
			t.Errorf("%s: %d", path, code)
		}
	}
}

func TestMount(t *testing.T) {
	h, err := NewHandler(SnapshotDir())
	if err != nil {
		t.Fatal(err)
	}
	h.Mount("/other/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("mounted " + r.URL.Path))
	}))
	s := NewServer(t, h)
	if code, body := get(t, s, http.MethodGet, "/other/x"); code != http.StatusOK || body != "mounted /other/x" {
		t.Errorf("mounted path: %d %s", code, body)
	}
	if code, _ := get(t, s, http.MethodGet, "/table/zone"); code != http.StatusOK {
		t.Errorf("snapshot path: %d", code)
	}
	if code, _ := get(t, s, http.MethodGet, "/unmounted/x"); code != http.StatusNotFound {
		t.Errorf("unmounted path: %d", code)
	}
}
