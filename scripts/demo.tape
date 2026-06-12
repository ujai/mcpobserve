# demo.tape — VHS script for the README demo GIF.
#
# Generate with:  go install github.com/charmbracelet/vhs@latest  (or: pacman -S vhs)
#                 make build && (cd testdata/fakeserver && go build -o ../../bin/fakeserver .)
#                 vhs scripts/demo.tape
# Output: demo.gif (link it from README under the architecture diagram).

Output demo.gif
Set FontSize 16
Set Width 1200
Set Height 600
Set Theme "Catppuccin Latte"
Set TypingSpeed 40ms

Type "# Wrap any MCP stdio server — zero code changes, zero dependencies"
Enter
Sleep 1s

# Keep stdin open after the request so the proxy (and its /metrics endpoint)
# stays alive while we scrape it.
Type `(echo '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{"query":"hi"}}}'; sleep 12) | ./bin/mcpobserve -q -- ./bin/fakeserver &`
Enter
Sleep 2.5s

Type "# The stream passed through untouched. Now the part that's new:"
Enter
Sleep 1s

Type "curl -s http://127.0.0.1:9464/metrics | grep mcp_requests_total"
Enter
Sleep 3s

Type "# Prometheus metrics for a server that never had any."
Enter
Sleep 2s
