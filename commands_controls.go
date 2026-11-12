package pig_cursor_sdk

import (
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

func (s *state) registerModelControls(e *sdk.Extension) {
	e.Command("cursor-fast", "Select Cursor fast mode: /cursor-fast on, off, or default.", func(ctx sdk.Context, args string) error {
		if !isCursorModel(ctx) {
			ctx.Notify("Select a Cursor model first.", "warning")
			return nil
		}
		s.mu.Lock()
		catalog := s.catalog
		s.mu.Unlock()
		current := catalog.Lookup(ctx.Model())
		if current == nil {
			ctx.Notify("The selected model is absent from the Cursor catalog; try /cursor-refresh-models.", "warning")
			return nil
		}
		mode := strings.ToLower(strings.TrimSpace(args))
		if mode == "" {
			ctx.Notify("Cursor selection: "+current.ID+". Use /cursor-fast on, off, or default. A change applies to the next new request.", "info")
			return nil
		}
		next, reason := catalog.FastSelection(current.ID, mode)
		if reason != "" {
			ctx.Notify(reason, "warning")
			return nil
		}
		if next.ID == current.ID {
			ctx.Notify("Cursor already uses "+next.ID+".", "info")
			return nil
		}
		ok, err := ctx.SetModel(providerID + "/" + next.ID)
		if err != nil || !ok {
			ctx.Notify("The host could not select that fast-mode variant.", "error")
			return nil
		}
		ctx.Notify("Cursor selection: "+next.ID+". Applies to the next new request; the current run is unchanged.", "info")
		return nil
	})
}
