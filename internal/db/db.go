// Package db provides the Postgres access layer for go-tari-explorer: a minimal, simple
// embedded-SQL migration runner (no golang-migrate dependency - see Migrate below for
// why) plus the query/upsert methods the indexer and HTTP server need against the
// `blocks` / `block_kernels` tables.
//
// Schema is intentionally minimal-viable for v1 (see internal/db/migrations/0001_init.up.sql)
// and is expected to grow incrementally as the explorer gains features - don't over-build
// this layer ahead of what's actually needed yet.
package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.up.sql
var migrationFS embed.FS

// DB wraps a pgx connection pool with the query methods this repo needs.
type DB struct {
	Pool *pgxpool.Pool
}

// Connect opens a pgx pool against dsn and returns a ready-to-use DB. Callers are
// responsible for calling Close when done. dsn is a standard Postgres connection string,
// e.g. "postgres://user:pass@localhost:5432/tari_explorer?sslmode=disable".
func Connect(ctx context.Context, dsn string) (*DB, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("db: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return &DB{Pool: pool}, nil
}

func (d *DB) Close() {
	d.Pool.Close()
}

// Migrate applies every embedded *.up.sql migration that hasn't already been recorded in
// schema_migrations, in filename order, each inside its own transaction. This is a
// deliberately small hand-rolled runner instead of golang-migrate: v1 only has one
// migration file and the whole point of "minimal-viable" here is not pulling in a full
// migration framework (with its own CLI, source drivers, etc.) for what is currently a
// single CREATE TABLE statement. If/when this schema grows enough that golang-migrate's
// down-migrations/dirty-state tooling earns its keep, swap this runner for it - the
// migrations/*.up.sql files are already named in golang-migrate's <version>_<name>.up.sql
// convention so that swap would be a drop-in.
func (d *DB) Migrate(ctx context.Context) error {
	if _, err := d.Pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("db: migrate: create schema_migrations: %w", err)
	}

	entries, err := fs.Glob(migrationFS, "migrations/*.up.sql")
	if err != nil {
		return fmt.Errorf("db: migrate: glob: %w", err)
	}
	sort.Strings(entries)

	for _, entry := range entries {
		version := strings.TrimSuffix(strings.TrimPrefix(entry, "migrations/"), ".up.sql")

		var alreadyApplied bool
		err := d.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&alreadyApplied)
		if err != nil {
			return fmt.Errorf("db: migrate: check %s: %w", version, err)
		}
		if alreadyApplied {
			continue
		}

		sqlBytes, err := migrationFS.ReadFile(entry)
		if err != nil {
			return fmt.Errorf("db: migrate: read %s: %w", entry, err)
		}

		tx, err := d.Pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("db: migrate: begin %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("db: migrate: apply %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("db: migrate: record %s: %w", version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("db: migrate: commit %s: %w", version, err)
		}
	}
	return nil
}

// Block is the row shape for the `blocks` table: a full decomposition of the real Tari
// protobuf BlockHeader message (github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated,
// block.proto) plus its nested ProofOfWork message (1:1 per block, so flattened into
// this same row rather than a separate table, with a pow_ prefix on its two fields),
// plus the pre-existing block-level summary fields (string PowAlgo classification,
// PoolTag, KernelCount/OutputCount) that are derived from the block body rather than
// the header itself.
//
// Hash/PrevHash stay hex-encoded strings (as originally stored, and as rendered
// verbatim by internal/server's templates) rather than the proto's raw []byte, to avoid
// a breaking change to the UI layer. Every other BlockHeader/ProofOfWork field is carried
// as its real wire type: []byte for proto `bytes` fields, uint64/uint32 for proto
// integer fields.
type Block struct {
	// BlockHeader fields (github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated.BlockHeader).
	Height            uint64
	Hash              string // hex-encoded BlockHeader.Hash
	Version           uint32
	PrevHash          string // hex-encoded BlockHeader.PrevHash
	Timestamp         int64
	OutputMr          []byte
	BlockOutputMr     []byte
	KernelMr          []byte
	InputMr           []byte
	TotalKernelOffset []byte
	Nonce             uint64
	KernelMmrSize     uint64
	OutputMmrSize     uint64
	TotalScriptOffset []byte
	ValidatorNodeMr   []byte
	ValidatorNodeSize uint64

	// ProofOfWork fields (BlockHeader.Pow), flattened with a pow_ prefix. PowAlgoRaw is
	// the raw wire id (0=RXM, 1=SHA3X, 2=RXT, 3=C29); PowAlgo below is the classified
	// string built from it via internal/poolattr.AlgoFromRaw - both are kept, see
	// migrations/0002_block_header_decomposition.up.sql.
	PowAlgoRaw uint64
	PowData    []byte

	// Block-level summary fields, derived from the block body (AggregateBody), not the
	// header - pre-existing from 0001_init, unchanged by this decomposition.
	PowAlgo     string // "RXM" | "RXT" | "C29" | "SHA3X" (see internal/poolattr)
	Difficulty  int64
	KernelCount int32
	OutputCount int32
	PoolTag     *string // nil == unattributed
}

// UpsertBlock inserts or updates a single block row, keyed on height. Used by the
// indexer for both backfill and tip-follow modes.
func (d *DB) UpsertBlock(ctx context.Context, b Block) error {
	_, err := d.Pool.Exec(ctx, `
		INSERT INTO blocks (
			height, hash, version, prev_hash, "timestamp",
			output_mr, block_output_mr, kernel_mr, input_mr, total_kernel_offset, nonce,
			pow_algo_raw, pow_data,
			kernel_mmr_size, output_mmr_size, total_script_offset, validator_node_mr, validator_node_size,
			pow_algo, difficulty, kernel_count, output_count, pool_tag
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)
		ON CONFLICT (height) DO UPDATE SET
			hash = EXCLUDED.hash,
			version = EXCLUDED.version,
			prev_hash = EXCLUDED.prev_hash,
			"timestamp" = EXCLUDED."timestamp",
			output_mr = EXCLUDED.output_mr,
			block_output_mr = EXCLUDED.block_output_mr,
			kernel_mr = EXCLUDED.kernel_mr,
			input_mr = EXCLUDED.input_mr,
			total_kernel_offset = EXCLUDED.total_kernel_offset,
			nonce = EXCLUDED.nonce,
			pow_algo_raw = EXCLUDED.pow_algo_raw,
			pow_data = EXCLUDED.pow_data,
			kernel_mmr_size = EXCLUDED.kernel_mmr_size,
			output_mmr_size = EXCLUDED.output_mmr_size,
			total_script_offset = EXCLUDED.total_script_offset,
			validator_node_mr = EXCLUDED.validator_node_mr,
			validator_node_size = EXCLUDED.validator_node_size,
			pow_algo = EXCLUDED.pow_algo,
			difficulty = EXCLUDED.difficulty,
			kernel_count = EXCLUDED.kernel_count,
			output_count = EXCLUDED.output_count,
			pool_tag = EXCLUDED.pool_tag
	`,
		b.Height, b.Hash, b.Version, b.PrevHash, b.Timestamp,
		b.OutputMr, b.BlockOutputMr, b.KernelMr, b.InputMr, b.TotalKernelOffset, b.Nonce,
		b.PowAlgoRaw, b.PowData,
		b.KernelMmrSize, b.OutputMmrSize, b.TotalScriptOffset, b.ValidatorNodeMr, b.ValidatorNodeSize,
		b.PowAlgo, b.Difficulty, b.KernelCount, b.OutputCount, b.PoolTag,
	)
	if err != nil {
		return fmt.Errorf("db: upsert block %d: %w", b.Height, err)
	}
	return nil
}

// blockColumns is the shared column list (and scan order) used by ListBlocks and
// GetBlock, so the two queries can't silently drift out of sync with each other or
// with scanBlockRow below.
const blockColumns = `
	height, hash, version, prev_hash, "timestamp",
	output_mr, block_output_mr, kernel_mr, input_mr, total_kernel_offset, nonce,
	pow_algo_raw, pow_data,
	kernel_mmr_size, output_mmr_size, total_script_offset, validator_node_mr, validator_node_size,
	pow_algo, difficulty, kernel_count, output_count, pool_tag
`

// scanBlockRow scans a row shaped like blockColumns into a Block.
func scanBlockRow(row pgx.Row, b *Block) error {
	return row.Scan(
		&b.Height, &b.Hash, &b.Version, &b.PrevHash, &b.Timestamp,
		&b.OutputMr, &b.BlockOutputMr, &b.KernelMr, &b.InputMr, &b.TotalKernelOffset, &b.Nonce,
		&b.PowAlgoRaw, &b.PowData,
		&b.KernelMmrSize, &b.OutputMmrSize, &b.TotalScriptOffset, &b.ValidatorNodeMr, &b.ValidatorNodeSize,
		&b.PowAlgo, &b.Difficulty, &b.KernelCount, &b.OutputCount, &b.PoolTag,
	)
}

// ListBlocks returns up to limit blocks ordered by height descending, starting strictly
// below beforeHeight (pass a very large value, e.g. math.MaxInt64, for the first page).
// Used to drive the paginated / HTMX "load more" blocks-list page.
func (d *DB) ListBlocks(ctx context.Context, beforeHeight int64, limit int) ([]Block, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT `+blockColumns+`
		FROM blocks
		WHERE height < $1
		ORDER BY height DESC
		LIMIT $2
	`, beforeHeight, limit)
	if err != nil {
		return nil, fmt.Errorf("db: list blocks: %w", err)
	}
	defer rows.Close()

	var out []Block
	for rows.Next() {
		var b Block
		if err := scanBlockRow(rows, &b); err != nil {
			return nil, fmt.Errorf("db: list blocks: scan: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetBlock returns a single block by height, or pgx.ErrNoRows if it doesn't exist.
func (d *DB) GetBlock(ctx context.Context, height uint64) (Block, error) {
	var b Block
	err := scanBlockRow(d.Pool.QueryRow(ctx, `
		SELECT `+blockColumns+`
		FROM blocks
		WHERE height = $1
	`, height), &b)
	if err != nil {
		if err == pgx.ErrNoRows {
			return Block{}, pgx.ErrNoRows
		}
		return Block{}, fmt.Errorf("db: get block %d: %w", height, err)
	}
	return b, nil
}

// MaxIndexedHeight returns the highest height currently stored, or 0 if the table is
// empty. Used by the indexer to know where backfill should resume.
func (d *DB) MaxIndexedHeight(ctx context.Context) (uint64, error) {
	var max *uint64
	err := d.Pool.QueryRow(ctx, `SELECT MAX(height) FROM blocks`).Scan(&max)
	if err != nil {
		return 0, fmt.Errorf("db: max indexed height: %w", err)
	}
	if max == nil {
		return 0, nil
	}
	return *max, nil
}

// Kernel is the row shape for the `kernels` table: one row per
// github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated.TransactionKernel found in a
// block's body, keyed on (BlockHeight, Index). ExcessSigSignature is the canonical
// per-transaction identifier used by internal/txsearch and the live TransactionState
// RPC; ExcessSigNonce is carried alongside it because the two together make up the
// real wire Signature message (public_nonce + signature), not because the nonce alone
// is useful for lookups.
type Kernel struct {
	BlockHeight        uint64
	Index              int32
	Features           uint64
	Fee                uint64
	LockHeight         uint64
	Excess             []byte
	ExcessSigNonce     []byte
	ExcessSigSignature []byte
	Hash               []byte
}

// Output is the row shape for the `outputs` table: one row per
// tari_generated.TransactionOutput found in a block's body, keyed on
// (BlockHeight, Index). FeaturesVersion/OutputType/Maturity/CoinbaseExtra come from
// the output's nested OutputFeatures message; Commitment is the output's own
// homomorphic commitment, searchable via SearchUtxos.
type Output struct {
	BlockHeight     uint64
	Index           int32
	FeaturesVersion uint32
	OutputType      uint32
	Maturity        uint64
	CoinbaseExtra   []byte
	Commitment      []byte
}

// nonNilBytes coalesces a nil []byte to an empty (non-nil) one. Needed because
// tari_generated's proto3 getters (e.g. TransactionKernel.GetExcess,
// OutputFeatures.GetCoinbaseExtra) return a nil slice for an unset/empty bytes field -
// completely normal for, say, a non-coinbase output's CoinbaseExtra - but pgx sends a
// nil []byte as SQL NULL, which the kernels/outputs columns below reject (NOT NULL,
// per migrations/0003_kernels_outputs.up.sql). Without this, indexing any block
// containing a standard (non-coinbase) output would fail outright.
func nonNilBytes(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

// ReplaceKernelsForBlock atomically replaces every kernel row for blockHeight with
// kernels, inside a single transaction. DELETE-then-INSERT (rather than a diffing
// update) is the simplest correct approach here: kernels have no natural per-row
// update-key within a block re-index (there's nothing to ON CONFLICT against short of
// the same (block_height, kernel_index) pair this already keys on), and block bodies
// are small enough - typically tens of kernels/outputs, not thousands - that a full
// delete+reinsert per re-indexed block is not a meaningful perf concern. Safe to call
// with an empty kernels slice (e.g. a block with no kernels), which simply clears any
// previously stored rows for that height.
func (d *DB) ReplaceKernelsForBlock(ctx context.Context, blockHeight uint64, kernels []Kernel) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: replace kernels for block %d: begin: %w", blockHeight, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM kernels WHERE block_height = $1`, blockHeight); err != nil {
		return fmt.Errorf("db: replace kernels for block %d: delete: %w", blockHeight, err)
	}
	for _, k := range kernels {
		if _, err := tx.Exec(ctx, `
			INSERT INTO kernels (
				block_height, kernel_index, features, fee, lock_height,
				excess, excess_sig_nonce, excess_sig_signature, hash
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		`, blockHeight, k.Index, k.Features, k.Fee, k.LockHeight,
			nonNilBytes(k.Excess), nonNilBytes(k.ExcessSigNonce), nonNilBytes(k.ExcessSigSignature), nonNilBytes(k.Hash)); err != nil {
			return fmt.Errorf("db: replace kernels for block %d: insert index %d: %w", blockHeight, k.Index, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: replace kernels for block %d: commit: %w", blockHeight, err)
	}
	return nil
}

// ReplaceOutputsForBlock atomically replaces every output row for blockHeight with
// outputs, inside a single transaction. Same DELETE-then-INSERT rationale as
// ReplaceKernelsForBlock above.
func (d *DB) ReplaceOutputsForBlock(ctx context.Context, blockHeight uint64, outputs []Output) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: replace outputs for block %d: begin: %w", blockHeight, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM outputs WHERE block_height = $1`, blockHeight); err != nil {
		return fmt.Errorf("db: replace outputs for block %d: delete: %w", blockHeight, err)
	}
	for _, o := range outputs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO outputs (
				block_height, output_index, features_version, output_type, maturity,
				coinbase_extra, commitment
			) VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, blockHeight, o.Index, o.FeaturesVersion, o.OutputType, o.Maturity,
			nonNilBytes(o.CoinbaseExtra), nonNilBytes(o.Commitment)); err != nil {
			return fmt.Errorf("db: replace outputs for block %d: insert index %d: %w", blockHeight, o.Index, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: replace outputs for block %d: commit: %w", blockHeight, err)
	}
	return nil
}

// kernelColumns/scanKernelRow mirror blockColumns/scanBlockRow's pattern above, keeping
// the column list and scan order for `kernels` in one place.
const kernelColumns = `
	block_height, kernel_index, features, fee, lock_height,
	excess, excess_sig_nonce, excess_sig_signature, hash
`

func scanKernelRow(row pgx.Row, k *Kernel) error {
	return row.Scan(
		&k.BlockHeight, &k.Index, &k.Features, &k.Fee, &k.LockHeight,
		&k.Excess, &k.ExcessSigNonce, &k.ExcessSigSignature, &k.Hash,
	)
}

// GetKernelsForBlock returns every kernel row for blockHeight, ordered by kernel_index
// ascending (the order they appeared in the block's body).
func (d *DB) GetKernelsForBlock(ctx context.Context, blockHeight uint64) ([]Kernel, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT `+kernelColumns+`
		FROM kernels
		WHERE block_height = $1
		ORDER BY kernel_index ASC
	`, blockHeight)
	if err != nil {
		return nil, fmt.Errorf("db: get kernels for block %d: %w", blockHeight, err)
	}
	defer rows.Close()

	var out []Kernel
	for rows.Next() {
		var k Kernel
		if err := scanKernelRow(rows, &k); err != nil {
			return nil, fmt.Errorf("db: get kernels for block %d: scan: %w", blockHeight, err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// outputColumns/scanOutputRow mirror kernelColumns/scanKernelRow above for `outputs`.
const outputColumns = `
	block_height, output_index, features_version, output_type, maturity,
	coinbase_extra, commitment
`

func scanOutputRow(row pgx.Row, o *Output) error {
	return row.Scan(
		&o.BlockHeight, &o.Index, &o.FeaturesVersion, &o.OutputType, &o.Maturity,
		&o.CoinbaseExtra, &o.Commitment,
	)
}

// GetOutputsForBlock returns every output row for blockHeight, ordered by output_index
// ascending (the order they appeared in the block's body).
func (d *DB) GetOutputsForBlock(ctx context.Context, blockHeight uint64) ([]Output, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT `+outputColumns+`
		FROM outputs
		WHERE block_height = $1
		ORDER BY output_index ASC
	`, blockHeight)
	if err != nil {
		return nil, fmt.Errorf("db: get outputs for block %d: %w", blockHeight, err)
	}
	defer rows.Close()

	var out []Output
	for rows.Next() {
		var o Output
		if err := scanOutputRow(rows, &o); err != nil {
			return nil, fmt.Errorf("db: get outputs for block %d: scan: %w", blockHeight, err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// FindKernelByExcessSigSignature looks up a kernel by its excess signature's scalar
// component alone (excess_sig_signature), ignoring the nonce. This is the search key
// internal/txsearch uses for a bare 32-byte ("64 hex char") query, since a caller
// providing only the signature scalar (not the full nonce+signature pair) can still be
// matched against what's already indexed. Returns pgx.ErrNoRows if nothing matches; if
// more than one kernel happens to share a signature scalar (see the migration's note on
// why this column isn't UNIQUE) the first by (block_height, kernel_index) is returned.
func (d *DB) FindKernelByExcessSigSignature(ctx context.Context, sig []byte) (Kernel, error) {
	var k Kernel
	err := scanKernelRow(d.Pool.QueryRow(ctx, `
		SELECT `+kernelColumns+`
		FROM kernels
		WHERE excess_sig_signature = $1
		ORDER BY block_height ASC, kernel_index ASC
		LIMIT 1
	`, sig), &k)
	if err != nil {
		if err == pgx.ErrNoRows {
			return Kernel{}, pgx.ErrNoRows
		}
		return Kernel{}, fmt.Errorf("db: find kernel by excess sig signature: %w", err)
	}
	return k, nil
}

// FindKernelByExcessSig looks up a kernel by its full excess signature (both the
// public nonce and the signature scalar), for a 64-byte ("128 hex char") query that
// unambiguously identifies one Signature message. Returns pgx.ErrNoRows if nothing
// matches.
func (d *DB) FindKernelByExcessSig(ctx context.Context, nonce, sig []byte) (Kernel, error) {
	var k Kernel
	err := scanKernelRow(d.Pool.QueryRow(ctx, `
		SELECT `+kernelColumns+`
		FROM kernels
		WHERE excess_sig_nonce = $1 AND excess_sig_signature = $2
		ORDER BY block_height ASC, kernel_index ASC
		LIMIT 1
	`, nonce, sig), &k)
	if err != nil {
		if err == pgx.ErrNoRows {
			return Kernel{}, pgx.ErrNoRows
		}
		return Kernel{}, fmt.Errorf("db: find kernel by excess sig: %w", err)
	}
	return k, nil
}

// FindOutputByCommitment looks up an output by its commitment. Returns pgx.ErrNoRows
// if nothing matches; if more than one output happens to share a commitment (see the
// migration's note on why this column isn't UNIQUE) the first by
// (block_height, output_index) is returned.
func (d *DB) FindOutputByCommitment(ctx context.Context, commitment []byte) (Output, error) {
	var o Output
	err := scanOutputRow(d.Pool.QueryRow(ctx, `
		SELECT `+outputColumns+`
		FROM outputs
		WHERE commitment = $1
		ORDER BY block_height ASC, output_index ASC
		LIMIT 1
	`, commitment), &o)
	if err != nil {
		if err == pgx.ErrNoRows {
			return Output{}, pgx.ErrNoRows
		}
		return Output{}, fmt.Errorf("db: find output by commitment: %w", err)
	}
	return o, nil
}

// AlgoBucketRow is one row of the height-bucketed pow-algo aggregation report: a
// [BucketStart, BucketEnd] inclusive height range, plus the count of blocks in that
// range attributed to each of the four known internal/poolattr.PowAlgo values.
type AlgoBucketRow struct {
	BucketStart uint64
	BucketEnd   uint64
	RXM         int64
	RXT         int64
	C29         int64
	SHA3X       int64
}

// AlgoBucketCounts groups blocks in [fromHeight, toHeight] (inclusive) into consecutive
// buckets of bucketSize heights (bucket boundaries aligned to multiples of bucketSize,
// e.g. bucket-size 1000 gives buckets [0,999], [1000,1999], ...) and counts, per bucket,
// how many blocks were mined on each pow_algo. The aggregation is done entirely in
// Postgres via integer bucketing + FILTER-clause conditional counts - this table can
// have tens of thousands of rows, so we never pull raw rows into Go to aggregate here.
// Results are ordered by bucket_start ascending. bucketSize must be > 0.
func (d *DB) AlgoBucketCounts(ctx context.Context, bucketSize uint64, fromHeight, toHeight uint64) ([]AlgoBucketRow, error) {
	if bucketSize == 0 {
		return nil, fmt.Errorf("db: algo bucket counts: bucket size must be > 0")
	}

	rows, err := d.Pool.Query(ctx, `
		SELECT
			(height / $1) * $1 AS bucket_start,
			COUNT(*) FILTER (WHERE pow_algo = 'RXM')   AS rxm,
			COUNT(*) FILTER (WHERE pow_algo = 'RXT')   AS rxt,
			COUNT(*) FILTER (WHERE pow_algo = 'C29')   AS c29,
			COUNT(*) FILTER (WHERE pow_algo = 'SHA3X') AS sha3x
		FROM blocks
		WHERE height BETWEEN $2 AND $3
		GROUP BY bucket_start
		ORDER BY bucket_start ASC
	`, bucketSize, fromHeight, toHeight)
	if err != nil {
		return nil, fmt.Errorf("db: algo bucket counts: %w", err)
	}
	defer rows.Close()

	var out []AlgoBucketRow
	for rows.Next() {
		var r AlgoBucketRow
		if err := rows.Scan(&r.BucketStart, &r.RXM, &r.RXT, &r.C29, &r.SHA3X); err != nil {
			return nil, fmt.Errorf("db: algo bucket counts: scan: %w", err)
		}
		r.BucketEnd = r.BucketStart + bucketSize - 1
		out = append(out, r)
	}
	return out, rows.Err()
}
