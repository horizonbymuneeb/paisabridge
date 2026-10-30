package integrations

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/store"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

// fakeBotStore satisfies bot.Store without a database.
type fakeBotStore struct{}

func (fakeBotStore) Order(context.Context, string) (*types.Order, error) {
	return &types.Order{Ref: "PB-TEST", Status: types.StatusFulfilled}, nil
}
func (fakeBotStore) WalletBalance(context.Context, string) (int64, error) { return 0, nil }

// fakeTelegram is a Bot API double: it records calls and can be told to reject.
type fakeTelegram struct {
	srv      *httptest.Server
	getMe    atomic.Bool
	sendText atomic.Value // string
	polls    atomic.Int64
	sendFail atomic.Bool
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	t.Helper()
	f := &fakeTelegram{}
	f.getMe.Store(true)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			if !f.getMe.Load() {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"ok":false,"description":"Unauthorized"}`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":{"username":"paisa_bot"}}`)
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			if f.sendFail.Load() {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"ok":false,"description":"Bad Request: chat not found"}`)
				return
			}
			f.sendText.Store(r.Form.Get("text"))
			_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			f.polls.Add(1)
			// Long-poll style: hold the connection briefly, then return nothing.
			select {
			case <-r.Context().Done():
			case <-time.After(20 * time.Millisecond):
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
		default:
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newManager(t *testing.T, tg *fakeTelegram) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	m, err := New(Config{Store: st, BotStore: fakeBotStore{}, APIBase: tg.srv.URL,
		CheckoutURL: "https://shop.test/checkout"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	t.Cleanup(m.Shutdown)
	return m, st
}

// TestSaveRejectsBadTokenBeforePersisting: validation comes first, so a typo
// cannot leave the connector half-configured.
func TestSaveRejectsBadTokenBeforePersisting(t *testing.T) {
	ctx := context.Background()
	tg := newFakeTelegram(t)
	m, st := newManager(t, tg)

	if err := m.SaveTelegram(ctx, "not-a-token", "123"); err == nil {
		t.Fatal("a token without a colon must be refused")
	}
	tg.getMe.Store(false)
	if err := m.SaveTelegram(ctx, "123:ABC", "123"); err == nil {
		t.Fatal("a token Telegram rejects must be refused")
	}
	if _, ok, _ := st.Integration(ctx, store.IntegrationTelegramToken); ok {
		t.Fatal("a rejected token was persisted")
	}
}

// TestSaveStartsBotAndTestDelivers: the happy path the console button drives.
func TestSaveStartsBotAndTestDelivers(t *testing.T) {
	ctx := context.Background()
	tg := newFakeTelegram(t)
	m, _ := newManager(t, tg)

	if err := m.SaveTelegram(ctx, "123456:TESTTOKEN", "777"); err != nil {
		t.Fatalf("save: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.Status(ctx).Running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	st := m.Status(ctx)
	if !st.Configured || !st.Running {
		t.Fatalf("status after save = %+v, want configured+running", st)
	}
	if st.TokenTail != "…OKEN" {
		t.Fatalf("token tail = %q, want a masked tail", st.TokenTail)
	}
	if st.ChatID != "777" {
		t.Fatalf("chat id = %q", st.ChatID)
	}

	if err := m.SendTest(ctx); err != nil {
		t.Fatalf("send test: %v", err)
	}
	if got, _ := tg.sendText.Load().(string); !strings.Contains(got, "test message") {
		t.Fatalf("test message not delivered, got %q", got)
	}
	if at := m.Status(ctx).LastTestAt; at.IsZero() {
		t.Fatal("last test timestamp was not recorded")
	}

	// The bot actually polls through the same credentials.
	before := tg.polls.Load()
	time.Sleep(60 * time.Millisecond)
	if tg.polls.Load() <= before {
		t.Fatal("the started bot is not polling")
	}
}

// TestEmptyTokenStopsBot: clearing the field turns the connector off rather
// than silently keeping the old credentials.
func TestEmptyTokenStopsBot(t *testing.T) {
	ctx := context.Background()
	tg := newFakeTelegram(t)
	m, st := newManager(t, tg)
	if err := m.SaveTelegram(ctx, "123456:TESTTOKEN", "777"); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := m.SaveTelegram(ctx, "", ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, ok, _ := st.Integration(ctx, store.IntegrationTelegramToken); ok {
		t.Fatal("clearing the token did not remove it")
	}
	if st2 := m.Status(ctx); st2.Configured || st2.Running {
		t.Fatalf("status after clear = %+v, want stopped", st2)
	}
	if err := m.SendTest(ctx); err != ErrNotConfigured {
		t.Fatalf("send after clear = %v, want ErrNotConfigured", err)
	}
}

// TestSendTestSurfacesTelegramError: the operator needs the provider's own words.
func TestSendTestSurfacesTelegramError(t *testing.T) {
	ctx := context.Background()
	tg := newFakeTelegram(t)
	m, _ := newManager(t, tg)
	if err := m.SaveTelegram(ctx, "123456:TESTTOKEN", "777"); err != nil {
		t.Fatalf("save: %v", err)
	}
	tg.sendFail.Store(true)
	err := m.SendTest(ctx)
	if err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("want Telegram's description, got %v", err)
	}
}

// TestChatIDMustBeNumeric keeps a pasted @username from becoming a silent no-op.
func TestChatIDMustBeNumeric(t *testing.T) {
	tg := newFakeTelegram(t)
	m, _ := newManager(t, tg)
	err := m.SaveTelegram(context.Background(), "123456:TESTTOKEN", "@my_channel")
	if err == nil || !strings.Contains(err.Error(), "numeric") {
		t.Fatalf("want a numeric-chat-id error, got %v", err)
	}
}

// TestLoadStartsStoredBot: after a restart the stored credentials come back.
func TestLoadStartsStoredBot(t *testing.T) {
	ctx := context.Background()
	tg := newFakeTelegram(t)
	_, st := newManager(t, tg)
	if err := st.SetIntegration(ctx, store.IntegrationTelegramToken, "123456:STORED"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetIntegration(ctx, store.IntegrationTelegramChatID, "42"); err != nil {
		t.Fatal(err)
	}
	m2, err := New(Config{Store: st, BotStore: fakeBotStore{}, APIBase: tg.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m2.Shutdown)
	if err := m2.Load(ctx); err != nil {
		t.Fatalf("load: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !m2.Status(ctx).Running {
		time.Sleep(10 * time.Millisecond)
	}
	if !m2.Status(ctx).Running {
		t.Fatal("stored token did not start the bot after Load")
	}
	_ = json.Valid // keep encoding/json if trimmed later
}
