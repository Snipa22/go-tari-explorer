// Package poolattr rebuilds the ad-hoc pool-attribution logic originally found in
// go-tari-grpc-lib's cmd/blockWinners/main.go into structured, reusable data suitable
// for JSON/HTML template rendering (rather than a fmt.Println CLI report).
//
// The rebuild intentionally keeps the same *detection* rules the original CLI used
// (algo id -> name, coinbase-extra prefix -> pool name) but organizes the prefix
// matching into a table instead of a chain of if/HasPrefix statements, and returns a
// typed BlockAttribution instead of printing.
package poolattr

import (
	"strings"
	"unicode"
)

// PowAlgo mirrors the algo IDs surfaced by tari_generated.ProofOfWork.GetPowAlgo().
// Values and meaning are taken directly from the original blockWinners CLI:
// 0 = RandomX merge-mined, 2 = RandomX Tari (RXT), 3 = C29, anything else = SHA3x.
type PowAlgo string

const (
	PowAlgoRXM   PowAlgo = "RXM"   // RandomX merge-mine
	PowAlgoRXT   PowAlgo = "RXT"   // RandomX Tari
	PowAlgoC29   PowAlgo = "C29"   // Cuckaroo29
	PowAlgoSHA3X PowAlgo = "SHA3X" // SHA3x (also the fallback bucket, matching upstream Tari's "else" convention)
)

// AlgoFromRaw converts the raw uint64 algo id returned by
// tari_generated.ProofOfWork.GetPowAlgo() into a PowAlgo. Any value other than 0, 2, or
// 3 is treated as SHA3X, matching the original CLI's fallback ("Sha3x is ID 1, but using
// it as a catch here.").
func AlgoFromRaw(raw uint64) PowAlgo {
	switch raw {
	case 0:
		return PowAlgoRXM
	case 2:
		return PowAlgoRXT
	case 3:
		return PowAlgoC29
	default:
		return PowAlgoSHA3X
	}
}

// Reason enumerates why a block could not be attributed to a specific pool tag, distinct
// from the "unknown extra" bucket (which means the coinbase extra was present but didn't
// match any known prefix).
type Reason string

const (
	ReasonOK             Reason = ""
	ReasonNoOutput       Reason = "no_output"      // block had no outputs at all
	ReasonNoFeatures     Reason = "no_features"    // no coinbase output had output features
	ReasonNoCoinbase     Reason = "no_coinbase"    // block had outputs, but none were OutputType == COINBASE (1)
	ReasonEmptyTxExtra   Reason = "empty_tx_extra" // coinbase output had features but an empty/nil extra
	ReasonUnknownTxExtra Reason = "unknown_tx_extra"
)

// BlockAttribution is the structured result of attributing a single block's coinbase to
// a pool (or to "unknown"/"own pool"). Designed to be JSON/HTML-template friendly.
type BlockAttribution struct {
	BlockHeight uint64  `json:"block_height"`
	PowAlgo     PowAlgo `json:"pow_algo"`
	PoolTag     string  `json:"pool_tag"`  // human-readable pool name, or "" if unattributed
	RawExtra    string  `json:"raw_extra"` // printable-only rendering of the raw coinbase extra bytes
	IsOwnPool   bool    `json:"is_own_pool"`
	Reason      Reason  `json:"reason,omitempty"` // set when PoolTag == "" (or is a generic "unknown" bucket)
}

// knownPrefix maps a coinbase-extra byte prefix to a human-readable pool name. Order
// matters only in that longer/more specific prefixes should be listed before shorter
// ones that could also match — see prefixTable below, which is built once and matched
// in declaration order.
type knownPrefix struct {
	prefix    string
	poolName  string
	isOwnPool bool
}

// ownPoolTag is one entry in ownPoolTags, the ordered table of coinbase-extra prefixes
// that belong to this operator's own pool infrastructure (as opposed to prefixTable's
// third-party pools/solo miners). Unlike knownPrefix, each ownPoolTag also carries its
// own tagLen: own-pool tags aren't all the same byte length across families (WUF's
// tags are; supportxtm-*'s aren't - see the doc comments below), so truncation length
// has to travel with the specific prefix, not be a single package-level constant.
type ownPoolTag struct {
	prefix        string // exact byte prefix to match via strings.HasPrefix against txExtra
	tagLen        int    // exact truncation length for this specific prefix's tags
	canonicalName string // canonical display name for this own-pool family (see below)
}

// canonicalName above is not consumed by attributeExtra itself - PoolTag is still set
// to the truncated raw tag (matching WUF's pre-existing behavior of storing the real
// per-node tag, not a folded display name), exactly as before this field existed.
// It exists purely as documentation/cross-reference for
// internal/analysis.DefaultPoolTagMappings, whose {MatchPrefix, CanonicalName} entries
// fold these same per-node/per-algo tag families into one display series for the
// pool-share and algo-breakdown charts - see that variable's doc comment.

// ownPoolTags is the ordered table of own-pool coinbase-extra prefixes, checked before
// prefixTable in attributeExtra (own-pool status always wins over a third-party-pool
// match, exactly as the single ourPoolPrefix check did before this table existed).
//
// WUF (canonicalName "Jagtech"): the common prefix shared by every tag this operator's
// legacy pool infrastructure emits (WUFJagtechE0, WUFJagtechE1, WUFJagtechS1, and any
// future WUF-prefixed tag). tagLen 12 is ported directly from go-tari-grpc-lib's
// cmd/blockWinners/main.go (txExtraParser), which truncates via txString[0:12] rather
// than keeping the whole printable-filtered string.
//
// Why 12, and why truncate at all: confirmed against live production data (query:
// SELECT DISTINCT pool_tag FROM blocks WHERE pool_tag LIKE 'WUF%' against the
// tari_explorer database) that every real own-pool coinbase_extra is genuinely,
// deterministically exactly 12 bytes long when hex-decoded (e.g. "WUFJagtechE0",
// "WUF  Ahri   ", "WUF  Nytro  ", "WUF  Taila  "). Anything beyond byte 12 is
// non-identifying binary/padding noise, not a legitimate variable-length "worker ID"
// feature - an earlier version of this package's rebuild dropped the truncation this
// constant restores, which fragmented what should be ~55-60 real per-node pool tags
// into ~45,900 spurious distinct values in the blocks table (one per garbage-suffix
// variant). Keep this in sync with the reference implementation if it ever changes.
//
// supportxtm-* (canonicalName "SupportXTM"): confirmed as this operator's own pool
// infrastructure (SupportXTM), same tier as WUF, not a third-party prefixTable entry.
// go-crypto-pool's cmd/leaf-direct/main.go (mirrored by cmd/leaf-solo/main.go)
// defaultCoinbaseExtraTag/resolveCoinbaseExtraTag build the default per-algo
// coinbase-extra tag as exactly "supportxtm-" + algoTagSuffix(cfg), with
// algoTagSuffix returning one of "sha3x"/"c29"/"rxt"/"rxm" and no separator or version
// byte in between. internal/leaflib/solo/node.go and internal/leaflib/direct/node.go's
// GetJobParams then append coinbaseExtraTag directly (coinbaseExtra = append(
// coinbaseExtra, c.coinbaseExtraTag...)) with no separator before the random nonce
// buffer, so the literal tag bytes land at the front of the on-chain coinbase_extra
// unmodified. Unlike WUF, the four real tags are NOT all the same length ("
// supportxtm-sha3x" is 16 bytes; "supportxtm-c29"/"supportxtm-rxt"/"supportxtm-rxm"
// are each 14 bytes), so each variant gets its own table row with its own exact
// tagLen rather than sharing one - a single shared tagLen (e.g. the longest, 16)
// would include 2 bytes of the following nonce buffer as garbage on the three
// 14-byte variants instead of stopping exactly at the real tag boundary.
//
// GCPOOL-SOLO (canonicalName "SupportXTM"): go-crypto-pool's ORIGINAL hardcoded
// default coinbase-extra tag, predating the algo-aware supportxtm-<algo> defaults
// documented directly above. internal/leaflib/solo/node.go's doc comment on
// GRPCNodeClient.coinbaseExtraTag says the field is "Runtime-configurable per-process
// (was formerly a single hardcoded package-level "GCPOOL-SOLO" constant) so
// cmd/leaf-solo can set it to a per-algo default (e.g. "supportxtm-sha3x")" - i.e.
// GCPOOL-SOLO isn't a third-party pool, it's an OLDER GENERATION of the exact same
// own-pool infrastructure that now emits supportxtm-<algo> tags: same infra, same
// operator, just a legacy tag format from before leaf-solo became configurable. It
// folds into the same canonicalName "SupportXTM" as the current supportxtm-* tags
// (not a separate name) for exactly that reason, so historical and current blocks
// from this infra group together on the pool-share/algo-breakdown charts. The literal
// string "GCPOOL-SOLO" is exactly 11 bytes, and (per live testnet DB analysis) real
// historical coinbase_extra values for this tag are always exactly that 11-byte
// string with no legitimate suffix - anything past byte 11 is garbage/nonce-buffer
// noise from the block, the exact same suffix-garbage-truncation problem WUF and
// supportxtm-* had before being added to this table (confirmed: 35 distinct
// fragmented pool_tag rows in the live testnet blocks table, e.g. "GCPOOL-SOLO&",
// "GCPOOL-SOLO*fes", "GCPOOL-SOLO+N", etc., all collapsing to the one real 11-byte
// tag once truncated here).
var ownPoolTags = []ownPoolTag{
	{prefix: "WUF", tagLen: 12, canonicalName: "Jagtech"},
	{prefix: "supportxtm-sha3x", tagLen: 16, canonicalName: "SupportXTM"},
	{prefix: "supportxtm-c29", tagLen: 14, canonicalName: "SupportXTM"},
	{prefix: "supportxtm-rxt", tagLen: 14, canonicalName: "SupportXTM"},
	{prefix: "supportxtm-rxm", tagLen: 14, canonicalName: "SupportXTM"},
	{prefix: "GCPOOL-SOLO", tagLen: 11, canonicalName: "SupportXTM"},
}

// prefixTable is the cleaned-up replacement for the original CLI's chain of
// strings.HasPrefix checks. Extend this table as new pools are identified via chain
// survey (see https://core.tari.jagtech.io/winners_1000.txt for real-world examples)
// rather than adding another if-statement.
var prefixTable = []knownPrefix{
	{prefix: "/pool.kryptex.com/", poolName: "pool.kryptex.com"},
	{prefix: "H9.com.", poolName: "H9.com"},
	{prefix: "hash2coin", poolName: "hash2coin"},
	{prefix: "DxPool_tari", poolName: "DxPool_tari"},
	{prefix: "dxpool_tari", poolName: "dxpool_tari"},
	{prefix: "c3pool_merge_mining_proxy", poolName: "c3pool_merge_mining_proxy"},
	{prefix: "supportxmr.com_mm_proxy", poolName: "supportxmr.com_mm_proxy"},
	{prefix: "tari_merge_mining_proxy", poolName: "tari_merge_mining_proxy"},
	{prefix: "xmr-pool", poolName: "xmr-pool"},
	{prefix: "RXLuckyPool", poolName: "RXLuckyPool"},
	{prefix: "LuckyPool", poolName: "LuckyPool"},
	{prefix: "solo test", poolName: "solo test", isOwnPool: false}, // a solo miner, not a pool, but a recognized tag
}

// Attribute inspects a single coinbase output's raw extra bytes (features.GetCoinbaseExtra())
// and produces a structured BlockAttribution for the given block/algo. Pass the raw algo id
// straight from tari_generated.ProofOfWork.GetPowAlgo(); Attribute handles the RXM/RXT/C29/SHA3X
// mapping itself.
//
// hasCoinbaseOutput/hasFeatures/txExtra together let the caller communicate exactly which
// "not found" case applies (mirrors the branches the original CLI tracked via its
// *_no_output/*_no_features/*_no_tx_extra result buckets), without Attribute needing to know
// anything about tari_generated's protobuf types itself — keeping this package dependency-free
// from go-tari-grpc-lib so it's independently unit-testable.
func Attribute(height uint64, rawAlgo uint64, hasOutputs, hasCoinbaseOutput, hasFeatures bool, txExtra []byte) BlockAttribution {
	algo := AlgoFromRaw(rawAlgo)

	if !hasOutputs {
		return BlockAttribution{BlockHeight: height, PowAlgo: algo, Reason: ReasonNoOutput}
	}
	if !hasCoinbaseOutput {
		return BlockAttribution{BlockHeight: height, PowAlgo: algo, Reason: ReasonNoCoinbase}
	}
	if !hasFeatures {
		return BlockAttribution{BlockHeight: height, PowAlgo: algo, Reason: ReasonNoFeatures}
	}
	if len(txExtra) == 0 {
		return BlockAttribution{BlockHeight: height, PowAlgo: algo, Reason: ReasonEmptyTxExtra}
	}

	return attributeExtra(height, algo, txExtra)
}

// attributeExtra does the actual prefix-table lookup once a non-empty coinbase extra is
// known to exist. Split out from Attribute for testability against known real-world byte
// strings without needing to fake the surrounding block/output plumbing.
func attributeExtra(height uint64, algo PowAlgo, txExtra []byte) BlockAttribution {
	raw := printableOnly(txExtra)

	for _, opt := range ownPoolTags {
		if strings.HasPrefix(string(txExtra), opt.prefix) {
			return BlockAttribution{
				BlockHeight: height,
				PowAlgo:     algo,
				PoolTag:     truncatePoolTag(raw, opt.tagLen),
				RawExtra:    raw,
				IsOwnPool:   true,
			}
		}
	}

	for _, kp := range prefixTable {
		if strings.HasPrefix(string(txExtra), kp.prefix) {
			return BlockAttribution{
				BlockHeight: height,
				PowAlgo:     algo,
				PoolTag:     kp.poolName,
				RawExtra:    raw,
				IsOwnPool:   kp.isOwnPool,
			}
		}
	}

	// Fallback bucket: extra was present but didn't match anything known. Surface the raw
	// (printable-filtered) bytes so operators can spot new pools to add to prefixTable,
	// rather than silently dropping the data the way an unlabeled map key would. PoolTag
	// uses the stricter ASCII-only filter (applied to the original txExtra bytes, not the
	// already-printable-filtered raw) since it becomes a real pool_tag value stored in the
	// database, while RawExtra keeps the broader printableOnly rendering for diagnostics.
	return BlockAttribution{
		BlockHeight: height,
		PowAlgo:     algo,
		PoolTag:     asciiPrintableOnly(txExtra),
		RawExtra:    raw,
		IsOwnPool:   false,
		Reason:      ReasonUnknownTxExtra,
	}
}

// truncatePoolTag clamps s to at most n bytes, matching the reference CLI's
// txString[0:12] slice while gracefully handling any real-world extra shorter than n
// (rather than panicking with an index-out-of-range on a slice bound past len(s)).
func truncatePoolTag(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

// printableOnly strips non-printable runes from raw coinbase-extra bytes, matching the
// original CLI's unicode.IsPrint filter used for its fallback/"unknown" bucket label.
func printableOnly(b []byte) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, string(b))
}

// asciiPrintableOnly strips everything outside the printable ASCII range (0x20-0x7E
// inclusive) from raw coinbase-extra bytes. This is intentionally stricter than
// printableOnly (which allows any unicode.IsPrint rune, including wide Unicode
// punctuation, emoji, and combining marks): printableOnly is used for RawExtra, a
// diagnostic/debug field where broader Unicode is acceptable and even useful for
// spotting what garbage/binary bytes actually showed up. asciiPrintableOnly is used
// for the fallback PoolTag label, which becomes a real pool_tag value stored in the
// database and surfaced/grouped in the UI - it must not admit non-ASCII printable
// noise from garbage/binary coinbase-extra bytes into what's supposed to be a stable,
// groupable pool identifier.
func asciiPrintableOnly(b []byte) string {
	return strings.Map(func(r rune) rune {
		if r >= 0x20 && r <= 0x7E {
			return r
		}
		return -1
	}, string(b))
}
