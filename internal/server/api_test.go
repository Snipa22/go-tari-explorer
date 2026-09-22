// Tests for the /api/* JSON namespace (api.go). Reuses this package's own
// openTestDB/seedBlock test helpers (defined in analysis_test.go) for the
// Postgres-backed routes - same embedded-Postgres-skip-if-unreachable convention as
// every other DB-backed test in this repo.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"gopkg.in/yaml.v3"

	"github.com/Snipa22/go-tari-explorer/internal/db"
	"github.com/Snipa22/go-tari-explorer/internal/nodeclient"
	"github.com/Snipa22/go-tari-explorer/internal/poolstats"
)

// decodeJSON decodes rec's body into a fresh T, failing the test on any decode error.
func decodeJSON[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode json response: %v (body: %s)", err, rec.Body.String())
	}
	return v
}

func assertJSONContentType(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json; charset=utf-8")
	}
}

// seedBlockFull inserts a minimal valid `blocks` row at height with caller-supplied
// powAlgo/poolTag/difficulty - mirrors internal/db's own db_test.go seedBlockFull
// helper (this package's analysis_test.go only has the simpler seedBlock, which
// hardcodes PowAlgo "RXM" and leaves PoolTag/Difficulty unset - insufficient for this
// file's per-algo/per-pool/per-difficulty analysis-route tests).
func seedBlockFull(t *testing.T, d *db.DB, height uint64, powAlgo string, poolTag *string, difficulty int64) {
	t.Helper()
	err := d.UpsertBlock(context.Background(), db.Block{
		Height:            height,
		Hash:              "aa",
		PrevHash:          "bb",
		OutputMr:          []byte{},
		BlockOutputMr:     []byte{},
		KernelMr:          []byte{},
		InputMr:           []byte{},
		TotalKernelOffset: []byte{},
		TotalScriptOffset: []byte{},
		ValidatorNodeMr:   []byte{},
		PowData:           []byte{},
		PowAlgo:           powAlgo,
		PoolTag:           poolTag,
		Difficulty:        difficulty,
	})
	if err != nil {
		t.Fatalf("server: seed block full %d: %v", height, err)
	}
}

// ---- GET /api/blocks ----

func TestHandleAPIBlocks_HappyPath(t *testing.T) {
	d := openTestDB(t)
	seedBlock(t, d, 100)
	seedBlock(t, d, 101)
	seedBlock(t, d, 102)

	s, err := New(d, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/blocks", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	assertJSONContentType(t, rec)

	blocks := decodeJSON[[]db.Block](t, rec)
	if len(blocks) != 3 {
		t.Fatalf("len(blocks) = %d, want 3", len(blocks))
	}
	// ListBlocks orders height DESC.
	if blocks[0].Height != 102 || blocks[1].Height != 101 || blocks[2].Height != 100 {
		t.Errorf("unexpected height order: %+v", blocks)
	}
}

func TestHandleAPIBlocks_LimitParam(t *testing.T) {
	d := openTestDB(t)
	for h := uint64(1); h <= 5; h++ {
		seedBlock(t, d, h)
	}

	s, err := New(d, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/blocks?limit=2", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	blocks := decodeJSON[[]db.Block](t, rec)
	if len(blocks) != 2 {
		t.Fatalf("len(blocks) = %d, want 2 (limit=2)", len(blocks))
	}
}

// TestHandleAPIBlocks_LimitClampedToMax proves a ?limit= above MaxAPILimit is
// silently clamped down to it, rather than honored verbatim or rejected.
func TestHandleAPIBlocks_LimitClampedToMax(t *testing.T) {
	d := openTestDB(t)
	for h := uint64(1); h <= 3; h++ {
		seedBlock(t, d, h)
	}
	s, err := New(d, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/blocks?limit=99999", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	blocks := decodeJSON[[]db.Block](t, rec)
	if len(blocks) != 3 {
		t.Fatalf("len(blocks) = %d, want 3 (only 3 blocks exist)", len(blocks))
	}
}

func TestHandleAPIBlocks_BeforeParam(t *testing.T) {
	d := openTestDB(t)
	seedBlock(t, d, 10)
	seedBlock(t, d, 20)
	seedBlock(t, d, 30)

	s, err := New(d, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/blocks?before=20", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	blocks := decodeJSON[[]db.Block](t, rec)
	if len(blocks) != 1 || blocks[0].Height != 10 {
		t.Fatalf("blocks = %+v, want just height 10 (strictly below before=20)", blocks)
	}
}

func TestHandleAPIBlocks_InvalidBeforeParam(t *testing.T) {
	d := openTestDB(t)
	s, err := New(d, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/blocks?before=notanumber", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	errResp := decodeJSON[map[string]string](t, rec)
	if errResp["error"] == "" {
		t.Errorf("expected a non-empty JSON error message, got %+v", errResp)
	}
}

func TestHandleAPIBlocks_InvalidLimitParam(t *testing.T) {
	d := openTestDB(t)
	s, err := New(d, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/blocks?limit=0", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestHandleAPIBlocks_EmptyResultIsEmptyArrayNotNull proves an empty result set
// marshals as `[]`, never the literal `null`.
func TestHandleAPIBlocks_EmptyResultIsEmptyArrayNotNull(t *testing.T) {
	d := openTestDB(t)
	s, err := New(d, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/blocks", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if body == "null\n" || body == "null" {
		t.Fatalf("body = %q, want an empty JSON array, not null", body)
	}
	blocks := decodeJSON[[]db.Block](t, rec)
	if blocks == nil {
		t.Errorf("decoded blocks slice is nil, want non-nil empty slice")
	}
	if len(blocks) != 0 {
		t.Errorf("len(blocks) = %d, want 0", len(blocks))
	}
}

// ---- GET /api/blocks/{height} ----

func TestHandleAPIBlockDetail_HappyPath(t *testing.T) {
	d := openTestDB(t)
	seedBlock(t, d, 500)
	ctx := context.Background()
	if err := d.ReplaceKernelsForBlock(ctx, 500, []db.Kernel{
		{BlockHeight: 500, Index: 0, Fee: 123, Excess: []byte{0xaa}, ExcessSigNonce: []byte{0xbb}, ExcessSigSignature: []byte{0xcc}, Hash: []byte{0xdd}},
	}); err != nil {
		t.Fatalf("ReplaceKernelsForBlock: %v", err)
	}
	if err := d.ReplaceOutputsForBlock(ctx, 500, []db.Output{
		{BlockHeight: 500, Index: 0, OutputType: 1, Commitment: []byte{0xee}},
	}); err != nil {
		t.Fatalf("ReplaceOutputsForBlock: %v", err)
	}

	s, err := New(d, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/blocks/500", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	assertJSONContentType(t, rec)

	got := decodeJSON[apiBlockDetail](t, rec)
	if got.Block.Height != 500 {
		t.Errorf("Block.Height = %d, want 500", got.Block.Height)
	}
	if len(got.Kernels) != 1 || got.Kernels[0].Fee != 123 {
		t.Errorf("Kernels = %+v, want one kernel with Fee 123", got.Kernels)
	}
	if len(got.Outputs) != 1 || got.Outputs[0].OutputType != 1 {
		t.Errorf("Outputs = %+v, want one output with OutputType 1", got.Outputs)
	}
}

func TestHandleAPIBlockDetail_NotFound(t *testing.T) {
	d := openTestDB(t)
	s, err := New(d, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/blocks/999999", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404, body: %s", rec.Code, rec.Body.String())
	}
	assertJSONContentType(t, rec)
	errResp := decodeJSON[map[string]string](t, rec)
	if errResp["error"] == "" {
		t.Errorf("expected a non-empty JSON error message, got %+v", errResp)
	}
}

func TestHandleAPIBlockDetail_InvalidHeight(t *testing.T) {
	d := openTestDB(t)
	s, err := New(d, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/blocks/not-a-height", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// ---- GET /api/pool-stats ----

// fakePoolStatsProvider is a minimal poolstats.PoolStatsProvider test double - this
// package has no existing reusable fake for that interface, so one is added here,
// scoped to this test file only.
type fakePoolStatsProvider struct {
	stats poolstats.PoolStats
	err   error
}

func (f *fakePoolStatsProvider) GetStats(ctx context.Context) (poolstats.PoolStats, error) {
	if f.err != nil {
		return poolstats.PoolStats{}, f.err
	}
	return f.stats, nil
}

func TestHandleAPIPoolStats_NotConfigured(t *testing.T) {
	d := openTestDB(t)
	s, err := New(d, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/pool-stats", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	assertJSONContentType(t, rec)
}

func TestHandleAPIPoolStats_HappyPath(t *testing.T) {
	d := openTestDB(t)
	provider := &fakePoolStatsProvider{stats: poolstats.PoolStats{
		HashRate:         12345,
		Miners:           7,
		TotalBlocksFound: 42,
	}}
	s, err := New(d, provider, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/pool-stats", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	got := decodeJSON[poolstats.PoolStats](t, rec)
	if got.HashRate != 12345 || got.Miners != 7 || got.TotalBlocksFound != 42 {
		t.Errorf("got = %+v, want HashRate=12345 Miners=7 TotalBlocksFound=42", got)
	}
}

func TestHandleAPIPoolStats_FetchError(t *testing.T) {
	d := openTestDB(t)
	provider := &fakePoolStatsProvider{err: errors.New("boom")}
	s, err := New(d, provider, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/pool-stats", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// ---- GET /api/analysis/* ----

func TestHandleAPIAnalysisAlgoDistribution_HappyPath(t *testing.T) {
	d := openTestDB(t)
	seedBlockFull(t, d, 1000, "RXM", nil, 100)
	seedBlockFull(t, d, 1001, "RXT", nil, 200)

	s, err := New(d, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/analysis/algo-distribution?bucket_size=1000&from=0&to=2000", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	assertJSONContentType(t, rec)
	rows := decodeJSON[[]db.AlgoBucketRow](t, rec)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1 bucket", len(rows))
	}
	if rows[0].RXM != 1 || rows[0].RXT != 1 {
		t.Errorf("rows[0] = %+v, want RXM=1 RXT=1", rows[0])
	}
}

func TestHandleAPIAnalysisDifficulty_NullForNoData(t *testing.T) {
	d := openTestDB(t)
	seedBlockFull(t, d, 1000, "RXM", nil, 555)

	s, err := New(d, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/analysis/difficulty?bucket_size=1000&from=0&to=2000", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	rows := decodeJSON[[]db.DifficultyBucketRow](t, rec)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1 bucket", len(rows))
	}
	if rows[0].RXM == nil || *rows[0].RXM != 555 {
		t.Errorf("rows[0].RXM = %v, want pointer to 555", rows[0].RXM)
	}
	if rows[0].RXT != nil {
		t.Errorf("rows[0].RXT = %v, want nil (no RXT blocks in bucket)", rows[0].RXT)
	}
}

func TestHandleAPIAnalysisRewardsByAlgo_RawMicroMinotari(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if err := d.UpsertBlock(ctx, db.Block{
		Height: 2000, Hash: "aa", PrevHash: "bb",
		OutputMr: []byte{}, BlockOutputMr: []byte{}, KernelMr: []byte{}, InputMr: []byte{},
		TotalKernelOffset: []byte{}, TotalScriptOffset: []byte{}, ValidatorNodeMr: []byte{}, PowData: []byte{},
		PowAlgo: "RXM", RewardMicroMinotari: 5_000_000,
	}); err != nil {
		t.Fatalf("UpsertBlock: %v", err)
	}

	s, err := New(d, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/analysis/rewards-by-algo?bucket_size=1000&from=0&to=3000", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	rows := decodeJSON[[]db.RewardBucketRow](t, rec)
	if len(rows) != 1 || rows[0].RXM != 5_000_000 {
		t.Fatalf("rows = %+v, want one bucket with RXM=5000000 (raw MicroMinotari, no XTM conversion)", rows)
	}
}

func TestHandleAPIAnalysisPoolAlgoBreakdown_DefaultPoolFromMapping(t *testing.T) {
	d := openTestDB(t)
	tag := "WUFJagtechE0"
	seedBlockFull(t, d, 3000, "RXM", &tag, 1)

	s, err := New(d, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// No ?pool= - should default to the first analysis.DefaultPoolTagMappings entry
	// ("Jagtech"), matching parsePoolAlgoBreakdownPool's own HTML-route behavior.
	req := httptest.NewRequest("GET", "/api/analysis/pool-algo-breakdown?bucket_size=1000&from=0&to=4000", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	rows := decodeJSON[[]db.AlgoBucketRow](t, rec)
	if len(rows) != 1 || rows[0].RXM != 1 {
		t.Fatalf("rows = %+v, want one bucket with RXM=1 (WUFJagtechE0 folds into default Jagtech pool)", rows)
	}
}

func TestHandleAPIAnalysisPoolAlgoBreakdown_ExplicitPoolParam(t *testing.T) {
	d := openTestDB(t)
	tag := "some-other-pool"
	seedBlockFull(t, d, 3000, "RXT", &tag, 1)

	s, err := New(d, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/analysis/pool-algo-breakdown?bucket_size=1000&from=0&to=4000&pool=some-other-pool", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	rows := decodeJSON[[]db.AlgoBucketRow](t, rec)
	if len(rows) != 1 || rows[0].RXT != 1 {
		t.Fatalf("rows = %+v, want one bucket with RXT=1 for the literal unmapped pool tag", rows)
	}
}

func TestHandleAPIAnalysisPoolShare_HappyPath(t *testing.T) {
	d := openTestDB(t)
	tag := "WUFJagtechE0"
	seedBlockFull(t, d, 4000, "RXM", &tag, 1)
	seedBlockFull(t, d, 4001, "RXM", nil, 1)

	s, err := New(d, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/analysis/pool-share?bucket_size=1000&from=0&to=5000", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	rows := decodeJSON[[]db.PoolShareBucketRow](t, rec)
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2 (one 'Jagtech' row, one 'unknown' row)", len(rows))
	}
}

func TestHandleAPIAnalysisBlockTime_HappyPath(t *testing.T) {
	d := openTestDB(t)
	seedBlock(t, d, 5000)
	seedBlock(t, d, 5001)

	s, err := New(d, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/analysis/block-time?bucket_size=1000&from=0&to=6000", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	rows := decodeJSON[[]db.BlockTimeBucketRow](t, rec)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1 bucket", len(rows))
	}
}

func TestHandleAPIAnalysisRewardsByPool_HappyPath(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	tag := "WUFJagtechE0"
	if err := d.UpsertBlock(ctx, db.Block{
		Height: 6000, Hash: "aa", PrevHash: "bb",
		OutputMr: []byte{}, BlockOutputMr: []byte{}, KernelMr: []byte{}, InputMr: []byte{},
		TotalKernelOffset: []byte{}, TotalScriptOffset: []byte{}, ValidatorNodeMr: []byte{}, PowData: []byte{},
		PowAlgo: "RXM", PoolTag: &tag, RewardMicroMinotari: 1_000_000,
	}); err != nil {
		t.Fatalf("UpsertBlock: %v", err)
	}

	s, err := New(d, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/analysis/rewards-by-pool?bucket_size=1000&from=0&to=7000", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	rows := decodeJSON[[]db.RewardPoolBucketRow](t, rec)
	if len(rows) != 1 || rows[0].Reward != 1_000_000 {
		t.Fatalf("rows = %+v, want one bucket with Reward=1000000 (raw MicroMinotari)", rows)
	}
}

// ---- GET /api/tip-info ----

// fakeTipInfoBaseNodeServer is a real in-process GRPC BaseNode server (bufconn),
// same pattern internal/nodeclient's own nodeclient_test.go and internal/txsearch's
// txsearch_test.go already use for exercising a real *nodeclient.Client end-to-end
// rather than mocking the client interface away.
type fakeTipInfoBaseNodeServer struct {
	tari_generated.UnimplementedBaseNodeServer
	resp *tari_generated.TipInfoResponse
	err  error
}

func (f *fakeTipInfoBaseNodeServer) GetTipInfo(ctx context.Context, _ *tari_generated.Empty) (*tari_generated.TipInfoResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// startFakeTipInfoServer boots fake on a bufconn listener and returns a real
// *nodeclient.Client dialed against it, plus a cleanup func.
func startFakeTipInfoServer(t *testing.T, fake *fakeTipInfoBaseNodeServer) (*nodeclient.Client, func()) {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	tari_generated.RegisterBaseNodeServer(grpcServer, fake)
	go func() { _ = grpcServer.Serve(lis) }()

	dialer := grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	})

	client, err := nodeclient.New([]string{"passthrough:///bufnet"}, dialer, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("nodeclient.New: %v", err)
	}

	cleanup := func() {
		_ = client.Close()
		grpcServer.Stop()
		_ = lis.Close()
	}
	return client, cleanup
}

func TestHandleAPITipInfo_NodeNotConfigured(t *testing.T) {
	d := openTestDB(t)
	s, err := New(d, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/tip-info", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503, body: %s", rec.Code, rec.Body.String())
	}
	assertJSONContentType(t, rec)
}

// TestHandleAPITipInfo_HappyPath proves the real tari_generated.TipInfoResponse/
// MetaData fields are surfaced correctly, with BestBlockHash/AccumulatedDifficulty
// hex-encoded (not base64) and BaseNodeState rendered as its human enum name.
func TestHandleAPITipInfo_HappyPath(t *testing.T) {
	d := openTestDB(t)
	fake := &fakeTipInfoBaseNodeServer{
		resp: &tari_generated.TipInfoResponse{
			Metadata: &tari_generated.MetaData{
				BestBlockHeight:       123456,
				BestBlockHash:         []byte{0xde, 0xad, 0xbe, 0xef},
				AccumulatedDifficulty: []byte{0x01, 0x02},
				PrunedHeight:          0,
				Timestamp:             1700000000,
			},
			InitialSyncAchieved: true,
			BaseNodeState:       tari_generated.BaseNodeState_LISTENING,
			FailedCheckpoints:   false,
		},
	}
	node, cleanup := startFakeTipInfoServer(t, fake)
	defer cleanup()

	s, err := New(d, nil, "", nil, node)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/tip-info", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	assertJSONContentType(t, rec)

	got := decodeJSON[apiTipInfo](t, rec)
	if got.BestBlockHeight != 123456 {
		t.Errorf("BestBlockHeight = %d, want 123456", got.BestBlockHeight)
	}
	if got.BestBlockHash != "deadbeef" {
		t.Errorf("BestBlockHash = %q, want %q (hex, not base64)", got.BestBlockHash, "deadbeef")
	}
	if got.AccumulatedDifficulty != "0102" {
		t.Errorf("AccumulatedDifficulty = %q, want %q", got.AccumulatedDifficulty, "0102")
	}
	if !got.InitialSyncAchieved {
		t.Errorf("InitialSyncAchieved = false, want true")
	}
	if got.BaseNodeState != "LISTENING" {
		t.Errorf("BaseNodeState = %q, want %q", got.BaseNodeState, "LISTENING")
	}
	if got.FailedCheckpoints {
		t.Errorf("FailedCheckpoints = true, want false")
	}
}

func TestHandleAPITipInfo_GRPCCallFails(t *testing.T) {
	d := openTestDB(t)
	fake := &fakeTipInfoBaseNodeServer{err: status.Error(codes.Unavailable, "boom")}
	node, cleanup := startFakeTipInfoServer(t, fake)
	defer cleanup()

	s, err := New(d, nil, "", nil, node)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/tip-info", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503, body: %s", rec.Code, rec.Body.String())
	}
	assertJSONContentType(t, rec)
}

// ---- GET /api/health ----

func TestHandleAPIHealth_DatabaseReachable(t *testing.T) {
	d := openTestDB(t)
	s, err := New(d, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/health", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	got := decodeJSON[map[string]string](t, rec)
	if got["status"] != "ok" || got["database"] != "reachable" {
		t.Errorf("got = %+v, want status=ok database=reachable", got)
	}
}

// TestHandleAPIHealth_DatabaseDegraded proves a closed/unreachable Postgres pool
// still yields a 200 with "database": "degraded" in the body, never a non-200 status
// - the "always 200, degraded reported in body" convention.
func TestHandleAPIHealth_DatabaseDegraded(t *testing.T) {
	d := openTestDB(t)
	s, err := New(d, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.Pool.Close() // force every subsequent query to fail

	req := httptest.NewRequest("GET", "/api/health", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (degraded is reported in the body, not the status)", rec.Code)
	}
	got := decodeJSON[map[string]string](t, rec)
	if got["status"] != "ok" || got["database"] != "degraded" {
		t.Errorf("got = %+v, want status=ok database=degraded", got)
	}
	if got["error"] == "" {
		t.Errorf("expected a non-empty error detail, got %+v", got)
	}
}

// ---- GET /api/spec ----
//
// Neither /api/spec nor /api/docs (below) touch s.DB/s.PoolStats/s.Node at all (see
// their handlers in api.go), so these tests construct a Server with a nil *db.DB
// rather than requiring a real reachable Postgres instance like every other test in
// this file - New itself never dereferences database, only stores it.

func TestHandleAPISpec_YAML(t *testing.T) {
	s, err := New(nil, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/spec", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/yaml; charset=utf-8" {
		t.Errorf("Content-Type = %q, want %q", got, "application/yaml; charset=utf-8")
	}
	body := rec.Body.Bytes()
	if len(body) == 0 {
		t.Fatal("body is empty, want non-empty YAML spec")
	}
	var doc map[string]any
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("body is not valid YAML: %v; body=%s", err, body)
	}
	if doc["openapi"] == nil {
		t.Errorf("parsed YAML missing top-level \"openapi\" key, got: %+v", doc)
	}
	if _, ok := doc["paths"].(map[string]any); !ok {
		t.Errorf("parsed YAML missing top-level \"paths\" map, got: %+v", doc["paths"])
	}
}

func TestHandleAPISpec_JSONFormat(t *testing.T) {
	s, err := New(nil, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/spec?format=json", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	assertJSONContentType(t, rec)
	doc := decodeJSON[map[string]any](t, rec)
	if doc["openapi"] == nil {
		t.Errorf("parsed JSON missing top-level \"openapi\" key, got: %+v", doc)
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatalf("parsed JSON missing top-level \"paths\" map, got: %+v", doc["paths"])
	}
	for _, route := range []string{
		"/api/blocks", "/api/blocks/{height}", "/api/pool-stats",
		"/api/analysis/algo-distribution", "/api/analysis/pool-share",
		"/api/analysis/pool-algo-breakdown", "/api/analysis/block-time",
		"/api/analysis/difficulty", "/api/analysis/rewards-by-algo",
		"/api/analysis/rewards-by-pool", "/api/tip-info", "/api/health",
	} {
		if _, present := paths[route]; !present {
			t.Errorf("parsed JSON spec missing path %q", route)
		}
	}
}

// ---- GET /api/docs ----

func TestHandleAPIDocs(t *testing.T) {
	s, err := New(nil, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/docs", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want %q", got, "text/html; charset=utf-8")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "swagger-ui-bundle.js") {
		t.Errorf("body missing Swagger UI CDN script reference, got: %s", body)
	}
	if !strings.Contains(body, "/api/spec") {
		t.Errorf("body missing reference to /api/spec as the Swagger UI spec URL, got: %s", body)
	}
}

// TestHandleAPIDocs_DarkThemeOverrides is a cheap regression guard (not a substitute
// for the real browser-rendered visual verification this fix was checked with) against
// someone reverting/trimming the <style> block's dark-theme contrast overrides for
// Swagger UI v5's default light theme. Each substring below is a real selector copied
// verbatim from https://unpkg.com/swagger-ui-dist@5/swagger-ui.css (matching its exact
// specificity so the cascade favors this page's later <style> block without
// `!important`) - see templates/api_docs.html's own comments for the rationale per
// selector.
func TestHandleAPIDocs_DarkThemeOverrides(t *testing.T) {
	s, err := New(nil, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/docs", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		// Main info-paragraph body text (title/headings/body/base-url/links).
		".swagger-ui .info p,",
		".swagger-ui .info .title { color: #e6e6e6; }",
		".swagger-ui .info .base-url { color: #999; }",
		// Operation summary + expanded description text.
		".swagger-ui .opblock .opblock-summary-description { color: #999; }",
		".swagger-ui .opblock-description-wrapper p,",
		// Parameters/Responses tab bar - the same "stark white bar" issue as Servers.
		".swagger-ui .opblock .opblock-section-header { background: #17171b; }",
		".swagger-ui .parameter__name { color: #e6e6e6; }",
		// Schemas/Models section: model text + the newer json-schema-2020-12 viewer.
		".swagger-ui .model { color: #e6e6e6; }",
		".swagger-ui .model-title { color: #e6e6e6; }",
		".swagger-ui .json-schema-2020-12-property .json-schema-2020-12__title { color: #e6e6e6; }",
		// The "Servers" dropdown bar's white background, and native <select> chrome.
		".swagger-ui .scheme-container { background: #17171b; }",
		".swagger-ui select {",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing expected dark-theme override %q", want)
		}
	}
}
