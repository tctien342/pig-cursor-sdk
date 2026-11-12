package cursor

import (
	"os"
	"strings"
	"testing"

	sdkpb "github.com/tctien342/pig-cursor-sdk/gen/sdk/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// loadFixture reads the real frames captured from a live bridge run.
func loadFixture(t *testing.T) []*sdkpb.RunStreamMessage {
	t.Helper()
	raw, err := os.ReadFile("testdata/send-frames.jsonl")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	var out []*sdkpb.RunStreamMessage
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		msg := &sdkpb.RunStreamMessage{}
		if err := protojson.Unmarshal([]byte(line), msg); err != nil {
			t.Fatalf("protojson: %v\n%s", err, line[:min(len(line), 200)])
		}
		out = append(out, msg)
	}
	if len(out) < 100 {
		t.Fatalf("fixture too small: %d frames", len(out))
	}
	return out
}

// Cursor publishes tool-call ids containing a literal newline (the value below is
// copied from docs/evidence/live-tool-turn.jsonl). Ids are expected to be one
// token — they land in the host's session transcript and in one-line log records —
// but the rewrite must stay reversible, because this value keys things.
func TestDisplayCallIDSplitsWhitespaceWithoutLosingIt(t *testing.T) {
	raw := "call-dc6de823-12c8-4268-b573-5c28e0ae464f-1\nfc_00b4b5ed-b09c-964d-be33-bfa7a9c6826e_0"
	got := DisplayCallID(raw)
	if strings.ContainsAny(got, " \t\n\r") {
		t.Fatalf("DisplayCallID kept whitespace: %q", got)
	}
	if DecodeCallID(got) != raw {
		t.Fatalf("round trip lost information: %q -> %q -> %q", raw, got, DecodeCallID(got))
	}
	if DisplayCallID(got) != got {
		t.Fatal("DisplayCallID is not idempotent; parked and streamed ids could stop matching")
	}
	// Collapsing whitespace would map all of these onto the same string, and the
	// parked ledger and the host's tool cards would then point at different calls.
	for _, other := range []string{"call-x_b", "call-x b", "call-x\tb", "call-x\rb"} {
		if DisplayCallID(other) == DisplayCallID("call-x\nb") {
			t.Fatalf("%q and %q collide after DisplayCallID", other, "call-x\\nb")
		}
		if DecodeCallID(DisplayCallID(other)) != other {
			t.Fatalf("round trip failed for %q", other)
		}
	}
	if DisplayCallID("plain-id") != "plain-id" {
		t.Fatal("an id without whitespace must pass through untouched")
	}
}

func TestDecodeRealFrames(t *testing.T) {
	msgs := loadFixture(t)

	var (
		textDeltas     strings.Builder
		thinkingChunks int
		usageSeen      bool
		resultSeen     *RunResult
		doneSeen       bool
		started, ended []*ToolCallEvent
		unknown        int
	)
	for _, m := range msgs {
		events := Decode(m, true)
		if msgHasEnvelope(m) && len(events) == 0 {
			// Deliberately ignored payloads: step envelopes and the duplicate
			// assistant/thinking sdk_messages that deltas already cover.
			continue
		}
		for _, e := range events {
			switch e.Kind {
			case EventTextDelta:
				textDeltas.WriteString(e.Text)
			case EventThinkingDelta:
				thinkingChunks++
			case EventUsage:
				usageSeen = true
			case EventToolCallStarted:
				started = append(started, e.ToolCall)
			case EventToolCallCompleted:
				ended = append(ended, e.ToolCall)
			case EventResult:
				resultSeen = e.Result
			case EventDone:
				doneSeen = true
			case EventUnknown:
				unknown++
			}
		}
	}

	if textDeltas.Len() == 0 {
		t.Fatal("no text deltas decoded")
	}
	if thinkingChunks == 0 {
		t.Error("no thinking deltas decoded")
	}
	if !usageSeen {
		t.Error("no usage decoded")
	}
	if !doneSeen || resultSeen == nil {
		t.Fatalf("terminal events missing: done=%v result=%v", doneSeen, resultSeen != nil)
	}
	if resultSeen.Status != "FINISHED" {
		t.Errorf("result status = %q", resultSeen.Status)
	}
	if unknown != 0 {
		t.Errorf("unexpected EventUnknown count %d", unknown)
	}

	// The fixture's tool call is a host-owned custom tool: the bridge reports
	// the MCP envelope, the decoder must surface the real name and args.
	if len(started) == 0 || len(ended) == 0 {
		t.Fatalf("tool call events missing: started=%d completed=%d", len(started), len(ended))
	}
	sc := started[0]
	if sc.ToolName != "spike_host_tool" {
		t.Errorf("tool name = %q, want the unwrapped custom tool name", sc.ToolName)
	}
	if got, _ := sc.Args["command"].(string); got != "echo parked-spike" {
		t.Errorf("tool args = %v", sc.Args)
	}
	if sc.ID == "" {
		t.Error("tool call id empty")
	}
	ec := ended[0]
	if ec.ToolName != "spike_host_tool" {
		t.Errorf("completed tool name = %q", ec.ToolName)
	}
	if !strings.Contains(ec.Text, "parked-tool-output ok") {
		t.Errorf("completed tool result text = %q", ec.Text)
	}
	if ec.IsError {
		t.Error("completed tool call reported as error")
	}
}

// TestTextEmittedExactlyOnce is the anti-noise guarantee: with deltas enabled,
// a streamed chunk must not also arrive from the sdk_message duplicate.
func TestTextEmittedExactlyOnce(t *testing.T) {
	msgs := loadFixture(t)
	var fromDeltas, fromSDK int
	for _, m := range msgs {
		if iu := m.GetInteractionUpdate(); iu != nil && iu.GetType() == "text-delta" {
			if txt := iu.GetUpdate().AsMap()["text"]; txt != "" {
				fromDeltas++
			}
		}
		for _, e := range Decode(m, true) {
			if e.Kind == EventTextDelta {
				fromSDK++
			}
		}
	}
	if fromDeltas == 0 {
		t.Fatal("fixture has no text deltas")
	}
	if fromSDK != fromDeltas {
		t.Fatalf("decoded %d text events for %d deltas: text is %s", fromSDK, fromDeltas,
			map[bool]string{true: "duplicated", false: "missing"}[fromSDK > fromDeltas])
	}
}

// TestDecodeWithDeltasDisabledFallsBackToSDKMessages covers the non-delta path.
func TestDecodeWithDeltasDisabledFallsBackToSDKMessages(t *testing.T) {
	msgs := loadFixture(t)
	var text int
	for _, m := range msgs {
		for _, e := range Decode(m, false) {
			if e.Kind == EventTextDelta {
				text++
			}
		}
	}
	if text == 0 {
		t.Fatal("without deltas, assistant sdk_messages should still yield text")
	}
}

func TestKeepaliveYieldsNothing(t *testing.T) {
	if events := Decode(&sdkpb.RunStreamMessage{}, true); len(events) != 0 {
		t.Fatalf("keepalive produced %d events: %v", len(events), events)
	}
	if events := Decode(nil, true); len(events) != 0 {
		t.Fatalf("nil message produced %d events", len(events))
	}
}

// TestUnknownTypesAreSkipped proves forward compatibility: a payload type the
// adapter has never seen must be ignored, not turned into an error or text.
func TestUnknownTypesAreSkipped(t *testing.T) {
	unknown := &sdkpb.RunStreamMessage{Envelope: &sdkpb.RunStreamMessage_SdkMessage{
		SdkMessage: wrapSDKMessage(t, "brand_new_event", map[string]any{"hello": "world"}),
	}}
	if events := Decode(unknown, true); len(events) != 0 {
		t.Fatalf("unknown sdk_message type produced %d events: %+v", len(events), events)
	}
	unknownEnv := &sdkpb.RunStreamMessage{}
	if err := protojson.Unmarshal([]byte(`{"someFutureCase":{"x":1}}`), unknownEnv); err != nil {
		t.Skipf("protojson rejects unknown fields in this build: %v", err)
	}
	if events := Decode(unknownEnv, true); len(events) != 0 {
		t.Fatalf("unknown envelope case produced %d events", len(events))
	}
}

func TestStatusFailureCarriesReadableMessage(t *testing.T) {
	msg := &sdkpb.RunStreamMessage{Envelope: &sdkpb.RunStreamMessage_SdkMessage{
		SdkMessage: wrapSDKMessage(t, "status", map[string]any{
			"status":  "ERROR",
			"message": "Invalid User API Key",
		}),
	}}
	events := Decode(msg, true)
	if len(events) != 1 || events[0].Status == nil {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Status.Message != "Invalid User API Key" {
		t.Errorf("failure message = %q", events[0].Status.Message)
	}
}

func msgHasEnvelope(m *sdkpb.RunStreamMessage) bool { return m.GetEnvelope() != nil }

func wrapSDKMessage(t *testing.T, typ string, payload map[string]any) *sdkpb.SdkMessage {
	t.Helper()
	// The bridge repeats `type` inside the payload; mirror that shape.
	m := map[string]any{"type": typ}
	for k, v := range payload {
		m[k] = v
	}
	st2, err := structFromMap(m)
	if err != nil {
		t.Fatal(err)
	}
	return &sdkpb.SdkMessage{Type: typ, Message: st2}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
