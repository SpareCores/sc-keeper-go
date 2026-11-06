/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package keeper

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultURL is the public Keeper API.
const DefaultURL = "https://keeper.sparecores.net"

// DefaultRegionsPerRequest is Keeper's limit of vendor_regions values per
// anonymous /servers request.
const DefaultRegionsPerRequest = 3

// DefaultServerPricesRegionsPerRequest is the number of vendor_regions values
// per /server_prices request. Keeper accepted 20 in one request when this was
// measured and documents no limit; the number only bounds the size of one
// response (tens of MB per region).
const DefaultServerPricesRegionsPerRequest = 20

// DefaultUserAgent is the User-Agent header of requests (see WithUserAgent).
const DefaultUserAgent = "sc-keeper-go"

// Client reads the Keeper API. The zero value is not usable; use New.
// A Client is safe for concurrent use.
type Client struct {
	baseURL   string
	apiKey    string
	userAgent string
	http      *http.Client

	// RegionsPerRequest chunks Query.Regions; <= 0 sends all at once (an API
	// key lifts Keeper's limit).
	regionsPerRequest       int
	pricesRegionsPerRequest int
	maxRetries              int
	baseDelay               time.Duration
	maxDelay                time.Duration
	sleep                   func(ctx context.Context, d time.Duration) error
}

// Option configures a Client.
type Option func(*Client)

// WithAPIKey sends the key as "Authorization: Bearer <key>".
func WithAPIKey(key string) Option {
	return func(c *Client) { c.apiKey = key }
}

// WithUserAgent sets the User-Agent header (default DefaultUserAgent); programs
// built on the client should name themselves so that Keeper's operators can
// tell the callers apart.
func WithUserAgent(ua string) Option {
	return func(c *Client) { c.userAgent = ua }
}

// WithServerPricesRegionsPerRequest sets how many regions one /server_prices
// request carries (default DefaultServerPricesRegionsPerRequest; <= 0 sends
// all at once).
func WithServerPricesRegionsPerRequest(n int) Option {
	return func(c *Client) { c.pricesRegionsPerRequest = n }
}

// WithHTTPClient replaces the HTTP client (default: 2 minute timeout).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// WithRegionsPerRequest sets how many regions one /servers request carries.
func WithRegionsPerRequest(n int) Option {
	return func(c *Client) { c.regionsPerRequest = n }
}

// WithRetries sets the number of retries after the first attempt and the
// exponential backoff (base delay, doubled per attempt, capped at max). A
// Retry-After header of an HTTP 429 response is honoured up to max.
func WithRetries(n int, base, maxDelay time.Duration) Option {
	return func(c *Client) {
		c.maxRetries = n
		c.baseDelay = base
		c.maxDelay = maxDelay
	}
}

// WithSleep replaces how the client waits between retries (tests use it to
// observe backoff without waiting).
func WithSleep(sleep func(ctx context.Context, d time.Duration) error) Option {
	return func(c *Client) { c.sleep = sleep }
}

// New returns a client of the Keeper API at baseURL (DefaultURL if empty).
func New(baseURL string, opts ...Option) *Client {
	if baseURL == "" {
		baseURL = DefaultURL
	}
	c := &Client{
		baseURL:                 strings.TrimRight(baseURL, "/"),
		userAgent:               DefaultUserAgent,
		http:                    &http.Client{Timeout: 2 * time.Minute},
		regionsPerRequest:       DefaultRegionsPerRequest,
		pricesRegionsPerRequest: DefaultServerPricesRegionsPerRequest,
		maxRetries:              4,
		baseDelay:               2 * time.Second,
		maxDelay:                2 * time.Minute,
		sleep:                   sleepCtx,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// HTTPError is a non-2xx Keeper response that was not retried (or ran out of
// retries).
type HTTPError struct {
	URL        string
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("GET %s: HTTP %d: %s", e.URL, e.StatusCode, e.Body)
}

// Servers runs a /servers query. Regions are chunked into several requests and
// the rows merged by api_reference, keeping the cheapest row (Keeper reports
// the cheapest price over the regions of one request).
func (c *Client) Servers(ctx context.Context, q Query) ([]Server, error) {
	if q.Vendor == "" {
		return nil, errors.New("keeper: query without vendor")
	}
	var merged []Server
	index := map[string]int{}
	for _, regions := range chunks(q.Regions, c.regionsPerRequest) {
		var rows []Server
		if err := c.get(ctx, "/servers", q.values(regions), &rows); err != nil {
			return nil, err
		}
		for _, row := range rows {
			i, seen := index[row.APIReference]
			switch {
			case !seen:
				index[row.APIReference] = len(merged)
				merged = append(merged, row)
			case cheaper(row.Price, merged[i].Price):
				merged[i] = row
			}
		}
	}
	return merged, nil
}

func cheaper(a, b *float64) bool {
	return a != nil && (b == nil || *a < *b)
}

// BenchmarkConfigs returns every (benchmark_id, config) pair Keeper knows.
func (c *Client) BenchmarkConfigs(ctx context.Context) ([]BenchmarkConfig, error) {
	return c.benchmarkConfigsAt(ctx, "")
}

func (c *Client) benchmarkConfigsAt(ctx context.Context, version string) ([]BenchmarkConfig, error) {
	var out []BenchmarkConfig
	err := c.get(ctx, "/benchmark_configs", versionParams(version), &out)
	return out, err
}

// Regions returns the region table of every vendor.
func (c *Client) Regions(ctx context.Context) ([]Region, error) {
	return c.regionsAt(ctx, "")
}

func (c *Client) regionsAt(ctx context.Context, version string) ([]Region, error) {
	var out []Region
	err := c.get(ctx, "/table/region", versionParams(version), &out)
	return out, err
}

// Zones returns the zone table of every vendor.
func (c *Client) Zones(ctx context.Context) ([]Zone, error) {
	return c.zonesAt(ctx, "")
}

func (c *Client) zonesAt(ctx context.Context, version string) ([]Zone, error) {
	var out []Zone
	err := c.get(ctx, "/table/zone", versionParams(version), &out)
	return out, err
}

// TableServers returns the Server table of every vendor (GET /table/server,
// about 12 MB of JSON, no filters).
func (c *Client) TableServers(ctx context.Context) ([]TableServer, error) {
	return c.tableServersAt(ctx, "")
}

func (c *Client) tableServersAt(ctx context.Context, version string) ([]TableServer, error) {
	var out []TableServer
	err := c.get(ctx, "/table/server", versionParams(version), &out)
	return out, err
}

// TableServerPrices runs a /table/server_prices query. Keeper requires an API
// key for this endpoint (see WithAPIKey): anonymous requests get HTTP 401.
// Regions are chunked into several requests (see WithRegionsPerRequest) and
// the rows concatenated; rows of different regions never overlap.
func (c *Client) TableServerPrices(ctx context.Context, q PriceQuery) ([]ServerPrice, error) {
	if q.Vendor == "" {
		return nil, errors.New("keeper: price query without vendor")
	}
	var out []ServerPrice
	for _, regions := range chunks(q.Regions, c.regionsPerRequest) {
		var rows []ServerPrice
		if err := c.get(ctx, "/table/server_prices", q.values(regions), &rows); err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// ServerPrices runs a /server_prices query: the anonymous search endpoint,
// one row per zone and price. Keeper embeds the full server, vendor, region
// and zone objects in every row (about 4 KB per row, tens of MB per region,
// uncompressed), so the body is decoded as a stream and only the flat
// ZonePrice fields are kept. Regions are chunked into several requests (see
// WithServerPricesRegionsPerRequest) and the rows concatenated.
func (c *Client) ServerPrices(ctx context.Context, q ServerPricesQuery) ([]ZonePrice, error) {
	if q.Vendor == "" {
		return nil, errors.New("keeper: server prices query without vendor")
	}
	var out []ZonePrice
	for _, regions := range chunks(q.Regions, c.pricesRegionsPerRequest) {
		var rows []ZonePrice
		err := c.getWith(ctx, "/server_prices", q.values(regions), func(r io.Reader) error {
			var err error
			rows, err = decodeZonePrices(r)
			return err
		})
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// decodeZonePrices walks a JSON array element by element, so that memory is
// proportional to the kept rows, not to the body: json.Decoder.Decode of a
// whole slice would buffer the entire array first. Unknown fields (the nested
// objects) are skipped.
func decodeZonePrices(r io.Reader) ([]ZonePrice, error) {
	dec := json.NewDecoder(r)
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		if err == nil {
			err = errors.New("response is not a JSON array")
		}
		return nil, err
	}
	var rows []ZonePrice
	for dec.More() {
		var row ZonePrice
		if err := dec.Decode(&row); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return rows, nil
}

// Health returns the hash and update time of the database Keeper serves.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var raw struct {
		DatabaseHash        string  `json:"database_hash"`
		DatabaseLastUpdated float64 `json:"database_last_updated"`
	}
	if err := c.get(ctx, "/healthcheck", nil, &raw); err != nil {
		return Health{}, err
	}
	sec, frac := math.Modf(raw.DatabaseLastUpdated)
	return Health{
		DatabaseHash:        raw.DatabaseHash,
		DatabaseLastUpdated: time.Unix(int64(sec), int64(frac*1e9)).UTC(),
	}, nil
}

// versionParams is the data_version parameter of a cacheable table request
// (nil without a version).
func versionParams(version string) url.Values {
	if version == "" {
		return nil
	}
	return url.Values{DataVersionParam: {version}}
}

// get performs a GET with retries and decodes the JSON body into out.
func (c *Client) get(ctx context.Context, path string, params url.Values, out any) error {
	return c.getWith(ctx, path, params, func(r io.Reader) error { return json.NewDecoder(r).Decode(out) })
}

// getWith performs a GET with retries and hands the body to decode. decode
// must start from scratch on every call: a truncated body is retried.
func (c *Client) getWith(ctx context.Context, path string, params url.Values, decode func(io.Reader) error) error {
	u := c.baseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		retryAfter, err := c.do(ctx, u, decode)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable(err) || attempt >= c.maxRetries {
			return lastErr
		}
		delay := c.backoff(attempt)
		if retryAfter > 0 {
			delay = min(retryAfter, c.maxDelay)
		}
		if err := c.sleep(ctx, delay); err != nil {
			return fmt.Errorf("%w (last error: %w)", err, lastErr)
		}
	}
}

// do performs one request. It returns the Retry-After delay of a 429/503
// response, if any.
func (c *Client) do(ctx context.Context, u string, decode func(io.Reader) error) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, fmt.Errorf("building request %s: %w", u, err)
	}
	req.Header.Set("Accept", "application/json")
	// Setting the header ourselves turns off net/http's transparent gzip, so
	// the body is decoded below. Keeper's CDN does not compress today.
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("User-Agent", c.userAgent)
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, &transportError{err: fmt.Errorf("GET %s: %w", u, err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()), &HTTPError{
			URL:        u,
			StatusCode: resp.StatusCode,
			Body:       strings.TrimSpace(string(body)),
		}
	}
	body := io.Reader(resp.Body)
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return 0, &transportError{err: fmt.Errorf("decoding %s: %w", u, err)}
		}
		defer func() { _ = zr.Close() }()
		body = zr
	}
	if err := decode(body); err != nil {
		return 0, &transportError{err: fmt.Errorf("decoding %s: %w", u, err)}
	}
	return 0, nil
}

// transportError marks network and truncated-body errors, which are retried.
type transportError struct{ err error }

func (e *transportError) Error() string { return e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

func retryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if _, ok := errors.AsType[*transportError](err); ok {
		return true
	}
	if he, ok := errors.AsType[*HTTPError](err); ok {
		switch he.StatusCode {
		case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
			http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
	}
	return false
}

func (c *Client) backoff(attempt int) time.Duration {
	d := c.baseDelay << min(attempt, 30)
	if d <= 0 || d > c.maxDelay {
		return c.maxDelay
	}
	return d
}

// parseRetryAfter reads a Retry-After header: delay-seconds or an HTTP date.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil {
		if s < 0 {
			return 0
		}
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
