// Package metrics holds tordownloader's process-wide counters and renders them,
// together with point-in-time gauges supplied by the caller, in the Prometheus
// text exposition format (version 0.0.4).
//
// It is deliberately tiny and dependency-free: a handful of monotonically
// increasing counters bumped from the engine and the TorBox client, plus a
// Writer the /metrics handler uses to emit both those counters and gauges it
// reads from the store at scrape time. Counters are package-level, like
// Prometheus's default registry, so the code paths that bump them need no
// plumbing.
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Process-wide counters. Label values must come from small fixed sets (a
// failure kind, an endpoint name, a status class) — never from user data such
// as a torrent name — so series cardinality stays bounded.
var (
	// DownloadedBytes counts bytes written to local disk by CDN transfers
	// (resumed bytes already on disk are not counted again).
	DownloadedBytes = NewCounter("tordownloader_downloaded_bytes_total",
		"Bytes downloaded from the TorBox CDN to local disk.")
	// Completed counts torrents that finished downloading to local disk.
	Completed = NewCounter("tordownloader_completed_total",
		"Torrents fully downloaded to local disk.")
	// Failures counts torrents moved to ERROR, by why.
	Failures = NewCounterVec("tordownloader_failures_total",
		"Torrents failed (moved to ERROR), by reason.", "reason")
	// LocalRetries counts failed local download attempts that were scheduled
	// for a retry rather than failing the torrent.
	LocalRetries = NewCounterVec("tordownloader_local_retries_total",
		"Failed local download attempts scheduled for retry, by cause.", "cause")
	// TorBoxRequests counts TorBox API responses, by endpoint and status class.
	TorBoxRequests = NewCounterVec("tordownloader_torbox_requests_total",
		"TorBox API requests, by endpoint and result (2xx, 4xx, 429, 5xx, error).", "endpoint", "result")
	// ArrNotifications counts failure push-back attempts to Sonarr/Radarr.
	ArrNotifications = NewCounterVec("tordownloader_arr_notifications_total",
		"Failure push-back attempts to Sonarr/Radarr, by result (blocklisted, error, not_tracked).", "result")
)

// Counter is a monotonically increasing value.
type Counter struct {
	name, help string
	v          atomic.Uint64
}

// NewCounter returns an unlabelled counter.
func NewCounter(name, help string) *Counter { return &Counter{name: name, help: help} }

// Inc adds one.
func (c *Counter) Inc() { c.v.Add(1) }

// Add adds n; non-positive n is ignored (counters never go down).
func (c *Counter) Add(n int64) {
	if n > 0 {
		c.v.Add(uint64(n))
	}
}

// Value returns the current count.
func (c *Counter) Value() uint64 { return c.v.Load() }

// CounterVec is a family of counters partitioned by label values.
type CounterVec struct {
	name, help string
	labels     []string

	mu   sync.Mutex
	vals map[string]*atomic.Uint64 // keyed by the label values joined with \x00
}

// NewCounterVec returns a counter family with the given label names.
func NewCounterVec(name, help string, labels ...string) *CounterVec {
	return &CounterVec{name: name, help: help, labels: labels, vals: make(map[string]*atomic.Uint64)}
}

// Inc adds one to the series identified by values, which must match the label
// names in number and order (extras are dropped, missing ones are empty).
func (v *CounterVec) Inc(values ...string) { v.Add(1, values...) }

// Add adds n to the series identified by values; non-positive n is ignored.
func (v *CounterVec) Add(n int64, values ...string) {
	if n <= 0 {
		return
	}
	key := v.key(values)
	v.mu.Lock()
	c, ok := v.vals[key]
	if !ok {
		c = new(atomic.Uint64)
		v.vals[key] = c
	}
	v.mu.Unlock()
	c.Add(uint64(n))
}

// Value returns the current count of the series identified by values.
func (v *CounterVec) Value(values ...string) uint64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	if c, ok := v.vals[v.key(values)]; ok {
		return c.Load()
	}
	return 0
}

func (v *CounterVec) key(values []string) string {
	norm := make([]string, len(v.labels))
	copy(norm, values)
	return strings.Join(norm, "\x00")
}

// Writer renders metric families in the Prometheus text format. Errors are
// sticky: after the first write error every call is a no-op and Err reports it.
type Writer struct {
	w   io.Writer
	err error
}

// NewWriter returns a Writer over w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Err returns the first write error, if any.
func (w *Writer) Err() error { return w.err }

func (w *Writer) printf(format string, args ...any) {
	if w.err != nil {
		return
	}
	_, w.err = fmt.Fprintf(w.w, format, args...)
}

func (w *Writer) header(name, help, typ string) {
	w.printf("# HELP %s %s\n# TYPE %s %s\n", name, escapeHelp(help), name, typ)
}

// Counter writes an unlabelled counter.
func (w *Writer) Counter(c *Counter) {
	w.header(c.name, c.help, "counter")
	w.printf("%s %d\n", c.name, c.Value())
}

// CounterVec writes every series of a counter family, sorted by label values
// so output is stable. A family with no series yet still gets its header, so
// the metric is discoverable before its first event.
func (w *Writer) CounterVec(v *CounterVec) {
	w.header(v.name, v.help, "counter")
	v.mu.Lock()
	keys := make([]string, 0, len(v.vals))
	for k := range v.vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	vals := make([]uint64, len(keys))
	for i, k := range keys {
		vals[i] = v.vals[k].Load()
	}
	v.mu.Unlock()
	for i, k := range keys {
		w.printf("%s%s %d\n", v.name, labelSet(v.labels, strings.Split(k, "\x00")), vals[i])
	}
}

// Gauge writes an unlabelled gauge.
func (w *Writer) Gauge(name, help string, value float64) {
	w.header(name, help, "gauge")
	w.printf("%s %s\n", name, formatFloat(value))
}

// GaugeVec writes a gauge family with one label; series are emitted in sorted
// label-value order.
func (w *Writer) GaugeVec(name, help, label string, series map[string]float64) {
	w.header(name, help, "gauge")
	keys := make([]string, 0, len(series))
	for k := range series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w.printf("%s%s %s\n", name, labelSet([]string{label}, []string{k}), formatFloat(series[k]))
	}
}

// WriteCounters writes all of this package's process-wide counters.
func (w *Writer) WriteCounters() {
	w.Counter(DownloadedBytes)
	w.Counter(Completed)
	w.CounterVec(Failures)
	w.CounterVec(LocalRetries)
	w.CounterVec(TorBoxRequests)
	w.CounterVec(ArrNotifications)
}

func labelSet(names, values []string) string {
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		val := ""
		if i < len(values) {
			val = values[i]
		}
		b.WriteString(n)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(val))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

var (
	labelEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }
func escapeHelp(s string) string  { return helpEscaper.Replace(s) }

func formatFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "+Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	case math.IsNaN(f):
		return "NaN"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}
