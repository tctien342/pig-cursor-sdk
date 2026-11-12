package pig_cursor_sdk

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"

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
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})).With("ext", "pig-cursor-sdk")
}

// bridgeInstallDir is the directory holding the bridge binary; its parent is
// where this extension keeps its state.
func bridgeInstallDir() string { return cursor.StateDir() }

func bridgeInstallPathShim() string { return cursor.FindBinary() }
