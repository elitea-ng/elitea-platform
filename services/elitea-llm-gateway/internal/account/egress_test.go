package account

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// newEgressAccount builds an EliteaAccount with an egress allowlist configured.
func newEgressAccount(t *testing.T, db rowQuerier, vault vaultDecryptor, allowlist ...string) *EliteaAccount {
	t.Helper()
	a, err := New(Config{
		DB:                  db,
		Vault:               vault,
		ProviderConcurrency: 50,
		EgressAllowlist:     allowlist,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// TestGetConfigForProvider_PrivateNetworkGatedOnAllowlist is the primary issue
// #13 regression guard for the SSRF carve-out. Before the fix,
// GetConfigForProvider set AllowPrivateNetwork = true UNCONDITIONALLY for the
// vLLM and Ollama classes, and the URL those classes dial is a tenant-authored
// api_base — so any user who could author a credential row could make the
// gateway open a TCP connection to any RFC-1918 address the pod can reach.
//
// GAP G6 narrowed the gate further. It used to read "is ANY allowlist
// configured", which is not the same question. An allowlist naming only public
// SaaS hosts unlocked the whole private network, and an operator who wanted one
// private vLLM had to edit the chart. The gate now reads "does an entry
// EXPLICITLY name a private host or CIDR".
//
// Mutation: restore the unconditional `cfg.NetworkConfig.AllowPrivateNetwork =
// true` in GetConfigForProvider — the "no allowlist" subtest MUST fail. Change
// it back to `a.egress.configured()` — the "public hosts only" subtest MUST
// fail.
func TestGetConfigForProvider_PrivateNetworkGatedOnAllowlist(t *testing.T) {
	t.Run("no allowlist: SSRF dialer stays armed for every provider", func(t *testing.T) {
		a := newTestAccount(t, &fakeDB{}, &fakeVault{})
		for _, p := range supportedProviders {
			cfg, err := a.GetConfigForProvider(p)
			if err != nil {
				t.Fatalf("GetConfigForProvider(%s): %v", p, err)
			}
			if cfg.NetworkConfig.AllowPrivateNetwork {
				t.Fatalf("provider %s: AllowPrivateNetwork = true with NO egress allowlist — "+
					"a tenant-authored api_base can steer the gateway at an internal address (issue #13)", p)
			}
		}
	})

	t.Run("public hosts only: the dialer guard stays armed", func(t *testing.T) {
		// A NAME says nothing about where it resolves, so it cannot be read as
		// a declaration that private egress is intended. This is the half of
		// gap G6 that was too permissive.
		a := newEgressAccount(t, &fakeDB{}, &fakeVault{}, "vllm.ml.svc.cluster.local:8000")
		for _, p := range supportedProviders {
			cfg, err := a.GetConfigForProvider(p)
			if err != nil {
				t.Fatalf("GetConfigForProvider(%s): %v", p, err)
			}
			if cfg.NetworkConfig.AllowPrivateNetwork {
				t.Fatalf("provider %s: an allowlist of names alone unlocked the private network", p)
			}
		}
	})

	t.Run("an explicit private entry: self-hosted classes may dial private", func(t *testing.T) {
		a := newEgressAccount(t, &fakeDB{}, &fakeVault{},
			"vllm.ml.svc.cluster.local:8000", "10.0.0.0/8")
		for _, p := range []schemas.ModelProvider{schemas.VLLM, schemas.Ollama} {
			cfg, err := a.GetConfigForProvider(p)
			if err != nil {
				t.Fatalf("GetConfigForProvider(%s): %v", p, err)
			}
			if !cfg.NetworkConfig.AllowPrivateNetwork {
				t.Fatalf("provider %s: AllowPrivateNetwork = false although an entry names a private block — "+
					"the legitimate self-hosted vLLM/Ollama use case is broken", p)
			}
		}
	})

	t.Run("an explicit private entry: cloud providers keep the dialer guard", func(t *testing.T) {
		a := newEgressAccount(t, &fakeDB{}, &fakeVault{}, "192.168.29.60:8000")
		for _, p := range []schemas.ModelProvider{schemas.OpenAI, schemas.Azure, schemas.Anthropic} {
			cfg, err := a.GetConfigForProvider(p)
			if err != nil {
				t.Fatalf("GetConfigForProvider(%s): %v", p, err)
			}
			if cfg.NetworkConfig.AllowPrivateNetwork {
				t.Fatalf("provider %s: private-network dialing must never be enabled for a cloud provider class", p)
			}
		}
	})

	t.Run("an authored row alone unlocks the private network", func(t *testing.T) {
		// The whole point of gap G6: an on-premise endpoint must be reachable
		// without a chart edit and a restart.
		a := newTestAccount(t, &fakeDB{}, &fakeVault{})
		src := &fakeEgressSource{}
		a.SetEgressSource(src)
		if a.EgressPrivateNetworkAllowed() {
			t.Fatal("no entry anywhere, but the private network is open")
		}
		src.set("192.168.29.60:8000")
		cfg, err := a.GetConfigForProvider(schemas.VLLM)
		if err != nil {
			t.Fatalf("GetConfigForProvider: %v", err)
		}
		if !cfg.NetworkConfig.AllowPrivateNetwork {
			t.Fatal("an authored egress row naming a private host did not unlock the private network")
		}
	})
}

// TestGetKeysForProvider_EgressAllowlistRejectsHost covers the per-credential
// half of the policy: with an allowlist configured, a credential naming a host
// that is not on it yields no key at all.
func TestGetKeysForProvider_EgressAllowlistRejectsHost(t *testing.T) {
	db := &fakeDB{rows: [][]any{
		{"cfg-evil", "", []byte(`{"api_base":"http://elitea-main.default.svc:8080","api_key":"sk"}`)},
	}}
	a := newEgressAccount(t, db, &fakeVault{}, "vllm.ml.svc.cluster.local:8000")

	_, err := a.GetKeysForProvider(ctxWithProject("1"), schemas.VLLM)
	if err == nil {
		t.Fatal("expected the egress allowlist to reject a non-allowlisted api_base host")
	}
	if !errors.Is(err, ErrEgressNotAllowed) {
		t.Fatalf("error %v does not wrap ErrEgressNotAllowed", err)
	}
	if !strings.Contains(err.Error(), EgressNotAllowedReason) {
		t.Fatalf("error %q missing reason code", err.Error())
	}
	// The rejection must not hand the caller back the host it probed.
	if strings.Contains(err.Error(), "elitea-main") {
		t.Fatalf("error %q discloses the rejected host to the caller", err.Error())
	}
}

// TestGetKeysForProvider_EgressGuardBeforeVault is the credential-exfiltration
// half of issue #13: `api_key` may be a {{secret.NAME}} reference whose
// plaintext is decrypted from the project's Fernet vault and shipped to
// whatever host api_base names, as a Bearer token. The allowlist check must
// therefore run BEFORE the vault resolve, so a destination the operator never
// sanctioned never causes a decrypt.
//
// Mutation: move the `a.egress.allows(...)` block below the `a.vault.Resolve`
// call in GetKeysForProvider — this test MUST fail (the vault error surfaces
// instead of the egress error, proving the decrypt happened first).
func TestGetKeysForProvider_EgressGuardBeforeVault(t *testing.T) {
	db := &fakeDB{rows: [][]any{
		{"cfg-exfil", "", []byte(`{"api_base":"https://attacker.example.com/v1","api_key":"{{secret.PROVIDER_KEY}}"}`)},
	}}
	vault := &fakeVault{err: errors.New("vault must not be called for a non-allowlisted destination")}
	a := newEgressAccount(t, db, vault, "vllm.ml.svc.cluster.local:8000")

	_, err := a.GetKeysForProvider(ctxWithProject("1"), schemas.VLLM)
	if !errors.Is(err, ErrEgressNotAllowed) {
		t.Fatalf("expected the egress guard to fire BEFORE the vault resolve; got %v", err)
	}
}

// TestGetKeysForProvider_EgressAllowlistAdmitsAllowedHost proves the guard is a
// filter and not a wall: the operator's own vLLM instance still works.
func TestGetKeysForProvider_EgressAllowlistAdmitsAllowedHost(t *testing.T) {
	db := &fakeDB{rows: [][]any{
		{"cfg-ok", "self-hosted", []byte(`{"api_base":"http://vllm.ml.svc.cluster.local:8000","api_key":"sk"}`)},
	}}
	a := newEgressAccount(t, db, &fakeVault{}, "vllm.ml.svc.cluster.local:8000")

	keys, err := a.GetKeysForProvider(ctxWithProject("1"), schemas.VLLM)
	if err != nil {
		t.Fatalf("allowlisted host was rejected: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("got %d keys, want 1", len(keys))
	}
	if keys[0].VLLMKeyConfig == nil {
		t.Fatal("vLLM key config missing — the credential's api_base was not threaded through")
	}
}

// TestGetKeysForProvider_NoAllowlistLeavesHostsUnrestricted pins the default
// mode. Without an allowlist the host is unrestricted (only the DIALER stops
// private destinations), so existing cloud-provider installs are unaffected.
func TestGetKeysForProvider_NoAllowlistLeavesHostsUnrestricted(t *testing.T) {
	db := &fakeDB{rows: [][]any{
		{"cfg-1", "", []byte(`{"api_base":"https://api.openai.com/v1","api_key":"sk"}`)},
	}}
	a := newTestAccount(t, db, &fakeVault{})

	keys, err := a.GetKeysForProvider(ctxWithProject("1"), schemas.OpenAI)
	if err != nil {
		t.Fatalf("unexpected rejection with no allowlist configured: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("got %d keys, want 1", len(keys))
	}
}

func TestEgressAllowlist_Matching(t *testing.T) {
	a := newEgressAccount(t, &fakeDB{}, &fakeVault{},
		"vllm.ml.svc.cluster.local:8000",
		"ollama.internal",
		"*.openai.azure.com",
		"*.pinned.example:8443",
	)

	cases := []struct {
		apiBase string
		want    bool
		why     string
	}{
		{"", true, "empty api_base uses the provider default endpoint, which is not tenant-controlled"},
		{"http://vllm.ml.svc.cluster.local:8000/v1", true, "exact host:port"},
		{"http://vllm.ml.svc.cluster.local:9000/v1", false, "the entry pinned port 8000"},
		{"http://ollama.internal:11434", true, "host entry with no port matches any port"},
		{"https://ollama.internal", true, "host entry with no port matches the default port"},
		{"https://tenant.openai.azure.com/", true, "wildcard covers one leading label"},
		{"https://a.b.openai.azure.com/", true, "wildcard covers several leading labels"},
		{"https://openai.azure.com/", false, "wildcard requires at least one leading label"},
		{"https://evil-openai.azure.com.attacker.test/", false, "suffix must be a label boundary, not a substring"},
		{"https://OLLAMA.INTERNAL/v1", true, "host comparison is case-insensitive"},
		{"https://ollama.internal./v1", true, "a trailing FQDN dot must not evade the allowlist"},
		{"https://x.pinned.example:8443/v1", true, "wildcard entry with a pinned port"},
		{"https://x.pinned.example:9999/v1", false, "wildcard entry pinned a different port"},
		{"http://169.254.169.254/latest/meta-data/", false, "cloud metadata endpoint is not allowlisted"},
		{"http://127.0.0.1:8080", false, "loopback is not allowlisted"},
		{"::::not a url", false, "an unparsable api_base must fail closed"},
		{"/relative/path", false, "an api_base with no host must fail closed"},
	}
	for _, tc := range cases {
		if got := a.EgressAllows(tc.apiBase); got != tc.want {
			t.Errorf("EgressAllows(%q) = %v, want %v — %s", tc.apiBase, got, tc.want, tc.why)
		}
	}
}

func TestEgressAllowlist_Unconfigured(t *testing.T) {
	a := newTestAccount(t, &fakeDB{}, &fakeVault{})
	if a.EgressAllowlistConfigured() {
		t.Fatal("an empty allowlist must report itself unconfigured")
	}
	// With no allowlist the host check is inert; the dialer is what constrains.
	if !a.EgressAllows("http://127.0.0.1:8080") {
		t.Fatal("an unconfigured allowlist must not restrict hosts")
	}
}

func TestEgressAllowlist_RejectsMalformedEntries(t *testing.T) {
	for _, bad := range []string{
		"https://host/path",
		"host/path",
		"ev*il.example",
		"*.",
		"a b",
	} {
		_, err := New(Config{DB: &fakeDB{}, Vault: &fakeVault{}, EgressAllowlist: []string{bad}})
		if err == nil {
			t.Errorf("New with GATEWAY_EGRESS_ALLOWLIST=%q accepted a malformed entry — a typo must not "+
				"silently drop or widen a rule", bad)
		}
	}
}

// fakeEgressSource stands in for the compiled governance snapshot.
type fakeEgressSource struct {
	mu      sync.Mutex
	entries []string
}

func (f *fakeEgressSource) set(entries ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = entries
}

func (f *fakeEgressSource) EgressAllowlist() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.entries...)
}

// TestEgressGate_UnionAndFloor is the gap-G6 contract in one place: the
// environment variable is a FLOOR that an authored row can only widen.
//
// Mutation: make the merge an intersection, or let the authored set replace the
// environment set — the "the floor survives" subtest MUST fail.
func TestEgressGate_UnionAndFloor(t *testing.T) {
	src := &fakeEgressSource{}
	a := newEgressAccount(t, &fakeDB{}, &fakeVault{}, "api.openai.com")
	a.SetEgressSource(src)

	t.Run("the floor applies before any row exists", func(t *testing.T) {
		if !a.EgressAllows("https://api.openai.com/v1") {
			t.Fatal("the environment floor is not enforced")
		}
		if a.EgressAllows("http://192.168.29.60:8000/v1") {
			t.Fatal("a host on neither list was admitted")
		}
	})

	t.Run("an authored row widens the list", func(t *testing.T) {
		src.set("192.168.29.60:8000")
		if !a.EgressAllows("http://192.168.29.60:8000/v1") {
			t.Fatal("an authored entry did not take effect")
		}
	})

	t.Run("the floor survives an authored set that omits it", func(t *testing.T) {
		src.set("192.168.29.60:8000")
		if !a.EgressAllows("https://api.openai.com/v1") {
			t.Fatal("an authored set withdrew an environment entry; the environment variable is a FLOOR " +
				"and no admin session may cut the platform off from the host the chart named")
		}
	})

	t.Run("withdrawing the rows returns to the floor", func(t *testing.T) {
		src.set()
		if a.EgressAllows("http://192.168.29.60:8000/v1") {
			t.Fatal("a withdrawn authored entry is still in force; the snapshot is not being re-read")
		}
		if !a.EgressAllows("https://api.openai.com/v1") {
			t.Fatal("the environment floor did not survive the withdrawal")
		}
		if a.EgressPrivateNetworkAllowed() {
			t.Fatal("the private network stayed open after the private entry was withdrawn")
		}
	})
}

// TestEgressGate_RowsAloneTurnTheRestrictionOn pins the consequence an operator
// must be told about: with no environment floor, the FIRST authored row turns
// the host restriction on for every provider credential.
func TestEgressGate_RowsAloneTurnTheRestrictionOn(t *testing.T) {
	src := &fakeEgressSource{}
	a := newTestAccount(t, &fakeDB{}, &fakeVault{})
	a.SetEgressSource(src)

	if !a.EgressAllows("https://anything.example.com/v1") {
		t.Fatal("with no entry anywhere the host check must be inert")
	}
	src.set("192.168.29.60:8000")
	if !a.EgressAllowlistConfigured() {
		t.Fatal("an authored row did not arm the allowlist")
	}
	if a.EgressAllows("https://anything.example.com/v1") {
		t.Fatal("the allowlist is armed but an unlisted host was still admitted")
	}
}

// TestEgressGate_MalformedRowDoesNotDropTheFloor covers the defensive path.
// elitea-main validates on write and internal/policy rejects a bad row by name,
// so this should be unreachable — but a database restore has no such gate, and
// one bad authored entry must not take the operator's environment floor with it.
func TestEgressGate_MalformedRowDoesNotDropTheFloor(t *testing.T) {
	src := &fakeEgressSource{}
	a := newEgressAccount(t, &fakeDB{}, &fakeVault{}, "api.openai.com")
	a.SetEgressSource(src)
	src.set("https://malformed/path", "10.0.0.0/8")

	if !a.EgressAllows("https://api.openai.com/v1") {
		t.Fatal("a malformed authored entry dropped the environment floor")
	}
	if !a.EgressAllows("http://10.1.2.3:8000/v1") {
		t.Fatal("a malformed authored entry dropped a valid sibling entry")
	}
	rep := a.EgressReport()
	if len(rep.Dropped) != 1 || rep.Dropped[0] != "https://malformed/path" {
		t.Fatalf("EgressReport().Dropped = %v, want the one unparsable entry named", rep.Dropped)
	}
}

// TestEgressReport_TagsTheSources backs GET /governance/status. "The host I
// added is not working" has two different answers depending on whether the
// entry reached the gateway at all, and a merged list alone cannot tell them
// apart.
func TestEgressReport_TagsTheSources(t *testing.T) {
	src := &fakeEgressSource{}
	a := newEgressAccount(t, &fakeDB{}, &fakeVault{}, "api.openai.com")
	a.SetEgressSource(src)
	src.set("192.168.29.60:8000")

	rep := a.EgressReport()
	if !rep.Configured {
		t.Fatal("report says the allowlist is not configured")
	}
	if len(rep.Env) != 1 || rep.Env[0] != "api.openai.com" {
		t.Fatalf("report Env = %v, want the environment floor alone", rep.Env)
	}
	if len(rep.DB) != 1 || rep.DB[0] != "192.168.29.60:8000" {
		t.Fatalf("report DB = %v, want the authored entry alone", rep.DB)
	}
	if len(rep.Effective) != 2 {
		t.Fatalf("report Effective = %v, want both entries", rep.Effective)
	}
	if !rep.PrivateNetwork {
		t.Fatal("report says the private network is closed although an entry names a private host")
	}

	empty := newTestAccount(t, &fakeDB{}, &fakeVault{}).EgressReport()
	if empty.Configured || empty.PrivateNetwork {
		t.Fatal("an unconfigured gateway reported an armed allowlist")
	}
	// Never nil: the status route encodes this to JSON, and a nil slice there
	// renders as `null`, which a reader has to special-case.
	if empty.Env == nil || empty.DB == nil || empty.Effective == nil || empty.Dropped == nil {
		t.Fatal("EgressReport returned a nil slice; the status surface would render null")
	}
	var nilAccount *EliteaAccount
	if nilAccount.EgressReport().Configured || nilAccount.EgressPrivateNetworkAllowed() {
		t.Fatal("a nil account must be inert, not permissive")
	}
}

// TestNew_RejectsMalformedEgressAllowlist proves the parse error reaches the
// composition root, where it becomes a FATAL rather than a silently narrower
// policy than the operator wrote.
func TestNew_RejectsMalformedEgressAllowlist(t *testing.T) {
	_, err := New(Config{DB: &fakeDB{}, Vault: &fakeVault{}, EgressAllowlist: []string{"https://nope/path"}})
	if err == nil {
		t.Fatal("New must reject a malformed GATEWAY_EGRESS_ALLOWLIST entry")
	}
}

// TestEgressAllowlistConfigured_Reported backs the startup log line that tells
// an operator which of the two policy modes is armed.
func TestEgressAllowlistConfigured_Reported(t *testing.T) {
	if newTestAccount(t, &fakeDB{}, &fakeVault{}).EgressAllowlistConfigured() {
		t.Fatal("no allowlist configured, but the account reports one is armed")
	}
	if !newEgressAccount(t, &fakeDB{}, &fakeVault{}, "h.example").EgressAllowlistConfigured() {
		t.Fatal("an allowlist is configured, but the account reports it is not")
	}
}

// TestEgressGate_ConcurrentReadsAndRefreshes runs the merge under the race
// detector.
//
// The gate is read on the credential path of every request, its source is bound
// at startup, and the authored set behind that source moves on the governance
// poll. Those are three different goroutines, so both the cached merge and the
// source pointer have to be published safely. A plain field assignment for
// either one passes every sequential test in this file and fails here.
func TestEgressGate_ConcurrentReadsAndRefreshes(t *testing.T) {
	src := &fakeEgressSource{}
	a := newEgressAccount(t, &fakeDB{}, &fakeVault{}, "api.openai.com")

	// Two groups, not one: the readers stop when the WRITERS are done, so a
	// single group would wait on goroutines that are waiting on it.
	var readers, writers sync.WaitGroup
	stop := make(chan struct{})

	// Readers: the credential path.
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// The floor is on every merge, so this answer never changes.
				// A torn read shows up as a failure rather than as flakiness.
				if !a.EgressAllows("https://api.openai.com/v1") {
					t.Error("the environment floor was lost during a concurrent refresh")
					return
				}
				_ = a.EgressPrivateNetworkAllowed()
				_ = a.EgressReport()
			}
		}()
	}

	// One writer binds the source late, as main() does.
	writers.Add(1)
	go func() {
		defer writers.Done()
		a.SetEgressSource(src)
	}()

	// One writer moves the authored set, as the governance poll does.
	writers.Add(1)
	go func() {
		defer writers.Done()
		for i := range 200 {
			if i%2 == 0 {
				src.set("192.168.29.60:8000")
			} else {
				src.set("10.0.0.0/8", "b.example.com")
			}
		}
	}()

	writers.Wait()
	close(stop)
	readers.Wait()
}
