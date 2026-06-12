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

> **Tip:** GUI clients (Claude Desktop, Cursor) usually don't inherit your shell's `PATH`. Use the absolute path from `which mcpobserve` as the `command` value.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--metrics-addr ADDR` | `127.0.0.1:9464` | Address for the Prometheus endpoint. Loopback by default; no auth — a non-loopback bind logs a warning. |
| `--log-file PATH` | stderr | Write the structured JSON event log here (created `0600`). Never written to stdout. |
| `--max-message-bytes N` | `67108864` (64 MiB) | Messages larger than this are forwarded untouched but not parsed for metrics (counted as oversize). |
| `--quiet`, `-q` | off | Log warnings and errors only. |
| `--version`, `-v` | | Print version and exit. |

The metrics address also serves a liveness probe at `/healthz`.

## Metrics

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `mcp_requests_total` | counter | `method`, `tool`, `status` | Completed JSON-RPC requests. `status` is `ok`/`error`; `tool` is the tool name for `tools/call` and `""` otherwise. |
| `mcp_request_duration_seconds` | histogram | `method`, `tool` | Request-to-response latency. `tool` is `""` outside `tools/call`. |
| `mcp_errors_total` | counter | `method`, `code` | JSON-RPC error responses by error code. |
| `mcp_active_requests` | gauge | `server` | In-flight requests. A persistently rising value is a stuck or looping agent. |
| `mcp_notifications_total` | counter | `dir`, `method` | Notifications observed, by direction (`c2s`/`s2c`). |
| `mcp_up` | gauge | `server` | `1` while the wrapped server process is running. |
| `mcp_orphan_responses_total` | counter | `server` | Responses with no correlated request (protocol anomaly signal). |
| `mcp_unparsed_messages_total` | counter | `dir` | Stream bytes that didn't parse as JSON-RPC. |
| `mcp_oversize_messages_total` | counter | `dir` | Messages larger than `--max-message-bytes`: forwarded untouched, not parsed. |
| `mcp_duplicate_request_ids_total` | counter | `dir` | Requests reusing an id already in flight in the same direction (protocol anomaly). |
| `mcp_abandoned_requests_total` | counter | `server` | Requests still awaiting a response when the session ended. |
| `mcp_dropped_correlations_total` | counter | `dir` | Requests forwarded but not correlated because the pending-request cap (10,000) was hit. |

A note on label cardinality, because SREs will rightly ask: `method` and `tool` values come from the observed traffic. Method names are drawn from MCP's small protocol surface, but tool names are defined by the wrapped server, so the practical cardinality is "however many tools your server exposes" — low for every normal server, but not enforced to be. What *is* enforced: messages beyond the parse cap are never labeled, the correlation map is hard-capped, and you choose which servers to wrap. If you wrap a server you don't trust to keep its tool list sane, watch `mcp_requests_total` series growth.

### Grafana dashboard

A ready-made dashboard covering all of the above (rates, error %, p50/p95/p99 latency, in-flight requests, protocol anomalies) ships at [`examples/grafana/mcpobserve-dashboard.json`](examples/grafana/mcpobserve-dashboard.json). Import it in Grafana (**Dashboards → New → Import**), point it at your Prometheus datasource, done.

## What it observes — and what it never logs

This tool sits on a stream that can carry sensitive data, so the boundary is explicit:

**Metrics record:** JSON-RPC method names, tool names, error codes, request-to-response timings, message direction, and process liveness. **The event log records:** proxy lifecycle, metrics-listener status, the wrapped server's command/pid/exit code, and warnings. All of it is protocol metadata. Request ids are used in memory for correlation but never recorded.

**Never recorded anywhere:** tool call *arguments*, tool *results*, resource contents, prompts, sampling messages — the payload. Payload bytes are forwarded verbatim and parsed only in memory to extract the metadata above; they are never written to the metrics endpoint, the event log, or disk.

**Never transmitted:** nothing leaves your machine. There is no telemetry, no update check, no outbound connection of any kind. The only network surface is the metrics listener you configure, bound to loopback by default. The listener has **no authentication** — if you bind it beyond loopback (the proxy logs a warning when you do), put a firewall in front of it.

Vulnerability reports: see [SECURITY.md](SECURITY.md).

## Design notes

- **Transparency first.** Raw bytes are forwarded *before* they're parsed. A parse failure, an unknown method, or a malformed line is counted and ignored — it can never corrupt or stall the stream. Even the `/metrics` endpoint renders from a snapshot, so a slow scraper can't back-pressure the proxy.
- **Direction-scoped correlation.** MCP is bidirectional: the server can call the client (sampling, elicitation) too. Requests from each side are tracked separately and matched to responses flowing the other way, so a client request `id=1` and a server request `id=1` can be in flight simultaneously without confusion.
- **Large messages forwarded, bounded observation.** Messages stream through in chunks (no `bufio.Scanner` token ceiling, no whole-message buffering), so arbitrarily large tool results pass untruncated. Only the *metrics tap* has a size cap (`--max-message-bytes`).
- **Best-effort classification.** Messages are classified by shape (id/method presence), not validated as strict JSON-RPC 2.0. Batch arrays — which MCP's stdio transport doesn't use — are counted as unparsed.
- **Graceful lifecycle.** SIGINT/SIGTERM are forwarded to the wrapped server (SIGKILL after a 5s grace period), and the proxy exits promptly when the server dies, even if the client keeps stdin open.
- **stdout is sacred.** It carries the protocol. All logging goes to stderr or a file.

## Roadmap

v0.1 is intentionally small and solid. Planned next, roughly in order:

- [ ] Native **OTLP trace export** (one span per request, OTel GenAI semantic conventions) so MCP calls show up alongside the rest of your traces.
- [x] A ready-made **Grafana dashboard** JSON — shipped at [`examples/grafana/mcpobserve-dashboard.json`](examples/grafana/mcpobserve-dashboard.json).
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
