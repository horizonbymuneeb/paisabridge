// Package config loads paisabridge settings from the environment.
//
// Everything has a safe default so `go run ./cmd/paisabridge` starts on a
// laptop with no setup. Production overrides come from env vars only — no
// config files to drift, no secrets in the repo.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

// Config is the full runtime configuration.
type Config struct {
	// Addr is the HTTP listen address, e.g. ":8080".
	Addr string
	// DBPath is the SQLite file. ":memory:" is supported for tests.
	DBPath string
	// AdminToken guards POST /api/admin/approve via X-Admin-Token.
	// Required: the server refuses to start without one outside dev mode.
	AdminToken string
	// DevMode relaxes the admin-token requirement and logs verbosely.
	DevMode bool
	// OrderTTL is how long an order stays matchable before the sweeper
	// expires (and refunds) it.
	OrderTTL time.Duration
	// SweepInterval is how often expired orders are swept. Zero disables the
	// background sweeper (tests drive it explicitly).
	SweepInterval time.Duration
	// DiscountBPS is the prepay discount in basis points (500 = 5%).
	DiscountBPS int64
	// WalletCurrency is the settlement currency of the wallet.
	WalletCurrency string
	// EnabledMethods restricts which rails the webhook route accepts.
	EnabledMethods []types.Method
	// WebhookSecrets maps method -> shared secret for HMAC verification of
	// the generic notice feed (adapters may carry their own secrets).
	WebhookSecrets map[string]string
	// PublicURL is used in generated links (checkout deep links).
	PublicURL string

	// Rail credentials, consumed by internal/adapters/registry. Empty values
	// keep a rail unregistered (the webhook route then answers no_adapter).
	EasypaisaSecret      string
	JazzCashSalt         string
	NayaPayWebhookSecret string
	NayaPayToken         string
	NayaPayBaseURL       string
	SadaPayWebhookSecret string
	BankRelaySecret      string
	BinancePaySecret     string
	PayoneerSecret       string
	USDTReceiverAddress  string
	USDTContract         string
	USDTChain            int64
	// RequireSignature (default true) makes a missing rail secret fatal at
	// startup instead of silently accepting unsigned callbacks.
	RequireSignature bool
	// SeedDemoTopup, when set, credits this many paisa to the default wallet
	// at boot so a local demo can claim orders. Never set in production.
	SeedDemoTopup int64
}

// Load reads the environment. It returns an error for unusable values instead
// of silently falling back, because a bad port or a non-numeric discount bps
// is a deployment bug worth failing loudly on.
func Load() (*Config, error) {
	cfg := &Config{
		Addr:           env("PB_ADDR", ":8080"),
		DBPath:         env("PB_DB", "paisabridge.db"),
		AdminToken:     os.Getenv("PB_ADMIN_TOKEN"),
		DevMode:        envBool("PB_DEV", false),
		OrderTTL:       envDur("PB_ORDER_TTL", 30*time.Minute),
		SweepInterval:  envDur("PB_SWEEP_INTERVAL", time.Minute),
		DiscountBPS:    envInt("PB_DISCOUNT_BPS", 500),
		WalletCurrency: env("PB_WALLET_CURRENCY", "PKR"),
		WebhookSecrets: map[string]string{},
		PublicURL:      env("PB_PUBLIC_URL", "http://localhost:8080"),

		EasypaisaSecret:      os.Getenv("PB_EASYPAISA_SECRET"),
		JazzCashSalt:         os.Getenv("PB_JAZZCASH_SALT"),
		NayaPayWebhookSecret: os.Getenv("PB_NAYAPAY_WEBHOOK_SECRET"),
		NayaPayToken:         os.Getenv("PB_NAYAPAY_TOKEN"),
		NayaPayBaseURL:       os.Getenv("PB_NAYAPAY_BASE_URL"),
		SadaPayWebhookSecret: os.Getenv("PB_SADAPAY_WEBHOOK_SECRET"),
		BankRelaySecret:      os.Getenv("PB_BANK_RELAY_SECRET"),
		BinancePaySecret:     os.Getenv("PB_BINANCEPAY_SECRET"),
		PayoneerSecret:       os.Getenv("PB_PAYONEER_SECRET"),
		USDTReceiverAddress:  os.Getenv("PB_USDT_RECEIVER"),
		USDTContract:         os.Getenv("PB_USDT_CONTRACT"),
		USDTChain:            envInt("PB_USDT_CHAIN", 0),
		RequireSignature:     envBool("PB_REQUIRE_SIGNATURE", true),
		SeedDemoTopup:        envInt("PB_SEED_DEMO_TOPUP", 0),
	}

	methods := strings.TrimSpace(os.Getenv("PB_METHODS"))
	if methods == "" {
		cfg.EnabledMethods = []types.Method{
			types.MethodEasypaisa, types.MethodJazzCash, types.MethodNayaPay,
			types.MethodSadaPay, types.MethodBank, types.MethodUSDT, types.MethodBinance,
		}
	} else {
		for _, m := range strings.Split(methods, ",") {
			if m = strings.TrimSpace(m); m != "" {
				cfg.EnabledMethods = append(cfg.EnabledMethods, types.Method(m))
			}
		}
	}

	// PB_SECRET_<METHOD>=<hex secret>, e.g. PB_SECRET_EASYPAISA=...
	for _, m := range cfg.EnabledMethods {
		key := "PB_SECRET_" + strings.ToUpper(strings.ReplaceAll(string(m), "-", "_"))
		if v := os.Getenv(key); v != "" {
			cfg.WebhookSecrets[string(m)] = v
		}
	}

	if cfg.DiscountBPS < 0 || cfg.DiscountBPS > 10000 {
		return nil, fmt.Errorf("config: PB_DISCOUNT_BPS must be 0..10000, got %d", cfg.DiscountBPS)
	}
	if cfg.OrderTTL <= 0 {
		return nil, errors.New("config: PB_ORDER_TTL must be positive")
	}
	if strings.TrimSpace(cfg.AdminToken) == "" && !cfg.DevMode {
		// Deterministic dev token instead of refusing for the common
		// `go run` case would be unsafe; make the operator choose.
		return nil, errors.New("config: PB_ADMIN_TOKEN is required (set PB_DEV=1 for local development)")
	}
	if cfg.DevMode && cfg.AdminToken == "" {
		cfg.AdminToken = "dev-admin-token"
	}
	return cfg, nil
}

// Validate checks the invariants that a half-filled env can violate.
func (c *Config) Validate() error {
	if c.Addr == "" {
		return errors.New("config: empty addr")
	}
	if c.DBPath == "" {
		return errors.New("config: empty db path")
	}
	if len(c.EnabledMethods) == 0 {
		return errors.New("config: no payment methods enabled")
	}
	return nil
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt(key string, def int64) int64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

func envDur(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
