package api

import (
	"net/http"
	"strings"
)

// ---------------------------------------------------------------------------
// outbound dead letters
// ---------------------------------------------------------------------------

// listDeadLettersResponse is the JSON the console and operators read.
type listDeadLettersResponse struct {
	Items []any `json:"items"`
	Open  int64 `json:"open"`
	Total int64 `json:"total"`
}

// handleDeadLetters lists parked outbound deliveries.
//
//	GET /api/admin/deadletters?status=open|resolved|all&limit=50&order_id=...
func (s *Server) handleDeadLetters(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	ctx, cancel := ctx(r)
	defer cancel()

	status := r.URL.Query().Get("status")
	switch status {
	case "", "open":
		status = "open"
	case "resolved", "all":
	default:
		writeErr(w, http.StatusBadRequest, "invalid_status", "status must be open, resolved or all")
		return
	}
	limit := atoiDefault(r.URL.Query().Get("limit"), 50)
	orderID := r.URL.Query().Get("order_id")

	rows, err := s.cfg.Store.DeadLetters(ctx, status, orderID, limit)
	if err != nil {
		s.log.Error("list dead letters", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal", "could not list dead letters")
		return
	}
	open, total, err := s.cfg.Store.DeadLetterCounts(ctx)
	if err != nil {
		s.log.Error("dead letter counts", "err", err)
		open, total = 0, 0
	}
	items := make([]any, 0, len(rows))
	for _, dl := range rows {
		// The body is intentionally not listed: it is a whole event payload and
		// would bury the triage fields. It is available per-row for debugging
		// through the store, and it is what the retry re-sends.
		items = append(items, map[string]any{
			"id":          dl.ID,
			"delivery_id": dl.DeliveryID,
			"event":       dl.Event,
			"order_id":    dl.OrderID,
			"last_error":  dl.LastError,
			"attempts":    dl.Attempts,
			"created_at":  dl.CreatedAt,
			"retried_at":  dl.RetriedAt,
			"resolved":    dl.Resolved,
		})
	}
	writeJSON(w, http.StatusOK, listDeadLettersResponse{Items: items, Open: open, Total: total})
}

// handleDeadLetterRetry re-sends one parked delivery.
//
//	POST /api/admin/deadletters/{id}/retry
//
// A successful retry resolves the row; a failed one leaves it parked with the
// new error, so the operator can see that it is still broken rather than
// assuming the click worked.
func (s *Server) handleDeadLetterRetry(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing_id", "dead letter id is required")
		return
	}
	if s.cfg.RedeliverDeadLetter == nil {
		writeErr(w, http.StatusNotImplemented, "not_configured",
			"outbound deliveries are not configured, so there is nothing to retry")
		return
	}
	ctx, cancel := ctx(r)
	defer cancel()

	if err := s.cfg.RedeliverDeadLetter(ctx, id); err != nil {
		s.log.Warn("dead letter retry failed", "id", id, "err", err)
		writeErr(w, http.StatusBadGateway, "retry_failed", err.Error())
		return
	}
	s.log.Info("dead letter retried", "id", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}
