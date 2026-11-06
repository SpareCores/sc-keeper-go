# sc-keeper-go

Go client of the [Spare Cores](https://sparecores.com) Keeper API
(<https://keeper.sparecores.net>) and the engine that selects and ranks cloud
instance types from its data. It is shared by
[sc-karpenter-nodepool](https://github.com/SpareCores/sc-karpenter-nodepool)
and SCarpenter.

| Package | What it is |
|---|---|
| `github.com/SpareCores/sc-keeper-go` (`keeper`) | Keeper client: `/servers`, `/benchmark_configs`, `/table/region`, `/table/zone`, `/table/server`, `/table/server_prices` (API key), `/server_prices` (anonymous, per-zone prices), `/healthcheck`. Retries with backoff, `Retry-After`, region chunking, a per-cycle memoising `Fetcher`. |
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

The output directory is replaced as a whole. The run fails if the Keeper
database changes during the fetch, and identical data gives identical files
(sorted rows and keys, no timestamps in the gzip header). `KEEPER_URL` and
`KEEPER_API_KEY` come from the environment. This repository refreshes its own
snapshot with the "Refresh test snapshot" workflow (`make snapshot`).

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
