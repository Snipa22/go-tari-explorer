// Package config holds process-wide configuration shared by cmd/indexer and cmd/server:
// the Postgres DSN and the list of base-node GRPC hosts. Every value has a sane
// local-dev default and can be overridden by an environment variable or (where noted)
// a CLI flag - no hardcoded secrets, no required config file.
package config

import (
	"os"
	"strings"
	"time"
)

const (
	// DefaultPostgresDSN points at a local-dev Postgres instance. Override with
	// TARI_EXPLORER_POSTGRES_DSN or the -postgres-dsn flag in production.
	DefaultPostgresDSN = "postgres://tari_explorer:tari_explorer@localhost:5432/tari_explorer?sslmode=disable"

	// DefaultNodeGRPCHosts is a single-entry default pointing at Impala's public pool
	// node, matching the default already used by go-tari-grpc-lib's cmd/blockWinners.
	// Override with TARI_EXPLORER_NODE_HOSTS (comma-separated) or the
	// -base-node-grpc-hosts flag for a redundant, centrally-hosted node list.
	DefaultNodeGRPCHosts = "node-pool.tari.jagtech.io:18102"
)

// PostgresDSN returns TARI_EXPLORER_POSTGRES_DSN if set, else DefaultPostgresDSN.
func PostgresDSN() string {
	if v := os.Getenv("TARI_EXPLORER_POSTGRES_DSN"); v != "" {
		return v
	}
	return DefaultPostgresDSN
}

// NodeGRPCHosts returns the configured list of base-node GRPC host:port targets, from
// TARI_EXPLORER_NODE_HOSTS (comma-separated) if set, else DefaultNodeGRPCHosts.
func NodeGRPCHosts() []string {
	raw := os.Getenv("TARI_EXPLORER_NODE_HOSTS")
	if raw == "" {
		raw = DefaultNodeGRPCHosts
	}
	return ParseHostList(raw)
}

// ParseHostList splits a comma-separated host list, trimming whitespace and dropping
// empty entries. Exported so cmd/* can apply the same parsing to a CLI flag value.
func ParseHostList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// HTTPListenAddr returns TARI_EXPLORER_HTTP_ADDR if set, else ":8080".
func HTTPListenAddr() string {
	if v := os.Getenv("TARI_EXPLORER_HTTP_ADDR"); v != "" {
		return v
	}
	return ":8080"
}

// Default HTTP server-level timeouts. These exist to bound how long a single slow/
// malicious client can hold a connection open (classic Slowloris-class exposure) -
// acceptable to leave unset on an internal-VLAN-only deployment, but not once this
// server is reachable from the public internet (explore.tari.jagtech.io). Each has a
// matching TARI_EXPLORER_HTTP_*_TIMEOUT env var (see the accessor functions below) so
// an operator can tune them without a code change, following this package's existing
// env-var-with-sane-default pattern.
const (
	// DefaultHTTPReadHeaderTimeout bounds how long the server waits to receive a
	// client's request headers. 5s is generous for even a slow mobile client but
	// short enough that a client that never finishes sending headers can't hold a
	// connection open indefinitely.
	DefaultHTTPReadHeaderTimeout = 5 * time.Second

	// DefaultHTTPReadTimeout bounds the entire request (headers + body) read. This
	// server has no endpoints that accept a request body of any real size (every
	// route is a GET), so 15s is comfortably above any legitimate request while
	// still capping a slow-body-drip attack.
	DefaultHTTPReadTimeout = 15 * time.Second

	// DefaultHTTPWriteTimeout bounds how long the server has to write a response.
	// The /analysis/*.png endpoints are the slowest legitimate requests this server
	// serves - a real Postgres bucketed query plus a PNG encode - so this needs
	// enough headroom for that under normal load (30s), while still being a hard
	// upper bound rather than unbounded.
	DefaultHTTPWriteTimeout = 30 * time.Second

	// DefaultHTTPIdleTimeout bounds how long a keep-alive connection may sit idle
	// between requests before the server closes it. 60s is enough to keep a
	// browser's connection warm across normal page-to-page navigation without
	// tying up a connection slot indefinitely for a client that opened one and
	// never sent a second request.
	DefaultHTTPIdleTimeout = 60 * time.Second
)

// HTTPReadHeaderTimeout returns TARI_EXPLORER_HTTP_READ_HEADER_TIMEOUT (parsed as a
// Go duration string, e.g. "5s") if set and valid, else DefaultHTTPReadHeaderTimeout.
func HTTPReadHeaderTimeout() time.Duration {
	return durationEnv("TARI_EXPLORER_HTTP_READ_HEADER_TIMEOUT", DefaultHTTPReadHeaderTimeout)
}

// HTTPReadTimeout returns TARI_EXPLORER_HTTP_READ_TIMEOUT if set and valid, else
// DefaultHTTPReadTimeout.
func HTTPReadTimeout() time.Duration {
	return durationEnv("TARI_EXPLORER_HTTP_READ_TIMEOUT", DefaultHTTPReadTimeout)
}

// HTTPWriteTimeout returns TARI_EXPLORER_HTTP_WRITE_TIMEOUT if set and valid, else
// DefaultHTTPWriteTimeout.
func HTTPWriteTimeout() time.Duration {
	return durationEnv("TARI_EXPLORER_HTTP_WRITE_TIMEOUT", DefaultHTTPWriteTimeout)
}

// HTTPIdleTimeout returns TARI_EXPLORER_HTTP_IDLE_TIMEOUT if set and valid, else
// DefaultHTTPIdleTimeout.
func HTTPIdleTimeout() time.Duration {
	return durationEnv("TARI_EXPLORER_HTTP_IDLE_TIMEOUT", DefaultHTTPIdleTimeout)
}

// durationEnv reads envVar as a time.ParseDuration string, falling back to def if the
// env var is unset or fails to parse (a malformed override degrading to the safe
// default, rather than the process refusing to start, matches this package's existing
// philosophy elsewhere - e.g. HTTPListenAddr/PostgresDSN never validate their input).
func durationEnv(envVar string, def time.Duration) time.Duration {
	v := os.Getenv(envVar)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

// DefaultPoolStatsBaseURL points at the pool.rxt.tari.jagtech.io nodejs-pool instance.
// Override with TARI_EXPLORER_POOL_STATS_BASE_URL or the -pool-stats-base-url flag -
// this backend is expected to be rebuilt/replaced over time, so never hardcode this URL
// anywhere outside this one default.
const DefaultPoolStatsBaseURL = "https://pool.rxt.tari.jagtech.io"

// PoolStatsBaseURL returns TARI_EXPLORER_POOL_STATS_BASE_URL if set, else
// DefaultPoolStatsBaseURL.
func PoolStatsBaseURL() string {
	if v := os.Getenv("TARI_EXPLORER_POOL_STATS_BASE_URL"); v != "" {
		return v
	}
	return DefaultPoolStatsBaseURL
}

// DefaultMempoolPollInterval is how often cmd/mempool-poller polls the base node's
// live GetMempoolStats RPC absent an override, matching cmd/indexer's own
// 30-second -poll-interval default for its "follow" mode. Override with
// TARI_EXPLORER_MEMPOOL_POLL_INTERVAL (a time.ParseDuration string, e.g. "45s") or the
// -poll-interval flag.
const DefaultMempoolPollInterval = 30 * time.Second

// MempoolPollInterval returns TARI_EXPLORER_MEMPOOL_POLL_INTERVAL parsed as a
// time.Duration if set to a valid duration string, else DefaultMempoolPollInterval. An
// unparseable value is treated the same as unset (falls back to the default) rather
// than failing startup outright - not worth taking a config typo down a whole process
// for a poll-interval knob that already has a sane default.
func MempoolPollInterval() time.Duration {
	if v := os.Getenv("TARI_EXPLORER_MEMPOOL_POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return DefaultMempoolPollInterval
}

// DefaultDifficultyPollInterval is how often cmd/difficulty-poller polls the
// already-indexed `blocks` table (via db.CurrentDifficultyPerAlgo - no live base-node
// GRPC call, see that method's doc comment) for each algo's latest height/difficulty
// absent an override. Deliberately much shorter than DefaultMempoolPollInterval: this
// poll is just a cheap indexed Postgres read (not a GRPC round trip), and the point is
// to reflect the front page's "current difficulty" stat cards as close to real-time as
// practical - a new row is only ever inserted when an algo's height actually advances
// (see internal/difficultypoller.Poller.Tick), so a short interval doesn't bloat the
// table, it just lowers the detection latency. Override with
// TARI_EXPLORER_DIFFICULTY_POLL_INTERVAL (a time.ParseDuration string, e.g. "500ms") or
// the -poll-interval flag.
const DefaultDifficultyPollInterval = 1 * time.Second

// DifficultyPollInterval returns TARI_EXPLORER_DIFFICULTY_POLL_INTERVAL parsed as a
// time.Duration if set to a valid duration string, else DefaultDifficultyPollInterval.
// Same "typo falls back to default rather than failing startup" behavior as
// MempoolPollInterval.
func DifficultyPollInterval() time.Duration {
	if v := os.Getenv("TARI_EXPLORER_DIFFICULTY_POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return DefaultDifficultyPollInterval
}
