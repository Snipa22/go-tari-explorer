package db

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// testDSN returns the Postgres connection string to run these tests against. Defaults
// to a dedicated local test database on the embedded Postgres instance used across this
// workspace (see AGENTS.md / the task's embedded-pg note) - not the shared
// tari_explorer_test database other agents/branches may be using concurrently.
// Override with TARI_EXPLORER_TEST_POSTGRES_DSN in CI or a different local setup.
func testDSN() string {
	if v := os.Getenv("TARI_EXPLORER_TEST_POSTGRES_DSN"); v != "" {
		return v
	}
	return "postgres://postgres@localhost:5433/tari_explorer_txcheck_test?sslmode=disable&host=/workspace/pg-embed/sockets"
}

// openTestDB connects to testDSN(), runs migrations, and truncates every table this
// package's tests touch so each test starts from a clean slate. Skips the test (not
// fails) if the database isn't reachable, so this suite doesn't break `go test ./...`
// runs in environments without the embedded Postgres instance available.
func openTestDB(t *testing.T) *DB {
	t.Helper()
	ctx := context.Background()

	d, err := Connect(ctx, testDSN())
	if err != nil {
		t.Skipf("db: test postgres not reachable at %s: %v", testDSN(), err)
	}
	t.Cleanup(d.Close)

	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("db: migrate: %v", err)
	}
	if _, err := d.Pool.Exec(ctx, `TRUNCATE TABLE kernels, outputs, block_kernels, blocks CASCADE`); err != nil {
		t.Fatalf("db: truncate: %v", err)
	}
	return d
}

// seedBlock inserts a minimal valid `blocks` row at height, satisfying the
// kernels/outputs tables' FK on blocks(height).
func seedBlock(t *testing.T, d *DB, height uint64) {
	t.Helper()
	err := d.UpsertBlock(context.Background(), Block{
		Height:            height,
		Hash:              "aa",
		PrevHash:          "bb",
		OutputMr:          []byte{},
		BlockOutputMr:     []byte{},
		KernelMr:          []byte{},
		InputMr:           []byte{},
		TotalKernelOffset: []byte{},
		TotalScriptOffset: []byte{},
		ValidatorNodeMr:   []byte{},
		PowData:           []byte{},
		PowAlgo:           "RXM",
	})
	if err != nil {
		t.Fatalf("db: seed block %d: %v", height, err)
	}
}

// seedBlockFull inserts a minimal valid `blocks` row at height like seedBlock, but
// with caller-supplied powAlgo/poolTag/difficulty - used by TestRecentBlocksStats and
// friends, which (unlike seedBlock's other callers) need to control those three
// fields to exercise the pool/algo breakdown and avg-difficulty aggregation.
func seedBlockFull(t *testing.T, d *DB, height uint64, powAlgo string, poolTag *string, difficulty int64) {
	t.Helper()
	err := d.UpsertBlock(context.Background(), Block{
		Height:            height,
		Hash:              "aa",
		PrevHash:          "bb",
		OutputMr:          []byte{},
		BlockOutputMr:     []byte{},
		KernelMr:          []byte{},
		InputMr:           []byte{},
		TotalKernelOffset: []byte{},
		TotalScriptOffset: []byte{},
		ValidatorNodeMr:   []byte{},
		PowData:           []byte{},
		PowAlgo:           powAlgo,
		PoolTag:           poolTag,
		Difficulty:        difficulty,
	})
	if err != nil {
		t.Fatalf("db: seed block %d: %v", height, err)
	}
}

func TestReplaceKernelsForBlock_InsertsAndReplaces(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	seedBlock(t, d, 100)

	k0 := Kernel{
		Index:              0,
		Features:           1,
		Fee:                5_000_000,
		LockHeight:         0,
		Excess:             []byte{0x01, 0x02},
		ExcessSigNonce:     bytesOf(32, 0xAA),
		ExcessSigSignature: bytesOf(32, 0xBB),
		Hash:               []byte{0x03},
	}
	k1 := Kernel{
		Index:              1,
		Features:           0,
		Fee:                1_234,
		LockHeight:         10,
		Excess:             []byte{0x04},
		ExcessSigNonce:     bytesOf(32, 0xCC),
		ExcessSigSignature: bytesOf(32, 0xDD),
		Hash:               []byte{0x05},
	}

	if err := d.ReplaceKernelsForBlock(ctx, 100, []Kernel{k0, k1}); err != nil {
		t.Fatalf("ReplaceKernelsForBlock: %v", err)
	}

	got, err := d.GetKernelsForBlock(ctx, 100)
	if err != nil {
		t.Fatalf("GetKernelsForBlock: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 kernels, got %d: %+v", len(got), got)
	}
	if got[0].Index != 0 || got[0].Fee != 5_000_000 || got[0].BlockHeight != 100 {
		t.Errorf("kernel 0 mismatch: %+v", got[0])
	}
	if got[1].Index != 1 || got[1].Fee != 1_234 || got[1].LockHeight != 10 {
		t.Errorf("kernel 1 mismatch: %+v", got[1])
	}

	// Replace with a smaller set: the old rows for this block must be gone, not
	// merged/appended - this is the whole point of delete-then-insert re-indexing.
	k2 := Kernel{
		Index:              0,
		Features:           2,
		Fee:                999,
		ExcessSigNonce:     bytesOf(32, 0xEE),
		ExcessSigSignature: bytesOf(32, 0xFF),
	}
	if err := d.ReplaceKernelsForBlock(ctx, 100, []Kernel{k2}); err != nil {
		t.Fatalf("ReplaceKernelsForBlock (replace): %v", err)
	}
	got, err = d.GetKernelsForBlock(ctx, 100)
	if err != nil {
		t.Fatalf("GetKernelsForBlock after replace: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 kernel after replace, got %d: %+v", len(got), got)
	}
	if got[0].Fee != 999 {
		t.Errorf("expected replaced kernel fee 999, got %d", got[0].Fee)
	}

	// Replacing with an empty slice must clear all rows for the block (documented
	// behavior in ReplaceKernelsForBlock's doc comment).
	if err := d.ReplaceKernelsForBlock(ctx, 100, nil); err != nil {
		t.Fatalf("ReplaceKernelsForBlock (empty): %v", err)
	}
	got, err = d.GetKernelsForBlock(ctx, 100)
	if err != nil {
		t.Fatalf("GetKernelsForBlock after clear: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 kernels after clearing, got %d", len(got))
	}
}

func TestReplaceOutputsForBlock_InsertsAndReplaces(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	seedBlock(t, d, 200)

	o0 := Output{
		Index:           0,
		FeaturesVersion: 0,
		OutputType:      1, // COINBASE
		Maturity:        60,
		CoinbaseExtra:   []byte("WUFJagtechE0"),
		Commitment:      bytesOf(33, 0x11),
	}
	o1 := Output{
		Index:           1,
		FeaturesVersion: 0,
		OutputType:      0, // STANDARD
		Commitment:      bytesOf(33, 0x22),
	}

	if err := d.ReplaceOutputsForBlock(ctx, 200, []Output{o0, o1}); err != nil {
		t.Fatalf("ReplaceOutputsForBlock: %v", err)
	}

	got, err := d.GetOutputsForBlock(ctx, 200)
	if err != nil {
		t.Fatalf("GetOutputsForBlock: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 outputs, got %d: %+v", len(got), got)
	}
	if got[0].OutputType != 1 || string(got[0].CoinbaseExtra) != "WUFJagtechE0" {
		t.Errorf("output 0 mismatch: %+v", got[0])
	}
	if got[1].OutputType != 0 || got[1].BlockHeight != 200 {
		t.Errorf("output 1 mismatch: %+v", got[1])
	}

	// Replace with a different set entirely.
	o2 := Output{Index: 0, OutputType: 2, Commitment: bytesOf(33, 0x33)}
	if err := d.ReplaceOutputsForBlock(ctx, 200, []Output{o2}); err != nil {
		t.Fatalf("ReplaceOutputsForBlock (replace): %v", err)
	}
	got, err = d.GetOutputsForBlock(ctx, 200)
	if err != nil {
		t.Fatalf("GetOutputsForBlock after replace: %v", err)
	}
	if len(got) != 1 || got[0].OutputType != 2 {
		t.Fatalf("expected 1 output of type 2 after replace, got %+v", got)
	}
}

func TestGetKernelsForBlock_OrderedByIndex(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	seedBlock(t, d, 300)

	// Insert out of order; GetKernelsForBlock must still return ascending by index.
	kernels := []Kernel{
		{Index: 2, ExcessSigNonce: bytesOf(32, 0x01), ExcessSigSignature: bytesOf(32, 0x02)},
		{Index: 0, ExcessSigNonce: bytesOf(32, 0x03), ExcessSigSignature: bytesOf(32, 0x04)},
		{Index: 1, ExcessSigNonce: bytesOf(32, 0x05), ExcessSigSignature: bytesOf(32, 0x06)},
	}
	if err := d.ReplaceKernelsForBlock(ctx, 300, kernels); err != nil {
		t.Fatalf("ReplaceKernelsForBlock: %v", err)
	}
	got, err := d.GetKernelsForBlock(ctx, 300)
	if err != nil {
		t.Fatalf("GetKernelsForBlock: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 kernels, got %d", len(got))
	}
	for i, k := range got {
		if k.Index != int32(i) {
			t.Errorf("position %d: got kernel index %d, want %d", i, k.Index, i)
		}
	}
}

func TestFindKernelByExcessSigSignature(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	seedBlock(t, d, 400)

	nonce := bytesOf(32, 0x10)
	sig := bytesOf(32, 0x20)
	if err := d.ReplaceKernelsForBlock(ctx, 400, []Kernel{
		{Index: 0, ExcessSigNonce: nonce, ExcessSigSignature: sig, Fee: 42},
	}); err != nil {
		t.Fatalf("ReplaceKernelsForBlock: %v", err)
	}

	got, err := d.FindKernelByExcessSigSignature(ctx, sig)
	if err != nil {
		t.Fatalf("FindKernelByExcessSigSignature: %v", err)
	}
	if got.BlockHeight != 400 || got.Fee != 42 {
		t.Errorf("unexpected kernel: %+v", got)
	}

	_, err = d.FindKernelByExcessSigSignature(ctx, bytesOf(32, 0xFF))
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expected pgx.ErrNoRows for unknown signature, got %v", err)
	}
}

func TestFindKernelByExcessSig(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	seedBlock(t, d, 401)

	nonce := bytesOf(32, 0x30)
	sig := bytesOf(32, 0x40)
	if err := d.ReplaceKernelsForBlock(ctx, 401, []Kernel{
		{Index: 0, ExcessSigNonce: nonce, ExcessSigSignature: sig, Fee: 7},
	}); err != nil {
		t.Fatalf("ReplaceKernelsForBlock: %v", err)
	}

	got, err := d.FindKernelByExcessSig(ctx, nonce, sig)
	if err != nil {
		t.Fatalf("FindKernelByExcessSig: %v", err)
	}
	if got.BlockHeight != 401 || got.Fee != 7 {
		t.Errorf("unexpected kernel: %+v", got)
	}

	// Same signature scalar but wrong nonce must not match - full-signature lookup is
	// stricter than the signature-scalar-only lookup above.
	_, err = d.FindKernelByExcessSig(ctx, bytesOf(32, 0x99), sig)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expected pgx.ErrNoRows for mismatched nonce, got %v", err)
	}
}

func TestFindOutputByCommitment(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	seedBlock(t, d, 500)

	commitment := bytesOf(33, 0x55)
	if err := d.ReplaceOutputsForBlock(ctx, 500, []Output{
		{Index: 0, Commitment: commitment, OutputType: 1, Maturity: 60},
	}); err != nil {
		t.Fatalf("ReplaceOutputsForBlock: %v", err)
	}

	got, err := d.FindOutputByCommitment(ctx, commitment)
	if err != nil {
		t.Fatalf("FindOutputByCommitment: %v", err)
	}
	if got.BlockHeight != 500 || got.Maturity != 60 {
		t.Errorf("unexpected output: %+v", got)
	}

	_, err = d.FindOutputByCommitment(ctx, bytesOf(33, 0xAB))
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expected pgx.ErrNoRows for unknown commitment, got %v", err)
	}
}

// TestKernelsOutputs_CascadeDeleteOnBlockRemoval proves the FK ON DELETE CASCADE from
// migration 0003 actually works: deleting a block's row removes its kernels/outputs
// too, rather than leaving orphaned rows or erroring on the FK.
func TestKernelsOutputs_CascadeDeleteOnBlockRemoval(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	seedBlock(t, d, 600)

	if err := d.ReplaceKernelsForBlock(ctx, 600, []Kernel{
		{Index: 0, ExcessSigNonce: bytesOf(32, 0x01), ExcessSigSignature: bytesOf(32, 0x02)},
	}); err != nil {
		t.Fatalf("ReplaceKernelsForBlock: %v", err)
	}
	if err := d.ReplaceOutputsForBlock(ctx, 600, []Output{
		{Index: 0, Commitment: bytesOf(33, 0x03)},
	}); err != nil {
		t.Fatalf("ReplaceOutputsForBlock: %v", err)
	}

	if _, err := d.Pool.Exec(ctx, `DELETE FROM blocks WHERE height = $1`, uint64(600)); err != nil {
		t.Fatalf("delete block: %v", err)
	}

	kernels, err := d.GetKernelsForBlock(ctx, 600)
	if err != nil {
		t.Fatalf("GetKernelsForBlock after cascade: %v", err)
	}
	if len(kernels) != 0 {
		t.Fatalf("expected kernels to cascade-delete with their block, got %d", len(kernels))
	}
	outputs, err := d.GetOutputsForBlock(ctx, 600)
	if err != nil {
		t.Fatalf("GetOutputsForBlock after cascade: %v", err)
	}
	if len(outputs) != 0 {
		t.Fatalf("expected outputs to cascade-delete with their block, got %d", len(outputs))
	}
}

func TestRecentBlocksStats(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	jagtech := "WUFJagtechE0"
	other := "OtherPool"
	seedBlockFull(t, d, 700, "RXM", &jagtech, 100)
	seedBlockFull(t, d, 701, "RXM", nil, 200)
	seedBlockFull(t, d, 702, "SHA3X", &other, 300)
	seedBlockFull(t, d, 703, "SHA3X", &jagtech, 400)
	seedBlockFull(t, d, 704, "RXT", &jagtech, 500)

	mappings := []PoolTagMapping{{MatchPrefix: "WUF", CanonicalName: "Jagtech"}}
	got, err := d.RecentBlocksStats(ctx, 100, mappings)
	if err != nil {
		t.Fatalf("RecentBlocksStats: %v", err)
	}
	if got.SampleCount != 5 {
		t.Fatalf("expected sample count 5, got %d", got.SampleCount)
	}
	wantAvgDiff := (100.0 + 200.0 + 300.0 + 400.0 + 500.0) / 5.0
	if got.AvgDifficulty != wantAvgDiff {
		t.Errorf("expected avg difficulty %v, got %v", wantAvgDiff, got.AvgDifficulty)
	}

	poolCounts := map[string]int64{}
	for _, p := range got.Pools {
		poolCounts[p.PoolTag] = p.Count
	}
	if poolCounts["Jagtech"] != 3 {
		t.Errorf("expected Jagtech count 3, got %d (%+v)", poolCounts["Jagtech"], got.Pools)
	}
	if poolCounts["unknown"] != 1 {
		t.Errorf("expected unknown count 1, got %d", poolCounts["unknown"])
	}
	if poolCounts["OtherPool"] != 1 {
		t.Errorf("expected OtherPool count 1, got %d", poolCounts["OtherPool"])
	}

	algoCounts := map[string]int64{}
	for _, a := range got.Algos {
		algoCounts[a.Algo] = a.Count
	}
	if algoCounts["RXM"] != 2 || algoCounts["SHA3X"] != 2 || algoCounts["RXT"] != 1 {
		t.Errorf("unexpected algo breakdown: %+v", got.Algos)
	}
}

// TestRecentBlocksStats_LimitBoundsSample proves the `limit` parameter actually
// bounds the sample to the most recent N blocks (by height descending), not the whole
// table.
func TestRecentBlocksStats_LimitBoundsSample(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	for h := uint64(800); h < 805; h++ {
		seedBlockFull(t, d, h, "RXM", nil, 10)
	}

	got, err := d.RecentBlocksStats(ctx, 3, nil)
	if err != nil {
		t.Fatalf("RecentBlocksStats: %v", err)
	}
	if got.SampleCount != 3 {
		t.Fatalf("expected sample count bounded to limit 3, got %d", got.SampleCount)
	}
}

// TestRecentBlocksStats_EmptyTable proves this degrades to a clean zero-value result
// (not an error, not a crash on e.g. AVG() over zero rows) when `blocks` is empty -
// the shape the front page sees before the indexer has written anything yet.
func TestRecentBlocksStats_EmptyTable(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	got, err := d.RecentBlocksStats(ctx, 100, nil)
	if err != nil {
		t.Fatalf("RecentBlocksStats: %v", err)
	}
	if got.SampleCount != 0 || len(got.Pools) != 0 || len(got.Algos) != 0 || got.AvgDifficulty != 0 {
		t.Fatalf("expected zero-value stats on empty table, got %+v", got)
	}
}

func TestRecentBlocksStats_RejectsNonPositiveLimit(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	if _, err := d.RecentBlocksStats(ctx, 0, nil); err == nil {
		t.Fatal("expected error for limit 0")
	}
}

// seedBlockWithPoolTag inserts a minimal valid `blocks` row at height with poolTag set
// (nil for a NULL pool_tag), for tests exercising pool_tag-based queries that
// seedBlock's fixed "no pool tag" shape doesn't cover.
func seedBlockWithPoolTag(t *testing.T, d *DB, height uint64, poolTag *string) {
	t.Helper()
	err := d.UpsertBlock(context.Background(), Block{
		Height:            height,
		Hash:              "aa",
		PrevHash:          "bb",
		OutputMr:          []byte{},
		BlockOutputMr:     []byte{},
		KernelMr:          []byte{},
		InputMr:           []byte{},
		TotalKernelOffset: []byte{},
		TotalScriptOffset: []byte{},
		ValidatorNodeMr:   []byte{},
		PowData:           []byte{},
		PowAlgo:           "RXM",
		PoolTag:           poolTag,
	})
	if err != nil {
		t.Fatalf("db: seed block %d with pool tag: %v", height, err)
	}
}

// TestUnmappedPoolTags proves UnmappedPoolTags excludes both a pool_tag matching a
// mapping's MatchPrefix (already folded into a canonical series, so not "unmapped")
// and a NULL pool_tag (not a real pool tag at all), while including a real pool_tag
// that matches no mapping entry.
func TestUnmappedPoolTags(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	wufTag := "WUFJagtechE0"
	unmappedTag := "pool.kryptex.com"
	seedBlockWithPoolTag(t, d, 700, &wufTag)      // matches WUF mapping - must be excluded
	seedBlockWithPoolTag(t, d, 701, &unmappedTag) // real, unmapped tag - must be included
	seedBlockWithPoolTag(t, d, 702, nil)          // NULL pool_tag - must be excluded

	mappings := []PoolTagMapping{{MatchPrefix: "WUF", CanonicalName: "Jagtech"}}
	got, err := d.UnmappedPoolTags(ctx, mappings)
	if err != nil {
		t.Fatalf("UnmappedPoolTags: %v", err)
	}
	want := []string{unmappedTag}
	if len(got) != len(want) {
		t.Fatalf("UnmappedPoolTags: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("UnmappedPoolTags[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

// bytesOf returns an n-byte slice filled with fill, for building distinct
// fixture excess-sig/commitment values without hand-writing byte literals.
func bytesOf(n int, fill byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = fill
	}
	return b
}
