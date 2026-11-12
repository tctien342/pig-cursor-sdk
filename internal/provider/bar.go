package provider

import (
	"math"
	"strings"
)

// The plan bar: a compact rendering of "how much of the allowance is gone" that
// reads at a glance in a terminal line.

// BarFilled and BarEmpty are the two cells of the bar. They are block glyphs of
// the same size, so a bar of n cells is exactly n cells wide regardless of fill.
const (
	BarFilled = "▰" // U+25F0, same advance width as the empty cell
	BarEmpty  = "▱" // U+25F1
)

// PlanBar renders a usage bar `cells` wide for a percentage.
//
// reveal is the animation phase: the number of cells the bar is allowed to show
// as filled, so a freshly read plan fills in from empty instead of appearing
// fully drawn. reveal < 0 means "show everything" (the settled state).
//
// Percentages outside 0..100 are clamped: Cursor reports values above 100 when
// granted bonus usage is in play, and a bar that overflows its own width would
// wrap and look broken.
func PlanBar(percent float64, cells, reveal int) string {
	if cells <= 0 {
		return ""
	}
	if math.IsNaN(percent) || percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	filled := int(math.Round(percent / 100 * float64(cells)))
	if filled < 0 {
		filled = 0
	}
	if filled > cells {
		filled = cells
	}
	if reveal >= 0 && filled > reveal {
		filled = reveal
	}
	var b strings.Builder
	b.Grow(cells * len(BarFilled))
	for i := 0; i < cells; i++ {
		if i < filled {
			b.WriteString(BarFilled)
		} else {
			b.WriteString(BarEmpty)
		}
	}
	return b.String()
}

// BarCells returns how many cells a bar of the given terminal width may use, so
// the row never wraps on a narrow terminal.
func BarCells(width int) int {
	switch {
	case width >= 110:
		return 20
	case width >= 88:
		return 14
	case width >= 66:
		return 10
	default:
		return 6
	}
}

// PercentChanged reports whether two percentages are different enough to be worth
// redrawing for.
func PercentChanged(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d >= 0.05
}
