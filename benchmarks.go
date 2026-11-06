/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package keeper

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// ErrUnknownBenchmarkConfig is matched (errors.Is) by every error of
// BenchmarkCatalog.Resolve: unknown benchmark, unknown config, missing config
// where one is required, and rejected benchmarks.
var ErrUnknownBenchmarkConfig = errors.New("unknown benchmark config")

// BenchmarkConfigError describes why a (benchmark id, config) pair could not
// be resolved. It matches ErrUnknownBenchmarkConfig.
type BenchmarkConfigError struct {
	ID     string
	Config string
	Reason string
}

func (e *BenchmarkConfigError) Error() string {
	if e.Config == "" {
		return fmt.Sprintf("benchmark %s: %s", e.ID, e.Reason)
	}
	return fmt.Sprintf("benchmark %s config %s: %s", e.ID, e.Config, e.Reason)
}

// Is makes errors.Is(err, ErrUnknownBenchmarkConfig) true.
func (e *BenchmarkConfigError) Is(target error) bool {
	return target == ErrUnknownBenchmarkConfig
}

// lowerIsBetter lists the benchmarks where a lower score is better (the "↓"
// rows of the benchmark catalog in docs/reference/selectors.md). Keeper's
// /benchmark_configs does not carry the direction.
var lowerIsBetter = map[string]bool{
	"passmark:memory_latency":  true,
	"membench:latency":         true,
	"redis:latency":            true,
	"static_web:latency":       true,
	"compression_text:ratio":   true,
	"nvbandwidth:slot:latency": true,
	"nvbandwidth:p2p:latency":  true,
}

// rejected are benchmarks in the data that are not performance measures.
var rejected = map[string]string{
	"bogomips": "bogomips is not a performance measure",
}

// LowerIsBetter reports whether a lower score of the benchmark is better.
func LowerIsBetter(id string) bool {
	return lowerIsBetter[id]
}

// WorkloadProfileID is the benchmark id of a workload profile.
func WorkloadProfileID(name string) string {
	return "workload_profile:" + name
}

type catalogEntry struct {
	canonical string
	parsed    any
}

// BenchmarkCatalog resolves user-supplied benchmark configs to the canonical
// strings Keeper matches exactly.
type BenchmarkCatalog struct {
	byID map[string][]catalogEntry
}

// NewBenchmarkCatalog indexes the rows of /benchmark_configs.
func NewBenchmarkCatalog(configs []BenchmarkConfig) (*BenchmarkCatalog, error) {
	c := &BenchmarkCatalog{byID: map[string][]catalogEntry{}}
	for _, bc := range configs {
		parsed, err := parseConfig([]byte(bc.Config))
		if err != nil {
			return nil, fmt.Errorf("benchmark_configs: %s %q: %w", bc.BenchmarkID, bc.Config, err)
		}
		c.byID[bc.BenchmarkID] = append(c.byID[bc.BenchmarkID], catalogEntry{canonical: bc.Config, parsed: parsed})
	}
	return c, nil
}

// Has reports whether the catalog knows the benchmark id.
func (c *BenchmarkCatalog) Has(id string) bool {
	return len(c.byID[id]) > 0
}

// Configs returns the canonical config strings of a benchmark, sorted.
func (c *BenchmarkCatalog) Configs(id string) []string {
	out := make([]string, 0, len(c.byID[id]))
	for _, e := range c.byID[id] {
		out = append(out, e.canonical)
	}
	slices.Sort(out)
	return out
}

// Resolve returns the key of a benchmark with a user-supplied config (raw
// JSON; empty or null means "no config"). Configs are compared as parsed
// JSON: key order does not matter, 8.0 equals 8, "8" does not. Without a
// config the benchmark must have exactly one.
func (c *BenchmarkCatalog) Resolve(id string, config []byte) (BenchmarkKey, error) {
	if why, ok := rejected[id]; ok {
		return BenchmarkKey{}, &BenchmarkConfigError{ID: id, Reason: why}
	}
	entries := c.byID[id]
	if len(entries) == 0 {
		return BenchmarkKey{}, &BenchmarkConfigError{ID: id, Reason: "not in /benchmark_configs"}
	}
	trimmed := bytes.TrimSpace(config)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		if len(entries) == 1 {
			return BenchmarkKey{ID: id, Config: entries[0].canonical}, nil
		}
		return BenchmarkKey{}, &BenchmarkConfigError{
			ID:     id,
			Reason: fmt.Sprintf("a config is required, one of %s", strings.Join(c.Configs(id), ", ")),
		}
	}
	want, err := parseConfig(trimmed)
	if err != nil {
		return BenchmarkKey{}, &BenchmarkConfigError{ID: id, Config: string(trimmed), Reason: "not a JSON object: " + err.Error()}
	}
	for _, e := range entries {
		if reflect.DeepEqual(e.parsed, want) {
			return BenchmarkKey{ID: id, Config: e.canonical}, nil
		}
	}
	return BenchmarkKey{}, &BenchmarkConfigError{ID: id, Config: string(trimmed), Reason: "not in /benchmark_configs"}
}

// parseConfig parses a JSON object; numbers become float64 so 8 and 8.0
// compare equal.
func parseConfig(raw []byte) (any, error) {
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	if v == nil {
		return nil, errors.New("null is not an object")
	}
	return v, nil
}
