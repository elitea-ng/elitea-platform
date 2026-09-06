package llmproxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/account"
)

// checkconnection_egress_predicate_test.go pins ONE property: the probe and
// the request path answer "may this credential reach a private address?" with
// the same predicate, read from the same gate.
//
// THE DEFECT. The probe asked EgressAllowlistConfigured ("is anything set at
// all") while the dialer asked allowsPrivateNetwork ("does an entry name a
// private host or CIDR"). With a name-only entry — the chart's own example —
// the two disagree: "Test connection" reported success and wrote status_ok,
// and every chat turn then died in the dialer. These tests drive the REAL
// account, so they measure the gate the request path uses, not a double.

// probeStubVault satisfies the account's vault seam. No test here resolves a
// credential: every assertion is about the egress gate.
type probeStubVault struct{}

func (probeStubVault) Resolve(context.Context, string, string) (string, error) { return "", nil }

// probeEgressSource stands in for the compiled governance snapshot — the
// authored `egress_allowlist` rows the gateway polls from the database.
type probeEgressSource struct{ entries []string }

func (s *probeEgressSource) EgressAllowlist() []string { return append([]string{}, s.entries...) }

// newProbeEgressAccount builds a real *account.EliteaAccount with the given
// environment floor and, when db is not empty, an authored governance source.
func newProbeEgressAccount(t *testing.T, env, db []string) *account.EliteaAccount {
	t.Helper()
	acct, err := account.New(account.Config{
		DB:              account.NewPoolQuerier(nil),
		Vault:           probeStubVault{},
		EgressAllowlist: env,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("account.New: %v", err)
	}
	if len(db) > 0 {
		acct.SetEgressSource(&probeEgressSource{entries: db})
	}
	return acct
}

// dialerAllowsPrivateNetwork reads the value the REQUEST path actually dials
// with: bifrost takes NetworkConfig.AllowPrivateNetwork from
// GetConfigForProvider for the self-hosted classes.
func dialerAllowsPrivateNetwork(t *testing.T, acct *account.EliteaAccount, provider schemas.ModelProvider) bool {
	t.Helper()
	cfg, err := acct.GetConfigForProvider(provider)
	if err != nil {
		t.Fatalf("GetConfigForProvider(%s): %v", provider, err)
	}
	return cfg.NetworkConfig.AllowPrivateNetwork
}

// TestProbeEgressPredicate_MatchesTheDialer is the regression test. For each
// allowlist shape the probe's decision must EQUAL the dialer's decision.
//
// Mutation: put EgressAllowlistConfigured back in probeAllowsPrivateNetwork —
// the name-only case MUST fail, because that predicate answers true while the
// dialer answers false.
func TestProbeEgressPredicate_MatchesTheDialer(t *testing.T) {
	const privateBase = "http://vllm.ml.svc.cluster.local:8000/v1"
	const literalBase = "http://192.168.29.60:8000/v1"

	for _, test := range []struct {
		name    string
		env     []string
		db      []string
		apiBase string
		want    bool
		why     string
	}{
		{
			name:    "a name-only entry grants no private reachability",
			env:     []string{"vllm.ml.svc.cluster.local:8000"},
			apiBase: privateBase,
			want:    false,
			why:     "a name does not declare where it resolves, so the dialer keeps the SSRF guard on",
		},
		{
			name:    "an explicit host:port literal opens it",
			env:     []string{"192.168.29.60:8000"},
			apiBase: literalBase,
			want:    true,
			why:     "the operator named a private address",
		},
		{
			name:    "a CIDR entry beside the name opens it",
			env:     []string{"vllm.ml.svc.cluster.local:8000", "10.0.0.0/8"},
			apiBase: privateBase,
			want:    true,
			why:     "the CIDR is the declaration that private egress is intended",
		},
		{
			name:    "an authored row alone opens it, with an empty environment floor",
			db:      []string{"vllm.ml.svc.cluster.local:8000", "10.0.0.0/8"},
			apiBase: privateBase,
			want:    true,
			why:     "the gate merges the authored governance rows, so the probe must read them too",
		},
		{
			name:    "an authored name-only row alone does not open it",
			db:      []string{"vllm.ml.svc.cluster.local:8000"},
			apiBase: privateBase,
			want:    false,
			why:     "the authored half of the merge obeys the same rule as the floor",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			acct := newProbeEgressAccount(t, test.env, test.db)
			h := newCheckConnectionHandler(acct)
			req := checkConnectionRequest{Type: "open_ai", APIBase: test.apiBase}

			// The host itself is on the list in every case, so the only
			// decision under test is the private-network one.
			if !acct.EgressAllows(test.apiBase) {
				t.Fatalf("EgressAllows(%q) = false; the case does not measure what it claims", test.apiBase)
			}
			// The allowlist is armed in every case. That is exactly why
			// "is anything configured" cannot stand in for this decision.
			if !acct.EgressAllowlistConfigured() {
				t.Fatal("the allowlist is not armed; the case does not measure what it claims")
			}

			probe := h.probeAllowsPrivateNetwork(req)
			dialer := dialerAllowsPrivateNetwork(t, acct, schemas.VLLM)

			if probe != dialer {
				t.Fatalf("probe = %v but the dialer = %v: the probe decides a destination on other terms than the request path", probe, dialer)
			}
			if probe != test.want {
				t.Fatalf("probe = %v, want %v — %s", probe, test.want, test.why)
			}
			// Ollama is the other self-hosted class and must not diverge.
			if got := dialerAllowsPrivateNetwork(t, acct, schemas.Ollama); got != test.want {
				t.Fatalf("ollama dialer = %v, want %v", got, test.want)
			}
		})
	}
}

// TestProbeEgressPredicate_CloudClassStaysClosed keeps the other half of the
// predicate: a private allowlist entry does not make a CLOUD credential
// reachable on a private address, because GetConfigForProvider carves the
// dialer out for the self-hosted classes alone.
func TestProbeEgressPredicate_CloudClassStaysClosed(t *testing.T) {
	acct := newProbeEgressAccount(t, []string{"10.0.0.0/8"}, nil)
	h := newCheckConnectionHandler(acct)

	if h.probeAllowsPrivateNetwork(checkConnectionRequest{
		Type: "azure_open_ai", APIBase: "http://10.1.2.3:8000",
	}) {
		t.Fatal("a cloud credential got the private-network carve-out the request path never grants it")
	}
	if got := dialerAllowsPrivateNetwork(t, acct, schemas.Azure); got {
		t.Fatal("the azure dialer opened the private network; the two rules have diverged")
	}
}

// TestProbeEgressPredicate_NilPolicyFailsClosed pins the fail-closed default:
// a handler with no policy grants nothing.
func TestProbeEgressPredicate_NilPolicyFailsClosed(t *testing.T) {
	h := NewHandler(nil, nil, nil) // no WithEgressPolicy
	if h.probeAllowsPrivateNetwork(checkConnectionRequest{
		Type: "vllm", APIBase: "http://10.1.2.3:8000/v1",
	}) {
		t.Fatal("a handler with no egress policy opened the private network")
	}
}

// TestCheckConnection_ExplicitPrivateEntryDialsForReal is the positive half,
// end to end and through the real gate: an explicit host:port entry lets the
// probe reach the loopback provider, and the round trip really happens.
func TestCheckConnection_ExplicitPrivateEntryDialsForReal(t *testing.T) {
	fp := newFakeProvider(http.StatusOK)
	defer fp.Close()

	acct := newProbeEgressAccount(t, []string{probeHostPort(t, fp.URL)}, nil)
	h := newCheckConnectionHandler(acct)
	rec := doCheckConnection(t, h, checkConnectionRequest{
		Type: "open_ai", APIBase: fp.URL + "/v1", APIKey: "sk-test",
	})

	resp := decodeCheckConnectionResponse(t, rec)
	if !resp.Success {
		t.Fatalf("resp = %+v, want success: the operator named this private address", resp)
	}
	if fp.Hits() != 1 {
		t.Fatalf("expected exactly one real round trip, got %d", fp.Hits())
	}
	if !dialerAllowsPrivateNetwork(t, acct, schemas.VLLM) {
		t.Fatal("the probe dialled a destination the request path refuses")
	}
}

// TestCheckConnection_AuthoredRowAloneDialsForReal is the same proof for a
// gateway whose environment floor is EMPTY and whose only entry is an
// authored governance row. The probe must read the merged gate, not the
// environment.
func TestCheckConnection_AuthoredRowAloneDialsForReal(t *testing.T) {
	fp := newFakeProvider(http.StatusOK)
	defer fp.Close()

	acct := newProbeEgressAccount(t, nil, []string{probeHostPort(t, fp.URL)})
	h := newCheckConnectionHandler(acct)
	rec := doCheckConnection(t, h, checkConnectionRequest{
		Type: "vllm", APIBase: fp.URL + "/v1",
	})

	resp := decodeCheckConnectionResponse(t, rec)
	if !resp.Success {
		t.Fatalf("resp = %+v, want success: the authored row names this private address", resp)
	}
	if fp.Hits() != 1 {
		t.Fatalf("expected exactly one real round trip, got %d", fp.Hits())
	}
	if !dialerAllowsPrivateNetwork(t, acct, schemas.VLLM) {
		t.Fatal("the probe dialled a destination the request path refuses")
	}
}

// TestListProviderModels_PrivateBaseNeedsAPrivateEntry is the lister's half of
// the refusal. It shares one predicate with the checker, so a name-only
// allowlist must stop it at the dial as well — the fake provider is a real,
// listening loopback server, so a zero hit count proves the dial was refused.
func TestListProviderModels_PrivateBaseNeedsAPrivateEntry(t *testing.T) {
	fp := newFakeListProvider(http.StatusOK, `{"data":[{"id":"gpt-4o"}]}`)
	defer fp.Close()

	h := newCheckConnectionHandler(fakeEgressPolicy{allow: true, privateNetwork: false})
	resp := doListProviderModels(t, h, checkConnectionRequest{
		Type: "open_ai", APIBase: fp.URL + "/v1", APIKey: "sk-test",
	})

	if resp.Success {
		t.Fatalf("resp = %+v, want a refusal while no entry names a private destination", resp)
	}
	if resp.Reason != checkConnectionReasonUnreachable {
		t.Fatalf("reason = %q, want %q — the dialer refuses the address the same way",
			resp.Reason, checkConnectionReasonUnreachable)
	}
	if fp.Hits() != 0 {
		t.Fatalf("the dial must be refused, got %d hits", fp.Hits())
	}
}

// probeHostPort returns the "host:port" allowlist entry for a test server URL.
func probeHostPort(t *testing.T, rawURL string) string {
	t.Helper()
	trimmed := strings.TrimPrefix(rawURL, "http://")
	if trimmed == rawURL {
		t.Fatalf("unexpected test server scheme in %q", rawURL)
	}
	return trimmed
}
