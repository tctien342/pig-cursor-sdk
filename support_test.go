package pig_cursor_sdk

import (
	"testing"

	planusage "github.com/tctien342/pig-cursor-sdk/internal/planusage"
)

func TestFormatTokensStaysCompact(t *testing.T) {
	for _, c := range []struct {
		in   int64
		want string
	}{{0, "0"}, {999, "999"}, {1234, "1.2k"}, {2000, "2k"}, {12_345_678, "12.3M"}} {
		if got := formatTokens(c.in); got != c.want {
			t.Errorf("formatTokens(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Cursor reports cents, and a plan-included account reports raw cost rather than
// charged cost. The footer must show small amounts exactly and mark the raw case
// so nobody reads it as a bill.
func TestFormatCostKeepsCentsAndLabelsRaw(t *testing.T) {
	for _, c := range []struct {
		cents float64
		label string
		want  string
	}{{0, "charged", ""}, {7, "charged", "$0.07"}, {70, "charged", "$0.7"}, {120, "raw", "$1.2 raw"}, {1200, "raw", "$12 raw"}} {
		if got := formatCost(c.cents, c.label); got != c.want {
			t.Errorf("formatCost(%v, %q) = %q, want %q", c.cents, c.label, got, c.want)
		}
	}
}

// With PI_CURSOR_PLAN_BAR=0 the allowance moves into the status line, so this
// formatter is the only place the number appears. "included used" and "bonus"
// must not be confused with each other: the second is money Cursor granted after
// the plan ran out, which is exactly when the figure matters.
func TestFormatPlanSaysWhichPotIsBeingSpent(t *testing.T) {
	withRoom := planusage.Snapshot{IncludedLimit: 2000, IncludedSpend: 400, PercentUsed: 20}
	if got, want := formatPlan(&withRoom), "plan 20% · $16 left"; got != want {
		t.Errorf("formatPlan(with room) = %q, want %q", got, want)
	}
	exhausted := planusage.Snapshot{IncludedLimit: 2000, IncludedSpend: 2000, BonusSpend: 5513, PercentUsed: 100}
	if got, want := formatPlan(&exhausted), "plan 100% · included used · $55.13 bonus"; got != want {
		t.Errorf("formatPlan(exhausted) = %q, want %q", got, want)
	}
	if got := formatPlan(nil); got != "" {
		t.Errorf("formatPlan(nil) = %q, want empty", got)
	}
}
