// Data-table shaping for the historical-analysis HTML views: turns the same
// []chartrender.Point + series-name order already fetched for the PNG charts into a
// template-friendly row structure, so each analysis page can render a plain HTML table
// below its chart without a second query.
package server

import (
	"math"
	"strconv"

	"github.com/Snipa22/go-tari-explorer/internal/chartrender"
)

// analysisTableView is what templates/analysis_view.html renders as the data table.
type analysisTableView struct {
	Columns []string // header row: "Height" + series names in order
	Rows    []analysisTableRow
}

// analysisTableRow is a single bucket's worth of formatted values, in Columns order.
type analysisTableRow struct {
	Height string   // bucket start height, ALWAYS formatted as a plain integer
	Values []string // one formatted string per series, same order as Columns[1:]
}

// formatBucketHeight renders a bucket-start height (Point.X) as a plain integer
// string, no decimal point, ever - rounding to the nearest integer first so a
// pathological non-integral float (float noise, etc.) never panics or leaks a
// fractional part into the table.
func formatBucketHeight(x float64) string {
	return strconv.FormatInt(int64(math.Round(x)), 10)
}

// formatSeriesValue renders a single series value for one table cell. When isCount is
// true the value is a block count and is rendered as a plain rounded integer (no
// decimal point, matching formatBucketHeight's rounding behavior). Otherwise the value
// is rendered with 2 decimal places, matching the AvgDifficultyDisplay/formatSecondsPtr
// convention already used elsewhere in this package.
func formatSeriesValue(v float64, isCount bool) string {
	if isCount {
		return strconv.FormatInt(int64(math.Round(v)), 10)
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// newAnalysisTableView shapes points/order (the same values already returned by every
// internal/analysis chart-data function) into an analysisTableView. isCount selects
// the per-series formatting rule (block counts render as plain integers, everything
// else - difficulty, block-time seconds - renders with 2 decimal places) uniformly for
// every series in order; callers with genuinely mixed count/non-count series don't
// exist among the 5 current analysis views, so a single bool per call site is
// sufficient. Never panics on an empty points or order slice - it just produces a
// table with a header row (or an empty Columns/Rows pair if order is also empty) and
// zero data rows.
func newAnalysisTableView(points []chartrender.Point, order []string, isCount bool) *analysisTableView {
	columns := make([]string, 0, len(order)+1)
	columns = append(columns, "Height")
	columns = append(columns, order...)

	rows := make([]analysisTableRow, 0, len(points))
	for _, pt := range points {
		values := make([]string, len(order))
		for i, name := range order {
			values[i] = formatSeriesValue(pt.Series[name], isCount)
		}
		rows = append(rows, analysisTableRow{
			Height: formatBucketHeight(pt.X),
			Values: values,
		})
	}

	return &analysisTableView{Columns: columns, Rows: rows}
}
