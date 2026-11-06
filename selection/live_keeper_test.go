//go:build livekeeper

/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package selection_test

// The selector contract against the live Keeper API: every selector field,
// with ordinary and edge values the CRD admits, on every vendor of the
// snapshot. For each case:
//
//   - every query the controller plans must be accepted by Keeper (no 4xx:
//     wrong types, out-of-range values, unknown enums);
//   - the push-down must not change the outcome: the selection evaluated on
//     the pushed-down responses equals the selection evaluated on the same
//     queries without push-down filters (a push-down stricter than the
//     client, say a unit mismatch, would drop types silently).
//
// It needs network access and takes minutes (anonymous rate limit):
//
//   go test -tags livekeeper -run TestLiveKeeper -timeout 60m ./selection/
//
// KEEPER_URL overrides the API, KEEPER_VENDORS limits the vendors
// (comma-separated), KEEPER_DEBUG=1 prints every query with its row count.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	keeper "github.com/SpareCores/sc-keeper-go"
	"github.com/SpareCores/sc-keeper-go/keepertest"
	"github.com/SpareCores/sc-keeper-go/selection"
	"github.com/SpareCores/sc-keeper-go/selection/api"
)

// liveCases are pool specs (selector, ranking, churn), one concern each.
var liveCases = map[string]string{
	"nothing":                   `{}`,
	"instance-type globs":       `{selector: {instanceTypes: {include: ["*.large", "*.xlarge", "*-standard-*", "Standard_D*", "cx*", "cpx*"], exclude: ["*.metal*"]}}}`,
	"family globs":              `{selector: {families: {include: ["m*", "c*", "D*", "ecs.g*"], exclude: ["t*"]}}}`,
	"vcpus range":               `{selector: {vcpus: {min: 2, max: 16}}}`,
	"vcpus exact":               `{selector: {vcpus: {min: 2, max: 2}}}`,
	"vcpus min only":            `{selector: {vcpus: {min: 64}}}`,
	"vcpus above keeper":        `{selector: {vcpus: {min: 300}}}`,
	"vcpus max 0":               `{selector: {vcpus: {max: 0}}}`,
	"vcpus min 0":               `{selector: {vcpus: {min: 0, max: 4}}}`,
	"architectures":             `{selector: {architectures: [arm64]}}`,
	"architectures both":        `{selector: {architectures: [amd64, arm64]}}`,
	"cpu manufacturers":         `{selector: {cpu: {manufacturers: [AMD, Intel]}}}`,
	"cpu allocations":           `{selector: {cpu: {allocations: [Dedicated]}}}`,
	"cpu clock":                 `{selector: {cpu: {clockSpeedMinGHz: 3.1}}}`,
	"cpu clock 0":               `{selector: {cpu: {clockSpeedMinGHz: 0}}}`,
	"hyperthreading on":         `{selector: {cpu: {hyperthreading: true}}}`,
	"hyperthreading off":        `{selector: {cpu: {hyperthreading: false}}}`,
	"cpu flags":                 `{selector: {cpu: {flags: {allOf: [avx2], anyOf: [avx512f, sve], noneOf: [hypervisor_unknown]}}}}`,
	"l1d cache":                 `{selector: {cpu: {caches: {l1dPerCoreMinKiB: 48}}}}`,
	"l1i cache":                 `{selector: {cpu: {caches: {l1iPerCoreMinKiB: 32}}}}`,
	"l2 cache":                  `{selector: {cpu: {caches: {l2PerCoreMinKiB: 1024}}}}`,
	"l2 cache odd":              `{selector: {cpu: {caches: {l2PerCoreMinKiB: 1250.5}}}}`,
	"l3 cache":                  `{selector: {cpu: {caches: {l3MinMiB: 32}}}}`,
	"l3 cache fraction":         `{selector: {cpu: {caches: {l3MinMiB: 0.5}}}}`,
	"l3 cache total":            `{selector: {cpu: {caches: {l3TotalMinMiB: 96}}}}`,
	"all caches":                `{selector: {cpu: {caches: {l1dPerCoreMinKiB: 32, l1iPerCoreMinKiB: 32, l2PerCoreMinKiB: 512, l3MinMiB: 8, l3TotalMinMiB: 8}}}}`,
	"memory range":              `{selector: {memory: {minGiB: 3.5, maxGiB: 64}}}`,
	"memory max 0":              `{selector: {memory: {maxGiB: 0}}}`,
	"memory min 0":              `{selector: {memory: {minGiB: 0, maxGiB: 8}}}`,
	"memory per vcpu":           `{selector: {memory: {perVcpu: {minGiB: 4, maxGiB: 8}}}}`,
	"memory per vcpu max 0":     `{selector: {memory: {perVcpu: {maxGiB: 0}}}}`,
	"memory per vcpu odd":       `{selector: {memory: {perVcpu: {minGiB: 1.75, maxGiB: 2.25}}}}`,
	"gpu none":                  `{selector: {accelerator: {count: {min: 0, max: 0}}}}`,
	"gpu some":                  `{selector: {accelerator: {count: {min: 1, max: 4}}}}`,
	"gpu types":                 `{selector: {accelerator: {types: [GPU], manufacturers: [NVIDIA]}}}`,
	"gpu models":                `{selector: {accelerator: {models: [L4, L40S, T4, A10G]}}}`,
	"gpu memory":                `{selector: {accelerator: {memoryPerDevice: {minGiB: 16}, memoryTotal: {minGiB: 24}}}}`,
	"network":                   `{selector: {network: {baselineMinGbps: 10, peakMinGbps: 25}}}`,
	"network odd":               `{selector: {network: {baselineMinGbps: 0.75, peakMinGbps: 12.5}}}`,
	"storage size":              `{selector: {storage: {local: {minGB: 200}}}}`,
	"storage count 1":           `{selector: {storage: {local: {countMin: 1}}}}`,
	"storage count 2":           `{selector: {storage: {local: {countMin: 2}}}}`,
	"storage size and count":    `{selector: {storage: {local: {minGB: 100, countMin: 1}}}}`,
	"price max":                 `{selector: {price: {maxHourly: "0.40"}}}`,
	"price max tiny":            `{selector: {price: {maxHourly: "0.001"}}}`,
	"price max 0":               `{selector: {price: {maxHourly: "0"}}}`,
	"price per vcpu":            `{selector: {price: {maxHourlyPerVcpu: "0.05"}}}`,
	"price spot":                `{selector: {price: {basis: Spot, maxHourly: "0.30"}}}`,
	"benchmark min":             `{selector: {benchmarks: [{id: "geekbench:score", config: {cores: single}, min: 1500}]}}`,
	"benchmark min odd":         `{selector: {benchmarks: [{id: "stress_ng:best1", min: 1234.567}]}}`,
	"benchmark max":             `{selector: {benchmarks: [{id: "passmark:memory_latency", max: 60}]}}`,
	"benchmark per vcpu":        `{selector: {benchmarks: [{id: "stress_ng:bestn", min: 1500, normalize: PerVcpu}]}}`,
	"benchmark per price":       `{selector: {benchmarks: [{id: "stress_ng:bestn", min: 20000, normalize: PerPrice}]}}`,
	"benchmark per price spot":  `{selector: {price: {basis: Spot}, benchmarks: [{id: "stress_ng:bestn", min: 20000, normalize: PerPrice}]}}`,
	"benchmark config":          `{selector: {benchmarks: [{id: "membench:bandwidth_read", config: {size_kb: 1024}, min: 100000}]}}`,
	"benchmark static web":      `{selector: {benchmarks: [{id: "static_web:rps-extrapolated", config: {connections_per_vcpus: 8.0, size: "1k"}, min: 20000}]}}`,
	"two benchmarks":            `{selector: {benchmarks: [{id: "geekbench:score", config: {cores: single}, min: 1500}, {id: "geekbench:score", config: {cores: single}, min: 1800}, {id: "stress_ng:bestn", min: 3000}]}}`,
	"benchmark include":         `{selector: {benchmarks: [{id: "geekbench:score", config: {cores: single}, min: 1500}], unbenchmarked: Include}}`,
	"benchmark family":          `{selector: {benchmarks: [{id: "geekbench:score", config: {cores: single}, min: 1500}], unbenchmarked: IncludeIfFamilyQualifies}}`,
	"workload profile":          `{selector: {workloadProfiles: [{name: web, min: 1.5}]}}`,
	"workload profile per vcpu": `{selector: {workloadProfiles: [{name: compute, min: 0.5, normalize: PerVcpu}]}}`,
	"ranking score":             `{ranking: {score: {benchmark: {id: "geekbench:score", config: {cores: single}}}, objective: Score, band: {withinPercent: 10}, families: {min: 2, max: 4}}}`,
	"ranking per price":         `{ranking: {score: {benchmark: {id: "stress_ng:bestn"}}, objective: ScorePerPrice, families: {max: 3}}}`,
	"ranking price":             `{ranking: {score: {workloadProfile: web}, objective: Price, band: {withinPercent: 20}}}`,
	"ranking per vcpu":          `{ranking: {score: {benchmark: {id: "stress_ng:bestn"}}, objective: Score, normalize: PerVcpu, families: {max: 2}}}`,
	"ranking lower better":      `{ranking: {score: {benchmark: {id: "passmark:memory_latency"}}, objective: Score, band: {withinPercent: 15}}}`,
	"demo gilty":                `{selector: {vcpus: {min: 2, max: 4}, cpu: {caches: {l2PerCoreMinKiB: 1024}}, price: {maxHourly: "0.25"}}, ranking: {score: {benchmark: {id: "geekbench:score", config: {cores: single}}}, objective: Score, band: {withinPercent: 10}, families: {min: 2, max: 4}}}`,
	"demo batch":                `{selector: {vcpus: {min: 2, max: 8}, price: {basis: Spot}}, ranking: {score: {benchmark: {id: "stress_ng:bestn"}}, objective: ScorePerPrice, families: {max: 3}}}`,
	"everything":                `{selector: {vcpus: {min: 2, max: 32}, architectures: [amd64], cpu: {manufacturers: [AMD, Intel], allocations: [Dedicated], clockSpeedMinGHz: 2.5, hyperthreading: true, caches: {l2PerCoreMinKiB: 1024, l3MinMiB: 16}}, memory: {minGiB: 4, maxGiB: 128, perVcpu: {minGiB: 2, maxGiB: 8}}, accelerator: {count: {max: 0}}, network: {baselineMinGbps: 1}, price: {maxHourly: "2", maxHourlyPerVcpu: "0.1"}, benchmarks: [{id: "stress_ng:best1", min: 1000}]}, ranking: {score: {workloadProfile: compute}, objective: ScorePerPrice, band: {withinPercent: 15}, families: {min: 2}}}`,
	"churn margin 0":            `{selector: {vcpus: {min: 2, max: 8}, cpu: {caches: {l2PerCoreMinKiB: 1024}}, memory: {minGiB: 8}}, churn: {removeMarginPercent: 0}}`,
	"churn margin 50":           `{selector: {vcpus: {min: 2, max: 8}, cpu: {caches: {l2PerCoreMinKiB: 1024}}, memory: {minGiB: 8}}, churn: {removeMarginPercent: 50}}`,
}

func TestLiveKeeper(t *testing.T) {
	url := os.Getenv("KEEPER_URL")
	if url == "" {
		url = "https://keeper.sparecores.net"
	}
	manifest, err := keepertest.LoadManifest(keepertest.SnapshotDir())
	if err != nil {
		t.Fatal(err)
	}
	vendors := make([]string, 0, len(manifest.Vendors))
	for v := range manifest.Vendors {
		vendors = append(vendors, v)
	}
	if only := os.Getenv("KEEPER_VENDORS"); only != "" {
		vendors = strings.Split(only, ",")
	}
	slices.Sort(vendors)

	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Minute)
	defer cancel()
	client := keeper.New(url, keeper.WithRetries(8, 2*time.Second, time.Minute))
	fetch := keeper.NewFetcher(client)
	catalog, err := fetch.BenchmarkCatalog(ctx)
	if err != nil {
		t.Fatalf("benchmark configs: %v", err)
	}

	names := make([]string, 0, len(liveCases))
	for n := range liveCases {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, vendor := range vendors {
		// Keeper's own region id (gcp and hcloud number their regions).
		region, ok := manifest.KeeperRegion(vendor)
		if !ok {
			t.Fatalf("no Keeper region for %s in the snapshot", vendor)
		}
		for _, name := range names {
			var spec api.Spec
			if err := yaml.Unmarshal([]byte(liveCases[name]), &spec); err != nil {
				t.Fatalf("%s: case spec: %v", name, err)
			}
			t.Run(vendor+"/"+name, func(t *testing.T) {
				checkLive(t, ctx, client, fetch, catalog, &spec, vendor, region)
			})
			// Only benchmark queries carry the push-down: without a
			// benchmark, ranking or profile of its own, the case also runs
			// with a ranking that keeps every family.
			if spec.Ranking == nil {
				ranked := spec.DeepCopy()
				ranked.Ranking = &api.Ranking{}
				if err := yaml.Unmarshal([]byte(keepAllRanking), ranked.Ranking); err != nil {
					t.Fatal(err)
				}
				t.Run(vendor+"/"+name+"+ranking", func(t *testing.T) {
					checkLive(t, ctx, client, fetch, catalog, ranked, vendor, region)
				})
			}
		}
	}
}

// keepAllRanking ranks by single-core speed but keeps every family.
const keepAllRanking = `{score: {benchmark: {id: "stress_ng:best1"}}, objective: Score, families: {max: 100000}}`

func checkLive(t *testing.T, ctx context.Context, client *keeper.Client, shared *keeper.Fetcher, catalog *keeper.BenchmarkCatalog,
	spec *api.Spec, vendor, region string) {
	opts := selection.Options{Catalog: catalog, DefaultBasis: api.PriceBasisOnDemand}
	if err := selection.Validate(spec, opts); err != nil {
		t.Skipf("not a valid spec: %v", err)
	}
	var pushed, plain selection.Result
	// The data is live and Keeper sits behind a CDN that caches every URL
	// for a few minutes, so two queries of one evaluation can see data of
	// different ages (spot offerings come and go). A difference counts only if
	// it survives a retry that bypasses the cache.
	fetch := shared
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			fetch = keeper.NewFetcher(uncachedClient(client))
		}
		var err error
		if pushed, err = evaluateLive(ctx, fetch, spec, vendor, region, opts, false); err != nil {
			t.Fatalf("with push-down: %v", err)
		}
		if plain, err = evaluateLive(ctx, fetch, spec, vendor, region, opts, true); err != nil {
			t.Fatalf("without push-down: %v", err)
		}
		if slices.Equal(pushed.Names(), plain.Names()) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if p, q := pushed.Names(), plain.Names(); !slices.Equal(p, q) {
		t.Errorf("the push-down changed the selection:\n  only without: %v\n  only with:    %v\n  families without: %s\n  families with:    %s",
			minus(q, p), minus(p, q), families(plain), families(pushed))
	}
	t.Logf("%d instance types", len(plain.Names()))
}

// evaluateLive plans the queries of spec, runs them (optionally without the
// push-down filters) and returns the selected instance types.
func evaluateLive(ctx context.Context, f *keeper.Fetcher, spec *api.Spec, vendor, region string,
	opts selection.Options, strip bool) (selection.Result, error) {
	queries, err := selection.PlanQueries(spec, vendor, []string{region}, opts)
	if err != nil {
		return selection.Result{}, err
	}
	var results []keeper.QueryResult
	for _, q := range queries {
		if strip {
			q.PushDown = keeper.PushDown{}
		}
		servers, err := f.Servers(ctx, q)
		if err != nil {
			b, _ := json.Marshal(q.PushDown)
			return selection.Result{}, &queryError{err: err, pushDown: string(b)}
		}
		if os.Getenv("KEEPER_DEBUG") != "" {
			pd, _ := json.Marshal(q.PushDown)
			bench := ""
			if q.Benchmark != nil {
				bench = q.Benchmark.ID
			}
			fmt.Fprintf(os.Stderr, "strip=%v %s %s alloc=%s rows=%d\n", strip, bench, pd, q.Allocation, len(servers))
		}
		results = append(results, keeper.QueryResult{Query: q, Servers: servers})
	}
	return selection.Evaluate(spec, selection.BuildCandidates(results), opts)
}

type queryError struct {
	err      error
	pushDown string
}

func (e *queryError) Error() string { return e.err.Error() + " (push-down " + e.pushDown + ")" }

// uncachedClient is a Keeper client whose requests carry a unique, ignored
// query parameter, so that the CDN passes them to Keeper.
func uncachedClient(*keeper.Client) *keeper.Client {
	url := os.Getenv("KEEPER_URL")
	if url == "" {
		url = "https://keeper.sparecores.net"
	}
	h := &http.Client{Transport: cacheBuster{http.DefaultTransport}}
	return keeper.New(url, keeper.WithHTTPClient(h), keeper.WithRetries(8, 2*time.Second, time.Minute))
}

type cacheBuster struct{ next http.RoundTripper }

func (c cacheBuster) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	q := r.URL.Query()
	q.Set("_live_test", strconv.FormatInt(time.Now().UnixNano(), 36))
	r.URL.RawQuery = q.Encode()
	return c.next.RoundTrip(r)
}

func families(r selection.Result) string {
	var b strings.Builder
	for _, f := range r.Families {
		fmt.Fprintf(&b, "%s(%d", f.Name, f.Rank)
		if f.Objective != nil {
			fmt.Fprintf(&b, " %.4g", *f.Objective)
		}
		b.WriteString(") ")
	}
	return b.String()
}

func minus(a, b []string) []string {
	var out []string
	for _, s := range a {
		if !slices.Contains(b, s) {
			out = append(out, s)
		}
	}
	return out
}
