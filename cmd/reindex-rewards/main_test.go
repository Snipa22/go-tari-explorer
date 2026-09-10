package main

import (
	"testing"

	"github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated"
)

func TestExtractRewardMicroMinotari_CoinbaseWithRevealedValue(t *testing.T) {
	block := &tari_generated.Block{
		Header: &tari_generated.BlockHeader{Height: 100},
		Body: &tari_generated.AggregateBody{
			Outputs: []*tari_generated.TransactionOutput{
				{
					Features:            &tari_generated.OutputFeatures{OutputType: uint32(tari_generated.OutputType_COINBASE)},
					MinimumValuePromise: 5_000_000_000,
				},
			},
		},
	}
	got := extractRewardMicroMinotari(block)
	if got != 5_000_000_000 {
		t.Errorf("extractRewardMicroMinotari = %d, want 5000000000", got)
	}
}

func TestExtractRewardMicroMinotari_CoinbaseWithBulletProofPlusIsZero(t *testing.T) {
	// A BulletProofPlus coinbase's minimum_value_promise reads 0 - the real value is
	// hidden in the (unreconstructable without keys) range proof. This is a real
	// protocol limitation this function must faithfully report as 0, not error on or
	// invent a value for.
	block := &tari_generated.Block{
		Header: &tari_generated.BlockHeader{Height: 101},
		Body: &tari_generated.AggregateBody{
			Outputs: []*tari_generated.TransactionOutput{
				{
					Features:            &tari_generated.OutputFeatures{OutputType: uint32(tari_generated.OutputType_COINBASE)},
					MinimumValuePromise: 0,
				},
			},
		},
	}
	got := extractRewardMicroMinotari(block)
	if got != 0 {
		t.Errorf("extractRewardMicroMinotari = %d, want 0", got)
	}
}

func TestExtractRewardMicroMinotari_NoCoinbaseOutput(t *testing.T) {
	block := &tari_generated.Block{
		Header: &tari_generated.BlockHeader{Height: 102},
		Body: &tari_generated.AggregateBody{
			Outputs: []*tari_generated.TransactionOutput{
				{
					Features:            &tari_generated.OutputFeatures{OutputType: 0}, // STANDARD
					MinimumValuePromise: 123456,                                        // must be ignored - not a coinbase
				},
			},
		},
	}
	got := extractRewardMicroMinotari(block)
	if got != 0 {
		t.Errorf("extractRewardMicroMinotari = %d, want 0 (no coinbase output)", got)
	}
}

func TestExtractRewardMicroMinotari_NoOutputsAtAll(t *testing.T) {
	block := &tari_generated.Block{
		Header: &tari_generated.BlockHeader{Height: 103},
		Body:   &tari_generated.AggregateBody{},
	}
	got := extractRewardMicroMinotari(block)
	if got != 0 {
		t.Errorf("extractRewardMicroMinotari = %d, want 0 (no outputs)", got)
	}
}

func TestExtractRewardMicroMinotari_NilFeaturesIsSkipped(t *testing.T) {
	// A nil OutputFeatures must not panic (GetOutputType()/GetFeatures() are
	// nil-safe proto3 getters) and must not be mistaken for a coinbase.
	block := &tari_generated.Block{
		Header: &tari_generated.BlockHeader{Height: 104},
		Body: &tari_generated.AggregateBody{
			Outputs: []*tari_generated.TransactionOutput{
				{Features: nil},
				{
					Features:            &tari_generated.OutputFeatures{OutputType: uint32(tari_generated.OutputType_COINBASE)},
					MinimumValuePromise: 999,
				},
			},
		},
	}
	got := extractRewardMicroMinotari(block)
	if got != 999 {
		t.Errorf("extractRewardMicroMinotari = %d, want 999", got)
	}
}

func TestExtractRewardMicroMinotari_FirstCoinbaseWins(t *testing.T) {
	// Mirrors internal/indexer.go's indexBlock own "first coinbase output found,
	// break" loop - if a block somehow has more than one COINBASE-typed output, the
	// first one seen must win, not the last.
	block := &tari_generated.Block{
		Header: &tari_generated.BlockHeader{Height: 105},
		Body: &tari_generated.AggregateBody{
			Outputs: []*tari_generated.TransactionOutput{
				{
					Features:            &tari_generated.OutputFeatures{OutputType: uint32(tari_generated.OutputType_COINBASE)},
					MinimumValuePromise: 111,
				},
				{
					Features:            &tari_generated.OutputFeatures{OutputType: uint32(tari_generated.OutputType_COINBASE)},
					MinimumValuePromise: 222,
				},
			},
		},
	}
	got := extractRewardMicroMinotari(block)
	if got != 111 {
		t.Errorf("extractRewardMicroMinotari = %d, want 111 (first coinbase wins)", got)
	}
}

func TestRewardChanged(t *testing.T) {
	tests := []struct {
		name string
		old  uint64
		new  uint64
		want bool
	}{
		{name: "same value", old: 100, new: 100, want: false},
		{name: "zero vs zero", old: 0, new: 0, want: false},
		{name: "zero vs nonzero", old: 0, new: 5_000_000, want: true},
		{name: "nonzero vs zero", old: 5_000_000, new: 0, want: true},
		{name: "different nonzero values", old: 5_000_000, new: 6_000_000, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rewardChanged(tt.old, tt.new)
			if got != tt.want {
				t.Errorf("rewardChanged(%d, %d) = %v, want %v", tt.old, tt.new, got, tt.want)
			}
		})
	}
}

func TestMakeRange(t *testing.T) {
	got := makeRange(5, 8)
	want := []uint64{5, 6, 7, 8}
	if len(got) != len(want) {
		t.Fatalf("makeRange(5, 8) = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("makeRange(5, 8)[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestMakeRange_SingleHeight(t *testing.T) {
	got := makeRange(42, 42)
	if len(got) != 1 || got[0] != 42 {
		t.Fatalf("makeRange(42, 42) = %v, want [42]", got)
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
