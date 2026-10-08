package nodeclient

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// fakeBaseNodeServer is a real in-process GRPC BaseNode server (bufconn), so these
// tests exercise the actual wire calls through a real *Client (dial/withFailover/
// stream-draining), not a hand-rolled interface stub - same rationale as
// internal/txsearch's own fakeBaseNodeServer/startFakeServer (see that package's test
// file), replicated here for nodeclient's own new methods.
type fakeBaseNodeServer struct {
	tari_generated.UnimplementedBaseNodeServer

	transactions []*tari_generated.Transaction
	stats        *tari_generated.MempoolStatsResponse

	template    *tari_generated.NewBlockTemplateResponse
	templateErr error

	// networkDifficulty/networkDifficultyErr back the fake GetNetworkDifficulty
	// streaming RPC below - networkDifficulty is sent in order, one Send() call per
	// entry, then the stream closes (returning the client's Recv() loop io.EOF).
	networkDifficulty    []*tari_generated.NetworkDifficultyResponse
	networkDifficultyErr error

	// headerByHash/headerByHashErr back the fake GetHeaderByHash unary RPC below -
	// keyed by the hex-encoded request hash so a test can respond differently per
	// hash; headerByHashErr (if set) takes precedence over any headerByHash entry.
	headerByHash    map[string]*tari_generated.BlockHeaderResponse
	headerByHashErr error
}

func (f *fakeBaseNodeServer) GetNetworkDifficulty(req *tari_generated.HeightRequest, stream tari_generated.BaseNode_GetNetworkDifficultyServer) error {
	if f.networkDifficultyErr != nil {
		return f.networkDifficultyErr
	}
	for _, resp := range f.networkDifficulty {
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeBaseNodeServer) GetHeaderByHash(ctx context.Context, req *tari_generated.GetHeaderByHashRequest) (*tari_generated.BlockHeaderResponse, error) {
	if f.headerByHashErr != nil {
		return nil, f.headerByHashErr
	}
	resp, ok := f.headerByHash[fmt.Sprintf("%x", req.GetHash())]
	if !ok {
		return nil, status.Error(codes.NotFound, "header not found for hash")
	}
	return resp, nil
}

func (f *fakeBaseNodeServer) GetMempoolTransactions(req *tari_generated.GetMempoolTransactionsRequest, stream tari_generated.BaseNode_GetMempoolTransactionsServer) error {
	for _, tx := range f.transactions {
		if err := stream.Send(&tari_generated.GetMempoolTransactionsResponse{Transaction: tx}); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeBaseNodeServer) GetMempoolStats(ctx context.Context, _ *tari_generated.Empty) (*tari_generated.MempoolStatsResponse, error) {
	if f.stats != nil {
		return f.stats, nil
	}
	return &tari_generated.MempoolStatsResponse{}, nil
}

func (f *fakeBaseNodeServer) GetNewBlockTemplate(ctx context.Context, req *tari_generated.NewBlockTemplateRequest) (*tari_generated.NewBlockTemplateResponse, error) {
	if f.templateErr != nil {
		return nil, f.templateErr
	}
	if f.template != nil {
		return f.template, nil
	}
	return &tari_generated.NewBlockTemplateResponse{}, nil
}

// startFakeServer boots fake on a bufconn listener and returns a real *Client dialed
// against it (via New's opts... seam - see nodeclient.go's doc comment on New), plus a
// cleanup func.
func startFakeServer(t *testing.T, fake *fakeBaseNodeServer) (*Client, func()) {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	tari_generated.RegisterBaseNodeServer(grpcServer, fake)
	go func() { _ = grpcServer.Serve(lis) }()

	dialer := grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	})

	client, err := New([]string{"passthrough:///bufnet"}, dialer, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cleanup := func() {
		_ = client.Close()
		grpcServer.Stop()
		_ = lis.Close()
	}
	return client, cleanup
}

func TestGetMempoolTransactions_DrainsStream(t *testing.T) {
	fake := &fakeBaseNodeServer{
		transactions: []*tari_generated.Transaction{
			{Offset: []byte{0x01}},
			{Offset: []byte{0x02}},
			{Offset: []byte{0x03}},
		},
	}
	client, cleanup := startFakeServer(t, fake)
	defer cleanup()

	got, err := client.GetMempoolTransactions(context.Background())
	if err != nil {
		t.Fatalf("GetMempoolTransactions: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 transactions, got %d: %+v", len(got), got)
	}
	if got[0].GetOffset()[0] != 0x01 || got[2].GetOffset()[0] != 0x03 {
		t.Fatalf("unexpected transactions: %+v", got)
	}
}

func TestGetMempoolTransactions_EmptyMempool(t *testing.T) {
	fake := &fakeBaseNodeServer{}
	client, cleanup := startFakeServer(t, fake)
	defer cleanup()

	got, err := client.GetMempoolTransactions(context.Background())
	if err != nil {
		t.Fatalf("GetMempoolTransactions: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 transactions for an empty mempool, got %d", len(got))
	}
}

func TestGetMempoolStats(t *testing.T) {
	fake := &fakeBaseNodeServer{
		stats: &tari_generated.MempoolStatsResponse{
			UnconfirmedTxs:    42,
			ReorgTxs:          3,
			UnconfirmedWeight: 123456,
		},
	}
	client, cleanup := startFakeServer(t, fake)
	defer cleanup()

	got, err := client.GetMempoolStats(context.Background())
	if err != nil {
		t.Fatalf("GetMempoolStats: %v", err)
	}
	if got.GetUnconfirmedTxs() != 42 || got.GetReorgTxs() != 3 || got.GetUnconfirmedWeight() != 123456 {
		t.Fatalf("unexpected stats: %+v", got)
	}
}

// TestGetNewBlockTemplate proves GetNewBlockTemplate round-trips the algo in the
// request and returns the height/target_difficulty/reward carried on a fake response.
func TestGetNewBlockTemplate(t *testing.T) {
	fake := &fakeBaseNodeServer{
		template: &tari_generated.NewBlockTemplateResponse{
			NewBlockTemplate: &tari_generated.NewBlockTemplate{
				Header: &tari_generated.NewBlockHeaderTemplate{Height: 123456},
			},
			MinerData: &tari_generated.MinerData{
				TargetDifficulty: 987654321,
				Reward:           5000000,
			},
		},
	}
	client, cleanup := startFakeServer(t, fake)
	defer cleanup()

	got, err := client.GetNewBlockTemplate(context.Background(), tari_generated.PowAlgo_POW_ALGOS_SHA3X)
	if err != nil {
		t.Fatalf("GetNewBlockTemplate: %v", err)
	}
	if height := got.GetNewBlockTemplate().GetHeader().GetHeight(); height != 123456 {
		t.Errorf("expected height 123456, got %d", height)
	}
	if diff := got.GetMinerData().GetTargetDifficulty(); diff != 987654321 {
		t.Errorf("expected target_difficulty 987654321, got %d", diff)
	}
	if reward := got.GetMinerData().GetReward(); reward != 5000000 {
		t.Errorf("expected reward 5000000, got %d", reward)
	}
}

// TestGetNetworkDifficulty_SingleHeight proves the pre-existing single-height call
// shape (fromHeight == toHeight) round-trips a one-entry stream, including a non-nil
// AdjustedDifficulty (the TIP-004 field this whole feature is about).
func TestGetNetworkDifficulty_SingleHeight(t *testing.T) {
	adjusted := uint64(320_000)
	fake := &fakeBaseNodeServer{
		networkDifficulty: []*tari_generated.NetworkDifficultyResponse{
			{Height: 100, Difficulty: 10_000, AdjustedDifficulty: &adjusted},
		},
	}
	client, cleanup := startFakeServer(t, fake)
	defer cleanup()

	got, err := client.GetNetworkDifficulty(context.Background(), 100, 100)
	if err != nil {
		t.Fatalf("GetNetworkDifficulty: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 response, got %d: %+v", len(got), got)
	}
	if got[0].GetHeight() != 100 || got[0].GetDifficulty() != 10_000 {
		t.Errorf("unexpected response: %+v", got[0])
	}
	if got[0].AdjustedDifficulty == nil || got[0].GetAdjustedDifficulty() != 320_000 {
		t.Errorf("expected AdjustedDifficulty 320000, got %+v", got[0].AdjustedDifficulty)
	}
}

// TestGetNetworkDifficulty_RangeDrainsEveryHeight proves a real [from, to] range
// request drains every streamed response into the returned slice, in order - the
// shape cmd/backfill-adjusted-difficulty relies on to fetch a whole batch of heights
// in one GRPC round trip rather than N single-height calls.
func TestGetNetworkDifficulty_RangeDrainsEveryHeight(t *testing.T) {
	fake := &fakeBaseNodeServer{
		networkDifficulty: []*tari_generated.NetworkDifficultyResponse{
			{Height: 100, Difficulty: 10},
			{Height: 101, Difficulty: 11},
			{Height: 102, Difficulty: 12},
		},
	}
	client, cleanup := startFakeServer(t, fake)
	defer cleanup()

	got, err := client.GetNetworkDifficulty(context.Background(), 100, 102)
	if err != nil {
		t.Fatalf("GetNetworkDifficulty: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 responses, got %d: %+v", len(got), got)
	}
	for i, wantHeight := range []uint64{100, 101, 102} {
		if got[i].GetHeight() != wantHeight {
			t.Errorf("got[%d].Height = %d, want %d", i, got[i].GetHeight(), wantHeight)
		}
	}
}

// TestGetNetworkDifficulty_MissingAdjustedDifficultyIsNil proves a response from a
// (simulated) base-node host that predates the TIP-004 field round-trips with a nil
// AdjustedDifficulty - never a coerced 0 - since callers (internal/indexer.go,
// cmd/backfill-adjusted-difficulty) depend on nil to mean "not captured".
func TestGetNetworkDifficulty_MissingAdjustedDifficultyIsNil(t *testing.T) {
	fake := &fakeBaseNodeServer{
		networkDifficulty: []*tari_generated.NetworkDifficultyResponse{
			{Height: 200, Difficulty: 20}, // AdjustedDifficulty deliberately left unset
		},
	}
	client, cleanup := startFakeServer(t, fake)
	defer cleanup()

	got, err := client.GetNetworkDifficulty(context.Background(), 200, 200)
	if err != nil {
		t.Fatalf("GetNetworkDifficulty: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 response, got %d", len(got))
	}
	if got[0].AdjustedDifficulty != nil {
		t.Errorf("expected AdjustedDifficulty to be nil (field absent on the wire), got %v", *got[0].AdjustedDifficulty)
	}
}

// TestGetHeaderByHash proves GetHeaderByHash round-trips a single 32-byte hash into
// GetHeaderByHashRequest.Hash and returns the fake server's BlockHeaderResponse,
// including its Difficulty field - the achieved_difficulty value this whole feature
// exists to surface (see this method's doc comment in nodeclient.go).
func TestGetHeaderByHash(t *testing.T) {
	hash := []byte{0xde, 0xad, 0xbe, 0xef}
	fake := &fakeBaseNodeServer{
		headerByHash: map[string]*tari_generated.BlockHeaderResponse{
			fmt.Sprintf("%x", hash): {
				Header:     &tari_generated.BlockHeader{Height: 961162},
				Difficulty: 193350,
			},
		},
	}
	client, cleanup := startFakeServer(t, fake)
	defer cleanup()

	got, err := client.GetHeaderByHash(context.Background(), hash)
	if err != nil {
		t.Fatalf("GetHeaderByHash: %v", err)
	}
	if got.GetHeader().GetHeight() != 961162 {
		t.Errorf("Header.Height = %d, want 961162", got.GetHeader().GetHeight())
	}
	if got.GetDifficulty() != 193350 {
		t.Errorf("Difficulty = %d, want 193350", got.GetDifficulty())
	}
}

// TestGetHeaderByHash_UnknownHashReturnsError proves a hash the fake server has no
// response for surfaces as an error (NotFound, here), not a zero-value
// BlockHeaderResponse - callers (internal/indexer.go, cmd/backfill-achieved-difficulty)
// depend on the error, rather than a present-but-zero Difficulty field, to detect a
// failed/missing lookup (see this method's doc comment: BlockHeaderResponse.Difficulty
// is a plain, non-optional uint64, unlike NetworkDifficultyResponse.AdjustedDifficulty).
func TestGetHeaderByHash_UnknownHashReturnsError(t *testing.T) {
	fake := &fakeBaseNodeServer{headerByHash: map[string]*tari_generated.BlockHeaderResponse{}}
	client, cleanup := startFakeServer(t, fake)
	defer cleanup()

	_, err := client.GetHeaderByHash(context.Background(), []byte{0x01, 0x02})
	if err == nil {
		t.Fatal("expected an error for an unknown hash, got nil")
	}
}

// startFakeServerPair boots two fake BaseNode servers on their own bufconn listeners
// and returns a real *Client configured with BOTH as its host list (in order), so
// tests can exercise withFailover's actual "first host fails, second succeeds"
// behavior end-to-end rather than mocking the failover logic away - same
// dial-via-opts... seam as startFakeServer above, just switching the in-process dialer
// on the target address to route each configured host to its own listener.
func startFakeServerPair(t *testing.T, fake1, fake2 *fakeBaseNodeServer) (*Client, func()) {
	t.Helper()

	lis1 := bufconn.Listen(1024 * 1024)
	srv1 := grpc.NewServer()
	tari_generated.RegisterBaseNodeServer(srv1, fake1)
	go func() { _ = srv1.Serve(lis1) }()

	lis2 := bufconn.Listen(1024 * 1024)
	srv2 := grpc.NewServer()
	tari_generated.RegisterBaseNodeServer(srv2, fake2)
	go func() { _ = srv2.Serve(lis2) }()

	dialer := grpc.WithContextDialer(func(ctx context.Context, target string) (net.Conn, error) {
		switch target {
		case "bufnet1":
			return lis1.DialContext(ctx)
		case "bufnet2":
			return lis2.DialContext(ctx)
		default:
			return nil, fmt.Errorf("startFakeServerPair: unexpected dial target %q", target)
		}
	})

	client, err := New(
		[]string{"passthrough:///bufnet1", "passthrough:///bufnet2"},
		dialer,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cleanup := func() {
		_ = client.Close()
		srv1.Stop()
		srv2.Stop()
		_ = lis1.Close()
		_ = lis2.Close()
	}
	return client, cleanup
}

// TestGetNewBlockTemplate_FailsOverToSecondHost proves that when the first configured
// host's GetNewBlockTemplate call errors, withFailover retries against the second host
// and returns its (successful) response rather than the first host's error.
func TestGetNewBlockTemplate_FailsOverToSecondHost(t *testing.T) {
	fake1 := &fakeBaseNodeServer{templateErr: status.Error(codes.Unavailable, "boom: host 1 down")}
	fake2 := &fakeBaseNodeServer{
		template: &tari_generated.NewBlockTemplateResponse{
			NewBlockTemplate: &tari_generated.NewBlockTemplate{
				Header: &tari_generated.NewBlockHeaderTemplate{Height: 42},
			},
			MinerData: &tari_generated.MinerData{TargetDifficulty: 111, Reward: 222},
		},
	}
	client, cleanup := startFakeServerPair(t, fake1, fake2)
	defer cleanup()

	got, err := client.GetNewBlockTemplate(context.Background(), tari_generated.PowAlgo_POW_ALGOS_RANDOMXM)
	if err != nil {
		t.Fatalf("GetNewBlockTemplate: expected failover to the second host to succeed, got error: %v", err)
	}
	if height := got.GetNewBlockTemplate().GetHeader().GetHeight(); height != 42 {
		t.Errorf("expected height 42 from the second host, got %d", height)
	}
	if diff := got.GetMinerData().GetTargetDifficulty(); diff != 111 {
		t.Errorf("expected target_difficulty 111 from the second host, got %d", diff)
	}
}

// TestGetHeaderByHash_FailsOverToSecondHost proves that when the first configured
// host's GetHeaderByHash call errors, withFailover retries against the second host
// and returns its (successful) response rather than the first host's error - the same
// failover contract every other method in this file gets, now exercised for
// GetHeaderByHash too.
func TestGetHeaderByHash_FailsOverToSecondHost(t *testing.T) {
	hash := []byte{0xaa, 0xbb}
	fake1 := &fakeBaseNodeServer{headerByHashErr: status.Error(codes.Unavailable, "boom: host 1 down")}
	fake2 := &fakeBaseNodeServer{
		headerByHash: map[string]*tari_generated.BlockHeaderResponse{
			fmt.Sprintf("%x", hash): {
				Header:     &tari_generated.BlockHeader{Height: 961162},
				Difficulty: 193350,
			},
		},
	}
	client, cleanup := startFakeServerPair(t, fake1, fake2)
	defer cleanup()

	got, err := client.GetHeaderByHash(context.Background(), hash)
	if err != nil {
		t.Fatalf("GetHeaderByHash: expected failover to the second host to succeed, got error: %v", err)
	}
	if got.GetDifficulty() != 193350 {
		t.Errorf("expected Difficulty 193350 from the second host, got %d", got.GetDifficulty())
	}
}
