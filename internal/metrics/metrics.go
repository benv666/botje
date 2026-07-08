// Package metrics is a tiny Prometheus text-exposition registry: enough
// counters and gauges for a Grafana dashboard without pulling in
// client_golang (same few-deps choice as the hand-rolled sigv4). Series
// are (name, label set); collectors refresh dynamic values (like the
// bus callstats) at scrape time.
package metrics

import (
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
)

type seriesType int

const (
	counter seriesType = iota
	gauge
)

func (t seriesType) String() string {
	if t == gauge {
		return "gauge"
	}
	return "counter"
}

type series struct {
	name   string
	labels map[string]string
	typ    seriesType
	value  float64
}

// Registry holds series and scrape-time collectors.
type Registry struct {
	mu         sync.Mutex
	series     map[string]*series // key = name + sorted labels
	collectors []func()
}

func New() *Registry {
	return &Registry{series: make(map[string]*series)}
}

func key(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	names := slices.Sorted(maps.Keys(labels))
	var b strings.Builder
	b.WriteString(name)
	for _, n := range names {
		b.WriteByte('\x00')
		b.WriteString(n)
		b.WriteByte('=')
		b.WriteString(labels[n])
	}
	return b.String()
}

func (r *Registry) at(name string, labels map[string]string, typ seriesType) *series {
	k := key(name, labels)
	s := r.series[k]
	if s == nil {
		s = &series{name: name, labels: maps.Clone(labels), typ: typ}
		r.series[k] = s
	}
	return s
}

// IncCounter adds 1 to a counter series.
func (r *Registry) IncCounter(name string, labels map[string]string) {
	r.AddCounter(name, labels, 1)
}

// AddCounter adds v to a counter series.
func (r *Registry) AddCounter(name string, labels map[string]string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.at(name, labels, counter).value += v
}

// SetCounter sets a counter to an absolute value: for counters
// recomputed from an external source each scrape (e.g. bus callstats).
func (r *Registry) SetCounter(name string, labels map[string]string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.at(name, labels, counter).value = v
}

// SetGauge sets a gauge series.
func (r *Registry) SetGauge(name string, labels map[string]string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.at(name, labels, gauge).value = v
}

// AddCollector registers a function run before each scrape, to refresh
// dynamic series. Collectors call the Set* methods.
func (r *Registry) AddCollector(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.collectors = append(r.collectors, fn)
}

// WriteText runs collectors then writes the Prometheus text format.
func (r *Registry) WriteText(w io.Writer) {
	r.mu.Lock()
	collectors := slices.Clone(r.collectors)
	r.mu.Unlock()
	for _, fn := range collectors {
		fn()
	}

	r.mu.Lock()
	all := slices.Collect(maps.Values(r.series))
	r.mu.Unlock()

	// group by name for one TYPE line each, deterministic order
	sort.Slice(all, func(i, j int) bool {
		if all[i].name != all[j].name {
			return all[i].name < all[j].name
		}
		return key(all[i].name, all[i].labels) < key(all[j].name, all[j].labels)
	})
	lastName := ""
	for _, s := range all {
		if s.name != lastName {
			fmt.Fprintf(w, "# TYPE %s %s\n", s.name, s.typ)
			lastName = s.name
		}
		fmt.Fprintf(w, "%s%s %s\n", s.name, formatLabels(s.labels), formatValue(s.value))
	}
}

// Handler serves the exposition at, conventionally, /metrics.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.WriteText(w)
	})
}

func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	names := slices.Sorted(maps.Keys(labels))
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(labels[n]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func escapeLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

// formatValue renders a float without a trailing .0 for integers, the
// way Prometheus examples do.
func formatValue(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}
