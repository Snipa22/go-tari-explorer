package indexer

import (
	"context"
	"fmt"
	"net"
	"os"
	"testing"

	"github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/Snipa22/go-tari-explorer/internal/db"
	"github.com/Snipa22/go-tari-explorer/internal/nodeclient"
)

// indexerTestDSN returns the Postgres connection string this file's tests run
// against: a dedicated throwaway database, distinct from every other package's own
// test database (see AGENTS.md / this repo's embedded-pg convention). Override with
// TARI_EXPLORER_INDEXER_TEST_POSTGRES_DSN in CI or a different local setup.
func indexerTestDSN() string {
	if v := os.Getenv("TARI_EXPLORER_INDEXER_TEST_POSTGRES_DSN"); v != "" {
		return v
	}
	return "postgres://postgres@localhost:5433/tari_explorer_indexer_test?sslmode=disable&host=/workspace/pg-embed/sockets"
}

// openIndexerTestDB connects to indexerTestDSN(), runs migrations, and truncates
// `blocks` (and its dependents) so each test starts from a clean slate. Skips the
// test (not fails) if the database isn't reachable, matching every other DB-backed
// test suite's convention in this repo.
func openIndexerTestDB(t *testing.T) *db.DB {
	t.Helper()
	ctx := context.Background()

	d, err := db.Connect(ctx, indexerTestDSN())
	if err != nil {
		t.Skipf("indexer: test postgres not reachable at %s: %v", indexerTestDSN(), err)
	}
	t.Cleanup(d.Close)

	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("indexer: migrate: %v", err)
	}
	if _, err := d.Pool.Exec(ctx, `TRUNCATE TABLE kernels, outputs, block_kernels, blocks CASCADE`); err != nil {
		t.Fatalf("indexer: truncate: %v", err)
	}
	return d
}

// fakeBaseNodeServer is a real in-process GRPC BaseNode server (bufconn), so these
// tests exercise the actual indexBlock code path through a real *nodeclient.Client
// (the Indexer's Node field is a concrete type, not an interface) - same rationale as
// internal/nodeclient's own fakeBaseNodeServer, just implementing the two RPCs
// Backfill/indexBlock actually calls (GetBlocks, GetNetworkDifficulty).
type fakeBaseNodeServer struct {
	tari_generated.UnimplementedBaseNodeServer

	blocks []*tari_generated.Block

	// networkDifficultyByHeight maps a requested height to the response indexBlock's
	// single-height GetNetworkDifficulty(ctx, h, h) call should receive; a height
	// absent from this map gets no response at all (simulating a lookup that failed
	// or simply has no data), matching GetNetworkDifficulty's "non-fatal, leave
	// difficulty 0 / adjusted_difficulty NULL" contract.
	networkDifficultyByHeight map[uint64]*tari_generated.NetworkDifficultyResponse

	// headerByHash maps a hex-encoded block hash to the BlockHeaderResponse
	// indexBlock's GetHeaderByHash(ctx, header.GetHash()) call should receive; a hash
	// absent from this map gets a NotFound error (simulating a failed/missing lookup),
	// matching GetHeaderByHash's "non-fatal, leave achieved_difficulty NULL" contract.
	headerByHash map[string]*tari_generated.BlockHeaderResponse
}

func (f *fakeBaseNodeServer) GetHeaderByHash(ctx context.Context, req *tari_generated.GetHeaderByHashRequest) (*tari_generated.BlockHeaderResponse, error) {
	resp, ok := f.headerByHash[fmt.Sprintf("%x", req.GetHash())]
	if !ok {
		return nil, status.Error(codes.NotFound, "header not found for hash")
	}
	return resp, nil
}

func (f *fakeBaseNodeServer) GetBlocks(req *tari_generated.GetBlocksRequest, stream tari_generated.BaseNode_GetBlocksServer) error {
	want := make(map[uint64]bool, len(req.GetHeights()))
	for _, h := range req.GetHeights() {
		want[h] = true
	}
	for _, b := range f.blocks {
		if !want[b.GetHeader().GetHeight()] {
			continue
		}
		if err := stream.Send(&tari_generated.HistoricalBlock{Block: b}); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeBaseNodeServer) GetNetworkDifficulty(req *tari_generated.HeightRequest, stream tari_generated.BaseNode_GetNetworkDifficultyServer) error {
	for h := req.GetStartHeight(); h <= req.GetEndHeight(); h++ {
		resp, ok := f.networkDifficultyByHeight[h]
		if !ok {
			continue
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	return nil
}

// startFakeIndexer boots fake on a bufconn listener and returns a real *Indexer wired
// up with a real *nodeclient.Client dialed against it (via nodeclient.New's opts...
// seam) and database, plus a cleanup func.
func startFakeIndexer(t *testing.T, fake *fakeBaseNodeServer, database *db.DB) (*Indexer, func()) {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	tari_generated.RegisterBaseNodeServer(grpcServer, fake)
	go func() { _ = grpcServer.Serve(lis) }()

	dialer := grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	})

	node, err := nodeclient.New([]string{"passthrough:///bufnet"}, dialer, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("nodeclient.New: %v", err)
	}

	cleanup := func() {
		_ = node.Close()
		grpcServer.Stop()
		_ = lis.Close()
	}
	return New(node, database), cleanup
}

func fakeBlock(height uint64) *tari_generated.Block {
	return &tari_generated.Block{
		Header: &tari_generated.BlockHeader{
			Height: height,
			Hash:   []byte{byte(height)},
		},
		Body: &tari_generated.AggregateBody{},
	}
}

// TestIndexBlock_CapturesAdjustedDifficulty proves Backfill (via indexBlock) stores
// the base node's reported AdjustedDifficulty on the block row, alongside the
// pre-existing raw Difficulty - the "capture going forward" half of this feature (see
// migrations/0010_adjusted_difficulty.up.sql).
func TestIndexBlock_CapturesAdjustedDifficulty(t *testing.T) {
	database := openIndexerTestDB(t)
	ctx := context.Background()

	adjusted := uint64(320_000)
	fake := &fakeBaseNodeServer{
		blocks: []*tari_generated.Block{fakeBlock(100)},
		networkDifficultyByHeight: map[uint64]*tari_generated.NetworkDifficultyResponse{
			100: {Height: 100, Difficulty: 10_000, AdjustedDifficulty: &adjusted},
		},
	}
	ix, cleanup := startFakeIndexer(t, fake, database)
	defer cleanup()

	if err := ix.Backfill(ctx, 100, 100); err != nil {
		t.Fatalf("Backfill: %v", err)
	}

	got, err := database.GetBlock(ctx, 100)
	if err != nil {
		t.Fatalf("GetBlock: %v", err)
	}
	if got.Difficulty != 10_000 {
		t.Errorf("Difficulty = %d, want 10000", got.Difficulty)
	}
	if got.AdjustedDifficulty == nil || *got.AdjustedDifficulty != 320_000 {
		t.Fatalf("AdjustedDifficulty = %+v, want 320000", got.AdjustedDifficulty)
	}
}

// TestIndexBlock_MissingAdjustedDifficultyLeavesColumnNull proves a
// GetNetworkDifficulty response that doesn't carry AdjustedDifficulty (simulating a
// base-node host that predates the TIP-004 field) leaves blocks.adjusted_difficulty
// NULL rather than defaulting it to 0 - the core nullable-vs-zero contract this
// feature depends on (see migrations/0010_adjusted_difficulty.up.sql).
func TestIndexBlock_MissingAdjustedDifficultyLeavesColumnNull(t *testing.T) {
	database := openIndexerTestDB(t)
	ctx := context.Background()

	fake := &fakeBaseNodeServer{
		blocks: []*tari_generated.Block{fakeBlock(101)},
		networkDifficultyByHeight: map[uint64]*tari_generated.NetworkDifficultyResponse{
			101: {Height: 101, Difficulty: 5_000}, // AdjustedDifficulty deliberately unset
		},
	}
	ix, cleanup := startFakeIndexer(t, fake, database)
	defer cleanup()

	if err := ix.Backfill(ctx, 101, 101); err != nil {
		t.Fatalf("Backfill: %v", err)
	}

	got, err := database.GetBlock(ctx, 101)
	if err != nil {
		t.Fatalf("GetBlock: %v", err)
	}
	if got.Difficulty != 5_000 {
		t.Errorf("Difficulty = %d, want 5000", got.Difficulty)
	}
	if got.AdjustedDifficulty != nil {
		t.Errorf("AdjustedDifficulty = %v, want nil (field absent on the wire)", *got.AdjustedDifficulty)
	}
}

// TestIndexBlock_FailedDifficultyLookupLeavesAdjustedDifficultyNull proves a height
// with NO GetNetworkDifficulty response at all (simulating a failed/empty lookup -
// indexBlock's existing "err == nil && len(diffs) > 0" guard) still indexes the block
// successfully, with Difficulty left at its zero value and AdjustedDifficulty left
// NULL, matching this repo's "a failed difficulty lookup is non-fatal" convention
// (see indexer.go's indexBlock doc comment).
func TestIndexBlock_FailedDifficultyLookupLeavesAdjustedDifficultyNull(t *testing.T) {
	database := openIndexerTestDB(t)
	ctx := context.Background()

	fake := &fakeBaseNodeServer{
		blocks:                    []*tari_generated.Block{fakeBlock(102)},
		networkDifficultyByHeight: map[uint64]*tari_generated.NetworkDifficultyResponse{}, // no entry for height 102
	}
	ix, cleanup := startFakeIndexer(t, fake, database)
	defer cleanup()

	if err := ix.Backfill(ctx, 102, 102); err != nil {
		t.Fatalf("Backfill: %v", err)
	}

	got, err := database.GetBlock(ctx, 102)
	if err != nil {
		t.Fatalf("GetBlock: %v", err)
	}
	if got.Difficulty != 0 {
		t.Errorf("Difficulty = %d, want 0 (no difficulty response for this height)", got.Difficulty)
	}
	if got.AdjustedDifficulty != nil {
		t.Errorf("AdjustedDifficulty = %v, want nil", *got.AdjustedDifficulty)
	}
}

// TestIndexBlock_CapturesAchievedDifficulty proves Backfill (via indexBlock) stores
// the base node's reported achieved difficulty (BlockHeaderResponse.Difficulty, from
// a GetHeaderByHash lookup keyed on the block's own hash) on the block row, alongside
// the pre-existing raw/adjusted difficulty columns - the "capture going forward" half
// of this feature (see migrations/0011_achieved_difficulty.up.sql).
func TestIndexBlock_CapturesAchievedDifficulty(t *testing.T) {
	database := openIndexerTestDB(t)
	ctx := context.Background()

	block := fakeBlock(200)
	fake := &fakeBaseNodeServer{
		blocks: []*tari_generated.Block{block},
		headerByHash: map[string]*tari_generated.BlockHeaderResponse{
			fmt.Sprintf("%x", block.GetHeader().GetHash()): {
				Header:     block.GetHeader(),
				Difficulty: 193_350,
			},
		},
	}
	ix, cleanup := startFakeIndexer(t, fake, database)
	defer cleanup()

	if err := ix.Backfill(ctx, 200, 200); err != nil {
		t.Fatalf("Backfill: %v", err)
	}

	got, err := database.GetBlock(ctx, 200)
	if err != nil {
		t.Fatalf("GetBlock: %v", err)
	}
	if got.AchievedDifficulty == nil || *got.AchievedDifficulty != 193_350 {
		t.Fatalf("AchievedDifficulty = %+v, want 193350", got.AchievedDifficulty)
	}
}

// TestIndexBlock_FailedHeaderByHashLookupLeavesAchievedDifficultyNull proves a block
// whose hash has no corresponding entry in the fake server's headerByHash map
// (simulating a failed GetHeaderByHash lookup - unknown hash, host error, etc.) still
// indexes successfully, with achieved_difficulty left NULL rather than aborting the
// whole block index over this secondary metric - the same non-fatal-on-failure
// contract AdjustedDifficulty's own capture already has, now proven for
// AchievedDifficulty too (see indexer.go's indexBlock doc comment on achievedDifficulty).
func TestIndexBlock_FailedHeaderByHashLookupLeavesAchievedDifficultyNull(t *testing.T) {
	database := openIndexerTestDB(t)
	ctx := context.Background()

	fake := &fakeBaseNodeServer{
		blocks:       []*tari_generated.Block{fakeBlock(201)},
		headerByHash: map[string]*tari_generated.BlockHeaderResponse{}, // no entry for this block's hash
	}
	ix, cleanup := startFakeIndexer(t, fake, database)
	defer cleanup()

	if err := ix.Backfill(ctx, 201, 201); err != nil {
		t.Fatalf("Backfill: %v", err)
	}

	got, err := database.GetBlock(ctx, 201)
	if err != nil {
		t.Fatalf("GetBlock: %v", err)
	}
	if got.AchievedDifficulty != nil {
		t.Errorf("AchievedDifficulty = %v, want nil (failed lookup is non-fatal)", *got.AchievedDifficulty)
	}
}
