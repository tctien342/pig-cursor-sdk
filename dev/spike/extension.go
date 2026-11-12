package spike

import (
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

func sawToolResult(context map[string]any) bool {
	messages, _ := context["messages"].([]any)
	for _, m := range messages {
		msg, _ := m.(map[string]any)
		if msg["role"] == "toolResult" {
			return true
		}
	}
	return false
}

// Extension proves the two host capabilities the Go rewrite depends on:
// a provider whose stream is produced in Go, and a tool card the host renders.
func Extension() *sdk.Extension {
	e := sdk.New("spike")

	e.Tool("spike_ping", "Return a short pong so you can confirm the extension is wired up.",
		sdk.Schema{"type": "object", "properties": map[string]any{}},
		func(ctx sdk.Context, params map[string]any) (any, error) {
			return "pong from spike", nil
		})

	e.RegisterProvider("spike", sdk.ProviderConfig{
		"name":    "Spike",
		"api":     "spike-api",
		"apiKey":  "spike-not-a-real-key",
		"baseUrl": "http://127.0.0.1:1",
		"models": []any{
			map[string]any{
				"id":            "spike-test",
				"name":          "Spike Test",
				"api":           "spike-api",
				"provider":      "spike",
				"contextWindow": 200000,
				"maxTokens":     8192,
				"reasoning":     true,
				"input":         []string{"text"},
				"cost":          map[string]any{"input": 0.0, "output": 0.0, "cacheRead": 0.0, "cacheWrite": 0.0},
			},
		},
		"streamSimple": sdk.ProviderStreamSimpleFunc(streamSpike),
	})

	return e
}

func streamSpike(ctx sdk.Context, model map[string]any, context map[string]any, options map[string]any) (*sdk.ModelEventStream, error) {
	stream := sdk.CreateAssistantMessageEventStream()

	partial := map[string]any{
		"role":       "assistant",
		"content":    []any{},
		"api":        "spike-api",
		"provider":   "spike",
		"model":      model["id"],
		"stopReason": "stop",
		"timestamp":  time.Now().UnixMilli(),
		"usage":      map[string]any{"input": 1, "output": 1, "cacheRead": 0, "cacheWrite": 0, "totalTokens": 2, "cost": map[string]any{"input": 0.0, "output": 0.0, "cacheRead": 0.0, "cacheWrite": 0.0, "total": 0.0}},
	}
	content := partial["content"].([]any)
	push := func(event map[string]any) {
		event["partial"] = partial
		stream.Push(event)
	}

	go func() {
		push(map[string]any{"type": "start"})

		// thinking block
		content = append(content, map[string]any{"type": "thinking", "thinking": ""})
		partial["content"] = content
		thinking := content[len(content)-1].(map[string]any)
		push(map[string]any{"type": "thinking_start", "contentIndex": len(content) - 1})
		for _, d := range []string{"The user ", "wants a ", "tool card. "} {
			thinking["thinking"] = thinking["thinking"].(string) + d
			push(map[string]any{"type": "thinking_delta", "contentIndex": len(content) - 1, "delta": d})
			time.Sleep(80 * time.Millisecond)
		}
		push(map[string]any{"type": "thinking_end", "contentIndex": len(content) - 1, "content": thinking["thinking"]})

		// text block
		content = append(content, map[string]any{"type": "text", "text": ""})
		partial["content"] = content
		text := content[len(content)-1].(map[string]any)
		push(map[string]any{"type": "text_start", "contentIndex": len(content) - 1})
		for _, d := range []string{"Calling ", "spike_ping ", "now."} {
			text["text"] = text["text"].(string) + d
			push(map[string]any{"type": "text_delta", "contentIndex": len(content) - 1, "delta": d})
			time.Sleep(60 * time.Millisecond)
		}
		push(map[string]any{"type": "text_end", "contentIndex": len(content) - 1, "content": text["text"]})

		// tool call block, only on the first turn, so the host runs a tool and we can close out
		if !sawToolResult(context) {
			args := `{}`
			content = append(content, map[string]any{"type": "toolCall", "id": "call_spike_1", "name": "spike_ping", "arguments": map[string]any{}})
			partial["content"] = content
			call := content[len(content)-1].(map[string]any)
			push(map[string]any{"type": "toolcall_start", "contentIndex": len(content) - 1, "id": "call_spike_1", "toolName": "spike_ping"})
			push(map[string]any{"type": "toolcall_delta", "contentIndex": len(content) - 1, "delta": args})
			push(map[string]any{"type": "toolcall_end", "contentIndex": len(content) - 1, "toolCall": call})
			partial["stopReason"] = "toolUse"
			push(map[string]any{"type": "done", "reason": "toolUse", "message": partial})
			stream.End(partial)
			return
		}

		content = append(content, map[string]any{"type": "text", "text": "The tool result came back through Go. Done."})
		partial["content"] = content
		partial["stopReason"] = "stop"
		push(map[string]any{"type": "done", "reason": "stop", "message": partial})
		stream.End(partial)
	}()

	return stream, nil
}
