package cursor

import "strings"

// validText is a lossy fallback for human-readable strings: keep valid Unicode
// verbatim and replace malformed byte sequences with U+FFFD. Never apply this to
// routing identities, offsets, tool names or binary data.
func validText(s string) string { return strings.ToValidUTF8(s, "\uFFFD") }

// WithValidText repairs decoded human-readable fields without mutating the
// producer's event or its nested pointers. Machine identities and raw structured
// payloads are left untouched; this does not make arbitrary protocol data safe
// to serialize. A malformed protobuf frame rejected before Decode cannot be
// recovered by this post-decode fallback.
func (e Event) WithValidText() Event {
	e.Text = validText(e.Text)
	if e.Status != nil {
		copy := *e.Status
		copy.Message = validText(copy.Message)
		e.Status = &copy
	}
	if e.ToolCall != nil {
		copy := *e.ToolCall
		copy.Text = validText(copy.Text)
		e.ToolCall = &copy
	}
	if e.Result != nil {
		copy := *e.Result
		copy.Text = validText(copy.Text)
		copy.ErrorMessage = validText(copy.ErrorMessage)
		e.Result = &copy
	}
	if e.Task != nil {
		copy := *e.Task
		copy.Text = validText(copy.Text)
		e.Task = &copy
	}
	return e
}
