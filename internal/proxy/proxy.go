// Package proxy wraps an MCP stdio server, relaying traffic verbatim between
// the parent (an MCP client such as Claude Desktop or Cursor) and the child
// server process, while tapping the JSON-RPC stream to produce metrics.
//
// Design constraints:
//   - Never mutate or drop traffic. Raw bytes are forwarded before observation.
//   - Observation failures must never affect the stream.
//   - Bidirectional: both client->server and server->client can carry requests,
//     responses, and notifications, so correlation is direction-agnostic.
package proxy

import (
	"bufio"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/ujai/mcpobserve/internal/jsonrpc"
	"github.com/ujai/mcpobserve/internal/metrics"
)

const serverName = "mcpobserve"

type pending struct {
	method string
	tool   string
	start  time.Time
}

// Proxy relays a single MCP stdio session.
type Proxy struct {
	reg     *metrics.Registry
	log     *slog.Logger
	pending sync.Map // id key -> pending
}

// New creates a Proxy that records into reg and logs structured events to log.
func New(reg *metrics.Registry, log *slog.Logger) *Proxy {
	return &Proxy{reg: reg, log: log}
}

// Run launches command with args, wires it to the given client streams, and
// blocks until the stream closes or the child exits. It returns the child's
// exit code (0 on clean stdin EOF) and any startup error.
func (p *Proxy) Run(command string, args []string, clientIn io.Reader, clientOut io.Writer, serverErr io.Writer) (int, error) {
	cmd := exec.Command(command, args...)
	cmd.Stderr = serverErr // pass server logs straight through

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

	var wg sync.WaitGroup
	wg.Add(2)

	// client -> server
	go func() {
		defer wg.Done()
		p.relay(clientIn, serverStdin, "c2s")
		// Closing the server's stdin signals end-of-input to it.
		_ = serverStdin.Close()
	}()

	// server -> client
	go func() {
		defer wg.Done()
		p.relay(serverStdout, clientOut, "s2c")
	}()

	wg.Wait()
	err = cmd.Wait()
	p.reg.SetGauge("mcp_up", "1 if the wrapped MCP server process is running.", labels(), 0)

	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		code = 1
	}
	p.log.Info("server exited", "code", code)
	return code, nil
}

// relay copies newline-delimited messages from src to dst, forwarding raw bytes
// first and then observing a parsed copy. Uses bufio.Reader.ReadBytes to avoid
// the token-size ceiling of bufio.Scanner (MCP tool results can be large).
func (p *Proxy) relay(src io.Reader, dst io.Writer, dir string) {
	r := bufio.NewReader(src)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			// Forward verbatim, immediately. Transparency first.
			if _, werr := dst.Write(line); werr != nil {
				p.log.Warn("forward write failed", "dir", dir, "err", werr)
				return
			}
			p.observe(line, dir)
		}
		if err != nil {
			if err != io.EOF {
				p.log.Warn("read failed", "dir", dir, "err", err)
			}
			return
		}
	}
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
		p.pending.Store(msg.IDKey(), pending{
			method: msg.Method,
			tool:   msg.ToolName(),
			start:  time.Now(),
		})
		p.reg.AddGauge("mcp_active_requests", "In-flight JSON-RPC requests.", labels(), 1)

	case jsonrpc.KindNotification:
		p.reg.IncCounter("mcp_notifications_total",
			"JSON-RPC notifications observed.",
			map[string]string{"dir": dir, "method": msg.Method})

	case jsonrpc.KindResponse:
		v, found := p.pending.LoadAndDelete(msg.IDKey())
		if !found {
			// Response with no matching request we recorded; count and move on.
			p.reg.IncCounter("mcp_orphan_responses_total",
				"Responses with no correlated request.", labels())
			return
		}
		pr := v.(pending)
		p.reg.AddGauge("mcp_active_requests", "In-flight JSON-RPC requests.", labels(), -1)

		status := "ok"
		if msg.Error != nil {
			status = "error"
			p.reg.IncCounter("mcp_errors_total",
				"JSON-RPC error responses by method and code.",
				map[string]string{"method": pr.method, "code": itoa(msg.Error.Code)})
		}

		lbl := map[string]string{"method": pr.method, "status": status}
		if pr.tool != "" {
			lbl["tool"] = pr.tool
		}
		p.reg.IncCounter("mcp_requests_total",
			"JSON-RPC requests completed, by method, tool, and status.", lbl)

		durLbl := map[string]string{"method": pr.method}
		if pr.tool != "" {
			durLbl["tool"] = pr.tool
		}
		p.reg.ObserveHistogram("mcp_request_duration_seconds",
			"Request-to-response latency in seconds.", durLbl,
			time.Since(pr.start).Seconds())

	default:
		p.reg.IncCounter("mcp_unparsed_messages_total",
			"Messages that could not be classified.", map[string]string{"dir": dir})
	}
}

func labels() map[string]string { return map[string]string{"server": serverName} }

func itoa(i int) string {
	// small helper to avoid importing strconv across files for one use
	neg := i < 0
	if neg {
		i = -i
	}
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
