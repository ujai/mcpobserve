#!/usr/bin/env bash
# Smoke test: wrap the fake MCP server with mcpobserve, push a handful of
# JSON-RPC requests through stdin, and verify the metrics endpoint reflects them.
set -euo pipefail

# Port is overridable for CI; the default adds a small random offset so
# repeated/parallel local runs don't collide on a fixed port.
PORT="${SMOKE_PORT:-$((9470 + RANDOM % 500))}"
ADDR="127.0.0.1:$PORT"
PROXY="./bin/mcpobserve"
FAKE="./bin/fakeserver"
STDOUT_FILE="$(mktemp -t mcpobserve_smoke.XXXXXX)"
PROXY_PID=""

cleanup() {
  [ -n "$PROXY_PID" ] && kill "$PROXY_PID" 2>/dev/null || true
  [ -n "$PROXY_PID" ] && wait "$PROXY_PID" 2>/dev/null || true
  rm -f "$STDOUT_FILE"
}
trap cleanup EXIT

# Feed requests on stdin: two good tool calls, one that triggers an error,
# one tools/list, and a notification. Keep stdin open briefly so responses flow.
{
  printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search"}}'
  printf '%s\n' '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search"}}'
  printf '%s\n' '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"explode"}}'
  printf '%s\n' '{"jsonrpc":"2.0","id":4,"method":"tools/list"}'
  printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/initialized"}'
  sleep 1
} | "$PROXY" --metrics-addr "$ADDR" --quiet -- "$FAKE" > "$STDOUT_FILE" &

PROXY_PID=$!
sleep 0.4  # let metrics server bind and requests round-trip

OUT="$(curl -s "http://$ADDR/metrics" || true)"

echo "----- /metrics -----"
echo "$OUT"
echo "--------------------"

fail=0
check() {
  if echo "$OUT" | grep -qF "$1"; then
    echo "PASS: $1"
  else
    echo "FAIL (missing): $1"
    fail=1
  fi
}

# Label sets are consistent: tool="" outside tools/call.
check 'mcp_requests_total{method="tools/call",status="ok",tool="search"} 2'
check 'mcp_requests_total{method="tools/call",status="error",tool="explode"} 1'
check 'mcp_requests_total{method="tools/list",status="ok",tool=""} 1'
check 'mcp_errors_total{code="-32000",method="tools/call"} 1'
check 'mcp_notifications_total{dir="c2s",method="notifications/initialized"} 1'
check 'mcp_up{server="mcpobserve"}'
check 'mcp_request_duration_seconds_count{method="tools/call",tool="search"} 2'

# Verify protocol passthrough: client should have seen the server's responses.
if grep -qF '"id":1' "$STDOUT_FILE" && grep -qF '"id":3' "$STDOUT_FILE"; then
  echo "PASS: responses forwarded to client stdout"
else
  echo "FAIL: responses not forwarded"
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  echo "SMOKE TEST FAILED"
  exit 1
fi
echo "SMOKE TEST PASSED"
