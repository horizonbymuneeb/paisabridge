// Package metrics is a deliberately small Prometheus exporter.
//
// It exists because the obvious choice — the official client library — is a
// dependency this project refuses to take: paisabridge is one binary, one
// SQLite file, and a dependency tree that has to be audited and shipped for a
// handful of counters. The exposition format is a stable, tiny wire format
// (Prometheus text format 0.0.4, one sample per line), so emitting it by hand
// costs ~300 lines and zero modules.
//
// # What is exposed
//
// Counters and gauges only: the API increments a counter per interesting event
// and sets a few gauges scraped from the database. Histograms are fixed-bucket
// and declared statically in this package (see HTTPDuration below), because a
// histogram that a caller can invent at runtime is how an exporter grows an
// unbounded map of label sets.
//
// # Cardinality
//
// Every label value comes from a closed set: HTTP status *classes* (2xx, 4xx)
// rather than status codes, route *classes* rather than URLs, and the rails the
// gateway already knows. Nothing is derived from user input, so a scanner
// cannot grow the registry by inventing paths. That is a security property, not
// just a performance one.
//
// # Enabling
//
// The exporter is inert by default. main.go only registers handlers when
// PB_METRICS=1, and the handler itself checks its own Enabled flag, so a
// deployment that forgets the variable cannot leak the numbers by accident.
package metrics

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/version"
)

// Names of every metric this package (and the API that fills it) emits. They
// are constants so a test can assert the documented set without typo risk, and
// so a rename is a compile error rather than a silently gapped dashboard.
const (
	// OrdersCreated counts orders accepted by POST /api/orders, labelled by
	// payment rail and the order's initial status.
	OrdersCreated = "paisabridge_orders_created_total"
	// PaymentsMatched counts inbound notices that matched an order.
	PaymentsMatched = "paisabridge_payments_matched_total"
	// PaymentsUnmatched counts inbound notices that did not, labelled by the
	// matcher's reason (no_open_order, ambiguous_amount, order_expired, ...).
	PaymentsUnmatched = "paisabridge_payments_unmatched_total"
	// WebhookSignatureFailures counts inbound webhooks rejected before the
	// matcher, labelled by rail. A jump here is a forgery attempt or a secret
	// that drifted between two deployments — either is worth waking someone.
	WebhookSignatureFailures = "paisabridge_webhook_signature_failures_total"
	// RateLimitedRequests counts requests refused with 429, labelled by route
	// class.
	RateLimitedRequests = "paisabridge_ratelimited_requests_total"
	// OutboundDeliveries counts outbound deliveries by result:
	// delivered | retried | failed | permanent.
	OutboundDeliveries = "paisabridge_outbound_deliveries_total"
	// OutboundDeadLetterOpen is the number of parked deliveries still open.
	OutboundDeadLetterOpen = "paisabridge_outbound_deadletter_open"
	// OutboundDeadLetterTotal is every row ever parked, resolved or not.
	OutboundDeadLetterTotal = "paisabridge_outbound_deadletter_total"
	// HTTPRequestTotal counts handled requests by route class and status class.
	HTTPRequestTotal = "paisabridge_http_requests_total"
	// HTTPDurationSeconds is the request duration histogram per route class.
	HTTPDurationSeconds = "paisabridge_http_request_duration_seconds"
	// DBOpenConnections is sql.DB.Stats().OpenConnections: a connection that
	// never returns to the pool is usually a leaked rows.Rows value.
	DBOpenConnections = "paisabridge_db_open_connections"
	// DBInUseConnections is sql.DB.Stats().InUse: requests waiting for the
	// single SQLite connection show up here before they show up in latency.
	DBInUseConnections = "paisabridge_db_in_use_connections"
)

// HTTPDurationBuckets are the histogram boundaries in seconds. They are chosen
// around this service's actual shape: SQLite reads on local storage land well
// under 5ms; a webhook that has to verify a signature and run the matcher lands
// under 50ms; a fulfilment (transition + outbound kick) under 250ms. Anything
// above 1s is either a lock contention problem or an outage, not normal load.
var HTTPDurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

// outboundResults is the closed set of values for the result label on
// OutboundDeliveries.
var outboundResults = []string{"delivered", "retried", "failed", "permanent"}

// Registry is the process-wide metric store. One Registry per process is the
// norm (New); tests may create their own so they never observe each other.
type Registry struct {
	mu         sync.RWMutex
	counters   map[string]*counterVec
	histograms map[string]*histogramVec
	gauges     map[string]*gauge
	started    time.Time
	// enabled gates ServeHTTP only. Recording is always safe (it is a few
	// atomic adds), so the API can increment unconditionally and the switch
	// only decides whether anyone can *read* the numbers.
	enabled atomic.Bool
}

type counterVec struct {
	help   string
	labels []string
	mu     sync.Mutex
	series map[string]*counterChild
}

type counterChild struct {
	vals []string
	// value holds float64 bits in a Uint64 so Add can stay lock-free while
	// accepting fractional values; every caller in this repo adds 1.
	value atomic.Uint64
}

type histogramVec struct {
	help    string
	labels  []string
	buckets []float64
	mu      sync.Mutex
	series  map[string]*histogramChild
}

type histogramChild struct {
	vals   []string
	counts []uint64 // one per bucket, non-cumulative
	sum    float64
	count  uint64
}

type gauge struct {
	help  string
	value atomic.Uint64 // float64 bits
	set   func() (float64, error)
}

// New returns an empty registry. The process normally uses Default.
func New() *Registry {
	return &Registry{
		counters:   map[string]*counterVec{},
		histograms: map[string]*histogramVec{},
		gauges:     map[string]*gauge{},
		started:    time.Now(),
	}
}

// Default is the registry the running server uses.
var Default = New()

// SetEnabled turns the /metrics handler on (PB_METRICS=1) or off. Recording is
// unaffected: counters keep counting whether or not anyone can scrape them, so
// flipping the switch never produces a metric that starts at zero mid-run.
func (r *Registry) SetEnabled(on bool) { r.enabled.Store(on) }

// Enabled reports whether the endpoint is served.
func (r *Registry) Enabled() bool { return r.enabled.Load() }

// ---------------------------------------------------------------------------
// registration
// ---------------------------------------------------------------------------

// Counter registers (or returns) a counter vec. Re-registering the same name
// with the same labels is idempotent, so package-level wiring cannot fail on a
// double init; re-registering with *different* labels panics, because that is a
// programming error that would otherwise corrupt a dashboard silently.
func (r *Registry) Counter(name, help string, labels ...string) CounterVec {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.counters[name]; ok {
		if !sameLabels(c.labels, labels) {
			panic(fmt.Sprintf("metrics: counter %s re-registered with labels %v, already has %v", name, labels, c.labels))
		}
		return CounterVec{vec: c}
	}
	c := &counterVec{help: help, labels: labels, series: map[string]*counterChild{}}
	r.counters[name] = c
	return CounterVec{vec: c}
}

// Histogram registers a fixed-bucket histogram.
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) HistogramVec {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.histograms[name]; ok {
		if !sameLabels(h.labels, labels) {
			panic(fmt.Sprintf("metrics: histogram %s re-registered with labels %v, already has %v", name, labels, h.labels))
		}
		return HistogramVec{vec: h}
	}
	if len(buckets) == 0 {
		buckets = HTTPDurationBuckets
	}
	sorted := append([]float64(nil), buckets...)
	sort.Float64s(sorted)
	h := &histogramVec{help: help, labels: labels, buckets: sorted, series: map[string]*histogramChild{}}
	r.histograms[name] = h
	return HistogramVec{vec: h}
}

// Gauge registers a gauge whose value is computed by fn at scrape time. A
// callback gauge is the right shape for anything read from SQLite: caching the
// number would either be stale or force a background poller.
func (r *Registry) GaugeFunc(name, help string, fn func() (float64, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gauges[name] = &gauge{help: help, set: fn}
}

// Gauges provides a single-value gauge that the caller sets.
func (r *Registry) Gauge(name, help string) Gauge {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := &gauge{help: help}
	r.gauges[name] = g
	return Gauge{g: g}
}

func sameLabels(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// handles
// ---------------------------------------------------------------------------

// CounterVec is a handle to a labelled counter family.
type CounterVec struct{ vec *counterVec }

// With returns the child for a label set. The label values are copied into a
// fresh slice; callers may build the slice once and reuse it.
func (c CounterVec) With(values ...string) *Counter {
	if c.vec == nil {
		return &Counter{}
	}
	if len(values) != len(c.vec.labels) {
		panic(fmt.Sprintf("metrics: counter wants %d label values, got %d", len(c.vec.labels), len(values)))
	}
	key := strings.Join(values, "\x00")
	c.vec.mu.Lock()
	child, ok := c.vec.series[key]
	if !ok {
		child = &counterChild{vals: append([]string(nil), values...)}
		c.vec.series[key] = child
	}
	c.vec.mu.Unlock()
	return &Counter{child: child}
}

// Inc adds one.
func (c CounterVec) Inc(values ...string) { c.With(values...).Inc() }

// Counter is one labelled time series.
type Counter struct{ child *counterChild }

// Inc adds one to the series.
func (c *Counter) Inc() { c.Add(1) }

// Add adds v. v is a float64 because a counter occasionally counts something
// fractional (bytes, rounded seconds); everything in this repo adds 1.
//
// The accumulation is a compare-and-swap loop over the IEEE-754 bits, not a
// plain atomic Add on those bits. Adding the bit patterns treats the mantissa as
// an integer: the first increment looks correct by luck (bits(1.0) decodes back
// to 1.0), and the second decodes to an exponent field of all ones, which is how
// a counter reports 8.9e307 for its second request.
func (c *Counter) Add(v float64) {
	if c == nil || c.child == nil {
		return
	}
	p := &c.child.value
	for {
		old := p.Load()
		next := math.Float64bits(math.Float64frombits(old) + v)
		if p.CompareAndSwap(old, next) {
			return
		}
	}
}

// Value reads the current total (used by tests and by the scrape).
func (c *Counter) Value() float64 {
	if c == nil || c.child == nil {
		return 0
	}
	return math.Float64frombits(c.child.value.Load())
}

// HistogramVec is a handle to a labelled histogram family.
type HistogramVec struct{ vec *histogramVec }

// With returns the child for a label set.
func (h HistogramVec) With(values ...string) *Histogram {
	if h.vec == nil {
		return &Histogram{}
	}
	if len(values) != len(h.vec.labels) {
		panic(fmt.Sprintf("metrics: histogram wants %d label values, got %d", len(h.vec.labels), len(values)))
	}
	key := strings.Join(values, "\x00")
	h.vec.mu.Lock()
	child, ok := h.vec.series[key]
	if !ok {
		child = &histogramChild{vals: append([]string(nil), values...), counts: make([]uint64, len(h.vec.buckets))}
		h.vec.series[key] = child
	}
	h.vec.mu.Unlock()
	return &Histogram{vec: h.vec, child: child}
}

// Observe records one duration.
func (h HistogramVec) Observe(seconds float64, values ...string) {
	h.With(values...).Observe(seconds)
}

// Histogram is one labelled histogram series.
type Histogram struct {
	vec   *histogramVec
	child *histogramChild
}

// Observe records one observation in seconds.
func (h *Histogram) Observe(seconds float64) {
	if h == nil || h.child == nil || h.vec == nil {
		return
	}
	h.vec.mu.Lock()
	defer h.vec.mu.Unlock()
	i := sort.SearchFloat64s(h.vec.buckets, seconds)
	// SearchFloat64s finds the first bucket >= seconds; a value above the
	// last boundary lands at len(buckets) (the implicit +Inf bucket).
	if i < len(h.child.counts) {
		h.child.counts[i]++
	}
	h.child.sum += seconds
	h.child.count++
}

// Gauge is a value the caller sets.
type Gauge struct{ g *gauge }

// Set replaces the gauge value.
func (g Gauge) Set(v float64) {
	if g.g == nil {
		return
	}
	g.g.value.Store(math.Float64bits(v))
}

// ---------------------------------------------------------------------------
// scrape
// ---------------------------------------------------------------------------

// WritePrometheus renders the registry in the Prometheus text exposition
// format (version 0.0.4). It is deterministic: families are emitted in sorted
// order and series within a family in insertion-independent sorted order, so
// two scrapes of an idle server are byte-identical and diffs stay readable.
//
// A gauge callback that fails is skipped, with a comment line recording why —
// a broken metric must never take the scrape down, because the scrape is how
// an operator finds out something is broken.
func (r *Registry) WritePrometheus(w io.Writer) error {
	var b strings.Builder

	r.mu.RLock()
	counterNames := sortedKeys(r.counters)
	histoNames := sortedKeys(r.histograms)
	gaugeNames := sortedKeys(r.gauges)
	r.mu.RUnlock()

	// Process metadata first: it tells a fresh operator what they are looking
	// at without a second endpoint.
	b.WriteString("# HELP paisabridge_build_info Build identity of the running gateway.\n")
	b.WriteString("# TYPE paisabridge_build_info gauge\n")
	fmt.Fprintf(&b, "paisabridge_build_info{version=%q} 1\n", escapeLabel(version.Get().Version))
	b.WriteString("# HELP paisabridge_start_time_seconds Unix time the process started collecting metrics.\n")
	b.WriteString("# TYPE paisabridge_start_time_seconds gauge\n")
	fmt.Fprintf(&b, "paisabridge_start_time_seconds %s\n", formatFloat(float64(r.started.UnixNano())/1e9))

	for _, name := range counterNames {
		r.mu.RLock()
		c := r.counters[name]
		r.mu.RUnlock()
		c.mu.Lock()
		children := make([]*counterChild, 0, len(c.series))
		for _, ch := range c.series {
			children = append(children, ch)
		}
		c.mu.Unlock()
		sort.Slice(children, func(i, j int) bool { return lessVals(children[i].vals, children[j].vals) })

		fmt.Fprintf(&b, "# HELP %s %s\n", name, escapeHelp(c.help))
		fmt.Fprintf(&b, "# TYPE %s counter\n", name)
		for _, ch := range children {
			fmt.Fprintf(&b, "%s%s %s\n", name, renderLabels(c.labels, ch.vals), formatFloat(math.Float64frombits(ch.value.Load())))
		}
	}

	for _, name := range histoNames {
		r.mu.RLock()
		h := r.histograms[name]
		r.mu.RUnlock()
		h.mu.Lock()
		children := make([]*histogramChild, 0, len(h.series))
		for _, ch := range h.series {
			children = append(children, ch)
		}
		buckets := h.buckets
		h.mu.Unlock()
		sort.Slice(children, func(i, j int) bool { return lessVals(children[i].vals, children[j].vals) })

		fmt.Fprintf(&b, "# HELP %s %s\n", name, escapeHelp(h.help))
		fmt.Fprintf(&b, "# TYPE %s histogram\n", name)
		for _, ch := range children {
			cum := uint64(0)
			for i, bound := range buckets {
				cum += ch.counts[i]
				fmt.Fprintf(&b, "%s_bucket%s %d\n", name,
					renderLabels(h.labels, ch.vals, "le", formatFloat(bound)), cum)
			}
			fmt.Fprintf(&b, "%s_bucket%s %d\n", name,
				renderLabels(h.labels, ch.vals, "le", "+Inf"), ch.count)
			fmt.Fprintf(&b, "%s_sum%s %s\n", name, renderLabels(h.labels, ch.vals), formatFloat(ch.sum))
			fmt.Fprintf(&b, "%s_count%s %d\n", name, renderLabels(h.labels, ch.vals), ch.count)
		}
	}

	for _, name := range gaugeNames {
		r.mu.RLock()
		g := r.gauges[name]
		r.mu.RUnlock()
		fmt.Fprintf(&b, "# HELP %s %s\n", name, escapeHelp(g.help))
		fmt.Fprintf(&b, "# TYPE %s gauge\n", name)
		if g.set != nil {
			v, err := g.set()
			if err != nil {
				// A broken gauge must not fail the scrape: the scrape is how
				// an operator learns that something is broken. Record the
				// reason as a comment above where the sample would have been.
				fmt.Fprintf(&b, "# %s unavailable: %s\n", name, oneLine(err.Error()))
				continue
			}
			fmt.Fprintf(&b, "%s %s\n", name, formatFloat(v))
		} else {
			fmt.Fprintf(&b, "%s %s\n", name, formatFloat(math.Float64frombits(g.value.Load())))
		}
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// ServeHTTP writes the exposition. main.go only mounts it when PB_METRICS=1,
// and the handler re-checks SetEnabled so a route registered by mistake still
// answers 404 to everyone.
func (r *Registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if !r.Enabled() {
		http.NotFound(w, req)
		return
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// WritePrometheus writes into a buffer first, so a partial scrape never
	// reaches the wire; an error here means the client went away.
	_ = r.WritePrometheus(w)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func lessVals(a, b []string) bool {
	for i := range a {
		if i >= len(b) {
			return false
		}
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// renderLabels builds the {a="1",b="2"} suffix. extra allows a histogram's
// "le" label to ride along without a second code path.
func renderLabels(names, vals []string, extra ...string) string {
	if len(names) == 0 && len(extra) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	wrote := false
	for i, n := range names {
		if wrote {
			b.WriteByte(',')
		}
		b.WriteString(n)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(vals[i]))
		b.WriteByte('"')
		wrote = true
	}
	for i := 0; i+1 < len(extra); i += 2 {
		if wrote {
			b.WriteByte(',')
		}
		// "le" values are numbers, but quoting them is also valid and keeps
		// one escaping path; Prometheus parses both forms.
		b.WriteString(extra[i])
		b.WriteString(`="`)
		b.WriteString(escapeLabel(extra[i+1]))
		b.WriteByte('"')
		wrote = true
	}
	b.WriteByte('}')
	return b.String()
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
var helpEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }
func escapeHelp(s string) string  { return helpEscaper.Replace(s) }

func oneLine(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
}

// formatFloat renders a float the way Prometheus expects: shortest
// round-trippable form, with +Inf/-Inf/NaN spelled out.
func formatFloat(v float64) string {
	if math.IsNaN(v) {
		return "NaN"
	}
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	if math.IsInf(v, -1) {
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
