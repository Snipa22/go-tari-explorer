// Package nodeclient wraps go-tari-grpc-lib/v3's generated Tari base-node GRPC client
// with support for a LIST of base-node host:port targets and basic failover.
//
// go-tari-grpc-lib's nodeGRPC package (see nodeGRPC/client.go) is intentionally a thin,
// single-connection, package-level-global wrapper: InitNodeGRPC(addr) dials exactly one
// connection into an unexported package variable, and every subsequent call
// (GetTipInfo, GetBlockByHeight, ...) reaches for that same global. That's fine for a
// short-lived CLI that only ever talks to one node, but it is unsafe for an explorer that
// needs to poll N base-nodes for redundancy: a second InitNodeGRPC call from a different
// goroutine/host would silently repoint the global connection out from under any
// in-flight call.
//
// Rather than fight that global (there's no exported per-connection variant, and adding
// one would mean patching a dependency this repo doesn't own), Client here dials its own
// *grpc.ClientConn per configured host and calls the generated
// tari_generated.NewBaseNodeClient(conn) directly - the same generated client type
// nodeGRPC itself uses internally, just constructed per-host instead of off a shared
// global. This keeps go-tari-grpc-lib as the single source of truth for the generated
// protobuf/gRPC stubs while making multi-host polling safe.
package nodeclient

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client talks to one or more Tari base-node GRPC endpoints, retrying against the next
// configured host on error. It does not do any active health checking beyond
// "did the last call to this host fail" - on failure it simply advances to the next host
// in the list for the *next* attempt, wrapping around. This is deliberately simple:
// good enough for a small, operator-controlled list of redundant hosts, not a general
// load balancer.
type Client struct {
	mu      sync.Mutex
	hosts   []string
	conns   []*grpc.ClientConn // lazily dialed, index-aligned with hosts
	current int                // index into hosts/conns to try first on the next call
}

// New constructs a Client for the given list of "host:port" base-node GRPC targets.
// Connections are dialed lazily (on first use per host), not eagerly in New, so
// constructing a Client never blocks or fails even if a host is currently unreachable.
func New(hosts []string) (*Client, error) {
	if len(hosts) == 0 {
		return nil, fmt.Errorf("nodeclient: at least one base-node GRPC host is required")
	}
	return &Client{
		hosts: hosts,
		conns: make([]*grpc.ClientConn, len(hosts)),
	}, nil
}

// Close tears down every dialed connection. Safe to call even if some hosts were never
// dialed.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var firstErr error
	for i, conn := range c.conns {
		if conn == nil {
			continue
		}
		if err := conn.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		c.conns[i] = nil
	}
	return firstErr
}

// connAt lazily dials the connection for hosts[i] if it hasn't been dialed yet.
func (c *Client) connAt(i int) (*grpc.ClientConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conns[i] != nil {
		return c.conns[i], nil
	}
	conn, err := grpc.NewClient(c.hosts[i], grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	c.conns[i] = conn
	return conn, nil
}

// nextIndex advances the failover cursor and returns the new current index.
func (c *Client) nextIndex(from int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = (from + 1) % len(c.hosts)
	return c.current
}

// startIndex returns the index to try first for a new call.
func (c *Client) startIndex() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

// withFailover runs fn against each configured host in turn, starting from the current
// failover cursor and wrapping around, until one succeeds or every host has been tried.
// On success, the cursor is left pointing at the host that worked so subsequent calls
// prefer it first. On total failure, the returned error wraps every per-host error seen.
func withFailover[T any](c *Client, ctx context.Context, fn func(ctx context.Context, client tari_generated.BaseNodeClient) (T, error)) (T, error) {
	var zero T
	start := c.startIndex()
	var errs []error
	for attempt := 0; attempt < len(c.hosts); attempt++ {
		idx := (start + attempt) % len(c.hosts)
		conn, err := c.connAt(idx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: dial: %w", c.hosts[idx], err))
			c.nextIndex(idx)
			continue
		}
		result, err := fn(ctx, tari_generated.NewBaseNodeClient(conn))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.hosts[idx], err))
			c.nextIndex(idx)
			continue
		}
		c.mu.Lock()
		c.current = idx
		c.mu.Unlock()
		return result, nil
	}
	return zero, fmt.Errorf("nodeclient: all %d host(s) failed: %w", len(c.hosts), joinErrs(errs))
}

func joinErrs(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	msg := errs[0].Error()
	for _, e := range errs[1:] {
		msg += "; " + e.Error()
	}
	return fmt.Errorf("%s", msg)
}

// GetTipInfo wraps the GetTipInfo GRPC call with failover across configured hosts.
func (c *Client) GetTipInfo(ctx context.Context) (*tari_generated.TipInfoResponse, error) {
	return withFailover(c, ctx, func(ctx context.Context, client tari_generated.BaseNodeClient) (*tari_generated.TipInfoResponse, error) {
		return client.GetTipInfo(ctx, &tari_generated.Empty{})
	})
}

// GetBlockByHeight retrieves blocks for the given heights, handling the streaming
// response and returning them as a slice, with failover across configured hosts.
func (c *Client) GetBlockByHeight(ctx context.Context, heights []uint64) ([]*tari_generated.Block, error) {
	return withFailover(c, ctx, func(ctx context.Context, client tari_generated.BaseNodeClient) ([]*tari_generated.Block, error) {
		stream, err := client.GetBlocks(ctx, &tari_generated.GetBlocksRequest{Heights: heights}, grpc.MaxCallRecvMsgSize(16*1024*1024))
		if err != nil {
			return nil, err
		}
		resp := make([]*tari_generated.Block, 0, len(heights))
		for {
			blockResp, err := stream.Recv()
			if err != nil {
				if err == io.EOF {
					return resp, nil
				}
				return nil, err
			}
			resp = append(resp, blockResp.GetBlock())
		}
	})
}

// GetNetworkDifficulty returns network difficulty info for a single height, with
// failover across configured hosts.
func (c *Client) GetNetworkDifficulty(ctx context.Context, height uint64) (*tari_generated.NetworkDifficultyResponse, error) {
	return withFailover(c, ctx, func(ctx context.Context, client tari_generated.BaseNodeClient) (*tari_generated.NetworkDifficultyResponse, error) {
		diffClient, err := client.GetNetworkDifficulty(ctx, &tari_generated.HeightRequest{StartHeight: height, EndHeight: height})
		if err != nil {
			return nil, err
		}
		return diffClient.Recv()
	})
}
