package api

// Recorder is the metrics sink the HTTP layer fills. It is an interface rather
// than a concrete dependency so internal/api stays free of the exporter package
// (and its tests can pass nil). Every method must be safe for concurrent use.
//
// The label values are deliberately from closed sets - route classes, rails,
// status classes - so a caller cannot grow a metric's cardinality by inventing
// paths or status codes.
type Recorder interface {
	// RequestDone records one served request.
	RequestDone(class, statusClass string, seconds float64)
	// OrderCreated records a new order by rail and initial status.
	OrderCreated(method, status string)
	// PaymentProcessed records a matched or unmatched inbound payment.
	PaymentProcessed(outcome, reason string)
	// WebhookSignatureFailure records a rejected inbound webhook.
	WebhookSignatureFailure(method string)
	// RateLimited records a request refused by the limiter.
	RateLimited(class string)
}

// noteMetrics calls a recorder method only when one is configured, so the
// hot paths never branch on "is metrics on" more than once.
func (s *Server) noteMetrics(f func(Recorder)) {
	if s.cfg.Metrics == nil {
		return
	}
	f(s.cfg.Metrics)
}
