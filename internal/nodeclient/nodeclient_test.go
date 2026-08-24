package nodeclient

import (
	"context"
	"net"
	"testing"

	"github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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
