// Command paisabridge runs the paisabridge payment gateway.
//
// It wires four things together and does nothing else:
//
//	config  -> env-driven settings (internal/config)
//	store   -> SQLite (internal/store)
//	matcher -> PaymentNotice -> exactly one order (internal/match)
//	wallet  -> append-only ledger (internal/wallet)
//	api     -> net/http, Go 1.22 method patterns (internal/api)
//
// The rail adapters come from internal/adapters/registry; the API only ever
// sees a small AdapterLookup interface, so a rail can be absent (no secret
// configured) without taking the server down.
//
// Every minute the expiry sweeper closes past-due orders and refunds any that
// had been paid from the wallet.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/adapter"
	"github.com/horizonbymuneeb/paisabridge/internal/adapters/registry"
	"github.com/horizonbymuneeb/paisabridge/internal/api"
	"github.com/horizonbymuneeb/paisabridge/internal/bot"
	"github.com/horizonbymuneeb/paisabridge/internal/config"
	"github.com/horizonbymuneeb/paisabridge/internal/match"
	"github.com/horizonbymuneeb/paisabridge/internal/store"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
	"github.com/horizonbymuneeb/paisabridge/internal/wallet"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "paisabridge:", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	// --- storage -------------------------------------------------------
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open database %s: %w", cfg.DBPath, err)
	}
	defer st.Close()
	logger.Info("database ready", "path", cfg.DBPath)

	// --- money ---------------------------------------------------------
	w := wallet.New(st,
		wallet.WithDiscountBPS(cfg.DiscountBPS),
		wallet.WithCurrency(cfg.WalletCurrency),
	)
	m := match.New(st)
	if cfg.SeedDemoTopup > 0 {
		if _, err := w.Topup(context.Background(), "", cfg.SeedDemoTopup, "boot-seed", "demo seed"); err != nil {
			logger.Warn("demo seed topup failed", "err", err)
		}
	}

	// --- payment rails -------------------------------------------------
	reg, err := buildRegistry(cfg, st, logger)
	if err != nil {
		return err
	}

	// --- http ----------------------------------------------------------
	apiSrv, err := api.New(api.Config{
		Store:          st,
		Wallet:         w,
		Matcher:        m,
		Adapters:       lookupFunc(adaptersFrom(reg)),
		AdminToken:     cfg.AdminToken,
		OrderTTL:       cfg.OrderTTL,
		Logger:         logger,
		EnabledMethods: cfg.EnabledMethods,
	})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           logRequests(logger, apiSrv),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// --- expiry sweeper ------------------------------------------------
	sweepDone := make(chan struct{})
	if cfg.SweepInterval > 0 {
		go func() {
			defer close(sweepDone)
			sweepLoop(ctx, logger, st, w, cfg.SweepInterval)
		}()
	} else {
		close(sweepDone)
	}

	// --- telegram bot --------------------------------------------------
	// Long polling: no public URL and no webhook registration needed. The bot
	// is optional; without a token the gateway runs headless.
	if cfg.TelegramToken != "" {
		tgBot, err := bot.New(bot.Config{
			Token:       cfg.TelegramToken,
			APIBase:     cfg.TelegramAPIBase,
			CheckoutURL: cfg.PublicURL + "/checkout",
		}, botStore{st: st, w: w})
		if err != nil {
			logger.Warn("telegram bot disabled", "err", err)
		} else {
			go func() {
				logger.Info("telegram bot polling")
				if err := tgBot.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
					logger.Error("telegram bot stopped", "err", err)
				}
			}()
		}
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Addr, "dev_mode", cfg.DevMode,
			"methods", strings.Join(methodNames(cfg.EnabledMethods), ","))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case err := <-errCh:
		if err != nil {
			return err
		}
		return nil
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown", "err", err)
	}
	<-sweepDone
	return nil
}

// buildRegistry turns config rail secrets into adapters. A rail with no
// secret is simply not registered: its webhook route answers no_adapter, which
// is safer than booting with a rail nobody configured.
func buildRegistry(cfg *config.Config, st *store.Store, logger *slog.Logger) (*registry.Registry, error) {
	rc := registry.Config{
		EasypaisaSecret:      cfg.EasypaisaSecret,
		JazzCashSalt:         cfg.JazzCashSalt,
		NayaPayWebhookSecret: cfg.NayaPayWebhookSecret,
		NayaPayToken:         cfg.NayaPayToken,
		NayaPayBaseURL:       cfg.NayaPayBaseURL,
		SadaPayWebhookSecret: cfg.SadaPayWebhookSecret,
		BankRelaySecret:      cfg.BankRelaySecret,
		BinancePaySecret:     cfg.BinancePaySecret,
		PayoneerSecret:       cfg.PayoneerSecret,
		USDTReceiverAddress:  cfg.USDTReceiverAddress,
		USDTContract:         cfg.USDTContract,
		USDTChain:            cfg.USDTChain,
		RequireSignature:     cfg.RequireSignature,
	}
	// The replay guard is the durable "seen once" store the notice envelope
	// needs; without it an unsigned replay would be accepted twice.
	if st != nil {
		rc.Guard = st
	}

	// registry.New offers only the rails whose secret is present and refuses
	// to boot with none: a gateway that accepts webhooks for an unconfigured
	// rail would be shipping a fake payment path.
	rc.Logf = func(format string, args ...any) { logger.Info(fmt.Sprintf(format, args...)) }
	if reg, err := registry.New(rc); err == nil {
		return reg, nil
	} else if cfg.DevMode {
		// Dev mode with no secrets at all: boot with an empty registry so the
		// operator can exercise order/wallet endpoints. Every webhook answers
		// "no adapter" until a secret exists.
		logger.Warn("dev mode with no rail secret: webhooks answer no_adapter", "err", err)
		return registry.NewWithAdapters(nil), nil
	} else {
		return nil, fmt.Errorf("registry: %w", err)
	}
}

// botStore adapts *store.Store + *wallet.Ledger to bot.Store. The bot package
// takes interfaces on purpose so it can be tested without a database.
type botStore struct {
	st *store.Store
	w  *wallet.Wallet
}

func (b botStore) Order(ctx context.Context, id string) (*types.Order, error) {
	return b.st.Order(ctx, id)
}

func (b botStore) WalletBalance(ctx context.Context, userID string) (int64, error) {
	return b.w.AvailableMinor(ctx, userID)
}

// lookupFunc adapts registry.Registry to api.AdapterLookup. The API package
// defines its own tiny interface on purpose, so it never imports the rail
// layer and its tests can inject fakes.
type lookupFunc func(types.Method) adapter.PaymentAdapter

func (f lookupFunc) Get(m types.Method) adapter.PaymentAdapter { return f(m) }

func adaptersFrom(reg *registry.Registry) func(types.Method) adapter.PaymentAdapter {
	if reg == nil {
		return func(types.Method) adapter.PaymentAdapter { return nil }
	}
	return func(m types.Method) adapter.PaymentAdapter {
		a, err := reg.Lookup(m)
		if err != nil {
			return nil
		}
		return a
	}
}

// sweepLoop expires past-due orders and refunds wallet payments on a ticker.
func sweepLoop(ctx context.Context, logger *slog.Logger, st *store.Store, w *wallet.Wallet, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if expired, err := w.RefundExpired(ctx, st); err != nil {
			if !errors.Is(err, context.Canceled) {
				logger.Error("expiry sweep", "err", err)
			}
		} else if len(expired) > 0 {
			logger.Info("expired orders", "count", len(expired))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// logRequests is the only middleware: one line per request, so a webhook
// mystery is debuggable from the logs alone.
func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"dur_ms", time.Since(start).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func methodNames(ms []types.Method) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, string(m))
	}
	return out
}
