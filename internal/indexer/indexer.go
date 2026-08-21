// Package indexer walks blocks from a configured set of Tari base-node GRPC hosts (via
// internal/nodeclient), runs pool attribution (via internal/poolattr) on each block's
// coinbase, and upserts the result into Postgres (via internal/db).
//
// Two modes are supported, matching the task's structural requirement for both a
// one-shot backfill and a "keep following the tip" mode:
//   - Backfill(ctx, from, to): walks a fixed height range once and returns.
//   - Follow(ctx, pollInterval): repeatedly backfills from the last indexed height to
//     the current chain tip, sleeping pollInterval between polls. This is a simple
//     polling loop, not a push/subscription-based follower - good enough for v1's scope
//     (see AGENTS.md for the note on this being a deliberately simple starting point).
package indexer

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated"

	"github.com/Snipa22/go-tari-explorer/internal/db"
	"github.com/Snipa22/go-tari-explorer/internal/nodeclient"
	"github.com/Snipa22/go-tari-explorer/internal/poolattr"
)

// BatchSize is the number of block heights requested per GetBlockByHeight call.
const BatchSize = 20

// Indexer bundles the dependencies needed to walk and persist blocks.
type Indexer struct {
	Node *nodeclient.Client
	DB   *db.DB
}

// New constructs an Indexer.
func New(node *nodeclient.Client, database *db.DB) *Indexer {
	return &Indexer{Node: node, DB: database}
}

// Backfill walks heights [from, to] inclusive, in batches of BatchSize, attributing and
// upserting each block. Safe to re-run over an already-indexed range (upserts are
// idempotent on height).
func (ix *Indexer) Backfill(ctx context.Context, from, to uint64) error {
	if from > to {
		return fmt.Errorf("indexer: backfill: from (%d) > to (%d)", from, to)
	}
	for start := from; start <= to; start += BatchSize {
		end := start + BatchSize - 1
		if end > to {
			end = to
		}
		heights := makeRange(start, end)
		blocks, err := ix.Node.GetBlockByHeight(ctx, heights)
		if err != nil {
			return fmt.Errorf("indexer: get blocks [%d-%d]: %w", start, end, err)
		}
		for _, block := range blocks {
			if err := ix.indexBlock(ctx, block); err != nil {
				return err
			}
		}
	}
	return nil
}

// Follow repeatedly backfills from the last indexed height (exclusive) to the current
// chain tip, sleeping pollInterval between iterations. Blocks until ctx is cancelled.
func (ix *Indexer) Follow(ctx context.Context, pollInterval time.Duration) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		tip, err := ix.Node.GetTipInfo(ctx)
		if err != nil {
			log.Printf("indexer: follow: get tip info: %v (will retry)", err)
		} else {
			last, err := ix.DB.MaxIndexedHeight(ctx)
			if err != nil {
				log.Printf("indexer: follow: max indexed height: %v (will retry)", err)
			} else {
				bestHeight := tip.Metadata.GetBestBlockHeight()
				from := last + 1
				if last == 0 {
					// Nothing indexed yet: start from the tip itself rather than walking
					// the entire chain history on first run of "follow" mode - full
					// history backfill is what the one-shot Backfill mode is for.
					from = bestHeight
				}
				if from <= bestHeight {
					if err := ix.Backfill(ctx, from, bestHeight); err != nil {
						log.Printf("indexer: follow: backfill [%d-%d]: %v (will retry)", from, bestHeight, err)
					} else if from != bestHeight || last == 0 {
						log.Printf("indexer: follow: indexed up to height %d", bestHeight)
					}
				}
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// indexBlock runs pool attribution on a single block and upserts it.
func (ix *Indexer) indexBlock(ctx context.Context, block *tari_generated.Block) error {
	header := block.GetHeader()
	if header == nil {
		return fmt.Errorf("indexer: block with nil header")
	}
	body := block.GetBody()

	outputs := body.GetOutputs()
	kernels := body.GetKernels()

	var coinbaseFound, coinbaseHasFeatures bool
	var coinbaseExtra []byte
	for _, output := range outputs {
		features := output.GetFeatures()
		if features == nil {
			continue
		}
		if features.GetOutputType() != uint32(tari_generated.OutputType_COINBASE) {
			continue
		}
		coinbaseFound = true
		coinbaseHasFeatures = true
		coinbaseExtra = features.GetCoinbaseExtra()
		break
	}

	rawAlgo := header.GetPow().GetPowAlgo()
	attribution := poolattr.Attribute(header.GetHeight(), rawAlgo, len(outputs) > 0, coinbaseFound, coinbaseHasFeatures, coinbaseExtra)

	var difficulty int64
	if diff, err := ix.Node.GetNetworkDifficulty(ctx, header.GetHeight()); err == nil && diff != nil {
		difficulty = int64(diff.GetDifficulty())
	}
	// A failed difficulty lookup is non-fatal for v1 - the block is still indexed with
	// difficulty 0 rather than aborting the whole batch over a secondary metric.

	var poolTag *string
	if attribution.PoolTag != "" {
		tag := attribution.PoolTag
		poolTag = &tag
	}

	row := db.Block{
		Height:            header.GetHeight(),
		Hash:              fmt.Sprintf("%x", header.GetHash()),
		Version:           header.GetVersion(),
		PrevHash:          fmt.Sprintf("%x", header.GetPrevHash()),
		Timestamp:         int64(header.GetTimestamp()),
		OutputMr:          header.GetOutputMr(),
		BlockOutputMr:     header.GetBlockOutputMr(),
		KernelMr:          header.GetKernelMr(),
		InputMr:           header.GetInputMr(),
		TotalKernelOffset: header.GetTotalKernelOffset(),
		Nonce:             header.GetNonce(),
		KernelMmrSize:     header.GetKernelMmrSize(),
		OutputMmrSize:     header.GetOutputMmrSize(),
		TotalScriptOffset: header.GetTotalScriptOffset(),
		ValidatorNodeMr:   header.GetValidatorNodeMr(),
		ValidatorNodeSize: header.GetValidatorNodeSize(),
		PowAlgoRaw:        rawAlgo,
		PowData:           header.GetPow().GetPowData(),
		PowAlgo:           string(attribution.PowAlgo),
		Difficulty:        difficulty,
		KernelCount:       int32(len(kernels)),
		OutputCount:       int32(len(outputs)),
		PoolTag:           poolTag,
	}
	if err := ix.DB.UpsertBlock(ctx, row); err != nil {
		return err
	}
	if err := ix.DB.ReplaceKernelsForBlock(ctx, header.GetHeight(), kernelRows(kernels)); err != nil {
		return fmt.Errorf("indexer: index block %d: %w", header.GetHeight(), err)
	}
	if err := ix.DB.ReplaceOutputsForBlock(ctx, header.GetHeight(), outputRows(outputs)); err != nil {
		return fmt.Errorf("indexer: index block %d: %w", header.GetHeight(), err)
	}
	return nil
}

// kernelRows converts a block body's raw kernels into db.Kernel rows, indexed by their
// position within the body (matching indexBlock's existing len(kernels) ->
// db.Block.KernelCount convention).
func kernelRows(kernels []*tari_generated.TransactionKernel) []db.Kernel {
	out := make([]db.Kernel, len(kernels))
	for i, k := range kernels {
		excessSig := k.GetExcessSig()
		out[i] = db.Kernel{
			Index:              int32(i),
			Features:           uint64(k.GetFeatures()),
			Fee:                k.GetFee(),
			LockHeight:         k.GetLockHeight(),
			Excess:             k.GetExcess(),
			ExcessSigNonce:     excessSig.GetPublicNonce(),
			ExcessSigSignature: excessSig.GetSignature(),
			Hash:               k.GetHash(),
		}
	}
	return out
}

// outputRows converts a block body's raw outputs into db.Output rows, indexed by their
// position within the body (matching indexBlock's existing len(outputs) ->
// db.Block.OutputCount convention).
func outputRows(outputs []*tari_generated.TransactionOutput) []db.Output {
	out := make([]db.Output, len(outputs))
	for i, o := range outputs {
		features := o.GetFeatures()
		out[i] = db.Output{
			Index:           int32(i),
			FeaturesVersion: features.GetVersion(),
			OutputType:      features.GetOutputType(),
			Maturity:        features.GetMaturity(),
			CoinbaseExtra:   features.GetCoinbaseExtra(),
			Commitment:      o.GetCommitment(),
		}
	}
	return out
}

func makeRange(min, max uint64) []uint64 {
	a := make([]uint64, max-min+1)
	for i := range a {
		a[i] = min + uint64(i)
	}
	return a
}
