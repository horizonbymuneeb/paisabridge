package adminui_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/adminui"
	"github.com/horizonbymuneeb/paisabridge/internal/store"
)

// fakeConnectors records what the screen asked the integration layer to do.
type fakeConnectors struct {
	mu      sync.Mutex
	state   adminui.TelegramState
	saved   [][2]string
	saveErr error
	testErr error
	tested  int
}

func (f *fakeConnectors) TelegramStatus(context.Context) adminui.TelegramState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeConnectors) SaveTelegram(_ context.Context, token, chat string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saved = append(f.saved, [2]string{token, chat})
	f.state.Configured = token != ""
	f.state.TokenTail = ""
	if token != "" {
		f.state.TokenTail = "…"
	}
	f.state.ChatID = chat
	return nil
}

func (f *fakeConnectors) TestTelegram(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tested++
	return f.testErr
}

// TestConnectionsRequiresAuth: the screen lists secrets' shape, so it is behind
// the same login as the review queue.
func TestConnectionsRequiresAuth(t *testing.T) {
	h := newHarnessCfg(t, func(c *adminui.Config) {
		c.Connectors = &fakeConnectors{}
	})
	c := h.newClient()
	resp, _ := h.get(c, "/connections", nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unauthenticated /connections = %d, want 303", resp.StatusCode)
	}
	resp, body := h.get(c, "/connections", map[string]string{"X-Admin-Token": testToken})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated /connections = %d %s", resp.StatusCode, firstLines(body, 3))
	}
	if !strings.Contains(body, "Connections") {
		t.Fatal("page does not render the Connections heading")
	}
}

// TestConnectionsListsRelays: every configured rail plus the e-mail and SMS
// relays appear with their endpoint, and unconfigured rows say so.
func TestConnectionsListsRelays(t *testing.T) {
	fc := &fakeConnectors{}
	h := newHarnessCfg(t, func(c *adminui.Config) {
		c.Connectors = fc
		c.Rails = []adminui.RailInfo{
			{Method: "bank", Label: "Bank IBFT / Raast", Signature: "envelope HMAC", Enabled: true,
				Note: "alerts arrive through a relay"},
			{Method: "easypaisa", Label: "Easypaisa", Signature: "AES merchantHashedResp", Enabled: false,
				Note: "set a secret to enable"},
		}
	})
	resp, body := h.get(h.newClient(), "/connections", map[string]string{"X-Admin-Token": testToken})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("connections = %d", resp.StatusCode)
	}
	for _, want := range []string{"E-mail relay", "SMS relay", "Bank IBFT / Raast", "Easypaisa",
		"/api/webhooks/bank", "/api/webhooks/easypaisa", "not configured", "live"} {
		if !strings.Contains(body, want) {
			t.Fatalf("connections screen missing %q", want)
		}
	}
}

// TestTelegramSaveRejectsBadCSRF: a forged cross-site form must not be able to
// plant a bot token.
func TestTelegramSaveRejectsBadCSRF(t *testing.T) {
	fc := &fakeConnectors{}
	h := newHarnessCfg(t, func(c *adminui.Config) { c.Connectors = fc })
	c := h.newClient()
	resp, _ := h.post(c, "/connections/telegram",
		url.Values{"csrf": {"nope"}, "token": {"1:ABC"}, "chat_id": {"1"}},
		map[string]string{"X-Admin-Token": testToken})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.saved) != 0 {
		t.Fatal("a bad CSRF token still saved credentials")
	}
}

// TestTelegramSaveAndTestFlow: the success path the two buttons drive.
func TestTelegramSaveAndTestFlow(t *testing.T) {
	fc := &fakeConnectors{}
	h := newHarnessCfg(t, func(c *adminui.Config) { c.Connectors = fc })
	c := h.newClient()

	_, body := h.get(c, "/connections", map[string]string{"X-Admin-Token": testToken})
	csrf := csrfFrom(t, body)

	resp, _ := h.post(c, "/connections/telegram",
		url.Values{"csrf": {csrf}, "token": {"123456:ABC"}, "chat_id": {"777"}},
		map[string]string{"X-Admin-Token": testToken})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save status = %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "notice=telegram_saved") {
		t.Fatalf("save redirect = %q", loc)
	}
	fc.mu.Lock()
	saved := fc.saved
	fc.mu.Unlock()
	if len(saved) != 1 || saved[0] != [2]string{"123456:ABC", "777"} {
		t.Fatalf("saved %+v", saved)
	}

	_, body = h.get(c, "/connections?notice=telegram_saved", map[string]string{"X-Admin-Token": testToken})
	if !strings.Contains(body, "Telegram settings saved") {
		t.Fatal("save notice not rendered")
	}
	csrf = csrfFrom(t, body)
	resp, _ = h.post(c, "/connections/telegram/test",
		url.Values{"csrf": {csrf}}, map[string]string{"X-Admin-Token": testToken})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("test status = %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "notice=telegram_test") {
		t.Fatalf("test redirect = %q", loc)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.tested != 1 {
		t.Fatalf("test button called TestTelegram %d times", fc.tested)
	}
}

// TestTelegramErrorsSurfaceOnTheScreen: validation failures must come back to
// the screen, because that is where the operator can fix them.
func TestTelegramErrorsSurfaceOnTheScreen(t *testing.T) {
	fc := &fakeConnectors{saveErr: errors.New("integrations: Telegram rejected the token")}
	h := newHarnessCfg(t, func(c *adminui.Config) { c.Connectors = fc })
	c := h.newClient()
	_, body := h.get(c, "/connections", map[string]string{"X-Admin-Token": testToken})
	csrf := csrfFrom(t, body)

	resp, _ := h.post(c, "/connections/telegram",
		url.Values{"csrf": {csrf}, "token": {"bad"}, "chat_id": {""}},
		map[string]string{"X-Admin-Token": testToken})
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "error=") {
		t.Fatalf("failure redirect = %q, want the reason back on the screen", loc)
	}

	_, body = h.get(c, "/connections?error="+url.QueryEscape("integrations: Telegram rejected the token"),
		map[string]string{"X-Admin-Token": testToken})
	if !strings.Contains(body, "Telegram rejected the token") {
		t.Fatal("the integration error is not shown to the operator")
	}
}

// TestConnectionsWithoutManagerExplainsItself: a deployment that did not wire
// the manager must say so instead of showing a dead form.
func TestConnectionsWithoutManagerExplainsItself(t *testing.T) {
	h := newHarnessCfg(t, nil)
	resp, body := h.get(h.newClient(), "/connections", map[string]string{"X-Admin-Token": testToken})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("connections = %d", resp.StatusCode)
	}
	if !strings.Contains(body, "did not wire the integrations manager") {
		t.Fatal("missing explanation for the un-wired deployment")
	}
}

// newHarnessCfg builds a harness whose Config can be tweaked, which the older
// newHarness cannot. It mirrors newHarness deliberately: the same StripPrefix
// mount and the same cookie policy, so the two helpers cannot drift.
func newHarnessCfg(t *testing.T, tweaks func(*adminui.Config)) *harness {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "ui.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	cfg := adminui.Config{Store: s, AdminToken: testToken, InsecureCookie: true}
	if tweaks != nil {
		tweaks(&cfg)
	}
	ui, err := adminui.New(cfg)
	if err != nil {
		t.Fatalf("adminui.New: %v", err)
	}
	ts := httptest.NewServer(http.StripPrefix("/admin", ui))
	t.Cleanup(ts.Close)
	return &harness{t: t, srv: ts, store: s, now: time.Now().UTC()}
}
