package cursor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"connectrpc.com/connect"
	sdkpb "github.com/tctien342/pig-cursor-sdk/gen/sdk/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// ToolSpec is one host-owned tool advertised to the Cursor agent. The name is
// the host tool's real name, so a Cursor tool call maps straight onto a tool
// the host can execute and render natively.
type ToolSpec struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// AgentSpec is what CreateAgent needs.
type AgentSpec struct {
	APIKey      string
	Model       string
	ModelParams []ModelParam
	Cwd         string
	Tools       []ToolSpec
	Mode        string // "" | "agent" | "plan"
	// DisableBuiltins sends tools:{names:[]} so only host tools exist. The
	// bridge currently drops the custom-tool mechanism with an empty set, so
	// this stays off by default; host tools still win because they carry the
	// host's own names.
	DisableBuiltins bool
	SettingSources  []string
	AutoReview      *bool
	Sandbox         *bool
}

// ModelParam is a model parameter selection (context window, fast mode, ...).
type ModelParam struct{ ID, Value string }

// Agent is a live Cursor agent that owns at most one run at a time.
type Agent struct {
	client *Client
	id     string
	spec   AgentSpec

	mu      sync.Mutex
	run     *Run
	closed  bool
	toolSet map[string]bool
}

// CreateAgent starts a Cursor local agent in the workspace and registers its
// host tools. The callback route for this agent id is installed before
// CreateAgent returns, so an immediate tool call cannot be dropped.
func (c *Client) CreateAgent(ctx context.Context, spec AgentSpec) (*Agent, error) {
	if spec.Model == "" {
		return nil, errors.New("cursor: local agents require an explicit model")
	}
	a := &Agent{client: c, spec: spec, toolSet: map[string]bool{}}
	for _, t := range spec.Tools {
		a.toolSet[t.Name] = true
	}

	res, err := c.Agent.CreateAgent(ctx, connect.NewRequest(&sdkpb.CreateAgentRequest{
		Options: a.options(spec),
	}))
	if err != nil {
		return nil, wrapErr("CreateAgent", err)
	}
	a.id = res.Msg.GetAgentId()
	c.bindAgent(a)
	return a, nil
}

// ResumeAgent attaches to an agent recorded by an earlier session.
func (c *Client) ResumeAgent(ctx context.Context, agentID string, spec AgentSpec) (*Agent, error) {
	a := &Agent{client: c, id: agentID, spec: spec, toolSet: map[string]bool{}}
	for _, t := range spec.Tools {
		a.toolSet[t.Name] = true
	}
	res, err := c.Agent.ResumeAgent(ctx, connect.NewRequest(&sdkpb.ResumeAgentRequest{
		AgentId: agentID,
		Options: a.options(spec),
	}))
	if err != nil {
		return nil, wrapErr("ResumeAgent", err)
	}
	if res.Msg.GetAgentId() != "" {
		a.id = res.Msg.GetAgentId()
	}
	c.bindAgent(a)
	return a, nil
}

// ID is the Cursor agent id.
func (a *Agent) ID() string { return a.id }

// Active returns the live run, if any.
func (a *Agent) Active() *Run {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.run
}

// SetTools replaces the advertised host tools for later sends.
func (a *Agent) SetTools(tools []ToolSpec) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.spec.Tools = tools
	a.toolSet = make(map[string]bool, len(tools))
	for _, t := range tools {
		a.toolSet[t.Name] = true
	}
}

func (a *Agent) options(spec AgentSpec) *sdkpb.AgentOptions {
	opts := &sdkpb.AgentOptions{
		Model:  &sdkpb.ModelSelection{Id: spec.Model},
		ApiKey: spec.APIKey,
		Local: &sdkpb.LocalAgentOptions{
			Cwd:         []string{spec.Cwd},
			CustomTools: map[string]*sdkpb.CustomToolDefinition{},
		},
	}
	for _, p := range spec.ModelParams {
		opts.Model.Params = append(opts.Model.Params, &sdkpb.ModelParameterValue{Id: p.ID, Value: p.Value})
	}
	for _, t := range spec.Tools {
		schema, err := structFromMap(t.InputSchema)
		if err != nil {
			schema, _ = structFromMap(map[string]any{"type": "object"})
		}
		desc := t.Description
		opts.Local.CustomTools[t.Name] = &sdkpb.CustomToolDefinition{Description: &desc, InputSchema: schema}
	}
	switch spec.Mode {
	case "plan":
		opts.Mode = sdkpb.AgentModeOption_AGENT_MODE_OPTION_PLAN
	default:
		opts.Mode = sdkpb.AgentModeOption_AGENT_MODE_OPTION_AGENT
	}
	if spec.DisableBuiltins {
		opts.Tools = &sdkpb.ToolList{Names: []string{}}
	}
	for _, s := range spec.SettingSources {
		if v, ok := settingSource(s); ok {
			opts.Local.SettingSources = append(opts.Local.SettingSources, v)
		}
	}
	if spec.AutoReview != nil {
		opts.Local.AutoReview = spec.AutoReview
	}
	if spec.Sandbox != nil {
		opts.Local.SandboxOptions = &sdkpb.SandboxOptions{Enabled: spec.Sandbox}
	}
	return opts
}

func settingSource(name string) (sdkpb.SettingSource, bool) {
	switch name {
	case "project":
		return sdkpb.SettingSource_SETTING_SOURCE_PROJECT, true
	case "user":
		return sdkpb.SettingSource_SETTING_SOURCE_USER, true
	case "team":
		return sdkpb.SettingSource_SETTING_SOURCE_TEAM, true
	case "mdm":
		return sdkpb.SettingSource_SETTING_SOURCE_MDM, true
	case "plugins":
		return sdkpb.SettingSource_SETTING_SOURCE_PLUGINS, true
	case "all":
		return sdkpb.SettingSource_SETTING_SOURCE_ALL, true
	}
	return 0, false
}

// SendText starts a run. The returned Run is owned by the caller's session,
// not by the provider call that started it: it keeps streaming after a provider
// turn ends, because a parked host tool call must survive the boundary.
func (a *Agent) Send(ctx context.Context, text string, images []Image) (*Run, error) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil, errors.New("cursor: agent is closed")
	}
	if a.run != nil && !a.run.Finished() {
		prev := a.run
		a.mu.Unlock()
		return nil, fmt.Errorf("cursor: run %s is still active for this agent", prev.RunID())
	}
	a.mu.Unlock()

	msg := &sdkpb.UserMessage{Text: text}
	for _, img := range images {
		msg.Images = append(msg.Images, &sdkpb.SdkImage{
			Source: &sdkpb.SdkImage_Data{Data: &sdkpb.SdkImageData{Data: img.Data, MimeType: img.MIMEType}},
		})
	}

	runCtx, cancel := context.WithCancel(context.Background())
	// streamCtx ends the read side only. Parking a host tool call closes the
	// live stream so the provider turn can finish, while runCtx keeps the run
	// itself alive for the durable resume.
	streamCtx, cancelStream := context.WithCancel(runCtx)
	stream, err := a.client.Agent.Send(streamCtx, connect.NewRequest(&sdkpb.SendRequest{
		AgentId: a.id,
		Message: msg,
		// No model override here: the agent already carries the selection with
		// its parameter values, and a bare override makes the bridge reject the
		// run. Model changes therefore create a new agent.
		Options: &sdkpb.SendOptions{
			EnableDeltas: true,
			EnableSteps:  true,
		},
	}))
	if err != nil {
		cancelStream()
		cancel()
		return nil, wrapErr("Send", err)
	}

	r := newRun(a, runCtx, cancel)
	r.cancelStream = cancelStream
	a.mu.Lock()
	a.run = r
	a.mu.Unlock()
	go r.drain(stream)
	return r, nil
}

// Close releases the agent and any parked callbacks.
func (a *Agent) Close(ctx context.Context) error {
	a.mu.Lock()
	run, closed := a.run, a.closed
	a.closed = true
	a.mu.Unlock()
	if closed {
		return nil
	}
	if run != nil {
		run.abandon(errors.New("cursor: agent closed"))
	}
	var err error
	if _, cerr := a.client.Agent.CloseAgent(ctx, connect.NewRequest(&sdkpb.CloseAgentRequest{AgentId: a.id})); cerr != nil {
		err = wrapErr("CloseAgent", cerr)
	}
	a.unroute()
	return err
}

func (a *Agent) unroute() { a.client.unbindAgent(a.id) }

// handleToolCallback parks until the host resolves the call. It runs on the
// bridge's HTTP request, so holding it open is what holds the Cursor run.
func (a *Agent) handleToolCallback(ctx context.Context, req ToolCallRequest) (map[string]any, error) {
	a.mu.Lock()
	run := a.run
	a.mu.Unlock()
	if run == nil {
		return nil, errors.New("cursor: no active run for this agent")
	}
	return run.park(ctx, req)
}

// IsHostTool reports whether a name was advertised as a host tool.
func (a *Agent) IsHostTool(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.toolSet[name]
}

// Image is one image attachment of a user message.
type Image struct {
	Data     string // base64
	MIMEType string
}

// ---- run ------------------------------------------------------------------

// ParkedCall is one host tool execution the Cursor run is waiting on.
type ParkedCall struct {
	ID        string
	ToolName  string
	Args      map[string]any
	ArrivedAt time.Time

	mu       sync.Mutex
	resolved bool
	result   map[string]any
	err      error
	done     chan struct{}
}

// Settled reports whether the call already has an outcome, and returns it.
func (p *ParkedCall) Settled() (map[string]any, error, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.result, p.err, p.resolved
}

// ID and name accessors for callers that only have the handle.
func (p *ParkedCall) Name() string { return p.ToolName }

// RunEvent is one item a provider turn consumes.
type RunEvent struct {
	Event
	// Pending is set for a host tool call the run is parked on: the provider
	// turn must hand it to the host and end, exactly like a normal provider
	// issuing a tool call.
	Pending *ParkedCall
}

// Run owns one Cursor run: its stream, its queue, and its parked callbacks.
type Run struct {
	agent *Agent
	ctx   context.Context
	c     context.CancelFunc

	queue *eventQueue

	mu       sync.Mutex
	runID    string
	agentID  string
	parked   map[string]*ParkedCall
	order    []string
	terminal *RunResult
	fin      bool
	drainErr error

	// emitted holds the host tool call ids already surfaced to the host, so a
	// durable replay cannot render the same tool card twice.
	emitted map[string]bool
	// resumed marks the durable ObserveRun stream; observeOffset is the last
	// offset seen on an ObserveRun stream, which is the only stream kind whose
	// offsets may be passed back as after_offset.
	resumed       bool
	barrierOpen   bool
	observeOffset string
	// cancelStream closes the live stream when the run parks on a host tool
	// call, so the provider turn can end while the run stays alive upstream.
	cancelStream context.CancelFunc
	paused       bool

	usage *Usage
}

const (
	maxQueuedEvents    = 20000
	defaultParkTimeout = 15 * time.Minute
)

func newRun(a *Agent, ctx context.Context, cancel context.CancelFunc) *Run {
	return &Run{
		agent: a, ctx: ctx, c: cancel,
		queue:   newEventQueue(maxQueuedEvents),
		parked:  map[string]*ParkedCall{},
		emitted: map[string]bool{},
	}
}

// markEmitted records that a host tool call has been surfaced to the host.
func (r *Run) markEmitted(id string) {
	r.mu.Lock()
	if r.emitted == nil {
		r.emitted = map[string]bool{}
	}
	r.emitted[id] = true
	r.mu.Unlock()
}

// emittedCall reports whether this call id was already surfaced.
func (r *Run) emittedCall(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.emitted[id]
}

// PauseStream closes the current read stream without ending the run, so the
// provider turn can finish while a host tool call is parked. The next turn
// resumes with Resume, which replays from the last durable offset.
func (r *Run) PauseStream() {
	r.mu.Lock()
	r.paused = true
	cancel := r.cancelStream
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	r.queue.close()
}

// PausedOnHostCall reports whether the live stream stopped because the run is
// waiting on a host tool result.
func (r *Run) PausedOnHostCall() bool { return r.pausedOnHostCall() }

// StreamClosedForPause reports whether this run is between provider turns with
// its stream closed on purpose, waiting for a host tool result.
func (r *Run) StreamClosedForPause() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.paused && r.terminal == nil
}

// pausedOnHostCall reports whether the live stream stopped because the run is
// waiting on a host tool result.
func (r *Run) pausedOnHostCall() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.paused && r.terminal == nil
}

// peekParked returns the parked call registered for a call id, if the callback
// has already arrived.
func (r *Run) peekParked(id string) *ParkedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.parked[id]
}

// noteOffset records resume bookkeeping. Only an ObserveRun stream's offsets may
// be handed back as after_offset, so a Send stream offset is never stored.
func (r *Run) noteOffset(msg *sdkpb.RunStreamMessage) {
	if msg == nil || !r.isResumed() {
		return
	}
	if off := msg.GetOffset(); off != "" {
		r.mu.Lock()
		r.observeOffset = off
		r.mu.Unlock()
	}
}

// IsResumed reports whether this run is being watched through the durable
// ObserveRun stream rather than its original live stream.
func (r *Run) IsResumed() bool { return r.isResumed() }

func (r *Run) isResumed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resumed
}

// openBarrierOnCompletion releases suppression once a durable replay has passed
// the host tool call the run parked on: text after that point is new.
func (r *Run) openBarrierOnCompletion(status string) {
	if status != "completed" && status != "error" {
		return
	}
	r.mu.Lock()
	r.barrierOpen = true
	r.mu.Unlock()
}

// suppressed reports whether a replayed event must not reach the host: on a
// durable replay, the assistant text of the already-streamed segment comes by
// again, and emitting it twice is exactly the duplicate-noise failure this
// extension is meant to remove.
func (r *Run) suppressed(kind EventKind) bool {
	r.mu.Lock()
	resumed, open := r.resumed, r.barrierOpen
	r.mu.Unlock()
	if !resumed || open {
		return false
	}
	switch kind {
	case EventTextDelta, EventThinkingDelta, EventThinkingCompleted:
		return true
	}
	return false
}

// Resume continues watching a run whose live Send stream was closed while it
// was parked on a host tool call. The durable stream replays from the offset
// this client last observed on an ObserveRun stream, or from the beginning when
// it has none, because a live Send offset must never be used as after_offset.
func (r *Run) Resume(ctx context.Context) error {
	r.mu.Lock()
	rid, off := r.runID, r.observeOffset
	if r.terminal != nil || r.fin {
		r.mu.Unlock()
		return nil // already over; the queued events and terminal tell the story
	}
	if r.emitted == nil {
		r.emitted = map[string]bool{}
	}
	// The replay carries the events of the first segment again; text and thinking
	// stay suppressed until the host call the run parked on reports completion.
	r.resumed = true
	r.paused = false
	r.queue = newEventQueue(maxQueuedEvents)
	r.mu.Unlock()
	if rid == "" {
		return errors.New("cursor: run id is not known yet")
	}

	req := &sdkpb.ObserveRunRequest{RunId: rid}
	if off != "" {
		req.AfterOffset = &off
	}
	streamCtx, cancelStream := context.WithCancel(r.ctx)
	stream, err := r.agent.client.Agent.ObserveRun(streamCtx, connect.NewRequest(req))
	if err != nil {
		cancelStream()
		return wrapErr("ObserveRun", err)
	}
	r.mu.Lock()
	if r.cancelStream != nil {
		r.cancelStream()
	}
	r.cancelStream = cancelStream
	r.mu.Unlock()
	go r.drain(stream)
	return nil
}

// RunID is the Cursor run id, known once the first event arrives.
func (r *Run) RunID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runID
}

// Next returns the next event a provider turn should consume, blocking until
// one arrives, the run ends, or ctx is done.
func (r *Run) Next(ctx context.Context) (RunEvent, bool) {
	return r.queue.pop(ctx, r.ctx)
}

// NextWithin waits up to wait for the next event. ok=false means either the
// wait expired or the stream ended; callers that use it to close a bounded
// window treat both the same way.
func (r *Run) NextWithin(ctx context.Context, wait time.Duration) (RunEvent, bool) {
	tctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	return r.queue.pop(tctx, r.ctx)
}

// NextReady returns the next event without blocking.
func (r *Run) NextReady() (RunEvent, bool) { return r.queue.popReady() }

// PendingHostCalls lists host tool calls still waiting for their result, in
// arrival order. A provider turn resolves these from the host tool results the
// next request carries.
func (r *Run) PendingHostCalls() []*ParkedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*ParkedCall, 0, len(r.order))
	for _, id := range r.order {
		if p := r.parked[id]; p != nil {
			if _, _, done := p.Settled(); !done {
				out = append(out, p)
			}
		}
	}
	return out
}

// ResolveHostCall delivers the host result to a parked call, exactly once.
func (r *Run) ResolveHostCall(id string, payload map[string]any, isError bool, note string) bool {
	r.mu.Lock()
	p := r.parked[id]
	if p == nil {
		r.mu.Unlock()
		return false
	}
	p.mu.Lock()
	if p.resolved {
		p.mu.Unlock()
		r.mu.Unlock()
		return false
	}
	p.resolved = true
	if isError {
		p.err = errors.New(orString(note, "host tool failed"))
	} else {
		p.result = wrapToolResult(orString(payloadText(payload), note))
	}
	p.mu.Unlock()
	close(p.done)
	delete(r.parked, id)
	var rest []string
	for _, oid := range r.order {
		if oid != id {
			rest = append(rest, oid)
		}
	}
	r.order = rest
	r.mu.Unlock()
	if r.agent != nil && r.agent.client != nil && r.agent.client.log != nil {
		r.agent.client.log.Info("host tool result delivered", "tool", p.ToolName, "id", id, "bytes", len(note))
	}
	return true
}

// CancelRun stops the upstream run and releases parked callbacks. The provider
// turn that requested the abort must not undo work the host already did; this
// only stops Cursor from going further.
func (r *Run) CancelRun(reason string) error {
	runID := r.RunID()
	r.abandon(errors.New(orString(reason, "cancelled")))
	if runID == "" {
		return nil
	}
	_, err := r.agent.client.Agent.CancelRun(context.Background(), connect.NewRequest(&sdkpb.CancelRunRequest{RunId: runID}))
	return wrapErr("CancelRun", err)
}

// abandon releases every parked call and stops the stream.
func (r *Run) abandon(reason error) {
	r.mu.Lock()
	ids := make([]string, 0, len(r.parked))
	for id, p := range r.parked {
		p.mu.Lock()
		if !p.resolved {
			p.resolved = true
			p.err = reason
			close(p.done)
		}
		p.mu.Unlock()
		delete(r.parked, id)
	}
	_ = ids
	r.order = nil
	r.fin = true
	r.mu.Unlock()
	r.c()
	r.queue.close()
}

// Finished reports whether the run reached a terminal state or was released.
func (r *Run) Finished() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fin
}

// Terminal returns the terminal result once known.
func (r *Run) Terminal() *RunResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.terminal
}

// Usage returns the latest usage snapshot.
func (r *Run) Usage() *Usage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.usage
}

// park holds a host tool call until the host resolves it.
func (r *Run) park(ctx context.Context, req ToolCallRequest) (map[string]any, error) {
	id := orString(req.ToolCallID, fmt.Sprintf("tool_%d", time.Now().UnixNano()))
	p := &ParkedCall{
		ID: id, ToolName: req.ToolName, Args: req.Args,
		ArrivedAt: req.ArrivedAt, done: make(chan struct{}),
	}
	if p.ArrivedAt.IsZero() {
		p.ArrivedAt = time.Now()
	}

	r.mu.Lock()
	if r.fin {
		r.mu.Unlock()
		return nil, errors.New("cursor: run already ended")
	}
	if existing, ok := r.parked[id]; ok {
		// A duplicate callback for the same call id must not run the tool twice.
		r.mu.Unlock()
		if res, err2, done := existing.Settled(); done {
			return res, err2
		}
		return nil, errors.New("cursor: duplicate tool callback is already pending")
	}
	r.parked[id] = p
	paused := r.paused
	r.paused = true
	r.order = append(r.order, id)
	cancel := r.cancelStream
	r.mu.Unlock()

	// The callback is the authoritative record of a host tool call: it carries the
	// tool the host advertised and the arguments Cursor chose. The stream's own
	// tool_call event for the same call is bookkeeping only, so exactly one card
	// reaches the host.
	r.queue.push(RunEvent{
		Event: Event{
			Kind:     EventToolCallStarted,
			ToolCall: &ToolCallEvent{ID: id, ToolName: req.ToolName, Args: req.Args, Status: "running"},
		},
		Pending: p,
	})
	if r.agent != nil && r.agent.client != nil && r.agent.client.log != nil {
		// INFO, not debug: "did the host run this tool once or twice?" is a
		// question worth answering from the default log.
		r.agent.client.log.Info("host tool call parked", "tool", req.ToolName, "id", id)
	}
	if !paused && cancel != nil {
		// Close the live stream so this provider turn ends; the run keeps waiting
		// upstream and ObserveRun picks it up with the next request.
		r.mu.Lock()
		r.mu.Unlock()
		cancel()
	}

	timer := time.NewTimer(defaultParkTimeout)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-r.ctx.Done():
		r.settleAbandoned(p, id, errors.New("cursor: run cancelled while waiting for the host tool"))
	case <-timer.C:
		r.settleAbandoned(p, id, fmt.Errorf("cursor: host tool %s did not return within %s", req.ToolName, defaultParkTimeout))
	}
	if p.err != nil {
		return nil, p.err
	}
	return r.copyResult(p)
}

// settleAbandoned ends a parked call from the run's side (cancel/timeout).
func (r *Run) settleAbandoned(p *ParkedCall, id string, reason error) {
	p.mu.Lock()
	if !p.resolved {
		p.resolved = true
		p.err = reason
		close(p.done)
		r.mu.Lock()
		delete(r.parked, id)
		r.mu.Unlock()
	}
	p.mu.Unlock()
}

func (r *Run) copyResult(p *ParkedCall) (map[string]any, error) {
	res, err, _ := p.Settled()
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(res))
	for k, v := range res {
		out[k] = v
	}
	return out, nil
}

// drain reads the run stream until it ends. It never blocks on a provider
// turn: events queue, and a provider that stops consuming cannot stall the run.
func (r *Run) drain(stream *connect.ServerStreamForClient[sdkpb.RunStreamMessage]) {
	defer stream.Close()
	for stream.Receive() {
		r.noteOffset(stream.Msg())
		for _, ev := range Decode(stream.Msg(), true) {
			if ev.RunID != "" || ev.AgentID != "" {
				r.mu.Lock()
				if r.runID == "" {
					r.runID = ev.RunID
				}
				if r.agentID == "" {
					r.agentID = ev.AgentID
				}
				r.mu.Unlock()
			}
			if id := ev.ToolCallID(); id != "" {
				r.noteToolCompletion(id, ev)
			}
			if ev.Kind == EventResult || ev.Kind == EventDone {
				r.recordTerminal(ev)
			}
			if ev.Kind == EventUsage && ev.Usage != nil {
				r.mu.Lock()
				r.usage = ev.Usage
				r.mu.Unlock()
			}
			if ev.Kind == EventStatus && ev.Status != nil && ev.Status.Status == "ERROR" {
				r.mu.Lock()
				if r.terminal == nil {
					r.terminal = &RunResult{Status: "ERROR", ErrorMessage: ev.Status.Message, RunID: r.runID}
				}
				r.mu.Unlock()
			}
			if ev.Kind == EventToolCallStarted && ev.ToolCall != nil {
				id := ev.ToolCall.ID
				if !r.agent.IsHostTool(ev.ToolCall.ToolName) {
					// Cursor executed one of its own tools. It is not a host tool
					// card and must not be replayed as one.
					continue
				}
				if r.emittedCall(id) {
					// A durable replay of a call the host already ran: everything
					// before its completion belongs to text already streamed.
					r.openBarrierOnCompletion(ev.ToolCall.Status)
					continue
				}
				// Bookkeeping only: the callback path emits the card, so emitting it
				// here as well would show the same tool call twice.
				r.markEmitted(id)
				continue
			}
			if ev.Kind == EventToolCallCompleted && ev.ToolCall != nil && r.emittedCall(ev.ToolCall.ID) {
				r.openBarrierOnCompletion(ev.ToolCall.Status)
				continue
			}
			if r.suppressed(ev.Kind) {
				continue
			}
			if !r.queue.push(RunEvent{Event: ev}) {
				r.mu.Lock()
				r.drainErr = errors.New("cursor: run produced more events than the host can consume")
				r.mu.Unlock()
				r.abandon(errors.New("cursor: event queue overflow"))
				return
			}
		}
	}
	if err := stream.Err(); err != nil && !errors.Is(err, context.Canceled) {
		r.mu.Lock()
		r.drainErr = wrapErr("run stream", err)
		r.mu.Unlock()
	}
	r.mu.Lock()
	paused := r.paused
	if r.terminal == nil && r.drainErr == nil && !paused {
		r.fin = true
	}
	r.mu.Unlock()
	r.queue.close()
}

func (r *Run) noteToolCompletion(id string, ev Event) {
	if ev.Kind != EventToolCallCompleted {
		return
	}
	r.mu.Lock()
	if p := r.parked[id]; p != nil {
		_ = p
	}
	r.mu.Unlock()
}

func (r *Run) recordTerminal(ev Event) {
	if ev.Kind == EventResult && ev.Result != nil {
		r.mu.Lock()
		r.terminal = ev.Result
		if r.runID == "" {
			r.runID = ev.Result.RunID
		}
		r.fin = true
		r.mu.Unlock()
	}
}

// DrainErr reports a stream failure that is not a normal end.
func (r *Run) DrainErr() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.drainErr
}

// ---- event queue ----------------------------------------------------------

type eventQueue struct {
	mu        sync.Mutex
	cond      *sync.Cond
	items     []RunEvent
	closed    bool
	overflow  error
	maxEvents int
}

func newEventQueue(max int) *eventQueue {
	q := &eventQueue{maxEvents: max}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *eventQueue) push(ev RunEvent) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return true // dropping after close is normal: the host already stopped
	}
	if len(q.items) >= q.maxEvents {
		return false
	}
	q.items = append(q.items, ev)
	q.cond.Signal()
	return true
}

func (q *eventQueue) pop(ctx context.Context, runCtx context.Context) (RunEvent, bool) {
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			q.cond.Broadcast()
		case <-runCtx.Done():
			q.cond.Broadcast()
		case <-stop:
		}
	}()
	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		if len(q.items) > 0 {
			ev := q.items[0]
			q.items = q.items[1:]
			return ev, true
		}
		if q.closed {
			return RunEvent{}, false
		}
		if ctx.Err() != nil || runCtx.Err() != nil {
			return RunEvent{}, false
		}
		q.cond.Wait()
	}
}

func (q *eventQueue) popReady() (RunEvent, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return RunEvent{}, false
	}
	ev := q.items[0]
	q.items = q.items[1:]
	return ev, true
}

func (q *eventQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

func (q *eventQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// queuedIDs is a test helper for ordering assertions.
func (r *Run) queuedIDs() []string {
	var out []string
	for {
		ev, ok := r.queue.popReady()
		if !ok {
			break
		}
		out = append(out, string(ev.Kind)+":"+ev.ToolCallID())
	}
	return out
}

// ---- helpers --------------------------------------------------------------

func wrapToolResult(text string) map[string]any {
	return map[string]any{"content": text}
}

func payloadText(m map[string]any) string {
	if m == nil {
		return ""
	}
	if s, ok := m["content"].(string); ok {
		return s
	}
	if s, ok := m["text"].(string); ok {
		return s
	}
	return ""
}

func orString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// structFromToolResult keeps a host result as a Struct with a text field, so
// the bridge never sees a bare scalar.
func structFromToolResult(text string) (*structpb.Struct, error) {
	return structFromMap(map[string]any{"content": text})
}

// sortStrings is kept for deterministic callback reporting in logs.
func sortStrings(s []string) { sort.Strings(s) }
