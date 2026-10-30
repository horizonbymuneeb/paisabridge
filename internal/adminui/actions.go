package adminui

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/store"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

// ---------------------------------------------------------------------------
// login
// ---------------------------------------------------------------------------

// loginData is the sign-in form model.
type loginData struct {
	Next  string
	Error string
	Note  string
}

// handleLoginForm renders the token form.
func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	if s.authed(r) {
		http.Redirect(w, r, orDefault(next, "/"), http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	s.render(w, http.StatusOK, page{
		chrome:     s.chromeFor(r, ctx, "Sign in", ""),
		Page:       "login",
		Standalone: true,
		Data: loginData{
			Next: next,
			Note: "This is the same secret the JSON admin API expects in X-Admin-Token (PB_ADMIN_TOKEN).",
		},
	})
}

// handleLogin checks the submitted token and, on success, sets the session
// cookie. The comparison is constant-time and every failure is logged with the
// client address so a brute-force attempt is visible in the log.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderLoginError(w, r, "", "Malformed form submission.")
		return
	}
	next := safeNext(r.PostFormValue("next"))
	tok := strings.TrimSpace(r.PostFormValue("token"))
	if tok == "" {
		s.renderLoginError(w, r, next, "Enter the admin token.")
		return
	}
	if !s.tokenEqual(tok) {
		s.log.Warn("adminui login rejected", "remote", clientIP(r))
		s.renderLoginError(w, r, next, "That token is not valid.")
		return
	}
	id, err := sessionID()
	if err != nil {
		s.log.Error("adminui session id", "err", err)
		s.renderLoginError(w, r, next, "Could not start a session. Try again.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    s.signCookie(id),
		Path:     "/",
		HttpOnly: true,
		Secure:   !s.cfg.InsecureCookie,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL / time.Second),
	})
	s.log.Info("adminui login", "remote", clientIP(r))
	http.Redirect(w, r, orDefault(next, "/"), http.StatusSeeOther)
}

func (s *Server) renderLoginError(w http.ResponseWriter, r *http.Request, next, msg string) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	s.render(w, http.StatusUnauthorized, page{
		chrome:     s.chromeFor(r, ctx, "Sign in", ""),
		Page:       "login",
		Standalone: true,
		Data: loginData{
			Next:  next,
			Error: msg,
			Note:  "This is the same secret the JSON admin API expects in X-Admin-Token (PB_ADMIN_TOKEN).",
		},
	})
}

// handleLogout clears the session cookie.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Redirect(w, r, "login", http.StatusSeeOther)
		return
	}
	if !s.csrfOK(r) {
		http.Redirect(w, r, flash("login", "badtoken", "", ""), http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   !s.cfg.InsecureCookie,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	http.Redirect(w, r, flash("login", "loggedout", "", ""), http.StatusSeeOther)
}

// safeNext only allows a same-origin absolute path, so a crafted ?next= can
// never turn the login form into an open redirect.
func safeNext(v string) string {
	if v == "" || !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") {
		return ""
	}
	if strings.ContainsAny(v, "\\\r\n") {
		return ""
	}
	return v
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// clientIP is only used for logging, so the header forms are fine; it never
// takes part in authorisation.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	return r.RemoteAddr
}

// ---------------------------------------------------------------------------
// approve / reject
// ---------------------------------------------------------------------------

// handleQueueAction performs the one human decision the review queue exists
// for. It is a POST so it is out of reach of link prefetchers and crawlers,
// and it re-checks the credential and the CSRF binding on every call.
func (s *Server) handleQueueAction(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.redirectLogin(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, flash("/queue", "failed", "", "malformed form"), http.StatusSeeOther)
		return
	}
	if !s.csrfOK(r) {
		http.Redirect(w, r, flash("/queue", "badtoken", "", ""), http.StatusSeeOther)
		return
	}

	orderID := strings.TrimSpace(r.PostFormValue("order_id"))
	act := r.PostFormValue("action")
	txnRef := strings.TrimSpace(r.PostFormValue("txn_ref"))
	if orderID == "" {
		http.Redirect(w, r, flash("/queue", "failed", "", "missing order id"), http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	switch act {
	case "approve":
		s.approve(w, r, ctx, orderID, txnRef)
	case "reject":
		s.reject(w, r, ctx, orderID)
	default:
		http.Redirect(w, r, flash("/queue", "failed", "", "unknown action"), http.StatusSeeOther)
	}
}

// approve moves an order to verified through the same state machine and
// journal the API uses, then to fulfilled, mirroring POST /api/admin/approve:
// bind the txn ref, review -> verified, verified -> fulfilled.
func (s *Server) approve(w http.ResponseWriter, r *http.Request, ctx context.Context, orderID, txnRef string) {
	ord, err := s.cfg.Store.Order(ctx, orderID)
	if errors.Is(err, store.ErrNotFound) {
		http.Redirect(w, r, flash("/queue", "failed", "", "order not found"), http.StatusSeeOther)
		return
	}
	if err != nil {
		s.actionFailed(w, r, err)
		return
	}
	who := actor(r)
	if txnRef != "" {
		if err := s.cfg.Store.BindTxRef(ctx, ord.ID, txnRef); err != nil {
			if errors.Is(err, store.ErrConflict) {
				http.Redirect(w, r, flash("/queue", "failed", ord.Ref, "that order is already bound to another txn ref"), http.StatusSeeOther)
				return
			}
			s.actionFailed(w, r, err)
			return
		}
	}
	detail := "manual approve from review UI by " + who
	if txnRef != "" {
		detail += ", txn " + txnRef
	}
	if _, err := s.cfg.Store.TransitionOrder(ctx, ord.ID, ord.Status, types.StatusVerified, who, detail); err != nil {
		if errors.Is(err, store.ErrNoTransition) || errors.Is(err, store.ErrTransitionInvalid) {
			http.Redirect(w, r, flash("/queue", "gone", ord.Ref, ""), http.StatusSeeOther)
			return
		}
		s.actionFailed(w, r, err)
		return
	}
	// Delivery is its own audited step; a failure here leaves the order
	// verified (paid, waiting) rather than rolling the approval back.
	if _, err := s.cfg.Store.TransitionOrder(ctx, ord.ID, types.StatusVerified, types.StatusFulfilled, who, "approved and delivered from review UI"); err != nil &&
		!errors.Is(err, store.ErrNoTransition) && !errors.Is(err, store.ErrTransitionInvalid) {
		s.log.Error("adminui fulfil after approve", "order", ord.ID, "err", err)
	}
	if txnRef != "" {
		_ = s.cfg.Store.ResolveNotice(ctx, ord.Method, txnRef, ord.ID, "manual_approve", "matched")
	}
	s.log.Info("adminui approved", slog.String("order", ord.ID), slog.String("ref", ord.Ref),
		slog.String("amount", ordinalAmount(ord)), slog.String("actor", who))
	http.Redirect(w, r, flash("/queue", "approved", ord.Ref, ""), http.StatusSeeOther)
}

// reject cancels the order. Cancellation is a real state, not a deletion: the
// row and its journal stay for the audit trail.
func (s *Server) reject(w http.ResponseWriter, r *http.Request, ctx context.Context, orderID string) {
	ord, err := s.cfg.Store.Order(ctx, orderID)
	if errors.Is(err, store.ErrNotFound) {
		http.Redirect(w, r, flash("/queue", "failed", "", "order not found"), http.StatusSeeOther)
		return
	}
	if err != nil {
		s.actionFailed(w, r, err)
		return
	}
	who := actor(r)
	if _, err := s.cfg.Store.TransitionOrder(ctx, ord.ID, ord.Status, types.StatusCancelled, who, "rejected from review UI by "+who); err != nil {
		if errors.Is(err, store.ErrNoTransition) || errors.Is(err, store.ErrTransitionInvalid) {
			http.Redirect(w, r, flash("/queue", "gone", ord.Ref, ""), http.StatusSeeOther)
			return
		}
		s.actionFailed(w, r, err)
		return
	}
	s.log.Info("adminui rejected", slog.String("order", ord.ID), slog.String("ref", ord.Ref),
		slog.String("actor", who))
	http.Redirect(w, r, flash("/queue", "rejected", ord.Ref, ""), http.StatusSeeOther)
}

func (s *Server) actionFailed(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("adminui action", "path", r.URL.Path, "err", err)
	http.Redirect(w, r, flash("/queue", "failed", "", oneLine(err.Error())), http.StatusSeeOther)
}

// oneLine keeps an error message from breaking the flash banner layout or the
// query string.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}

// ordinalAmount is the log form of an order amount.
func ordinalAmount(o *types.Order) string {
	return formatAmount(o.Amount, o.Currency)
}
