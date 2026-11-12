package pig_cursor_sdk

import (
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
	provider "github.com/tctien342/pig-cursor-sdk/internal/provider"
)

func (s *state) registerDoctor(e *sdk.Extension) {
	e.Command("cursor-doctor", "Diagnose Cursor locally; --probe pings only an existing bridge.", func(ctx sdk.Context, args string) error {
		arg := strings.TrimSpace(args)
		if arg != "" && arg != "--probe" {
			ctx.Notify("Usage: /cursor-doctor [--probe]", "warning")
			return nil
		}
		s.mu.Lock()
		engine, catalog := s.engine, s.catalog
		s.mu.Unlock()
		var selected *provider.ModelEntry
		if isCursorModel(ctx) {
			selected = catalog.Lookup(ctx.Model())
		}
		report := engine.Doctor(ctx, selected, arg == "--probe")
		ctx.Notify(provider.FormatDoctor(report), "info")
		return nil
	})
}
