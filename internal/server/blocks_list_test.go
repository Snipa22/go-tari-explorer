package server

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Snipa22/go-tari-explorer/internal/db"
)

// TestHandleBlocksList_AlgoGlanceCurrentDifficulty_SourcedFromTemplateSnapshots is the
// regression guard for the front-page "Current block diff" column's data-source swap:
// it seeds BOTH the old retrospective difficulty_snapshots table and the new
// forward-looking template_difficulty_snapshots table with DISTINCT values for the
// same algo/height, renders the actual front page through handleBlocksList (not just
// the pure newAlgoGlanceRows helper), and asserts the rendered algo-glance row has
// template_difficulty_snapshots' value in the "Current block diff" position and
// difficulty_snapshots' value in the (separate, additive) "Last mined (recorded)"
// position - proving handleBlocksList reads "current block diff" from the right
// table, not just that both code paths compile. Unlike before the "Last mined
// (recorded)"/"Last mined (real, TIP-004)" columns existed, difficulty_snapshots'
// value is now LEGITIMATELY rendered on the page too (in its own column) - so this
// test asserts on the exact rendered row shape (cell order/position) rather than a
// blanket "value must never appear anywhere on the page", which would now be a false
// positive.
func TestHandleBlocksList_AlgoGlanceCurrentDifficulty_SourcedFromTemplateSnapshots(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	// Isolate this test from any other difficulty-snapshot data left behind by other
	// tests sharing this package's test database.
	if _, err := d.Pool.Exec(ctx, `TRUNCATE TABLE difficulty_snapshots, template_difficulty_snapshots`); err != nil {
		t.Fatalf("truncate difficulty snapshot tables: %v", err)
	}

	recordedAt := time.Now().UTC()

	// Old retrospective table: must land in "Last mined (recorded)", not "Current
	// block diff".
	if _, err := d.UpsertDifficultySnapshot(ctx, db.DifficultySnapshot{
		Algo:       "RXM",
		Height:     100,
		Difficulty: 11_111,
		RecordedAt: recordedAt,
	}); err != nil {
		t.Fatalf("UpsertDifficultySnapshot: %v", err)
	}

	// New forward-looking table: must be what "Current block diff" shows.
	if _, err := d.UpsertTemplateDifficultySnapshot(ctx, db.TemplateDifficultySnapshot{
		Algo:             "RXM",
		Height:           100,
		TargetDifficulty: 22_222,
		Reward:           1,
		RecordedAt:       recordedAt,
	}); err != nil {
		t.Fatalf("UpsertTemplateDifficultySnapshot: %v", err)
	}

	s, err := New(d, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	html := string(body)

	// RXM's algo-glance row: Algo, Blocks, Diff, Current block diff (22,222 from
	// template_difficulty_snapshots), Last mined recorded (11,111 from
	// difficulty_snapshots), Last mined adjusted (— : no adjusted_difficulty seeded).
	wantRow := "<tr><td>RXM</td><td>0</td><td>0.00</td><td>22,222</td><td>11,111</td><td>—</td></tr>"
	if !strings.Contains(html, wantRow) {
		t.Errorf("expected rendered page to contain algo-glance row %q, body:\n%s", wantRow, html)
	}
}
