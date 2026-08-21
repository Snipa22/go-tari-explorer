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
// blocks were mined by each pool_tag. To keep the result (and any legend built from it)
// bounded, only the topN pool tags by total block count over the whole queried range
// are kept as their own series; every other non-null pool_tag is folded into a single
// "other" series, and NULL pool_tag (unattributed blocks) is always its own "unknown"
// series regardless of topN. topN is a caller-supplied cap (the analysis HTTP handlers
// default to 8, chosen as a reasonable legend size for a chart embedded at normal page
// width - see internal/server/analysis.go) rather than hardcoded here, so callers can
// tune it.
//
// As with AlgoBucketCounts, all aggregation (bucketing, top-N ranking, grouping) happens
// Postgres-side via a CTE + GROUP BY - raw block rows are never pulled into Go to
// aggregate here. Results are ordered by bucket_start, then pool_tag, ascending.
// bucketSize must be > 0 and topN must be >= 0 (0 means every non-null pool_tag folds
// into "other").
func (d *DB) PoolShareBucketCounts(ctx context.Context, bucketSize uint64, fromHeight, toHeight uint64, topN int) ([]PoolShareBucketRow, error) {
	if bucketSize == 0 {
		return nil, fmt.Errorf("db: pool share bucket counts: bucket size must be > 0")
	}
	if topN < 0 {
		return nil, fmt.Errorf("db: pool share bucket counts: topN must be >= 0")
	}

	rows, err := d.Pool.Query(ctx, `
		WITH top_pools AS (
			SELECT pool_tag
			FROM blocks
			WHERE height BETWEEN $2 AND $3 AND pool_tag IS NOT NULL
			GROUP BY pool_tag
			ORDER BY COUNT(*) DESC
			LIMIT $4
		),
		classified AS (
			SELECT
				(height / $1) * $1 AS bucket_start,
				CASE
					WHEN pool_tag IS NULL THEN 'unknown'
					WHEN pool_tag IN (SELECT pool_tag FROM top_pools) THEN pool_tag
					ELSE 'other'
				END AS pool_key
			FROM blocks
			WHERE height BETWEEN $2 AND $3
		)
		SELECT bucket_start, pool_key, COUNT(*) AS block_count
		FROM classified
		GROUP BY bucket_start, pool_key
		ORDER BY bucket_start ASC, pool_key ASC
	`, bucketSize, fromHeight, toHeight, topN)
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

// DifficultyBucketRow is one row of the height-bucketed average-difficulty report:
// AvgDifficulty is the mean `difficulty` column value over blocks in
// [BucketStart, BucketEnd]. Difficulty is used directly as a hashrate proxy (difficulty
// is proportional to expected hashes-per-block; hashrate = difficulty / block_time, but
// this repo's blocks table - and the upstream go-tari-grpc-lib v3.2.0 BlockHeader proto
// it's decomposed from - carries no per-network target-block-time field, so a literal
// hashrate figure isn't derivable from indexed data alone). A difficulty-over-height
// chart is the intended stand-in; see internal/server/analysis.go's difficulty handler.
type DifficultyBucketRow struct {
	BucketStart   uint64
	BucketEnd     uint64
	AvgDifficulty float64
}

// DifficultyBucketAvg groups blocks in [fromHeight, toHeight] (inclusive) into the same
// height buckets as AlgoBucketCounts and averages `difficulty` per bucket, aggregated
// entirely in Postgres via GROUP BY (never pulling raw rows into Go). bucketSize must be
// > 0.
func (d *DB) DifficultyBucketAvg(ctx context.Context, bucketSize uint64, fromHeight, toHeight uint64) ([]DifficultyBucketRow, error) {
	if bucketSize == 0 {
		return nil, fmt.Errorf("db: difficulty bucket avg: bucket size must be > 0")
	}

	rows, err := d.Pool.Query(ctx, `
		SELECT
			(height / $1) * $1 AS bucket_start,
			AVG(difficulty) AS avg_difficulty
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
		if err := rows.Scan(&r.BucketStart, &r.AvgDifficulty); err != nil {
			return nil, fmt.Errorf("db: difficulty bucket avg: scan: %w", err)
		}
		r.BucketEnd = r.BucketStart + bucketSize - 1
		out = append(out, r)
	}
	return out, rows.Err()
}
