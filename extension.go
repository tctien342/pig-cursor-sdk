// Package extension is the pig extension factory: it registers the cursor
// provider, its model catalog, its API-key auth, and the /cursor-* controls.
// Every path that runs per turn is Go.
package pig_cursor_sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
}

// Extension is pig's standard Go factory entry point.
func Extension() *sdk.Extension {
	e := sdk.New("pig-cursor-sdk")
	st := &state{mode: "agent", fast: map[string]bool{}}

	// Register at load time from the on-disk or embedded catalog so
	// `pig --list-models cursor` works without a bridge or the network.
	e.RegisterProvider(providerID, st.providerConfig(st.startupModels()))

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
		ctx.Notify(strings.Join(lines, "\n"), "info")
		return nil
	})

	// Usage is server-side and only settles after a run ends, so the footer is
	// refreshed when a turn finishes rather than polled.
	e.OnEvent("turn_end", func(ctx sdk.Context, _ map[string]any) (any, error) {
		st.refreshStatus(ctx)
		return nil, nil
	})

	e.Command("cursor-usage", "Show Cursor token and cost usage for this session.", func(ctx sdk.Context, args string) error {
		eng := st.engineRef()
		if eng == nil {
			ctx.Notify("cursor: no active session yet (start a turn, then ask again)", "info")
			return nil
		}
		u := eng.Reported()
		if !u.Reported {
			ctx.Notify("cursor: no usage reported yet — finish a turn first", "info")
			return nil
		}
		lines := []string{fmt.Sprintf("tokens: %s total (%s in · %s out",
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
		lines = append(lines, "scope: the Cursor agents this pig session created — Cursor exposes no account-wide usage API")
		ctx.Notify(strings.Join(lines, "\n"), "info")
		return nil
	})

	e.OnSessionStart(func(ctx sdk.Context, _ map[string]any) (any, error) {
		st.configure(ctx)
		st.refreshStatus(ctx)
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
	s.mu.Lock()
	home := s.cfg.ConfigHome
	_ = home
	s.mu.Unlock()
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

func (s *state) configHomeSDK() string {
	s.mu.Lock()
	home := s.cfg.ConfigHome
	s.mu.Unlock()
	if home != "" {
		return home
	}
	return s.defaultConfigHome()
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
	home := s.cfg.ConfigHome
	if home == "" {
		home = s.configHomeSDK()
	}
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

// refreshStatus keeps one compact footer line. Cursor signals belong in the
// status area, never in the transcript.
func (s *state) refreshStatus(ctx sdk.Context) {
	s.mu.Lock()
	mode, pid := s.mode, 0
	if s.engine != nil {
		pid = s.engine.BridgePID()
	}
	s.mu.Unlock()
	full := ctx.Model()
	if !strings.HasPrefix(full, providerID+"/") {
		ctx.ClearStatus(statusKey)
		return
	}
	if os.Getenv("PI_CURSOR_FOOTER") == "0" {
		ctx.ClearStatus(statusKey)
		return
	}
	parts := []string{"cursor", strings.TrimPrefix(full, providerID+"/")}
	if mode == "plan" {
		parts = append(parts, "plan")
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
