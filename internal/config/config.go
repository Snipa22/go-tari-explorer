// Package config holds process-wide configuration shared by cmd/indexer and cmd/server:
// the Postgres DSN and the list of base-node GRPC hosts. Every value has a sane
// local-dev default and can be overridden by an environment variable or (where noted)
// a CLI flag - no hardcoded secrets, no required config file.
package config

import (
	"os"
	"strings"
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
