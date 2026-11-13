/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

package keeper

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"time"
)

// Problem types of Keeper's application/problem+json bodies (relative, as
// Keeper writes them).
const (
	// ProblemSnapshotNotAvailable: the replica cannot serve the requested
	// hash. Requested, Current and Available say what it has.
	ProblemSnapshotNotAvailable = "/problems/snapshot-not-available"
	// ProblemSnapshotPreparing: the replica is downloading or building the
	// hash; try again after RetryAfter.
	ProblemSnapshotPreparing = "/problems/snapshot-preparing"
	// ProblemNotFound: an unknown table, vendor or path.
	ProblemNotFound = "/problems/not-found"
)

// UnauthorizedError is Keeper's 401: no API key, or a rejected one. It is
// never retried (Keeper charges a rejected request 11 credits against its
// rate limit). It wraps the *HTTPError of the response, so errors.As finds
// both.
type UnauthorizedError struct {
	// KeyConfigured says whether the request carried a key (WithAPIKey):
	// false means none was sent, true that Keeper rejected the one sent.
	KeyConfigured bool
	// Detail is Keeper's explanation ("Authentication required" or "Invalid
	// or expired token").
	Detail string

	http *HTTPError
}

func (e *UnauthorizedError) Error() string {
	state := "no API key was sent"
	if e.KeyConfigured {
		state = "the API key was rejected"
	}
	return fmt.Sprintf("GET %s: HTTP 401: %s (%s)", e.http.URL, e.Detail, state)
}

// Unwrap returns the *HTTPError of the response.
func (e *UnauthorizedError) Unwrap() error { return e.http }

// ProblemError is an application/problem+json answer of Keeper (RFC 9457).
// It wraps the *HTTPError of the response. A 406 is one too: it means the
// client sent no Accept-Encoding: gzip, which Client always does.
type ProblemError struct {
	// Status is the HTTP status; Type and Title are the problem's. Type is
	// relative ("/problems/not-found") or "about:blank".
	Status int
	Type   string
	Title  string
	// Requested, Current and Available are the members of the snapshot
	// problems: the hash asked for, the hash the replica serves now, and the
	// hashes it has locally, newest first (snapshot-not-available only).
	Requested string
	Current   string
	Available []string
	// RetryAfter is the Retry-After header of the answer (0 without one).
	RetryAfter time.Duration

	hasRetryAfter bool
	http          *HTTPError
}

func (e *ProblemError) Error() string {
	s := fmt.Sprintf("GET %s: HTTP %d %s: %s", e.http.URL, e.Status, e.Type, e.Title)
	if e.Requested != "" {
		s += fmt.Sprintf(" (requested %s, current %s)", e.Requested, e.Current)
	}
	return s
}

// Unwrap returns the *HTTPError of the response.
func (e *ProblemError) Unwrap() error { return e.http }

func (e *ProblemError) is(problem string) bool {
	return e.Type == problem || strings.HasSuffix(e.Type, problem)
}

// IsSnapshotNotAvailable reports whether err is Keeper's 404
// snapshot-not-available (also the answer to a malformed hash).
func IsSnapshotNotAvailable(err error) bool { return isProblem(err, ProblemSnapshotNotAvailable) }

// IsSnapshotPreparing reports whether err is Keeper's 503 snapshot-preparing.
func IsSnapshotPreparing(err error) bool { return isProblem(err, ProblemSnapshotPreparing) }

// IsNotFound reports whether err is Keeper's 404 not-found (an unknown table,
// vendor or path).
func IsNotFound(err error) bool { return isProblem(err, ProblemNotFound) }

func isProblem(err error, problem string) bool {
	pe, ok := errors.AsType[*ProblemError](err)
	return ok && pe.is(problem)
}

// responseError builds the error of a non-2xx response from its (truncated)
// body.
func (c *Client) responseError(u string, resp *http.Response, body []byte, now time.Time) error {
	he := &HTTPError{URL: u, StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	if resp.StatusCode == http.StatusUnauthorized {
		var b struct {
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(body, &b)
		if b.Detail == "" {
			b.Detail = he.Body
		}
		return &UnauthorizedError{KeyConfigured: c.apiKey != "", Detail: b.Detail, http: he}
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt == "application/problem+json" {
		var b struct {
			Type      string   `json:"type"`
			Title     string   `json:"title"`
			Status    int      `json:"status"`
			Requested string   `json:"requested"`
			Current   string   `json:"current"`
			Available []string `json:"available"`
		}
		if json.Unmarshal(body, &b) == nil {
			if b.Status == 0 {
				b.Status = resp.StatusCode
			}
			return &ProblemError{
				Status: b.Status, Type: b.Type, Title: b.Title,
				Requested: b.Requested, Current: b.Current, Available: b.Available,
				RetryAfter:    parseRetryAfter(resp.Header.Get("Retry-After"), now),
				hasRetryAfter: strings.TrimSpace(resp.Header.Get("Retry-After")) != "", http: he,
			}
		}
	}
	return he
}
