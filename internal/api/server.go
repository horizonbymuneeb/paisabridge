package api

import (
	"net/http"

	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

// routes is the whole surface in one place. Go 1.22 ServeMux method patterns
// ("POST /api/orders/{id}/claim") give us path variables and 405 handling for
// free, so no third-party router is needed.
//
// See the package doc comment above (routes.go header) for the full request /
// response documentation of every endpoint.
func (s *Server) routes() {
	m := s.mux

	// --- public ---------------------------------------------------------
	m.HandleFunc("GET /api/healthz", s.handleHealthz)
	m.HandleFunc("GET /api/readyz", s.handleReadyz)

	m.HandleFunc("POST /api/orders", s.handleCreateOrder)
	m.HandleFunc("GET /api/orders/{id}", s.handleGetOrder)
	m.HandleFunc("POST /api/orders/{id}/claim", s.handleClaimOrder)
	m.HandleFunc("POST /api/orders/{id}/cancel", s.handleCancelOrder)

	m.HandleFunc("POST /api/webhooks/{method}", s.handleWebhook)

	m.HandleFunc("GET /api/wallet", s.handleGetWallet)
	m.HandleFunc("POST /api/wallet/topup", s.handleTopup)

	m.HandleFunc("GET /api/notices", s.handleListNotices)

	// --- admin ----------------------------------------------------------
	m.HandleFunc("POST /api/admin/approve", s.handleAdminApprove)
	m.HandleFunc("GET /api/admin/orders", s.handleAdminOrders)
}

// KnownMethod reports whether m is one of the rails paisabridge understands.
func KnownMethod(m string) bool {
	switch types.Method(m) {
	case types.MethodEasypaisa, types.MethodJazzCash, types.MethodNayaPay,
		types.MethodSadaPay, types.MethodBank, types.MethodUSDT, types.MethodBinance:
		return true
	}
	return false
}

// methodEnabled applies the optional allow-list from config.
func (s *Server) methodEnabled(m types.Method) bool {
	if len(s.cfg.EnabledMethods) == 0 {
		return true
	}
	for _, x := range s.cfg.EnabledMethods {
		if x == m {
			return true
		}
	}
	return false
}

var _ http.Handler = (*Server)(nil)
