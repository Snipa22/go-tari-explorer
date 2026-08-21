// Package txsearch implements the explorer's transaction search: given a single query
// string (typically pasted by a user - a kernel excess signature, a UTXO commitment,
// or a payment-reference hex), it tries to resolve which block (if any) contains a
// matching kernel or output, and separately exposes a live "check right now"
// transaction-state lookup.
//
// Strategy: try the already-indexed Postgres tables first (internal/db's `kernels` and
// `outputs`), then fall back to the base node's live GRPC search RPCs
// (SearchKernels/SearchUtxos/SearchPaymentReferences via internal/nodeclient) if
// nothing local matched. The indexed path is chosen first purely for latency - it's an
// equality-indexed lookup against Postgres, versus a live GRPC call that has to walk
// chain state on the node side - not because the indexed data is considered more
// authoritative. A transaction that exists on-chain but hasn't been (re)indexed yet
// (e.g. mempool-only, or the indexer is behind the tip) will only show up via the live
// fallback, which is also why TransactionState (see CheckTransactionState) is *never*
// answered from the index: "is this mempool or mined" is explicitly a live-only
// question.
//
// Query-shape detection: inputs are hex-decoded (an optional "0x" prefix is stripped);
// non-hex input is reported as not found rather than erroring, since a search box is
// expected to receive garbage. The decoded byte length then picks a lookup family:
//   - 32 bytes (64 hex chars): ambiguous between a kernel excess-signature *scalar*
//     alone (Signature.signature, ignoring the nonce), a UTXO commitment, and a
//     payment-reference hash - all three are 32-byte values in this protocol. Rather
//     than trying to disambiguate blindly, all three lookup strategies are attempted in
//     sequence (indexed kernel-by-signature-scalar, indexed output-by-commitment, live
//     SearchUtxos, live SearchPaymentReferences) and the first hit wins.
//   - 64 bytes (128 hex chars): unambiguously a full Signature (32-byte public_nonce +
//     32-byte signature scalar, concatenated), since that's the only field in this
//     protocol's search surface built by joining two 32-byte values. Tried as an
//     indexed exact kernel lookup, then a live SearchKernels call.
//   - any other length: reported not found - none of the supported search fields are
//     that size.
package txsearch

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated"
	"github.com/jackc/pgx/v5"

	"github.com/Snipa22/go-tari-explorer/internal/db"
)

// Kind identifies what a successful Result matched.
type Kind string

const (
	KindKernel           Kind = "kernel"
	KindOutput           Kind = "output"
	KindPaymentReference Kind = "payment_reference"
)

// Source identifies whether a Result came from the local Postgres index or a live
// GRPC call to the base node.
type Source string

const (
	SourceIndexed Source = "indexed"
	SourceLive    Source = "live"
)

// Result is the outcome of a single Search call. Found is false (and every other
// field zero) when nothing matched anywhere.
type Result struct {
	Query       string
	Found       bool
	Kind        Kind
	Source      Source
	BlockHeight uint64
	BlockHash   string // hex-encoded, when known (always known for live results; indexed results are resolved by the caller via db.GetBlock if needed)

	Kernel           *db.Kernel
	Output           *db.Output
	PaymentReference *tari_generated.PaymentReferenceResponse
}

// DBSearcher is the subset of *internal/db.DB's methods txsearch needs for the
// indexed-lookup path. Satisfied by *db.DB; declared here (rather than imported as a
// concrete type) so tests can substitute a fake without a real Postgres connection.
type DBSearcher interface {
	FindKernelByExcessSigSignature(ctx context.Context, sig []byte) (db.Kernel, error)
	FindKernelByExcessSig(ctx context.Context, nonce, sig []byte) (db.Kernel, error)
	FindOutputByCommitment(ctx context.Context, commitment []byte) (db.Output, error)
}

// NodeSearcher is the subset of *internal/nodeclient.Client's methods txsearch needs
// for the live-GRPC-fallback path. Satisfied by *nodeclient.Client; declared here so
// tests can substitute a bufconn-backed fake instead of dialing a real base node.
type NodeSearcher interface {
	SearchKernels(ctx context.Context, signatures []*tari_generated.Signature) ([]*tari_generated.HistoricalBlock, error)
	SearchUtxos(ctx context.Context, commitments [][]byte) ([]*tari_generated.HistoricalBlock, error)
	SearchPaymentReferences(ctx context.Context, paymentReferenceHex []string) ([]*tari_generated.PaymentReferenceResponse, error)
	TransactionState(ctx context.Context, excessSig *tari_generated.Signature) (*tari_generated.TransactionStateResponse, error)
}

// Searcher bundles the indexed-DB and live-node dependencies Search/CheckTransactionState
// need.
type Searcher struct {
	DB   DBSearcher
	Node NodeSearcher
}

// New constructs a Searcher.
func New(dbSearcher DBSearcher, nodeSearcher NodeSearcher) *Searcher {
	return &Searcher{DB: dbSearcher, Node: nodeSearcher}
}

// decodeQuery strips a leading "0x"/"0X" prefix and hex-decodes the rest.
func decodeQuery(query string) ([]byte, error) {
	q := strings.TrimSpace(query)
	q = strings.TrimPrefix(strings.TrimPrefix(q, "0x"), "0X")
	return hex.DecodeString(q)
}

// Search resolves query per the package doc's strategy above. A non-hex or
// unsupported-length query, or a hex value that matches nothing, all come back as
// Result{Found: false} (not an error) - only genuine lookup failures (a DB error other
// than "not found", or every configured node host failing) are returned as errors.
func (s *Searcher) Search(ctx context.Context, query string) (Result, error) {
	notFound := Result{Query: query}

	raw, err := decodeQuery(query)
	if err != nil {
		return notFound, nil
	}

	switch len(raw) {
	case 32:
		return s.search32(ctx, query, raw)
	case 64:
		return s.search64(ctx, query, raw)
	default:
		return notFound, nil
	}
}

// search32 handles a 32-byte query: kernel-signature-scalar, commitment, or payment
// reference, per the package doc.
func (s *Searcher) search32(ctx context.Context, query string, raw []byte) (Result, error) {
	if s.DB != nil {
		if k, err := s.DB.FindKernelByExcessSigSignature(ctx, raw); err == nil {
			return Result{Query: query, Found: true, Kind: KindKernel, Source: SourceIndexed, BlockHeight: k.BlockHeight, Kernel: &k}, nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return Result{}, fmt.Errorf("txsearch: find kernel by signature: %w", err)
		}

		if o, err := s.DB.FindOutputByCommitment(ctx, raw); err == nil {
			return Result{Query: query, Found: true, Kind: KindOutput, Source: SourceIndexed, BlockHeight: o.BlockHeight, Output: &o}, nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return Result{}, fmt.Errorf("txsearch: find output by commitment: %w", err)
		}
	}

	if s.Node != nil {
		if blocks, err := s.Node.SearchUtxos(ctx, [][]byte{raw}); err == nil && len(blocks) > 0 {
			return liveResult(query, KindOutput, blocks[0]), nil
		}

		if refs, err := s.Node.SearchPaymentReferences(ctx, []string{hex.EncodeToString(raw)}); err == nil && len(refs) > 0 {
			ref := refs[0]
			return Result{
				Query:            query,
				Found:            true,
				Kind:             KindPaymentReference,
				Source:           SourceLive,
				BlockHeight:      ref.GetBlockHeight(),
				BlockHash:        hex.EncodeToString(ref.GetBlockHash()),
				PaymentReference: ref,
			}, nil
		}
	}

	return Result{Query: query}, nil
}

// search64 handles a 64-byte query: a full (public_nonce || signature) Signature, per
// the package doc.
func (s *Searcher) search64(ctx context.Context, query string, raw []byte) (Result, error) {
	nonce, sig := raw[:32], raw[32:]

	if s.DB != nil {
		if k, err := s.DB.FindKernelByExcessSig(ctx, nonce, sig); err == nil {
			return Result{Query: query, Found: true, Kind: KindKernel, Source: SourceIndexed, BlockHeight: k.BlockHeight, Kernel: &k}, nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return Result{}, fmt.Errorf("txsearch: find kernel by excess sig: %w", err)
		}
	}

	if s.Node != nil {
		signature := &tari_generated.Signature{PublicNonce: nonce, Signature: sig}
		if blocks, err := s.Node.SearchKernels(ctx, []*tari_generated.Signature{signature}); err == nil && len(blocks) > 0 {
			return liveResult(query, KindKernel, blocks[0]), nil
		}
	}

	return Result{Query: query}, nil
}

// liveResult builds a Result from a live SearchKernels/SearchUtxos hit.
func liveResult(query string, kind Kind, block *tari_generated.HistoricalBlock) Result {
	header := block.GetBlock().GetHeader()
	return Result{
		Query:       query,
		Found:       true,
		Kind:        kind,
		Source:      SourceLive,
		BlockHeight: header.GetHeight(),
		BlockHash:   hex.EncodeToString(header.GetHash()),
	}
}

// TransactionStateResult is the human-facing outcome of CheckTransactionState.
type TransactionStateResult struct {
	Query string
	State string // "MEMPOOL" | "MINED" | "UNKNOWN" | "NOT_STORED"
}

// stateLabels maps the raw TransactionLocation enum to the human labels the block
// detail/search pages render. Kept as an explicit table (rather than relying on the
// proto's own String()) so this package's rendering isn't silently affected by a
// future proto-generated name change.
var stateLabels = map[tari_generated.TransactionLocation]string{
	tari_generated.TransactionLocation_UNKNOWN:    "UNKNOWN",
	tari_generated.TransactionLocation_MEMPOOL:    "MEMPOOL",
	tari_generated.TransactionLocation_MINED:      "MINED",
	tari_generated.TransactionLocation_NOT_STORED: "NOT_STORED",
}

// CheckTransactionState calls the base node's live TransactionState RPC for
// excessSigHex (expected to be a 128-char hex string: 32-byte public_nonce + 32-byte
// signature, concatenated - the same shape search64 above expects). This is always a
// live call, never answered from the Postgres index - see the package doc for why.
func (s *Searcher) CheckTransactionState(ctx context.Context, excessSigHex string) (TransactionStateResult, error) {
	raw, err := decodeQuery(excessSigHex)
	if err != nil {
		return TransactionStateResult{}, fmt.Errorf("txsearch: excess_sig is not valid hex: %w", err)
	}
	if len(raw) != 64 {
		return TransactionStateResult{}, fmt.Errorf("txsearch: excess_sig must be 64 bytes (128 hex chars: public_nonce+signature), got %d bytes", len(raw))
	}
	if s.Node == nil {
		return TransactionStateResult{}, fmt.Errorf("txsearch: no node client configured")
	}

	signature := &tari_generated.Signature{PublicNonce: raw[:32], Signature: raw[32:]}
	resp, err := s.Node.TransactionState(ctx, signature)
	if err != nil {
		return TransactionStateResult{}, fmt.Errorf("txsearch: transaction state: %w", err)
	}

	label, ok := stateLabels[resp.GetResult()]
	if !ok {
		label = "UNKNOWN"
	}
	return TransactionStateResult{Query: excessSigHex, State: label}, nil
}
