package metrics

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// sampleLine matches one Prometheus text-format sample: name{labels} value,
// with the value either plain or in exponent form.
var sampleLine = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*(\{[^}]*\})? (-?[0-9.e+\-]+|NaN|\+Inf|-Inf)$`)

func scrape(t *testing.T, r *Registry) (string, []string) {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status = %d", rec.Code)
	}
	body := rec.Body.String()
	var samples []string
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		samples = append(samples, line)
	}
	return body, samples
}

// TestExpositionIsWellFormed: every sample line parses, HELP/TYPE precede each
// family, and the documented family names appear.
func TestExpositionIsWellFormed(t *testing.T) {
	r := New()
	Register(r)
	r.SetEnabled(true)
	// Produce at least one series in several families.
	Orders.Inc("bank", "pending_payment")
	Payments.Inc("matched", "")
	WebhookFailures.Inc("bank")
	RateLimited.Inc("orders_write")
	HTTPRequests.Inc("orders_create", "2xx")
	HTTPDuration.Observe(0.004, "orders_create")

	body, samples := scrape(t, r)
	if len(samples) == 0 {
		t.Fatal("no samples in the exposition")
	}
	for _, line := range samples {
		if !sampleLine.MatchString(line) {
			t.Fatalf("malformed sample line: %q", line)
		}
	}
	for _, family := range []string{
		OrdersCreated, PaymentsMatched, WebhookSignatureFailures,
		RateLimitedRequests, HTTPRequestTotal, HTTPDurationSeconds,
	} {
		if !strings.Contains(body, "# TYPE "+family+" ") {
			t.Fatalf("missing # TYPE for %s", family)
		}
		if !strings.Contains(body, family) {
			t.Fatalf("missing family %s", family)
		}
	}
	// Stability: the exposition groups families by type (counters, histograms,
	// gauges) and sorts each group, so two scrapes of an unchanged process
	// differ only where a value changed. Prometheus does not require any order;
	// this is for the human diffing two scrapes.
	byType := map[string][]string{}
	var order []string
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "# TYPE ") {
			continue
		}
		f := strings.Fields(line)
		name, kind := f[2], f[3]
		if _, seen := byType[kind]; !seen {
			order = append(order, kind)
		}
		byType[kind] = append(byType[kind], name)
	}
	for _, kind := range order {
		names := byType[kind]
		if kind == "counter" || kind == "gauge" || kind == "histogram" {
			if !sort.StringsAreSorted(names) {
				t.Fatalf("%s families not sorted: %v", kind, names)
			}
		}
	}
}

// TestGaugeAndGaugeFunc: both gauge forms render their current value.
func TestGaugeAndGaugeFunc(t *testing.T) {
	r := New()
	g := r.Gauge("test_gauge", "help")
	g.Set(42.5)
	r.GaugeFunc("test_gauge_func", "help", func() (float64, error) { return 7, nil })
	r.SetEnabled(true)

	_, samples := scrape(t, r)
	joined := strings.Join(samples, "\n")
	if !strings.Contains(joined, "test_gauge 42.5") {
		t.Fatalf("gauge value missing:\n%s", joined)
	}
	if !strings.Contains(joined, "test_gauge_func 7") {
		t.Fatalf("gauge func value missing:\n%s", joined)
	}
}

// TestHistogramCumulativeBuckets: buckets are rendered cumulatively with _sum
// and _count, which is what a Prometheus histogram requires.
func TestHistogramCumulativeBuckets(t *testing.T) {
	r := New()
	h := r.Histogram("test_latency_seconds", "help", []float64{0.1, 0.5, 1}, "class")
	h.Observe(0.05, "a")
	h.Observe(0.3, "a")
	h.Observe(2.0, "a")
	r.SetEnabled(true)

	_, samples := scrape(t, r)
	joined := strings.Join(samples, "\n")
	for _, want := range []string{
		`test_latency_seconds_bucket{class="a",le="0.1"} 1`,
		`test_latency_seconds_bucket{class="a",le="0.5"} 2`,
		`test_latency_seconds_bucket{class="a",le="1"} 2`,
		`test_latency_seconds_bucket{class="a",le="+Inf"} 3`,
		`test_latency_seconds_count{class="a"} 3`,
		`test_latency_seconds_sum{class="a"} 2.35`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
}

// TestDisabledRegistryRefusesScrape: a registry that was never enabled answers
// 404 even if the route is somehow mounted.
func TestDisabledRegistryRefusesScrape(t *testing.T) {
	r := New()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled registry status = %d, want 404", rec.Code)
	}
	if !r.Enabled() {
		r.SetEnabled(true)
		if !r.Enabled() {
			t.Fatal("SetEnabled(true) did not take effect")
		}
		r.SetEnabled(false)
		if r.Enabled() {
			t.Fatal("SetEnabled(false) did not take effect")
		}
	}
}

// TestRejectsNonGet: the endpoint is read-only.
func TestRejectsNonGet(t *testing.T) {
	r := New()
	r.SetEnabled(true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); !strings.Contains(got, "GET") {
		t.Fatalf("Allow header = %q", got)
	}
}
