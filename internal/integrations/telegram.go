// Package integrations owns the operator-editable connections that are not
// process configuration: the Telegram bot token and chat id, and the test hooks
// the console uses to prove a connector works.
//
// Everything here is deliberately restartable at runtime. Changing the bot token
// from the console must not require a redeploy, so the bot's lifecycle lives in
// a manager that can stop and start it in place.
package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/bot"
	"github.com/horizonbymuneeb/paisabridge/internal/store"
)

// ErrNotConfigured means the connector has no token yet.
var ErrNotConfigured = errors.New("integrations: telegram is not configured")

// TelegramStatus is what the console renders. The token is never included, only
// whether one exists and its last four characters.
type TelegramStatus struct {
	Configured bool
	Running    bool
	TokenTail  string // last 4 characters, for "is this the token I pasted?"
	ChatID     string
	LastTestAt time.Time
	LastError  string
}

// Manager owns the bot's lifecycle and the stored credentials.
type Manager struct {
	st          *store.Store
	storeDS     bot.Store
	apiBase     string
	checkoutURL string
	client      *http.Client
	logf        func(format string, args ...any)

	mu      sync.Mutex
	bot     *bot.Bot
	cancel  context.CancelFunc
	running bool
	lastErr string
}

// Config configures the manager.
type Config struct {
	Store       *store.Store
	BotStore    bot.Store
	APIBase     string // Telegram API origin; empty means the public one
	CheckoutURL string
	HTTPClient  *http.Client
	Logf        func(format string, args ...any)
}

// New builds a manager. Call Load to apply stored settings.
func New(cfg Config) (*Manager, error) {
	if cfg.Store == nil {
		return nil, errors.New("integrations: store is required")
	}
	if cfg.BotStore == nil {
		return nil, errors.New("integrations: bot store is required")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Manager{
		st: cfg.Store, storeDS: cfg.BotStore, apiBase: cfg.APIBase,
		checkoutURL: cfg.CheckoutURL, client: cfg.HTTPClient, logf: cfg.Logf,
	}, nil
}

// Load reads the stored settings and starts the bot if a token is present. A
// stored token wins over the environment because the console is the more recent
// human decision.
func (m *Manager) Load(ctx context.Context) error {
	tok, _, err := m.st.Integration(ctx, store.IntegrationTelegramToken)
	if err != nil {
		return err
	}
	if strings.TrimSpace(tok) == "" {
		return nil
	}
	chat, _, err := m.st.Integration(ctx, store.IntegrationTelegramChatID)
	if err != nil {
		return err
	}
	m.apply(tok, chat)
	return nil
}

// Status reports the connector state for the console.
func (m *Manager) Status(ctx context.Context) TelegramStatus {
	tok, _, _ := m.st.Integration(ctx, store.IntegrationTelegramToken)
	chat, _, _ := m.st.Integration(ctx, store.IntegrationTelegramChatID)
	ts := TelegramStatus{
		Configured: strings.TrimSpace(tok) != "",
		TokenTail:  tail(tok, 4),
		ChatID:     chat,
	}
	if at, ok, err := m.st.IntegrationUpdatedAt(ctx, store.IntegrationLastTestAt); err == nil && ok {
		ts.LastTestAt = at
	}
	m.mu.Lock()
	ts.Running = m.running
	ts.LastError = m.lastErr
	m.mu.Unlock()
	return ts
}

// SaveTelegram validates the token against the Bot API, persists both values and
// (re)starts the bot. Validation happens first so a typo cannot leave the
// connector in a half-configured state.
func (m *Manager) SaveTelegram(ctx context.Context, token, chatID string) error {
	token = strings.TrimSpace(token)
	chatID = strings.TrimSpace(chatID)
	if token == "" {
		// Empty token means "turn the bot off", not "keep the old one".
		if err := m.st.DeleteIntegration(ctx, store.IntegrationTelegramToken); err != nil {
			return err
		}
		_ = m.st.SetIntegration(ctx, store.IntegrationTelegramChatID, "")
		m.stop()
		return nil
	}
	if err := m.validateToken(ctx, token); err != nil {
		return err
	}
	if chatID != "" {
		if _, err := strconv.ParseInt(chatID, 10, 64); err != nil {
			return fmt.Errorf("integrations: chat id must be numeric, got %q", chatID)
		}
	}
	if err := m.st.SetIntegration(ctx, store.IntegrationTelegramToken, token); err != nil {
		return err
	}
	if err := m.st.SetIntegration(ctx, store.IntegrationTelegramChatID, chatID); err != nil {
		return err
	}
	m.apply(token, chatID)
	return nil
}

// SendTest delivers one message to the configured chat id. It reports the Bot
// API's own error text, because "chat not found" and "unauthorized" need
// different fixes.
func (m *Manager) SendTest(ctx context.Context) error {
	tok, _, err := m.st.Integration(ctx, store.IntegrationTelegramToken)
	if err != nil {
		return err
	}
	if strings.TrimSpace(tok) == "" {
		return ErrNotConfigured
	}
	chat, _, err := m.st.Integration(ctx, store.IntegrationTelegramChatID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(chat) == "" {
		return errors.New("integrations: no chat id configured; send /start to the bot first")
	}
	body, err := m.call(ctx, tok, "sendMessage", url.Values{
		"chat_id": {chat},
		"text":    {"paisabridge test message — the Telegram connector works."},
	})
	if err != nil {
		return err
	}
	_ = body
	_ = m.st.SetIntegration(ctx, store.IntegrationLastTestAt, time.Now().UTC().Format(store.TimeLayout))
	return nil
}

// Shutdown stops the bot if it is running.
func (m *Manager) Shutdown() { m.stop() }

func (m *Manager) apply(token, chatID string) {
	m.stop()
	b, err := bot.New(bot.Config{
		Token: token, APIBase: m.apiBase, CheckoutURL: m.checkoutURL,
	}, m.storeDS)
	if err != nil {
		m.setErr(err)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.bot, m.cancel, m.running, m.lastErr = b, cancel, true, ""
	m.mu.Unlock()
	go func() {
		if err := b.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			m.setErr(err)
		}
	}()
	if m.logf != nil {
		m.logf("telegram bot started (chat %q)", chatID)
	}
}

func (m *Manager) stop() {
	m.mu.Lock()
	cancel, was := m.cancel, m.running
	m.cancel, m.bot, m.running = nil, nil, false
	m.mu.Unlock()
	if was && cancel != nil {
		cancel()
	}
}

func (m *Manager) setErr(err error) {
	m.mu.Lock()
	m.lastErr = err.Error()
	m.running = false
	m.mu.Unlock()
	if m.logf != nil {
		m.logf("telegram: %v", err)
	}
}

// Bot exposes the running bot (nil when stopped) so the gateway can push order
// status changes through the same credentials the console manages.
func (m *Manager) Bot() *bot.Bot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bot
}

func (m *Manager) validateToken(ctx context.Context, token string) error {
	if !strings.Contains(token, ":") {
		return errors.New("integrations: that does not look like a bot token (expected 123456:ABC...)")
	}
	resp, err := m.call(ctx, token, "getMe", url.Values{})
	if err != nil {
		return err
	}
	var out struct {
		OK     bool `json:"ok"`
		Result struct {
			Username string `json:"username"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &out); err != nil || !out.OK {
		return errors.New("integrations: Telegram rejected the token")
	}
	return nil
}

func (m *Manager) call(ctx context.Context, token, method string, form url.Values) ([]byte, error) {
	base := m.apiBase
	if base == "" {
		base = "https://api.telegram.org"
	}
	endpoint := base + "/bot" + token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("integrations: telegram %s: %w", method, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		// Telegram returns {"description": "..."} — surface that, it names the fix.
		var e struct {
			Description string `json:"description"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Description != "" {
			return nil, fmt.Errorf("integrations: telegram %s: %s", method, e.Description)
		}
		return nil, fmt.Errorf("integrations: telegram %s: status %d", method, resp.StatusCode)
	}
	return body, nil
}

// tail returns the last n characters of a secret for display as "...OKEN".
// Only the tail is ever rendered: the console shows enough to confirm which
// credential is in place, never enough to use it, and never the length of a
// token that could be guessed from it.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) <= n {
		return strings.Repeat("*", len(s))
	}
	return "…" + s[len(s)-n:]
}
