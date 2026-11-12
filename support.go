package pig_cursor_sdk

import (
	planusage "github.com/tctien342/pig-cursor-sdk/internal/planusage"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	sdkpb "github.com/tctien342/pig-cursor-sdk/gen/sdk/v1"
	cursor "github.com/tctien342/pig-cursor-sdk/internal/cursor"
)

// ModelItem is the catalog entry type used across this package.
type ModelItem = sdkpb.SdkModel

// newLogger builds the extension logger. pig owns the terminal, so diagnostics
// go to stderr, and with PI_CURSOR_GO_DEBUG=1 also to a file, because pig
// sends an extension subprocess's stderr to a temp log nobody points at.
func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if os.Getenv("PI_CURSOR_GO_DEBUG") == "1" {
		level = slog.LevelDebug
	}
	w := io.Writer(os.Stderr)
	// The file sink is unconditional: an extension subprocess's stderr lands in
	// a temp log nobody points at, and a provider that fails at 3am needs a
	// trail that outlives the run.
	path := os.Getenv("PI_CURSOR_GO_LOG")
	if path == "" {
		path = filepath.Join(bridgeInstallDir(), "agent.log")
	}
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		w = io.MultiWriter(os.Stderr, f)
	}
	// pid, because several pig processes append to this one file, and a question
	// like "did closing that session break this one?" cannot be answered without
	// knowing which process wrote which line.
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})).
		With("ext", "pig-cursor-sdk", "pid", os.Getpid())
}

// bridgeInstallDir is the directory holding the bridge binary; its parent is
// where this extension keeps its state.
func bridgeInstallDir() string { return cursor.StateDir() }

func bridgeInstallPathShim() string { return cursor.FindBinary() }

// formatTokens keeps the footer compact: 1234 → 1.2k, 12_345_678 → 12.3M.
func formatTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return trimFloat(float64(n)/1_000_000) + "M"
	case n >= 1_000:
		return trimFloat(float64(n)/1_000) + "k"
	default:
		return strconv.FormatInt(n, 10)
	}
}

func trimFloat(v float64) string {
	s := strconv.FormatFloat(v, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0")
}

// money renders dollars with at most two decimals and no trailing zeros, because
// Cursor reports cents and a single decimal would round $0.04 up to $0.0.
func money(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimSuffix(s, "0")
	s = strings.TrimSuffix(s, ".0")
	return strings.TrimSuffix(s, ".")
}

// formatCost renders Cursor's cost report. Cursor reports cents; "raw" means the
// undiscounted model cost because the account reports no charged amount (plan
// -included, BYOK, or grant usage), which the footer marks so the number is not
// read as a bill.
func formatCost(cents float64, label string) string {
	if cents <= 0 {
		return ""
	}
	dollars := "$" + money(cents/100)
	if label == "raw" {
		dollars += " raw"
	}
	return dollars
}

// formatPlan is the footer's plan line. Two cases, because "8% used" and "the
// allowance is gone, you are on granted bonus" are different states, and the
// second one is exactly when the number matters most.
func formatPlan(snap *planusage.Snapshot) string {
	if snap == nil {
		return ""
	}
	pct := snap.PercentUsed
	if pct <= 0 && snap.IncludedLimit > 0 {
		pct = 100 * float64(snap.IncludedSpend) / float64(snap.IncludedLimit)
	}
	trimmed := strings.TrimSuffix(money(pct), ".0")
	if snap.Exhausted() {
		line := "plan " + trimmed + "% · included used"
		if snap.BonusSpend > 0 {
			line += " · $" + money(float64(snap.BonusSpend)/100) + " bonus"
		}
		return line
	}
	return "plan " + trimmed + "% · $" + money(float64(snap.Remaining())/100) + " left"
}
