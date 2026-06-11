# mcpobserve

**Prometheus metrics for any MCP stdio server — with zero code changes to the server, and zero dependencies in the tool.**

MCP servers are now wired into Claude, Cursor, Copilot, and a growing pile of agents. Most of them expose no `/metrics`, emit no structured logs, and can't be traced by your existing stack. When an agent slows down, loops, or starts failing tool calls, the server is a black box. `mcpobserve` makes it observable.

It's a transparent proxy: you put it in front of your MCP server, it spawns the real server as a child process, forwards stdio **verbatim** in both directions, and taps the JSON-RPC stream to publish Prometheus metrics on a local endpoint. The client talks to it exactly as it talked to the server. Nothing about the protocol changes.

```
┌────────────┐  stdio   ┌──────────────┐  stdio   ┌─────────────┐
│ MCP client │ ───────► │  mcpobserve  │ ───────► │ MCP server  │
│ (Claude…)  │ ◄─────── │   (proxy)    │ ◄─────── │ (unchanged) │
└────────────┘          └──────┬───────┘          └─────────────┘
                               │ :9464/metrics
                               ▼
                         Prometheus / Grafana
```

## Why this exists

The MCP ecosystem has plenty of *security scanners* that inspect a server at rest — tool descriptions, secrets, prompt-injection patterns. What's missing is *runtime* observability: what is actually happening when the agent is live. `mcpobserve` is built for the operations side of the problem — latency, error rates, call volume, in-flight requests, per-tool breakdowns — the things an SRE needs to keep an agent stack healthy in production.

It is deliberately **dependency-free** (Go standard library only). The entire tool is auditable in one sitting, runs offline, ships as a single static binary, and sends no telemetry anywhere. That's a requirement, not a feature: you should not have to trust a third party to observe your own infrastructure.

## Install

```bash
go install github.com/ujai/mcpobserve@latest
```

Or build from source:

```bash
git clone https://github.com/ujai/mcpobserve
cd mcpobserve
make build      # produces ./bin/mcpobserve
```

## Use

Wrap any stdio MCP server by putting `mcpobserve --` in front of its command:

```bash
mcpobserve -- npx -y @modelcontextprotocol/server-filesystem /tmp
```

Then scrape metrics:

```bash
curl -s http://127.0.0.1:9464/metrics
```

### In an MCP client config

Take your existing server entry and move the command after `--`:

```jsonc
{
  "mcpServers": {
    "filesystem": {
      "command": "mcpobserve",
      "args": ["--", "npx", "-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
    }
  }
}
```

The client still launches one process and talks stdio to it. That process is now `mcpobserve`, which transparently runs your server underneath and exposes metrics on the side.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--metrics-addr ADDR` | `127.0.0.1:9464` | Address for the Prometheus endpoint. Loopback by default. |
| `--log-file PATH` | stderr | Write the structured JSON event log here. Never written to stdout. |
| `--quiet`, `-q` | off | Log warnings and errors only. |
| `--version`, `-v` | | Print version and exit. |

The metrics address also serves a liveness probe at `/healthz`.

## Metrics

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `mcp_requests_total` | counter | `method`, `tool`, `status` | Completed JSON-RPC requests. `status` is `ok`/`error`; `tool` is set for `tools/call`. |
| `mcp_request_duration_seconds` | histogram | `method`, `tool` | Request-to-response latency. |
| `mcp_errors_total` | counter | `method`, `code` | JSON-RPC error responses by error code. |
| `mcp_active_requests` | gauge | `server` | In-flight requests. A persistently rising value is a stuck or looping agent. |
| `mcp_notifications_total` | counter | `dir`, `method` | Notifications observed, by direction (`c2s`/`s2c`). |
| `mcp_up` | gauge | `server` | `1` while the wrapped server process is running. |
| `mcp_orphan_responses_total` | counter | `server` | Responses with no correlated request (protocol anomaly signal). |
| `mcp_unparsed_messages_total` | counter | `dir` | Stream bytes that didn't parse as JSON-RPC. |

Label cardinality is bounded by design — `tool` and `method` come from a finite protocol surface, never from free-form input.

## Design notes

- **Transparency first.** Raw bytes are forwarded *before* they're parsed. A parse failure, an unknown method, or a malformed line is counted and ignored — it can never corrupt or stall the stream.
- **Direction-agnostic correlation.** MCP is bidirectional: the server can call the client (sampling, elicitation) too. Requests are correlated to responses by JSON-RPC id regardless of which way they flow.
- **No scanner buffer ceiling.** Messages are read with `bufio.Reader.ReadBytes` rather than `Scanner`, so large tool results don't get truncated.
- **stdout is sacred.** It carries the protocol. All logging goes to stderr or a file.

## Roadmap

v0.1 is intentionally small and solid. Planned next, roughly in order:

- [ ] Native **OTLP trace export** (one span per request, OTel GenAI semantic conventions) so MCP calls show up alongside the rest of your traces.
- [ ] A ready-made **Grafana dashboard** JSON.
- [ ] **Streamable HTTP / SSE** transport support (v0.1 covers stdio, the common local case).
- [ ] Token/cost attribution where the server surfaces usage.
- [ ] Optional **per-tool argument shape** sampling (privacy-preserving) for capability mapping.

Issues and PRs welcome. If you're running MCP servers in production and hitting blind spots, I'd like to hear what you need to see.

## Testing

```bash
make test          # unit tests
make smoke         # builds, wraps the bundled fake server, asserts /metrics output
```

## Who made this

Built by Abu Dzar, a Site Reliability Engineer working on observability and security for the agent stack. If your team is running MCP servers or AI agents in production and wants a hand making them reliable and auditable, see [abudzar.io](https://abudzar.io).

## License

MIT — see [LICENSE](LICENSE).
