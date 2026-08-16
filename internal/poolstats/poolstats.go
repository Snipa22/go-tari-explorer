// Package poolstats fetches pool-wide (not per-miner) statistics from a mining pool's
// stats API and exposes them through a small provider interface, so the server package
// never depends directly on any one pool backend's HTTP shape.
//
// The initial (and currently only) implementation, HTTPClient, talks to a nodejs-pool /
// node-cryptonote-pool-derived backend (confirmed live at
// https://pool.rxt.tari.jagtech.io/ as of 2026-08-17) via its plain JSON REST endpoints
// (GET /api/pool/stats, etc). That backend is expected to be rebuilt/replaced over time
// (see the SupportXMR Go-rewrite effort) - callers (internal/server) should depend only
// on the PoolStatsProvider interface below, never on HTTPClient or its request/response
// shapes directly, so swapping the backend implementation later is a drop-in change.
//
// Only /api/pool/stats is fetched for v1. /api/network/stats and /api/pool/blocks are
// deliberately out of scope for now (see README "What's deliberately deferred"). No
// per-miner/per-worker/per-address data is fetched or modeled anywhere in this package -
// Tari is MimbleWimble (no public address model), and no such endpoint is confirmed to
// exist on this backend; don't add one without re-confirming a real endpoint first.
package poolstats

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// DefaultBaseURL is the nodejs-pool instance this explorer points at absent an override.
// Override via the -pool-stats-base-url flag or TARI_EXPLORER_POOL_STATS_BASE_URL env var
// (see internal/config) - never rely on this constant directly in calling code.
const DefaultBaseURL = "https://pool.rxt.tari.jagtech.io"

// defaultTimeout bounds a single GetStats call so a slow/unreachable pool backend can't
// hang a page render indefinitely.
const defaultTimeout = 10 * time.Second

// PoolStats is the explorer's own normalized view of pool-wide statistics, decoupled
// from any one backend's JSON field names/types so a future backend swap doesn't ripple
// into internal/server or its templates.
type PoolStats struct {
	// HashRate is the pool's current reported hash rate (H/s).
	HashRate int64
	// Miners is the number of currently connected miners.
	Miners int64
	// TotalHashes is the pool's lifetime cumulative hash count.
	TotalHashes int64
	// LastBlockFoundTime is the unix timestamp (seconds) the most recent block was found.
	LastBlockFoundTime int64
	// LastBlockFound is the height of the most recently found block.
	LastBlockFound int64
	// TotalBlocksFound is the pool's lifetime count of blocks found.
	TotalBlocksFound int64
	// RoundHashes is the cumulative hash count contributed so far in the current round.
	RoundHashes int64
	// TotalMinersPaid is the lifetime amount paid out to miners, if the backend reports
	// one; nil when the backend has no value (observed as JSON null on the live pool).
	TotalMinersPaid *float64
	// TotalPayments is the lifetime count/amount of payments made, if reported; nil when
	// the backend has no value (observed as JSON null on the live pool).
	TotalPayments *float64
	// LastPayment is the unix timestamp (seconds) of the most recent payment run.
	LastPayment int64
}

// PoolStatsProvider is the seam between internal/server and whatever pool backend is
// actually in use. internal/server must depend only on this interface, never on
// HTTPClient or any backend-specific type, so a future pool-backend rewrite (e.g. the
// SupportXMR Go-rewrite effort) can be swapped in as a new implementation of this
// interface without touching the server/templates at all.
type PoolStatsProvider interface {
	// GetStats fetches current pool-wide statistics. Returns an error if the backend is
	// unreachable or returns a response this implementation can't parse.
	GetStats(ctx context.Context) (PoolStats, error)
}

// HTTPClient is a PoolStatsProvider backed by a nodejs-pool-derived JSON HTTP API.
type HTTPClient struct {
	baseURL string
	client  *http.Client
}

// NewHTTPClient constructs an HTTPClient against baseURL (no trailing slash required).
// If baseURL is empty, DefaultBaseURL is used. If httpClient is nil, a client with
// defaultTimeout is used.
func NewHTTPClient(baseURL string, httpClient *http.Client) *HTTPClient {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &HTTPClient{baseURL: trimTrailingSlash(baseURL), client: httpClient}
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// poolStatsResponse mirrors the JSON shape of GET /api/pool/stats on the live
// nodejs-pool backend. Kept unexported and package-private on purpose - this is exactly
// the backend-specific shape PoolStats exists to insulate callers from.
type poolStatsResponse struct {
	PoolStatistics struct {
		HashRate           int64    `json:"hashRate"`
		Miners             int64    `json:"miners"`
		TotalHashes        int64    `json:"totalHashes"`
		LastBlockFoundTime int64    `json:"lastBlockFoundTime"`
		LastBlockFound     int64    `json:"lastBlockFound"`
		TotalBlocksFound   int64    `json:"totalBlocksFound"`
		RoundHashes        int64    `json:"roundHashes"`
		TotalMinersPaid    *float64 `json:"totalMinersPaid"`
		TotalPayments      *float64 `json:"totalPayments"`
	} `json:"pool_statistics"`
	LastPayment int64 `json:"last_payment"`
}

// GetStats fetches and normalizes GET {baseURL}/api/pool/stats.
func (c *HTTPClient) GetStats(ctx context.Context) (PoolStats, error) {
	url := c.baseURL + "/api/pool/stats"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return PoolStats{}, fmt.Errorf("poolstats: build request: %w", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return PoolStats{}, fmt.Errorf("poolstats: fetch %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return PoolStats{}, fmt.Errorf("poolstats: fetch %s: unexpected status %d", url, resp.StatusCode)
	}

	var parsed poolStatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return PoolStats{}, fmt.Errorf("poolstats: decode %s: %w", url, err)
	}

	return PoolStats{
		HashRate:           parsed.PoolStatistics.HashRate,
		Miners:             parsed.PoolStatistics.Miners,
		TotalHashes:        parsed.PoolStatistics.TotalHashes,
		LastBlockFoundTime: parsed.PoolStatistics.LastBlockFoundTime,
		LastBlockFound:     parsed.PoolStatistics.LastBlockFound,
		TotalBlocksFound:   parsed.PoolStatistics.TotalBlocksFound,
		RoundHashes:        parsed.PoolStatistics.RoundHashes,
		TotalMinersPaid:    parsed.PoolStatistics.TotalMinersPaid,
		TotalPayments:      parsed.PoolStatistics.TotalPayments,
		LastPayment:        parsed.LastPayment,
	}, nil
}

// compile-time assertion that HTTPClient satisfies PoolStatsProvider.
var _ PoolStatsProvider = (*HTTPClient)(nil)
