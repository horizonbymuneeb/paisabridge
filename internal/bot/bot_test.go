package bot

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

type fakeStore struct {
	orders  map[string]*types.Order
	balance int64
}

func (f *fakeStore) Order(_ context.Context, id string) (*types.Order, error) {
	if o, ok := f.orders[id]; ok {
		return o, nil
	}
	return nil, io.EOF
}
func (f *fakeStore) WalletBalance(context.Context, string) (int64, error) { return f.balance, nil }

func TestStartCommandSendsCheckoutLink(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = r.Form.Get("text")
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()

	b, err := New(Config{Token: "TEST", APIBase: srv.URL, CheckoutURL: "https://shop.test/checkout"}, &fakeStore{})
	if err != nil {
		t.Fatal(err)
	}
	b.handle(context.Background(), &Message{Text: "/start", Chat: Chat{ID: 42}})
	if !strings.Contains(got, "https://shop.test/checkout") {
		t.Fatalf("checkout link missing from reply: %q", got)
	}
}

func TestOrderCommandShowsStatus(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got = r.Form.Get("text")
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()

	st := &fakeStore{orders: map[string]*types.Order{
		"PB-ABC123": {Ref: "PB-ABC123", ProductName: "ChatGPT Plus", Amount: 1497, Currency: "PKR", Status: types.StatusPendingPayment},
	}}
	b, _ := New(Config{Token: "TEST", APIBase: srv.URL}, st)
	b.handle(context.Background(), &Message{Text: "/order PB-ABC123", Chat: Chat{ID: 42}})
	if !strings.Contains(got, "pending_payment") || !strings.Contains(got, "1497") {
		t.Fatalf("status reply wrong: %q", got)
	}
}

func TestNotifyOrderStatusUsesTelegramChatID(t *testing.T) {
	var chatID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		chatID = r.Form.Get("chat_id")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	b, _ := New(Config{Token: "TEST", APIBase: srv.URL}, &fakeStore{})
	o := &types.Order{Ref: "PB-X", Status: types.StatusFulfilled, SMSOTP: "123456", CustomerPhone: "tg:777"}
	if err := b.NotifyOrderStatus(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if chatID != "777" {
		t.Fatalf("want chat 777, got %q", chatID)
	}
}
