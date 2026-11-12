package pig_cursor_sdk

import (
	"testing"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"

	provider "github.com/tctien342/pig-cursor-sdk/internal/provider"
)

// watchdog runs fn and fails the test if it has not returned in time. A
// self-deadlock on state.mu never returns and never errors, so a timeout is the
// only way to see it from a test.
func watchdog(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return: a helper that locks state.mu was called while state.mu was already held (sync.Mutex is not reentrant)", what)
	}
}

// syncPlanBar used to take state.mu and then call barShown() and engineRef(),
// which take the same mutex. The extension then never answered turn_start, so pig
// froze on every chat and /reload with no error anywhere. These are the state
// reads that were on that path; each one must return on its own.
func TestPlanBarStateReadsDoNotSelfLock(t *testing.T) {
	st := &state{mode: "agent", fast: map[string]bool{}}
	watchdog(t, "planBarState", func() {
		st.planBarState()
		st.setPlanShown(true)
		st.planBarState()
		st.barShown()
		st.setPlanShown(false)
		st.planPercent()
		st.setPlanPct(nil)
	})
	// The renderer factory is on the same path (installPlanBar → planRenderFor).
	watchdog(t, "planRenderFor", func() {
		if st.planRenderFor(sdk.Context{}) == nil {
			t.Error("planRenderFor returned a nil renderer")
		}
	})
}

// loadCatalogLocked runs with state.mu held and used to resolve the pig home
// through the locking helper, which is the same mistake in a second place: it
// only stayed hidden because session_start always fills ConfigHome first.
func TestLoadCatalogLockedResolvesHomeWithoutRelocking(t *testing.T) {
	t.Setenv("PIG_HOME", t.TempDir())
	st := &state{mode: "agent", fast: map[string]bool{}}
	var cat *provider.Catalog
	watchdog(t, "loadCatalogLocked", func() {
		st.mu.Lock()
		cat = st.loadCatalogLocked()
		st.mu.Unlock()
	})
	if cat == nil {
		t.Fatal("loadCatalogLocked returned no catalog: the embedded fallback should always answer")
	}
}

// The usage worker is single-flight: a burst of turns must not stack goroutines,
// and pending work is a level, not a count — several turns that arrive while a
// pass is running are served by one more pass, not one per turn. Both decisions
// happen under state.mu, so they can be tested without a host.
func TestUsageWorkerCoalescesWithoutLosingWork(t *testing.T) {
	st := &state{mode: "agent", fast: map[string]bool{}}

	if !st.requestWork(false) {
		t.Fatal("the first request must start the worker")
	}
	if st.requestWork(false) {
		t.Fatal("a request that arrives while the worker runs must not start a second one")
	}
	// A forced request while running must survive as a forced pass, not vanish.
	if st.requestWork(true) {
		t.Fatal("a forced request while running must still not start a second worker")
	}

	force, ok := st.takeWork()
	if !ok || !force {
		t.Fatalf("first pass = (force %v, ok %v), want the forced pass", force, ok)
	}
	if _, ok = st.takeWork(); ok {
		t.Fatal("three turns while one pass ran must coalesce into one extra pass, not one each")
	}
	// takeWork cleared workRunning, so a later turn starts a fresh worker.
	if !st.requestWork(false) {
		t.Fatal("after the worker exits a new request must start it again")
	}
	if force, ok = st.takeWork(); !ok || force {
		t.Fatalf("pass after restart = (force %v, ok %v), want an unforced pass", force, ok)
	}
}

// A forced pass must stay forced even if an unforced request follows it before the
// worker gets there: a session_start read that ignores the cache is the one thing
// coalescing must not downgrade.
func TestUsageWorkerKeepsAForcedPass(t *testing.T) {
	st := &state{mode: "agent", fast: map[string]bool{}}
	st.requestWork(true)
	st.requestWork(false)
	if force, ok := st.takeWork(); !ok || !force {
		t.Fatalf("pass = (force %v, ok %v), want the forced pass to win", force, ok)
	}
}

// A request that arrives just as the worker exits must not be lost: requestWork
// and takeWork share the mutex, so whichever order they land in, the work runs.
func TestUsageWorkerDoesNotLoseARequestAtExit(t *testing.T) {
	for _, requestFirst := range []bool{true, false} {
		st := &state{mode: "agent", fast: map[string]bool{}}
		if !st.requestWork(false) {
			t.Fatal("first request must start the worker")
		}
		if _, ok := st.takeWork(); !ok {
			t.Fatal("first pass was not claimed")
		}
		// The worker is about to decide whether to exit; a turn arrives now.
		var started bool
		if requestFirst {
			started = st.requestWork(false)
			if _, ok := st.takeWork(); !ok && !started {
				t.Fatalf("requestFirst=%v: the late request was lost", requestFirst)
			}
		} else {
			if _, ok := st.takeWork(); ok {
				t.Fatal("no request is pending, so no pass may be claimed")
			}
			started = st.requestWork(false)
			if !started {
				t.Fatalf("requestFirst=%v: a request after the worker exited must start it", requestFirst)
			}
		}
	}
}
