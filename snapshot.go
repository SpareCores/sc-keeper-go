/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package keeper

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SpareCores/sc-keeper-go/columnar"
)

// Keeper's snapshots are files of the tables of one database hash, built once
// per hash: GET /snapshot/{hash} is the manifest, and the files are below it
// (see Manifest, TablePath, VendorTablePath, ScorePath). They are columnar
// JSON (package columnar), always gzip, immutable for a hash, and need an API
// key (WithAPIKey) at Keeper's public instance.

// maxSnapshotBody bounds what one snapshot response may hold in memory (gzip
// bytes); the largest file seen is 1 MB.
const maxSnapshotBody = 256 << 20

// Headers Keeper sets on every response.
const (
	// HeaderDataHash carries the database hash a response is of.
	HeaderDataHash = "SC-Data-Hash"
	// HeaderDataPublishedAt carries when that database was published
	// (RFC 3339).
	HeaderDataPublishedAt = "SC-Data-Published-At"
)

// DataVersion is the database version of a snapshot response, from the
// SC-Data-Hash and SC-Data-Published-At headers. On a snapshot file or
// manifest it is the version of the hash requested; Keeper sets the headers
// on errors too, then of the replica's current database.
type DataVersion struct {
	Hash        string
	PublishedAt time.Time
}

func dataVersion(h http.Header) DataVersion {
	v := DataVersion{Hash: h.Get(HeaderDataHash)}
	if t, err := time.Parse(time.RFC3339, h.Get(HeaderDataPublishedAt)); err == nil {
		v.PublishedAt = t.UTC()
	}
	return v
}

// ManifestFile is one file of a snapshot: Path is the route below
// /snapshot/{hash}/ ("vendor", "server_price/aws",
// "benchmark_score/aws/stress_ng:bestn"), Rows the number of rows and SHA256
// the hex digest of the uncompressed JSON (the gzip bytes of one hash can
// differ between two builds).
type ManifestFile struct {
	Path   string `json:"path"`
	Rows   int    `json:"rows"`
	SHA256 string `json:"sha256"`
}

// Manifest lists the files of a snapshot, sorted by path.
type Manifest struct {
	Files []ManifestFile

	index map[string]int
}

// Lookup returns the file with the path.
func (m *Manifest) Lookup(path string) (ManifestFile, bool) {
	if m.index == nil {
		m.index = make(map[string]int, len(m.Files))
		for i, f := range m.Files {
			m.index[f.Path] = i
		}
	}
	i, ok := m.index[path]
	if !ok {
		return ManifestFile{}, false
	}
	return m.Files[i], true
}

// TablePath is the path of a global table ("vendor", "country",
// "compliance_framework", "benchmark", "currency").
func TablePath(table string) string { return table }

// VendorTablePath is the path of a per-vendor table ("region", "zone",
// "server", "server_price", "storage_price", "ipv4_price", ...).
func VendorTablePath(table, vendor string) string { return table + "/" + vendor }

// ScorePath is the path of every benchmark score of one benchmark at one
// vendor, all configs in one file. The benchmark id has a colon in it
// ("stress_ng:bestn"); it is sent as is.
func ScorePath(vendor, benchmarkID string) string {
	return "benchmark_score/" + vendor + "/" + benchmarkID
}

// SnapshotBody is one snapshot response: the gzip bytes as received (what a
// cache stores), and what came with them.
type SnapshotBody struct {
	// Gzip is the body as received.
	Gzip []byte
	// Version is of the hash requested.
	Version DataVersion
	// Header is the response header (Content-Type, Cache-Control, ...).
	Header http.Header
}

// Open returns the uncompressed JSON as a stream. Closing it releases the
// decompressor; the gzip checksum is verified when the stream is read to its
// end.
func (b *SnapshotBody) Open() (io.ReadCloser, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b.Gzip))
	if err != nil {
		return nil, fmt.Errorf("snapshot body: %w", err)
	}
	return zr, nil
}

// ErrChecksum is wrapped by the error of a snapshot body whose SHA-256 is not
// the one expected.
var ErrChecksum = errors.New("snapshot checksum mismatch")

// Verify checks the SHA-256 (hex, as in the manifest) of the uncompressed
// JSON.
func (b *SnapshotBody) Verify(sha string) error {
	zr, err := b.Open()
	if err != nil {
		return err
	}
	defer func() { _ = zr.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, zr); err != nil {
		return fmt.Errorf("snapshot body: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, sha) {
		return fmt.Errorf("%w: got %s, want %s", ErrChecksum, got, sha)
	}
	return nil
}

// DecodeSnapshotRows decodes the body as the columnar rows of T (see
// columnar.DecodeRows; rows is the manifest's row count, -1 when unknown),
// streaming from the gzip bytes.
func DecodeSnapshotRows[T any](b *SnapshotBody, rows int) ([]T, error) {
	zr, err := b.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	out, err := columnar.DecodeRows[T](zr, rows)
	if err != nil {
		return nil, err
	}
	// Read to the end so that a corrupt gzip trailer shows.
	if _, err := io.Copy(io.Discard, zr); err != nil {
		return nil, fmt.Errorf("snapshot body: %w", err)
	}
	return out, nil
}

// SnapshotManifest returns the manifest of the snapshot of a database hash
// (GET /snapshot/{hash}), the DataVersion of the hash.
func (c *Client) SnapshotManifest(ctx context.Context, hash string) (Manifest, DataVersion, error) {
	body, err := c.SnapshotManifestBody(ctx, hash)
	if err != nil {
		return Manifest{}, DataVersion{}, err
	}
	m, err := DecodeManifest(body)
	if err != nil {
		return Manifest{}, DataVersion{}, fmt.Errorf("snapshot %s: %w", hash, err)
	}
	return m, body.Version, nil
}

// SnapshotManifestBody fetches the manifest of a hash as received (what a
// cache stores); DecodeManifest reads it.
func (c *Client) SnapshotManifestBody(ctx context.Context, hash string) (*SnapshotBody, error) {
	return c.snapshotGet(ctx, hash, "")
}

// DecodeManifest reads the manifest in a body (of SnapshotManifestBody, or a
// copy of it from a cache).
func DecodeManifest(b *SnapshotBody) (Manifest, error) {
	files, err := DecodeSnapshotRows[ManifestFile](b, -1)
	if err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	return Manifest{Files: files}, nil
}

// SnapshotFile fetches one file of the snapshot of a hash, by its path (see
// TablePath, VendorTablePath and ScorePath), as received. A 503
// snapshot-preparing is retried (see WithPreparingBudget); the other
// problems, a 401 included, come back as typed errors (UnauthorizedError,
// ProblemError).
func (c *Client) SnapshotFile(ctx context.Context, hash, path string) (*SnapshotBody, error) {
	if path == "" {
		return nil, errors.New("keeper: snapshot file without a path")
	}
	return c.snapshotGet(ctx, hash, path)
}

func (c *Client) snapshotGet(ctx context.Context, hash, path string) (*SnapshotBody, error) {
	u := c.baseURL + "/snapshot/" + url.PathEscape(hash)
	if path != "" {
		segments := strings.Split(path, "/")
		for i, s := range segments {
			segments[i] = url.PathEscape(s) // a colon stays as it is
		}
		u += "/" + strings.Join(segments, "/")
	}
	var out *SnapshotBody
	err := c.getResponse(ctx, u, func(resp *http.Response) error {
		if !strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
			return &permanentError{fmt.Errorf("GET %s: the response is not gzip-encoded", u)}
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSnapshotBody+1))
		if err != nil {
			return fmt.Errorf("GET %s: %w", u, err)
		}
		if len(raw) > maxSnapshotBody {
			return &permanentError{fmt.Errorf("GET %s: the response is over %d MiB", u, maxSnapshotBody>>20)}
		}
		out = &SnapshotBody{Gzip: raw, Version: dataVersion(resp.Header), Header: resp.Header.Clone()}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// StoragePrice is one row of a storage_price file: the price of a storage
// type in a region (unit GB/month).
type StoragePrice struct {
	VendorID  string  `json:"vendor_id"`
	RegionID  string  `json:"region_id"`
	StorageID string  `json:"storage_id"`
	Unit      string  `json:"unit"`
	Price     float64 `json:"price"`
	Currency  string  `json:"currency"`
	Status    string  `json:"status"`
}

// IPv4Price is one row of an ipv4_price file: the price of a public IPv4
// address in a region. Unit differs per vendor ("hour" at AWS, "month" at
// Hetzner).
type IPv4Price struct {
	VendorID string  `json:"vendor_id"`
	RegionID string  `json:"region_id"`
	Unit     string  `json:"unit"`
	Price    float64 `json:"price"`
	Currency string  `json:"currency"`
	Status   string  `json:"status"`
}

// BenchmarkScore is one row of a benchmark_score file. Config is the stored
// config text, byte for byte what /benchmark_configs returns as its config
// string: compare the bytes, never a re-encoding. ResourceType is "server" or
// "database", ResourceID the server_id (not the api_reference) of the server.
type BenchmarkScore struct {
	VendorID     string          `json:"vendor_id"`
	BenchmarkID  string          `json:"benchmark_id"`
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id"`
	Config       json.RawMessage `json:"config"`
	Score        *float64        `json:"score"`
	Status       string          `json:"status"`
}

// Server returns the row as a Server: the specification columns, without
// prices and scores (the fields that only /servers fills stay empty).
func (t *TableServer) Server() Server {
	s := Server{
		APIReference: t.APIReference, ServerID: t.ServerID, VendorID: t.VendorID,
		Name: t.Name, DisplayName: t.DisplayName, Description: t.Description,
		Series: t.Series, Status: t.Status,
		VCPUs: t.VCPUs, CPUCores: t.CPUCores, CPUAllocation: t.CPUAllocation,
		CPUArchitecture: t.CPUArchitecture, CPUManufacturer: t.CPUManufacturer,
		CPUFamily: t.CPUFamily, CPUModel: t.CPUModel, CPUSpeed: t.CPUSpeed, CPUFlags: t.CPUFlags,
		CPUL1dCache: t.CPUL1dCache, CPUL1iCache: t.CPUL1iCache, CPUL2Cache: t.CPUL2Cache,
		CPUL3Cache: t.CPUL3Cache, CPUL3CacheTotal: t.CPUL3CacheTotal,
		MemoryAmount:     t.MemoryAmount,
		AcceleratorCount: t.AcceleratorCount, AcceleratorType: t.AcceleratorType,
		AcceleratorFamily: t.AcceleratorFamily, AcceleratorManufacturer: t.AcceleratorManufacturer,
		AcceleratorModel: t.AcceleratorModel, AcceleratorMemoryMin: t.AcceleratorMemoryMin,
		AcceleratorMemoryTotal: t.AcceleratorMemoryTotal,
		NetworkSpeedBaseline:   t.NetworkSpeedBaseline, NetworkSpeedMax: t.NetworkSpeedMax,
		StorageSize: t.StorageSize, Storages: t.Storages,
	}
	if t.Family != nil {
		s.Family = *t.Family
	}
	return s
}
