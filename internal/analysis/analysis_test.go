package analysis

import (
	"bytes"
	"context"
	"image/png"
	"os"
	"reflect"
	"testing"

	"github.com/Snipa22/go-tari-explorer/internal/chartrender"
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

// TestFilterAlgo is a table-driven, no-DB unit test for FilterAlgo proving: (a)
// filtering by one algo returns ONLY that algo's values, with zero cross-contamination
// from other algos' values even when they differ by many orders of magnitude, and (b)
// a bucket where the filtered algo had no data ends up with an empty Series map, not a
// spurious 0.0 entry.
func TestFilterAlgo(t *testing.T) {
	// RXM ~1e11-scale, C29 ~1e4-scale - deliberately many orders of magnitude apart,
	// so any accidental blending/leakage between the two would be unmistakable.
	points := []chartrender.Point{
		{X: 0, Series: map[string]float64{"RXM": 1.5e11, "RXT": 2.0e11, "C29": 12345, "SHA3X": 99}},
		// RXM absent in this bucket - must stay absent after filtering, not become 0.
		{X: 1000, Series: map[string]float64{"RXT": 3.0e11, "C29": 54321}},
		{X: 2000, Series: map[string]float64{"RXM": 9.9e11}},
	}

	cases := []struct {
		name string
		algo string
		want []map[string]float64
	}{
		{
			name: "RXM",
			algo: "RXM",
			want: []map[string]float64{
				{"RXM": 1.5e11},
				{},
				{"RXM": 9.9e11},
			},
		},
		{
			name: "C29",
			algo: "C29",
			want: []map[string]float64{
				{"C29": 12345},
				{"C29": 54321},
				{},
			},
		},
		{
			name: "algo with no data anywhere",
			algo: "SHA3X_NEVER_SEEN",
			want: []map[string]float64{{}, {}, {}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FilterAlgo(points, tc.algo)
			if len(got) != len(tc.want) {
				t.Fatalf("len(got) = %d, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i].X != points[i].X {
					t.Errorf("point %d: X = %v, want %v (X/order must be unchanged)", i, got[i].X, points[i].X)
				}
				if !reflect.DeepEqual(got[i].Series, tc.want[i]) {
					t.Errorf("point %d: Series = %+v, want %+v", i, got[i].Series, tc.want[i])
				}
				for other := range points[i].Series {
					if other == tc.algo {
						continue
					}
					if _, leaked := got[i].Series[other]; leaked {
						t.Errorf("point %d: filtering by %q leaked unrelated algo %q into Series: %+v", i, tc.algo, other, got[i].Series)
					}
				}
			}
		})
	}
}

// TestDifficulty_PerAlgoChartsIndependentlyRenderable is a DB-backed test proving the
// end-to-end per-algo-chart shape works with real, varying data across multiple
// buckets and orders-of-magnitude-different algos (RXM ~1e11-scale, SHA3X ~1e4-scale,
// each following a real increasing-then-decreasing trend, not a flat line): both
// algos' filtered points render to distinct, valid, non-trivial PNGs via
// FilterAlgo + chartrender.LineChart, and neither is byte-identical to the other or to
// chartrender's "not enough data" placeholder.
func TestDifficulty_PerAlgoChartsIndependentlyRenderable(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()

	// RXM: ~1e11-scale, rising then falling across 4 buckets.
	seedBlock(t, database, 0, 1000, "RXM", 100_000_000_000, nil)
	seedBlock(t, database, 1000, 1010, "RXM", 250_000_000_000, nil)
	seedBlock(t, database, 2000, 1020, "RXM", 400_000_000_000, nil)
	seedBlock(t, database, 3000, 1030, "RXM", 150_000_000_000, nil)
	// SHA3X: ~1e4-scale, rising then falling across the same 4 buckets.
	seedBlock(t, database, 0, 1040, "SHA3X", 5_000, nil)
	seedBlock(t, database, 1000, 1050, "SHA3X", 20_000, nil)
	seedBlock(t, database, 2000, 1060, "SHA3X", 45_000, nil)
	seedBlock(t, database, 3000, 1070, "SHA3X", 12_000, nil)

	points, _, err := Difficulty(ctx, database, 1000, 0, 3999)
	if err != nil {
		t.Fatalf("Difficulty: %v", err)
	}
	if len(points) != 4 {
		t.Fatalf("len(points) = %d, want 4", len(points))
	}

	rxmPNG, err := chartrender.LineChart(FilterAlgo(points, "RXM"), []string{"RXM"}, "Difficulty (RXM, avg, hashrate proxy)", "block height", "difficulty")
	if err != nil {
		t.Fatalf("LineChart(RXM): %v", err)
	}
	sha3xPNG, err := chartrender.LineChart(FilterAlgo(points, "SHA3X"), []string{"SHA3X"}, "Difficulty (SHA3X, avg, hashrate proxy)", "block height", "difficulty")
	if err != nil {
		t.Fatalf("LineChart(SHA3X): %v", err)
	}
	placeholderPNG, err := chartrender.NotEnoughDataPNG("Difficulty (RXM, avg, hashrate proxy)")
	if err != nil {
		t.Fatalf("NotEnoughDataPNG: %v", err)
	}

	for name, data := range map[string][]byte{"RXM": rxmPNG, "SHA3X": sha3xPNG} {
		if _, err := png.Decode(bytes.NewReader(data)); err != nil {
			t.Errorf("%s PNG failed to decode: %v", name, err)
		}
		// An arbitrary but generous floor: a real 960x400 rendered line chart is
		// comfortably larger than this; a degenerate/near-blank image would not be.
		if len(data) < 500 {
			t.Errorf("%s PNG suspiciously small (%d bytes), want a non-trivial real chart", name, len(data))
		}
	}
	if bytes.Equal(rxmPNG, sha3xPNG) {
		t.Error("RXM and SHA3X PNGs are byte-identical, want distinct per-algo charts")
	}
	if bytes.Equal(rxmPNG, placeholderPNG) {
		t.Error("RXM PNG is byte-identical to the not-enough-data placeholder")
	}
	if bytes.Equal(sha3xPNG, placeholderPNG) {
		t.Error("SHA3X PNG is byte-identical to the not-enough-data placeholder")
	}
}
