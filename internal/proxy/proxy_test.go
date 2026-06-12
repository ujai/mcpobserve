package proxy

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestObserveCorrelationIsBidirectional(t *testing.T) {
	p, reg := newTestProxy()

	// Server-initiated request (e.g. sampling), answered by the client.
	p.observe([]byte(`{"jsonrpc":"2.0","id":"srv-1","method":"sampling/createMessage"}`), "s2c")
	p.observe([]byte(`{"jsonrpc":"2.0","id":"srv-1","result":{}}`), "c2s")

	out := dump(t, reg)
	expectLine(t, out, `mcp_requests_total{method="sampling/createMessage",status="ok",tool=""} 1`)
}

// Client and server may each have their own request id=1 in flight at the same
// time. Correlation is scoped by origin direction, so neither clobbers the other.
func TestObserveSameIDBothDirections(t *testing.T) {
	p, reg := newTestProxy()

	p.observe([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), "c2s")
	p.observe([]byte(`{"jsonrpc":"2.0","id":1,"method":"sampling/createMessage"}`), "s2c")
	out := dump(t, reg)
	expectLine(t, out, `mcp_active_requests{server="mcpobserve"} 2`)

	// Each response matches the request that flowed the other way.
	p.observe([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`), "s2c") // answers tools/list
	p.observe([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`), "c2s") // answers sampling

	out = dump(t, reg)
	expectLine(t, out, `mcp_requests_total{method="tools/list",status="ok",tool=""} 1`)
	expectLine(t, out, `mcp_requests_total{method="sampling/createMessage",status="ok",tool=""} 1`)
	expectLine(t, out, `mcp_active_requests{server="mcpobserve"} 0`)
	if strings.Contains(out, "mcp_orphan_responses_total") {
		t.Errorf("no orphans expected:\n%s", out)
	}
}

func TestObserveDuplicateRequestID(t *testing.T) {
	p, reg := newTestProxy()

	p.observe([]byte(`{"jsonrpc":"2.0","id":5,"method":"tools/list"}`), "c2s")
	p.observe([]byte(`{"jsonrpc":"2.0","id":5,"method":"resources/list"}`), "c2s")

	out := dump(t, reg)
	expectLine(t, out, `mcp_duplicate_request_ids_total{dir="c2s"} 1`)
	// The gauge must not drift: one slot, one in-flight.
	expectLine(t, out, `mcp_active_requests{server="mcpobserve"} 1`)

	// The first entry wins; one response settles it.
	p.observe([]byte(`{"jsonrpc":"2.0","id":5,"result":{}}`), "s2c")
	out = dump(t, reg)
	expectLine(t, out, `mcp_requests_total{method="tools/list",status="ok",tool=""} 1`)
	expectLine(t, out, `mcp_active_requests{server="mcpobserve"} 0`)
}

func TestPendingCapDropsCorrelation(t *testing.T) {
	p, reg := newTestProxy()
	p.MaxPending = 2

	p.observe([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), "c2s")
	p.observe([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`), "c2s")
	p.observe([]byte(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`), "c2s")

	out := dump(t, reg)
	expectLine(t, out, `mcp_dropped_correlations_total{dir="c2s"} 1`)
	expectLine(t, out, `mcp_active_requests{server="mcpobserve"} 2`)
}

func TestEndSessionAbandonsPending(t *testing.T) {
	p, reg := newTestProxy()

	p.observe([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), "c2s")
	p.observe([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search"}}`), "c2s")
	p.endSession()

	out := dump(t, reg)
	expectLine(t, out, `mcp_abandoned_requests_total{server="mcpobserve"} 2`)
	expectLine(t, out, `mcp_active_requests{server="mcpobserve"} 0`)
	if got := p.pendingCount.Load(); got != 0 {
		t.Errorf("pendingCount after endSession = %d, want 0", got)
	}
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

	code, err := p.Run(context.Background(), "cat", nil, strings.NewReader(input), &clientOut, io.Discard)
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

	code, err := p.Run(context.Background(), "sh", []string{"-c", "exit 3"}, strings.NewReader(""), io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
}

// blockingReader models a real MCP client that holds stdin open without
// sending anything. Read blocks until the reader is closed.
type blockingReader struct {
	once sync.Once
	ch   chan struct{}
}

func newBlockingReader() *blockingReader { return &blockingReader{ch: make(chan struct{})} }
func (b *blockingReader) Read([]byte) (int, error) {
	<-b.ch
	return 0, io.EOF
}
func (b *blockingReader) Close() { b.once.Do(func() { close(b.ch) }) }

// runAsync runs p.Run in a goroutine and fails the test if it hasn't returned
// within the deadline — the regression shape for every lifecycle hang.
func runAsync(t *testing.T, deadline time.Duration, f func() (int, error)) (int, error) {
	t.Helper()
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := f()
		done <- result{code, err}
	}()
	select {
	case r := <-done:
		return r.code, r.err
	case <-time.After(deadline):
		t.Fatalf("Run did not return within %v", deadline)
		return 0, nil
	}
}

// The child exits while the client keeps stdin open: Run must still return
// the child's exit code instead of waiting on the client forever.
func TestRunReturnsWhenChildExitsAndClientStdinStaysOpen(t *testing.T) {
	p, reg := newTestProxy()
	in := newBlockingReader()
	defer in.Close()

	code, err := runAsync(t, 5*time.Second, func() (int, error) {
		return p.Run(context.Background(), "sh", []string{"-c", "exit 7"}, in, io.Discard, io.Discard)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
	expectLine(t, dump(t, reg), `mcp_up{server="mcpobserve"} 0`)
}

// Cancelling the context must terminate a child that would otherwise run forever.
func TestRunCancelTerminatesChild(t *testing.T) {
	p, _ := newTestProxy()
	in := newBlockingReader()
	defer in.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	code, err := runAsync(t, 5*time.Second, func() (int, error) {
		return p.Run(ctx, "sleep", []string{"30"}, in, io.Discard, io.Discard)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code == 0 {
		t.Errorf("exit code = 0, want nonzero for a terminated child")
	}
}

// A child that traps SIGTERM is force-killed after the WaitDelay grace period.
func TestRunKillsChildIgnoringSigterm(t *testing.T) {
	p, _ := newTestProxy()
	p.WaitDelay = 200 * time.Millisecond
	in := newBlockingReader()
	defer in.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	_, err := runAsync(t, 5*time.Second, func() (int, error) {
		return p.Run(ctx, "sh", []string{"-c", `trap "" TERM; sleep 30 & wait`}, in, io.Discard, io.Discard)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// An oversize line must be forwarded byte-for-byte even though it is never
// parsed, and counted instead of observed.
func TestRelayForwardsOversizeLineVerbatim(t *testing.T) {
	p, reg := newTestProxy()
	p.MaxObserveBytes = 1024

	big := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` +
		strings.Repeat("x", 8192) + `"}}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"
	var out strings.Builder
	p.relay(strings.NewReader(big), &out, "c2s")

	if out.String() != big {
		t.Errorf("oversize forward not verbatim: got %d bytes, want %d", out.Len(), len(big))
	}
	m := dump(t, reg)
	expectLine(t, m, `mcp_oversize_messages_total{dir="c2s"} 1`)
	// The small line after the oversize one is still observed normally.
	expectLine(t, m, `mcp_notifications_total{dir="c2s",method="notifications/initialized"} 1`)
	if strings.Contains(m, "mcp_active_requests{server=\"mcpobserve\"} 1") {
		t.Errorf("oversize request must not be parsed into pending:\n%s", m)
	}
}

// A line spanning multiple bufio chunks (>4096 bytes) but under the parse cap
// must be reassembled and observed normally — chunked reading is an internal
// detail, not a framing boundary.
func TestRelayReassemblesMultiChunkLine(t *testing.T) {
	p, reg := newTestProxy()

	line := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{"q":"` +
		strings.Repeat("y", 6000) + `"}}}` + "\n"
	var out strings.Builder
	p.relay(strings.NewReader(line), &out, "c2s")

	if out.String() != line {
		t.Errorf("multi-chunk forward not verbatim: got %d bytes, want %d", out.Len(), len(line))
	}
	m := dump(t, reg)
	expectLine(t, m, `mcp_active_requests{server="mcpobserve"} 1`)
	if strings.Contains(m, "mcp_oversize_messages_total") {
		t.Errorf("under-cap line wrongly counted oversize:\n%s", m)
	}
	if strings.Contains(m, "mcp_unparsed_messages_total") {
		t.Errorf("reassembled line failed to parse:\n%s", m)
	}
}

// zeroWriter models a pathological writer that returns (0, nil) — legal-looking
// but contract-breaking. writeAll must fail fast instead of spinning forever.
type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }

func TestWriteAllRejectsZeroProgressWriter(t *testing.T) {
	err := writeAll(zeroWriter{}, []byte("payload"))
	if err != io.ErrShortWrite {
		t.Errorf("writeAll on zero-progress writer: err = %v, want io.ErrShortWrite", err)
	}
}
