package jsonrpc

import "testing"

func TestKindClassification(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Kind
	}{
		{"request", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"}}`, KindRequest},
		{"notification", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, KindNotification},
		{"response_result", `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`, KindResponse},
		{"response_error", `{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"boom"}}`, KindResponse},
		{"null_id_notification", `{"jsonrpc":"2.0","id":null,"method":"ping"}`, KindNotification},
		{"string_id", `{"jsonrpc":"2.0","id":"abc","method":"tools/list"}`, KindRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, ok := Parse([]byte(c.in))
			if !ok {
				t.Fatalf("parse failed for %q", c.in)
			}
			if got := m.Kind(); got != c.want {
				t.Fatalf("Kind() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, ok := Parse([]byte("not json at all")); ok {
		t.Fatal("expected parse to fail on non-JSON")
	}
}

func TestToolName(t *testing.T) {
	m, _ := Parse([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{}}}`))
	if got := m.ToolName(); got != "search" {
		t.Fatalf("ToolName() = %q, want search", got)
	}
	other, _ := Parse([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if got := other.ToolName(); got != "" {
		t.Fatalf("ToolName() = %q, want empty", got)
	}
}

func TestIDKeyStable(t *testing.T) {
	reqMsg, _ := Parse([]byte(`{"jsonrpc":"2.0","id":42,"method":"x"}`))
	respMsg, _ := Parse([]byte(`{"jsonrpc":"2.0","id":42,"result":{}}`))
	if reqMsg.IDKey() != respMsg.IDKey() {
		t.Fatalf("id keys differ: %q vs %q", reqMsg.IDKey(), respMsg.IDKey())
	}
}
