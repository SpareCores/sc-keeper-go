/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package churn implements churn control: additions are
// applied immediately; removals wait for a margin, a number of consecutive
// refreshes, and a per-refresh cap, because removing a type drifts every
// running node of that type.
package churn

import (
	"cmp"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/SpareCores/sc-keeper-go/selection"
	"github.com/SpareCores/sc-keeper-go/selection/api"
)

// Config is spec.churn with defaults applied, plus the evaluation time.
type Config struct {
	// RemoveMarginPercent: a type failing a threshold by at most this
	// percentage is kept.
	RemoveMarginPercent float64
	// RemoveAfterRefreshes: consecutive failing refreshes before removal.
	RemoveAfterRefreshes int
	// MaxRemovalsPerRefresh caps removals of qualifying-but-failing types;
	// types no longer offered are exempt.
	MaxRemovalsPerRefresh int
	// Now stamps new pending removals.
	Now time.Time
}

// ConfigFromSpec applies the defaults of spec.churn.
func ConfigFromSpec(c api.Churn, now time.Time) Config {
	cfg := Config{
		RemoveMarginPercent:   selection.DefaultRemoveMarginPercent,
		RemoveAfterRefreshes:  selection.DefaultRemoveAfterRefreshes,
		MaxRemovalsPerRefresh: selection.DefaultMaxRemovalsPerRefresh,
		Now:                   now,
	}
	if c.RemoveMarginPercent != nil {
		cfg.RemoveMarginPercent = *c.RemoveMarginPercent
	}
	if c.RemoveAfterRefreshes != nil {
		cfg.RemoveAfterRefreshes = int(*c.RemoveAfterRefreshes)
	}
	if c.MaxRemovalsPerRefresh != nil {
		cfg.MaxRemovalsPerRefresh = int(*c.MaxRemovalsPerRefresh)
	}
	return cfg
}

// Pending is a type that no longer qualifies but is still applied.
type Pending struct {
	Name string
	// Since is the first refresh it failed.
	Since time.Time
	// Refreshes is the number of consecutive refreshes it has failed.
	Refreshes int
}

// State is what churn control remembers between refreshes.
type State struct {
	// Types are the applied instance types, sorted.
	Types []string
	// Pending are the applied types waiting for removal, sorted by name.
	Pending []Pending
}

// StateFromStatus rebuilds the state from the applied types and
// status.pendingRemovals.
func StateFromStatus(types []string, pending []api.PendingRemoval) State {
	s := State{Types: slices.Sorted(slices.Values(types))}
	for _, p := range pending {
		s.Pending = append(s.Pending, Pending{Name: p.Name, Since: p.Since.Time, Refreshes: int(p.Refreshes)})
	}
	slices.SortFunc(s.Pending, func(a, b Pending) int { return strings.Compare(a.Name, b.Name) })
	return s
}

// PendingRemovals serialises the pending types for status.pendingRemovals.
func (s State) PendingRemovals() []api.PendingRemoval {
	if len(s.Pending) == 0 {
		return nil
	}
	out := make([]api.PendingRemoval, len(s.Pending))
	for i, p := range s.Pending {
		out[i] = api.PendingRemoval{Name: p.Name, Since: metav1.NewTime(p.Since), Refreshes: int32(p.Refreshes)}
	}
	return out
}

type removal struct {
	pending    Pending
	notOffered bool
	miss       float64
}

// Apply merges the new selection into the applied types. specChanged (a new
// metadata.generation) bypasses the margin and the refresh count, but not
// the removal cap. It returns the types to apply (sorted) and the new state.
func Apply(prev State, next selection.Result, cfg Config, specChanged bool) ([]string, State) {
	selected := map[string]bool{}
	for _, t := range next.InstanceTypes {
		selected[t.Name] = true
	}
	prevPending := map[string]Pending{}
	for _, p := range prev.Pending {
		prevPending[p.Name] = p
	}

	keep := map[string]bool{}
	for name := range selected {
		keep[name] = true
	}
	var due []removal
	var waiting []Pending
	margin := cfg.RemoveMarginPercent / 100
	for _, name := range prev.Types {
		if selected[name] {
			continue
		}
		miss, evaluated := next.Misses[name]
		notOffered := !evaluated || miss.NotOffered
		if !specChanged && !notOffered && miss.Relative <= margin {
			// Within the margin: still counts as qualifying.
			keep[name] = true
			continue
		}
		p, ok := prevPending[name]
		if !ok {
			p = Pending{Name: name, Since: cfg.Now}
		}
		p.Refreshes++
		if specChanged || p.Refreshes >= cfg.RemoveAfterRefreshes {
			due = append(due, removal{pending: p, notOffered: notOffered, miss: miss.Relative})
			continue
		}
		keep[name] = true
		waiting = append(waiting, p)
	}

	// The worst misses go first; not-offered types are exempt from the cap.
	slices.SortFunc(due, func(a, b removal) int {
		if c := cmp.Compare(b.miss, a.miss); c != 0 {
			return c
		}
		return strings.Compare(a.pending.Name, b.pending.Name)
	})
	removed := 0
	for _, r := range due {
		if r.notOffered {
			continue
		}
		if removed < cfg.MaxRemovalsPerRefresh {
			removed++
			continue
		}
		keep[r.pending.Name] = true
		waiting = append(waiting, r.pending)
	}

	types := make([]string, 0, len(keep))
	for name := range keep {
		types = append(types, name)
	}
	slices.Sort(types)
	slices.SortFunc(waiting, func(a, b Pending) int { return strings.Compare(a.Name, b.Name) })
	return types, State{Types: types, Pending: waiting}
}
