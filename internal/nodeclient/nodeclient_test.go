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
