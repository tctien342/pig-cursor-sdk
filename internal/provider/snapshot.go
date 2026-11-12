package provider

// snapshotValue freezes JSON-shaped protocol containers before an asynchronous
// SDK handoff. Numbers keep their Go types (notably integer content indices).
// Emitter state is owned by its pump goroutine; copying must happen there, before
// the SDK can read or annotate a delivered message in another goroutine.
func snapshotValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = snapshotValue(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = snapshotValue(item)
		}
		return out
	case []map[string]any:
		out := make([]map[string]any, len(v))
		for i, item := range v {
			out[i] = snapshotMap(item)
		}
		return out
	case []string:
		return append([]string(nil), v...)
	default:
		return value // protocol scalars are immutable
	}
}

func snapshotMap(value map[string]any) map[string]any {
	return snapshotValue(value).(map[string]any)
}
