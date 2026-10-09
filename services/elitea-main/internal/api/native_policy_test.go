package api

import (
	"context"
	"testing"

	v2discovery "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/discovery"
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
	policy.AllowShareOut, policy.AllowShareIn, policy.AllowCloudSTT = false, false, true
	policy.NotificationPreview = platformconfig.NotificationPreviewTitle
	policy.AllowNotificationActions, policy.AllowSystemSurfaces = false, true
	policy.LocalWork.Allowed = true
	policy.LocalWork.CommandDeny = []string{"rm -rf *"}
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
	// Client contract 1.3: the data controls are public too, so a share
	// extension or a widget can obey them before the app has a token.
	if public.AllowShareOut || public.AllowShareIn || !public.AllowCloudSTT || public.NotificationPreview != "title" ||
		public.AllowNotificationActions || !public.AllowSystemSurfaces {
		t.Fatalf("public data controls = %+v", public)
	}
	// Client contract 1.5: only local_work.allowed is public; the patterns
	// travel with a token.
	if !public.LocalWorkAllowed {
		t.Fatalf("public local_work_allowed = false, want true")
	}
	if minimums["ai.elitea.ios"] != "1.3.0" || minimums["ai.elitea.android"] != "1.1.0" || len(minimums) != 2 {
		t.Fatalf("minimums = %v", minimums)
	}
}

// TestDefaultPublicPolicyMatchesThePlatformDefault — discovery's own default
// (served when no policy source is composed) is the public subset of the
// platform default, so a client sees the same policy either way.
func TestDefaultPublicPolicyMatchesThePlatformDefault(t *testing.T) {
	svc := nativepolicy.NewWithLoader(func(context.Context) (platformconfig.NativeClientPolicy, error) {
		return platformconfig.DefaultNativeClientPolicy(), nil
	}, stubNativeClients{})
	public, _, err := nativePolicyDiscovery{policy: svc}.PublicPolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if public != v2discovery.DefaultPublicPolicy() {
		t.Fatalf("discovery default %+v differs from the platform default's public subset %+v",
			v2discovery.DefaultPublicPolicy(), public)
	}
}
