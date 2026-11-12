// Package extension is the pig extension factory: it registers the cursor
// provider, its model catalog, its API-key auth, and the /cursor-* controls.
// Every path that runs per turn is Go.
package pig_cursor_sdk

import (
	"context"
	"encoding/json"
	"fmt"
	planusage "github.com/tctien342/pig-cursor-sdk/internal/planusage"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"

	provider "github.com/tctien342/pig-cursor-sdk/internal/provider"
)

const (
	providerID   = "cursor"
	providerName = "Cursor (local bridge)"
	providerAPI  = "pig-cursor-sdk"
	statusKey    = "cursor"
	catalogFile  = "catalog.json"
	credFile     = "credential.json"
)

// state is the mutable runtime shared by the provider callbacks and commands.
type state struct {
	mu sync.Mutex

	cfg     provider.Config
	engine  *provider.Engine
	catalog *provider.Catalog

	mode      string // Cursor mode: agent | plan
	fast      map[string]bool
	configErr string

	// Plan bar widget: a usage bar above the editor, filled in on read.
	planRender func(int) []string
	renderCtx  *sdk.Context // newest context the widget render may read through
	planPhase  int          // cells the bar may reveal; -1 once settled
	planShown  bool         // widget currently installed for this model
	planPct    float64
	planAnim   uint64    // stale-animation guard
	planWarn   sync.Once // "no data" is worth saying once per session

	// One background worker does every network read and every host push that a
	// handler must not wait for. workPending makes repeat turns coalesce instead
	// of stacking a goroutine per turn.
	workPending bool
	workRunning bool
	workForce   bool // a session_start read, which ignores the cache
}

// Extension is pig's standard Go factory entry point.
func Extension() *sdk.Extension {
	e := sdk.New("pig-cursor-sdk")
	st := &state{mode: "agent", fast: map[string]bool{}}

	// Register at load time from the on-disk or embedded catalog so
	// `pig --list-models cursor` works without a bridge or the network.
	e.RegisterProvider(providerID, st.providerConfig(st.startupModels()))

	// Cursor's own tool calls (its built-in shell, reader, search) have no host
	// tool behind them, so the extension draws their transcript line itself. One
	// registration covers live turns and the replay of a resumed session.
	e.EntryRenderer(provider.CursorToolEntryType, provider.RenderCursorToolEntry)

	e.Command("cursor-refresh-models", "Refresh the live Cursor model catalog without restarting pig.", func(ctx sdk.Context, args string) error {
		n, err := st.refreshModels(ctx)
		if err != nil {
			ctx.Notify("Cursor catalog refresh failed: "+err.Error(), "error")
			return nil
		}
		ctx.Notify(fmt.Sprintf("Cursor catalog refreshed: %d models available.", n), "info")
		return nil
	})

	e.Command("cursor-login", "Store a Cursor API key for this extension.", func(ctx sdk.Context, args string) error {
		if err := st.login(ctx); err != nil {
			ctx.Notify(err.Error(), "error")
		}
		return nil
	})

	e.Command("cursor-mode", "Cursor plan mode (/cursor-mode plan) or agent mode (/cursor-mode agent).", func(ctx sdk.Context, args string) error {
		mode := strings.TrimSpace(strings.ToLower(args))
		switch mode {
		case "plan", "agent":
			st.mu.Lock()
			st.mode = mode
			st.cfg.Mode = mode
			eng := st.engine
			st.engine = nil // agents built after this use the new mode
			st.mu.Unlock()
			if eng != nil {
				eng.Stop()
			}
			ctx.Notify("Cursor mode: "+mode+". The next request starts a fresh Cursor agent.", "info")
		case "":
			ctx.Notify("Cursor mode: "+st.currentMode(), "info")
		default:
			ctx.Notify("Usage: /cursor-mode plan or /cursor-mode agent", "warning")
		}
		return nil
	})

	e.Command("cursor-abort", "Abort this session's live Cursor run without undoing completed work.", func(ctx sdk.Context, args string) error {
		st.mu.Lock()
		eng := st.engine
		st.mu.Unlock()
		if eng != nil && eng.AbortLiveRuns() {
			ctx.Notify("Live Cursor run released. Work the host already completed was not undone.", "info")
		} else {
			ctx.Notify("No live Cursor run in this session.", "info")
		}
		return nil
	})

	e.Command("cursor-status", "Show the Cursor bridge, catalog, and tool mode.", func(ctx sdk.Context, args string) error {
		st.mu.Lock()
		mode, models, pid, bin := st.mode, 0, 0, provider.BridgePathFor(st.cfg)
		if st.catalog != nil {
			models = st.catalog.Size()
		}
		if st.engine != nil {
			pid = st.engine.BridgePID()
		}
		st.mu.Unlock()
		key := "not set"
		if k := st.apiKey(ctx); k != "" {
			key = "set"
		}
		lines := []string{
			fmt.Sprintf("bridge: %s (pid %d)", orNone(bin), pid),
			fmt.Sprintf("catalog: %d models · mode %s · key %s", models, mode, key),
			"tools: host tools — pig executes and renders them natively",
		}
		if l := usageLine(st.engineRef()); l != "" {
			lines = append(lines, l)
		}
		if eng := st.engineRef(); eng != nil {
			lines = append(lines, planDetail(eng, false)[0])
		}
		ctx.Notify(strings.Join(lines, "\n"), "info")
		return nil
	})

	// Usage is server-side and only settles after a run ends, so the footer is
	// refreshed when a turn finishes rather than polled.
	// Plan usage rides on Cursor Desktop's login, so it is read only when that
	// exists; Check() is a stat plus one indexed key, and the answer is cached.
	// The bar belongs to a Cursor session only, and the host does not call a
	// widget renderer when the model changes, so the widget is synced per turn.
	e.OnEvent("turn_start", func(ctx sdk.Context, _ map[string]any) (any, error) {
		st.syncPlanBar(ctx)
		return nil, nil
	})

	// Account totals cost a fan-out over agents, so the refresh runs on the
	// background worker, at most every PI_CURSOR_ACCOUNT_TTL_S (3 min default).
	e.OnEvent("turn_end", func(ctx sdk.Context, _ map[string]any) (any, error) {
		st.deferUsageWork(ctx, "turn_end")
		return nil, nil
	})

	e.Command("cursor-usage", "Show Cursor token and cost usage for this session.", func(ctx sdk.Context, args string) error {
		eng := st.engineRef()
		if eng == nil {
			ctx.Notify("cursor: no active session yet (start a turn, then ask again)", "info")
			return nil
		}
		var out []string
		if a := st.refreshAccount(ctx, true); a.Known {
			line := fmt.Sprintf("account: %s tokens across %d of %d agents (%d runs)",
				formatTokens(a.TotalTokens), a.Sampled, a.Agents, a.Runs)
			if c := formatCost(a.CostCents, a.CostLabel); c != "" {
				line += " · " + c + " (" + a.CostLabel + ")"
			}
			if a.Truncated {
				line += " · list capped; widen with PI_CURSOR_ACCOUNT_LIMIT"
			}
			if a.Failed > 0 {
				line += fmt.Sprintf(" · %d agent(s) reported no usage", a.Failed)
			}
			out = append(out, line)
		} else if a.Err != "" {
			out = append(out, "account: could not read ("+a.Err+")")
		}
		u := eng.Reported()
		if !u.Reported {
			out = append(out, "this session: nothing yet — finish a turn first")
			out = append(out, planDetail(eng, true)...)
			out = append(out, "window: "+windowNote(eng))
			ctx.Notify(strings.Join(out, "\n"), "info")
			return nil
		}
		lines := []string{fmt.Sprintf("this session: %s tokens (%s in · %s out",
			formatTokens(u.TotalTokens), formatTokens(u.InputTokens), formatTokens(u.OutputTokens))}
		if u.Reasoning > 0 {
			lines[0] += fmt.Sprintf(" · %s reasoning", formatTokens(u.Reasoning))
		}
		lines[0] += ")"
		if c := formatCost(u.CostCents, u.CostLabel); c != "" {
			lines = append(lines, fmt.Sprintf("cost: %s (%s, %d run%s reported by Cursor)",
				c, u.CostLabel, u.Runs, plural(u.Runs)))
		} else {
			lines = append(lines, "cost: not reported by Cursor for this account")
		}

		out = append(out, lines...)
		out = append(out, "window: "+windowNote(eng))
		out = append(out, "scope: agents listed for this API key — Cursor exposes no account-wide usage or plan-limit endpoint to a key")
		ctx.Notify(strings.Join(out, "\n"), "info")
		return nil
	})

	e.OnSessionStart(func(ctx sdk.Context, _ map[string]any) (any, error) {
		st.configure(ctx)
		st.refreshStatus(ctx)
		// Handlers stay local: the plan and account reads reach Cursor over the
		// network, so they run on the background worker rather than making every
		// session start wait on an HTTP round trip.
		st.deferUsageWork(ctx, "session_start")
		return nil, nil
	})

	e.OnSessionShutdown(func(ctx sdk.Context, _ map[string]any) (any, error) {
		st.mu.Lock()
		eng := st.engine
		st.engine = nil
		st.mu.Unlock()
		if eng != nil {
			eng.Stop()
		}
		return nil, nil
	})

	return e
}

// providerConfig is what pig receives for the provider registration.
func (s *state) providerConfig(models []map[string]any) sdk.ProviderConfig {
	return sdk.ProviderConfig{
		"name":   providerName,
		"api":    providerAPI,
		"models": models,
		// The bridge needs no base URL; pig only requires the key to exist.
		"baseUrl": "http://127.0.0.1",
		"streamSimple": func(ctx sdk.Context, model, piContext, options map[string]any) (*sdk.ModelEventStream, error) {
			return s.stream(ctx, model, piContext, options)
		},
	}
}

// configure resolves host environment once per session start.
func (s *state) configure(ctx sdk.Context) {
	home, _ := ctx.ConfigHome()
	s.mu.Lock()
	s.cfg = provider.Config{
		Provider:       providerID,
		Workspace:      ctx.Cwd(),
		ConfigHome:     home,
		BridgeBin:      strings.TrimSpace(os.Getenv("CURSOR_SDK_BRIDGE_BIN")),
		SettingSources: settingSourcesFromEnv(),
		Mode:           s.mode,
		Logger:         newLogger(),
	}
	// The Cursor-tool renderer is a plain function with no config; this is the
	// point where a logger exists to hand it.
	provider.SetToolEntryLogger(s.cfg.Logger)
	s.rebuildEngineLocked()
	s.mu.Unlock()
}

// rebuildEngineLocked replaces the engine so it picks up a new catalog or mode.
// The caller holds s.mu.
func (s *state) rebuildEngineLocked() {
	if s.engine != nil {
		go s.engine.Stop()
	}
	eng := provider.NewEngine(s.cfg)
	eng.SetCatalog(s.loadCatalogLocked())
	s.engine = eng
}

func settingSourcesFromEnv() []string {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("PI_CURSOR_SETTING_SOURCES")))
	switch v {
	case "":
		return nil // Cursor's own defaults
	case "none":
		return []string{}
	}
	return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })
}

// engineFor returns a usable engine even when session_start has not run
// (registration inspection, RPC-only hosts).
func (s *state) engineFor(ctx sdk.Context) *provider.Engine {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.engine == nil {
		home, _ := ctx.ConfigHome()
		s.cfg = provider.Config{
			Provider:   providerID,
			Workspace:  ctx.Cwd(),
			ConfigHome: home,
			Logger:     newLogger(),
			Mode:       s.mode,
		}
		s.rebuildEngineLocked()
	}
	return s.engine
}

func (s *state) currentMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

// ---- stream ---------------------------------------------------------------

// stream is streamSimple: the whole per-turn path, in Go.
func (s *state) stream(ctx sdk.Context, model, piContext, options map[string]any) (*sdk.ModelEventStream, error) {
	eng := s.engineFor(ctx)
	modelID := stringOf(model, "id")
	s.mu.Lock()
	ov := provider.ModelOverride{Thinking: thinkingLevel(ctx, options)}
	if v, ok := s.fast[modelID]; ok {
		ov.Fast, ov.HasFast = v, true
	}
	s.mu.Unlock()
	eng.SetModelOverride(ov)
	s.refreshStatus(ctx)
	return eng.Stream(ctx, model, piContext, options)
}

func thinkingLevel(ctx sdk.Context, options map[string]any) string {
	if v := stringOf(options, "reasoning", "thinkingLevel"); v != "" {
		return v
	}
	if level, err := ctx.GetThinkingLevel(); err == nil {
		return level
	}
	return ""
}

// ---- auth -----------------------------------------------------------------

// apiKey resolves the Cursor key: this extension's stored credential, then
// pig's own credential store, then the environment.
func (s *state) apiKey(ctx sdk.Context) string {
	if k := s.storedKey(); k != "" {
		return k
	}
	if k := ctx.ModelRegistry().GetApiKeyForProvider(providerID); k != nil && *k != "" {
		return *k
	}
	return strings.TrimSpace(os.Getenv("CURSOR_API_KEY"))
}

func (s *state) login(ctx sdk.Context) error {
	ctx.Notify("The key is at cursor.com/dashboard → API Keys. Cursor Desktop login does not provide this key.", "info")
	value, ok, err := ctx.Input("Cursor API key", "crsr_…")
	if err != nil || !ok {
		return err
	}
	key := strings.TrimSpace(fmt.Sprint(value))
	if key == "" {
		return fmt.Errorf("no API key entered")
	}
	if err := s.saveKey(ctx, key); err != nil {
		return err
	}
	ctx.Notify("Cursor API key stored.", "info")
	return nil
}

// saveKey writes our own credential file and mirrors the key into pig's
// runtime registry so the model list becomes available right away.
func (s *state) saveKey(ctx sdk.Context, key string) error {
	dir := provider.DefaultStateDir(s.configHome(ctx))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, _ := json.Marshal(map[string]any{"type": "api_key", "key": key, "savedAt": time.Now().Unix()})
	if err := os.WriteFile(filepath.Join(dir, credFile), raw, 0o600); err != nil {
		return err
	}
	// Re-register with the key so the catalog and availability update at once.
	s.reRegister(ctx)
	return nil
}

func (s *state) storedKey() string {
	home := s.configHomeSDK()
	if home == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(provider.DefaultStateDir(home), credFile))
	if err != nil {
		return ""
	}
	var doc struct {
		Key string `json:"key"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	return strings.TrimSpace(doc.Key)
}

func (s *state) configHome(ctx sdk.Context) string {
	if home, err := ctx.ConfigHome(); err == nil && home != "" {
		return home
	}
	return s.configHomeSDK()
}

// configHomeLocked resolves the pig home. It takes no lock, so a caller that
// already holds s.mu can use it; [state.configHomeSDK] is the locking form.
func (s *state) configHomeLocked() string {
	if s.cfg.ConfigHome != "" {
		return s.cfg.ConfigHome
	}
	return s.defaultConfigHome()
}

func (s *state) configHomeSDK() string {
	s.mu.Lock()
	home := s.configHomeLocked()
	s.mu.Unlock()
	return home
}

// ---- catalog --------------------------------------------------------------

// startupModels answers pig's catalog request at load time: cached catalog on
// disk, else the embedded snapshot. Never the network, so a cold start stays
// fast and works offline.
func (s *state) startupModels() []map[string]any {
	home := s.defaultConfigHome()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.ConfigHome == "" {
		s.cfg.ConfigHome = home
	}
	cat := s.loadCatalogLocked()
	if cat == nil {
		return []map[string]any{}
	}
	return cat.PigModels()
}

// defaultConfigHome resolves the pig home without touching shared state, so it
// is safe to call while s.mu is held.
func (s *state) defaultConfigHome() string {
	if h := strings.TrimSpace(os.Getenv("PIG_HOME")); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".pig")
	}
	return ".pig"
}

// loadCatalogLocked returns the catalog, loading it on first use.
func (s *state) loadCatalogLocked() *provider.Catalog {
	if s.catalog != nil {
		return s.catalog
	}
	// Locked variant on purpose: the callers of this method hold s.mu, and the
	// locking form would deadlock on the same mutex (see
	// TestLoadCatalogLockedResolvesHomeWithoutRelocking).
	home := s.configHomeLocked()
	windows := provider.LoadContextWindows(home)
	if items, err := readCatalog(filepath.Join(provider.DefaultStateDir(home), catalogFile)); err == nil && len(items) > 0 {
		s.catalog = provider.BuildCatalog(items, windows)
		return s.catalog
	}
	items, err := provider.FallbackCatalog()
	if err != nil {
		return nil
	}
	s.catalog = provider.BuildCatalog(items, windows)
	return s.catalog
}

// refreshModels fetches Cursor's live catalog through the bridge, caches it,
// and re-registers the provider so the new list is visible without a restart.
func (s *state) refreshModels(ctx sdk.Context) (int, error) {
	key := s.apiKey(ctx)
	if key == "" {
		return 0, fmt.Errorf("no Cursor API key; run /cursor-login first")
	}
	eng := s.engineFor(ctx)
	callCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := eng.Client(callCtx, key)
	if err != nil {
		return 0, err
	}
	defer cancel()
	items, err := c.Models(callCtx, key)
	if err != nil {
		return 0, err
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return 0, err
	}
	parsed, err := provider.ParseModelItems(raw)
	if err != nil {
		return 0, err
	}
	home := s.configHome(ctx)
	if err := writeCatalog(filepath.Join(provider.DefaultStateDir(home), catalogFile), parsed); err != nil {
		newLogger().Error("catalog cache write failed", "err", err)
	}
	s.mu.Lock()
	s.catalog = provider.BuildCatalog(parsed, provider.LoadContextWindows(home))
	n := s.catalog.Size()
	s.mu.Unlock()
	s.reRegister(ctx)
	return n, nil
}

// reRegister pushes the current catalog into pig's live provider registry.
func (s *state) reRegister(ctx sdk.Context) {
	models := s.startupModels()
	ctx.ModelRegistry().RegisterProvider(providerID, s.providerConfig(models))
}

func readCatalog(path string) ([]*ModelItem, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Items json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Items) == 0 {
		return nil, fmt.Errorf("catalog file is not readable")
	}
	return provider.ParseModelItems(doc.Items)
}

func writeCatalog(path string, items []*ModelItem) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(map[string]any{"version": 1, "savedAt": time.Now().UnixMilli(), "items": items})
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// engineRef returns the live engine without blocking the caller.
func (s *state) engineRef() *provider.Engine {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.engine
}

// ---- status ---------------------------------------------------------------

// isCursorModel reports whether the active model belongs to this provider. The
// host replicates the bare model id in [sdk.Context.Model] ("grok-4.7@500k:slow",
// not "cursor/grok-4.7@500k:slow"), so the provider id is the reliable signal and
// a "cursor/" prefix check silently never matches. A model the host has not
// replicated yet reads "" and is treated as ours: the caller installs and lets the
// next sync remove it, which turn_end guarantees.
func isCursorModel(ctx sdk.Context) bool {
	if p := ctx.ModelProvider(); p != "" {
		return p == providerID
	}
	m := ctx.Model()
	return m == "" || strings.HasPrefix(m, providerID+"/")
}

// cursorModelName is the active model without a provider prefix, for the footer.
func cursorModelName(ctx sdk.Context) string {
	return strings.TrimPrefix(ctx.Model(), providerID+"/")
}

// refreshStatus keeps one compact footer line. Cursor signals belong in the
// status area, never in the transcript.
func (s *state) refreshStatus(ctx sdk.Context) {
	s.mu.Lock()
	mode, pid := s.mode, 0
	if s.engine != nil {
		pid = s.engine.BridgePID()
	}
	s.mu.Unlock()
	if !isCursorModel(ctx) {
		ctx.ClearStatus(statusKey)
		return
	}
	if os.Getenv("PI_CURSOR_FOOTER") == "0" {
		ctx.ClearStatus(statusKey)
		return
	}
	parts := []string{"cursor", cursorModelName(ctx)}
	if mode == "plan" {
		parts = append(parts, "plan")
	}
	// The allowance lives in the widget above the editor. With the bar turned off
	// it belongs here instead, so the number is still visible somewhere.
	if os.Getenv("PI_CURSOR_PLAN_BAR") == "0" {
		if snap := enginePlan(s.engineRef()); snap != nil {
			parts = append(parts, formatPlan(snap))
		}
	}
	if pid > 0 {
		parts = append(parts, fmt.Sprintf("pid %d", pid))
	}
	if eng := s.engineRef(); eng != nil {
		if u := eng.Reported(); u.Reported {
			if u.TotalTokens > 0 {
				parts = append(parts, formatTokens(u.TotalTokens)+" tok")
			}
			if c := formatCost(u.CostCents, u.CostLabel); c != "" {
				parts = append(parts, c)
			}
		}
	}
	ctx.SetStatus(statusKey, strings.Join(parts, " · "))
}

// stringOf returns the first non-empty string value among the given keys.
func stringOf(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "not found (set CURSOR_SDK_BRIDGE_BIN)"
	}
	return filepath.Base(s)
}

// usageLine is the status command's usage row; empty when nothing is reported.
func usageLine(eng *provider.Engine) string {
	if eng == nil {
		return ""
	}
	u := eng.Reported()
	if !u.Reported {
		return "usage: not reported yet"
	}
	line := fmt.Sprintf("usage: %s tokens · %d runs", formatTokens(u.TotalTokens), u.Runs)
	if c := formatCost(u.CostCents, u.CostLabel); c != "" {
		line += " · " + c + " (" + u.CostLabel + ")"
	}
	return line
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// refreshPlan re-reads Cursor's plan usage when the cached answer is stale, and
// returns the snapshot when there is one.
func (s *state) refreshPlan(force bool) *planusage.Snapshot {
	eng := s.engineRef()
	if eng == nil {
		return nil
	}
	snap, _, _ := eng.RefreshPlan(force)
	return snap
}

// enginePlan returns the last good plan snapshot without contacting Cursor.
func enginePlan(eng *provider.Engine) *planusage.Snapshot {
	if eng == nil {
		return nil
	}
	snap, _, _ := eng.Plan()
	return snap
}

// planDetail is the /cursor-usage block: the numbers when they are known, and the
// reason they are not when they are not.
func planDetail(eng *provider.Engine, force bool) []string {
	snap, kind, msg := eng.RefreshPlan(force)
	if snap == nil {
		switch kind {
		case planusage.NoInstall:
			return []string{"plan: not read — Cursor Desktop is not installed on this machine"}
		case planusage.NoLogin:
			return []string{"plan: not read — Cursor Desktop is installed but logged out (sign in inside the Cursor app)"}
		default:
			return []string{"plan: not read — " + orNone(msg)}
		}
	}
	line := fmt.Sprintf("plan: %s%s · used $%s of $%s included",
		orNone(snap.PlanName), planPrice(snap),
		money(float64(snap.IncludedSpend)/100), money(float64(snap.IncludedLimit)/100))
	if snap.BonusSpend > 0 {
		line += fmt.Sprintf(" + $%s bonus ($%s total)",
			money(float64(snap.BonusSpend)/100), money(float64(snap.TotalSpend)/100))
	}
	out := []string{line, fmt.Sprintf("cycle: %s → %s · %.1f%% used",
		snap.CycleStart.Local().Format("2 Jan"), snap.CycleEnd.Local().Format("2 Jan"), snap.PercentUsed)}
	if snap.PercentUsed > 0 {
		out[1] += fmt.Sprintf(" · %.2f%% (Cursor's own figure)", snap.PercentUsed)
	}
	if snap.Account != "" {
		out = append(out, "account: "+snap.Account+" (via Cursor Desktop login)")
	}
	return out
}

func planPrice(snap *planusage.Snapshot) string {
	if snap.Price == "" {
		return ""
	}
	return " (" + snap.Price + ")"
}

// refreshAccount re-reads account-wide usage and remembers the engine so the
// footer can show it. Cheap when the cached copy is still fresh.
func (s *state) refreshAccount(ctx sdk.Context, force bool) provider.AccountTotals {
	eng := s.engineRef()
	if eng == nil {
		return provider.AccountTotals{}
	}
	limit := uint32(50)
	if v := os.Getenv("PI_CURSOR_ACCOUNT_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = uint32(n)
		}
	}
	return eng.RefreshAccount(s.apiKey(ctx), limit, force)
}

// windowNote says, in words, what is and is not known about plan limits.
func windowNote(eng *provider.Engine) string {
	if w := limitWindow(eng); w != "" {
		return w + " — reported by Cursor when it refused a call"
	}
	return "no plan-limit data: Cursor gives limit windows only inside a refusal error, and none to an API key proactively"
}

// limitWindow renders Cursor's reported usage window, if any. The bridge protocol
// carries limit data only on a refusal (SdkErrorDetails.rate_limit), so an empty
// string means "Cursor has not told us", not "there is headroom".
func limitWindow(eng *provider.Engine) string {
	if eng == nil {
		return ""
	}
	r := eng.LimitWindow()
	if r == nil {
		return ""
	}
	return "limit " + r.Summary(time.Now())
}

// ---- plan bar widget ------------------------------------------------------

const planWidgetKey = "cursor-plan"

// snapPercent is the percentage the bar shows: Cursor's own figure when it sends
// one, otherwise the ratio of the included allowance.
func snapPercent(snap *planusage.Snapshot) float64 {
	if snap == nil {
		return 0
	}
	if snap.PercentUsed > 0 {
		return snap.PercentUsed
	}
	if snap.IncludedLimit > 0 {
		return 100 * float64(snap.IncludedSpend) / float64(snap.IncludedLimit)
	}
	return 0
}

func (s *state) setPlanPct(snap *planusage.Snapshot) {
	s.mu.Lock()
	s.planPct = snapPercent(snap)
	s.mu.Unlock()
}

// planBarState is the plan-bar state syncPlanBar decides from, copied out under
// one lock so no decision is made while the mutex is held.
type planBarState struct {
	shown    bool // widget currently installed for this model
	havePlan bool // a plan snapshot exists, so the row has something to draw
	render   func(width int) []string
}

// planBarState copies the state the bar needs. The engine has its own mutex and
// never calls back into this package, so reading it under s.mu is a fixed lock
// order (state → engine), not a cycle.
func (s *state) planBarState() planBarState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := planBarState{shown: s.planShown, render: s.planRender}
	if s.engine != nil {
		if snap, _, _ := s.engine.Plan(); snap != nil {
			out.havePlan = true
		}
	}
	return out
}

// syncPlanBar makes the widget match the active model: a Cursor model gets the
// bar, anything else gets the built-in layout back.
//
// Every state read here goes through planBarState, and s.mu is never held across
// a host call. The version that held the lock and then called barShown() /
// engineRef() (which lock the same mutex) hung the extension permanently on the
// first turn_start: sync.Mutex is not reentrant, so the handler never returned,
// the host waited on it, and pig froze on every chat and /reload.
func (s *state) syncPlanBar(ctx sdk.Context) {
	if os.Getenv("PI_CURSOR_PLAN_BAR") == "0" {
		return
	}
	model := ctx.Model()
	// The host replicates the model with its state updates, so it can still read
	// "" in the session_start handler. Install anyway and let the renderer decide:
	// a model that turns out not to be ours gets the widget removed on the next
	// sync, which turn_end guarantees.
	mine := isCursorModel(ctx)
	st := s.planBarState()
	newLogger().Info("plan bar sync", "model", model, "mine", mine, "shown", st.shown,
		"width", ctx.Width(), "planData", st.havePlan)
	if !mine {
		if st.shown {
			// Only forget the widget once the host accepted the removal, so a
			// rejected call is retried on the next sync instead of leaving a bar
			// on screen for a model that is not ours.
			if err := ctx.SetWidgetRenderer(planWidgetKey, nil); err != nil {
				newLogger().Warn("plan bar removal rejected by the host", "err", err.Error())
				return
			}
			s.setPlanShown(false)
		}
		return
	}
	if !st.shown || st.render == nil {
		s.installPlanBar(ctx)
		return
	}
	// Already installed: re-push so the row is laid out for the live width. The
	// host does not re-measure lines it was given, and a row that is too wide for
	// the terminal is fatal to the TUI, not merely ugly.
	_ = ctx.SetWidgetRenderer(planWidgetKey, st.render, sdk.WidgetOptions{"placement": "aboveEditor"})
}

func (s *state) barShown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.planShown
}

func (s *state) setPlanShown(shown bool) {
	s.mu.Lock()
	s.planShown = shown
	s.mu.Unlock()
}

// planRenderFor records ctx as the one the widget may read through and returns the
// stable renderer. The renderer keeps a context handle only as a source of live
// state (model, provider, width), so it must always be the newest one: a width
// change or a re-push can arrive long after the handler that installed the widget,
// including across a session replacement, where the old context is stale and panics.
func (s *state) planRenderFor(ctx sdk.Context) func(int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renderCtx = &ctx
	if s.planRender == nil {
		s.planRender = func(width int) []string {
			s.mu.Lock()
			c := s.renderCtx
			s.mu.Unlock()
			if c == nil {
				return nil
			}
			return s.planBarLines(*c, width)
		}
	}
	return s.planRender
}

// installPlanBar must not be called with s.mu held: it calls the host.
func (s *state) installPlanBar(ctx sdk.Context) {
	render := s.planRenderFor(ctx)
	if err := ctx.SetWidgetRenderer(planWidgetKey, render, sdk.WidgetOptions{"placement": "aboveEditor"}); err != nil {
		newLogger().Warn("plan bar rejected by the host", "err", err.Error())
		return
	}
	s.setPlanShown(true)
	newLogger().Info("plan bar installed")
}

// planBarLines is one row: label, bar, percentage, and the two facts that change
// what the number means (which pot is being spent, and when it resets).
func (s *state) planBarLines(ctx sdk.Context, width int) []string {
	// PI_CURSOR_WIDGET_PROBE=1 draws a constant row instead of any plan data. It
	// answers one question in isolation — does this host paint a widget pushed by a
	// Go subprocess at all — so a missing bar is not confused with missing plan data.
	if os.Getenv("PI_CURSOR_WIDGET_PROBE") == "1" {
		return []string{" PI-WIDGET-PROBE-OK"}
	}
	if !isCursorModel(ctx) {
		return nil
	}
	snap := enginePlan(s.engineRef())
	if snap == nil {
		s.planWarn.Do(func() {
			newLogger().Warn("plan bar has no data to draw", "hint", "the plan read did not return a snapshot")
		})
		return nil
	}
	s.mu.Lock()
	phase, pct := s.planPhase, s.planPct
	s.mu.Unlock()
	if pct <= 0 {
		pct = snapPercent(snap)
	}
	if width < 0 {
		width = 0
	}
	cells := provider.BarCells(width)
	if width == 0 {
		// No size reported yet. The host does not re-measure a line it is given,
		// and an over-wide row kills the TUI, so send a short row and let the next
		// sync resize it.
		cells = 8
	}
	if phase < 0 {
		phase = cells
	}
	bar := provider.PlanBar(pct, cells, phase)

	rest := " " + trimFloat(pct) + "%"
	switch {
	case snap.Exhausted() && snap.BonusSpend > 0:
		rest += " · included used · $" + money(float64(snap.BonusSpend)/100) + " bonus"
	case snap.Exhausted():
		rest += " · included used"
	default:
		rest += " · $" + money(float64(snap.Remaining())/100) + " left"
	}
	if !snap.CycleEnd.IsZero() {
		rest += " · resets " + snap.CycleEnd.Local().Format("2 Jan")
	}
	if width == 0 {
		rest = " " + trimFloat(pct) + "%"
	}
	line := " cursor " + bar + rest
	if r := []rune(line); width > 0 && len(r) > width {
		line = string(r[:max(0, width-1)]) + "…"
	}
	newLogger().Debug("plan bar rendered", "width", width, "cells", cells, "cols", len([]rune(line)))
	return []string{line}
}

// animatePlanBar fills the bar in over ~0.8s after a fresh read. The burst stops
// by itself and a superseding refresh wins, so it never redraws indefinitely.
func (s *state) animatePlanBar(ctx sdk.Context, snap *planusage.Snapshot) {
	if os.Getenv("PI_CURSOR_PLAN_BAR") == "0" || snap == nil {
		return
	}
	if !s.barShown() {
		s.installPlanBar(ctx)
	}
	render := s.planRenderFor(ctx)
	s.mu.Lock()
	s.planAnim++
	gen := s.planAnim
	s.mu.Unlock()

	cells := provider.BarCells(ctx.Width())
	go func() {
		// Same reason as the usage worker: this goroutine can outlive its context.
		defer func() {
			if r := recover(); r != nil {
				newLogger().Warn("plan bar animation stopped", "panic", fmt.Sprint(r))
			}
		}()
		for phase := 1; phase <= cells; phase++ {
			time.Sleep(65 * time.Millisecond)
			s.mu.Lock()
			stale := s.planAnim != gen
			s.planPhase = phase
			s.mu.Unlock()
			if stale {
				return
			}
			_ = ctx.SetWidgetRenderer(planWidgetKey, render, sdk.WidgetOptions{"placement": "aboveEditor"})
		}
		s.mu.Lock()
		if s.planAnim == gen {
			s.planPhase = -1 // settled: show the true fill
		}
		s.mu.Unlock()
		_ = ctx.SetWidgetRenderer(planWidgetKey, render, sdk.WidgetOptions{"placement": "aboveEditor"})
	}()
}

// deferUsageWork moves the two network reads (Cursor's plan allowance and the
// account fan-out) and the widget/status pushes off the event handler that asked
// for them. A handler that waits on the network makes the host look frozen: pig
// is blocked on the handler's result for the whole round trip.
//
// One worker, single-flight: a request that arrives while it is running sets
// workPending and is served by the same goroutine on its next pass, so a burst of
// turns cannot stack goroutines or repeat the reads.
func (s *state) deferUsageWork(ctx sdk.Context, reason string) {
	if !s.requestWork(reason == "session_start") {
		return
	}
	go func() {
		// A worker outlives the handler that started it, and its context goes stale
		// when the session is replaced (/new, /resume, /fork). A stale read panics;
		// the worker must log that instead of taking the extension process down.
		defer func() {
			if r := recover(); r != nil {
				newLogger().Warn("background usage work stopped", "panic", fmt.Sprint(r))
			}
		}()
		for {
			force, ok := s.takeWork()
			if !ok {
				return
			}
			s.runUsageWork(ctx, force)
		}
	}()
}

// requestWork records a request and reports whether the caller must start the
// worker. Every flag transition happens under s.mu, so a request that lands while
// a pass is running (or just as the worker is about to exit) is never lost: either
// it sees workRunning and is picked up by the next pass, or it starts a new worker.
func (s *state) requestWork(force bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workPending = true
	if force {
		s.workForce = true
	}
	if s.workRunning {
		return false
	}
	s.workRunning = true
	return true
}

// takeWork claims the next pass, or reports that the worker should exit.
func (s *state) takeWork() (force bool, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.workPending {
		s.workRunning = false
		return false, false
	}
	s.workPending = false
	force, s.workForce = s.workForce, false
	return force, true
}

// runUsageWork is one pass of the background worker: read, then show.
func (s *state) runUsageWork(ctx sdk.Context, force bool) {
	before := s.planPercent()
	snap := s.refreshPlan(force)
	if snap != nil {
		s.setPlanPct(snap)
	}
	s.syncPlanBar(ctx)
	if snap != nil && provider.PercentChanged(before, snapPercent(snap)) {
		s.animatePlanBar(ctx, snap)
	}
	s.refreshAccount(ctx, false)
	s.refreshStatus(ctx)
}

func (s *state) planPercent() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.planPct
}
