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
		name       string
		rawAlgo    uint64
		extra      string
		wantTag    string
		wantOwn    bool
		wantAlgo   PowAlgo
		wantReason Reason
	}{
		{"own pool E0", 0, "WUFJagtechE0", "WUFJagtechE0", true, PowAlgoRXM, ReasonOK},
		{"own pool E1", 3, "WUFJagtechE1", "WUFJagtechE1", true, PowAlgoC29, ReasonOK},
		{"own pool S1", 2, "WUFJagtechS1", "WUFJagtechS1", true, PowAlgoRXT, ReasonOK},
		// Real coinbase_extra bytes are always exactly 12 bytes once decoded; anything
		// after that is non-identifying binary/padding noise from the block, not a
		// legitimate variable-length worker-ID feature (confirmed via live production
		// data - see ourPoolTagLen's doc comment). This exercises the truncation itself.
		{"own pool truncates trailing bytes", 0, "WUFJagtechE0-worker42", "WUFJagtechE0", true, PowAlgoRXM, ReasonOK},
		// Jagtech's pool infra dropped the "WUF" prefix on its coinbase-extra tags.
		// Confirmed live against production Postgres on 2026-09-23: block height
		// 351096 has pool_tag='JagtechE0ARs', octet_length exactly 12, no WUF prefix.
		{"own pool bare Jagtech no WUF prefix", 0, "JagtechE0ARs", "JagtechE0ARs", true, PowAlgoRXM, ReasonOK},
		// Same truncation behavior as the WUF-prefixed family above, just without the
		// "WUF" prefix - trailing worker-id/garbage bytes past the 12-byte tag are
		// dropped exactly the same way.
		{"own pool bare Jagtech truncates trailing bytes", 0, "JagtechE0ARs-worker42", "JagtechE0ARs", true, PowAlgoRXM, ReasonOK},
		// Legacy/inactive node-name shape with embedded spaces, matching the real
		// production byte pattern "WUF  Ahri   " (WUF + 2 spaces + "Ahri" + 3 trailing
		// spaces = 12 bytes) once printable-filtered.
		{"own pool legacy Ahri node", 4, "WUF  Ahri   -garbage-tail-bytes", "WUF  Ahri   ", true, PowAlgoSHA3X, ReasonOK},
		// Shorter than ourPoolTagLen must clamp to len(extra), not panic.
		{"own pool shorter than tag length", 0, "WUFshort", "WUFshort", true, PowAlgoRXM, ReasonOK},
		{"kryptex", 4, "/pool.kryptex.com/", "pool.kryptex.com", false, PowAlgoSHA3X, ReasonOK},
		{"H9", 4, "H9.com.some-worker-id", "H9.com", false, PowAlgoSHA3X, ReasonOK},
		{"LuckyPool", 4, "LuckyPool", "LuckyPool", false, PowAlgoSHA3X, ReasonOK},
		{"RXLuckyPool", 2, "RXLuckyPool", "RXLuckyPool", false, PowAlgoRXT, ReasonOK},
		{"hash2coin", 4, "hash2coin", "hash2coin", false, PowAlgoSHA3X, ReasonOK},
		{"DxPool_tari", 4, "DxPool_tari", "DxPool_tari", false, PowAlgoSHA3X, ReasonOK},
		{"dxpool_tari lowercase", 4, "dxpool_tari", "dxpool_tari", false, PowAlgoSHA3X, ReasonOK},
		{"c3pool", 0, "c3pool_merge_mining_proxy", "c3pool_merge_mining_proxy", false, PowAlgoRXM, ReasonOK},
		{"supportxmr", 0, "supportxmr.com_mm_proxy", "supportxmr.com_mm_proxy", false, PowAlgoRXM, ReasonOK},
		{"tari_merge_mining_proxy", 0, "tari_merge_mining_proxy", "tari_merge_mining_proxy", false, PowAlgoRXM, ReasonOK},
		{"xmr-pool", 0, "xmr-pool", "xmr-pool", false, PowAlgoRXM, ReasonOK},
		{"solo test", 4, "solo test", "solo test", false, PowAlgoSHA3X, ReasonOK},
		{"unknown extra", 4, "some-random-unrecognized-tag", "some-random-unrecognized-tag", false, PowAlgoSHA3X, ReasonUnknownTxExtra},
		// Real production coinbase-extra: go-crypto-pool's leaf-solo/leaf-direct binaries
		// build the default per-algo coinbase-extra tag as exactly
		// "supportxtm-" + algoTagSuffix(cfg), which is this operator's own SupportXTM
		// pool infrastructure - a recognized own-pool prefix (see ownPoolTags), not an
		// unknown-fallback tag.
		{"supportxtm sha3x default tag", 4, "supportxtm-sha3x", "supportxtm-sha3x", true, PowAlgoSHA3X, ReasonOK},
		// Each supportxtm-<algo> variant truncates to its own exact tagLen, dropping
		// trailing worker-id/nonce-buffer noise exactly the way the WUF "truncates
		// trailing bytes" case above does, but per-variant since the four tags aren't
		// all the same length (16 bytes for sha3x, 14 for c29/rxt/rxm).
		{"supportxtm sha3x truncates trailing bytes", 4, "supportxtm-sha3x-worker07\x00\x01", "supportxtm-sha3x", true, PowAlgoSHA3X, ReasonOK},
		{"supportxtm c29 truncates trailing bytes", 3, "supportxtm-c29-worker07\x00\x01", "supportxtm-c29", true, PowAlgoC29, ReasonOK},
		{"supportxtm rxt truncates trailing bytes", 2, "supportxtm-rxt-worker07\x00\x01", "supportxtm-rxt", true, PowAlgoRXT, ReasonOK},
		{"supportxtm rxm truncates trailing bytes", 0, "supportxtm-rxm-worker07\x00\x01", "supportxtm-rxm", true, PowAlgoRXM, ReasonOK},
		// GCPOOL-SOLO is go-crypto-pool's legacy pre-algo-aware default tag, folding
		// into the same "SupportXTM" canonicalName as supportxtm-* above; it truncates
		// trailing worker-id/nonce-buffer noise exactly the same way.
		{"gcpool-solo truncates trailing bytes", 0, "GCPOOL-SOLO\x00\x01worker03", "GCPOOL-SOLO", true, PowAlgoRXM, ReasonOK},
		// Exactly 11 bytes with nothing appended must clamp to len(extra) without
		// dropping any real bytes - exercises truncatePoolTag's exact-length (no
		// garbage) boundary case explicitly, mirroring "own pool shorter than tag
		// length" above.
		{"gcpool-solo exact length", 0, "GCPOOL-SOLO", "GCPOOL-SOLO", true, PowAlgoRXM, ReasonOK},
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
			if got.Reason != c.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, c.wantReason)
			}
		})
	}
}

// TestAttribute_NULDelimitedTags exercises go-crypto-pool's newer coinbase-extra wire
// format: "<base-tag-string><one literal 0x00 byte><4 cryptographically-random bytes>".
// These are a distinct test function (rather than more TestAttribute_KnownTags rows)
// specifically so we can assert InstanceSuffix, which the shared KnownTags table/loop
// deliberately does not check (several of its existing cases already embed literal
// \x00 bytes as legacy "garbage tail" filler, predating this convention, and adding a
// blanket InstanceSuffix assertion there would incorrectly require those unrelated
// pre-existing cases to have a specific InstanceSuffix value).
func TestAttribute_NULDelimitedTags(t *testing.T) {
	cases := []struct {
		name               string
		rawAlgo            uint64
		extra              []byte
		wantTag            string
		wantOwn            bool
		wantAlgo           PowAlgo
		wantReason         Reason
		wantInstanceSuffix string
	}{
		{
			name:               "supportxtm rxt-pplns NUL-delimited random suffix",
			rawAlgo:            2,
			extra:              append([]byte("supportxtm-rxt-pplns\x00"), 0xA1, 0xB2, 0xC3, 0xD4),
			wantTag:            "supportxtm-rxt-pplns",
			wantOwn:            true,
			wantAlgo:           PowAlgoRXT,
			wantReason:         ReasonOK,
			wantInstanceSuffix: "a1b2c3d4",
		},
		{
			name:               "supportxtm sha3x-solo NUL-delimited random suffix",
			rawAlgo:            4,
			extra:              append([]byte("supportxtm-sha3x-solo\x00"), 0x11, 0x22, 0x33, 0x44),
			wantTag:            "supportxtm-sha3x-solo",
			wantOwn:            true,
			wantAlgo:           PowAlgoSHA3X,
			wantReason:         ReasonOK,
			wantInstanceSuffix: "11223344",
		},
		{
			name:               "supportxtm c29-pplns NUL-delimited random suffix",
			rawAlgo:            3,
			extra:              append([]byte("supportxtm-c29-pplns\x00"), 0xDE, 0xAD, 0xBE, 0xEF),
			wantTag:            "supportxtm-c29-pplns",
			wantOwn:            true,
			wantAlgo:           PowAlgoC29,
			wantReason:         ReasonOK,
			wantInstanceSuffix: "deadbeef",
		},
		// Same base+NUL+random-suffix shape, but rxm-solo, for a second algo/suffix
		// combination beyond the three required by the brief.
		{
			name:               "supportxtm rxm-solo NUL-delimited random suffix",
			rawAlgo:            0,
			extra:              append([]byte("supportxtm-rxm-solo\x00"), 0x01, 0x02, 0x03, 0x04),
			wantTag:            "supportxtm-rxm-solo",
			wantOwn:            true,
			wantAlgo:           PowAlgoRXM,
			wantReason:         ReasonOK,
			wantInstanceSuffix: "01020304",
		},
		// Proves the no-NUL legacy fallback path is untouched: no literal 0x00 byte
		// anywhere in extra, so this must take the exact same code path as (and produce
		// the same InstanceSuffix-less result as) every pre-existing KnownTags case -
		// still truncating to the short-prefix supportxtm-rxt's tagLen (14), not
		// matching the longer NUL-delimited "supportxtm-rxt-pplns" row, and leaving
		// InstanceSuffix empty since no 0x00 delimiter was found.
		{
			name:               "legacy no-NUL supportxtm-rxt tag is untouched",
			rawAlgo:            2,
			extra:              []byte("supportxtm-rxt-worker07-garbage-tail-no-nul-byte-here"),
			wantTag:            "supportxtm-rxt",
			wantOwn:            true,
			wantAlgo:           PowAlgoRXT,
			wantReason:         ReasonOK,
			wantInstanceSuffix: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Attribute(1000, c.rawAlgo, true, true, true, c.extra)
			if got.PoolTag != c.wantTag {
				t.Errorf("PoolTag = %q, want %q", got.PoolTag, c.wantTag)
			}
			if got.IsOwnPool != c.wantOwn {
				t.Errorf("IsOwnPool = %v, want %v", got.IsOwnPool, c.wantOwn)
			}
			if got.PowAlgo != c.wantAlgo {
				t.Errorf("PowAlgo = %q, want %q", got.PowAlgo, c.wantAlgo)
			}
			if got.Reason != c.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, c.wantReason)
			}
			if got.InstanceSuffix != c.wantInstanceSuffix {
				t.Errorf("InstanceSuffix = %q, want %q", got.InstanceSuffix, c.wantInstanceSuffix)
			}
		})
	}
}

// TestAttribute_FallbackPoolTagIsASCIIOnly proves that the fallback PoolTag uses the
// stricter asciiPrintableOnly filter, not printableOnly: a non-ASCII rune that
// unicode.IsPrint accepts (e.g. U+00E9, "é") must still be stripped from PoolTag.
func TestAttribute_FallbackPoolTagIsASCIIOnly(t *testing.T) {
	extra := []byte("tag-\u00e9-end") // "tag-" + é (U+00E9, printable but non-ASCII) + "-end"
	got := Attribute(1, 4, true, true, true, extra)
	if got.Reason != ReasonUnknownTxExtra {
		t.Errorf("expected ReasonUnknownTxExtra, got %q", got.Reason)
	}
	if got.PoolTag != "tag--end" {
		t.Errorf("PoolTag = %q, want %q", got.PoolTag, "tag--end")
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
