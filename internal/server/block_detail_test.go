package server

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Snipa22/go-tari-explorer/internal/db"
)

// TestHandleBlockDetail_RendersDifficultyRecordedAndAdjustedRows is a template-level
// smoke test for the block detail page's two difficulty rows (see
// templates/block_detail.html): proves the real HTTP handler, through the real
// html/template, renders both "Difficulty (recorded)" and "Difficulty (real, TIP-004
// adjusted)" with their respective values for a block that HAS a captured
// AdjustedDifficulty - catching a template-syntax/field-name typo that a pure Go unit
// test on blockView.AdjustedDifficultyDisplay alone wouldn't.
func TestHandleBlockDetail_RendersDifficultyRecordedAndAdjustedRows(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	adjusted := int64(320_000)
	if err := d.UpsertBlock(ctx, db.Block{
		Height:             900,
		Hash:               "aa",
		PrevHash:           "bb",
		OutputMr:           []byte{},
		BlockOutputMr:      []byte{},
		KernelMr:           []byte{},
		InputMr:            []byte{},
		TotalKernelOffset:  []byte{},
		TotalScriptOffset:  []byte{},
		ValidatorNodeMr:    []byte{},
		PowData:            []byte{},
		PowAlgo:            "RXM",
		Difficulty:         10_000,
		AdjustedDifficulty: &adjusted,
	}); err != nil {
		t.Fatalf("UpsertBlock: %v", err)
	}

	s, err := New(d, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/blocks/900", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		body, _ := io.ReadAll(rec.Body)
		t.Fatalf("status = %d, want 200, body:\n%s", rec.Code, body)
	}
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	html := string(body)

	if !strings.Contains(html, "<th>Difficulty (recorded)</th><td>10000</td>") {
		t.Errorf("expected rendered page to contain the recorded-difficulty row, body:\n%s", html)
	}
	if !strings.Contains(html, "<th>Difficulty (real, TIP-004 adjusted)</th><td>320,000</td>") {
		t.Errorf("expected rendered page to contain the adjusted-difficulty row with value 320,000, body:\n%s", html)
	}
}

// TestHandleBlockDetail_AdjustedDifficultyNilRendersNotYetCaptured proves a block
// whose adjusted_difficulty is NULL (not yet backfilled/captured) renders the
// explicit "not yet captured" placeholder in that row - never a blank or misleading
// "0" - through the real handler/template, not just the pure blockView unit test.
func TestHandleBlockDetail_AdjustedDifficultyNilRendersNotYetCaptured(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	if err := d.UpsertBlock(ctx, db.Block{
		Height:            901,
		Hash:              "cc",
		PrevHash:          "dd",
		OutputMr:          []byte{},
		BlockOutputMr:     []byte{},
		KernelMr:          []byte{},
		InputMr:           []byte{},
		TotalKernelOffset: []byte{},
		TotalScriptOffset: []byte{},
		ValidatorNodeMr:   []byte{},
		PowData:           []byte{},
		PowAlgo:           "SHA3X",
		Difficulty:        5_000,
		// AdjustedDifficulty deliberately left nil/unset.
	}); err != nil {
		t.Fatalf("UpsertBlock: %v", err)
	}

	s, err := New(d, nil, "", nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/blocks/901", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		body, _ := io.ReadAll(rec.Body)
		t.Fatalf("status = %d, want 200, body:\n%s", rec.Code, body)
	}
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	html := string(body)

	if !strings.Contains(html, "<th>Difficulty (real, TIP-004 adjusted)</th><td>not yet captured</td>") {
		t.Errorf("expected rendered page to show \"not yet captured\" for a NULL adjusted_difficulty, body:\n%s", html)
	}
}
