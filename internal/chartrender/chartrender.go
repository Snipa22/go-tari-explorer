// Package chartrender is a thin, decoupled wrapper around
// github.com/wcharczuk/go-chart/v2 (a pure-Go, actively-used SVG/PNG chart renderer with
// no cgo/system-font dependency, which is why it was picked over e.g. gonum/plot for this
// server-side-only embedded-<img> use case) that renders PNG chart images from a simple,
// db-agnostic input shape. It deliberately knows nothing about internal/db's row types -
// callers (internal/server's analysis handlers, via internal/analysis) are responsible
// for converting query results into []Point before calling in here, which keeps this
// package unit-testable with plain literal data and reusable if the input ever comes
// from somewhere other than Postgres.
package chartrender

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"sort"

	chart "github.com/wcharczuk/go-chart/v2"
	"github.com/wcharczuk/go-chart/v2/drawing"
)

// Point is one X-axis sample (typically a bucket-start block height) with a value per
// named data series (e.g. one entry per pow-algo or pool tag). A series name absent from
// a given Point's Series map is treated as a real zero for that X value (see
// seriesValues below) rather than "no data" - callers should not omit a series from a
// Point's map to mean "unknown", only to mean "zero blocks in this bucket".
type Point struct {
	X      float64
	Series map[string]float64
}

// DefaultWidth/DefaultHeight are the PNG dimensions used when a chart function isn't
// given an explicit size. Sized for a comfortable embedded <img> width on the existing
// dark-themed layout without needing the page to scroll horizontally.
const (
	DefaultWidth  = 960
	DefaultHeight = 400
)

// palette is a small fixed, readable-on-dark-background color rotation used for both
// StackedAreaChart and LineChart, assigned to series in the caller-supplied seriesOrder
// so a given series name gets a stable color across repeated renders (e.g. re-rendering
// the algo-distribution chart with a different bucket_size shouldn't flip RXM from green
// to blue). If seriesOrder has more entries than the palette, colors repeat.
var palette = []drawing.Color{
	{R: 0x7c, G: 0xc7, B: 0xff, A: 255}, // blue   (matches layout.html's link color)
	{R: 0x7f, G: 0xff, B: 0xa0, A: 255}, // green  (matches layout.html's pool-own color)
	{R: 0xff, G: 0xb0, B: 0x80, A: 255}, // orange
	{R: 0xff, G: 0x80, B: 0x80, A: 255}, // red
	{R: 0xd0, G: 0x80, B: 0xff, A: 255}, // purple
	{R: 0xff, G: 0xe6, B: 0x80, A: 255}, // yellow
	{R: 0x80, G: 0xff, B: 0xe6, A: 255}, // teal
	{R: 0xbb, G: 0xbb, B: 0xbb, A: 255}, // grey (good default for "other"/"unknown")
	{R: 0xff, G: 0x80, B: 0xd0, A: 255}, // pink
	{R: 0x80, G: 0x80, B: 0xff, A: 255}, // indigo
}

func colorFor(seriesIndex int) drawing.Color {
	return palette[seriesIndex%len(palette)]
}

// seriesValues extracts one series' Y-values across points, in point order, treating a
// missing map entry as 0.
func seriesValues(points []Point, name string) []float64 {
	out := make([]float64, len(points))
	for i, p := range points {
		out[i] = p.Series[name]
	}
	return out
}

func xValues(points []Point) []float64 {
	out := make([]float64, len(points))
	for i, p := range points {
		out[i] = p.X
	}
	return out
}

// renderPNG runs c.Render with the PNG renderer into a byte slice.
func renderPNG(c chart.Chart) ([]byte, error) {
	buf := bytes.NewBuffer(nil)
	if err := c.Render(chart.PNG, buf); err != nil {
		return nil, fmt.Errorf("chartrender: render: %w", err)
	}
	return buf.Bytes(), nil
}

// baseChart builds a chart.Chart with the title/axis labels and dark-theme-appropriate
// styling (light text/gridlines on a dark canvas, matching layout.html) common to both
// StackedAreaChart and LineChart, with no Series set yet.
func baseChart(title, xLabel, yLabel string) chart.Chart {
	axisStyle := chart.Style{
		StrokeColor: drawing.Color{R: 0x66, G: 0x66, B: 0x66, A: 255},
		FontColor:   drawing.Color{R: 0xcc, G: 0xcc, B: 0xcc, A: 255},
		StrokeWidth: 1,
	}
	return chart.Chart{
		Title: title,
		TitleStyle: chart.Style{
			FontColor: drawing.Color{R: 0xe6, G: 0xe6, B: 0xe6, A: 255},
		},
		Width:  DefaultWidth,
		Height: DefaultHeight,
		Background: chart.Style{
			FillColor: drawing.Color{R: 0x0f, G: 0x0f, B: 0x12, A: 255},
		},
		Canvas: chart.Style{
			FillColor: drawing.Color{R: 0x17, G: 0x17, B: 0x1b, A: 255},
		},
		XAxis: chart.XAxis{
			Name:      xLabel,
			Style:     axisStyle,
			NameStyle: axisStyle,
		},
		YAxis: chart.YAxis{
			Name:      yLabel,
			Style:     axisStyle,
			NameStyle: axisStyle,
		},
	}
}

// legendStyle returns a text style for chart.Legend matching the dark theme.
func legendStyle() chart.Style {
	return chart.Style{
		FontColor: drawing.Color{R: 0xe6, G: 0xe6, B: 0xe6, A: 255},
		FillColor: drawing.Color{R: 0x17, G: 0x17, B: 0x1b, A: 200},
	}
}

// StackedAreaChart renders points as a stacked-area PNG: for each X, the named series in
// seriesOrder are drawn as cumulative bands stacked bottom-to-top in that order (so
// seriesOrder[0] is the bottommost band), preserving each series' absolute value rather
// than normalizing to a percentage - this is the "block count per algo/pool" shape the
// algo-distribution and pool-share analysis pages need, as opposed to go-chart's built-in
// StackedBarChart, which always normalizes each bar to 100% and would silently discard
// the fact that some buckets may have fewer total blocks (partial ranges, indexing gaps)
// than others.
//
// Implementation note: go-chart has no native "stacked area" series type, so this
// computes a cumulative sum per X across seriesOrder and renders one filled
// chart.ContinuousSeries per cumulative level, drawn from the largest (total) cumulative
// sum down to the smallest (first series alone). Because later-added series draw on top
// in go-chart, drawing in decreasing-cumulative order paints the "total" band first, then
// progressively smaller filled bands on top of it - the net visual effect is a correctly
// stacked area chart where only the top-most band's true color remains visible between
// each pair of cumulative lines. seriesOrder controls both stack order and legend/color
// order; a series name not present in a given Point's map is treated as 0 (see Point).
func StackedAreaChart(points []Point, seriesOrder []string, title, xLabel, yLabel string) ([]byte, error) {
	if len(points) == 0 {
		return nil, fmt.Errorf("chartrender: stacked area chart: no points")
	}
	if len(seriesOrder) == 0 {
		return nil, fmt.Errorf("chartrender: stacked area chart: no series")
	}

	sorted := make([]Point, len(points))
	copy(sorted, points)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].X < sorted[j].X })
	xs := xValues(sorted)

	// cumulative[k] holds, per X, the running total of seriesOrder[0..k] inclusive.
	cumulative := make([][]float64, len(seriesOrder))
	running := make([]float64, len(sorted))
	for k, name := range seriesOrder {
		vals := seriesValues(sorted, name)
		level := make([]float64, len(sorted))
		for i := range sorted {
			running[i] += vals[i]
			level[i] = running[i]
		}
		cumulative[k] = level
	}

	c := baseChart(title, xLabel, yLabel)
	// Draw from the topmost cumulative level (largest, index len-1) down to the
	// bottommost (seriesOrder[0] alone, index 0) so each later series paints over and
	// crops the one before it, per the doc comment above.
	for k := len(seriesOrder) - 1; k >= 0; k-- {
		color := colorFor(k)
		c.Series = append(c.Series, chart.ContinuousSeries{
			Name:    seriesOrder[k],
			XValues: xs,
			YValues: cumulative[k],
			Style: chart.Style{
				StrokeColor: color,
				FillColor:   color.WithAlpha(200),
				StrokeWidth: 1,
			},
		})
	}
	c.Elements = []chart.Renderable{chart.Legend(&c, legendStyle())}

	return renderPNG(c)
}

// LineChart renders points as a plain (non-stacked) multi-series line chart PNG, one line
// per name in seriesOrder. Used for the block-time and difficulty analysis pages, which
// each only need a single series, but this supports N for reuse/testability.
func LineChart(points []Point, seriesOrder []string, title, xLabel, yLabel string) ([]byte, error) {
	if len(points) == 0 {
		return nil, fmt.Errorf("chartrender: line chart: no points")
	}
	if len(seriesOrder) == 0 {
		return nil, fmt.Errorf("chartrender: line chart: no series")
	}

	sorted := make([]Point, len(points))
	copy(sorted, points)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].X < sorted[j].X })
	xs := xValues(sorted)

	c := baseChart(title, xLabel, yLabel)
	for k, name := range seriesOrder {
		color := colorFor(k)
		c.Series = append(c.Series, chart.ContinuousSeries{
			Name:    name,
			XValues: xs,
			YValues: seriesValues(sorted, name),
			Style: chart.Style{
				StrokeColor: color,
				StrokeWidth: 2,
			},
		})
	}
	if len(seriesOrder) > 1 {
		c.Elements = []chart.Renderable{chart.Legend(&c, legendStyle())}
	}

	return renderPNG(c)
}

// PlaceholderPNG renders a minimal blank placeholder PNG - matching the dark theme's
// canvas fill color, sized DefaultWidth x DefaultHeight so it drops into the same
// <img> slot a real chart would occupy without a layout jump once there's enough data
// to plot one. Deliberately not a go-chart chart.Chart (that type requires at least
// one non-empty Series, see StackedAreaChart/LineChart's own "no points"/"no series"
// guards above): this is for the case call sites hit *before* they'd even have a
// series to hand it - e.g. 0 or 1 mempool_snapshots rows, not enough to draw a
// meaningful line - so a companion PNG route can still return a valid image (never a
// 500) rather than needing its own chart.Chart-shaped workaround for "not enough
// points to satisfy those guards".
func PlaceholderPNG() ([]byte, error) {
	bg := color.RGBA{R: 0x17, G: 0x17, B: 0x1b, A: 255}
	img := image.NewRGBA(image.Rect(0, 0, DefaultWidth, DefaultHeight))
	for y := 0; y < DefaultHeight; y++ {
		for x := 0; x < DefaultWidth; x++ {
			img.Set(x, y, bg)
		}
	}
	buf := bytes.NewBuffer(nil)
	if err := png.Encode(buf, img); err != nil {
		return nil, fmt.Errorf("chartrender: placeholder: encode: %w", err)
	}
	return buf.Bytes(), nil
}
