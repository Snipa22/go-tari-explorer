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
	"time"

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
		nonNilBytes(b.OutputMr), nonNilBytes(b.BlockOutputMr), nonNilBytes(b.KernelMr), nonNilBytes(b.InputMr), nonNilBytes(b.TotalKernelOffset), b.Nonce,
		b.PowAlgoRaw, nonNilBytes(b.PowData),
		b.KernelMmrSize, b.OutputMmrSize, nonNilBytes(b.TotalScriptOffset), nonNilBytes(b.ValidatorNodeMr), b.ValidatorNodeSize,
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

// PoolTagMapping is one entry in an ordered "known tag-family -> canonical display
// name" mapping, used to fold a single pool operator's multiple per-node/per-worker
// pool_tag values (e.g. WUFJagtechE0, WUFJagtechS1, WUF  Ahri   , ...) into one display
// series for the pool-share chart, and to select that same family for the per-pool
// algo-breakdown view. This is a display/query-layer concept only: it is evaluated
// entirely inside PoolShareBucketCounts/AlgoBucketCountsForPool's SQL and never writes
// back to (or reads a decision from) the stored pool_tag column - internal/poolattr's
// attribution logic is unaffected by this mapping.
//
// MatchPrefix is checked via `pool_tag LIKE MatchPrefix || '%'`. Callers pass an
// ordered []PoolTagMapping; entries are tried in order and the first match wins, so
// list more specific prefixes before more general ones if that ever matters. A
// pool_tag that matches no entry falls back to being its own series, unchanged from
// this mechanism not existing at all - so adding no entries (nil/empty slice)
// reproduces the pre-mapping behavior exactly, and adding a second pool operator's
// per-node family later is just appending one more {MatchPrefix, CanonicalName} entry,
// not a new code path. See internal/analysis.DefaultPoolTagMappings for the current
// production entry (WUF -> Jagtech) and how it was derived from live data.
type PoolTagMapping struct {
	MatchPrefix   string
	CanonicalName string
}

// PoolShareBucketRow is one (bucket, pool) row of the height-bucketed pool market-share
// report: how many blocks in [BucketStart, BucketEnd] were attributed to PoolTag.
// PoolTag is either a real pool_tag value, the literal string "unknown" (blocks with a
// NULL pool_tag - unattributed), or the literal string "other" (blocks attributed to a
// real pool tag that didn't make the top-N cut for the queried range, see
// PoolShareBucketCounts' topN parameter). This is a "long" row shape (one row per
// bucket+pool combination) rather than AlgoBucketRow's fixed-column shape, because the
// set of pool tags is open-ended/data-dependent while the four pow-algo values are not.
type PoolShareBucketRow struct {
	BucketStart uint64
	BucketEnd   uint64
	PoolTag     string
	Count       int64
}

// PoolShareBucketCounts groups blocks in [fromHeight, toHeight] (inclusive) into the
// same height buckets as AlgoBucketCounts, and within each bucket counts how many
// blocks were mined by each pool_tag - after first folding pool_tag through mappings
// (see PoolTagMapping's doc comment): any pool_tag matching a mapping entry's
// MatchPrefix is counted under that entry's CanonicalName instead of its own raw
// pool_tag, so e.g. every WUFJagtech* / WUF  Ahri   -shaped tag can land in one
// "Jagtech" series rather than one series per physical node. mappings may be nil/empty
// to disable this folding entirely (pre-mapping behavior).
//
// To keep the result (and any legend built from it) bounded, only the topN mapped
// tags by total block count over the whole queried range are kept as their own series;
// every other non-null mapped tag is folded into a single "other" series, and NULL
// pool_tag (unattributed blocks) is always its own "unknown" series regardless of
// topN. topN is a caller-supplied cap (the analysis HTTP handlers default to 8, chosen
// as a reasonable legend size for a chart embedded at normal page width - see
// internal/server/analysis.go) rather than hardcoded here, so callers can tune it.
//
// As with AlgoBucketCounts, all aggregation (mapping, bucketing, top-N ranking,
// grouping) happens Postgres-side via a CTE + GROUP BY - raw block rows are never
// pulled into Go to aggregate here. Results are ordered by bucket_start, then pool_tag,
// ascending. bucketSize must be > 0 and topN must be >= 0 (0 means every non-null
// mapped tag folds into "other").
func (d *DB) PoolShareBucketCounts(ctx context.Context, bucketSize uint64, fromHeight, toHeight uint64, topN int, mappings []PoolTagMapping) ([]PoolShareBucketRow, error) {
	if bucketSize == 0 {
		return nil, fmt.Errorf("db: pool share bucket counts: bucket size must be > 0")
	}
	if topN < 0 {
		return nil, fmt.Errorf("db: pool share bucket counts: topN must be >= 0")
	}

	// args starts with the 4 fixed positional params ($1 bucketSize, $2 from,
	// $3 to, $4 topN); each mapping entry then appends its own (LIKE pattern,
	// canonical name) pair as two more params, referenced by position in the CASE
	// expression built below.
	args := []interface{}{bucketSize, fromHeight, toHeight, topN}
	var caseClauses strings.Builder
	for _, m := range mappings {
		args = append(args, m.MatchPrefix+"%", m.CanonicalName)
		patternParam := len(args) - 1
		nameParam := len(args)
		fmt.Fprintf(&caseClauses, "\n				WHEN pool_tag LIKE $%d THEN $%d", patternParam, nameParam)
	}

	query := fmt.Sprintf(`
		WITH mapped AS (
			SELECT
				height,
				CASE
					WHEN pool_tag IS NULL THEN NULL%s
					ELSE pool_tag
				END AS mapped_tag
			FROM blocks
			WHERE height BETWEEN $2 AND $3
		),
		top_pools AS (
			SELECT mapped_tag
			FROM mapped
			WHERE mapped_tag IS NOT NULL
			GROUP BY mapped_tag
			ORDER BY COUNT(*) DESC
			LIMIT $4
		),
		classified AS (
			SELECT
				(height / $1) * $1 AS bucket_start,
				CASE
					WHEN mapped_tag IS NULL THEN 'unknown'
					WHEN mapped_tag IN (SELECT mapped_tag FROM top_pools) THEN mapped_tag
					ELSE 'other'
				END AS pool_key
			FROM mapped
		)
		SELECT bucket_start, pool_key, COUNT(*) AS block_count
		FROM classified
		GROUP BY bucket_start, pool_key
		ORDER BY bucket_start ASC, pool_key ASC
	`, caseClauses.String())

	rows, err := d.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: pool share bucket counts: %w", err)
	}
	defer rows.Close()

	var out []PoolShareBucketRow
	for rows.Next() {
		var r PoolShareBucketRow
		if err := rows.Scan(&r.BucketStart, &r.PoolTag, &r.Count); err != nil {
			return nil, fmt.Errorf("db: pool share bucket counts: scan: %w", err)
		}
		r.BucketEnd = r.BucketStart + bucketSize - 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// AlgoBucketCountsForPool is AlgoBucketCounts scoped to a single canonical pool name
// from a PoolTagMapping list (see that type's doc comment) - the per-pool algo-
// breakdown view's query. canonicalName is looked up against mappings: every
// MatchPrefix entry whose CanonicalName equals canonicalName contributes a
// `pool_tag LIKE prefix || '%'` clause (OR'd together), so a merged series like
// "Jagtech" that spans multiple raw pool_tag prefixes is still scoped correctly. If no
// mapping entry has that CanonicalName, canonicalName is instead treated as a literal,
// unmapped pool_tag value (exact match) - so this same endpoint also works for a pool
// operator who hasn't (yet, or ever needs to) fragment their tags across mapping
// entries, keeping the mechanism general rather than tied to any specific mapping.
// bucketSize must be > 0; canonicalName must be non-empty.
func (d *DB) AlgoBucketCountsForPool(ctx context.Context, bucketSize uint64, fromHeight, toHeight uint64, mappings []PoolTagMapping, canonicalName string) ([]AlgoBucketRow, error) {
	if bucketSize == 0 {
		return nil, fmt.Errorf("db: algo bucket counts for pool: bucket size must be > 0")
	}
	if canonicalName == "" {
		return nil, fmt.Errorf("db: algo bucket counts for pool: canonicalName must be non-empty")
	}

	args := []interface{}{bucketSize, fromHeight, toHeight}
	var poolClauses []string
	for _, m := range mappings {
		if m.CanonicalName != canonicalName {
			continue
		}
		args = append(args, m.MatchPrefix+"%")
		poolClauses = append(poolClauses, fmt.Sprintf("pool_tag LIKE $%d", len(args)))
	}
	var poolFilter string
	if len(poolClauses) > 0 {
		poolFilter = "(" + strings.Join(poolClauses, " OR ") + ")"
	} else {
		args = append(args, canonicalName)
		poolFilter = fmt.Sprintf("pool_tag = $%d", len(args))
	}

	query := fmt.Sprintf(`
		SELECT
			(height / $1) * $1 AS bucket_start,
			COUNT(*) FILTER (WHERE pow_algo = 'RXM')   AS rxm,
			COUNT(*) FILTER (WHERE pow_algo = 'RXT')   AS rxt,
			COUNT(*) FILTER (WHERE pow_algo = 'C29')   AS c29,
			COUNT(*) FILTER (WHERE pow_algo = 'SHA3X') AS sha3x
		FROM blocks
		WHERE height BETWEEN $2 AND $3 AND %s
		GROUP BY bucket_start
		ORDER BY bucket_start ASC
	`, poolFilter)

	rows, err := d.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: algo bucket counts for pool: %w", err)
	}
	defer rows.Close()

	var out []AlgoBucketRow
	for rows.Next() {
		var r AlgoBucketRow
		if err := rows.Scan(&r.BucketStart, &r.RXM, &r.RXT, &r.C29, &r.SHA3X); err != nil {
			return nil, fmt.Errorf("db: algo bucket counts for pool: scan: %w", err)
		}
		r.BucketEnd = r.BucketStart + bucketSize - 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// UnmappedPoolTags returns every distinct non-NULL pool_tag value currently stored in
// `blocks` that does not match any entry in mappings' MatchPrefix (i.e. does not
// satisfy `pool_tag LIKE MatchPrefix || '%'` for any mapping entry - see
// PoolTagMapping's doc comment). These are pool operators whose blocks haven't (yet,
// or may never need to) be folded into a canonical mapping entry, and are exactly as
// valid a literal ?pool= value to AlgoBucketCountsForPool as any CanonicalName (see
// that method's doc comment on its literal-exact-match fallback for a canonicalName
// absent from mappings). Results are ordered alphabetically; NULL pool_tag (unattributed
// blocks) is never included, since it isn't a real pool tag. mappings may be nil/empty,
// in which case every distinct non-NULL pool_tag is returned.
//
// As with AlgoBucketCounts/PoolShareBucketCounts/AlgoBucketCountsForPool above, the
// prefix exclusion is expressed as one dynamically-built `NOT LIKE` clause per mapping
// entry (ANDed together) rather than pulling every distinct pool_tag into Go to filter
// there, for consistency with how those methods already build their WHERE/CASE clauses
// from a []PoolTagMapping.
func (d *DB) UnmappedPoolTags(ctx context.Context, mappings []PoolTagMapping) ([]string, error) {
	var args []interface{}
	var clauses []string
	for _, m := range mappings {
		args = append(args, m.MatchPrefix+"%")
		clauses = append(clauses, fmt.Sprintf("pool_tag NOT LIKE $%d", len(args)))
	}

	where := "pool_tag IS NOT NULL"
	if len(clauses) > 0 {
		where += " AND " + strings.Join(clauses, " AND ")
	}

	query := fmt.Sprintf(`
		SELECT DISTINCT pool_tag
		FROM blocks
		WHERE %s
		ORDER BY pool_tag ASC
	`, where)

	rows, err := d.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: unmapped pool tags: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, fmt.Errorf("db: unmapped pool tags: scan: %w", err)
		}
		out = append(out, tag)
	}
	return out, rows.Err()
}

// BlockTimeBucketRow is one row of the height-bucketed block-time report: the median
// inter-block time (seconds) for blocks in [BucketStart, BucketEnd] that have a usable
// predecessor, plus how many blocks in that bucket actually contributed a sample.
// MedianSeconds is nil if the bucket had zero usable samples (e.g. every block in it hit
// a predecessor gap - see BlockTimeDeltaBuckets).
type BlockTimeBucketRow struct {
	BucketStart   uint64
	BucketEnd     uint64
	MedianSeconds *float64
	SampleCount   int64
}

// blockTimeDeltasCTE computes, for each block in [fromHeight, toHeight], its time delta
// (seconds) from its immediate height-1 predecessor via a self-LEFT JOIN. delta_seconds
// is NULL whenever that predecessor row doesn't exist (an indexing gap) so it's simply
// excluded by every aggregate below (Postgres aggregates ignore NULL input) rather than
// poisoning the average/median/etc. or crashing. This is shared textually (not as a Go
// helper - Postgres doesn't let us parameterize a CTE across two separate queries) by
// both BlockTimeDeltaBuckets and BlockTimeSummary; keep them in sync if this changes.
// Takes fromHeight/toHeight as $1/$2; callers needing an additional bucketSize
// parameter (BlockTimeDeltaBuckets) bind it as $3 in their own SELECT.
const blockTimeDeltasCTE = `
	WITH deltas AS (
		SELECT
			b.height,
			b.timestamp - p.timestamp AS delta_seconds
		FROM blocks b
		LEFT JOIN blocks p ON p.height = b.height - 1
		WHERE b.height BETWEEN $1 AND $2
	)
`

// BlockTimeDeltaBuckets groups blocks in [fromHeight, toHeight] (inclusive) into the same
// height buckets as AlgoBucketCounts, and within each bucket computes the MEDIAN
// inter-block time (via Postgres' PERCENTILE_CONT(0.5), computed server-side) rather than
// the mean. Block times have a long right tail (network blips, indexer catch-up gaps,
// occasional multi-minute stretches) that would otherwise skew a mean upward and make the
// chart look noisier than the typical block cadence actually is - median is more robust to
// those outliers and is what this chart plots. (See BlockTimeSummary for the full
// mean/median/stddev/max breakdown used by the stat-card panel, which surfaces the mean
// too so the skew itself is visible to the reader.) bucketSize must be > 0.
func (d *DB) BlockTimeDeltaBuckets(ctx context.Context, bucketSize uint64, fromHeight, toHeight uint64) ([]BlockTimeBucketRow, error) {
	if bucketSize == 0 {
		return nil, fmt.Errorf("db: block time delta buckets: bucket size must be > 0")
	}

	rows, err := d.Pool.Query(ctx, blockTimeDeltasCTE+`
		SELECT
			(height / $3) * $3 AS bucket_start,
			PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY delta_seconds) AS median_seconds,
			COUNT(delta_seconds) AS sample_count
		FROM deltas
		GROUP BY bucket_start
		ORDER BY bucket_start ASC
	`, fromHeight, toHeight, bucketSize)
	if err != nil {
		return nil, fmt.Errorf("db: block time delta buckets: %w", err)
	}
	defer rows.Close()

	var out []BlockTimeBucketRow
	for rows.Next() {
		var r BlockTimeBucketRow
		if err := rows.Scan(&r.BucketStart, &r.MedianSeconds, &r.SampleCount); err != nil {
			return nil, fmt.Errorf("db: block time delta buckets: scan: %w", err)
		}
		r.BucketEnd = r.BucketStart + bucketSize - 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// BlockTimeSummaryRow holds mean/median/stddev/max inter-block-time (seconds) over a
// queried height range, plus how many blocks actually contributed a sample (blocks
// whose predecessor row was missing - an indexing gap - are excluded, see
// blockTimeDeltasCTE). All of Mean/Median/StdDev/Max are nil when SampleCount is 0.
type BlockTimeSummaryRow struct {
	Mean        *float64
	Median      *float64
	StdDev      *float64
	Max         *int64
	SampleCount int64
}

// BlockTimeSummary computes summary statistics (mean, median, sample standard deviation,
// max) of inter-block time over blocks in [fromHeight, toHeight] (inclusive), for the
// plain-HTML stat-card panel next to the block-time chart. Like BlockTimeDeltaBuckets,
// this reuses blockTimeDeltasCTE so a block with a missing predecessor is excluded from
// every statistic rather than causing a NULL-propagation crash.
func (d *DB) BlockTimeSummary(ctx context.Context, fromHeight, toHeight uint64) (BlockTimeSummaryRow, error) {
	var r BlockTimeSummaryRow
	err := d.Pool.QueryRow(ctx, blockTimeDeltasCTE+`
		SELECT
			AVG(delta_seconds),
			PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY delta_seconds),
			STDDEV_SAMP(delta_seconds),
			MAX(delta_seconds),
			COUNT(delta_seconds)
		FROM deltas
	`, fromHeight, toHeight).Scan(&r.Mean, &r.Median, &r.StdDev, &r.Max, &r.SampleCount)
	if err != nil {
		return BlockTimeSummaryRow{}, fmt.Errorf("db: block time summary: %w", err)
	}
	return r, nil
}

// DifficultyBucketRow is one row of the height-bucketed average-difficulty report: for
// blocks in [BucketStart, BucketEnd], the mean `difficulty` column value computed
// separately per pow_algo (RXM/RXT/C29/SHA3X), mirroring AlgoBucketRow's fixed-column
// per-algo shape rather than blending all four algos' difficulties into one meaningless
// average. RXM/RXT/C29/SHA3X difficulties differ by orders of magnitude and measure
// entirely different, non-comparable proof-of-work spaces, so a single blended average
// across all of them has no real-world interpretation - see AlgoBucketRow's doc comment
// for the same per-algo-column precedent this mirrors.
//
// Difficulty is used directly as a hashrate proxy (difficulty is proportional to
// expected hashes-per-block; hashrate = difficulty / block_time, but this repo's blocks
// table - and the upstream go-tari-grpc-lib v3.2.0 BlockHeader proto it's decomposed
// from - carries no per-network target-block-time field, so a literal hashrate figure
// isn't derivable from indexed data alone). A per-algo difficulty-over-height chart is
// the intended stand-in; see internal/server/analysis.go's difficulty handler.
//
// Each field is *float64, not float64, and nil (not a pointer to 0.0) when that algo had
// zero blocks in the bucket: AVG() over zero rows is SQL NULL, and NULL here means
// "no data for this algo in this bucket", which is semantically distinct from "the
// average difficulty for this algo in this bucket is exactly 0.0" (never a valid real
// value). Coercing that NULL to 0.0 would misrepresent absence-of-data as a real
// measurement, so it must stay a nil pointer all the way through this layer (see
// BlockTimeBucketRow.MedianSeconds for the same *float64-for-NULL convention already
// used elsewhere in this file).
type DifficultyBucketRow struct {
	BucketStart uint64
	BucketEnd   uint64
	RXM         *float64
	RXT         *float64
	C29         *float64
	SHA3X       *float64
}

// DifficultyBucketAvg groups blocks in [fromHeight, toHeight] (inclusive) into the same
// height buckets as AlgoBucketCounts and, per bucket, averages `difficulty` separately
// for each of the four known pow_algo values via FILTER-clause conditional AVG (mirrors
// AlgoBucketCounts' FILTER pattern exactly, substituting AVG(difficulty) for COUNT(*)),
// aggregated entirely in Postgres (never pulling raw rows into Go). A bucket/algo
// combination with zero blocks yields SQL NULL from AVG(), scanned into that field's
// *float64 as a nil pointer - see DifficultyBucketRow's doc comment for why that must
// never be coerced to 0.0. bucketSize must be > 0.
func (d *DB) DifficultyBucketAvg(ctx context.Context, bucketSize uint64, fromHeight, toHeight uint64) ([]DifficultyBucketRow, error) {
	if bucketSize == 0 {
		return nil, fmt.Errorf("db: difficulty bucket avg: bucket size must be > 0")
	}

	rows, err := d.Pool.Query(ctx, `
		SELECT
			(height / $1) * $1 AS bucket_start,
			AVG(difficulty) FILTER (WHERE pow_algo = 'RXM')   AS rxm,
			AVG(difficulty) FILTER (WHERE pow_algo = 'RXT')   AS rxt,
			AVG(difficulty) FILTER (WHERE pow_algo = 'C29')   AS c29,
			AVG(difficulty) FILTER (WHERE pow_algo = 'SHA3X') AS sha3x
		FROM blocks
		WHERE height BETWEEN $2 AND $3
		GROUP BY bucket_start
		ORDER BY bucket_start ASC
	`, bucketSize, fromHeight, toHeight)
	if err != nil {
		return nil, fmt.Errorf("db: difficulty bucket avg: %w", err)
	}
	defer rows.Close()

	var out []DifficultyBucketRow
	for rows.Next() {
		var r DifficultyBucketRow
		if err := rows.Scan(&r.BucketStart, &r.RXM, &r.RXT, &r.C29, &r.SHA3X); err != nil {
			return nil, fmt.Errorf("db: difficulty bucket avg: scan: %w", err)
		}
		r.BucketEnd = r.BucketStart + bucketSize - 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// MempoolSnapshot is the row shape for the `mempool_snapshots` table: one row per poll
// tick of the base node's live aggregate mempool stats (see
// tari_generated.MempoolStatsResponse / internal/nodeclient.Client.GetMempoolStats),
// taken by cmd/mempool-poller's Poller (internal/mempoolpoller.Poller). This is an
// aggregate-only snapshot - one row summarizes the *entire* mempool at SnapshotTime -
// not one row per pending transaction; see migrations/0006_mempool_snapshots.up.sql for
// why per-transaction mempool detail is never persisted anywhere in this schema.
type MempoolSnapshot struct {
	ID                int64
	SnapshotTime      time.Time
	UnconfirmedTxs    int32
	ReorgTxs          int32
	UnconfirmedWeight int64
}

// InsertMempoolSnapshot inserts one new mempool_snapshots row. Unlike UpsertBlock, this
// is a plain INSERT with no ON CONFLICT/upsert-on-key: every poll tick is a distinct
// point-in-time observation with no natural key to upsert against, and duplicate or
// out-of-order snapshot_time values across ticks are expected and fine (analysis
// queries group by time range, not by a unique identity - see
// migrations/0006_mempool_snapshots.up.sql).
func (d *DB) InsertMempoolSnapshot(ctx context.Context, s MempoolSnapshot) error {
	_, err := d.Pool.Exec(ctx, `
		INSERT INTO mempool_snapshots (snapshot_time, unconfirmed_txs, reorg_txs, unconfirmed_weight)
		VALUES ($1, $2, $3, $4)
	`, s.SnapshotTime, s.UnconfirmedTxs, s.ReorgTxs, s.UnconfirmedWeight)
	if err != nil {
		return fmt.Errorf("db: insert mempool snapshot: %w", err)
	}
	return nil
}

// ListMempoolSnapshots returns mempool_snapshots rows ordered by snapshot_time
// ascending, optionally bounded to snapshot_time in [from, to] (inclusive on both
// ends). Either bound may be nil to leave that side unbounded - from == nil && to ==
// nil returns every stored snapshot.
func (d *DB) ListMempoolSnapshots(ctx context.Context, from, to *time.Time) ([]MempoolSnapshot, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT id, snapshot_time, unconfirmed_txs, reorg_txs, unconfirmed_weight
		FROM mempool_snapshots
		WHERE ($1::timestamptz IS NULL OR snapshot_time >= $1)
		  AND ($2::timestamptz IS NULL OR snapshot_time <= $2)
		ORDER BY snapshot_time ASC
	`, from, to)
	if err != nil {
		return nil, fmt.Errorf("db: list mempool snapshots: %w", err)
	}
	defer rows.Close()

	var out []MempoolSnapshot
	for rows.Next() {
		var s MempoolSnapshot
		if err := rows.Scan(&s.ID, &s.SnapshotTime, &s.UnconfirmedTxs, &s.ReorgTxs, &s.UnconfirmedWeight); err != nil {
			return nil, fmt.Errorf("db: list mempool snapshots: scan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// PoolCountRow is one row of the "how many of the most recent N blocks were mined by
// each pool" breakdown - see RecentBlocksStats. PoolTag is either a real
// (mapping-folded, see PoolTagMapping) pool_tag value or the literal string "unknown"
// for a NULL pool_tag, matching PoolShareBucketRow's convention. Unlike
// PoolShareBucketCounts, there is no topN/"other" folding here - the last-100-blocks
// sample is small enough that every distinct (mapped) pool tag present is returned as
// its own row.
type PoolCountRow struct {
	PoolTag string
	Count   int64
}

// AlgoCountRow is one row of the "how many of the most recent N blocks were mined on
// each pow-algo" breakdown - see RecentBlocksStats. AvgDifficulty is the mean
// `difficulty` column value over just this algo's blocks within that same sample.
type AlgoCountRow struct {
	Algo          string
	Count         int64
	AvgDifficulty float64
}

// RecentBlocksStats is the front-page "at a glance" snapshot over the most recently
// indexed limit blocks (by height descending): a pool-tag breakdown and a pow-algo
// breakdown (the latter including a per-algo average difficulty, see AlgoCountRow).
// SampleCount is how many blocks actually contributed (== limit once at least that
// many blocks are indexed, less before then, 0 on an empty table).
type RecentBlocksStats struct {
	Pools       []PoolCountRow
	Algos       []AlgoCountRow
	SampleCount int64
}

// RecentBlocksStats computes the front-page pool/algo breakdown over the most recently
// indexed `limit` blocks (ORDER BY height DESC LIMIT limit), in a single round trip to
// Postgres: one CTE selects the candidate blocks (with pool_tag folded through
// mappings, the same LIKE-prefix mechanism PoolShareBucketCounts uses - see
// PoolTagMapping's doc comment), three more CTEs aggregate that same sample three
// different ways (per-pool count, per-algo count + per-algo avg difficulty, and the
// overall sample size), and the results are UNION ALL'd together with a `kind`
// discriminator column so the three different-shaped aggregates can travel back as one
// result set rather than three separate queries. All aggregation happens
// Postgres-side; raw block rows are never pulled into Go. limit must be > 0.
func (d *DB) RecentBlocksStats(ctx context.Context, limit int, mappings []PoolTagMapping) (RecentBlocksStats, error) {
	if limit <= 0 {
		return RecentBlocksStats{}, fmt.Errorf("db: recent blocks stats: limit must be > 0")
	}

	// args starts with the single fixed positional param ($1, the LIMIT); each
	// mapping entry then appends its own (LIKE pattern, canonical name) pair as two
	// more params, referenced by position in the CASE expression built below - same
	// pattern as PoolShareBucketCounts.
	args := []interface{}{limit}
	var caseClauses strings.Builder
	for _, m := range mappings {
		args = append(args, m.MatchPrefix+"%", m.CanonicalName)
		patternParam := len(args) - 1
		nameParam := len(args)
		fmt.Fprintf(&caseClauses, "\n				WHEN pool_tag LIKE $%d THEN $%d", patternParam, nameParam)
	}

	query := fmt.Sprintf(`
		WITH recent AS (
			SELECT
				pow_algo,
				difficulty,
				CASE
					WHEN pool_tag IS NULL THEN 'unknown'%s
					ELSE pool_tag
				END AS mapped_pool
			FROM blocks
			ORDER BY height DESC
			LIMIT $1
		),
		pool_counts AS (
			SELECT 'pool' AS kind, mapped_pool AS key, COUNT(*) AS cnt, NULL::float8 AS avg_diff
			FROM recent
			GROUP BY mapped_pool
		),
		algo_counts AS (
			SELECT 'algo' AS kind, pow_algo AS key, COUNT(*) AS cnt, AVG(difficulty) AS avg_diff
			FROM recent
			GROUP BY pow_algo
		),
		sample_size AS (
			SELECT 'sample' AS kind, 'n' AS key, COUNT(*) AS cnt, NULL::float8 AS avg_diff
			FROM recent
		)
		SELECT kind, key, cnt, avg_diff FROM pool_counts
		UNION ALL
		SELECT kind, key, cnt, avg_diff FROM algo_counts
		UNION ALL
		SELECT kind, key, cnt, avg_diff FROM sample_size
		ORDER BY kind, cnt DESC
	`, caseClauses.String())

	rows, err := d.Pool.Query(ctx, query, args...)
	if err != nil {
		return RecentBlocksStats{}, fmt.Errorf("db: recent blocks stats: %w", err)
	}
	defer rows.Close()

	var out RecentBlocksStats
	for rows.Next() {
		var kind, key string
		var cnt int64
		var avgDiff *float64
		if err := rows.Scan(&kind, &key, &cnt, &avgDiff); err != nil {
			return RecentBlocksStats{}, fmt.Errorf("db: recent blocks stats: scan: %w", err)
		}
		switch kind {
		case "pool":
			out.Pools = append(out.Pools, PoolCountRow{PoolTag: key, Count: cnt})
		case "algo":
			var ad float64
			if avgDiff != nil {
				ad = *avgDiff
			}
			out.Algos = append(out.Algos, AlgoCountRow{Algo: key, Count: cnt, AvgDifficulty: ad})
		case "sample":
			out.SampleCount = cnt
		}
	}
	return out, rows.Err()
}
