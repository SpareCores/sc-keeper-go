/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SpareCores/sc-keeper-go/keepertest"
)

const key = "sc-test-key-0123456789abcdef"

func build(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "sc-snapshot")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func runBin(t *testing.T, bin, url string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...) //nolint:gosec // the binary this test built
	cmd.Env = append(os.Environ()[:0:0], "PATH="+os.Getenv("PATH"), "KEEPER_URL="+url)
	cmd.Env = append(cmd.Env, env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// The round trip through the command: record the snapshot routes of a Keeper
// that needs a key, from a key file or the environment, and get the committed
// recording back.
func TestRoundTrip(t *testing.T) {
	bin := build(t)
	queries := filepath.Join("..", "..", "keepertest", "testdata", "catalog-queries.yaml")
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		env  []string
		args []string
	}{
		"key file":    {nil, []string{"-api-key-file", keyFile}},
		"environment": {[]string{"KEEPER_API_KEY=" + key}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			srv := keepertest.NewCatalog(t)
			srv.RequireAPIKey(key)
			out := filepath.Join(t.TempDir(), "recording")
			output, err := runBin(t, bin, srv.URL, tc.env, append([]string{"-queries", queries, "-out", out}, tc.args...)...)
			if err != nil {
				t.Fatalf("%v\n%s", err, output)
			}
			if strings.Contains(output, key) {
				t.Error("the key is in the output")
			}
			for _, rel := range []string{"snapshot/manifest.json.gz", "snapshot/vendor.json.gz", "snapshot/benchmark_score/hcloud/stress_ng_bestn.json.gz"} {
				got, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(rel)))
				if err != nil {
					t.Fatal(err)
				}
				want, err := os.ReadFile(filepath.Join(keepertest.CatalogDir(), filepath.FromSlash(rel)))
				if err != nil || !bytes.Equal(got, want) {
					t.Errorf("%s differs from the committed recording (%v)", rel, err)
				}
			}
			// The recording replays.
			if _, err := keepertest.NewHandler(out); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestNoKey(t *testing.T) {
	bin := build(t)
	queries := filepath.Join("..", "..", "keepertest", "testdata", "catalog-queries.yaml")
	srv := keepertest.NewCatalog(t)
	out := filepath.Join(t.TempDir(), "recording")
	output, err := runBin(t, bin, srv.URL, nil, "-queries", queries, "-out", out)
	if err == nil || !strings.Contains(output, "the snapshot routes need a Keeper API key") {
		t.Errorf("err = %v\n%s", err, output)
	}
	if n := len(srv.Requests()); n != 0 {
		t.Errorf("%d requests before the failure", n)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("an output directory was made")
	}

	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := runBin(t, bin, srv.URL, nil, "-queries", queries, "-out", out, "-api-key-file", empty); err == nil || !strings.Contains(output, "is empty") {
		t.Errorf("an empty key file: %v\n%s", err, output)
	}
	if output, err := runBin(t, bin, srv.URL, nil, "-queries", queries, "-out", out, "-api-key-file", filepath.Join(t.TempDir(), "missing")); err == nil || !strings.Contains(output, "API key file") {
		t.Errorf("a missing key file: %v\n%s", err, output)
	}
}
