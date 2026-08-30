package analysis

import (
	"context"
	"os"
	"testing"

	"github.com/Snipa22/go-tari-explorer/internal/db"
)

// testDSN resolves the Postgres connection string used by these tests: an explicit
// TARI_EXPLORER_TEST_DSN environment variable if set, otherwise the local embedded
// Postgres instance used for this repo's own agent-driven development/testing
// (postgres 17, socket-less TCP on 5433, trust auth, tari_explorer_test database).
func testDSN() string {
	if dsn := os.Getenv("TARI_EXPLORER_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://postgres@/tari_explorer_test?host=/workspace/pg-embed/sockets&port=5433&sslmode=disable"
}

// setupTestDB connects to testDSN, runs migrations, and truncates `blocks` so each test
// starts from a clean slate (tests in this package run sequentially and share the same
// database, matching how a real single-writer indexer would populate it). Skips the
// test (not fails) if the database is unreachable, so this suite doesn't break CI
// environments without a live Postgres.
func setupTestDB(t *testing.T) *db.DB {
	t.Helper()
	ctx := context.Background()
	database, err := db.Connect(ctx, testDSN())
	if err != nil {
		t.Skipf("analysis: skipping DB-backed test, cannot connect to %s: %v", testDSN(), err)
	}
	if err := database.Migrate(ctx); err != nil {
		database.Close()
		t.Fatalf("analysis: migrate: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `TRUNCATE TABLE blocks CASCADE`); err != nil {
		database.Close()
		t.Fatalf("analysis: truncate blocks: %v", err)
	}
	t.Cleanup(database.Close)
	return database
}

// seedBlock inserts one fixture block row with the fields the analysis queries care
// about (height, timestamp, pow_algo, difficulty, pool_tag); every other Block field is
// left at its zero value since none of the analysis queries touch them.
func seedBlock(t *testing.T, database *db.DB, height uint64, timestamp int64, powAlgo string, difficulty int64, poolTag *string) {
	t.Helper()
	b := db.Block{
		Height:            height,
		Hash:              "hash",
		PrevHash:          "prev",
		Timestamp:         timestamp,
		OutputMr:          []byte{},
		BlockOutputMr:     []byte{},
		KernelMr:          []byte{},
		InputMr:           []byte{},
		TotalKernelOffset: []byte{},
		TotalScriptOffset: []byte{},
		ValidatorNodeMr:   []byte{},
		PowData:           []byte{},
		PowAlgo:           powAlgo,
		Difficulty:        difficulty,
		PoolTag:           poolTag,
	}
	if err := database.UpsertBlock(context.Background(), b); err != nil {
		t.Fatalf("analysis: seed block %d: %v", height, err)
	}
}

func strPtr(s string) *string { return &s }

func TestAlgoDistribution(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()

	// Bucket [0,999]: 2 RXM, 1 RXT, 1 C29, 0 SHA3X.
	seedBlock(t, database, 0, 1000, "RXM", 100, nil)
	seedBlock(t, database, 1, 1010, "RXM", 100, nil)
	seedBlock(t, database, 2, 1020, "RXT", 100, nil)
	seedBlock(t, database, 3, 1030, "C29", 100, nil)
	// Bucket [1000,1999]: 1 SHA3X.
	seedBlock(t, database, 1000, 1040, "SHA3X", 100, nil)

	points, order, err := AlgoDistribution(ctx, database, 1000, 0, 1999)
	if err != nil {
		t.Fatalf("AlgoDistribution: %v", err)
	}
	if got, want := order, AlgoOrder; len(got) != len(want) {
		t.Fatalf("series order = %v, want %v", got, want)
	}
	if len(points) != 2 {
		t.Fatalf("len(points) = %d, want 2", len(points))
	}
	// points aren't guaranteed sorted by AlgoDistribution itself (chartrender sorts at
	// render time), so index by X.
	byX := map[float64]map[string]float64{}
	for _, p := range points {
		byX[p.X] = p.Series
	}
	b0, ok := byX[0]
	if !ok {
		t.Fatalf("missing bucket 0 in points: %+v", points)
	}
	if b0["RXM"] != 2 || b0["RXT"] != 1 || b0["C29"] != 1 || b0["SHA3X"] != 0 {
		t.Errorf("bucket 0 = %+v, want RXM=2 RXT=1 C29=1 SHA3X=0", b0)
	}
	b1, ok := byX[1000]
	if !ok {
		t.Fatalf("missing bucket 1000 in points: %+v", points)
	}
	if b1["SHA3X"] != 1 {
		t.Errorf("bucket 1000 SHA3X = %v, want 1", b1["SHA3X"])
	}
}

func TestPoolShare(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()

	poolA, poolB, poolC := strPtr("poolA"), strPtr("poolB"), strPtr("poolC")
	// Bucket [0,999]: poolA x3, poolB x2, poolC x1, unknown(nil) x1.
	seedBlock(t, database, 0, 1000, "RXM", 100, poolA)
	seedBlock(t, database, 1, 1010, "RXM", 100, poolA)
	seedBlock(t, database, 2, 1020, "RXM", 100, poolA)
	seedBlock(t, database, 3, 1030, "RXM", 100, poolB)
	seedBlock(t, database, 4, 1040, "RXM", 100, poolB)
	seedBlock(t, database, 5, 1050, "RXM", 100, poolC)
	seedBlock(t, database, 6, 1060, "RXM", 100, nil)

	// topN=2, no mappings: poolA + poolB are kept as their own series, poolC folds
	// into "other", nil folds into "unknown".
	points, order, err := PoolShare(ctx, database, 1000, 0, 999, 2, nil)
	if err != nil {
		t.Fatalf("PoolShare: %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("len(points) = %d, want 1", len(points))
	}
	series := points[0].Series
	if series["poolA"] != 3 {
		t.Errorf("poolA = %v, want 3", series["poolA"])
	}
	if series["poolB"] != 2 {
		t.Errorf("poolB = %v, want 2", series["poolB"])
	}
	if series["other"] != 1 {
		t.Errorf("other = %v, want 1", series["other"])
	}
	if series["unknown"] != 1 {
		t.Errorf("unknown = %v, want 1", series["unknown"])
	}
	// order: real pools by descending total (poolA, poolB), then unknown, then other.
	wantOrder := []string{"poolA", "poolB", "unknown", "other"}
	if len(order) != len(wantOrder) {
		t.Fatalf("order = %v, want %v", order, wantOrder)
	}
	for i, name := range wantOrder {
		if order[i] != name {
			t.Errorf("order[%d] = %q, want %q (full order %v)", i, order[i], name, order)
		}
	}
}

// TestPoolShare_WithMappings proves the WUF-family folding mechanism itself: multiple
// distinct WUF-prefixed pool_tag values (different node suffixes) must be merged into
// one "Jagtech" series, while a non-WUF pool tag and a NULL pool_tag remain their own
// separate series.
func TestPoolShare_WithMappings(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()

	e0, s1, ahri := strPtr("WUFJagtechE0"), strPtr("WUFJagtechS1"), strPtr("WUF  Ahri   ")
	other := strPtr("pool.kryptex.com")
	// Bucket [0,999]: WUFJagtechE0 x2, WUFJagtechS1 x2, WUF  Ahri   x1 (all "Jagtech"
	// once mapped = 5), pool.kryptex.com x2 (unmapped, own series), nil x1 (unknown).
	seedBlock(t, database, 0, 1000, "RXM", 100, e0)
	seedBlock(t, database, 1, 1010, "RXM", 100, e0)
	seedBlock(t, database, 2, 1020, "RXT", 100, s1)
	seedBlock(t, database, 3, 1030, "C29", 100, s1)
	seedBlock(t, database, 4, 1040, "SHA3X", 100, ahri)
	seedBlock(t, database, 5, 1050, "RXM", 100, other)
	seedBlock(t, database, 6, 1060, "RXM", 100, other)
	seedBlock(t, database, 7, 1070, "RXM", 100, nil)

	mappings := []db.PoolTagMapping{{MatchPrefix: "WUF", CanonicalName: "Jagtech"}}
	points, order, err := PoolShare(ctx, database, 1000, 0, 999, 8, mappings)
	if err != nil {
		t.Fatalf("PoolShare: %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("len(points) = %d, want 1", len(points))
	}
	series := points[0].Series
	if series["Jagtech"] != 5 {
		t.Errorf("Jagtech = %v, want 5 (merged WUFJagtechE0+WUFJagtechS1+WUF  Ahri   )", series["Jagtech"])
	}
	if series["pool.kryptex.com"] != 2 {
		t.Errorf("pool.kryptex.com = %v, want 2 (must stay its own series, unaffected by WUF mapping)", series["pool.kryptex.com"])
	}
	if series["unknown"] != 1 {
		t.Errorf("unknown = %v, want 1", series["unknown"])
	}
	if _, ok := series["WUFJagtechE0"]; ok {
		t.Errorf("raw WUFJagtechE0 must not appear as its own series once mapped, got series=%+v", series)
	}
	wantOrder := []string{"Jagtech", "pool.kryptex.com", "unknown"}
	if len(order) != len(wantOrder) {
		t.Fatalf("order = %v, want %v", order, wantOrder)
	}
	for i, name := range wantOrder {
		if order[i] != name {
			t.Errorf("order[%d] = %q, want %q (full order %v)", i, order[i], name, order)
		}
	}
}

// TestPoolAlgoBreakdown proves the per-pool algo-breakdown query correctly scopes to a
// merged canonical pool name across multiple raw pool_tag prefixes, with real
// multi-algo fixture rows for the merged pool.
func TestPoolAlgoBreakdown(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()

	e0, s1, ahri := strPtr("WUFJagtechE0"), strPtr("WUFJagtechS1"), strPtr("WUF  Ahri   ")
	other := strPtr("pool.kryptex.com")
	// Bucket [0,999]: Jagtech-family blocks split across algos: RXM x2 (e0), RXT x1
	// (s1), C29 x1 (s1), SHA3X x1 (ahri). Plus a non-WUF pool block that must NOT be
	// counted in the breakdown.
	seedBlock(t, database, 0, 1000, "RXM", 100, e0)
	seedBlock(t, database, 1, 1010, "RXM", 100, e0)
	seedBlock(t, database, 2, 1020, "RXT", 100, s1)
	seedBlock(t, database, 3, 1030, "C29", 100, s1)
	seedBlock(t, database, 4, 1040, "SHA3X", 100, ahri)
	seedBlock(t, database, 5, 1050, "RXM", 100, other)

	mappings := []db.PoolTagMapping{{MatchPrefix: "WUF", CanonicalName: "Jagtech"}}
	points, order, err := PoolAlgoBreakdown(ctx, database, 1000, 0, 999, mappings, "Jagtech")
	if err != nil {
		t.Fatalf("PoolAlgoBreakdown: %v", err)
	}
	if len(order) != len(AlgoOrder) {
		t.Fatalf("order = %v, want %v", order, AlgoOrder)
	}
	if len(points) != 1 {
		t.Fatalf("len(points) = %d, want 1", len(points))
	}
	series := points[0].Series
	if series["RXM"] != 2 {
		t.Errorf("RXM = %v, want 2", series["RXM"])
	}
	if series["RXT"] != 1 {
		t.Errorf("RXT = %v, want 1", series["RXT"])
	}
	if series["C29"] != 1 {
		t.Errorf("C29 = %v, want 1", series["C29"])
	}
	if series["SHA3X"] != 1 {
		t.Errorf("SHA3X = %v, want 1", series["SHA3X"])
	}
}

// TestPoolAlgoBreakdown_UnmappedLiteral proves the same endpoint works for a canonical
// name absent from mappings entirely - treated as an exact, literal pool_tag match, so
// a pool operator who never needs per-node folding can still use this view.
func TestPoolAlgoBreakdown_UnmappedLiteral(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()

	kryptex := strPtr("pool.kryptex.com")
	seedBlock(t, database, 0, 1000, "RXM", 100, kryptex)
	seedBlock(t, database, 1, 1010, "SHA3X", 100, kryptex)

	mappings := []db.PoolTagMapping{{MatchPrefix: "WUF", CanonicalName: "Jagtech"}}
	points, _, err := PoolAlgoBreakdown(ctx, database, 1000, 0, 999, mappings, "pool.kryptex.com")
	if err != nil {
		t.Fatalf("PoolAlgoBreakdown: %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("len(points) = %d, want 1", len(points))
	}
	series := points[0].Series
	if series["RXM"] != 1 || series["SHA3X"] != 1 {
		t.Errorf("series = %+v, want RXM=1 SHA3X=1", series)
	}
}

func TestBlockTime(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()

	// Heights 0..3 with deltas 10, 20, 30 seconds (median 20), all in bucket [0,999].
	seedBlock(t, database, 0, 1000, "RXM", 100, nil)
	seedBlock(t, database, 1, 1010, "RXM", 100, nil) // delta 10
	seedBlock(t, database, 2, 1030, "RXM", 100, nil) // delta 20
	seedBlock(t, database, 3, 1060, "RXM", 100, nil) // delta 30

	points, summary, err := BlockTime(ctx, database, 1000, 0, 3)
	if err != nil {
		t.Fatalf("BlockTime: %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("len(points) = %d, want 1", len(points))
	}
	if got := points[0].Series[blockTimeSeriesName]; got != 20 {
		t.Errorf("median block time = %v, want 20", got)
	}
	if summary.SampleCount != 3 {
		t.Errorf("summary.SampleCount = %d, want 3", summary.SampleCount)
	}
	if summary.Mean == nil || *summary.Mean != 20 {
		t.Errorf("summary.Mean = %v, want 20", summary.Mean)
	}
	if summary.Median == nil || *summary.Median != 20 {
		t.Errorf("summary.Median = %v, want 20", summary.Median)
	}
	if summary.Max == nil || *summary.Max != 30 {
		t.Errorf("summary.Max = %v, want 30", summary.Max)
	}
	if summary.StdDev == nil {
		t.Errorf("summary.StdDev = nil, want a value")
	}
}

func TestBlockTime_NoSamples(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()

	// Single block with no predecessor row -> zero usable samples.
	seedBlock(t, database, 500, 1000, "RXM", 100, nil)

	points, summary, err := BlockTime(ctx, database, 1000, 0, 999)
	if err != nil {
		t.Fatalf("BlockTime: %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("len(points) = %d, want 1", len(points))
	}
	if got := points[0].Series[blockTimeSeriesName]; got != 0 {
		t.Errorf("median block time with no samples = %v, want 0", got)
	}
	if summary.SampleCount != 0 {
		t.Errorf("summary.SampleCount = %d, want 0", summary.SampleCount)
	}
	if summary.Mean != nil {
		t.Errorf("summary.Mean = %v, want nil", *summary.Mean)
	}
}

// TestBlockTime_BucketMeanStdDevMax proves db.BlockTimeDeltaBuckets' per-bucket
// mean/median/stddev/max fields (added alongside the existing MedianSeconds/
// SampleCount for the block-time data table - see internal/server's
// newBlockTimeBucketTableView) come back correct, using the exact same fixture blocks
// (heights 0-3, deltas 10/20/30) as TestBlockTime above, via a direct
// db.BlockTimeDeltaBuckets call - the same parallel/direct-call pattern
// handleAnalysisBlockTime now uses in production to get raw per-bucket rows for the
// table alongside analysis.BlockTime's chart Points.
func TestBlockTime_BucketMeanStdDevMax(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()

	// Heights 0..3 with deltas 10, 20, 30 seconds (mean 20, median 20, max 30), all in
	// bucket [0,999].
	seedBlock(t, database, 0, 1000, "RXM", 100, nil)
	seedBlock(t, database, 1, 1010, "RXM", 100, nil) // delta 10
	seedBlock(t, database, 2, 1030, "RXM", 100, nil) // delta 20
	seedBlock(t, database, 3, 1060, "RXM", 100, nil) // delta 30

	rows, err := database.BlockTimeDeltaBuckets(ctx, 1000, 0, 3)
	if err != nil {
		t.Fatalf("BlockTimeDeltaBuckets: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.SampleCount != 3 {
		t.Errorf("SampleCount = %d, want 3", r.SampleCount)
	}
	if r.MeanSeconds == nil || *r.MeanSeconds != 20 {
		t.Errorf("MeanSeconds = %v, want 20", r.MeanSeconds)
	}
	if r.MedianSeconds == nil || *r.MedianSeconds != 20 {
		t.Errorf("MedianSeconds = %v, want 20", r.MedianSeconds)
	}
	if r.MaxSeconds == nil || *r.MaxSeconds != 30 {
		t.Errorf("MaxSeconds = %v, want 30", r.MaxSeconds)
	}
	if r.StdDevSeconds == nil {
		t.Errorf("StdDevSeconds = nil, want a value")
	}
}

// TestBlockTime_BucketMeanStdDevMax_NoSamples proves a zero-sample bucket's
// MeanSeconds/MedianSeconds/StdDevSeconds/MaxSeconds all come back nil (not, say, a
// pointer to 0.0) and SampleCount is 0, via a direct db.BlockTimeDeltaBuckets call -
// parallel to TestBlockTime_NoSamples above, which covers the same zero-sample shape
// via analysis.BlockTime's chart-Points/summary return instead.
func TestBlockTime_BucketMeanStdDevMax_NoSamples(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()

	// Single block with no predecessor row -> zero usable samples.
	seedBlock(t, database, 500, 1000, "RXM", 100, nil)

	rows, err := database.BlockTimeDeltaBuckets(ctx, 1000, 0, 999)
	if err != nil {
		t.Fatalf("BlockTimeDeltaBuckets: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.SampleCount != 0 {
		t.Errorf("SampleCount = %d, want 0", r.SampleCount)
	}
	if r.MeanSeconds != nil {
		t.Errorf("MeanSeconds = %v, want nil", *r.MeanSeconds)
	}
	if r.MedianSeconds != nil {
		t.Errorf("MedianSeconds = %v, want nil", *r.MedianSeconds)
	}
	if r.StdDevSeconds != nil {
		t.Errorf("StdDevSeconds = %v, want nil", *r.StdDevSeconds)
	}
	if r.MaxSeconds != nil {
		t.Errorf("MaxSeconds = %v, want nil", *r.MaxSeconds)
	}
}

// TestDifficulty is the core regression test for per-algo average difficulty on the
// chart-reshaping layer: it seeds two algos with deliberately very different
// difficulty magnitudes in the same bucket so that an (incorrect) single blended
// average across all algos would be nowhere near either algo's true average, making a
// regression to the old blended-line behavior unmistakable. It also covers a bucket
// where one algo (C29) has zero blocks, asserting that algo's key is absent from that
// bucket's Series map entirely (not present with value 0).
func TestDifficulty(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()

	// Bucket [0,999]: RXM 1000/2000 (avg 1500), SHA3X 10/20 (avg 15). C29 has zero
	// blocks in this bucket.
	seedBlock(t, database, 0, 1000, "RXM", 1000, nil)
	seedBlock(t, database, 1, 1010, "RXM", 2000, nil)
	seedBlock(t, database, 2, 1020, "SHA3X", 10, nil)
	seedBlock(t, database, 3, 1030, "SHA3X", 20, nil)
	// Bucket [1000,1999]: RXM only, difficulty 500.
	seedBlock(t, database, 1000, 1040, "RXM", 500, nil)

	points, order, err := Difficulty(ctx, database, 1000, 0, 1999)
	if err != nil {
		t.Fatalf("Difficulty: %v", err)
	}
	if len(order) != len(AlgoOrder) {
		t.Fatalf("order = %v, want %v", order, AlgoOrder)
	}
	for i, name := range AlgoOrder {
		if order[i] != name {
			t.Errorf("order[%d] = %q, want %q (full order %v)", i, order[i], name, order)
		}
	}
	if len(points) != 2 {
		t.Fatalf("len(points) = %d, want 2", len(points))
	}
	byX := map[float64]map[string]float64{}
	for _, p := range points {
		byX[p.X] = p.Series
	}
	b0, ok := byX[0]
	if !ok {
		t.Fatalf("missing bucket 0 in points: %+v", points)
	}
	if b0["RXM"] != 1500 {
		t.Errorf("bucket 0 RXM avg difficulty = %v, want 1500", b0["RXM"])
	}
	if b0["SHA3X"] != 15 {
		t.Errorf("bucket 0 SHA3X avg difficulty = %v, want 15", b0["SHA3X"])
	}
	if _, ok := b0["C29"]; ok {
		t.Errorf("bucket 0 C29 must be absent from Series (zero blocks), got %+v", b0)
	}
	b1, ok := byX[1000]
	if !ok {
		t.Fatalf("missing bucket 1000 in points: %+v", points)
	}
	if b1["RXM"] != 500 {
		t.Errorf("bucket 1000 RXM avg difficulty = %v, want 500", b1["RXM"])
	}
}
