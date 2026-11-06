/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package keeper_test

import (
	"context"
	"errors"
	"testing"

	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/SpareCores/sc-keeper-go/keepertest"
)

func snapshotCatalog(t *testing.T) *keeper.BenchmarkCatalog {
	t.Helper()
	srv := keepertest.New(t)
	cat, err := keeper.NewFetcher(keeper.New(srv.URL)).BenchmarkCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

func TestCatalogResolve(t *testing.T) {
	cat := snapshotCatalog(t)
	tests := []struct {
		name   string
		id     string
		config string
		want   string
		errIs  bool
	}{
		{name: "exact", id: "geekbench:score", config: `{"cores": "single"}`, want: `{"cores": "single"}`},
		{name: "compact spelling", id: "geekbench:score", config: `{"cores":"multi"}`, want: `{"cores": "multi"}`},
		{name: "key order and 8 vs 8.0", id: "static_web:rps-extrapolated",
			config: `{"size": "1k", "connections_per_vcpus": 8}`, want: `{"connections_per_vcpus": 8.0, "size": "1k"}`},
		{name: "string is not a number", id: "static_web:rps-extrapolated",
			config: `{"size": "1k", "connections_per_vcpus": "8"}`, errIs: true},
		{name: "unknown config", id: "geekbench:score", config: `{"cores": "dual"}`, errIs: true},
		{name: "config required", id: "geekbench:score", errIs: true},
		{name: "null config required", id: "membench:latency", config: "null", errIs: true},
		{name: "single config implied", id: "stress_ng:bestn", want: `{}`},
		{name: "empty object", id: "workload_profile:web", config: `{}`, want: `{}`},
		{name: "unknown benchmark", id: "nope:nope", errIs: true},
		{name: "rejected benchmark", id: "bogomips", errIs: true},
		{name: "not an object", id: "geekbench:score", config: `[1]`, errIs: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := cat.Resolve(tt.id, []byte(tt.config))
			if tt.errIs {
				if !errors.Is(err, keeper.ErrUnknownBenchmarkConfig) {
					t.Fatalf("want ErrUnknownBenchmarkConfig, got %v", err)
				}
				if err.Error() == "" {
					t.Error("empty message")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if key.ID != tt.id || key.Config != tt.want {
				t.Errorf("got %s, want %s %s", key, tt.id, tt.want)
			}
		})
	}
	if !cat.Has("geekbench:score") || cat.Has("nope") {
		t.Error("Has")
	}
}

func TestCatalogErrors(t *testing.T) {
	if _, err := keeper.NewBenchmarkCatalog([]keeper.BenchmarkConfig{{BenchmarkID: "x", Config: "{"}}); err == nil {
		t.Error("broken config accepted")
	}
	if _, err := keeper.NewBenchmarkCatalog([]keeper.BenchmarkConfig{{BenchmarkID: "x", Config: "null"}}); err == nil {
		t.Error("null config accepted")
	}
}

func TestLowerIsBetter(t *testing.T) {
	for id, want := range map[string]bool{
		"membench:latency": true, "passmark:memory_latency": true, "geekbench:score": false,
		keeper.WorkloadProfileID("web"): false,
	} {
		if got := keeper.LowerIsBetter(id); got != want {
			t.Errorf("LowerIsBetter(%s) = %v", id, got)
		}
	}
}
