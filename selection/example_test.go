/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package selection_test

import (
	"context"
	"fmt"

	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/SpareCores/sc-keeper-go/keepertest"
	"github.com/SpareCores/sc-keeper-go/selection"
	"github.com/SpareCores/sc-keeper-go/selection/api"
)

// Select the on-demand AWS instance types with 8 to 16 vCPUs in a region,
// replaying the committed snapshot instead of the public Keeper API
// (use keeper.New("") for the real one).
func Example() {
	srv := keepertest.New(t{}) // a *testing.T in a test
	ctx := context.Background()
	fetcher := keeper.NewFetcher(keeper.New(srv.URL))
	catalog, err := fetcher.BenchmarkCatalog(ctx)
	if err != nil {
		panic(err)
	}

	lo, hi := int32(8), int32(16)
	spec := &api.Spec{Selector: &api.Selector{VCPUs: &api.IntRange{Min: &lo, Max: &hi}}}
	opts := selection.Options{Catalog: catalog}

	queries, err := selection.PlanQueries(spec, "aws", []string{"us-east-1"}, opts)
	if err != nil {
		panic(err)
	}
	results, err := fetcher.ServersAll(ctx, queries)
	if err != nil {
		panic(err)
	}
	res, err := selection.Evaluate(spec, selection.BuildCandidates(results), opts)
	if err != nil {
		panic(err)
	}
	fmt.Println(len(res.InstanceTypes) > 100)
	// Output: true
}

// t is the minimal testing.TB the replay server needs; an example has no
// *testing.T, so it never fails and never cleans up (the process ends).
type t struct{}

func (t) Helper()                   {}
func (t) Fatalf(f string, a ...any) { panic(fmt.Sprintf(f, a...)) }
func (t) Cleanup(func())            {}
