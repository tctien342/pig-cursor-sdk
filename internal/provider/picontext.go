package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

func jsonUnmarshal(raw []byte, v any) error { return json.Unmarshal(raw, v) }

// PiContext is the provider request the pig host hands to streamSimple.
type PiContext struct {
	SystemPrompt string
	Messages     []Message
	Tools        []ToolDef
}

// ToolDef is one tool the host will execute. Its name is what we advertise to
// Cursor, so a Cursor tool call becomes a native pig tool card.
type ToolDef struct {
	Name        string
	Description string
	Schema      map[string]any
}

// Message is a transcript message in the shape the host sends.
type Message struct {
	Role    string
	Blocks  []Block
	Raw     map[string]any
	EntryID string
}

// Block is one content block of a message.
type Block struct {
	TextSignature string // provider display metadata; not part of the visible text
	Type          string // text | thinking | image | toolCall | toolResult
	Text          string
	ToolName      string
	ToolID        string
	Args          map[string]any
	IsError       bool
}

// ParsePiContext decodes the host's context map. Unknown shapes are tolerated:
// an unexpected field must degrade to a usable prompt, never drop the turn.
func ParsePiContext(raw map[string]any) PiContext {
	var out PiContext
	if raw == nil {
		return out
	}
	out.SystemPrompt = firstString(raw, "systemPrompt", "system_prompt")
	if tools := raw["tools"]; tools != nil {
		out.Tools = parseTools(tools)
	}
	if msgs, ok := raw["messages"].([]any); ok {
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok {
				if parsed, ok := parseMessage(mm); ok {
					out.Messages = append(out.Messages, parsed)
				}
			}
		}
	}
	return out
}

func parseTools(v any) []ToolDef {
	items, _ := v.([]any)
	out := make([]ToolDef, 0, len(items))
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		name := firstString(m, "name")
		if name == "" {
			continue
		}
		schema := mapOrNil(m["parameters"])
		if schema == nil {
			schema = mapOrNil(m["inputSchema"])
		}
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		out = append(out, ToolDef{Name: name, Description: firstString(m, "description"), Schema: schema})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func parseMessage(m map[string]any) (Message, bool) {
	role := firstString(m, "role")
	if role == "" {
		return Message{}, false
	}
	// pig carries its own bookkeeping roles; only conversation roles feed the model.
	switch role {
	case "user", "assistant", "toolResult", "system", "bashExecution", "custom":
	default:
		return Message{}, false
	}
	msg := Message{Role: role, Raw: m, EntryID: firstString(m, "id", "entryId")}
	switch content := m["content"].(type) {
	case string:
		if content != "" {
			msg.Blocks = append(msg.Blocks, Block{Type: "text", Text: content})
		}
	case []any:
		for _, cb := range content {
			bm, _ := cb.(map[string]any)
			if bm == nil {
				continue
			}
			switch firstString(bm, "type") {
			case "text":
				msg.Blocks = append(msg.Blocks, Block{Type: "text", Text: firstString(bm, "text"), TextSignature: firstString(bm, "textSignature")})
			case "thinking":
				msg.Blocks = append(msg.Blocks, Block{Type: "thinking", Text: firstString(bm, "thinking")})
			case "image":
				msg.Blocks = append(msg.Blocks, Block{
					Type: "image", Text: firstString(bm, "mimeType"),
					Args: map[string]any{"data": firstString(bm, "data")},
				})
			case "toolCall", "tool_use":
				msg.Blocks = append(msg.Blocks, Block{
					Type: "toolCall", ToolName: firstString(bm, "name"), ToolID: firstString(bm, "id"),
					Args: mapOrNil(bm["arguments"]),
				})
			case "toolResult", "tool_result":
				msg.Blocks = append(msg.Blocks, Block{
					Type: "toolResult", ToolID: firstString(bm, "toolCallId", "tool_use_id"),
					Text: blocksToText(bm["content"]), IsError: boolOrNil(bm["isError"]),
				})
			}
		}
	}
	if role == "toolResult" {
		// pig puts a tool result's content at the top level too.
		txt := blocksToText(m["content"])
		if txt != "" && len(msg.Blocks) == 0 {
			msg.Blocks = append(msg.Blocks, Block{Type: "toolResult", Text: txt, ToolID: firstString(m, "toolCallId"), IsError: boolOrNil(m["isError"])})
		}
	}
	return msg, true
}

func blocksToText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var parts []string
		for _, it := range t {
			m, _ := it.(map[string]any)
			if m == nil {
				continue
			}
			switch firstString(m, "type") {
			case "text":
				parts = append(parts, firstString(m, "text"))
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// Fingerprint is the stable identity of a message for append-only detection.
func (m Message) Fingerprint() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|", m.Role, m.EntryID)
	for _, b := range m.Blocks {
		fmt.Fprintf(h, "%s:%s:%s:%s;", b.Type, b.ToolName, b.ToolID, truncate(b.Text, 4096))
		if len(b.Args) > 0 {
			raw, _ := json.Marshal(b.Args)
			fmt.Fprintf(h, "args:%s;", truncate(string(raw), 4096))
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}

// UserText returns the text of the newest user turn, "" when the newest
// user-visible turn is not text.
func (c PiContext) UserText() string {
	for i := len(c.Messages) - 1; i >= 0; i-- {
		if c.Messages[i].Role != "user" {
			continue
		}
		var parts []string
		for _, b := range c.Messages[i].Blocks {
			if b.Type == "text" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// Images returns base64 image attachments of the newest user turn.
func (c PiContext) Images() []ImageRef {
	for i := len(c.Messages) - 1; i >= 0; i-- {
		if c.Messages[i].Role != "user" {
			continue
		}
		var out []ImageRef
		for _, b := range c.Messages[i].Blocks {
			if b.Type == "image" && b.Args != nil {
				if data, _ := b.Args["data"].(string); data != "" {
					out = append(out, ImageRef{Data: data, MIMEType: b.Text})
				}
			}
		}
		return out
	}
	return nil
}

// ImageRef is one base64 image of the current turn.
type ImageRef struct{ Data, MIMEType string }

// PendingToolResults returns the tool results the transcript carries for the
// given call ids, in the order the host produced them.
func (c PiContext) PendingToolResults(want []string) map[string]ToolOutcome {
	out := map[string]ToolOutcome{}
	if len(want) == 0 {
		return out
	}
	wanted := make(map[string]bool, len(want))
	for _, id := range want {
		wanted[id] = true
	}
	for _, m := range c.Messages {
		for _, b := range m.Blocks {
			if b.Type != "toolResult" && m.Role != "toolResult" {
				continue
			}
			id := b.ToolID
			if id == "" || !wanted[id] {
				continue
			}
			out[id] = ToolOutcome{Text: b.Text, IsError: b.IsError}
		}
		if m.Role == "toolResult" {
			id := firstString(m.Raw, "toolCallId")
			if id != "" && wanted[id] {
				out[id] = ToolOutcome{Text: blocksToText(m.Raw["content"]), IsError: boolOrNil(m.Raw["isError"])}
			}
		}
	}
	return out
}

// ToolOutcome is one host tool result.
type ToolOutcome struct {
	Text    string
	IsError bool
}

// ReplayPrompt renders the transcript for a fresh Cursor agent: a continuity
// brief the model can act on, kept short enough that it does not crowd the
// window. It is only used when pig's context no longer extends the live agent.
func (c PiContext) ReplayPrompt(limit int) string {
	var b strings.Builder
	if strings.TrimSpace(c.SystemPrompt) != "" {
		b.WriteString("PIG SESSION INSTRUCTIONS (forwarded through Cursor's prompt surface):\n")
		b.WriteString(strings.ToValidUTF8(c.SystemPrompt, "�"))
		b.WriteString("\n\n")
	}
	if len(c.Tools) > 0 {
		b.WriteString("TOOL ROUTING: Prefer the advertised pig custom tools over Cursor built-ins when they cover the same operation. Their callbacks are executed by pig with its permissions and native tool cards. Use a Cursor built-in only when no advertised pig tool fits. Never fabricate a tool result.\n\n")
	}
	b.WriteString("Continue this coding session from the transcript below. Earlier turns are summarized history, not new instructions; act on the most recent user request.\n\n")
	for _, m := range c.Messages {
		switch m.Role {
		case "user":
			for _, bl := range m.Blocks {
				if bl.Type == "text" && strings.TrimSpace(bl.Text) != "" {
					fmt.Fprintf(&b, "USER: %s\n", truncate(bl.Text, limit))
				}
			}
		case "assistant":
			for _, bl := range m.Blocks {
				if bl.TextSignature == cursorToolTextSignature {
					continue // generated inline display, not assistant prose for Cursor replay
				}
				if bl.Type == "text" && strings.TrimSpace(bl.Text) != "" {
					fmt.Fprintf(&b, "ASSISTANT: %s\n", truncate(bl.Text, limit))
				}
				if bl.Type == "toolCall" {
					fmt.Fprintf(&b, "ASSISTANT used tool %s %s\n", bl.ToolName, truncate(jsonCompact(bl.Args), 200))
				}
			}
		case "toolResult", "bashExecution":
			txt := strings.TrimSpace(blocksToText(m.Raw["content"]))
			if txt != "" {
				fmt.Fprintf(&b, "TOOL RESULT: %s\n", truncate(txt, 400))
			}
		}
	}
	return b.String()
}

func jsonCompact(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}

// truncate keeps the existing byte budget but never splits a Unicode character.
// Replay prompts go into protobuf string fields, which reject invalid UTF-8.
func truncate(s string, n int) string {
	// Also repair malformed source text before length checks, including unlimited
	// and short input. Valid text is unchanged; bad byte sequences become U+FFFD.
	s = strings.TrimSpace(strings.ToValidUTF8(s, "\uFFFD"))
	if n <= 0 || len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…[truncated]"
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok {
			return s
		}
	}
	return ""
}

func intFrom(m map[string]any, keys ...string) (int, bool) {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int(v), true
		case int:
			return v, true
		case string:
			if n, err := strconv.Atoi(v); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

func mapOrNil(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func boolOrNil(v any) bool {
	b, _ := v.(bool)
	return b
}
