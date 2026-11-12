package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"

	"github.com/tctien342/pig-cursor-sdk/internal/bridge"
	cursor "github.com/tctien342/pig-cursor-sdk/internal/cursor"
	planusage "github.com/tctien342/pig-cursor-sdk/internal/planusage"
)

// Config is the engine's environment, resolved once from the host.
type Config struct {
	Provider       string // "cursor"
	Workspace      string
	ConfigHome     string
	BridgeBin      string
	SettingSources []string
	Mode           string // "" | "agent" | "plan"
	Logger         *slog.Logger
	// DisableHostTools advertises no tools at all (escape hatch for testing).
	DisableHostTools bool

	// LiveTools receives zero or one currently running internal call, and nil on
	// turn teardown. Inline annotations are independent of this optional UI hint.
	LiveTools func(ctx sdk.Context, calls []LiveToolView)
}

// Engine owns the bridge process, the model catalog, and one session lease per
// conversation branch.
type Engine struct {
	cfg Config

	mu       sync.Mutex
	client   *cursor.Client
	catalog  *Catalog
	sessions map[string]*Session
	startErr string
	override ModelOverride
	agents   *agentRegistry

	// nonce separates throwaway sessions from each other; reaped stops the bridge
	// sweep from running on every turn. See sessionKey and Client.
	nonce  string
	reaped bool

	// plan is Cursor's plan-usage picture, read through Cursor Desktop's login
	// when that exists. Nil until it has been read successfully once.
	plan     *planusage.Snapshot
	planKind planusage.Availability
	planMsg  string
	planAt   time.Time

	// account is Cursor's usage summed across the agents it lists for this key,
	// refreshed lazily so the footer does not pay for the fan-out every turn.
	account   AccountTotals
	accountAt time.Time

	// lastRate is the most recent usage window Cursor reported when refusing a
	// call. sdk.v1 has no proactive limit RPC, so this is opportunistic: it is
	// shown once Cursor has told us, and dropped once that window has reset.
	lastRate *cursor.RateLimit
}

// SetModelOverride installs the thinking/fast controls for later turns.
func (e *Engine) SetModelOverride(o ModelOverride) {
	e.mu.Lock()
	e.override = o
	e.mu.Unlock()
}

// AbortLiveRuns releases every live run this process owns: the Cursor run is
// cancelled and parked host calls are released. Completed host work is not
// undone; that is stated to the user by the command.
func (e *Engine) AbortLiveRuns() bool {
	e.mu.Lock()
	sessions := make([]*Session, 0, len(e.sessions))
	for _, s := range e.sessions {
		sessions = append(sessions, s)
	}
	e.mu.Unlock()
	released := false
	for _, s := range sessions {
		if s.agent == nil {
			continue
		}
		if run := s.agent.Active(); run != nil && !run.Finished() {
			_ = run.CancelRun("aborted by the user")
			released = true
		}
	}
	return released
}

// Session is one conversation's Cursor state.
type Session struct {
	Key string

	AgentID string
	agent   *cursor.Agent

	// sentFingerprints is the transcript prefix already fed to the agent, used
	// to detect an append-only continuation versus a rewritten branch.
	sentFingerprints []string

	// prevFingerprints is what sentFingerprints was before the last send, so a
	// send that never reached the agent can be retried instead of being read as
	// a rewritten conversation.
	prevFingerprints []string

	// pending are host tool calls the run is parked on, in arrival order.
	pending []pendingRef

	toolSig     string
	modelID     string
	overrideSig string

	lastUsage *cursor.Usage

	// usage is Cursor's own billing report for this session's agent, refreshed
	// after each turn. It is what the footer shows.
	usage UsageTotals
}

// ModelOverride carries the per-request controls pig exposes (thinking level,
// fast mode) onto Cursor's model parameters.
type ModelOverride struct {
	Fast     bool
	HasFast  bool
	Thinking string
}

type pendingRef struct {
	ID       string
	ToolName string
}

// UsageTotals is the account usage this extension can report: Cursor's numbers
// for the agents it created. Cursor does not expose an account-wide usage RPC, so
// this is deliberately "what we spent", not "what the plan allows".
type UsageTotals struct {
	TotalTokens  int64
	InputTokens  int64
	OutputTokens int64
	Reasoning    int64
	CostCents    float64
	CostLabel    string // "charged" | "raw" | ""
	Runs         int
	Reported     bool
}

// planTTL bounds how often the plan readout may contact Cursor's own API.
func planTTL() time.Duration {
	if v := os.Getenv("PI_CURSOR_PLAN_TTL_S"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return 5 * time.Minute
}

// planEnabled reports whether the plan readout is switched on. It is on by
// default and turns itself off when Cursor Desktop is absent or logged out, so a
// machine without Cursor simply never shows a plan line.
func planEnabled() bool { return os.Getenv("PI_CURSOR_PLAN_USAGE") != "0" }

// RefreshPlan re-reads Cursor's plan usage when the cached copy is stale, and
// never blocks longer than one short HTTP round trip. Call it from turn_end or a
// command — never mid-stream. It checks locally first, so a machine without
// Cursor Desktop costs one stat and no network.
func (e *Engine) RefreshPlan(force bool) (*planusage.Snapshot, planusage.Availability, string) {
	e.mu.Lock()
	snap, kind, msg, fresh := e.plan, e.planKind, e.planMsg, time.Since(e.planAt) < planTTL()
	e.mu.Unlock()
	if !planEnabled() {
		return nil, "disabled", "PI_CURSOR_PLAN_USAGE=0"
	}
	// A recent "not installed" or "not logged in" answer is cached too, so a
	// machine without Cursor Desktop is asked about once per TTL, not every turn.
	if !force && fresh {
		if snap != nil {
			return snap, planusage.Available, ""
		}
		if kind != "" {
			return nil, kind, msg
		}
	}

	avail, why := planusage.Check()
	if avail != planusage.Available {
		e.mu.Lock()
		e.planKind, e.planMsg = avail, why
		e.mu.Unlock()
		return nil, avail, why
	}
	token, err := planusage.Token()
	if err != nil {
		e.mu.Lock()
		e.planKind, e.planMsg = planusage.Unreadable, err.Error()
		e.mu.Unlock()
		return nil, planusage.Unreadable, err.Error()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	got, err := planusage.Fetch(ctx, token)
	if err != nil {
		e.cfg.Logger.Debug("cursor plan usage unavailable", "err", err)
		e.mu.Lock()
		e.planKind, e.planMsg = "unavailable", err.Error()
		e.mu.Unlock()
		return nil, "unavailable", err.Error()
	}
	e.mu.Lock()
	e.plan, e.planKind, e.planMsg, e.planAt = got, planusage.Available, "", time.Now()
	e.mu.Unlock()
	return got, planusage.Available, ""
}

// Plan returns the last successful plan snapshot plus the current availability.
func (e *Engine) Plan() (*planusage.Snapshot, planusage.Availability, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.plan, e.planKind, e.planMsg
}

// AccountTotals is Cursor's own usage numbers summed across every agent it lists
// for this API key. This is the widest official view available: sdk.v1 has no
// account-wide usage or plan-limit RPC, so "account" here means "the agents
// Cursor will list for this key", and Sampled/Failed say how complete it is.
type AccountTotals struct {
	Agents       int
	Sampled      int
	Failed       int
	TotalTokens  int64
	InputTokens  int64
	OutputTokens int64
	Reasoning    int64
	CostCents    float64
	CostLabel    string
	Runs         int
	Truncated    bool
	Known        bool
	Err          string
}

// accountTTL bounds how often the footer refresh may fan out over agents.
func accountTTL() time.Duration {
	if v := os.Getenv("PI_CURSOR_ACCOUNT_TTL_S"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return 3 * time.Minute
}

// RefreshAccount re-reads account-wide usage when the cached copy has gone stale
// (or force is set), and returns the current picture. It runs on the caller's
// clock: call it from turn_end or a command, never mid-stream.
func (e *Engine) RefreshAccount(apiKey string, limit uint32, force bool) AccountTotals {
	e.mu.Lock()
	cached, fresh := e.account, time.Since(e.accountAt) < accountTTL()
	client := e.client
	e.mu.Unlock()
	if !force && fresh && cached.Known {
		return cached
	}
	if client == nil || apiKey == "" {
		return cached
	}

	// Not goCtx(ctx): a fan-out over dozens of agents is slower than one event
	// handler's life, and a canceled mid-flight read would leave the footer stuck
	// on whatever it had. Bounded, and independent of the caller's clock.
	rctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rep, err := client.AccountUsage(rctx, apiKey, limit)
	if err != nil {
		cached.Err = err.Error()
		e.cfg.Logger.Debug("account usage refresh failed", "err", err)
		return cached
	}
	out := AccountTotals{
		Agents: rep.Agents, Sampled: rep.Sampled, Failed: rep.Failed,
		TotalTokens: rep.TotalTokens, InputTokens: rep.InputTokens, OutputTokens: rep.OutputTokens,
		Reasoning: rep.Reasoning, CostCents: rep.CostCents, CostLabel: rep.CostLabel,
		Runs: rep.Runs, Truncated: rep.Truncated, Known: true,
	}
	e.mu.Lock()
	e.account, e.accountAt = out, time.Now()
	e.mu.Unlock()
	return out
}

// Account returns the cached account-wide picture without contacting Cursor.
func (e *Engine) Account() AccountTotals {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.account
}

// clientNow returns the live bridge client, or nil when none has been spawned.
func (e *Engine) clientNow() *cursor.Client {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.client
}

// Reported returns the merged usage report across every session this process has
// driven, for the footer and /cursor-usage.
func (e *Engine) Reported() UsageTotals {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out UsageTotals
	for _, s := range e.sessions {
		if !s.usage.Reported {
			continue
		}
		out.Reported = true
		out.TotalTokens += s.usage.TotalTokens
		out.InputTokens += s.usage.InputTokens
		out.OutputTokens += s.usage.OutputTokens
		out.Reasoning += s.usage.Reasoning
		out.CostCents += s.usage.CostCents
		if out.CostLabel == "" {
			out.CostLabel = s.usage.CostLabel
		}
		out.Runs += s.usage.Runs
	}
	return out
}

// reportUsage records what the turn cost. Cursor's per-agent totals are the
// authority (they cover every run in the agent); the run's own usage frame is the
// fallback when that RPC is unavailable, and it has no cost.
func (e *Engine) reportUsage(sess *Session, terminal *cursor.RunResult) {
	var snapshot UsageTotals
	if u := usageOf(terminal); u != nil {
		snapshot = UsageTotals{
			TotalTokens: orInt(u.TotalTokens, u.InputTokens+u.OutputTokens), InputTokens: u.InputTokens,
			OutputTokens: u.OutputTokens, Reasoning: u.ReasoningTokens, Reported: true, Runs: 1,
		}
	}

	client := e.clientNow()
	if client != nil && sess.AgentID != "" {
		// Deliberately not goCtx(ctx): the provider request is already finished at
		// this point, and its context is canceled — a usage call made on it fails
		// instantly with "context canceled" and the footer never fills in. The
		// agent outlives the request, so the call gets its own short deadline.
		uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rep, err := client.Usage(uctx, sess.AgentID)
		if err == nil && rep != nil {
			sess.usage = UsageTotals{
				TotalTokens:  orInt(rep.Tokens.TotalTokens, rep.Tokens.InputTokens+rep.Tokens.OutputTokens),
				InputTokens:  rep.Tokens.InputTokens,
				OutputTokens: rep.Tokens.OutputTokens,
				Reasoning:    rep.Tokens.ReasoningTokens,
				CostCents:    rep.CostCents,
				CostLabel:    rep.CostLabel,
				Runs:         rep.Runs,
				Reported:     true,
			}
			return
		} else if err != nil {
			e.cfg.Logger.Debug("cursor usage report unavailable", "err", err)
		}
	}
	if snapshot.Reported {
		sess.usage = snapshot
	}
}

func usageOf(terminal *cursor.RunResult) *cursor.Usage {
	if terminal == nil {
		return nil
	}
	return terminal.Usage
}

func orInt(v int64, fallback int64) int64 {
	if v != 0 {
		return v
	}
	return fallback
}

// NewEngine builds an engine; nothing is spawned until the first request.
func NewEngine(cfg Config) *Engine {
	if cfg.Provider == "" {
		cfg.Provider = "cursor"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	nonce := make([]byte, 6)
	if _, err := rand.Read(nonce); err != nil {
		// Only ever used to keep two throwaway sessions apart.
		nonce = []byte(strconv.FormatInt(time.Now().UnixNano(), 10))
	}
	return &Engine{
		cfg:      cfg,
		sessions: map[string]*Session{},
		agents:   newAgentRegistry(DefaultStateDir(cfg.ConfigHome)),
		nonce:    hex.EncodeToString(nonce),
	}
}

// SetCatalog installs a model catalog.
func (e *Engine) SetCatalog(c *Catalog) {
	e.mu.Lock()
	e.catalog = c
	e.mu.Unlock()
}

// Catalog returns the installed catalog.
func (e *Engine) Catalog() *Catalog {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.catalog
}

// Client returns the running bridge client, starting it on first use.
func (e *Engine) Client(ctx context.Context, apiKey string) (*cursor.Client, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.client != nil {
		return e.client, nil
	}
	if e.startErr != "" {
		return nil, errors.New(e.startErr)
	}
	bin := e.cfg.BridgeBin
	if bin == "" {
		bin = cursor.FindBinary()
	}
	if !e.reaped {
		// Bridges only serve the process that started them. A session that was
		// killed leaves one holding a port and a live Cursor run, which makes that
		// agent refuse every later send — clear those before starting ours.
		if n := bridge.ReapStale(DefaultStateDir(e.cfg.ConfigHome), e.cfg.Logger); n > 0 && e.cfg.Logger != nil {
			e.cfg.Logger.Info("reaped bridges left by dead sessions", "count", n)
		}
		e.reaped = true
	}
	started := time.Now()
	c, err := cursor.New(ctx, cursor.Options{
		BridgeBin: bin,
		Workspace: e.cfg.Workspace,
		APIKey:    apiKey,
		Logger:    e.cfg.Logger,
	})
	if err != nil {
		e.startErr = err.Error()
		return nil, err
	}
	e.cfg.Logger.Info("cursor bridge started", "ms", time.Since(started).Milliseconds(), "pid", c.PID())
	e.client = c
	return c, nil
}

// Stop shuts the bridge down.
func (e *Engine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range e.sessions {
		if s.agent != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = s.agent.Close(ctx)
			cancel()
			s.agent = nil
		}
	}
	if e.client != nil {
		e.client.Close()
		e.client = nil
	}
}

// BridgePID reports the live bridge pid, 0 when none.
func (e *Engine) BridgePID() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.client == nil {
		return 0
	}
	return e.client.PID()
}

// Stream is the provider implementation pig calls for every model turn.
func (e *Engine) Stream(ctx sdk.Context, model, piContext, options map[string]any) (*sdk.ModelEventStream, error) {
	stream := sdk.CreateAssistantMessageEventStream()
	go e.pump(ctx, stream, model, piContext, options)
	return stream, nil
}

func (e *Engine) pump(ctx sdk.Context, stream *sdk.ModelEventStream, model, piContext, options map[string]any) {
	debugStream := os.Getenv("PI_CURSOR_GO_DEBUG") == "1"
	if debugStream {
		e.cfg.Logger.Debug("streamSimple args", "optionsKeys", sortedKeys(options), "contextKeys", sortedKeys(piContext), "modelKeys", sortedKeys(model))
	}

	modelID := firstString(model, "id")
	entry := e.Catalog().Lookup(modelID)
	if entry == nil {
		e.fail(stream, model, modelID, fmt.Errorf("unknown Cursor model %q; run /cursor-refresh-models", modelID))
		return
	}
	piCtx := ParsePiContext(piContext)
	if len(piCtx.Tools) == 0 {
		// streamSimple's context carries only the transcript on some hosts, so
		// the advertised tool set comes from the host's tool registry instead.
		piCtx.Tools = hostTools(ctx)
	}
	apiKey := e.resolveAPIKey(ctx, options)
	if apiKey == "" {
		e.fail(stream, model, modelID, errors.New("no Cursor API key. Run /cursor-login to store one, or set CURSOR_API_KEY. The key comes from cursor.com/dashboard -> API Keys; signing in to Cursor Desktop does not provide it."))
		return
	}

	e.cfg.Logger.Debug("turn inputs", "keyLen", len(apiKey), "tools", len(piCtx.Tools), "messages", len(piCtx.Messages), "model", modelID)
	client, err := e.Client(goCtx(ctx), apiKey)
	if err != nil {
		e.fail(stream, model, modelID, err)
		return
	}
	if _, err := client.ProtocolVersion(goCtx(ctx)); err != nil {
		e.fail(stream, model, modelID, err)
		return
	}

	key := e.sessionKey(ctx, entry)
	e.mu.Lock()
	sess := e.sessions[key]
	if sess == nil {
		sess = &Session{Key: key, modelID: entry.ID}
		e.sessions[key] = sess
		// A pig session resumed in a new process has no live agent here. The
		// recorded id lets Cursor restore its own run state instead of replaying
		// the whole transcript — unless another process still holds that agent, in
		// which case taking it would collide with a turn in progress.
		if id, free := e.agents.lookup(key); id != "" {
			if free {
				sess.AgentID = id
			} else {
				e.cfg.Logger.Info("another live session owns this conversation's cursor agent; using a separate one", "agent", id)
			}
		}
	}
	e.mu.Unlock()

	// A turn that carries results for our parked host tool calls is a
	// continuation: resolve them and keep streaming the same run. Nothing new
	// is sent to Cursor, so the model sees its tool output and continues.
	if resume := e.resumeParked(ctx, sess, client, apiKey, entry, piCtx, debugStream); resume != nil {
		e.consume(ctx, stream, model, entry, sess, resume, debugStream, piCtx.Tools)
		return
	}

	// One retry, for the case where the agent restored from disk still owns an
	// upstream run this process cannot release.
	for attempt := 0; attempt < 2; attempt++ {
		run, err := e.startTurn(ctx, sess, client, apiKey, entry, piCtx, debugStream)
		if err != nil {
			if attempt == 0 && isStaleAgentError(err) {
				// Release the leftover run and try the same agent again: keeping it
				// keeps Cursor's side of the conversation, which a reset throws away.
				if e.releaseStaleRuns(ctx, sess) {
					sess.undoSend()
					continue
				}
				e.resetSession(ctx, sess)
				continue
			}
			e.fail(stream, model, modelID, err)
			return
		}
		out := e.consume(ctx, stream, model, entry, sess, run, debugStream, piCtx.Tools)
		if attempt == 0 && (out.retry || out.recover) {
			if out.recover {
				// The agent is usable now; the transcript it never received goes
				// back on the wire, and nothing else has to change.
				sess.undoSend()
			} else {
				e.resetSession(ctx, sess)
			}
			continue
		}
		return
	}
}

// resetSession drops the session's Cursor agent so the next attempt starts from
// a clean agent and replays the transcript.
func (e *Engine) resetSession(ctx sdk.Context, sess *Session) {
	e.mu.Lock()
	agent := sess.agent
	sess.agent = nil
	sess.AgentID = ""
	sess.pending = nil
	sess.sentFingerprints = nil
	sess.overrideSig = ""
	e.agents.forget(sess.Key)
	e.mu.Unlock()
	if agent != nil {
		if err := agent.Close(goCtx(ctx)); err != nil {
			e.cfg.Logger.Debug("closing stale cursor agent failed", "err", err)
		}
	}
}

// resumeParked resolves parked host calls from the transcript's tool results
// and returns the run to keep streaming, or nil when this is not a
// continuation turn.
func (e *Engine) resumeParked(ctx sdk.Context, sess *Session, client *cursor.Client, apiKey string, entry *ModelEntry, piCtx PiContext, debug bool) *cursor.Run {
	if sess.agent == nil {
		return nil
	}
	run := sess.agent.Active()
	if run == nil {
		return nil
	}
	parked := run.PendingHostCalls()
	if len(parked) == 0 {
		// pig moved on without resolving our calls (abort, compaction, edit).
		// Release them so the bridge is not left holding a dead callback.
		if !run.Finished() {
			if debug {
				e.cfg.Logger.Debug("no parked calls and run idle; letting the run finish")
			}
		}
		return nil
	}
	// pig knows these calls by the host-safe id we handed it, not by Cursor's.
	ids := make([]string, 0, len(parked))
	for _, p := range parked {
		ids = append(ids, p.HostID())
	}
	got := piCtx.PendingToolResults(ids)
	if len(got) == 0 {
		return nil
	}
	for _, p := range parked {
		out, ok := got[p.HostID()]
		if !ok {
			// A batch may still be in flight; resolve what we have and keep
			// waiting for the rest on the next turn.
			continue
		}
		run.ResolveHostCall(p.ID, map[string]any{"content": out.Text}, out.IsError, out.Text)
		if debug {
			e.cfg.Logger.Debug("resolved parked host call", "id", p.HostID(), "tool", p.ToolName, "bytes", len(out.Text))
		}
	}
	// Drop the calls we resolved from the session ledger.
	var keep []pendingRef
	for _, pr := range sess.pending {
		if _, ok := got[pr.ID]; !ok {
			keep = append(keep, pr)
		}
	}
	sess.pending = keep

	// The stream ended when the run parked; the durable stream continues it.
	if run.StreamClosedForPause() {
		if err := run.Resume(goCtx(ctx)); err != nil {
			e.cfg.Logger.Error("cursor run resume failed", "err", err)
		}
	}
	return run
}

// pausedOnHostCall is exported through the cursor package as PausedOnHostCall.

// currentOverride merges the installed model controls with the host's current
// thinking level.
func (e *Engine) currentOverride(ctx sdk.Context) ModelOverride {
	e.mu.Lock()
	ov := e.override
	e.mu.Unlock()
	if thinking := thinkingFromContext(ctx); thinking != "" {
		ov.Thinking = thinking
	}
	return ov
}

// startTurn decides between an append-only continuation and a replay, then
// sends the new user turn.
func (e *Engine) startTurn(ctx sdk.Context, sess *Session, client *cursor.Client, apiKey string, entry *ModelEntry, piCtx PiContext, debug bool) (*cursor.Run, error) {
	tools := piCtx.Tools
	if e.cfg.DisableHostTools {
		tools = nil
	}
	toolSig := toolSignature(tools)

	ov := e.currentOverride(ctx)
	if overrideSignature(entry, ov) != sess.overrideSig {
		// The model controls changed (thinking level, fast mode): the live
		// agent's parameter selections no longer match, so start a new one.
		if sess.agent != nil {
			e.cfg.Logger.Debug("model controls changed; agent will be recreated")
			_ = sess.agent.Close(goCtx(ctx))
			sess.agent = nil
			sess.AgentID = ""
		}
		sess.overrideSig = overrideSignature(entry, ov)
	}

	// Which agent this conversation is on, and how it was obtained: two sessions
	// in one directory share the registry and Cursor's per-project agent store, so
	// this line is what turns "the other session broke" into a traceable event.
	agentKind := "live"
	switch {
	case sess.agent == nil && sess.AgentID != "":
		agentKind = "resume"
	case sess.agent == nil:
		agentKind = "create"
	}
	e.cfg.Logger.Debug("agent decision", "key", registryKey(sess.Key)[:8], "kind", agentKind, "agent", sess.AgentID)

	agent := sess.agent
	if agent != nil && sess.toolSig != toolSig {
		// The tool set changed; the agent's advertised tools must change with it.
		agent.SetTools(toolSpecs(tools))
		sess.toolSig = toolSig
	}

	newText, appended := sess.appendedText(piCtx)
	if agent == nil {
		spec := AgentSpecFor(entry, e.cfg, apiKey, tools, ov)
		var err error
		if sess.AgentID != "" {
			agent, err = client.ResumeAgent(goCtx(ctx), sess.AgentID, spec)
			if err != nil {
				e.cfg.Logger.Info("cursor agent resume failed; starting a fresh agent", "err", err)
				sess.AgentID = ""
			}
		}
		if agent == nil {
			agent, err = client.CreateAgent(goCtx(ctx), spec)
			if err != nil {
				return nil, err
			}
		}
		sess.agent = agent
		sess.toolSig = toolSig
		sess.AgentID = agent.ID()
		e.agents.remember(sess.Key, agent.ID())
		sess.sentFingerprints = nil
		// A new agent needs the conversation so far.
		prompt := piCtx.ReplayPrompt(2000)
		if strings.TrimSpace(newText) != "" {
			prompt = prompt + "\n\nMOST RECENT USER REQUEST (act on this):\n" + newText
		}
		if debug {
			e.cfg.Logger.Debug("replaying transcript to a fresh agent", "bytes", len(prompt), "messages", len(piCtx.Messages))
		}
		run, err := agent.Send(goCtx(ctx), prompt, images(piCtx.Images()))
		if err != nil {
			// The new agent never received the conversation, so keeping it would make
			// a retry send only the newest message into an empty agent. Drop it: the
			// retry replays, which is what a fresh agent needs.
			_ = agent.Close(goCtx(ctx))
			sess.agent = nil
			sess.AgentID = ""
			sess.sentFingerprints = nil
			e.agents.forget(sess.Key)
			return nil, err
		}
		sess.sentFingerprints = fingerprints(piCtx)
		return run, nil
	}

	if !appended {
		// The branch was rewritten (edit, branch switch, compaction): the live
		// agent's memory no longer matches pig's transcript.
		e.cfg.Logger.Info("cursor conversation diverged; restarting the agent for this branch")
		if err := agent.Close(goCtx(ctx)); err != nil && debug {
			e.cfg.Logger.Debug("close diverged agent", "err", err)
		}
		sess.agent = nil
		sess.AgentID = ""
		e.agents.forget(sess.Key)
		return e.startTurn(ctx, sess, client, apiKey, entry, piCtx, debug)
	}

	sess.prevFingerprints = sess.sentFingerprints
	sess.sentFingerprints = fingerprints(piCtx)
	return agent.Send(goCtx(ctx), newText, images(piCtx.Images()))
}

// consume streams assistant prose, thinking, marked inline annotations, and real
// host tool calls. Cursor-internal tools never become executable toolCall blocks.
// turnOutcome reports whether the caller should retry the turn with a clean
// session. Only a failure that happens before any content reached the host can
// be retried, so a retry never duplicates a partially streamed message.
type turnOutcome struct {
	retry bool
	// recover means the same agent became usable again (a leftover run was
	// cancelled), so the turn is re-sent without losing Cursor's context.
	recover bool
}

func (e *Engine) consume(ctx sdk.Context, stream *sdk.ModelEventStream, model map[string]any, entry *ModelEntry, sess *Session, run *cursor.Run, debug bool, tools []ToolDef) turnOutcome {
	em := newTurnEmitter(model, entry)
	em.sink = func(ev map[string]any) { stream.Push(ev) }
	em.hostToolSet = toolNameSet(tools)

	// Annotations are streamed inline; the entry renderer is only for old history.
	defer func() {
		ctx.ClearStatus("cursor-activity")
		if e.cfg.LiveTools != nil && ctx.HasUI() {
			e.cfg.LiveTools(ctx, nil)
		}
	}()

	em.begin()

	turnCtx, cancelTurn := context.WithTimeout(goCtx(ctx), e.turnDeadline())
	defer cancelTurn()

	// A host tool call ends the turn — but Cursor may batch several calls in one
	// turn, and pig runs every call we hand back. So after the first call keep
	// reading for a short quiet window to catch the rest of the batch, then park.
	// Dropping the tail leaves Cursor waiting for a result that never arrives.
	hosted := false
	grace := e.batchGrace()
	for {
		var ev cursor.RunEvent
		var ok bool
		if hosted {
			ev, ok = run.NextWithin(turnCtx, grace)
		} else {
			ev, ok = run.Next(turnCtx)
		}
		if !ok {
			break
		}
		if debug {
			e.cfg.Logger.Debug("run event", "kind", string(ev.Kind), "bytes", len(ev.Text), "resumed", run.IsResumed())
		}
		if msg, failed := em.statusFailure(ev); failed {
			e.failWith(stream, em.p, model, errors.New(msg))
			return turnOutcome{}
		}
		if em.add(ev) {
			hosted = true
			continue
		}
		if ev.Kind == cursor.EventCursorToolStarted || ev.Kind == cursor.EventCursorToolCompleted {
			live := em.cursorTools.snapshot()
			if len(live) > 0 {
				ctx.SetStatus("cursor-activity", cursorToolHint(live[0].Tool, live[0].Summary, 0))
			} else {
				ctx.ClearStatus("cursor-activity")
			}
			if e.cfg.LiveTools != nil && ctx.HasUI() {
				e.cfg.LiveTools(ctx, live)
			}
		}
		if hosted && ev.Kind == cursor.EventStatus && ev.Status != nil && ev.Status.Status == "tool_pending" {
			// Cursor is blocked on our callbacks now: the batch is complete.
			break
		}
	}
	if hosted {
		// This turn is over: pig runs the tools and calls back with the results,
		// which resumes the run. No stream is kept open in between.
		run.PauseStream()
	}

	if err := run.DrainErr(); err != nil {
		if isStaleAgentError(err) && !em.sawContent() {
			// The agent we restored from disk still owns a run upstream that this
			// process cannot see — typical after an interrupted session. Show no
			// error either way; the question is only how much is thrown away.
			if e.releaseStaleRuns(ctx, sess) {
				// A dead session's run, cancelled: this agent and everything
				// Cursor remembers about this conversation survive.
				return turnOutcome{recover: true}
			}
			e.cfg.Logger.Info("restored cursor agent still holds an upstream run; starting a fresh one", "err", err)
			return turnOutcome{retry: true}
		}
		e.failWith(stream, em.p, model, err)
		return turnOutcome{}
	}
	if turnCtx.Err() == context.DeadlineExceeded && !run.Finished() {
		// A wedged upstream must not wedge the host: release the run and say so.
		_ = run.CancelRun("turn deadline exceeded")
		e.failWith(stream, em.p, model, fmt.Errorf("Cursor did not respond within %s; the run was released. Try again, or /cursor-abort to clean up.", e.turnDeadline()))
		return turnOutcome{}
	}

	terminal := run.Terminal()
	if len(em.tools) == 0 && terminal != nil && terminal.Status == "ERROR" {
		msg := terminal.ErrorMessage
		if msg == "" {
			msg = "Cursor run failed"
		}
		e.failWith(stream, em.p, model, errors.New(msg))
		return turnOutcome{}
	}

	reason := "stop"
	if terminal != nil && terminal.Status == "CANCELLED" {
		reason = "aborted"
	}
	reason = em.finish(reason, terminal)
	sess.lastUsage = em.p.usage
	if len(em.pending) > 0 {
		sess.pending = em.pending
	}
	stream.End(em.p.msg)
	// Cost accounting runs after the host has its answer: a slow or unsupported
	// usage RPC must never delay or break a turn.
	e.reportUsage(sess, terminal)
	if debug {
		e.cfg.Logger.Debug("turn complete", "reason", reason, "events", len(em.events()), "tools", len(em.tools),
			"tokens", sess.usage.TotalTokens, "cents", sess.usage.CostCents)
	}
	return turnOutcome{}
}

// isStaleAgentError recognises Cursor refusing a send because an agent restored
// from a previous process still owns a run upstream.
// isStaleAgentError reports whether the agent is blocked by a run nobody in this
// process is reading. Two wordings reach us: the bridge's ("already has active
// run") and this package's own guard on a leftover run ("run … is still active
// for this agent"). Both mean the same thing and have the same cure.
func isStaleAgentError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "already has active run") || strings.Contains(msg, "is still active for this agent")
}

// releaseStaleRuns cancels the runs another session left live on this agent, so
// the same agent can be used again with Cursor's side of the conversation intact.
// False means the caller has to start a fresh agent and replay instead.
func (e *Engine) releaseStaleRuns(ctx sdk.Context, sess *Session) bool {
	if sess == nil || sess.agent == nil {
		return false
	}
	n, err := sess.agent.ReleaseStaleRuns(goCtx(ctx))
	if err != nil {
		e.cfg.Logger.Debug("could not release runs left on this cursor agent", "err", err)
		return false
	}
	if n == 0 {
		return false
	}
	e.cfg.Logger.Info("cancelled runs left on this cursor agent", "runs", n, "agent", sess.AgentID)
	return true
}

// appendedText reports the new user text when pig's transcript extends what we
// already sent, and false when the branch diverged.
// undoSend restores the transcript bookkeeping after a send the agent never
// received, so the retry re-sends that text instead of concluding the branch was
// rewritten and rebuilding the agent from scratch.
func (s *Session) undoSend() {
	s.sentFingerprints = s.prevFingerprints
	s.prevFingerprints = nil
}

func (s *Session) appendedText(piCtx PiContext) (string, bool) {
	fps := fingerprints(piCtx)
	if len(s.sentFingerprints) == 0 {
		return piCtx.UserText(), len(fps) > 0
	}
	if len(fps) < len(s.sentFingerprints) {
		return "", false
	}
	for i, fp := range s.sentFingerprints {
		if fps[i] != fp {
			return "", false
		}
	}
	txt := piCtx.UserText()
	if strings.TrimSpace(txt) == "" {
		return "", false
	}
	return txt, true
}

func fingerprints(piCtx PiContext) []string {
	out := make([]string, 0, len(piCtx.Messages))
	for _, m := range piCtx.Messages {
		out = append(out, m.Fingerprint())
	}
	return out
}

func (e *Engine) sessionKey(ctx sdk.Context, entry *ModelEntry) string {
	cwd := ctx.Cwd()
	if cwd == "" {
		cwd = e.cfg.Workspace
	}
	file := ""
	if f, err := ctx.GetSessionFile(); err == nil && f != nil {
		file = *f
	}
	return conversationKey(file, cwd, entry.BaseID, e.nonce)
}

// conversationKey identifies one conversation for the purpose of reusing a Cursor
// agent. A session file is durable identity; without one, the per-process nonce is
// what keeps two throwaway runs apart. They must be apart: with no file, every run
// in one directory would otherwise compute the same key, fight over one agent, get
// told "already has active run", and have that agent closed under it when the other
// run exits.
func conversationKey(sessionFile, cwd, baseID, nonce string) string {
	file := sessionFile
	if strings.TrimSpace(file) == "" {
		file = "ephemeral:" + nonce
	}
	return strings.Join([]string{file, cwd, baseID}, "\x00")
}

func (e *Engine) resolveAPIKey(ctx sdk.Context, options map[string]any) string {
	if k := firstString(options, "apiKey", "api_key"); k != "" {
		return k
	}
	if auth, err := ctx.GetModelAuth(e.cfg.Provider, ""); err == nil && len(auth) > 0 {
		var m map[string]any
		if json.Unmarshal(auth, &m) == nil {
			if k := firstString(m, "apiKey", "key"); k != "" {
				return k
			}
		}
	}
	if k := ctx.ModelRegistry().GetApiKeyForProvider(e.cfg.Provider); k != nil && *k != "" {
		return *k
	}
	return os.Getenv("CURSOR_API_KEY")
}

// hostTools reads the active tool names and their schemas from the host
// registry. pig's streamSimple context does not always carry the tool list, and
// the advertised set must match what pig can actually execute.
func hostTools(ctx sdk.Context) []ToolDef {
	active, err := ctx.GetActiveTools()
	if err != nil || len(active) == 0 {
		return nil
	}
	all, err := ctx.GetAllTools()
	if err != nil {
		return nil
	}
	byName := make(map[string]ToolDef, len(all))
	for _, t := range all {
		var schema map[string]any
		if len(t.Parameters) > 0 {
			_ = json.Unmarshal(t.Parameters, &schema)
		}
		byName[t.Name] = ToolDef{Name: t.Name, Description: t.Description, Schema: schema}
	}
	out := make([]ToolDef, 0, len(active))
	for _, name := range active {
		if t, ok := byName[name]; ok {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func images(refs []ImageRef) []cursor.Image {
	out := make([]cursor.Image, 0, len(refs))
	for _, r := range refs {
		out = append(out, cursor.Image{Data: r.Data, MIMEType: r.MIMEType})
	}
	return out
}

// AgentSpecFor builds the Cursor agent options for one model selection.
func AgentSpecFor(entry *ModelEntry, cfg Config, apiKey string, tools []ToolDef, ov ModelOverride) cursor.AgentSpec {
	params := applyOverrides(entry, ov)
	spec := cursor.AgentSpec{
		APIKey:         apiKey,
		Model:          entry.BaseID,
		ModelParams:    toParams(params),
		Cwd:            cfg.Workspace,
		Tools:          toolSpecs(tools),
		Mode:           cfg.Mode,
		SettingSources: cfg.SettingSources,
	}
	return spec
}

// applyOverrides maps pig's controls onto the model's parameter selections.
func applyOverrides(entry *ModelEntry, ov ModelOverride) []Param {
	params := entry.Params
	if ov.HasFast && entry.SupportsFast {
		value := "false"
		if ov.Fast {
			value = "true"
		}
		params = replaceParam(params, "fast", value)
	}
	if ov.Thinking != "" && entry.ThinkingParamID != "" {
		if mapped, ok := entry.ThinkingLevels[strings.ToLower(ov.Thinking)]; ok {
			params = replaceParam(params, entry.ThinkingParamID, mapped)
		}
	}
	return params
}

func overrideSignature(entry *ModelEntry, ov ModelOverride) string {
	params := applyOverrides(entry, ov)
	parts := make([]string, 0, len(params))
	for _, p := range params {
		parts = append(parts, p.ID+"="+p.Value)
	}
	return strings.Join(parts, ",")
}

// thinkingFromContext reads pig's current thinking level, when the host exposes it.
func thinkingFromContext(ctx sdk.Context) string {
	if level, err := ctx.GetThinkingLevel(); err == nil {
		return level
	}
	return ""
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// goCtx adapts the host request context to a Go context: it is cancelled when
// the host cancels the request, and always carries a deadline-free background
// parent so a completed provider call never cancels the upstream run.
func goCtx(ctx sdk.Context) context.Context {
	cancelled, cancel := context.WithCancel(context.Background())
	done := ctx.Done()
	if done == nil {
		cancel()
		return cancelled
	}
	go func() {
		defer cancel()
		select {
		case <-done:
		case <-cancelled.Done():
		}
	}()
	return cancelled
}

func toParams(params []Param) []cursor.ModelParam {
	out := make([]cursor.ModelParam, 0, len(params))
	for _, p := range params {
		out = append(out, cursor.ModelParam{ID: p.ID, Value: p.Value})
	}
	return out
}

// toolSpecs advertises the host's own tool names and schemas, so a Cursor tool
// call maps straight onto a tool pig can execute and render.
func toolSpecs(tools []ToolDef) []cursor.ToolSpec {
	out := make([]cursor.ToolSpec, 0, len(tools))
	for _, t := range tools {
		if t.Name == "" {
			continue
		}
		out = append(out, cursor.ToolSpec{Name: t.Name, Description: t.Description, InputSchema: t.Schema})
	}
	return out
}

func toolSignature(tools []ToolDef) string {
	var b strings.Builder
	for _, t := range tools {
		raw, _ := json.Marshal(t.Schema)
		fmt.Fprintf(&b, "%s:%d;", t.Name, len(raw))
	}
	return fmt.Sprintf("%x", sha256Short(b.String()))
}

func sha256Short(s string) uint64 {
	var h uint64 = 1469598103934665603
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---- partial assistant message -------------------------------------------

type partial struct {
	msg          map[string]any
	content      []any
	events       []map[string]any
	usage        *cursor.Usage
	textOpen     bool
	textStarted  bool
	textIndex    int
	thinkOpen    bool
	thinkStarted bool
	thinkIndex   int
	lastTool     map[string]any
	modelID      string
}

func newPartial(model map[string]any, entry *ModelEntry) *partial {
	now := time.Now().UnixMilli()
	return &partial{
		msg: map[string]any{
			"role":       "assistant",
			"content":    []any{},
			"api":        "pig-cursor-sdk",
			"provider":   "cursor",
			"model":      firstString(model, "id"),
			"stopReason": "",
			"timestamp":  now,
			"usage":      usageMap(nil),
		},
		modelID: entry.ID,
	}
}

// block returns the content block at an index, nil when out of range.
func (p *partial) block(i int) any {
	if i < 0 || i >= len(p.content) {
		return nil
	}
	return p.content[i]
}

func (p *partial) openText() int {
	if p.textOpen {
		return p.textIndex
	}
	block := map[string]any{"type": "text", "text": ""}
	p.content = append(p.content, block)
	p.msg["content"] = p.content
	p.textOpen, p.textIndex = true, len(p.content)-1
	return p.textIndex
}

func (p *partial) openThinking() int {
	if p.thinkOpen {
		return p.thinkIndex
	}
	block := map[string]any{"type": "thinking", "thinking": ""}
	p.content = append(p.content, block)
	p.msg["content"] = p.content
	p.thinkOpen, p.thinkIndex = true, len(p.content)-1
	return p.thinkIndex
}

func (p *partial) openToolCall(id, name string, args map[string]any) map[string]any {
	if args == nil {
		args = map[string]any{}
	}
	block := map[string]any{"type": "toolCall", "id": id, "name": name, "arguments": args}
	p.content = append(p.content, block)
	p.msg["content"] = p.content
	p.lastTool = block
	return block
}

func (p *partial) appendText(delta string) {
	if p.textIndex < len(p.content) {
		if b, ok := p.content[p.textIndex].(map[string]any); ok {
			b["text"] = firstString(b, "text") + delta
		}
	}
}

func (p *partial) appendThinking(delta string) {
	if b, ok := p.content[p.thinkIndex].(map[string]any); ok {
		b["thinking"] = firstString(b, "thinking") + delta
	}
}

// joinedText returns the assistant text of the message so far.
func (p *partial) joinedText() string {
	var b strings.Builder
	for _, blk := range p.content {
		m, _ := blk.(map[string]any)
		if m != nil && firstString(m, "type") == "text" {
			b.WriteString(firstString(m, "text"))
		}
	}
	return b.String()
}

func (p *partial) hasNoText() bool {
	for _, b := range p.content {
		if m, _ := b.(map[string]any); m != nil && firstString(m, "type") == "text" && firstString(m, "textSignature") != cursorToolTextSignature && firstString(m, "text") != "" {
			return false
		}
	}
	return true
}

func (p *partial) setStop(reason string, usage *cursor.Usage) {
	p.msg["stopReason"] = reason
	p.msg["usage"] = usageMap(usage)
}

func contentIndex(msg map[string]any, block map[string]any) int {
	items, _ := msg["content"].([]any)
	for i, it := range items {
		if m, _ := it.(map[string]any); m != nil {
			if id1, _ := m["id"].(string); id1 != "" {
				if id2, _ := block["id"].(string); id1 == id2 {
					return i
				}
				continue
			}
			if firstString(m, "type") == firstString(block, "type") && m["text"] == block["text"] {
				return i
			}
		}
	}
	return len(items) - 1
}

// usageMap renders pig's usage object from Cursor's counters. Cursor does not
// publish a dollar figure per run here, so cost is reported as an estimate of
// zero with totalFromProvider false, which is what pig's accounting expects for
// a provider that does not bill in tokens.
func usageMap(u *cursor.Usage) map[string]any {
	m := map[string]any{
		"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0,
		"totalTokens": 0, "reasoning": 0,
		"cost": map[string]any{
			"input": 0.0, "output": 0.0, "cacheRead": 0.0, "cacheWrite": 0.0,
			"total": 0.0, "totalFromProvider": false,
		},
	}
	if u == nil {
		return m
	}
	m["input"] = u.InputTokens
	m["output"] = u.OutputTokens
	m["cacheRead"] = u.CacheReadTokens
	m["cacheWrite"] = u.CacheWriteToks
	m["totalTokens"] = u.TotalTokens
	m["reasoning"] = u.ReasoningTokens
	total := u.TotalTokens
	if total == 0 {
		total = u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteToks
	}
	m["totalTokens"] = total
	return m
}

func (e *Engine) fail(stream *sdk.ModelEventStream, model map[string]any, modelID string, err error) {
	e.noteRate(err)
	e.cfg.Logger.Error("cursor provider request failed", "model", modelID, "err", err)
	p := newPartial(model, &ModelEntry{ID: modelID})
	p.msg["stopReason"] = "error"
	p.msg["errorMessage"] = e.humanError(err)
	// reason must be one of pig's stop reasons; the classification belongs in the
	// message, not in a field the host validates against a fixed set.
	stream.Push(map[string]any{"type": "error", "reason": "error", "error": p.msg})
	stream.End(p.msg)
}

// noteRate remembers a usage window Cursor attached to a refusal.
func (e *Engine) noteRate(err error) {
	if r := cursor.RateOf(err); r != nil {
		e.mu.Lock()
		e.lastRate = r
		e.mu.Unlock()
	}
}

// LimitWindow returns the usage window Cursor last reported, or nil when it never
// reported one or the reported window has already reset.
func (e *Engine) LimitWindow() *cursor.RateLimit {
	e.mu.Lock()
	r := e.lastRate
	e.mu.Unlock()
	if r == nil {
		return nil
	}
	if r.HasReset && time.Now().After(r.ResetAt) {
		return nil
	}
	return r
}

// humanError turns a provider failure into one actionable line. A fresh install
// is the most likely cause of a startup failure, so it names the path to fix.
func (e *Engine) humanError(err error) string {
	msg := err.Error()
	if r := cursor.RateOf(err); r != nil {
		if sum := r.Summary(time.Now()); sum != "" {
			return msg + " — Cursor usage window: " + sum
		}
	}
	if strings.Contains(msg, "bridge binary not found") {
		return "Cursor bridge is not installed. Put the cursor-sdk-bridge executable at " +
			cursor.InstallPath() + " (see the repository README), or point CURSOR_SDK_BRIDGE_BIN at it. Original error: " + msg
	}
	return msg
}

func (e *Engine) failWith(stream *sdk.ModelEventStream, p *partial, model map[string]any, err error) {
	e.cfg.Logger.Error("cursor run failed", "err", err)
	p.msg["stopReason"] = "error"
	p.msg["errorMessage"] = err.Error()
	stream.Push(map[string]any{"type": "error", "reason": "error", "error": p.msg})
	stream.End(p.msg)
}

// toolNameSet is the set of tool names advertised to Cursor for this turn.
func toolNameSet(tools []ToolDef) map[string]bool {
	if len(tools) == 0 {
		return nil
	}
	out := make(map[string]bool, len(tools))
	for _, t := range tools {
		out[t.Name] = true
	}
	return out
}

// turnDeadline bounds one provider turn. A Cursor run that stops producing
// events must not leave pig waiting forever behind a spinner.
// batchGrace is how long to keep the stream open after a host tool call to catch
// the rest of a batched turn.
func (e *Engine) batchGrace() time.Duration {
	if v := os.Getenv("PI_CURSOR_TOOL_BATCH_GRACE_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms >= 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return 900 * time.Millisecond
}

func (e *Engine) turnDeadline() time.Duration {
	if v := os.Getenv("PI_CURSOR_TURN_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 5 * time.Minute
}

func (e *Engine) closeText(push func(map[string]any), p *partial) {
	if !p.textOpen {
		return
	}
	block, _ := p.content[p.textIndex].(map[string]any)
	push(map[string]any{"type": "text_end", "contentIndex": p.textIndex, "content": firstString(block, "text")})
	p.textOpen = false
}

func (e *Engine) closeThinking(push func(map[string]any), p *partial) {
	if !p.thinkOpen {
		return
	}
	block, _ := p.content[p.thinkIndex].(map[string]any)
	push(map[string]any{"type": "thinking_end", "contentIndex": p.thinkIndex, "content": firstString(block, "thinking")})
	p.thinkOpen = false
}

// BridgePathFor reports the bridge binary the engine will use.
func BridgePathFor(cfg Config) string {
	if cfg.BridgeBin != "" {
		return cfg.BridgeBin
	}
	return cursor.FindBinary()
}

// DefaultStateDir is where Go-side Cursor state lives.
func DefaultStateDir(configHome string) string {
	return filepath.Join(configHome, "agent", "pig-cursor-sdk")
}

// Context is an alias used by callers that only need a Go context.
type Context = context.Context
