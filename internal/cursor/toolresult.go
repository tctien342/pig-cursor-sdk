package cursor

import "encoding/json"

// The installed bridge's normalizeCustomToolResult recognizes an envelope only
// when content is an array. A string-valued content object is otherwise rendered
// as JSON, and an RPC error becomes infrastructure failure rather than a normal
// tool result. Keep native pig data unchanged and build a separate SDK envelope.
func hostToolResult(payload map[string]any, isError bool, note string) map[string]any {
	content := make([]any, 0)
	var raw any
	var exists bool
	if payload != nil {
		raw, exists = payload["content"]
	}
	if !exists && payload != nil {
		raw, exists = payload["text"]
	}
	if !exists {
		raw = note
	}
	add := func(value any) {
		block, ok := value.(map[string]any)
		if !ok {
			encoded, _ := json.Marshal(value)
			content = append(content, map[string]any{"type": "text", "text": validText(string(encoded))})
			return
		}
		switch block["type"] {
		case "text":
			text, _ := block["text"].(string)
			content = append(content, map[string]any{"type": "text", "text": validText(text)})
		case "image":
			data, _ := block["data"].(string)
			mime, _ := block["mimeType"].(string)
			if data != "" && mime != "" {
				content = append(content, map[string]any{"type": "image", "data": data, "mimeType": mime})
			} else {
				content = append(content, map[string]any{"type": "text", "text": "[pig image result has no inline data or MIME type]"})
			}
		default:
			encoded, _ := json.Marshal(block)
			content = append(content, map[string]any{"type": "text", "text": validText(string(encoded))})
		}
	}
	switch value := raw.(type) {
	case string:
		content = append(content, map[string]any{"type": "text", "text": validText(value)})
	case []any:
		for _, block := range value {
			add(block)
		}
	case []map[string]any:
		for _, block := range value {
			add(block)
		}
	}
	result := map[string]any{"content": content, "isError": isError}
	if structured, ok := payload["structuredContent"].(map[string]any); ok {
		result["structuredContent"] = structured
	}
	return result
}
