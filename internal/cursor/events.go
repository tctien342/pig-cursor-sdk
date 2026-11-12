package cursor

import (
	"fmt"
	"strconv"
	"strings"

	sdkpb "github.com/tctien342/pig-cursor-sdk/gen/sdk/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// EventKind names what a decoded run event carries.
type EventKind string

const (
	EventTextDelta         EventKind = "text_delta"
	EventThinkingDelta     EventKind = "thinking_delta"
	EventThinkingCompleted EventKind = "thinking_completed"
	EventToolCallStarted   EventKind = "tool_call_started"
	EventToolCallCompleted EventKind = "tool_call_completed"
	// EventCursorToolStarted / EventCursorToolCompleted are Cursor's own tools —
	// ones this host never advertised, so pig cannot run or render them. They are
	// tagged in the run stream rather than dropped, because Cursor really did
	// something and the user is entitled to see it; the provider turns them into a
	// transcript line of its own. They never become a host tool call.
	EventCursorToolStarted   EventKind = "cursor_tool_started"
	EventCursorToolCompleted EventKind = "cursor_tool_completed"
	EventStatus              EventKind = "status"
	EventUsage               EventKind = "usage"
	EventTask                EventKind = "task"
	EventResult              EventKind = "result"
	EventDone                EventKind = "done"
	EventUnknown             EventKind = "unknown"
)

// Event is one normalized item of a Cursor run stream.
//
// Text and thinking arrive twice on a live stream with enable_deltas: once as
// an interaction_update delta and once as an sdk_message carrying the same
// chunk. Decode emits a chunk exactly once (deltas win when enabled), because
// double-emission is what makes a transcript noisy.
type Event struct {
	Kind   EventKind
	Offset string
	Text   string

	// RunID and AgentID identify the run the event belongs to, for resuming a
	// run whose live stream ended while a host tool call was parked.
	RunID   string
	AgentID string

	ToolCall *ToolCallEvent
	Status   *StatusEvent
	Usage    *Usage
	Result   *RunResult
	Task     *TaskEvent
}

// ToolCallEvent describes one tool invocation of the run. ToolName is the
// declared tool name; for host-owned custom tools the bridge reports the
// envelope name ("mcp") and the real name sits in Args, which Decode unwraps.
type ToolCallEvent struct {
	// ID is Cursor's id, verbatim, and the internal key.
	ID string
	// DisplayID is the host-safe form of ID: see DisplayCallID.
	DisplayID string
	RunID     string
	AgentID   string
	ToolName  string
	Args      map[string]any
	Status    string // running | completed | error
	Result    map[string]any
	IsError   bool
	Text      string // the tool result rendered as text, when available
}

// DisplayCallID rewrites a Cursor tool-call id into a form that is safe to use
// where an id is expected to be one token.
//
// Cursor publishes ids containing a literal newline, e.g.
//
//	call-dc6de823-...-1\nfc_00b4b5ed-..._0
//
// Legal in a JSON string, but it lands verbatim in the host's session transcript
// and it splits one-line-per-event log parsing. Whitespace is percent-encoded
// rather than collapsed: collapsing maps `a\nb` and `a_b` onto each other, and
// this value keys things, so the rewrite must stay reversible.
//
// The raw id stays the internal key everywhere (parked ledger, replay dedup);
// this form is only what the host and the log see. DecodeCallID inverts it.
func DisplayCallID(id string) string {
	if !strings.ContainsAny(id, " \t\n\r") {
		return id
	}
	var b strings.Builder
	b.Grow(len(id) + 4)
	for i := 0; i < len(id); i++ {
		switch id[i] {
		case ' ':
			b.WriteString("%20")
		case '\t':
			b.WriteString("%09")
		case '\n':
			b.WriteString("%0A")
		case '\r':
			b.WriteString("%0D")
		default:
			b.WriteByte(id[i])
		}
	}
	return b.String()
}

// DecodeCallID inverts DisplayCallID. Callers that need the id Cursor itself
// knows should start from the raw value instead; this exists so a display id can
// be traced back when reading logs.
func DecodeCallID(id string) string {
	if !strings.Contains(id, "%") {
		return id
	}
	var b strings.Builder
	b.Grow(len(id))
	for i := 0; i < len(id); i++ {
		if id[i] == '%' && i+2 < len(id) {
			if v, err := strconv.ParseUint(id[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		b.WriteByte(id[i])
	}
	return b.String()
}

// StatusEvent is a run lifecycle status. On failure the human-readable reason
// only appears here, never in RunStreamResult.error_code.
type StatusEvent struct {
	Status  string
	Message string
}

// Usage mirrors TokenUsage.
type Usage struct {
	InputTokens     int64 `json:"inputTokens"`
	OutputTokens    int64 `json:"outputTokens"`
	CacheReadTokens int64 `json:"cacheReadTokens"`
	CacheWriteToks  int64 `json:"cacheWriteTokens"`
	TotalTokens     int64 `json:"totalTokens"`
	ReasoningTokens int64 `json:"reasoningTokens"`
}

// RunResult is the terminal outcome of a run.
type RunResult struct {
	RunID        string
	AgentID      string
	Status       string
	Text         string
	ErrorCode    string
	ErrorMessage string
	Usage        *Usage
	DurationMs   uint64
}

// TaskEvent is a sub-agent activity line. It is reported, never rendered as
// transcript noise by default.
type TaskEvent struct {
	Status string
	Text   string
	Raw    map[string]any
}

// Decode turns one run-stream message into events. An empty envelope is a
// keepalive and yields nothing; unknown cases are skipped silently, which is
// the forward-compatibility rule the protocol requires.
func Decode(msg *sdkpb.RunStreamMessage, deltasEnabled bool) []Event {
	if msg == nil {
		return nil
	}
	offset := msg.GetOffset()
	switch env := msg.GetEnvelope().(type) {
	case *sdkpb.RunStreamMessage_SdkMessage:
		return decodeSDKMessage(env.SdkMessage, offset, deltasEnabled)
	case *sdkpb.RunStreamMessage_InteractionUpdate:
		return decodeInteraction(env.InteractionUpdate, offset, deltasEnabled)
	case *sdkpb.RunStreamMessage_Step:
		// Completed steps duplicate the deltas we already streamed; only keep
		// the ones with no delta equivalent.
		return nil
	case *sdkpb.RunStreamMessage_Result:
		res := env.Result
		out := Event{Kind: EventResult, Offset: offset, Status: &StatusEvent{Status: lifecycleString(res.GetStatus())}}
		if r := res.GetResult(); r != nil {
			rr := &RunResult{
				RunID:      r.GetRunId(),
				AgentID:    r.GetAgentId(),
				Status:     lifecycleString(r.GetStatus()),
				Text:       r.GetResult(),
				DurationMs: r.GetDurationMs(),
			}
			rr.ErrorCode = res.GetErrorCode()
			rr.ErrorMessage = r.GetErrorMessage()
			if u := r.GetUsage(); u != nil {
				rr.Usage = convertUsage(u)
			}
			out.Result = rr
			if rr.Status == "ERROR" || rr.Status == "CANCELLED" {
				out.Status.Message = rr.ErrorMessage
			}
		}
		return []Event{out}
	case *sdkpb.RunStreamMessage_Done:
		return []Event{{Kind: EventDone, Offset: offset}}
	}
	return nil
}

func decodeSDKMessage(m *sdkpb.SdkMessage, offset string, deltasEnabled bool) []Event {
	payload := m.GetMessage().AsMap()
	out := decodeSDKPayload(m.GetType(), payload, offset, deltasEnabled)
	// Every sdk_message payload carries the run it belongs to. Capturing it here
	// means a run parked on a host tool call is still identifiable, which is what
	// ObserveRun needs to continue it after the provider turn ended.
	if len(out) > 0 {
		rid, aid := str(payload["run_id"]), str(payload["agent_id"])
		for i := range out {
			if out[i].RunID == "" {
				out[i].RunID = rid
			}
			if out[i].AgentID == "" {
				out[i].AgentID = aid
			}
		}
	}
	return out
}

func decodeSDKPayload(kind string, payload map[string]any, offset string, deltasEnabled bool) []Event {
	switch kind {
	case "assistant", "user":
		if deltasEnabled {
			return nil // already streamed as interaction_update deltas
		}
		// The bridge nests the provider message one level down: the payload is
		// {type, agent_id, run_id, message:{role, content:[...]}}.
		content := payload["content"]
		if inner := mapOrNil(payload["message"]); inner != nil && inner["content"] != nil {
			content = inner["content"]
		}
		var out []Event
		for _, text := range collectText(content) {
			out = append(out, Event{Kind: EventTextDelta, Offset: offset, Text: text})
		}
		return out
	case "thinking":
		if deltasEnabled {
			return nil
		}
		text, _ := payload["text"].(string)
		if text == "" {
			return nil
		}
		return []Event{{Kind: EventThinkingDelta, Offset: offset, Text: text}}
	case "tool_call":
		return []Event{decodeToolCall(payload, offset)}
	case "status":
		st, _ := payload["status"].(string)
		reason, _ := payload["message"].(string)
		return []Event{{Kind: EventStatus, Offset: offset, Status: &StatusEvent{Status: st, Message: reason}}}
	case "usage":
		if u := payload["usage"]; u != nil {
			return []Event{{Kind: EventUsage, Offset: offset, Usage: mapUsage(u)}}
		}
	case "task":
		txt, _ := payload["text"].(string)
		status, _ := payload["status"].(string)
		return []Event{{Kind: EventTask, Offset: offset, Task: &TaskEvent{Status: status, Text: txt, Raw: payload}}}
	}
	return nil
}

func decodeToolCall(payload map[string]any, offset string) Event {
	status, _ := payload["status"].(string)
	kind := EventToolCallStarted
	if status == "completed" || status == "error" {
		kind = EventToolCallCompleted
	}
	tc := &ToolCallEvent{
		ID:        str(payload["call_id"]),
		DisplayID: DisplayCallID(str(payload["call_id"])),
		RunID:     str(payload["run_id"]),
		AgentID:   str(payload["agent_id"]),
		ToolName:  str(payload["name"]),
		Args:      mapOrNil(payload["args"]),
		Status:    status,
	}
	unwrapCustomTool(tc)
	if res := mapOrNil(payload["result"]); res != nil {
		tc.Result = res
		if v := mapOrNil(res["value"]); v != nil {
			tc.IsError = boolOr(v["isError"])
			tc.Text = joinContentText(v["content"])
		}
	}
	return Event{Kind: kind, Offset: offset, ToolCall: tc, RunID: tc.RunID, AgentID: tc.AgentID}
}

// unwrapCustomTool turns the MCP envelope the bridge reports for host-owned
// custom tools back into the tool the host actually advertised.
func unwrapCustomTool(tc *ToolCallEvent) {
	if tc.Args == nil {
		return
	}
	if name := str(tc.Args["toolName"]); name != "" && (tc.ToolName == "mcp" || tc.ToolName == "") {
		tc.ToolName = name
		if inner := mapOrNil(tc.Args["args"]); inner != nil {
			tc.Args = inner
		}
	}
}

func decodeInteraction(u *sdkpb.InteractionUpdate, offset string, deltasEnabled bool) []Event {
	if !deltasEnabled {
		return nil
	}
	payload := u.GetUpdate().AsMap()
	switch u.GetType() {
	case "text-delta":
		return deltaOrSkip(EventTextDelta, offset, payload, "text")
	case "thinking-delta":
		return deltaOrSkip(EventThinkingDelta, offset, payload, "text")
	case "thinking-completed":
		d, _ := payload["thinkingDurationMs"].(float64)
		return []Event{{Kind: EventThinkingCompleted, Offset: offset, Text: fmt.Sprintf("%dms", int64(d))}}
	case "tool-call-started":
		return []Event{interactionToolCall(payload, offset, EventToolCallStarted)}
	case "tool-call-completed":
		return []Event{interactionToolCall(payload, offset, EventToolCallCompleted)}
	case "turn-ended":
		if um := mapOrNil(payload["usage"]); um != nil {
			return []Event{{Kind: EventUsage, Offset: offset, Usage: mapUsage(um)}}
		}
	}
	return nil
}

func deltaOrSkip(kind EventKind, offset string, payload map[string]any, key string) []Event {
	text := str(payload[key])
	if text == "" {
		return nil
	}
	return []Event{{Kind: kind, Offset: offset, Text: text}}
}

func interactionToolCall(payload map[string]any, offset string, kind EventKind) Event {
	call := mapOrNil(payload["toolCall"])
	tc := &ToolCallEvent{ID: str(payload["callId"]), DisplayID: DisplayCallID(str(payload["callId"])), Status: "running"}
	if kind == EventToolCallCompleted {
		tc.Status = "completed"
	}
	if call != nil {
		tc.ToolName = str(call["type"])
		tc.Args = mapOrNil(call["args"])
		unwrapCustomTool(tc)
		if res := mapOrNil(call["result"]); res != nil {
			tc.Result = res
			if v := mapOrNil(res["value"]); v != nil {
				tc.IsError = boolOr(v["isError"])
				tc.Text = joinContentText(v["content"])
			}
		}
	}
	return Event{Kind: kind, Offset: offset, ToolCall: tc}
}

// joinContentText renders the SDK's tool result content blocks as plain text.
// The nested shape is {content:[{text:{text:"..."}}]} or [{text:"..."}].
func joinContentText(v any) string {
	items, _ := v.([]any)
	if len(items) == 0 {
		return ""
	}
	var b []byte
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		var chunk string
		switch t := m["text"].(type) {
		case string:
			chunk = t
		case map[string]any:
			chunk = str(t["text"])
		}
		if chunk == "" {
			continue
		}
		if len(b) > 0 {
			b = append(b, '\n')
		}
		b = append(b, chunk...)
	}
	return string(b)
}

// collectText flattens text blocks out of an assistant content array.
func collectText(v any) []string {
	switch t := v.(type) {
	case string:
		if t != "" {
			return []string{t}
		}
	case []any:
		var out []string
		for _, item := range t {
			m, _ := item.(map[string]any)
			if m == nil {
				continue
			}
			if str(m["type"]) == "text" {
				if s := str(m["text"]); s != "" {
					out = append(out, s)
				}
			}
		}
		return out
	}
	return nil
}

func convertUsage(u *sdkpb.TokenUsage) *Usage {
	return &Usage{
		InputTokens:     u.GetInputTokens(),
		OutputTokens:    u.GetOutputTokens(),
		CacheReadTokens: u.GetCacheReadTokens(),
		CacheWriteToks:  u.GetCacheWriteTokens(),
		TotalTokens:     u.GetTotalTokens(),
		ReasoningTokens: u.GetReasoningTokens(),
	}
}

func mapUsage(v any) *Usage {
	m := mapOrNil(v)
	if m == nil {
		return nil
	}
	num := func(k string) int64 {
		f, _ := m[k].(float64)
		return int64(f)
	}
	return &Usage{
		InputTokens:     num("inputTokens"),
		OutputTokens:    num("outputTokens"),
		CacheReadTokens: num("cacheReadTokens"),
		CacheWriteToks:  num("cacheWriteTokens"),
		TotalTokens:     num("totalTokens"),
		ReasoningTokens: num("reasoningTokens"),
	}
}

func lifecycleString(s sdkpb.RunLifecycleStatus) string {
	name := s.String()
	return trimPrefix(name, "RUN_LIFECYCLE_STATUS_")
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func boolOr(v any) bool {
	b, _ := v.(bool)
	return b
}

func mapOrNil(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func trimPrefix(s, p string) string {
	if len(s) >= len(p) && s[:len(p)] == p {
		return s[len(p):]
	}
	return s
}

// ToolCallID returns the tool call id for tool events, "" otherwise.
func (e Event) ToolCallID() string {
	if e.ToolCall == nil {
		return ""
	}
	return e.ToolCall.ID
}

// structFromMap encodes a Go map as the Struct the bridge expects. Tool
// results are Structs, so a bare scalar has to be wrapped in an object.
func structFromMap(m map[string]any) (*structpb.Struct, error) {
	if m == nil {
		m = map[string]any{}
	}
	return structpb.NewStruct(m)
}
