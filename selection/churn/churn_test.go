/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package churn

import (
	"math"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/sc-keeper-go/selection"
	"github.com/SpareCores/sc-keeper-go/selection/api"
)

var (
	t0  = time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	inf = math.Inf(1)
)

// result builds an evaluation result: selected names and misses.
func result(selected []string, misses map[string]selection.Miss) selection.Result {
	r := selection.Result{Misses: misses}
	for _, n := range selected {
		r.InstanceTypes = append(r.InstanceTypes, selection.InstanceType{Name: n})
	}
	if r.Misses == nil {
		r.Misses = map[string]selection.Miss{}
	}
	return r
}

func failing(rel float64) selection.Miss {
	return selection.Miss{Reason: "filters.vcpus", Relative: rel}
}

var notOffered = selection.Miss{Reason: selection.ReasonProviderIneligible, NotOffered: true, Relative: inf}

func defaults(now time.Time) Config { return ConfigFromSpec(api.Churn{}, now) }

func TestConfigFromSpec(t *testing.T) {
	c := defaults(t0)
	if c.RemoveMarginPercent != 5 || c.RemoveAfterRefreshes != 2 || c.MaxRemovalsPerRefresh != 3 || !c.Now.Equal(t0) {
		t.Errorf("defaults %+v", c)
	}
	c = ConfigFromSpec(api.Churn{RemoveMarginPercent: new(1.0), RemoveAfterRefreshes: new(int32(0)), MaxRemovalsPerRefresh: new(int32(7))}, t0)
	if c.RemoveMarginPercent != 1 || c.RemoveAfterRefreshes != 0 || c.MaxRemovalsPerRefresh != 7 {
		t.Errorf("explicit %+v", c)
	}
}

func TestAdditionsImmediate(t *testing.T) {
	types, st := Apply(State{Types: []string{"a"}}, result([]string{"c", "a", "b"}, nil), defaults(t0), false)
	if !slices.Equal(types, []string{"a", "b", "c"}) || len(st.Pending) != 0 || !slices.Equal(st.Types, types) {
		t.Errorf("types %v, state %+v", types, st)
	}
	// First evaluation.
	types, _ = Apply(State{}, result([]string{"x"}, nil), defaults(t0), false)
	if !slices.Equal(types, []string{"x"}) {
		t.Errorf("first evaluation: %v", types)
	}
}

func TestRemovalAfterRefreshes(t *testing.T) {
	cfg := defaults(t0)
	next := result([]string{"a"}, map[string]selection.Miss{"b": failing(0.5)})

	types, st := Apply(State{Types: []string{"a", "b"}}, next, cfg, false)
	if !slices.Equal(types, []string{"a", "b"}) || len(st.Pending) != 1 || st.Pending[0].Refreshes != 1 || !st.Pending[0].Since.Equal(t0) {
		t.Fatalf("refresh 1: %v %+v", types, st)
	}
	cfg.Now = t0.Add(24 * time.Hour)
	types, st = Apply(st, next, cfg, false)
	if !slices.Equal(types, []string{"a"}) || len(st.Pending) != 0 {
		t.Fatalf("refresh 2: %v %+v", types, st)
	}

	// A refresh where it qualifies again resets the count.
	_, st = Apply(State{Types: []string{"a", "b"}}, next, defaults(t0), false)
	_, st = Apply(st, result([]string{"a", "b"}, nil), defaults(t0), false)
	if len(st.Pending) != 0 {
		t.Fatalf("requalified: %+v", st)
	}
	types, st = Apply(st, next, defaults(t0), false)
	if !slices.Contains(types, "b") || st.Pending[0].Refreshes != 1 {
		t.Fatalf("count restarted: %v %+v", types, st)
	}

	// removeAfterRefreshes 0 or 1: immediate.
	for _, n := range []int{0, 1} {
		cfg := defaults(t0)
		cfg.RemoveAfterRefreshes = n
		if types, _ := Apply(State{Types: []string{"a", "b"}}, next, cfg, false); !slices.Equal(types, []string{"a"}) {
			t.Errorf("removeAfterRefreshes %d: %v", n, types)
		}
	}
}

func TestMargin(t *testing.T) {
	cfg := defaults(t0)
	cfg.RemoveAfterRefreshes = 1
	prev := State{Types: []string{"in-margin", "edge", "beyond", "missing-data"}}
	next := result(nil, map[string]selection.Miss{
		"in-margin":    failing(0.03),
		"edge":         failing(0.05),
		"beyond":       failing(0.051),
		"missing-data": {Reason: selection.ReasonMissingData, Relative: inf},
	})
	types, st := Apply(prev, next, cfg, false)
	if !slices.Equal(types, []string{"edge", "in-margin"}) || len(st.Pending) != 0 {
		t.Errorf("types %v, state %+v", types, st)
	}
	// A spec change ignores the margin (the cap of 3 keeps the smallest miss).
	types, _ = Apply(prev, next, cfg, true)
	if !slices.Equal(types, []string{"in-margin"}) {
		t.Errorf("spec change: %v", types)
	}
}

func TestRemovalCap(t *testing.T) {
	cfg := defaults(t0)
	cfg.RemoveAfterRefreshes = 1
	prev := State{Types: []string{"a", "b", "c", "d", "e", "gone1", "gone2", "keep"}}
	next := result([]string{"keep"}, map[string]selection.Miss{
		"a": failing(0.2), "b": failing(inf), "c": failing(0.9), "d": failing(0.1), "e": failing(0.9),
		"gone1": notOffered,
		// gone2 is absent from the data altogether.
	})
	types, st := Apply(prev, next, cfg, false)
	// Worst misses first (b, then c and e by name), at most 3; not-offered
	// types are exempt from the cap.
	if !slices.Equal(types, []string{"a", "d", "keep"}) {
		t.Errorf("types %v", types)
	}
	if len(st.Pending) != 2 || st.Pending[0].Name != "a" || st.Pending[1].Name != "d" || st.Pending[0].Refreshes != 1 {
		t.Errorf("pending %+v", st.Pending)
	}
	// Next refresh removes the rest.
	types, st = Apply(st, next, cfg, false)
	if !slices.Equal(types, []string{"keep"}) || len(st.Pending) != 0 {
		t.Errorf("second refresh: %v %+v", types, st)
	}
	// A cap of 0 removes only types that are no longer offered.
	cfg.MaxRemovalsPerRefresh = 0
	types, _ = Apply(prev, next, cfg, false)
	if !slices.Equal(types, []string{"a", "b", "c", "d", "e", "keep"}) {
		t.Errorf("cap 0: %v", types)
	}
}

func TestNotOfferedWaits(t *testing.T) {
	cfg := defaults(t0)
	next := result(nil, map[string]selection.Miss{"gone": notOffered})
	types, st := Apply(State{Types: []string{"gone"}}, next, cfg, false)
	if !slices.Equal(types, []string{"gone"}) || st.Pending[0].Refreshes != 1 {
		t.Errorf("first refresh: %v %+v", types, st)
	}
	types, _ = Apply(st, next, cfg, false)
	if len(types) != 0 {
		t.Errorf("second refresh: %v", types)
	}
}

func TestSpecChangeBypassesHysteresisNotCap(t *testing.T) {
	cfg := defaults(t0)
	prev := State{Types: []string{"a", "b", "c", "d", "e", "f"}}
	misses := map[string]selection.Miss{}
	for _, n := range prev.Types {
		misses[n] = failing(0.01) // within the margin, first failing refresh
	}
	types, st := Apply(prev, result([]string{"new"}, misses), cfg, true)
	if len(types) != 4 || !slices.Contains(types, "new") {
		t.Fatalf("types %v: want 3 removed (cap) and the addition", types)
	}
	if len(st.Pending) != 3 {
		t.Errorf("pending %+v", st.Pending)
	}
}

func TestStateRoundTrip(t *testing.T) {
	st := StateFromStatus([]string{"b", "a"}, []api.PendingRemoval{
		{Name: "b", Since: metav1.NewTime(t0), Refreshes: 1},
		{Name: "a", Since: metav1.NewTime(t0.Add(time.Hour)), Refreshes: 2},
	})
	if !slices.Equal(st.Types, []string{"a", "b"}) || st.Pending[0].Name != "a" || st.Pending[1].Refreshes != 1 {
		t.Fatalf("state %+v", st)
	}
	out := st.PendingRemovals()
	if len(out) != 2 || out[0].Name != "a" || out[0].Refreshes != 2 || !out[1].Since.Time.Equal(t0) {
		t.Errorf("status %+v", out)
	}
	if (State{}).PendingRemovals() != nil {
		t.Error("empty state")
	}
	// The serialised state continues the count.
	cfg := defaults(t0)
	cfg.RemoveAfterRefreshes = 3
	types, next := Apply(st, result(nil, map[string]selection.Miss{"a": failing(1), "b": failing(1)}), cfg, false)
	if !slices.Equal(types, []string{"b"}) || next.Pending[0].Refreshes != 2 || !next.Pending[0].Since.Equal(t0) {
		t.Errorf("continued: %v %+v", types, next)
	}
}
