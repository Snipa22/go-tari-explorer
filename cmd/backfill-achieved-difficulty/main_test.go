package main

import (
	"testing"

	"github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated"
)

func TestExtractAchievedDifficulty(t *testing.T) {
	hdr := &tari_generated.BlockHeaderResponse{Difficulty: 193_350}
	got := extractAchievedDifficulty(hdr)
	if got == nil || *got != 193_350 {
		t.Errorf("extractAchievedDifficulty = %v, want pointer to 193350", got)
	}
}

func TestExtractAchievedDifficulty_RealZero(t *testing.T) {
	// BlockHeaderResponse.Difficulty is a plain (non-optional) uint64 - a real
	// reported 0 is technically representable and must round-trip as a non-nil
	// pointer to 0, not nil (nil is reserved for "the GetHeaderByHash call itself
	// failed", handled one level up in backfillAchievedDifficultyBatch).
	hdr := &tari_generated.BlockHeaderResponse{Difficulty: 0}
	got := extractAchievedDifficulty(hdr)
	if got == nil {
		t.Fatal("extractAchievedDifficulty = nil, want pointer to 0")
	}
	if *got != 0 {
		t.Errorf("extractAchievedDifficulty = %d, want 0", *got)
	}
}

func TestAchievedDifficultyChanged(t *testing.T) {
	ptr := func(v int64) *int64 { return &v }

	tests := []struct {
		name string
		old  *int64
		new  *int64
		want bool
	}{
		{name: "both nil", old: nil, new: nil, want: false},
		{name: "same non-nil value", old: ptr(100), new: ptr(100), want: false},
		{name: "nil to non-nil (newly captured)", old: nil, new: ptr(100), want: true},
		{name: "non-nil to nil (regression, shouldn't normally happen but must be detected)", old: ptr(100), new: nil, want: true},
		{name: "different non-nil values", old: ptr(100), new: ptr(200), want: true},
		{name: "both zero (real, not nil) - unchanged", old: ptr(0), new: ptr(0), want: false},
		{name: "nil vs pointer-to-zero - changed (nil is never equivalent to a real 0)", old: nil, new: ptr(0), want: true},
		{name: "pointer-to-zero vs nil - changed", old: ptr(0), new: nil, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := achievedDifficultyChanged(tt.old, tt.new)
			if got != tt.want {
				t.Errorf("achievedDifficultyChanged(%s, %s) = %v, want %v", displayPtr(tt.old), displayPtr(tt.new), got, tt.want)
			}
		})
	}
}

func TestDisplayPtr(t *testing.T) {
	if got := displayPtr(nil); got != "NULL" {
		t.Errorf("displayPtr(nil) = %q, want %q", got, "NULL")
	}
	v := int64(42)
	if got := displayPtr(&v); got != "42" {
		t.Errorf("displayPtr(&42) = %q, want %q", got, "42")
	}
	zero := int64(0)
	if got := displayPtr(&zero); got != "0" {
		t.Errorf("displayPtr(&0) = %q, want %q", got, "0")
	}
}

func TestJoinHosts(t *testing.T) {
	if got := joinHosts([]string{"a:1", "b:2"}); got != "a:1,b:2" {
		t.Errorf("joinHosts = %q, want %q", got, "a:1,b:2")
	}
	if got := joinHosts(nil); got != "" {
		t.Errorf("joinHosts(nil) = %q, want empty string", got)
	}
}
