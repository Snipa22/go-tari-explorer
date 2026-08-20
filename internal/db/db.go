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

// Block is the row shape for the `blocks` table.
type Block struct {
	Height      uint64
	Hash        string
	PrevHash    string
	Timestamp   int64
	PowAlgo     string
	Difficulty  int64
	KernelCount int32
	OutputCount int32
	PoolTag     *string // nil == unattributed
}

// UpsertBlock inserts or updates a single block row, keyed on height. Used by the
// indexer for both backfill and tip-follow modes.
func (d *DB) UpsertBlock(ctx context.Context, b Block) error {
	_, err := d.Pool.Exec(ctx, `
		INSERT INTO blocks (height, hash, prev_hash, "timestamp", pow_algo, difficulty, kernel_count, output_count, pool_tag)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (height) DO UPDATE SET
			hash = EXCLUDED.hash,
			prev_hash = EXCLUDED.prev_hash,
			"timestamp" = EXCLUDED."timestamp",
			pow_algo = EXCLUDED.pow_algo,
			difficulty = EXCLUDED.difficulty,
			kernel_count = EXCLUDED.kernel_count,
			output_count = EXCLUDED.output_count,
			pool_tag = EXCLUDED.pool_tag
	`, b.Height, b.Hash, b.PrevHash, b.Timestamp, b.PowAlgo, b.Difficulty, b.KernelCount, b.OutputCount, b.PoolTag)
	if err != nil {
		return fmt.Errorf("db: upsert block %d: %w", b.Height, err)
	}
	return nil
}

// ListBlocks returns up to limit blocks ordered by height descending, starting strictly
// below beforeHeight (pass a very large value, e.g. math.MaxInt64, for the first page).
// Used to drive the paginated / HTMX "load more" blocks-list page.
func (d *DB) ListBlocks(ctx context.Context, beforeHeight int64, limit int) ([]Block, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT height, hash, prev_hash, "timestamp", pow_algo, difficulty, kernel_count, output_count, pool_tag
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
		if err := rows.Scan(&b.Height, &b.Hash, &b.PrevHash, &b.Timestamp, &b.PowAlgo, &b.Difficulty, &b.KernelCount, &b.OutputCount, &b.PoolTag); err != nil {
			return nil, fmt.Errorf("db: list blocks: scan: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetBlock returns a single block by height, or pgx.ErrNoRows if it doesn't exist.
func (d *DB) GetBlock(ctx context.Context, height uint64) (Block, error) {
	var b Block
	err := d.Pool.QueryRow(ctx, `
		SELECT height, hash, prev_hash, "timestamp", pow_algo, difficulty, kernel_count, output_count, pool_tag
		FROM blocks
		WHERE height = $1
	`, height).Scan(&b.Height, &b.Hash, &b.PrevHash, &b.Timestamp, &b.PowAlgo, &b.Difficulty, &b.KernelCount, &b.OutputCount, &b.PoolTag)
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
