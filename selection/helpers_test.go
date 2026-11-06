/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package selection

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/SpareCores/sc-keeper-go/keepertest"
	"github.com/SpareCores/sc-keeper-go/selection/api"
)

// The snapshot replay server and one Fetcher are shared by all tests of the
// package: responses are decoded once.
var (
	fetcher  *keeper.Fetcher
	catalog  *keeper.BenchmarkCatalog
	manifest *keepertest.Manifest
)

func TestMain(m *testing.M) {
	h, err := keepertest.NewHandler(keepertest.SnapshotDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	srv := httptest.NewServer(h)
	fetcher = keeper.NewFetcher(keeper.New(srv.URL))
	manifest = h.Manifest()
	catalog, err = fetcher.BenchmarkCatalog(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	srv.Close()
	os.Exit(code)
}

func cfg(raw string) *apiextensionsv1.JSON { return &apiextensionsv1.JSON{Raw: []byte(raw)} }

func specOf(sel *api.Selector) *api.Spec {
	return &api.Spec{Selector: sel}
}

func region(t *testing.T, vendor string) string {
	t.Helper()
	r, ok := manifest.KeeperRegion(vendor)
	if !ok {
		t.Fatalf("snapshot has no region for %s", vendor)
	}
	return r
}

// load plans, fetches (from the replay server) and joins the candidates of a
// spec.
func load(t *testing.T, vendor string, spec *api.Spec) []*Candidate {
	t.Helper()
	qs, err := PlanQueries(spec, vendor, []string{region(t, vendor)}, Options{Catalog: catalog})
	if err != nil {
		t.Fatalf("PlanQueries: %v", err)
	}
	results, err := fetcher.ServersAll(context.Background(), qs)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	return BuildCandidates(results)
}

// run loads and evaluates (without provider options).
func run(t *testing.T, vendor string, spec *api.Spec) (Result, []*Candidate) {
	t.Helper()
	cands := load(t, vendor, spec)
	res, err := Evaluate(spec, cands, Options{Catalog: catalog})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	checkCounters(t, res, cands)
	return res, cands
}

// checkCounters: every candidate is either selected or excluded exactly once.
func checkCounters(t *testing.T, res Result, cands []*Candidate) {
	t.Helper()
	e := res.Excluded
	total := e.ProviderIneligible + e.TemplateConstraints + e.MissingData + e.Ranking
	for _, n := range e.Filters {
		total += n
	}
	if total+len(res.InstanceTypes) != len(cands) {
		t.Errorf("selected %d + excluded %d != %d candidates (%+v)", len(res.InstanceTypes), total, len(cands), e)
	}
	if len(res.Misses) != total {
		t.Errorf("%d misses for %d exclusions", len(res.Misses), total)
	}
	sizes := 0
	for _, f := range res.Families {
		sizes += f.Sizes
	}
	if sizes != len(res.InstanceTypes) {
		t.Errorf("family sizes %d != %d types", sizes, len(res.InstanceTypes))
	}
}

func selectedSet(res Result) map[string]InstanceType {
	out := map[string]InstanceType{}
	for _, t := range res.InstanceTypes {
		out[t.Name] = t
	}
	return out
}

func byName(cands []*Candidate) map[string]*Candidate {
	out := map[string]*Candidate{}
	for _, c := range cands {
		out[c.Name()] = c
	}
	return out
}
