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
	"crypto/subtle"
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
	"github.com/horizonbymuneeb/paisabridge/internal/adminui"
	"github.com/horizonbymuneeb/paisabridge/internal/api"
	"github.com/horizonbymuneeb/paisabridge/internal/bot"
	"github.com/horizonbymuneeb/paisabridge/internal/config"
	"github.com/horizonbymuneeb/paisabridge/internal/match"
	"github.com/horizonbymuneeb/paisabridge/internal/metrics"
	"github.com/horizonbymuneeb/paisabridge/internal/outbound"
	"github.com/horizonbymuneeb/paisabridge/internal/store"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
	"github.com/horizonbymuneeb/paisabridge/internal/version"
	"github.com/horizonbymuneeb/paisabridge/internal/wallet"
)

func main() {
	// -version / --version prints build identity and exits before config
	// validation, so "what is deployed?" never needs a running server.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-version", "--version", "version":
			v := version.Get()
			fmt.Printf("paisabridge %s (%s %s/%s)\n", v.Version, v.Go, v.OS, v.Arch)
			return
		case "-h", "--help", "help":
			fmt.Fprint(os.Stderr, helpText)
			return
		}
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "paisabridge:", err)
		os.Exit(1)
	}
}

const helpText = `paisabridge - open-source payment bridge

Usage:
  paisabridge            run with the configuration in the environment
  paisabridge -version   print build identity
  paisabridge -help      this text
  pbsend                 mint a signed notice for testing or a relay

Configuration is environment-only; see docs/OPERATIONS.md for every variable.
Required to start: PB_ADMIN_TOKEN and at least one rail secret (or PB_DEV=1).
`

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
	// --- outbound notifications ----------------------------------------
	// The merchant side of the contract: every customer-visible transition is
	// pushed to the integrator, signed, with a stable delivery id.
	var (
		notify    func(context.Context, string, *types.Order)
		redeliver func(context.Context, string) error
	)
	if cfg.OutboundURL != "" {
		sender, err := outbound.New(outbound.Config{
			URL:    cfg.OutboundURL,
			Secret: cfg.OutboundSecret,
			Logf:   func(f string, a ...any) { logger.Info(fmt.Sprintf(f, a...)) },
		})
		if err != nil {
			return fmt.Errorf("outbound: %w", err)
		}
		// Park deliveries that exhaust their retries instead of losing them: an
		// order marked fulfilled must eventually reach the storefront even if
		// the storefront was down for an hour.
		sender.UseDeadLetter(deadLetterSink{st: st}, nil)
		notify = func(ctx context.Context, event string, o *types.Order) {
			deliveryID := event + ":" + o.ID + ":" + string(o.Status)
			if err := sender.Notify(ctx, event, deliveryID, o); err != nil {
				logger.Error("outbound delivery failed", "event", event, "order", o.ID, "err", err)
			}
		}
		redeliver = func(ctx context.Context, id string) error {
			row, err := st.DeadLetter(ctx, id)
			if err != nil {
				return err
			}
			out := &outbound.DeadLetter{
				DeliveryID:  row.DeliveryID,
				Event:       row.Event,
				OrderID:     row.OrderID,
				LastError:   row.LastError,
				Attempts:    row.Attempts,
				Body:        row.Body,
				AttemptedAt: row.CreatedAt,
			}
			if err := sender.Redeliver(ctx, out); err != nil {
				_ = st.FinishDeadLetter(ctx, id, false, err.Error(), time.Now().UTC())
				return err
			}
			return st.FinishDeadLetter(ctx, id, true, "", time.Now().UTC())
		}
		logger.Info("outbound notifications enabled", "url", cfg.OutboundURL)
	}

	// --- metrics --------------------------------------------------------
	// A nil registry keeps the API's metric calls harmless; when enabled the
	// same registry backs the /metrics route, so the counters the API fills
	// are exactly the ones scraped.
	var metricsReg *metrics.Registry
	if cfg.Metrics {
		metricsReg = metrics.New()
		metrics.Register(metricsReg)
		metricsReg.SetEnabled(true)
	}

	apiSrv, err := api.New(api.Config{
		Store:               st,
		Wallet:              w,
		Matcher:             m,
		Adapters:            lookupFunc(adaptersFrom(reg)),
		AdminToken:          cfg.AdminToken,
		OrderTTL:            cfg.OrderTTL,
		Logger:              logger,
		EnabledMethods:      cfg.EnabledMethods,
		Notify:              notify,
		TrustProxy:          cfg.TrustProxy,
		RedeliverDeadLetter: redeliver,
		Metrics:             recorderFor(metricsReg),
	})
	if err != nil {
		return err
	}

	// --- one mux --------------------------------------------------------
	// The API owns /api/*, the operator console owns /admin/*, and /metrics is
	// mounted separately so the two surfaces stay independently switchable.
	root := http.NewServeMux()
	root.Handle("/", apiSrv)

	// The operator console gets its own listener rather than a path prefix on
	// the public mux: its pages link to root-relative paths, and keeping it
	// separate means it can be bound to loopback or a VPN address and never
	// exposed on the payment port by accident.
	var adminSrv *http.Server
	if cfg.AdminAddr != "" {
		if cfg.AdminToken == "" {
			logger.Warn("admin console not started: PB_ADMIN_TOKEN is empty")
		} else if ui, uiErr := adminui.New(adminui.Config{
			Store:          st,
			Wallet:         w,
			AdminToken:     cfg.AdminToken,
			InsecureCookie: cfg.AdminInsecureCookie,
		}); uiErr != nil {
			logger.Error("admin console disabled", "err", uiErr)
		} else if ui == nil {
			logger.Warn("admin console disabled")
		} else {
			adminSrv = &http.Server{
				Addr:              cfg.AdminAddr,
				Handler:           ui,
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      30 * time.Second,
				IdleTimeout:       90 * time.Second,
			}
			go func() {
				logger.Info("admin console listening", "addr", cfg.AdminAddr)
				if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					logger.Error("admin console stopped", "err", err)
				}
			}()
		}
	}

	if metricsReg != nil {
		root.Handle("/metrics", metricsGuard(cfg.MetricsToken, metricsReg))
		logger.Info("metrics enabled", "path", "/metrics", "token_required", cfg.MetricsToken != "")
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           logRequests(logger, recorderFor(metricsReg), root),
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
	if adminSrv != nil {
		if err := adminSrv.Shutdown(shutdownCtx); err != nil {
			logger.Error("admin shutdown", "err", err)
		}
	}
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

// recorderFor returns a Recorder, or nil when metrics are off, so the API's
// noteMetrics short-circuits instead of calling into a dead registry.
func recorderFor(reg *metrics.Registry) api.Recorder {
	if reg == nil || !reg.Enabled() {
		return nil
	}
	return metricsRecorder{}
}

// deadLetterSink adapts store.Store to outbound.Sink. The two packages declare
// their own DeadLetter types on purpose (the store must not import the delivery
// code), so this shim owns the translation.
type deadLetterSink struct{ st *store.Store }

func (d deadLetterSink) Put(ctx context.Context, dl *outbound.DeadLetter) error {
	return d.st.PutDeadLetter(ctx, &store.DeadLetter{
		DeliveryID: dl.DeliveryID,
		Event:      dl.Event,
		OrderID:    dl.OrderID,
		LastError:  dl.LastError,
		Attempts:   dl.Attempts,
		Body:       dl.Body,
		CreatedAt:  dl.AttemptedAt,
	})
}

// metricsRecorder adapts the internal/metrics handles to api.Recorder. It is a
// thin shim on purpose: the API knows the label vocabulary, the exporter knows
// the wire format, and neither imports the other.
type metricsRecorder struct{}

func (metricsRecorder) RequestDone(class, statusClass string, seconds float64) {
	metrics.HTTPRequests.Inc(class, statusClass)
	metrics.HTTPDuration.Observe(seconds, class)
}

func (metricsRecorder) OrderCreated(method, status string) {
	metrics.Orders.Inc(method, status)
}

func (metricsRecorder) PaymentProcessed(outcome, reason string) {
	metrics.Payments.Inc(outcome, reason)
}

func (metricsRecorder) WebhookSignatureFailure(method string) {
	metrics.WebhookFailures.Inc(method)
}

func (metricsRecorder) RateLimited(class string) {
	metrics.RateLimited.Inc(class)
}

// metricsGuard optionally requires X-Metrics-Token. A scrape endpoint on a
// public host leaks order volume and rail mix, so the token is a cheap way to
// keep the numbers to the monitoring system.
func metricsGuard(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Metrics-Token")), []byte(token)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// logRequests is the only middleware: one line per request, so a webhook
// mystery is debuggable from the logs alone. When metrics are on it also
// records the request, because the handler is the only place that sees both the
// final status and the elapsed time.
func logRequests(logger *slog.Logger, rec api.Recorder, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sr, r)
		elapsed := time.Since(start)
		logger.Info("http", "method", r.Method, "path", r.URL.Path, "status", sr.status,
			"dur_ms", elapsed.Milliseconds())
		if rec != nil {
			rec.RequestDone(routeClass(r.URL.Path), statusClass(sr.status), elapsed.Seconds())
		}
	})
}

// routeClass reduces a URL to the bounded set the metrics use, so a scanner
// probing random paths cannot grow cardinality. Order and notice ids collapse
// into their collection class.
func routeClass(path string) string {
	switch {
	case path == "/api/orders":
		return "orders_create"
	case strings.HasPrefix(path, "/api/orders/"):
		return "orders_item"
	case strings.HasPrefix(path, "/api/webhooks/"):
		return "webhooks"
	case path == "/api/wallet" || strings.HasPrefix(path, "/api/wallet/"):
		return "wallet"
	case strings.HasPrefix(path, "/api/admin/"):
		return "admin"
	case path == "/api/notices":
		return "notices"
	case path == "/api/healthz" || path == "/api/readyz":
		return "health"
	case strings.HasPrefix(path, "/admin"):
		return "console"
	case path == "/metrics":
		return "metrics"
	default:
		return "other"
	}
}

// statusClass collapses a status code to 2xx/3xx/4xx/5xx.
func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	default:
		return "2xx"
	}
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
