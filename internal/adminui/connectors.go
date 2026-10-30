package adminui

import (
	"context"
	"errors"
	"time"
)

// Connectors is the console-facing view of the runtime integrations. The
// adminui package never imports the integrations package: it declares what it
// needs, so the screen is testable with fakes and the wiring stays in main.
type Connectors interface {
	// TelegramStatus reports the bot state without exposing the token.
	TelegramStatus(ctx context.Context) TelegramState
	// SaveTelegram validates and applies a token/chat id. An empty token turns
	// the connector off.
	SaveTelegram(ctx context.Context, token, chatID string) error
	// TestTelegram sends one message to the configured chat.
	TestTelegram(ctx context.Context) error
}

// TelegramState mirrors integrations.TelegramStatus.
type TelegramState struct {
	Configured bool
	Running    bool
	TokenTail  string
	ChatID     string
	LastTestAt time.Time
	LastError  string
}

// RelayInfo describes one inbound connector so the screen can print the exact
// endpoint an operator has to configure, and whether it is ready.
type RelayInfo struct {
	// Name is the rail or kind, e.g. "Bank IBFT / Raast", "E-mail relay".
	Name string
	// Kind is one of rail, email, sms, webhook.
	Kind string
	// Endpoint is the path the sender posts to, e.g. /api/webhooks/bank.
	Endpoint string
	// Method is the HTTP method expected.
	Method string
	// Signature explains how the request is authenticated.
	Signature string
	// Enabled reports whether the gateway currently accepts this connector
	// (a rail with no secret answers no_adapter).
	Enabled bool
	// Note carries one line of operational advice.
	Note string
}

// ErrConnectorsDisabled is returned when the screen is opened on a deployment
// that did not wire the integrations manager.
var ErrConnectorsDisabled = errors.New("adminui: connections are not configured on this deployment")
