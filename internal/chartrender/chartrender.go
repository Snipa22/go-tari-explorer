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
	"image/draw"
	"image/png"
	"sort"

	chart "github.com/wcharczuk/go-chart/v2"
	"github.com/wcharczuk/go-chart/v2/drawing"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
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

// distinctXCount returns the number of distinct X values across points. go-chart's
// axis-ranging code divides by (max-x - min-x) internally, so anything below 2 distinct
// X values (0 points, or every point sharing the same X) makes it error/panic with
// "zero x-range delta; there needs to be at least (2) values" - see
// notEnoughDataPNG's doc comment for how that case is handled instead of let through.
func distinctXCount(points []Point) int {
	seen := make(map[float64]struct{}, len(points))
	for _, p := range points {
		seen[p.X] = struct{}{}
	}
	return len(seen)
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

// notEnoughDataMessage is the placeholder text rendered by notEnoughDataPNG. Kept as a
// package-level const so both callers (and tests) reference the exact same wording.
const notEnoughDataMessage = "not enough data for this range - try a smaller bucket_size or wider from/to"

// notEnoughDataPNG renders a plain, hand-drawn placeholder PNG (title + centered
// message, on the same dark background as a real chart) using only stdlib
// image/image-png plus golang.org/x/image's basicfont (already an indirect dependency
// of this module via go-chart's own font handling, so this adds no new third-party
// dependency) - deliberately not a real chart.Chart, since go-chart itself is what
// can't render fewer than 2 distinct X values (see distinctXCount's doc comment).
// This is the graceful fallback StackedAreaChart/LineChart use instead of erroring
// (and therefore 500ing every /analysis/*.png endpoint) when a caller's
// bucket_size/from/to query params collapse the requested height range down to fewer
// than 2 buckets - a normal, user-triggerable input shape on a public read-only page,
// not a programming error.
func notEnoughDataPNG(title string) ([]byte, error) {
	img := image.NewNRGBA(image.Rect(0, 0, DefaultWidth, DefaultHeight))
	bg := color.NRGBA{R: 0x0f, G: 0x0f, B: 0x12, A: 255}
	draw.Draw(img, img.Bounds(), &image.Uniform{C: bg}, image.Point{}, draw.Src)

	canvasInset := 24
	canvasRect := image.Rect(canvasInset, canvasInset, DefaultWidth-canvasInset, DefaultHeight-canvasInset)
	canvasColor := color.NRGBA{R: 0x17, G: 0x17, B: 0x1b, A: 255}
	draw.Draw(img, canvasRect, &image.Uniform{C: canvasColor}, image.Point{}, draw.Src)

	titleColor := color.NRGBA{R: 0xe6, G: 0xe6, B: 0xe6, A: 255}
	messageColor := color.NRGBA{R: 0xcc, G: 0xcc, B: 0xcc, A: 255}

	drawCenteredText(img, title, DefaultHeight/2-30, titleColor)
	drawWrappedCenteredText(img, notEnoughDataMessage, DefaultHeight/2, messageColor, 70)

	buf := bytes.NewBuffer(nil)
	if err := png.Encode(buf, img); err != nil {
		return nil, fmt.Errorf("chartrender: encode placeholder png: %w", err)
	}
	return buf.Bytes(), nil
}

// basicFontFace is the fixed-width bitmap face used for all placeholder-PNG text -
// golang.org/x/image/font/basicfont is already an indirect dependency of this module
// (pulled in transitively via go-chart's font handling), so this adds no new
// third-party dependency, just a direct import of something already in go.sum.
var basicFontFace = basicfont.Face7x13

// textWidth returns the pixel width basicFontFace renders s at.
func textWidth(s string) int {
	return len(s) * basicFontFace.Advance
}

// drawCenteredText draws s horizontally centered on img at the given baseline y using
// golang.org/x/image/font's standard font.Drawer.
func drawCenteredText(img draw.Image, s string, y int, c color.Color) {
	x := (DefaultWidth - textWidth(s)) / 2
	if x < 0 {
		x = 0
	}
	d := &font.Drawer{
		Dst:  img,
		Src:  &image.Uniform{C: c},
		Face: basicFontFace,
		Dot:  fixed.P(x, y),
	}
	d.DrawString(s)
}

// drawWrappedCenteredText draws s centered at y, word-wrapping onto multiple centered
// lines no wider than maxWidth characters each, one line height (13px) apart.
func drawWrappedCenteredText(img draw.Image, s string, y int, c color.Color, maxWidth int) {
	lines := wrapText(s, maxWidth)
	const lineHeight = 16
	start := y - (len(lines)-1)*lineHeight/2
	for i, line := range lines {
		drawCenteredText(img, line, start+i*lineHeight, c)
	}
}

// wrapText greedily wraps s onto lines of at most maxWidth runes, breaking on spaces.
func wrapText(s string, maxWidth int) []string {
	words := splitWords(s)
	if len(words) == 0 {
		return []string{s}
	}
	var lines []string
	cur := words[0]
	for _, w := range words[1:] {
		if len(cur)+1+len(w) > maxWidth {
			lines = append(lines, cur)
			cur = w
			continue
		}
		cur += " " + w
	}
	lines = append(lines, cur)
	return lines
}

// splitWords is a tiny space-splitter (avoiding a strings import elsewhere in this
// file's minimal placeholder-rendering code path).
func splitWords(s string) []string {
	var words []string
	var cur []byte
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			if len(cur) > 0 {
				words = append(words, string(cur))
				cur = nil
			}
			continue
		}
		cur = append(cur, s[i])
	}
	if len(cur) > 0 {
		words = append(words, string(cur))
	}
	return words
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
//
// If points has fewer than 2 distinct X values (0 or 1 buckets - e.g. a caller's
// bucket_size/from/to query params collapsed the requested height range down that far),
// this renders a "not enough data" placeholder PNG instead of erroring - go-chart itself
// cannot range an axis with fewer than 2 distinct X values, and that's a normal
// user-triggerable input shape on a public page, not a caller bug. See notEnoughDataPNG.
//
// Similarly, if both points and seriesOrder are empty, there is genuinely nothing at all
// to plot (e.g. internal/analysis.PoolShare's height range matched zero DB rows, so both
// the point list and the totals-derived series list came back empty) - this is the same
// "not enough data" shape as above, not a caller forgetting to pass seriesOrder, so it
// also renders the placeholder rather than erroring. An empty seriesOrder alongside
// non-empty points is still treated as a caller bug (see below) since real point data
// with no series names to draw from it means the caller forgot to specify which series
// to render, not that there's no data.
func StackedAreaChart(points []Point, seriesOrder []string, title, xLabel, yLabel string) ([]byte, error) {
	if len(points) == 0 && len(seriesOrder) == 0 {
		return notEnoughDataPNG(title)
	}
	if len(seriesOrder) == 0 {
		return nil, fmt.Errorf("chartrender: stacked area chart: no series")
	}
	if distinctXCount(points) < 2 {
		return notEnoughDataPNG(title)
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
//
// If points has fewer than 2 distinct X values (0 or 1 buckets), this renders a "not
// enough data" placeholder PNG instead of erroring - see StackedAreaChart's doc comment
// above and notEnoughDataPNG for why.
//
// Likewise, if both points and seriesOrder are empty (genuinely nothing at all to plot,
// as opposed to real point data with no series names - see StackedAreaChart's doc
// comment above for the distinction), this also renders the placeholder rather than
// erroring.
func LineChart(points []Point, seriesOrder []string, title, xLabel, yLabel string) ([]byte, error) {
	if len(points) == 0 && len(seriesOrder) == 0 {
		return notEnoughDataPNG(title)
	}
	if len(seriesOrder) == 0 {
		return nil, fmt.Errorf("chartrender: line chart: no series")
	}
	if distinctXCount(points) < 2 {
		return notEnoughDataPNG(title)
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
