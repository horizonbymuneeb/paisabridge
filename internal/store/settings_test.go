package store

import (
	"context"
	"testing"
)

// TestIntegrationLifecycle: absent, written, overwritten, cleared.
func TestIntegrationLifecycle(t *testing.T) {
	ctx := context.Background()
	st := newStoreFor(t)

	if v, ok, err := st.Integration(ctx, IntegrationTelegramToken); ok || err != nil || v != "" {
		t.Fatalf("missing key = (%q, %v, %v), want empty/false/nil", v, ok, err)
	}
	if err := st.SetIntegration(ctx, IntegrationTelegramToken, "123456:ABC"); err != nil {
		t.Fatalf("set: %v", err)
	}
	v, ok, err := st.Integration(ctx, IntegrationTelegramToken)
	if err != nil || !ok || v != "123456:ABC" {
		t.Fatalf("read after set = (%q, %v, %v)", v, ok, err)
	}
	if err := st.SetIntegration(ctx, IntegrationTelegramToken, "654321:DEF"); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if v, _, _ := st.Integration(ctx, IntegrationTelegramToken); v != "654321:DEF" {
		t.Fatalf("overwrite did not take: %q", v)
	}
	if err := st.DeleteIntegration(ctx, IntegrationTelegramToken); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, _ := st.Integration(ctx, IntegrationTelegramToken); ok {
		t.Fatal("deleted key still reported as present")
	}
	// Deleting a key that never existed is not an error: the caller's intent
	// ("make sure this is unset") is already satisfied.
	if err := st.DeleteIntegration(ctx, "never-written"); err != nil {
		t.Fatalf("delete of a missing key: %v", err)
	}
}

// TestIntegrationRejectsEmptyKey keeps a typo from writing a row nobody reads.
func TestIntegrationRejectsEmptyKey(t *testing.T) {
	if err := newStoreFor(t).SetIntegration(context.Background(), "", "x"); err == nil {
		t.Fatal("empty key must be refused")
	}
}

// TestIntegrationUpdatedAt: the timestamp is written and readable, and absent
// before the first write.
func TestIntegrationUpdatedAt(t *testing.T) {
	ctx := context.Background()
	st := newStoreFor(t)
	if _, ok, err := st.IntegrationUpdatedAt(ctx, IntegrationLastTestAt); ok || err != nil {
		t.Fatalf("absent key = (ok=%v, err=%v)", ok, err)
	}
	if err := st.SetIntegration(ctx, IntegrationLastTestAt, "2026-10-31T00:00:00.000000000Z"); err != nil {
		t.Fatalf("set: %v", err)
	}
	at, ok, err := st.IntegrationUpdatedAt(ctx, IntegrationLastTestAt)
	if err != nil || !ok || at.IsZero() {
		t.Fatalf("updated at = (%v, %v, %v)", at, ok, err)
	}
}

// TestIntegrationClosedStore: a closed handle must error, not panic.
func TestIntegrationClosedStore(t *testing.T) {
	st := newStoreFor(t)
	_ = st.Close()
	ctx := context.Background()
	if _, _, err := st.Integration(ctx, "k"); err == nil {
		t.Error("Integration on a closed store should error")
	}
	if err := st.SetIntegration(ctx, "k", "v"); err == nil {
		t.Error("SetIntegration on a closed store should error")
	}
	if err := st.DeleteIntegration(ctx, "k"); err == nil {
		t.Error("DeleteIntegration on a closed store should error")
	}
	if _, _, err := st.IntegrationUpdatedAt(ctx, "k"); err == nil {
		t.Error("IntegrationUpdatedAt on a closed store should error")
	}
}
