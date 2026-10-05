package api

import (
	"context"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/nativepolicy"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/nativeauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

type stubNativeClients []nativeauth.Client

func (s stubNativeClients) Clients(context.Context) ([]nativeauth.Client, error) { return s, nil }

// TestNativePolicyDiscoveryPublishesThePublicSubset — the subset a client may
// act on before sign-in, and the per-client effective minimums.
func TestNativePolicyDiscoveryPublishesThePublicSubset(t *testing.T) {
	policy := platformconfig.DefaultNativeClientPolicy()
	policy.RequireDeviceLock = true
	policy.OfflineRetentionDays = 0
	policy.MinClientVersion = "1.1.0"
	policy.AllowScreenshots = false
	svc := nativepolicy.NewWithLoader(func(context.Context) (platformconfig.NativeClientPolicy, error) {
		return policy, nil
	}, stubNativeClients{
		{ClientID: "ai.elitea.ios", Enabled: true, MinClientVersion: "1.3.0"},
		{ClientID: "ai.elitea.android", Enabled: true},
	})
	public, minimums, err := nativePolicyDiscovery{policy: svc}.PublicPolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !public.RequireDeviceLock || public.OfflineEnabled || public.MinClientVersion != "1.1.0" {
		t.Fatalf("public subset = %+v", public)
	}
	if minimums["ai.elitea.ios"] != "1.3.0" || minimums["ai.elitea.android"] != "1.1.0" || len(minimums) != 2 {
		t.Fatalf("minimums = %v", minimums)
	}
}
