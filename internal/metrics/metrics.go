// Package metrics implements a tiny, dependency-free metrics registry that
// emits the Prometheus text exposition format. It supports counters, gauges,
// and histograms with bounded label sets.
//
// This is intentionally minimal: no external client library, no global state.
// The whole thing is auditable in a few minutes, which matters for a tool that
// security teams are asked to run.
package metrics

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// DefaultBuckets are latency histogram upper bounds in seconds.
var DefaultBuckets = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

type metricType int

const (
	typeCounter metricType = iota
	typeGauge
	typeHistogram
)

type meta struct {
	help string
	typ  metricType
}

type histogram struct {
	bounds []float64
	counts []uint64
	sum    float64
	count  uint64
}

func newHistogram(bounds []float64) *histogram {
	return &histogram{bounds: bounds, counts: make([]uint64, len(bounds))}
}

func (h *histogram) observe(v float64) {
	for i, b := range h.bounds {
		if v <= b {
			h.counts[i]++
		}
	}
	h.sum += v
	h.count++
}

// Registry holds all metric series.
type Registry struct {
	mu       sync.Mutex
	meta     map[string]meta              // metric name -> help/type
	counters map[string]*labeledFloat     // series key -> value
	gauges   map[string]*labeledFloat     // series key -> value
	hists    map[string]*labeledHistogram // series key -> histogram
}

type labeledFloat struct {
	name   string
	labels map[string]string
	value  float64
}

type labeledHistogram struct {
	name   string
	labels map[string]string
	hist   *histogram
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		meta:     map[string]meta{},
		counters: map[string]*labeledFloat{},
		gauges:   map[string]*labeledFloat{},
		hists:    map[string]*labeledHistogram{},
	}
}

func seriesKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
	}
	b.WriteByte('}')
	return b.String()
}

// AddCounter increments a counter series by delta, registering it if needed.
func (r *Registry) AddCounter(name, help string, labels map[string]string, delta float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.meta[name] = meta{help: help, typ: typeCounter}
	key := seriesKey(name, labels)
	s, ok := r.counters[key]
	if !ok {
		s = &labeledFloat{name: name, labels: cloneLabels(labels)}
		r.counters[key] = s
	}
	s.value += delta
}

// IncCounter adds 1 to a counter series.
func (r *Registry) IncCounter(name, help string, labels map[string]string) {
	r.AddCounter(name, help, labels, 1)
}

// SetGauge sets a gauge series to v.
func (r *Registry) SetGauge(name, help string, labels map[string]string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.meta[name] = meta{help: help, typ: typeGauge}
	key := seriesKey(name, labels)
	s, ok := r.gauges[key]
	if !ok {
		s = &labeledFloat{name: name, labels: cloneLabels(labels)}
		r.gauges[key] = s
	}
	s.value = v
}

// AddGauge adds delta to a gauge series (delta may be negative).
func (r *Registry) AddGauge(name, help string, labels map[string]string, delta float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.meta[name] = meta{help: help, typ: typeGauge}
	key := seriesKey(name, labels)
	s, ok := r.gauges[key]
	if !ok {
		s = &labeledFloat{name: name, labels: cloneLabels(labels)}
		r.gauges[key] = s
	}
	s.value += delta
}

// ObserveHistogram records a value into a histogram series.
func (r *Registry) ObserveHistogram(name, help string, labels map[string]string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.meta[name] = meta{help: help, typ: typeHistogram}
	key := seriesKey(name, labels)
	s, ok := r.hists[key]
	if !ok {
		s = &labeledHistogram{name: name, labels: cloneLabels(labels), hist: newHistogram(DefaultBuckets)}
		r.hists[key] = s
	}
	s.hist.observe(v)
}

func cloneLabels(in map[string]string) map[string]string {
	if in == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// WritePrometheus writes all series in the Prometheus text exposition format.
// The registry lock is held only while rendering into a memory buffer, never
// while writing to w — a slow scraper must not block metric updates (which run
// synchronously on the relay path).
func (r *Registry) WritePrometheus(w io.Writer) error {
	var buf bytes.Buffer
	r.mu.Lock()
	err := r.render(&buf)
	r.mu.Unlock()
	if err != nil {
		return err
	}
	_, err = w.Write(buf.Bytes())
	return err
}

// render writes the exposition text. Callers must hold r.mu.
func (r *Registry) render(w io.Writer) error {
	// Gather metric names and emit HELP/TYPE once per name, grouped.
	names := make([]string, 0, len(r.meta))
	for n := range r.meta {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		m := r.meta[name]
		typeStr := map[metricType]string{typeCounter: "counter", typeGauge: "gauge", typeHistogram: "histogram"}[m.typ]
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, escapeHelp(m.help), name, typeStr); err != nil {
			return err
		}
		switch m.typ {
		case typeCounter:
			if err := writeFloatSeries(w, name, r.counters); err != nil {
				return err
			}
		case typeGauge:
			if err := writeFloatSeries(w, name, r.gauges); err != nil {
				return err
			}
		case typeHistogram:
			if err := writeHistSeries(w, name, r.hists); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeFloatSeries(w io.Writer, name string, m map[string]*labeledFloat) error {
	keys := make([]string, 0, len(m))
	for k, s := range m {
		if s.name == name {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := m[k]
		if _, err := fmt.Fprintf(w, "%s%s %s\n", name, formatLabels(s.labels), formatFloat(s.value)); err != nil {
			return err
		}
	}
	return nil
}

func writeHistSeries(w io.Writer, name string, m map[string]*labeledHistogram) error {
	keys := make([]string, 0, len(m))
	for k, s := range m {
		if s.name == name {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := m[k]
		// s.hist.counts[i] already holds the count of observations <= bounds[i]
		// (see histogram.observe), i.e. it is cumulative; emit it directly.
		for i, b := range s.hist.bounds {
			lbl := mergeLabel(s.labels, "le", formatFloat(b))
			if _, err := fmt.Fprintf(w, "%s_bucket%s %d\n", name, formatLabels(lbl), s.hist.counts[i]); err != nil {
				return err
			}
		}
		infLbl := mergeLabel(s.labels, "le", "+Inf")
		if _, err := fmt.Fprintf(w, "%s_bucket%s %d\n", name, formatLabels(infLbl), s.hist.count); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "%s_sum%s %s\n", name, formatLabels(s.labels), formatFloat(s.hist.sum)); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "%s_count%s %d\n", name, formatLabels(s.labels), s.hist.count); err != nil {
			return err
		}
	}
	return nil
}

func mergeLabel(base map[string]string, k, v string) map[string]string {
	out := cloneLabels(base)
	out[k] = v
	return out
}

func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(labels[k]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func escapeHelp(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

func escapeLabelValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

func formatFloat(f float64) string {
	// +Inf is handled by callers passing the literal; here format finite floats.
	return strconv.FormatFloat(f, 'g', -1, 64)
}
