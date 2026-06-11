// Package jsonrpc provides minimal parsing of JSON-RPC 2.0 messages as used by
// the MCP stdio transport. Messages are newline-delimited JSON objects.
//
// We deliberately parse only what we need to produce observability signals and
// never mutate or re-serialize traffic: the proxy forwards raw bytes verbatim
// and parses a copy. A malformed or unrecognized message must never block or
// corrupt the stream.
package jsonrpc

import "encoding/json"

// Message is a loosely-typed view over a JSON-RPC 2.0 message. Any of the
// fields may be absent depending on whether the message is a request, a
// response, or a notification.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *Error          `json:"error"`
}

// Error is the JSON-RPC error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Kind classifies a message by its shape.
type Kind int

const (
	// KindUnknown means the bytes did not parse as a JSON-RPC message.
	KindUnknown Kind = iota
	// KindRequest has both an id and a method.
	KindRequest
	// KindNotification has a method but no id.
	KindNotification
	// KindResponse has an id but no method (carries result or error).
	KindResponse
)

// Parse attempts to decode a single message. ok is false when the bytes are not
// a JSON object we recognize; callers should still forward the raw bytes.
func Parse(b []byte) (Message, bool) {
	var m Message
	if err := json.Unmarshal(b, &m); err != nil {
		return Message{}, false
	}
	return m, true
}

// HasID reports whether the message carries a non-null id.
func (m Message) HasID() bool {
	if len(m.ID) == 0 {
		return false
	}
	// JSON null id is treated as absent for matching purposes.
	return string(m.ID) != "null"
}

// Kind classifies the message.
func (m Message) Kind() Kind {
	hasMethod := m.Method != ""
	switch {
	case hasMethod && m.HasID():
		return KindRequest
	case hasMethod && !m.HasID():
		return KindNotification
	case !hasMethod && m.HasID():
		return KindResponse
	default:
		return KindUnknown
	}
}

// IDKey returns a stable string key for correlating a request with its
// response. The raw id bytes are used directly, which is robust because the
// same bytes are echoed back in the response.
func (m Message) IDKey() string {
	return string(m.ID)
}

// callParams is the shape of params for a tools/call request.
type callParams struct {
	Name string `json:"name"`
}

// ToolName extracts the tool name for a tools/call request, or "" otherwise.
func (m Message) ToolName() string {
	if m.Method != "tools/call" || len(m.Params) == 0 {
		return ""
	}
	var p callParams
	if err := json.Unmarshal(m.Params, &p); err != nil {
		return ""
	}
	return p.Name
}
