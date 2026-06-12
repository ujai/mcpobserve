// Package proxy wraps an MCP stdio server, relaying traffic verbatim between
// the parent (an MCP client such as Claude Desktop or Cursor) and the child
// server process, while tapping the JSON-RPC stream to produce metrics.
//
// Design constraints:
//   - Never mutate or drop traffic. Raw bytes are forwarded before observation.
//   - Observation failures must never affect the stream.
//   - Bidirectional: both client->server and server->client can carry requests,
//     responses, and notifications. Correlation is direction-scoped: a response
//     flowing one way matches a request that flowed the other way.
//   - Bounded memory: observation buffers and the pending-request map have hard
//     caps; forwarding itself is never capped.
package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ujai/mcpobserve/internal/jsonrpc"
	"github.com/ujai/mcpobserve/internal/metrics"
)

const serverName = "mcpobserve"

// Defaults for the tunable bounds. Exposed as Proxy fields so main can wire
// flags and tests can tighten them.
const (
	DefaultMaxObserveBytes = 64 << 20 // parse cap per message; forwarding is unbounded
	DefaultMaxPending      = 10000    // in-flight request correlation entries
	DefaultWaitDelay       = 5 * time.Second
)

type pendingKey struct {
	origin string // direction the request flowed: "c2s" or "s2c"
	id     string
}

type pending struct {
	method string
	tool   string
	start  time.Time
}

// Proxy relays a single MCP stdio session.
type Proxy struct {
	reg *metrics.Registry
	log *slog.Logger

	// MaxObserveBytes caps how large a single message may be and still get
	// parsed for metrics. Larger messages are forwarded verbatim but counted
	// as oversize instead of observed.
	MaxObserveBytes int
	// MaxPending caps the request-correlation map. At the cap, new requests
	// are forwarded but not correlated (counted as dropped correlations).
	MaxPending int
	// WaitDelay is the grace period between SIGTERM and SIGKILL when the
	// context is cancelled, and the cap on waiting for I/O after child exit.
	WaitDelay time.Duration

	pending      sync.Map // pendingKey -> pending
	pendingCount atomic.Int64
}

// New creates a Proxy that records into reg and logs structured events to log.
func New(reg *metrics.Registry, log *slog.Logger) *Proxy {
	return &Proxy{
		reg:             reg,
		log:             log,
		MaxObserveBytes: DefaultMaxObserveBytes,
		MaxPending:      DefaultMaxPending,
		WaitDelay:       DefaultWaitDelay,
	}
}

// Run launches command with args, wires it to the given client streams, and
// blocks until the child exits, the stream closes, or ctx is cancelled. On
// cancellation the child gets SIGTERM, then SIGKILL after WaitDelay. It returns
// the child's exit code (0 on clean stdin EOF) and any startup error.
func (p *Proxy) Run(ctx context.Context, command string, args []string, clientIn io.Reader, clientOut io.Writer, serverErr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stderr = serverErr // pass server logs straight through
	// Graceful shutdown on ctx cancel: SIGTERM first; WaitDelay escalates to
	// SIGKILL and force-closes pipes if the child ignores it.
	cmd.Cancel = func() error {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = p.WaitDelay

	serverStdin, err := cmd.StdinPipe()
	if err != nil {
		return 1, err
	}
	serverStdout, err := cmd.StdoutPipe()
	if err != nil {
		return 1, err
	}

	if err := cmd.Start(); err != nil {
		return 1, err
	}
	p.reg.SetGauge("mcp_up", "1 if the wrapped MCP server process is running.", labels(), 1)
	p.log.Info("server started", "command", command, "pid", cmd.Process.Pid)

	// client -> server. Deliberately never joined: if the child dies first,
	// this goroutine can stay blocked on client stdin forever. It becomes
	// harmless — writes to the dead child's stdin fail and end the relay.
	go func() {
		p.relay(clientIn, serverStdin, "c2s")
		// Closing the server's stdin signals end-of-input to it.
		_ = serverStdin.Close()
	}()

	// server -> client. This is the goroutine whose completion gates Wait:
	// it ends when the child exits (stdout EOF) or its stdout breaks, which
	// is exactly when reading from StdoutPipe is done.
	s2cDone := make(chan struct{})
	go func() {
		p.relay(serverStdout, clientOut, "s2c")
		close(s2cDone)
	}()

	select {
	case <-s2cDone:
	case <-ctx.Done():
		// cmd.Cancel has signalled the child. Stop feeding it and give the
		// s2c relay until WaitDelay (plus slack) to drain; WaitDelay then
		// guarantees Wait cannot block forever.
		_ = serverStdin.Close()
		select {
		case <-s2cDone:
		case <-time.After(p.WaitDelay + time.Second):
			p.log.Warn("server stdout did not close after cancellation grace period")
		}
	}

	err = cmd.Wait()
	p.endSession()

	code := 0
	if ee := new(exec.ExitError); errors.As(err, &ee) {
		code = ee.ExitCode()
		if code < 0 { // killed by signal
			code = 1
		}
	} else if err != nil {
		code = 1
	}
	p.log.Info("server exited", "code", code)
	return code, nil
}

// endSession clears correlation state after the child exits: every still-
// pending request is abandoned, and the in-flight gauge is reset so it cannot
// drift across sessions.
func (p *Proxy) endSession() {
	abandoned := 0
	p.pending.Range(func(k, _ any) bool {
		p.pending.Delete(k)
		abandoned++
		return true
	})
	if abandoned > 0 {
		p.pendingCount.Add(int64(-abandoned))
		p.reg.AddCounter("mcp_abandoned_requests_total",
			"Requests still awaiting a response when the session ended.", labels(), float64(abandoned))
	}
	p.reg.SetGauge("mcp_active_requests", "In-flight JSON-RPC requests.", labels(), 0)
	p.reg.SetGauge("mcp_up", "1 if the wrapped MCP server process is running.", labels(), 0)
}

// relay copies newline-delimited messages from src to dst, forwarding raw
// bytes first and then observing a parsed copy. It reads in bounded chunks
// (bufio.ErrBufferFull marks a partial line) so a single huge line cannot
// grow memory without limit: chunks are forwarded as they arrive, and the
// observation copy stops accumulating past MaxObserveBytes.
func (p *Proxy) relay(src io.Reader, dst io.Writer, dir string) {
	r := bufio.NewReader(src)
	var msg []byte // observation copy of the current line
	oversize := false
	for {
		chunk, err := r.ReadSlice('\n')
		if len(chunk) > 0 {
			// Forward verbatim, immediately. Transparency first.
			if werr := writeAll(dst, chunk); werr != nil {
				p.log.Warn("forward write failed", "dir", dir, "err", werr)
				return
			}
			if !oversize {
				if len(msg)+len(chunk) > p.MaxObserveBytes {
					oversize = true
					msg = nil
				} else {
					msg = append(msg, chunk...)
				}
			}
		}

		switch {
		case err == nil, err == io.EOF:
			// Line complete (or stream ended mid-line): observe the copy.
			if oversize {
				p.reg.IncCounter("mcp_oversize_messages_total",
					"Messages exceeding the parse size limit (forwarded but not observed).",
					map[string]string{"dir": dir})
			} else if len(msg) > 0 {
				p.observe(msg, dir)
			}
			msg = msg[:0]
			oversize = false
			if err == io.EOF {
				return
			}
		case err == bufio.ErrBufferFull:
			// Partial line; keep streaming chunks.
		default:
			if !errors.Is(err, os.ErrClosed) {
				p.log.Warn("read failed", "dir", dir, "err", err)
			}
			return
		}
	}
}

// writeAll handles short writes: io.Writer may legally return n < len(b) with
// a nil error, and a partial forward would corrupt the stream.
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

// observe parses a copy of the line and updates metrics. It must never panic or
// block the stream; all failures are swallowed (best-effort instrumentation).
func (p *Proxy) observe(raw []byte, dir string) {
	msg, ok := jsonrpc.Parse(raw)
	if !ok {
		p.reg.IncCounter("mcp_unparsed_messages_total",
			"Messages that could not be parsed as JSON-RPC.", map[string]string{"dir": dir})
		return
	}

	switch msg.Kind() {
	case jsonrpc.KindRequest:
		if p.pendingCount.Load() >= int64(p.MaxPending) {
			p.reg.IncCounter("mcp_dropped_correlations_total",
				"Requests forwarded but not correlated because the pending map is full.",
				map[string]string{"dir": dir})
			return
		}
		key := pendingKey{origin: dir, id: msg.IDKey()}
		entry := pending{method: msg.Method, tool: msg.ToolName(), start: time.Now()}
		if _, dup := p.pending.LoadOrStore(key, entry); dup {
			// Same id reused in the same direction before its response: a
			// protocol anomaly. Keep the first entry so the gauge can't drift.
			p.reg.IncCounter("mcp_duplicate_request_ids_total",
				"Requests reusing an id already in flight in the same direction.",
				map[string]string{"dir": dir})
			return
		}
		p.pendingCount.Add(1)
		p.reg.AddGauge("mcp_active_requests", "In-flight JSON-RPC requests.", labels(), 1)

	case jsonrpc.KindNotification:
		p.reg.IncCounter("mcp_notifications_total",
			"JSON-RPC notifications observed.",
			map[string]string{"dir": dir, "method": msg.Method})

	case jsonrpc.KindResponse:
		// A response travels the opposite direction from its request.
		key := pendingKey{origin: opposite(dir), id: msg.IDKey()}
		v, found := p.pending.LoadAndDelete(key)
		if !found {
			// Response with no matching request we recorded; count and move on.
			p.reg.IncCounter("mcp_orphan_responses_total",
				"Responses with no correlated request.", labels())
			return
		}
		pr := v.(pending)
		p.pendingCount.Add(-1)
		p.reg.AddGauge("mcp_active_requests", "In-flight JSON-RPC requests.", labels(), -1)

		status := "ok"
		if msg.Error != nil {
			status = "error"
			p.reg.IncCounter("mcp_errors_total",
				"JSON-RPC error responses by method and code.",
				map[string]string{"method": pr.method, "code": strconv.Itoa(msg.Error.Code)})
		}

		// Label sets are consistent across all methods: tool is "" outside
		// tools/call rather than absent.
		p.reg.IncCounter("mcp_requests_total",
			"JSON-RPC requests completed, by method, tool, and status.",
			map[string]string{"method": pr.method, "status": status, "tool": pr.tool})
		p.reg.ObserveHistogram("mcp_request_duration_seconds",
			"Request-to-response latency in seconds.",
			map[string]string{"method": pr.method, "tool": pr.tool},
			time.Since(pr.start).Seconds())

	default:
		p.reg.IncCounter("mcp_unparsed_messages_total",
			"Messages that could not be classified.", map[string]string{"dir": dir})
	}
}

func opposite(dir string) string {
	if dir == "c2s" {
		return "s2c"
	}
	return "c2s"
}

func labels() map[string]string { return map[string]string{"server": serverName} }
