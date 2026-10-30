// Package bot is a Telegram bot for paisabridge.
//
// It is deliberately dependency-free: it talks to the Telegram Bot API over
// plain HTTPS, uses long polling (so it works behind NAT with no public URL),
// and only ever reads state through the injected Store interface.
//
// Flow:
//
//	/start   -> greeting with a link to the checkout page
//	/order X -> live status of order X
//	/wallet  -> wallet balance
//	webhook  -> TellWebhook(order) pushes a status change notification
//
// The bot never sends OTPs for numeric products on its own: it forwards the
// OTP that arrived over SMS to the buyer who owns that order, and only after
// the order reached fulfilled.
package bot

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

	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

// Store is the minimal read surface the bot needs.
type Store interface {
	Order(ctx context.Context, id string) (*types.Order, error)
	WalletBalance(ctx context.Context, userID string) (int64, error)
}

// Config configures the bot.
type Config struct {
	Token       string // BotFather token
	APIBase     string // default https://api.telegram.org
	CheckoutURL string // e.g. https://shop.example/checkout
	PollTimeout time.Duration
	HTTPClient  *http.Client
}

// Bot is a long-polling Telegram bot.
type Bot struct {
	cfg   Config
	store Store
	http  *http.Client

	mu     sync.Mutex
	offset int64
}

// New builds a bot. Call Run in a goroutine.
func New(cfg Config, store Store) (*Bot, error) {
	if cfg.Token == "" {
		return nil, errors.New("telegram: empty token")
	}
	if cfg.APIBase == "" {
		cfg.APIBase = "https://api.telegram.org"
	}
	if cfg.PollTimeout <= 0 {
		cfg.PollTimeout = 25 * time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: cfg.PollTimeout + 10*time.Second}
	}
	return &Bot{cfg: cfg, store: store, http: cfg.HTTPClient}, nil
}

// Run polls until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		updates, err := b.getUpdates(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Network hiccup: back off briefly and keep polling.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
			continue
		}
		for _, u := range updates {
			b.mu.Lock()
			if u.UpdateID >= b.offset {
				b.offset = u.UpdateID + 1
			}
			b.mu.Unlock()
			if u.Message != nil {
				b.handle(ctx, u.Message)
			}
		}
	}
}

// TellWebhook pushes a status change to a chat (fire and forget).
func (b *Bot) TellWebhook(ctx context.Context, chatID int64, text string) error {
	_, err := b.call(ctx, "sendMessage", url.Values{
		"chat_id":    {strconv.FormatInt(chatID, 10)},
		"text":       {text},
		"parse_mode": {"HTML"},
	})
	return err
}

// NotifyOrderStatus sends an order status update to the customer's Telegram,
// if the order carries a Telegram chat id in CustomerPhone ("tg:<id>").
func (b *Bot) NotifyOrderStatus(ctx context.Context, o *types.Order) error {
	chatID, ok := ChatID(o)
	if !ok {
		return nil
	}
	text := fmt.Sprintf("<b>Order %s</b>\nStatus: <code>%s</code>", o.Ref, o.Status)
	if o.Status == types.StatusFulfilled && o.SMSOTP != "" {
		text += "\nOTP: <code>" + o.SMSOTP + "</code>"
	}
	return b.TellWebhook(ctx, chatID, text)
}

// ChatID extracts a telegram chat id from an order (CustomerPhone = "tg:123").
func ChatID(o *types.Order) (int64, bool) {
	if strings.HasPrefix(o.CustomerPhone, "tg:") {
		id, err := strconv.ParseInt(strings.TrimPrefix(o.CustomerPhone, "tg:"), 10, 64)
		if err == nil {
			return id, true
		}
	}
	return 0, false
}

func (b *Bot) handle(ctx context.Context, m *Message) {
	chatID := m.Chat.ID
	text := strings.TrimSpace(m.Text)
	switch {
	case strings.HasPrefix(text, "/start"):
		b.reply(ctx, chatID, "Welcome to paisabridge.\nCheckout: "+b.cfg.CheckoutURL+"\nOrder status: <code>/order PB-XXXXXX</code>")
	case strings.HasPrefix(text, "/order"):
		ref := strings.TrimSpace(strings.TrimPrefix(text, "/order"))
		if ref == "" {
			b.reply(ctx, chatID, "Usage: <code>/order PB-XXXXXX</code>")
			return
		}
		o, err := b.store.Order(ctx, ref)
		if err != nil {
			b.reply(ctx, chatID, "Order not found.")
			return
		}
		b.reply(ctx, chatID, fmt.Sprintf("<b>%s</b>\nProduct: %s\nAmount: %d %s\nStatus: <code>%s</code>",
			o.Ref, o.ProductName, o.Amount, o.Currency, o.Status))
	case strings.HasPrefix(text, "/wallet"):
		bal, err := b.store.WalletBalance(ctx, fmt.Sprintf("tg:%d", chatID))
		if err != nil {
			b.reply(ctx, chatID, "Wallet unavailable, try again.")
			return
		}
		b.reply(ctx, chatID, fmt.Sprintf("Wallet: %d PKR", bal))
	default:
		b.reply(ctx, chatID, "Commands: /start, /order PB-XXXXXX, /wallet")
	}
}

func (b *Bot) reply(ctx context.Context, chatID int64, text string) {
	_, _ = b.call(ctx, "sendMessage", url.Values{
		"chat_id":    {strconv.FormatInt(chatID, 10)},
		"text":       {text},
		"parse_mode": {"HTML"},
	})
}

type updateResponse struct {
	OK     bool     `json:"ok"`
	Result []Update `json:"result"`
}

// Update is a Telegram update.
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

// Message is a Telegram message.
type Message struct {
	MessageID int64  `json:"message_id"`
	Text      string `json:"text"`
	Chat      Chat   `json:"chat"`
}

// Chat is a Telegram chat.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

func (b *Bot) getUpdates(ctx context.Context) ([]Update, error) {
	b.mu.Lock()
	off := b.offset
	b.mu.Unlock()
	raw, err := b.call(ctx, "getUpdates", url.Values{
		"offset":  {strconv.FormatInt(off, 10)},
		"timeout": {strconv.Itoa(int(b.cfg.PollTimeout / time.Second))},
	})
	if err != nil {
		return nil, err
	}
	var r updateResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return r.Result, nil
}

func (b *Bot) call(ctx context.Context, method string, form url.Values) ([]byte, error) {
	endpoint := b.cfg.APIBase + "/bot" + b.cfg.Token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("telegram %s: %s", method, strings.TrimSpace(string(body)))
	}
	return body, nil
}
