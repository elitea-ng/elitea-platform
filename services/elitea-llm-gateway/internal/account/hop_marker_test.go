package account

// hop_marker_test.go — the OUTBOUND half of hop-marker detection (issue #164).
//
// The inbound half (internal/llmproxy/hopguard.go) can only recognise a marker
// the gateway actually SENT. If GetConfigForProvider stops stamping it, every
// inbound test in this repository still passes and the guard detects nothing
// anywhere — the "built but not wired" shape this codebase has shipped before.

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/hopmarker"
)

// newHopTestAccount builds an account with the hop marker armed.
func newHopTestAccount(t *testing.T, secret string) *EliteaAccount {
	t.Helper()
	a, err := New(Config{
		DB:                  &fakeDB{},
		Vault:               &fakeVault{},
		ProviderConcurrency: 50,
		HopMarker:           hopmarker.New([]byte(secret)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// TestGetConfigForProvider_StampsHopMarkerOnEveryProvider is the outbound
// acceptance check. bifrost applies NetworkConfig.ExtraHeaders to the requests
// it sends for a provider, so this map IS the outbound stamp.
//
// It walks EVERY supported provider on purpose. A routing loop closes through
// whichever endpoint a credential names, so a provider class the marker skipped
// would be a class hop detection cannot see — and a per-class carve-out is the
// kind of change that reads as harmless in review.
func TestGetConfigForProvider_StampsHopMarkerOnEveryProvider(t *testing.T) {
	a := newHopTestAccount(t, "hop-secret")
	want := hopmarker.New([]byte("hop-secret")).Value()

	for _, provider := range supportedProviders {
		cfg, err := a.GetConfigForProvider(provider)
		if err != nil {
			t.Fatalf("GetConfigForProvider(%s): %v", provider, err)
		}
		got := cfg.NetworkConfig.ExtraHeaders[hopmarker.Header]
		if got != want {
			t.Errorf("provider %s: ExtraHeaders[%s] = %q, want %q.\n"+
				"A provider that carries no marker is a provider whose loop the gateway cannot detect.",
				provider, hopmarker.Header, got, want)
		}
	}
}

// TestGetConfigForProvider_UnarmedStampsNothing pins the no-GATEWAY_HOP_SECRET
// deployment: it must send exactly what it sent before hop detection existed.
// An empty-valued header is not the same as an absent one — some upstreams
// reject a header they do not know with an empty value — so the key must be
// missing entirely.
func TestGetConfigForProvider_UnarmedStampsNothing(t *testing.T) {
	a := newTestAccount(t, &fakeDB{}, &fakeVault{}) // no HopMarker

	cfg, err := a.GetConfigForProvider(schemas.OpenAI)
	if err != nil {
		t.Fatalf("GetConfigForProvider: %v", err)
	}
	if _, present := cfg.NetworkConfig.ExtraHeaders[hopmarker.Header]; present {
		t.Errorf("an unarmed account stamped %s", hopmarker.Header)
	}
}

// TestGetConfigForProvider_HopMarkerDoesNotDisturbTheEgressDecision pins that
// the stamp is additive. AllowPrivateNetwork is the issue #13 egress control
// and it shares this method; a marker that reset the config would silently
// re-open private-network dialing for every provider.
func TestGetConfigForProvider_HopMarkerDoesNotDisturbTheEgressDecision(t *testing.T) {
	a := newHopTestAccount(t, "hop-secret")

	for _, provider := range []schemas.ModelProvider{schemas.OpenAI, schemas.VLLM, schemas.Ollama} {
		cfg, err := a.GetConfigForProvider(provider)
		if err != nil {
			t.Fatalf("GetConfigForProvider(%s): %v", provider, err)
		}
		// No allowlist is configured here, so the SSRF-safe dialer must stay on
		// for every provider class including the self-hosted ones.
		if cfg.NetworkConfig.AllowPrivateNetwork {
			t.Errorf("provider %s: AllowPrivateNetwork = true with no egress allowlist configured", provider)
		}
		if cfg.ConcurrencyAndBufferSize.Concurrency != 50 {
			t.Errorf("provider %s: Concurrency = %d, want 50", provider, cfg.ConcurrencyAndBufferSize.Concurrency)
		}
	}
}
