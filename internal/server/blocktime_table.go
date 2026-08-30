// Data-table shaping for the /analysis/block-time view specifically: unlike the other
// 4 analysis views (see analysis_table.go's newAnalysisTableView, used unchanged by
// those), the block-time table needs a per-bucket mean/median/stddev/max breakdown
// rather than a single chart-series value per bucket, so it's built from the raw
// []db.BlockTimeBucketRow (fetched by a second, explicit db.BlockTimeDeltaBuckets call
// - see handleAnalysisBlockTime) instead of from chartrender.Points. It still produces
// the same *analysisTableView shape those other views use, so
// templates/analysis_view.html's {{if .Table}} block renders it with no template
// changes needed.
package server

import (
	"strconv"

	"github.com/Snipa22/go-tari-explorer/internal/db"
)

// formatMaxSecondsPtr renders a nullable max-seconds value as a plain integer string,
// "n/a" when nil - mirroring newBlockTimeSummaryView's inline maxStr logic for the
// page-wide stat-card panel.
func formatMaxSecondsPtr(v *int64) string {
	if v == nil {
		return "n/a"
	}
	return strconv.FormatInt(*v, 10)
}

// newBlockTimeBucketTableView shapes []db.BlockTimeBucketRow (the per-bucket
// mean/median/stddev/max/sample-count rows from db.BlockTimeDeltaBuckets) into an
// *analysisTableView for the block-time page's data table, additive to (not a
// replacement for) the existing page-wide db.BlockTimeSummaryRow stat-card panel built
// by newBlockTimeSummaryView. Never panics on an empty rows slice - it just produces a
// header-only table with zero data rows, matching newAnalysisTableView's own
// empty-input behavior.
func newBlockTimeBucketTableView(rows []db.BlockTimeBucketRow) *analysisTableView {
	columns := []string{"Bucket", "Mean (s)", "Median (s)", "StdDev (s)", "Max (s)", "Samples"}

	out := make([]analysisTableRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, analysisTableRow{
			Height: strconv.FormatUint(r.BucketStart, 10),
			Values: []string{
				formatSecondsPtr(r.MeanSeconds),
				formatSecondsPtr(r.MedianSeconds),
				formatSecondsPtr(r.StdDevSeconds),
				formatMaxSecondsPtr(r.MaxSeconds),
				strconv.FormatInt(r.SampleCount, 10),
			},
		})
	}

	return &analysisTableView{Columns: columns, Rows: out}
}
