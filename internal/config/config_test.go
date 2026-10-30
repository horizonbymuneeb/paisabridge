package config_test

import (
	"testing"
	"time"

	"github.com/horizonbymuneeb/paisabridge/internal/config"
	"github.com/horizonbymuneeb/paisabridge/internal/types"
)

func TestLoadDefaultsInDev(t *testing.T) {
	t.Setenv("PB_DEV", "1")
	t.Setenv("PB_ADMIN_TOKEN", "")
	t.Setenv("PB_ADDR", "")
	t.Setenv("PB_DB", "")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Addr != ":8080" {
		t.Fatalf("addr = %q", cfg.Addr)
	}
	if cfg.OrderTTL != 30*time.Minute {
		t.Fatalf("ttl = %v", cfg.OrderTTL)
	}
	if cfg.DiscountBPS != 500 {
		t.Fatalf("discount = %d, want 500 (5%%)", cfg.DiscountBPS)
	}
	if len(cfg.EnabledMethods) != 7 {
		t.Fatalf("methods = %v", cfg.EnabledMethods)
	}
	if !cfg.DevMode {
		t.Fatal("dev mode not detected")
	}
}

func TestAdminTokenRequiredOutsideDev(t *testing.T) {
	t.Setenv("PB_DEV", "")
	t.Setenv("PB_ADMIN_TOKEN", "")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected an error when PB_ADMIN_TOKEN is unset outside dev mode")
	}
	t.Setenv("PB_ADMIN_TOKEN", "s3cret")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load with token: %v", err)
	}
	if cfg.AdminToken != "s3cret" {
		t.Fatalf("token = %q", cfg.AdminToken)
	}
}

func TestOverridesAndValidation(t *testing.T) {
	t.Setenv("PB_DEV", "1")
	t.Setenv("PB_ADDR", "127.0.0.1:9999")
	t.Setenv("PB_ORDER_TTL", "5m")
	t.Setenv("PB_DISCOUNT_BPS", "750")
	t.Setenv("PB_METHODS", "easypaisa, bank ,usdt")
	t.Setenv("PB_EASYPAISA_SECRET", "abc")
	t.Setenv("PB_SWEEP_INTERVAL", "0s")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Addr != "127.0.0.1:9999" || cfg.OrderTTL != 5*time.Minute || cfg.DiscountBPS != 750 {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
	if len(cfg.EnabledMethods) != 3 || cfg.EnabledMethods[1] != types.MethodBank {
		t.Fatalf("methods = %v", cfg.EnabledMethods)
	}
	if cfg.SweepInterval != 0 {
		t.Fatalf("sweep interval = %v, want 0 (disabled)", cfg.SweepInterval)
	}
	if cfg.EasypaisaSecret != "abc" {
		t.Fatalf("rail secret not loaded")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestBadDiscountRejected(t *testing.T) {
	t.Setenv("PB_DEV", "1")
	t.Setenv("PB_DISCOUNT_BPS", "20000")
	if _, err := config.Load(); err == nil {
		t.Fatal("expected error for discount above 10000 bps")
	}
}
