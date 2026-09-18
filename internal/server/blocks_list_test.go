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
// the pure newAlgoGlanceRows helper), and asserts the rendered "Current block diff"
// column reflects template_difficulty_snapshots' value - proving handleBlocksList
// really did switch which table it reads, not just that both code paths compile.
func TestHandleBlocksList_AlgoGlanceCurrentDifficulty_SourcedFromTemplateSnapshots(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	// Isolate this test from any other difficulty-snapshot data left behind by other
	// tests sharing this package's test database.
	if _, err := d.Pool.Exec(ctx, `TRUNCATE TABLE difficulty_snapshots, template_difficulty_snapshots`); err != nil {
		t.Fatalf("truncate difficulty snapshot tables: %v", err)
	}

	recordedAt := time.Now().UTC()

	// Old retrospective table: must NOT be reflected in the rendered column anymore.
	if _, err := d.UpsertDifficultySnapshot(ctx, db.DifficultySnapshot{
		Algo:       "RXM",
		Height:     100,
		Difficulty: 11_111,
		RecordedAt: recordedAt,
	}); err != nil {
		t.Fatalf("UpsertDifficultySnapshot: %v", err)
	}

	// New forward-looking table: must be what the rendered column shows.
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

	if !strings.Contains(html, "22,222") {
		t.Errorf("expected rendered page to contain template_difficulty_snapshots value %q, body:\n%s", "22,222", html)
	}
	if strings.Contains(html, "11,111") {
		t.Errorf("rendered page contains old difficulty_snapshots value %q - the swap did not happen, body:\n%s", "11,111", html)
	}
}
