package api_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/horizonbymuneeb/paisabridge/internal/api"
)

// TestDeadLetterAdminEndpoints covers the operator path: list requires the
// admin token, and retry reports success, failure and "not configured"
// distinctly so the console never claims a delivery was re-sent when it was not.
func TestDeadLetterAdminEndpoints(t *testing.T) {
	h := newHarness(t)

	// Without a token the list is refused.
	code, _ := h.do("GET", "/api/admin/deadletters", nil, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list = %d, want 401", code)
	}

	hdr := map[string]string{"X-Admin-Token": adminToken}
	code, body := h.do("GET", "/api/admin/deadletters", nil, hdr)
	if code != http.StatusOK {
		t.Fatalf("list = %d %s", code, body)
	}
	var listed struct {
		Items []any `json:"items"`
		Open  int64 `json:"open"`
		Total int64 `json:"total"`
	}
	decodeInto(t, body, &listed)
	if listed.Open != 0 || listed.Total != 0 || len(listed.Items) != 0 {
		t.Fatalf("empty store listed %+v", listed)
	}

	// Invalid status is a 400, not an empty page that looks healthy.
	if code, _ := h.do("GET", "/api/admin/deadletters?status=bogus", nil, hdr); code != http.StatusBadRequest {
		t.Fatalf("bogus status = %d, want 400", code)
	}

	// Retry without a configured outbound sender answers 501.
	if code, _ := h.do("POST", "/api/admin/deadletters/dl_1/retry", nil, hdr); code != http.StatusNotImplemented {
		t.Fatalf("retry with no sender = %d, want 501", code)
	}

}

// TestDeadLetterRetryWired: with a redeliverer installed, success is 200 and a
// failure is 502 so the console never claims a delivery was re-sent when it was
// not.
func TestDeadLetterRetryWired(t *testing.T) {
	h := newHarness(t, func(c *api.Config) {
		c.RedeliverDeadLetter = func(ctx context.Context, id string) error {
			if id == "bad" {
				return errors.New("status 502")
			}
			return nil
		}
	})
	hdr := map[string]string{"X-Admin-Token": adminToken}
	if code, body := h.do("POST", "/api/admin/deadletters/dl_1/retry", nil, hdr); code != http.StatusOK {
		t.Fatalf("retry = %d %s", code, body)
	}
	if code, _ := h.do("POST", "/api/admin/deadletters/bad/retry", nil, hdr); code != http.StatusBadGateway {
		t.Fatalf("failing retry = %d, want 502", code)
	}
	if code, _ := h.do("POST", "/api/admin/deadletters//retry", nil, hdr); code == http.StatusOK {
		t.Fatal("empty id must not report success")
	}
}
