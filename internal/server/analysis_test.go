package server

import (
	"context"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/Snipa22/go-tari-explorer/internal/db"
)

// testDSN returns the Postgres connection string to run these tests against, matching
// internal/db's own db_test.go convention (see that file's testDSN doc comment) -
// override with TARI_EXPLORER_TEST_POSTGRES_DSN in CI or a different local setup.
func testDSN() string {
	if v := os.Getenv("TARI_EXPLORER_TEST_POSTGRES_DSN"); v != "" {
		return v
	}
	return "postgres://postgres@localhost:5433/tari_explorer_txcheck_test?sslmode=disable&host=/workspace/pg-embed/sockets"
}

// openTestDB connects to testDSN(), runs migrations, and truncates every table these
// tests touch so each test starts from a clean slate. Skips the test (not fails) if
// the database isn't reachable, matching internal/db's own openTestDB helper.
func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	ctx := context.Background()

	d, err := db.Connect(ctx, testDSN())
	if err != nil {
		t.Skipf("server: test postgres not reachable at %s: %v", testDSN(), err)
	}
	t.Cleanup(d.Close)

	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("server: migrate: %v", err)
	}
	if _, err := d.Pool.Exec(ctx, `TRUNCATE TABLE kernels, outputs, block_kernels, blocks CASCADE`); err != nil {
		t.Fatalf("server: truncate: %v", err)
	}
	return d
}

// seedBlock inserts a minimal valid `blocks` row at height, matching internal/db's own
// seedBlock test helper shape.
func seedBlock(t *testing.T, d *db.DB, height uint64) {
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
		PowAlgo:           "RXM",
	})
	if err != nil {
		t.Fatalf("server: seed block %d: %v", height, err)
	}
}

// TestParseAnalysisParams_NoParams_DefaultsToLast10000Blocks proves that with no
// from/to query params, From defaults to maxHeight-10000 (not 0/full-history) and To
// defaults to maxHeight, sharing a single MaxIndexedHeight call for both defaults.
func TestParseAnalysisParams_NoParams_DefaultsToLast10000Blocks(t *testing.T) {
	d := openTestDB(t)
	seedBlock(t, d, 50_000)

	s := &Server{DB: d}
	r := httptest.NewRequest("GET", "/analysis/algo-distribution", nil)
	p := s.parseAnalysisParams(r)

	if p.To != 50_000 {
		t.Errorf("To = %d, want 50000", p.To)
	}
	if p.From != 40_000 {
		t.Errorf("From = %d, want 40000", p.From)
	}
	if p.BucketSize != 1000 {
		t.Errorf("BucketSize = %d, want default 1000", p.BucketSize)
	}
}

// TestParseAnalysisParams_ExplicitFromZero_OverridesDefault proves an explicit
// ?from=0 is respected exactly (not overridden by the new last-10000-blocks default),
// even when the DB has plenty of blocks above the window size.
func TestParseAnalysisParams_ExplicitFromZero_OverridesDefault(t *testing.T) {
	d := openTestDB(t)
	seedBlock(t, d, 50_000)

	s := &Server{DB: d}
	r := httptest.NewRequest("GET", "/analysis/algo-distribution?from=0", nil)
	p := s.parseAnalysisParams(r)

	if p.From != 0 {
		t.Errorf("From = %d, want 0 (explicit override)", p.From)
	}
	if p.To != 50_000 {
		t.Errorf("To = %d, want 50000", p.To)
	}
}

// TestParseAnalysisParams_ShortHistory_FromDefaultsToZero proves that when the
// indexed chain is shorter than the 10,000-block default window, From still defaults
// to 0 rather than underflowing/going negative.
func TestParseAnalysisParams_ShortHistory_FromDefaultsToZero(t *testing.T) {
	d := openTestDB(t)
	seedBlock(t, d, 500)

	s := &Server{DB: d}
	r := httptest.NewRequest("GET", "/analysis/algo-distribution", nil)
	p := s.parseAnalysisParams(r)

	if p.From != 0 {
		t.Errorf("From = %d, want 0 (chain shorter than default window)", p.From)
	}
	if p.To != 500 {
		t.Errorf("To = %d, want 500", p.To)
	}
}

// TestParseAnalysisParams_EmptyTable_FromAndToDefaultsUnaffected proves the pre-
// existing empty-table behavior (To falls back to defaultAnalysisToHeight) is
// unchanged, and From still defaults to 0 rather than erroring/underflowing.
func TestParseAnalysisParams_EmptyTable_FromAndToDefaultsUnaffected(t *testing.T) {
	d := openTestDB(t)

	s := &Server{DB: d}
	r := httptest.NewRequest("GET", "/analysis/algo-distribution", nil)
	p := s.parseAnalysisParams(r)

	if p.From != 0 {
		t.Errorf("From = %d, want 0 (empty table)", p.From)
	}
	if p.To != defaultAnalysisToHeight {
		t.Errorf("To = %d, want %d", p.To, defaultAnalysisToHeight)
	}
}

// TestParseAnalysisParams_ExplicitToOnly_FromStillDefaultsFromMaxHeight proves that
// when only ?to= is explicit (and from is absent), From's default still derives from
// MaxIndexedHeight via its own call (the shared-call optimization only applies when
// both from and to are absent).
func TestParseAnalysisParams_ExplicitToOnly_FromStillDefaultsFromMaxHeight(t *testing.T) {
	d := openTestDB(t)
	seedBlock(t, d, 50_000)

	s := &Server{DB: d}
	r := httptest.NewRequest("GET", "/analysis/algo-distribution?to=12345", nil)
	p := s.parseAnalysisParams(r)

	if p.To != 12345 {
		t.Errorf("To = %d, want 12345 (explicit)", p.To)
	}
	if p.From != 40_000 {
		t.Errorf("From = %d, want 40000 (still derived from MaxIndexedHeight)", p.From)
	}
}
