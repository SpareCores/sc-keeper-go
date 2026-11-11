# sc-keeper-go

Go client of the [Spare Cores](https://sparecores.com) Keeper API
(<https://keeper.sparecores.net>) and the engine that selects and ranks cloud
instance types from its data. It is shared by
[sc-karpenter-nodepool](https://github.com/SpareCores/sc-karpenter-nodepool)
and SCarpenter.

| Package | What it is |
|---|---|
| `github.com/SpareCores/sc-keeper-go` (`keeper`) | Keeper client: `/servers`, `/benchmark_configs`, `/table/region`, `/table/zone`, `/table/vendor` (with billing fields), `/table/server`, `/table/server_prices` (API key), `/server_prices` (anonymous, per-zone prices), `/healthcheck`. Retries with backoff, `Retry-After`, region chunking, a per-cycle memoising `Fetcher`. |
| `.../selection` | Candidates from Keeper rows, selector filters, data-quality policy, ranking (families, band), the Keeper query plan. Pure functions, no Kubernetes client. |
| `.../selection/api` | The `Selector`, `Ranking`, `Churn` API types with kubebuilder markers and generated DeepCopy, to embed in a CRD. |
| `.../selection/churn` | Hysteresis for instance types leaving a selection. |
| `.../keepertest` | A fake Keeper that replays a snapshot of real responses, for offline tests. |
| `.../snapshot`, `cmd/sc-snapshot` | Fetch such a snapshot from a query list. |

The semantics of selectors and rankings are specified in
[`docs/reference/selectors.md` of sc-karpenter-nodepool](https://github.com/SpareCores/sc-karpenter-nodepool/blob/main/docs/reference/selectors.md).

## Stability

The module is **v0**: minor versions may break the API. Pin a version in
`go.mod`. Responses are decoded by field name and unknown fields are ignored,
so additive changes of the Keeper schema do not break the client.

## Client

```go
c := keeper.New("", keeper.WithUserAgent("my-tool/1.0")) // "" is https://keeper.sparecores.net
f := keeper.NewFetcher(c)                               // memoises one refresh cycle

servers, err := f.Servers(ctx, keeper.Query{
	Vendor:     "aws",
	Regions:    []string{"us-east-1"},
	Allocation: keeper.AllocationOnDemand,
})
zones, err := f.Zones(ctx)         // /table/zone: zone_id, name, api_reference, status
vendors, err := f.Vendors(ctx)     // /table/vendor: billing increment/minimum, stopped-server charge (nil = unknown)
table, err := f.TableServers(ctx)  // /table/server: the whole Server table (about 12 MB)
```

`ServerPrices` reads the anonymous `/server_prices` search endpoint: one row
per zone and price. Keeper embeds the full server, vendor, region and zone
objects in every row and does not compress the response (about 4 KB per row;
four AWS regions are 35,000 rows and 137 MB), so the client sends
`Accept-Encoding: gzip`, decodes the body as a stream and keeps only the flat
`ZonePrice` fields (memory is proportional to the kept rows). Regions are
chunked by 20 (`WithServerPricesRegionsPerRequest`):

```go
prices, err := f.ServerPrices(ctx, keeper.ServerPricesQuery{
	Vendor:     "aws",
	Regions:    []string{"us-east-1", "us-west-2"},
	Allocation: keeper.PriceAllocationSpot,
})
```

`/table/server_prices` (the `TableServerPrices` alternative: the price table
without the embedded objects) needs a Keeper API key. A key also
lifts the anonymous limit of three regions per `/servers` request;
`WithRegionsPerRequest(0)` sends all regions at once:

```go
c := keeper.New("", keeper.WithAPIKey(key), keeper.WithRegionsPerRequest(0))
prices, err := c.TableServerPrices(ctx, keeper.PriceQuery{
	Vendor:     "aws",
	Regions:    []string{"us-east-1", "eu-west-1"},
	Allocation: keeper.PriceAllocationSpot,
	Currency:   "USD",
})
```

`ServerPrice` and `ZonePrice` carry `PriceTiered` (`[]PriceTier{Lower, Upper, Price}`
in hours; the last `Upper` is `+Inf`, decoded from the string `"Infinity"`).

Cacheable requests carry the database hash of `/healthcheck` as `data_version`,
so all requests of a refresh see one version of the data.

## Selection

See `selection/example_test.go`: `PlanQueries` turns a `selection/api.Spec` into
Keeper queries, `BuildCandidates` joins the results, `Evaluate` filters and
ranks them.

## Tests without the network: the replay server

`keepertest` serves a snapshot (a directory of gzipped responses plus a
`manifest.json`) as a Keeper API. The module's own snapshot is committed under
`keepertest/testdata/snapshot`:

```go
srv := keepertest.New(t) // closed when the test ends
c := keeper.New(srv.URL)
```

Requests are matched on path, `vendor`, `vendor_regions`,
`best_price_allocation`, `allocation`, `currency`, `benchmark_id` and
`benchmark_config`; filter parameters are ignored, and an unknown query gets an
HTTP 404 naming the recorded ones. Use `keepertest.NewFromDir(t, dir)` for your
own snapshot, and `Handler.Mount` to serve another API next to it.

### Making a snapshot

`/server_prices` responses are stored as a flat projection (only the fields of
`ZonePrice`; the manifest entry says `"projection": "flat"`) to keep the
committed files small.

`cmd/sc-snapshot` fetches the requests of a query list (YAML: path and
parameters per entry, plus vendor and benchmark expansions; the format is
documented on `snapshot.QueryList`, an example is
`keepertest/testdata/queries.yaml`):

```bash
go run github.com/SpareCores/sc-keeper-go/cmd/sc-snapshot -queries queries.yaml -out testdata/snapshot
```

The query list is expanded and validated before the first data request: a
region that does not exist, one that only matches retired (inactive) regions, an
ambiguous one, an unknown benchmark config or two queries writing one file fail
the run at once. Region names (`region_id`, `api_reference` or alias) resolve to
active regions first, so hcloud `fsn1` is the active `fsn1`, not the retired
`4` that carries it as an alias. Benchmark ids listed more than once (Geekbench
single- and multi-core) get a short hash of the canonical config in
`{benchmark_slug}` (`geekbench_score-1a2b3c4d`); an id listed once keeps its
plain slug.

Each response is read as a stream and written to its file as soon as it is
complete (a response is canonicalised and sorted first, so its rows are held in
encoded form, one response at a time, never the whole snapshot). The output
directory is replaced as a whole when the run succeeds; until then the files
are in `<out>.tmp`, and files not part of the query list (leftovers of killed
runs) are pruned before the swap. The swap renames `<out>` to `<out>.old`, the
staging directory to `<out>`, then deletes the old one; a crash in between is
repaired by the next run. A lock (`<out>.lock`) makes a second run on the same
output fail at once (the lock file stays next to the output; add `*.lock` and
`*.tmp` to your `.gitignore`); SIGINT and SIGTERM cancel the run and remove the file
being written. After an
interruption, `-resume` skips the queries whose file exists (in `<out>` or
`<out>.tmp`), is complete (its SHA-256 matches the manifest) and was fetched
from the same Keeper database hash; everything else is fetched again. The run
fails if the Keeper database changes during the fetch, and identical data gives
identical files and manifest (sorted rows and keys, no timestamps in the gzip
header; only `fetched_at` differs). `KEEPER_URL` and `KEEPER_API_KEY` come from
the environment. Library users call `snapshot.Fetch` with `Options{Dir: ...,
Resume: ...}`. This repository refreshes its own snapshot with the "Refresh test
snapshot" workflow (`make snapshot`).

## Development

Everything runs in a dev container (Go, golangci-lint, controller-gen; the
host needs only Docker):

```bash
hack/dev.sh make generate   # DeepCopy of selection/api
hack/dev.sh make lint
hack/dev.sh make test       # offline
hack/dev.sh go mod tidy
```

CI runs the same. Releases are tags `v0.x.y`; the release workflow checks the
tagged commit and creates the GitHub release.

## License

[MPL-2.0](LICENSE)
