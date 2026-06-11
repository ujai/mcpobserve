package proxy

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/ujai/mcpobserve/internal/metrics"
)

func newTestProxy() (*Proxy, *metrics.Registry) {
	reg := metrics.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(reg, log), reg
}

func dump(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	var b strings.Builder
	if err := reg.WritePrometheus(&b); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	return b.String()
}

func expectLine(t *testing.T, out, want string) {
	t.Helper()
	if !strings.Contains(out, want) {
		t.Errorf("metrics output missing %q\n%s", want, out)
	}
}

func TestObserveRequestResponseCorrelation(t *testing.T) {
	p, reg := newTestProxy()

	p.observe([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search"}}`), "c2s")
	out := dump(t, reg)
	expectLine(t, out, `mcp_active_requests{server="mcpobserve"} 1`)

	p.observe([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`), "s2c")
	out = dump(t, reg)
	expectLine(t, out, `mcp_active_requests{server="mcpobserve"} 0`)
	expectLine(t, out, `mcp_requests_total{method="tools/call",status="ok",tool="search"} 1`)
	expectLine(t, out, `mcp_request_duration_seconds_count{method="tools/call",tool="search"} 1`)
}

func TestObserveErrorResponse(t *testing.T) {
	p, reg := newTestProxy()

	p.observe([]byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"explode"}}`), "c2s")
	p.observe([]byte(`{"jsonrpc":"2.0","id":7,"error":{"code":-32000,"message":"boom"}}`), "s2c")

	out := dump(t, reg)
	expectLine(t, out, `mcp_requests_total{method="tools/call",status="error",tool="explode"} 1`)
	expectLine(t, out, `mcp_errors_total{code="-32000",method="tools/call"} 1`)
}

func TestObserveCorrelationIsDirectionAgnostic(t *testing.T) {
	p, reg := newTestProxy()

	// Server-initiated request (e.g. sampling), answered by the client.
	p.observe([]byte(`{"jsonrpc":"2.0","id":"srv-1","method":"sampling/createMessage"}`), "s2c")
	p.observe([]byte(`{"jsonrpc":"2.0","id":"srv-1","result":{}}`), "c2s")

	out := dump(t, reg)
	expectLine(t, out, `mcp_requests_total{method="sampling/createMessage",status="ok"} 1`)
}

func TestObserveOrphanResponse(t *testing.T) {
	p, reg := newTestProxy()

	p.observe([]byte(`{"jsonrpc":"2.0","id":99,"result":{}}`), "s2c")

	out := dump(t, reg)
	expectLine(t, out, `mcp_orphan_responses_total{server="mcpobserve"} 1`)
}

func TestObserveNotificationAndUnparsed(t *testing.T) {
	p, reg := newTestProxy()

	p.observe([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`), "c2s")
	p.observe([]byte("not json at all\n"), "s2c")

	out := dump(t, reg)
	expectLine(t, out, `mcp_notifications_total{dir="c2s",method="notifications/initialized"} 1`)
	expectLine(t, out, `mcp_unparsed_messages_total{dir="s2c"} 1`)
}

// TestRunForwardsVerbatim wraps `cat` as the child: everything written to the
// client side must come back byte-for-byte, including lines that don't parse.
func TestRunForwardsVerbatim(t *testing.T) {
	p, reg := newTestProxy()

	input := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n" +
		"garbage line\n"
	var clientOut strings.Builder

	code, err := p.Run("cat", nil, strings.NewReader(input), &clientOut, io.Discard)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if clientOut.String() != input {
		t.Errorf("output not verbatim:\ngot  %q\nwant %q", clientOut.String(), input)
	}

	out := dump(t, reg)
	expectLine(t, out, `mcp_up{server="mcpobserve"} 0`)
	expectLine(t, out, `mcp_unparsed_messages_total{dir="c2s"} 1`)
}

func TestRunPropagatesExitCode(t *testing.T) {
	p, _ := newTestProxy()

	code, err := p.Run("sh", []string{"-c", "exit 3"}, strings.NewReader(""), io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
}
