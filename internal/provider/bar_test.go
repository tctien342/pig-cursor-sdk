package provider

import (
	"strings"
	"testing"
)

func TestPlanBarFillMatchesPercent(t *testing.T) {
	for _, c := range []struct {
		pct    float64
		cells  int
		filled int
	}{{0, 10, 0}, {5, 10, 1}, {7.08, 20, 1}, {50, 10, 5}, {100, 10, 10}, {140, 10, 10}, {-3, 10, 0}} {
		got := PlanBar(c.pct, c.cells, -1)
		if n := strings.Count(got, BarFilled); n != c.filled {
			t.Errorf("PlanBar(%v, %d) = %q, want %d filled cells", c.pct, c.cells, got, c.filled)
		}
		if r := []rune(got); len(r) != c.cells {
			t.Errorf("PlanBar(%v, %d) is %d cells wide, want %d — a wider bar wraps the row", c.pct, c.cells, len(r), c.cells)
		}
	}
}

// The fill animation must reveal progressively and never overshoot the real
// value, so the settled bar is the honest one.
func TestPlanBarRevealIsCappedAtTheRealFill(t *testing.T) {
	full := strings.Count(PlanBar(80, 10, -1), BarFilled)
	for reveal := 0; reveal <= 12; reveal++ {
		n := strings.Count(PlanBar(80, 10, reveal), BarFilled)
		if n > full {
			t.Fatalf("reveal %d showed %d filled cells, more than the real %d", reveal, n, full)
		}
		if reveal <= full && n != reveal {
			t.Fatalf("reveal %d showed %d cells, want %d", reveal, n, reveal)
		}
	}
}

func TestBarCellsFitsNarrowTerminals(t *testing.T) {
	for width, want := range map[int]int{40: 6, 70: 10, 90: 14, 120: 20} {
		if got := BarCells(width); got != want {
			t.Errorf("BarCells(%d) = %d, want %d", width, got, want)
		}
	}
	if got := PlanBar(50, BarCells(40), -1); len([]rune(got)) != 6 {
		t.Fatalf("narrow bar is %d cells, want 6", len([]rune(got)))
	}
}

func TestPercentChangedIgnoresNoise(t *testing.T) {
	if PercentChanged(7.08, 7.09) {
		t.Fatal("0.01pp should not trigger a redraw")
	}
	if !PercentChanged(7.0, 8.0) {
		t.Fatal("a full percentage point must trigger a redraw")
	}
}
