package server

import (
	"testing"

	"github.com/Snipa22/go-tari-explorer/internal/db"
)

// TestBlockView_AdjustedDifficultyDisplay_Nil proves a block whose adjusted_difficulty
// hasn't been captured yet (NULL in Postgres, nil *int64 in Go) renders the explicit
// "not yet captured" placeholder - never a blank or misleading "0" - on the block
// detail page (see migrations/0010_adjusted_difficulty.up.sql for why NULL must stay
// distinguishable from a real zero).
func TestBlockView_AdjustedDifficultyDisplay_Nil(t *testing.T) {
	v := blockView{db.Block{AdjustedDifficulty: nil}}
	if got := v.AdjustedDifficultyDisplay(); got != "not yet captured" {
		t.Errorf("AdjustedDifficultyDisplay() = %q, want %q", got, "not yet captured")
	}
}

// TestBlockView_AdjustedDifficultyDisplay_RealValue proves a captured (non-nil)
// adjusted_difficulty renders with the same comma-grouped formatting every other
// difficulty display on this page uses.
func TestBlockView_AdjustedDifficultyDisplay_RealValue(t *testing.T) {
	adjusted := int64(1_234_567)
	v := blockView{db.Block{AdjustedDifficulty: &adjusted}}
	if got := v.AdjustedDifficultyDisplay(); got != "1,234,567" {
		t.Errorf("AdjustedDifficultyDisplay() = %q, want %q", got, "1,234,567")
	}
}

// TestBlockView_AdjustedDifficultyDisplay_ZeroIsDistinctFromNil proves a real captured
// 0 (a non-nil pointer to int64(0)) renders as the humanized "0", not the "not yet
// captured" placeholder - the two states must stay distinguishable through display.
func TestBlockView_AdjustedDifficultyDisplay_ZeroIsDistinctFromNil(t *testing.T) {
	zero := int64(0)
	v := blockView{db.Block{AdjustedDifficulty: &zero}}
	if got := v.AdjustedDifficultyDisplay(); got != "0" {
		t.Errorf("AdjustedDifficultyDisplay() = %q, want %q (a real captured 0 must not look like \"not yet captured\")", got, "0")
	}
}

// TestBlockView_AchievedDifficultyDisplay_Nil proves a block whose achieved_difficulty
// hasn't been captured yet (NULL in Postgres, nil *int64 in Go) renders the explicit
// "not yet captured" placeholder - never a blank or misleading "0" - on the block
// detail page (see migrations/0011_achieved_difficulty.up.sql for why NULL must stay
// distinguishable from a real zero). Mirrors TestBlockView_AdjustedDifficultyDisplay_Nil
// one column over.
func TestBlockView_AchievedDifficultyDisplay_Nil(t *testing.T) {
	v := blockView{db.Block{AchievedDifficulty: nil}}
	if got := v.AchievedDifficultyDisplay(); got != "not yet captured" {
		t.Errorf("AchievedDifficultyDisplay() = %q, want %q", got, "not yet captured")
	}
}

// TestBlockView_AchievedDifficultyDisplay_RealValue proves a captured (non-nil)
// achieved_difficulty renders with the same comma-grouped formatting every other
// difficulty display on this page uses.
func TestBlockView_AchievedDifficultyDisplay_RealValue(t *testing.T) {
	achieved := int64(193_350)
	v := blockView{db.Block{AchievedDifficulty: &achieved}}
	if got := v.AchievedDifficultyDisplay(); got != "193,350" {
		t.Errorf("AchievedDifficultyDisplay() = %q, want %q", got, "193,350")
	}
}

// TestBlockView_AchievedDifficultyDisplay_ZeroIsDistinctFromNil proves a real captured
// 0 (a non-nil pointer to int64(0)) renders as the humanized "0", not the "not yet
// captured" placeholder - the two states must stay distinguishable through display.
func TestBlockView_AchievedDifficultyDisplay_ZeroIsDistinctFromNil(t *testing.T) {
	zero := int64(0)
	v := blockView{db.Block{AchievedDifficulty: &zero}}
	if got := v.AchievedDifficultyDisplay(); got != "0" {
		t.Errorf("AchievedDifficultyDisplay() = %q, want %q (a real captured 0 must not look like \"not yet captured\")", got, "0")
	}
}
