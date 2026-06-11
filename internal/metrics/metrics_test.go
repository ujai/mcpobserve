package metrics

import (
	"strings"
	"testing"
)

func TestCounterAndGauge(t *testing.T) {
	r := New()
	r.IncCounter("mcp_requests_total", "help", map[string]string{"method": "tools/call", "status": "ok"})
	r.IncCounter("mcp_requests_total", "help", map[string]string{"method": "tools/call", "status": "ok"})
	r.SetGauge("mcp_up", "help", map[string]string{"server": "mcpobserve"}, 1)

	var sb strings.Builder
	if err := r.WritePrometheus(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()

	if !strings.Contains(out, `mcp_requests_total{method="tools/call",status="ok"} 2`) {
		t.Fatalf("counter not aggregated correctly:\n%s", out)
	}
	if !strings.Contains(out, `mcp_up{server="mcpobserve"} 1`) {
		t.Fatalf("gauge missing:\n%s", out)
	}
	if !strings.Contains(out, "# TYPE mcp_requests_total counter") {
		t.Fatalf("missing TYPE line:\n%s", out)
	}
}

func TestHistogram(t *testing.T) {
	r := New()
	for _, v := range []float64{0.002, 0.02, 0.2, 2} {
		r.ObserveHistogram("mcp_request_duration_seconds", "help", map[string]string{"method": "x"}, v)
	}
	var sb strings.Builder
	if err := r.WritePrometheus(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()

	if !strings.Contains(out, "# TYPE mcp_request_duration_seconds histogram") {
		t.Fatalf("missing histogram type:\n%s", out)
	}
	if !strings.Contains(out, `mcp_request_duration_seconds_count{method="x"} 4`) {
		t.Fatalf("histogram count wrong:\n%s", out)
	}
	if !strings.Contains(out, `le="+Inf"`) {
		t.Fatalf("missing +Inf bucket:\n%s", out)
	}
	// 0.002 and 0.02 and 0.2 are <= 0.25; 2 is not. So le="0.25" should be 3.
	if !strings.Contains(out, `mcp_request_duration_seconds_bucket{le="0.25",method="x"} 3`) {
		t.Fatalf("cumulative bucket wrong:\n%s", out)
	}
}

func TestLabelEscaping(t *testing.T) {
	r := New()
	r.IncCounter("weird", "help", map[string]string{"k": `a"b\c`})
	var sb strings.Builder
	_ = r.WritePrometheus(&sb)
	if !strings.Contains(sb.String(), `k="a\"b\\c"`) {
		t.Fatalf("label not escaped:\n%s", sb.String())
	}
}
