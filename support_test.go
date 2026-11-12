package pig_cursor_sdk

import "testing"

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
