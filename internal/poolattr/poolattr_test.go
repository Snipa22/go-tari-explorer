package poolattr

import "testing"

func TestAlgoFromRaw(t *testing.T) {
	cases := []struct {
		raw  uint64
		want PowAlgo
	}{
		{0, PowAlgoRXM},
		{2, PowAlgoRXT},
		{3, PowAlgoC29},
		{1, PowAlgoSHA3X}, // documented catch-all, including the real SHA3x id (1)
		{99, PowAlgoSHA3X},
	}
	for _, c := range cases {
		if got := AlgoFromRaw(c.raw); got != c.want {
			t.Errorf("AlgoFromRaw(%d) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestAttribute_MissingData(t *testing.T) {
	if got := Attribute(100, 0, false, false, false, nil); got.Reason != ReasonNoOutput {
		t.Errorf("expected ReasonNoOutput, got %q", got.Reason)
	}
	if got := Attribute(100, 0, true, false, false, nil); got.Reason != ReasonNoCoinbase {
		t.Errorf("expected ReasonNoCoinbase, got %q", got.Reason)
	}
	if got := Attribute(100, 0, true, true, false, nil); got.Reason != ReasonNoFeatures {
		t.Errorf("expected ReasonNoFeatures, got %q", got.Reason)
	}
	if got := Attribute(100, 0, true, true, true, nil); got.Reason != ReasonEmptyTxExtra {
		t.Errorf("expected ReasonEmptyTxExtra, got %q", got.Reason)
	}
	if got := Attribute(100, 0, true, true, true, []byte{}); got.Reason != ReasonEmptyTxExtra {
		t.Errorf("expected ReasonEmptyTxExtra for empty (non-nil) slice, got %q", got.Reason)
	}
}

// TestAttribute_KnownTags exercises real coinbase-extra byte strings observed via chain
// survey (see https://core.tari.jagtech.io/winners_1000.txt), covering our own pool tags
// and known external pools/solo miners.
func TestAttribute_KnownTags(t *testing.T) {
	cases := []struct {
		name     string
		rawAlgo  uint64
		extra    string
		wantTag  string
		wantOwn  bool
		wantAlgo PowAlgo
	}{
		{"own pool E0", 0, "WUFJagtechE0", "WUFJagtechE0", true, PowAlgoRXM},
		{"own pool E1", 3, "WUFJagtechE1", "WUFJagtechE1", true, PowAlgoC29},
		{"own pool S1", 2, "WUFJagtechS1", "WUFJagtechS1", true, PowAlgoRXT},
		{"own pool with trailing junk", 0, "WUFJagtechE0-worker42", "WUFJagtechE0-worker42", true, PowAlgoRXM},
		{"kryptex", 4, "/pool.kryptex.com/", "pool.kryptex.com", false, PowAlgoSHA3X},
		{"H9", 4, "H9.com.some-worker-id", "H9.com", false, PowAlgoSHA3X},
		{"LuckyPool", 4, "LuckyPool", "LuckyPool", false, PowAlgoSHA3X},
		{"RXLuckyPool", 2, "RXLuckyPool", "RXLuckyPool", false, PowAlgoRXT},
		{"hash2coin", 4, "hash2coin", "hash2coin", false, PowAlgoSHA3X},
		{"DxPool_tari", 4, "DxPool_tari", "DxPool_tari", false, PowAlgoSHA3X},
		{"dxpool_tari lowercase", 4, "dxpool_tari", "dxpool_tari", false, PowAlgoSHA3X},
		{"c3pool", 0, "c3pool_merge_mining_proxy", "c3pool_merge_mining_proxy", false, PowAlgoRXM},
		{"supportxmr", 0, "supportxmr.com_mm_proxy", "supportxmr.com_mm_proxy", false, PowAlgoRXM},
		{"tari_merge_mining_proxy", 0, "tari_merge_mining_proxy", "tari_merge_mining_proxy", false, PowAlgoRXM},
		{"xmr-pool", 0, "xmr-pool", "xmr-pool", false, PowAlgoRXM},
		{"solo test", 4, "solo test", "solo test", false, PowAlgoSHA3X},
		{"unknown extra", 4, "some-random-unrecognized-tag", "", false, PowAlgoSHA3X},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Attribute(1000, c.rawAlgo, true, true, true, []byte(c.extra))
			if got.PoolTag != c.wantTag {
				t.Errorf("PoolTag = %q, want %q", got.PoolTag, c.wantTag)
			}
			if got.IsOwnPool != c.wantOwn {
				t.Errorf("IsOwnPool = %v, want %v", got.IsOwnPool, c.wantOwn)
			}
			if got.PowAlgo != c.wantAlgo {
				t.Errorf("PowAlgo = %q, want %q", got.PowAlgo, c.wantAlgo)
			}
			if c.wantTag == "" && got.Reason != ReasonUnknownTxExtra {
				t.Errorf("expected ReasonUnknownTxExtra for unattributed tag, got %q", got.Reason)
			}
		})
	}
}

func TestAttribute_NonPrintableBytesStripped(t *testing.T) {
	extra := []byte{0x00, 0x01, 'W', 'U', 'F', 'x', 0x02}
	got := Attribute(1, 0, true, true, true, extra)
	// Doesn't start with "WUF" once raw bytes are considered (leading NUL/SOH bytes),
	// so it should land in the unknown bucket with non-printable bytes stripped from RawExtra.
	if got.Reason != ReasonUnknownTxExtra {
		t.Errorf("expected ReasonUnknownTxExtra, got %q (tag=%q)", got.Reason, got.PoolTag)
	}
	if got.RawExtra != "WUFx" {
		t.Errorf("RawExtra = %q, want %q", got.RawExtra, "WUFx")
	}
}
