// Command fakeserver is a minimal stand-in MCP stdio server used to exercise
// the proxy in tests and demos. It reads newline-delimited JSON-RPC requests
// and replies, simulating a little latency and an occasional error.
//
// Not a real MCP server — just enough JSON-RPC shape to drive the proxy.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type req struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func main() {
	in := bufio.NewReader(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	for {
		line, err := in.ReadBytes('\n')
		if len(line) > 0 {
			var r req
			if json.Unmarshal(line, &r) == nil && len(r.ID) > 0 && string(r.ID) != "null" {
				time.Sleep(8 * time.Millisecond) // simulate work

				// Simulate an error for a specific tool to exercise error paths.
				if r.Method == "tools/call" && containsName(r.Params, "explode") {
					fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"boom"}}`+"\n", r.ID)
				} else {
					fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%s,"result":{"ok":true}}`+"\n", r.ID)
				}
				out.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func containsName(params json.RawMessage, name string) bool {
	var p struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(params, &p)
	return p.Name == name
}
