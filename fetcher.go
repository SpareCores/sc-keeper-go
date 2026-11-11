/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package keeper

import (
	"context"
	"sync"
)

// Fetcher memoises Keeper responses for one refresh cycle: identical queries
// issued by several callers (or several times by one) hit Keeper once.
// Errors are memoised too, so one failing query fails consistently within the
// cycle. Create a new Fetcher per refresh cycle; it is safe for concurrent
// use, and concurrent identical calls share one request.
//
// The cacheable requests of a cycle (/servers, /benchmark_configs, the region,
// zone, server and server price tables and /server_prices) carry the database
// hash of /healthcheck, read once per cycle, as data_version (see
// DataVersionParam). Without a hash (healthcheck failed) they are sent without
// it.
type Fetcher struct {
	client *Client

	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	done  chan struct{}
	value any
	err   error
}

// NewFetcher returns a Fetcher reading through c.
func NewFetcher(c *Client) *Fetcher {
	return &Fetcher{client: c, entries: map[string]*entry{}}
}

func (f *Fetcher) memo(ctx context.Context, key string, fetch func(context.Context) (any, error)) (any, error) {
	f.mu.Lock()
	e, ok := f.entries[key]
	if !ok {
		e = &entry{done: make(chan struct{})}
		f.entries[key] = e
		f.mu.Unlock()
		e.value, e.err = fetch(ctx)
		if e.err != nil && ctx.Err() != nil {
			// A cancelled caller must not poison the cycle for the others.
			f.mu.Lock()
			delete(f.entries, key)
			f.mu.Unlock()
		}
		close(e.done)
		return e.value, e.err
	}
	f.mu.Unlock()
	select {
	case <-e.done:
		return e.value, e.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// QueryResult is the response of one Query.
type QueryResult struct {
	Query   Query
	Servers []Server
}

// Servers runs (or reuses) a /servers query. The returned slice is shared and
// must not be modified.
func (f *Fetcher) Servers(ctx context.Context, q Query) ([]Server, error) {
	q.DataVersion = f.dataVersion(ctx)
	v, err := f.memo(ctx, q.cacheKey(), func(ctx context.Context) (any, error) {
		return f.client.Servers(ctx, q)
	})
	if err != nil {
		return nil, err
	}
	return v.([]Server), nil
}

// ServersAll runs several queries and returns their results in order.
func (f *Fetcher) ServersAll(ctx context.Context, qs []Query) ([]QueryResult, error) {
	out := make([]QueryResult, 0, len(qs))
	for _, q := range qs {
		rows, err := f.Servers(ctx, q)
		if err != nil {
			return nil, err
		}
		out = append(out, QueryResult{Query: q, Servers: rows})
	}
	return out, nil
}

// BenchmarkConfigs returns (or reuses) /benchmark_configs.
func (f *Fetcher) BenchmarkConfigs(ctx context.Context) ([]BenchmarkConfig, error) {
	v, err := f.memo(ctx, "/benchmark_configs", func(ctx context.Context) (any, error) {
		return f.client.benchmarkConfigsAt(ctx, f.dataVersion(ctx))
	})
	if err != nil {
		return nil, err
	}
	return v.([]BenchmarkConfig), nil
}

// BenchmarkCatalog builds (or reuses) the catalog from /benchmark_configs.
func (f *Fetcher) BenchmarkCatalog(ctx context.Context) (*BenchmarkCatalog, error) {
	v, err := f.memo(ctx, "catalog", func(ctx context.Context) (any, error) {
		configs, err := f.BenchmarkConfigs(ctx)
		if err != nil {
			return nil, err
		}
		return NewBenchmarkCatalog(configs)
	})
	if err != nil {
		return nil, err
	}
	return v.(*BenchmarkCatalog), nil
}

// Regions returns (or reuses) /table/region.
func (f *Fetcher) Regions(ctx context.Context) ([]Region, error) {
	v, err := f.memo(ctx, "/table/region", func(ctx context.Context) (any, error) {
		return f.client.regionsAt(ctx, f.dataVersion(ctx))
	})
	if err != nil {
		return nil, err
	}
	return v.([]Region), nil
}

// Zones returns (or reuses) /table/zone.
func (f *Fetcher) Zones(ctx context.Context) ([]Zone, error) {
	v, err := f.memo(ctx, "/table/zone", func(ctx context.Context) (any, error) {
		return f.client.zonesAt(ctx, f.dataVersion(ctx))
	})
	if err != nil {
		return nil, err
	}
	return v.([]Zone), nil
}

// Vendors returns (or reuses) /table/vendor.
func (f *Fetcher) Vendors(ctx context.Context) ([]Vendor, error) {
	v, err := f.memo(ctx, "/table/vendor", func(ctx context.Context) (any, error) {
		return f.client.vendorsAt(ctx, f.dataVersion(ctx))
	})
	if err != nil {
		return nil, err
	}
	return v.([]Vendor), nil
}

// TableServers returns (or reuses) /table/server. The returned slice is
// shared and must not be modified.
func (f *Fetcher) TableServers(ctx context.Context) ([]TableServer, error) {
	v, err := f.memo(ctx, "/table/server", func(ctx context.Context) (any, error) {
		return f.client.tableServersAt(ctx, f.dataVersion(ctx))
	})
	if err != nil {
		return nil, err
	}
	return v.([]TableServer), nil
}

// TableServerPrices runs (or reuses) a /table/server_prices query. The
// returned slice is shared and must not be modified.
func (f *Fetcher) TableServerPrices(ctx context.Context, q PriceQuery) ([]ServerPrice, error) {
	q.DataVersion = f.dataVersion(ctx)
	v, err := f.memo(ctx, q.cacheKey(), func(ctx context.Context) (any, error) {
		return f.client.TableServerPrices(ctx, q)
	})
	if err != nil {
		return nil, err
	}
	return v.([]ServerPrice), nil
}

// ServerPrices runs (or reuses) a /server_prices query. The returned slice is
// shared and must not be modified.
func (f *Fetcher) ServerPrices(ctx context.Context, q ServerPricesQuery) ([]ZonePrice, error) {
	q.DataVersion = f.dataVersion(ctx)
	v, err := f.memo(ctx, q.cacheKey(), func(ctx context.Context) (any, error) {
		return f.client.ServerPrices(ctx, q)
	})
	if err != nil {
		return nil, err
	}
	return v.([]ZonePrice), nil
}

// Health returns (or reuses) /healthcheck.
func (f *Fetcher) Health(ctx context.Context) (Health, error) {
	v, err := f.memo(ctx, "/healthcheck", func(ctx context.Context) (any, error) {
		return f.client.Health(ctx)
	})
	if err != nil {
		return Health{}, err
	}
	return v.(Health), nil
}

// dataVersion is the database hash of the cycle, or "" if /healthcheck
// failed (its error is memoised, so it is asked once per cycle).
func (f *Fetcher) dataVersion(ctx context.Context) string {
	h, err := f.Health(ctx)
	if err != nil {
		return ""
	}
	return h.DatabaseHash
}
