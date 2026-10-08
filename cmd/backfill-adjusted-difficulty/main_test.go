package main

import (
	"testing"

	"github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated"
)

func TestExtractAdjustedDifficulty_Present(t *testing.T) {
	adjusted := uint64(320_000)
	diff := &tari_generated.NetworkDifficultyResponse{Height: 100, Difficulty: 10_000, AdjustedDifficulty: &adjusted}
	got := extractAdjustedDifficulty(diff)
	if got == nil || *got != 320_000 {
		t.Errorf("extractAdjustedDifficulty = %v, want pointer to 320000", got)
	}
}

func TestExtractAdjustedDifficulty_AbsentIsNil(t *testing.T) {
	// A response from a base-node host that predates the TIP-004 field - the proto3
	// `optional` AdjustedDifficulty field is simply unset (nil), not a real 0.
	diff := &tari_generated.NetworkDifficultyResponse{Height: 101, Difficulty: 5_000}
	got := extractAdjustedDifficulty(diff)
	if got != nil {
		t.Errorf("extractAdjustedDifficulty = %v, want nil (field absent on the wire)", *got)
	}
}

func TestExtractAdjustedDifficulty_RealZeroIsDistinctFromAbsent(t *testing.T) {
	// A node CAN legitimately report a real 0 (e.g. a pointer to uint64(0)) -
	// distinct from the field being entirely absent (nil pointer). Both must stay
	// distinguishable all the way through this function.
	zero := uint64(0)
	diff := &tari_generated.NetworkDifficultyResponse{Height: 102, AdjustedDifficulty: &zero}
	got := extractAdjustedDifficulty(diff)
	if got == nil {
		t.Fatal("extractAdjustedDifficulty = nil, want pointer to 0 (a real captured zero, not absent)")
	}
	if *got != 0 {
		t.Errorf("extractAdjustedDifficulty = %d, want 0", *got)
	}
}

func TestAdjustedDifficultyChanged(t *testing.T) {
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
			got := adjustedDifficultyChanged(tt.old, tt.new)
			if got != tt.want {
				t.Errorf("adjustedDifficultyChanged(%s, %s) = %v, want %v", displayPtr(tt.old), displayPtr(tt.new), got, tt.want)
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
