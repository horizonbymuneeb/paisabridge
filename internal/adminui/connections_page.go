package adminui

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// connectionsData is the model behind the Connections screen.
type connectionsData struct {
	Telegram TelegramState
	Relays   []RelayInfo
	Notice   string
	Error    string
	Now      time.Time
}

// handleConnections renders the connector overview: the Telegram bot and every
// inbound relay endpoint, with the exact URL and signature scheme for each.
func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.redirectLogin(w, r)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	d := connectionsData{Relays: s.relays(), Now: s.now().UTC()}
	if s.cfg.Connectors == nil {
		d.Error = "This deployment did not wire the integrations manager, so connections cannot be edited here. Set the values in the environment instead."
	} else {
		d.Telegram = s.cfg.Connectors.TelegramStatus(ctx)
	}
	if f := r.URL.Query().Get("notice"); f == "telegram_saved" {
		d.Notice = "Telegram settings saved."
	}
	if f := r.URL.Query().Get("notice"); f == "telegram_test" {
		d.Notice = "Test message sent. Check the chat."
	}
	if f := r.URL.Query().Get("error"); f != "" {
		d.Error = f
	}
	s.render(w, http.StatusOK, page{
		chrome: s.chromeFor(r, ctx, "Connections", "connections"),
		Page:   "connections",
		Data:   d,
	})
}

// handleTelegramSave stores the bot token and chat id, validating both first.
func (s *Server) handleTelegramSave(w http.ResponseWriter, r *http.Request) {
	if !s.csrfOK(r) {
		s.renderError(w, r, http.StatusForbidden, "Check failed",
			"That form did not come from this console.",
			"Reload the page and try again; a stale tab cannot submit an action.")
		return
	}
	if s.cfg.Connectors == nil {
		s.connectionError(w, r, ErrConnectorsDisabled.Error())
		return
	}
	if err := r.ParseForm(); err != nil {
		s.connectionError(w, r, "Malformed form submission.")
		return
	}
	token := strings.TrimSpace(r.PostFormValue("token"))
	chat := strings.TrimSpace(r.PostFormValue("chat_id"))
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.cfg.Connectors.SaveTelegram(ctx, token, chat); err != nil {
		s.connectionError(w, r, err.Error())
		return
	}
	http.Redirect(w, r, "/connections?notice=telegram_saved", http.StatusSeeOther)
}

// handleTelegramTest sends one message so the operator can prove the wiring
// without waiting for a real order.
func (s *Server) handleTelegramTest(w http.ResponseWriter, r *http.Request) {
	if !s.csrfOK(r) {
		s.renderError(w, r, http.StatusForbidden, "Check failed",
			"That form did not come from this console.",
			"Reload the page and try again; a stale tab cannot submit an action.")
		return
	}
	if s.cfg.Connectors == nil {
		s.connectionError(w, r, ErrConnectorsDisabled.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.cfg.Connectors.TestTelegram(ctx); err != nil {
		s.connectionError(w, r, err.Error())
		return
	}
	http.Redirect(w, r, "/connections?notice=telegram_test", http.StatusSeeOther)
}

// connectionError sends the operator back to the screen with the reason, which
// is the only place they can fix it.
func (s *Server) connectionError(w http.ResponseWriter, r *http.Request, msg string) {
	s.log.Warn("connections action failed", "err", msg)
	http.Redirect(w, r, "/connections?error="+url.QueryEscape(msg), http.StatusSeeOther)
}

// relays builds the inbound connector table. The rail rows are derived from the
// adapters the gateway actually has configured, so the screen can never claim a
// rail is live when its webhook would answer no_adapter.
func (s *Server) relays() []RelayInfo {
	out := []RelayInfo{
		{
			Name: "E-mail relay", Kind: "email", Endpoint: "/api/webhooks/bank", Method: "POST",
			Signature: "X-Paisabridge-Signature: v1=<hex HMAC-SHA256 of the envelope>",
			Enabled:   s.railEnabled("bank"),
			Note:      "Forward bank credit alerts to a small relay you control; it signs and forwards them here. Never parse alerts inside the gateway.",
		},
		{
			Name: "SMS relay", Kind: "sms", Endpoint: "/api/webhooks/bank", Method: "POST",
			Signature: "X-Paisabridge-Signature: v1=<hex HMAC-SHA256 of the envelope>",
			Enabled:   s.railEnabled("bank"),
			Note:      "An SMS forwarder posts the alert text to your relay, which wraps it in the same signed envelope as e-mail.",
		},
		{
			Name: "Storefront webhook (outbound)", Kind: "webhook", Endpoint: "your storefront URL", Method: "POST",
			Signature: "X-PB-Signature: hex(HMAC-SHA256(outbound secret, body))",
			Enabled:   s.cfg.OutboundEnabled,
			Note:      "Outbound deliveries carry X-PB-Delivery as the receiver's idempotency key; failures are parked and retryable from the admin API.",
		},
	}
	for _, rail := range s.cfg.Rails {
		out = append(out, RelayInfo{
			Name: rail.Label, Kind: "rail", Endpoint: "/api/webhooks/" + rail.Method, Method: "POST",
			Signature: rail.Signature, Enabled: rail.Enabled, Note: rail.Note,
		})
	}
	return out
}

func (s *Server) railEnabled(method string) bool {
	for _, r := range s.cfg.Rails {
		if r.Method == method {
			return r.Enabled
		}
	}
	return false
}
