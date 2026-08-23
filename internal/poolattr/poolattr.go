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

// ourPoolPrefix is the common prefix shared by every tag this operator's own pool
// infrastructure emits (WUFJagtechE0, WUFJagtechE1, WUFJagtechS1, and any future
// WUF-prefixed tag). Individual tags are still reported verbatim via RawExtra/PoolTag;
// this only drives the IsOwnPool boolean.
const ourPoolPrefix = "WUF"

// ourPoolTagLen is the fixed byte length own-pool coinbase-extra tags are truncated to
// before being stored as PoolTag. Ported directly from go-tari-grpc-lib's
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
const ourPoolTagLen = 12

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

	if strings.HasPrefix(string(txExtra), ourPoolPrefix) {
		return BlockAttribution{
			BlockHeight: height,
			PowAlgo:     algo,
			PoolTag:     truncatePoolTag(raw, ourPoolTagLen),
			RawExtra:    raw,
			IsOwnPool:   true,
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
	// rather than silently dropping the data the way an unlabeled map key would.
	return BlockAttribution{
		BlockHeight: height,
		PowAlgo:     algo,
		PoolTag:     "",
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
