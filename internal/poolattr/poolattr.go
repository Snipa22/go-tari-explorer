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
	"bytes"
	"encoding/hex"
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
	// InstanceSuffix is the hex-encoded rendering of the raw bytes strictly after the
	// first literal 0x00 in txExtra (capped to 8 bytes), for display/uniqueness purposes
	// only - see the NUL-delimited coinbase-extra convention documented on ownPoolTags.
	// Populated whenever a 0x00 was found in txExtra, regardless of which branch
	// (own-pool match / third-party match / unknown fallback) the block lands in; left
	// empty for legacy tags that predate the convention (no 0x00 present).
	InstanceSuffix string `json:"instance_suffix,omitempty"`
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
	prefix string // exact byte prefix to match via strings.HasPrefix against txExtra
	// tagLen is the exact truncation length for this specific prefix's tags. A
	// sentinel value of 0 (or less) means "no truncation - use the exact matched
	// base-tag string as-is". No real tag today is 0 bytes long, so 0 is safe to
	// reserve as the sentinel. See the NUL-delimited-suffix entries in ownPoolTags
	// below for why some rows use this sentinel instead of a real byte count.
	tagLen        int
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
// Jagtech (canonicalName "Jagtech", bare prefix, no "WUF"): the active Jagtech node
// family's pool infrastructure changed its coinbase-extra tag format to drop the
// leading "WUF" prefix. Confirmed live against the production tari_explorer Postgres
// DB on 2026-09-23: block height 351096 has pool_tag='JagtechE0ARs', octet_length
// exactly 12, no WUF prefix and no trailing garbage. The old WUFJagtechE0/E1/S1/S2/
// S3/U0/U1/U2 family stopped appearing after height 349135; this new bare-"Jagtech"
// format starts at height 351096, so it's a format migration for the same operator,
// not a new pool. tagLen 12 is inferred by analogy with the old scheme (same total
// length as before, just without the 3-byte "WUF" prefix: "Jagtech" + 2-char node id
// + a fixed 3-char suffix "ARs" = 12 bytes). Declaration order relative to the "WUF"
// row above doesn't matter - "WUF" and "Jagtech" don't share a common prefix, so
// first-match-wins ordering is a non-issue between these two rows specifically.
//
// Scope limitation: this bare-prefix drop is confirmed ONLY for the active Jagtech
// family. The other WUF <legacy-name> tags in the WUF bucket above (WUF  Ahri   ,
// WUF  Nytro  , WUF  Taila  , WUF Ara-Ayn , WUF Nia-Mio , WUF Stratum , WUFGraha'tia,
// WUFY'shtola) all stopped appearing well before height 349135 (inactive/legacy test
// nodes) and there is NO live evidence they also dropped WUF - do not generalize this
// rule to them.
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
// NUL-delimited suffix variants (added below, ahead of their shorter same-prefix
// counterparts): go-crypto-pool's leaf-solo/leaf-direct binaries are moving to a new
// coinbase-extra wire format of "<base-tag-string><one literal 0x00 byte><4
// cryptographically-random bytes>" (e.g. "supportxtm-rxt-pplns\x00\xA1\xB2\xC3\xD4").
// attributeExtra recovers the base tag by splitting the RAW txExtra on the first 0x00
// byte, which gives an exact, unambiguous tag boundary - so these new
// "supportxtm-<algo>-pplns"/"-solo" rows use the tagLen==0 sentinel (no truncation,
// use the exact matched base string) instead of a hardcoded byte count. tagLen
// truncation is kept ONLY as a safety net for the legacy/no-NUL-delimiter path on the
// original short "supportxtm-<algo>" prefixes above (to preserve existing DB
// attribution behavior for old blocks that predate this convention); these new rows
// don't need it since the NUL delimiter itself already gives a clean boundary.
//
// Each pplns/solo row MUST be listed before its shorter same-prefix counterpart (e.g.
// "supportxtm-sha3x-pplns" before "supportxtm-sha3x") because matching is
// first-match-wins via strings.HasPrefix in declaration order, and
// "supportxtm-sha3x-pplns" also has "supportxtm-sha3x" as a prefix.
var ownPoolTags = []ownPoolTag{
	{prefix: "WUF", tagLen: 12, canonicalName: "Jagtech"},
	{prefix: "Jagtech", tagLen: 12, canonicalName: "Jagtech"},
	{prefix: "supportxtm-sha3x-pplns", tagLen: 0, canonicalName: "SupportXTM"},
	{prefix: "supportxtm-sha3x-solo", tagLen: 0, canonicalName: "SupportXTM"},
	{prefix: "supportxtm-sha3x", tagLen: 16, canonicalName: "SupportXTM"},
	{prefix: "supportxtm-c29-pplns", tagLen: 0, canonicalName: "SupportXTM"},
	{prefix: "supportxtm-c29-solo", tagLen: 0, canonicalName: "SupportXTM"},
	{prefix: "supportxtm-c29", tagLen: 14, canonicalName: "SupportXTM"},
	{prefix: "supportxtm-rxt-pplns", tagLen: 0, canonicalName: "SupportXTM"},
	{prefix: "supportxtm-rxt-solo", tagLen: 0, canonicalName: "SupportXTM"},
	{prefix: "supportxtm-rxt", tagLen: 14, canonicalName: "SupportXTM"},
	{prefix: "supportxtm-rxm-pplns", tagLen: 0, canonicalName: "SupportXTM"},
	{prefix: "supportxtm-rxm-solo", tagLen: 0, canonicalName: "SupportXTM"},
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
//
// go-crypto-pool's leaf-solo/leaf-direct binaries emit newer coinbase-extra tags in a
// NUL-delimited wire format: "<base-tag-string><one literal 0x00 byte><4
// cryptographically-random bytes>". Base tags are always printable ASCII and never
// contain a literal 0x00 themselves, so the first 0x00 byte in the RAW txExtra (before
// any printable-filtering) is an exact, unambiguous boundary between the base tag and
// the random suffix. When present, both the ownPoolTags and prefixTable prefix matches
// (and the truncation input for ownPoolTags rows that still use a tagLen) run against
// txExtra[:idx] rather than the full printable-filtered string, since printable
// filtering could mangle the raw random suffix bytes into something misleading instead
// of cleanly excluding them. When absent, behavior is completely unchanged from before
// this convention existed - this is the mandatory backward-compat path for every
// legacy/third-party tag that predates it.
func attributeExtra(height uint64, algo PowAlgo, txExtra []byte) BlockAttribution {
	raw := printableOnly(txExtra)

	// matchBytes is what prefix matching (both tables) runs against. truncBase is what
	// tagLen truncation (for ownPoolTags rows that still use one) runs against.
	var matchBytes []byte
	var truncBase string
	var instanceSuffix string
	if idx := bytes.IndexByte(txExtra, 0); idx >= 0 {
		matchBytes = txExtra[:idx]
		truncBase = string(matchBytes) // guaranteed printable ASCII per the wire format
		suffix := txExtra[idx+1:]
		if len(suffix) > 8 {
			suffix = suffix[:8]
		}
		instanceSuffix = hex.EncodeToString(suffix)
	} else {
		matchBytes = txExtra
		truncBase = raw // unchanged legacy behavior: truncate the printable-filtered string
	}
	matchStr := string(matchBytes)

	for _, opt := range ownPoolTags {
		if strings.HasPrefix(matchStr, opt.prefix) {
			tag := truncBase
			if opt.tagLen > 0 {
				tag = truncatePoolTag(truncBase, opt.tagLen)
			}
			return BlockAttribution{
				BlockHeight:    height,
				PowAlgo:        algo,
				PoolTag:        tag,
				RawExtra:       raw,
				IsOwnPool:      true,
				InstanceSuffix: instanceSuffix,
			}
		}
	}

	for _, kp := range prefixTable {
		if strings.HasPrefix(matchStr, kp.prefix) {
			return BlockAttribution{
				BlockHeight:    height,
				PowAlgo:        algo,
				PoolTag:        kp.poolName,
				RawExtra:       raw,
				IsOwnPool:      kp.isOwnPool,
				InstanceSuffix: instanceSuffix,
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
		BlockHeight:    height,
		PowAlgo:        algo,
		PoolTag:        asciiPrintableOnly(txExtra),
		RawExtra:       raw,
		IsOwnPool:      false,
		Reason:         ReasonUnknownTxExtra,
		InstanceSuffix: instanceSuffix,
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
