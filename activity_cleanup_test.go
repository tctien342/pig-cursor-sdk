package pig_cursor_sdk

import (
	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
	"github.com/tctien342/pig-cursor-sdk/internal/provider"
	"testing"
)

func TestLifecycleCleanupPreservesStillActiveNewerHint(t *testing.T) {
	source := sdk.Context{}
	state := &state{liveCtx: &source, liveTools: []provider.LiveToolView{{Tool: "newer-tool"}}, liveShown: true}
	// A nil SDK context would reject any attempted host call. Cleanup must return
	// before issuing one when the currently displayed activity is still active.
	state.clearCursorActivity(sdk.Context{})
	if !state.liveShown || len(state.liveTools) != 1 {
		t.Fatal("older lifecycle cleanup erased active activity")
	}
}
