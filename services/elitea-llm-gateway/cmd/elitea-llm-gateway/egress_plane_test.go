package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/account"
)

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

// countingRefresher records which provider workers were rebuilt.
type countingRefresher struct {
	mu   sync.Mutex
	seen []schemas.ModelProvider
}

func (c *countingRefresher) UpdateProvider(provider schemas.ModelProvider) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, provider)
	return nil
}

func (c *countingRefresher) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.seen)
}

// stubVault satisfies the account's vault seam. The account under test never
// resolves a credential: every assertion here is about the egress gate.
type stubVault struct{}

func (stubVault) Resolve(context.Context, string, string) (string, error) { return "", nil }

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestStartEgressAllowlistPlaneBindsTheSource is the wiring assertion in
// behaviour rather than in source text: after the call, an authored entry
// decides a credential host.
func TestStartEgressAllowlistPlaneBindsTheSource(t *testing.T) {
	acct := newPlaneAccount(t, "api.openai.com")
	src := &fakeEgressSource{}
	src.set("192.168.29.60:8000")

	if acct.EgressAllows("http://192.168.29.60:8000/v1") {
		t.Fatal("the authored entry applied before the plane was started")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startEgressAllowlistPlane(ctx, egressPlane{account: acct, source: src, logger: quietLogger()})

	if !acct.EgressAllows("http://192.168.29.60:8000/v1") {
		t.Fatal("the authored entry did not take effect after the bind")
	}
	if !acct.EgressAllows("https://api.openai.com/v1") {
		t.Fatal("the bind dropped the environment floor")
	}
}

// TestStartEgressAllowlistPlaneIsSafeWithoutAnAccount pins the no-database
// posture: nothing to govern, no goroutine, no panic.
func TestStartEgressAllowlistPlaneIsSafeWithoutAnAccount(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startEgressAllowlistPlane(ctx, egressPlane{logger: quietLogger()})
	startEgressAllowlistPlane(ctx, egressPlane{account: newPlaneAccount(t), logger: quietLogger()})
}

// TestWatchEgressPrivateNetworkRebuildsOnChange is the gap that would otherwise
// ship silently: bifrost latches AllowPrivateNetwork when it CREATES a provider
// worker, so an authored row that unlocks the private network changes nothing
// for a worker that already exists.
//
// Mutation: delete the UpdateProvider loop — this test MUST fail.
func TestWatchEgressPrivateNetworkRebuildsOnChange(t *testing.T) {
	acct := newPlaneAccount(t)
	src := &fakeEgressSource{}
	acct.SetEgressSource(src)
	refresher := &countingRefresher{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchEgressPrivateNetwork(ctx, acct, refresher, time.Millisecond, quietLogger())

	// No change: nothing is rebuilt. UpdateProvider drains and replaces a live
	// worker, so a rebuild every tick would recycle provider connections for
	// nothing.
	time.Sleep(20 * time.Millisecond)
	if got := refresher.count(); got != 0 {
		t.Fatalf("rebuilt %d providers with no change to the decision, want 0", got)
	}

	src.set("192.168.29.0/24")
	waitFor(t, func() bool { return refresher.count() >= len(privateNetworkProviders) })

	refresher.mu.Lock()
	seen := map[schemas.ModelProvider]bool{}
	for _, p := range refresher.seen {
		seen[p] = true
	}
	refresher.mu.Unlock()
	for _, want := range privateNetworkProviders {
		if !seen[want] {
			t.Fatalf("provider %s was not rebuilt; its dialer keeps the old setting until restart", want)
		}
	}
}

// TestWatchEgressPrivateNetworkStopsWithTheContext keeps the goroutine bounded
// by the process lifetime.
func TestWatchEgressPrivateNetworkStopsWithTheContext(t *testing.T) {
	acct := newPlaneAccount(t)
	src := &fakeEgressSource{}
	acct.SetEgressSource(src)
	refresher := &countingRefresher{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		watchEgressPrivateNetwork(ctx, acct, refresher, time.Millisecond, quietLogger())
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the watcher did not stop when its context was cancelled")
	}
}

// TestGovernanceStatusReportsTheMergedEgressList backs the operator's answer to
// "is the host I added actually in force, and where did it come from?".
func TestGovernanceStatusReportsTheMergedEgressList(t *testing.T) {
	acct := newPlaneAccount(t, "api.openai.com")
	src := &fakeEgressSource{}
	src.set("192.168.29.60:8000")
	acct.SetEgressSource(src)

	recorder := httptest.NewRecorder()
	makeGovernanceStatusHandler(nil, nil, "", acct)(
		recorder, httptest.NewRequest(http.MethodGet, "/governance/status", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var body governanceStatusBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Egress == nil {
		t.Fatal("the status body carries no egress report")
	}
	if len(body.Egress.Env) != 1 || body.Egress.Env[0] != "api.openai.com" {
		t.Fatalf("egress.env = %v, want the environment floor", body.Egress.Env)
	}
	if len(body.Egress.DB) != 1 || body.Egress.DB[0] != "192.168.29.60:8000" {
		t.Fatalf("egress.db = %v, want the authored entry", body.Egress.DB)
	}
	if len(body.Egress.Effective) != 2 {
		t.Fatalf("egress.effective = %v, want the union", body.Egress.Effective)
	}
	if !body.Egress.PrivateNetwork {
		t.Fatal("egress.private_network = false although an entry names a private host")
	}

	// No account (no database pool): the field is absent rather than reported
	// as an empty policy that nobody authored.
	recorder = httptest.NewRecorder()
	makeGovernanceStatusHandler(nil, nil, "", nil)(
		recorder, httptest.NewRequest(http.MethodGet, "/governance/status", nil))
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := raw["egress"]; present {
		t.Fatal("a gateway with no account reported an egress policy")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition was not met before the deadline")
}

func newPlaneAccount(t *testing.T, allowlist ...string) *account.EliteaAccount {
	t.Helper()
	a, err := account.New(account.Config{
		DB:              account.NewPoolQuerier(nil),
		Vault:           stubVault{},
		EgressAllowlist: allowlist,
		Logger:          quietLogger(),
	})
	if err != nil {
		t.Fatalf("account.New: %v", err)
	}
	return a
}
