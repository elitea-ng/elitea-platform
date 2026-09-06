package main

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/config"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/governance"
	natsinfra "github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/infra/nats"
	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/server"
)

// TestMainWiring is the "wiring gate": it parses main.go and asserts that each
// must-call lifecycle method is actually invoked somewhere in this package's
// source. Rounds 1-3 of review each re-found a "built but not wired" bug — a
// lifecycle method that existed and unit-tested green but was never called from
// the composition root (GovernanceStore never constructed → image endpoints
// never gated → Drain never called on shutdown). A unit test of the method in
// isolation cannot catch that; this can. If someone deletes one of these calls,
// this test fails with a message naming the missing wiring.
//
// It is a source-level assertion (not a runtime callgraph) so it needs no live
// NATS/DB and runs in the existing `test` CI job in milliseconds.
func TestMainWiring(t *testing.T) {
	// Each entry: the call as it appears in source, and why it must be wired.
	required := []struct {
		call string
		why  string
	}{
		{"llmproxy.WithBudgetGate(", "the budget gate is never attached to the handler — every /llm request would be admitted unconditionally (enforcement silently off)"},
		{"llmproxy.WithModelResolver(", "the model resolver is never attached to the handler — /llm/v1/models advertises nothing AND the inference path stops mapping the advertised model id onto the provider's own model name, so a caller that picks a model from the list sends the provider a name it does not know (issue #317)"},
		{"account.New(", "the vault-backed Account is never constructed — the gateway runs the zero-provider bootstrap account and cannot resolve ANY provider credential (BFF.6)"},
		{"account.NewFernetVault(", "the Fernet vault is never constructed — {{secret.NAME}} credential references cannot be resolved (BFF.6)"},
		{"llmproxy.WithLoopBreakerParams(", "the per-(project_id, model) amplification backstop (spec §2.6 guard #2's implementation) is never armed — unbounded request amplification for one tuple would run unchecked in production"},
		{"logLoopBreakerMode(", "the backstop's mode is never logged at startup — a disarmed or badly-tuned guard would be invisible to operators, which is how it shipped as a de-facto 5 req/s rate limiter (issue #12)"},
		{"hopmarker.New(", "the hop marker is never built, so BOTH halves of hop detection are unarmed — the outbound stamp and the inbound recognition read the same value from this one call, and nothing else constructs it (issue #164)"},
		{"llmproxy.WithHopMarker(", "the inbound half of hop-marker detection is never attached to the handler — the gateway would stamp every outbound provider request and then admit its own request back, so a circular route runs until the amplification backstop happens to fire, which it cannot do at a rate below its threshold (issue #164)"},
		{"logHopMarkerMode(", "hop detection's mode is never logged at startup — an install with no GATEWAY_HOP_SECRET is inert, and that is precisely how a previous attempt shipped a guard that defaulted to \"\" and detected nothing anywhere (issue #164)"},
		{"llmproxy.WithAlertEventPublisher(", "budget.soft_alert is never published to gateway.events.* — the 80% alert would be invisible to subscribers (spec §8.3)"},
		{"llmproxy.WithStreamGrace(", "the stream-disconnect grace period is never configured — a client that disconnects mid-stream is billed nothing and the hard budget is bypassable (issue #9)"},
		{"llmproxy.WithStreamDrainLimit(", "abandoned-stream drains are unbounded — a disconnect storm holds unbounded goroutines and provider sockets (issue #9)"},
		{"startupIdentityCheck(", "the identity-secret startup guard is never invoked — the gateway would boot with identity verification disabled while the vault-backed Account resolves per-project credentials from an unauthenticated X-Elitea-Project-Id (issue #11)"},
		{"shutdownSequence(", "the shutdown sequence is never invoked — stream grace, HTTP drain, billing drain and NATS close would not run in the one order that loses no spend (issue #9)"},
		{"llmproxy.WithOpsEventPublisher(", "budget.unbilled_stream is never published — a stream the gateway could not bill would be invisible to operators (issue #9)"},
		{"llmproxy.WithRealtimeDialer(", "the realtime route has no provider side — /llm/v1/realtime answers 501 for every caller, so pylon-indexer keeps the second, unmetered LiteLLM data plane this route exists to delete"},
		{"llmproxy.WithRealtimeBudgetRecheck(", "a realtime session is gated ONCE and then runs unbounded — bifrost reports a turn start only for events the only known caller never sends, so the periodic re-check is the ONLY thing that re-asks the budget for a live socket"},
		{"llmproxy.WithRealtimeSessionLimit(", "concurrent realtime sessions are unbounded — a hijacked connection has its deadlines cleared, so no server timeout can ever reap one"},
		{"llmproxy.WithRealtimeOrigins(", "the accept-side Origin allowlist is never configured — a WebSocket handshake is not subject to CORS, so this is the only place the operator's browser-origin policy is applied"},
		{"logRealtimeMode(", "the realtime Origin policy is never stated at startup — a widened allowlist would be invisible to operators, which is the failure logLoopBreakerMode exists to prevent for the other guard"},
		{"rt.CloseRealtimeSessions(", "live realtime sessions are never closed on shutdown — http.Server.Shutdown neither closes nor waits for a hijacked connection, so every rolling deploy would leave sessions running until the pod is killed and their last turn unbilled"},
		{"govStore.Start(", "the recovery reconciler is inert until Start binds its context — CheckBudget would silently skip recovery"},
		{"makeReadyzHandler(", "the NATS circuit-breaker /readyz handler is never mounted — a pod with a dead budget-enforcement path stays in the load-balancer rotation"},
		{"budgetEnforcementUnwired(", "the /readyz gate for a NATS that never connected is never computed — a pod that boots during a NATS outage serves /llm unmetered, for the life of the process, while reporting ready (issue #304)"},
		{"startBudgetRecovery(", "budget enforcement never comes back after a NATS outage — server.New dials once, so a pod that boots while NATS is down stays not-ready until an operator restarts it, and they can only do that after NATS returns (issue #315)"},
		{`mux.HandleFunc("/healthz"`, "the liveness /healthz route is never mounted — issue #242's healthz/readyz split silently loses liveness, and the chart's livenessProbe would 404 every pod"},
		{`mux.Handle("/metrics"`, "the operator controls have no route — the budget-enforcement gauge and the model-map refusal counters cannot be read, so the alarm for a gateway that enforces nothing (issue #304) cannot be built (issue #465)"},
		{"llmproxy.WithGovernancePolicy(", "the authored governance definitions are never attached to the handler — every model allowlist, MCP allowlist, rate limit, credential rate policy and CEL routing rule in gateway.governance_config would be read and enforced nowhere, which is the state issue #218 exists to end"},
		{"policyStore.Start(", "the definition store never performs its first load and never refreshes — the gateway would hold the Empty snapshot for the life of the process and enforce no authored definition"},
		{"govStore.SetBudgetDefaults(", "an authored budget row is never used as the fallback ceiling — a project with no gateway.project_budget row stays unlimited, so the budget rows on the governance page gate nothing"},
		{`mux.HandleFunc("/governance/status"`, "the operator cannot ask whether a saved definition is actually loaded — a rejected or inert row would be visible only in the logs of whichever pod happened to read it"},
		{"startEgressAllowlistPlane(", "the authored egress_allowlist rows are never bound to the account's egress gate — every row would load, appear on /governance/status and govern nothing, so an on-premise model endpoint would still need a chart edit and a pod restart (gap G6). The same call starts the watcher that rebuilds the vLLM and Ollama provider workers, whose SSRF-safe dialer LATCHES AllowPrivateNetwork when the worker is created"},
		{"drainForShutdown(", "in-flight billing + persist goroutines must be drained before pool.Close() or spend is dropped / a pool races"},
		{"grace.StopStreamGrace(", "phase 1 of shutdown is missing — the stream grace would extend the pod's termination window (issue #9)"},
		{"srv.ShutdownHTTP(", "graceful drain of in-flight SSE streams (§9.5) — without it, deploys truncate live responses"},
		{"srv.Close(", "the NATS client is never closed — and it MUST close after the billing drain, not inside the HTTP shutdown, or in-flight increments hit a dead connection"},
	}

	// Parse each non-test .go file in this package dir individually. (Avoids
	// parser.ParseDir, deprecated since Go 1.25 — SA1019.)
	fset := token.NewFileSet()
	goFiles, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package dir: %v", err)
	}

	// Collect every selector-call (x.Method(...)) and every bare call (f(...))
	// across the non-test files of package main. A selector call whose first
	// argument is a string literal (e.g. mux.HandleFunc("/healthz", ...)) is
	// ALSO keyed by that literal, so two calls to the same method (mounting
	// two different routes) don't collapse into one indistinguishable key —
	// otherwise deleting the /healthz mount would be invisible as long as
	// /readyz's mux.HandleFunc( call still existed.
	calls := map[string]bool{}
	for _, name := range goFiles {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := ce.Fun.(type) {
			case *ast.SelectorExpr: // x.Method
				if id, ok := fn.X.(*ast.Ident); ok {
					base := id.Name + "." + fn.Sel.Name + "("
					calls[base] = true
					if len(ce.Args) > 0 {
						if lit, ok := ce.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							calls[base+lit.Value] = true
						}
					}
				}
			case *ast.Ident: // bareFunc
				calls[fn.Name+"("] = true
			}
			return true
		})
	}

	for _, r := range required {
		if !calls[r.call] {
			t.Errorf("WIRING GATE: %s is never called from package main — %s.\n"+
				"A lifecycle method that isn't called from the composition root is a "+
				"'built but not wired' bug (this class recurred 3x in review). Wire it in main().",
				r.call, r.why)
		}
	}

	// The gate also holds one decision in place. Issue #465 asked whether the
	// whole expvar surface must be public, and the answer is no: /metrics serves
	// an allowlist (see gatewayMetrics). expvar.Handler() writes every published
	// variable, `cmdline` and `memstats` included, on the same listener that
	// serves /llm.
	forbidden := []struct {
		call string
		why  string
	}{
		{"expvar.Handler(", "expvar.Handler() publishes EVERY variable this process holds, including the process arguments and the memory statistics. /metrics serves a named allowlist instead (issue #465). Add the variable to gatewayMetrics rather than publishing the whole surface"},
	}
	for _, f := range forbidden {
		if calls[f.call] {
			t.Errorf("WIRING GATE: %s is called from package main — %s.", f.call, f.why)
		}
	}
}

// These tests guard the SHUTDOWN WIRING, not the drain logic in isolation. A
// unit test proving Handler.DrainBilling()/GovernanceStore.Drain() work does NOT
// catch a future edit that stops main() from calling them — that
// "built-but-not-wired" gap recurred three times in review. drainForShutdown is
// the extracted, testable seam that main() calls.

type spyBillingDrainer struct {
	order *[]string
}

func (s *spyBillingDrainer) DrainBilling() { *s.order = append(*s.order, "billing") }

type spyGovDrainer struct {
	order *[]string
}

func (s *spyGovDrainer) Drain() { *s.order = append(*s.order, "gov") }

// TestDrainForShutdown_DrainsBothInOrder is the regression guard: if a refactor
// drops either drain call or reverses the order, this fails.
func TestDrainForShutdown_DrainsBothInOrder(t *testing.T) {
	var order []string
	drainForShutdown(&spyBillingDrainer{&order}, &spyGovDrainer{&order})

	if len(order) != 2 {
		t.Fatalf("expected both drains to run, got %v", order)
	}
	if order[0] != "billing" || order[1] != "gov" {
		t.Fatalf("drain order = %v, want [billing gov] — billing MUST drain before gov "+
			"so in-flight UpdateUsage calls finish before the store's persist WaitGroup closes", order)
	}
}

// TestDrainForShutdown_NilGovStore covers the enforcement-disabled path, where
// buildGovernance returns a typed-nil *GovernanceStore. drainForShutdown must
// still drain billing and must NOT panic dereferencing the nil store.
func TestDrainForShutdown_NilGovStore(t *testing.T) {
	var order []string
	var nilStore *governance.GovernanceStore // typed nil, as the disabled path passes
	drainForShutdown(&spyBillingDrainer{&order}, nilStore)

	if len(order) != 1 || order[0] != "billing" {
		t.Fatalf("with a nil gov store, only billing should drain; got %v", order)
	}
}

// --- /healthz liveness contract tests -----------------------------------------
//
// /healthz must always be 200 while the process is alive: no dependency
// calls, no NATS ping, nothing that a NATS blip could fail (issue #242 — the
// chart used to point both liveness and readiness at the NATS-checked
// endpoint, so a blip got the pod restarted instead of just drained from
// Service endpoints).

func TestHealthz_AlwaysReturns200(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)

	livenessHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %s", rec.Body.String())
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q, want %q", body["status"], "ok")
	}
}

// --- /readyz response contract tests --------------------------------------
//
// This is the dependency-checked probe that used to be mounted at /healthz.

type fakePinger struct {
	err error
}

func (f *fakePinger) Ping(_ context.Context) error { return f.err }

// wired is the enforcementUnwired argument for a gateway whose budget
// enforcement IS wired. makeReadyzHandler takes a function and not a bool
// because a background re-dial clears that state while the process runs
// (issue #315).
func wired() bool { return false }

func TestReadyz_PingFailureReturns503(t *testing.T) {
	h := makeReadyzHandler(&fakePinger{err: errors.New("breaker open")}, wired)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %s", rec.Body.String())
	}
	if body.Status != "not_ready" {
		t.Errorf("status = %q, want %q", body.Status, "not_ready")
	}
	if body.Checks["nats"] != "unavailable" {
		t.Errorf("checks[nats] = %q, want %q", body.Checks["nats"], "unavailable")
	}
}

func TestReadyz_PingOKReturns200(t *testing.T) {
	h := makeReadyzHandler(&fakePinger{err: nil}, wired)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %s", rec.Body.String())
	}
	if body.Status != "ready" {
		t.Errorf("status = %q, want %q", body.Status, "ready")
	}
	if body.Checks["nats"] != "ok" {
		t.Errorf("checks[nats] = %q, want %q", body.Checks["nats"], "ok")
	}
}

func TestReadyz_NilPingerReturns200(t *testing.T) {
	h := makeReadyzHandler(nil, wired)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// TestReadyz_TypedNilGovStoreReturns200 is the regression guard for the panic
// that TestReadyz_NilPingerReturns200 could not see. That test passes an
// UNTYPED nil, so the handler's `p != nil` guard short-circuits and Ping is
// never called. The disabled-enforcement path passes a typed nil
// *GovernanceStore instead (GATEWAY_NATS_URL unset), which lives in a NON-nil
// interface — the guard fell through and Ping dereferenced a nil receiver, so
// every /readyz request panicked. Measured against the standalone compose
// stack, where /healthz (the pre-split, NATS-checked route) returned an empty
// reply.
//
// This mirrors TestDrainForShutdown_NilGovStore, which already covers the same
// typed-nil hazard on the drain path.
func TestReadyz_TypedNilGovStoreReturns200(t *testing.T) {
	var nilStore *governance.GovernanceStore // typed nil, as the disabled path passes
	h := makeReadyzHandler(nilStore, wired)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

	h.ServeHTTP(rec, req) // panicked before the fix

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// TestStartupIdentityCheck is the issue #11 regression guard. The pre-#11 guard
// was `cfg.NATSURL != "" && cfg.IdentitySecret == ""` — it covered ONLY the
// budget-enforcement reason the secret is mandatory, and was blind to the
// credential-backed Account wired on `pool != nil`. The
// credentialAccount-without-NATS row below is that exact hole: it is the
// NATS-less deployment (compose/dev/CI, GATEWAY_NATS_URL unset, DATABASE_URL
// set) in which an unauthenticated caller setting X-Elitea-Project-Id selects
// any tenant's decrypted provider credentials.
//
// Mutation-proof this test by narrowing startupIdentityCheck's `credentialAccount`
// arm back to nothing (i.e. deleting the `case credentialAccount:` branch): the
// "credential Account, no NATS" row MUST fail.
func TestStartupIdentityCheck(t *testing.T) {
	cases := []struct {
		name              string
		secret            string
		budgetEnforcement bool
		credentialAccount bool
		wantErr           bool
	}{
		{
			name:              "credential Account, no NATS, no secret — issue #11 credential disclosure",
			secret:            "",
			budgetEnforcement: false,
			credentialAccount: true,
			wantErr:           true,
		},
		{
			name:              "budget enforcement, no Account, no secret — original FIX #9",
			secret:            "",
			budgetEnforcement: true,
			credentialAccount: false,
			wantErr:           true,
		},
		{
			name:              "both consumers, no secret",
			secret:            "",
			budgetEnforcement: true,
			credentialAccount: true,
			wantErr:           true,
		},
		{
			name:              "no consumer wired, no secret — nothing depends on identity",
			secret:            "",
			budgetEnforcement: false,
			credentialAccount: false,
			wantErr:           false,
		},
		{
			name:              "secret configured with both consumers — verification is on",
			secret:            "not-a-real-secret",
			budgetEnforcement: true,
			credentialAccount: true,
			wantErr:           false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := startupIdentityCheck(tc.secret, tc.budgetEnforcement, tc.credentialAccount)
			if tc.wantErr && err == nil {
				t.Fatalf("startupIdentityCheck(secret=%q, budget=%v, account=%v) = nil, want a FATAL error: "+
					"the gateway would accept traffic with identity verification disabled while a consumer "+
					"of that identity is wired", tc.secret, tc.budgetEnforcement, tc.credentialAccount)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("startupIdentityCheck(secret=%q, budget=%v, account=%v) = %v, want nil",
					tc.secret, tc.budgetEnforcement, tc.credentialAccount, err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "GATEWAY_IDENTITY_SECRET") {
				t.Fatalf("error must name the env var an operator has to set; got %q", err)
			}
		})
	}
}

// --- issue #304: a gateway that boots without NATS must not report ready ----
//
// What this catches, and why the tests above could not.
//
// TestReadyz_TypedNilGovStoreReturns200 asserts that a nil govStore reports
// ready. That is correct for the posture it was written for (GATEWAY_NATS_URL
// unset — enforcement deliberately off) and wrong for the one below
// (GATEWAY_NATS_URL SET, the connect failed): same nil govStore, opposite
// correct answer. Nothing distinguished the two, so a pod that booted during a
// NATS outage stayed in rotation serving /llm with no budget gate and no
// billing — for the life of the process, because server.New dials once and
// nothing re-wires enforcement afterwards.
//
// This drives the REAL startup path rather than hand-setting the flag: it
// builds a Server through server.New with a connector that fails exactly the
// way an unreachable NATS fails, then feeds the resulting (nil) client into
// the same decision main() makes. Set the connector to succeed, or unset the
// URL, and the pod is ready again — so the test discriminates the outage from
// the deliberate-dev posture rather than asserting a constant.
func TestReadyz_NATSConfiguredButUnreachableAtStartupIsNotReady(t *testing.T) {
	cases := []struct {
		name          string
		natsURL       string
		connectErr    error
		wantUnwired   bool
		wantStatus    int
		wantBodyState string
	}{
		{
			name:          "configured NATS unreachable at boot",
			natsURL:       "nats://nats.invalid:4222",
			connectErr:    errors.New("dial tcp: connection refused"),
			wantUnwired:   true,
			wantStatus:    http.StatusServiceUnavailable,
			wantBodyState: "not_ready",
		},
		{
			// The dev/CI posture (issue #242's nil-govStore case) must be
			// untouched: no NATS configured means enforcement is off on
			// purpose, and the pod is legitimately ready.
			name:          "NATS deliberately not configured",
			natsURL:       "",
			connectErr:    nil,
			wantUnwired:   false,
			wantStatus:    http.StatusOK,
			wantBodyState: "ready",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{
				HTTPAddr:            "127.0.0.1:0",
				InitialPoolSize:     1,
				ProviderConcurrency: 1,
				NATSURL:             tc.natsURL,
			}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			srv, err := server.New(
				context.Background(), cfg, logger, new(slog.LevelVar), nil, http.NewServeMux(),
				server.WithNATSConnector(func(context.Context, natsinfra.Config) (server.NATSClient, error) {
					return nil, tc.connectErr
				}),
			)
			if err != nil {
				t.Fatalf("server.New: %v", err)
			}
			t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

			// main() builds governance only when srv.NATS() != nil, so an
			// unreachable NATS leaves a nil *GovernanceStore here — the exact
			// typed nil the composition root passes on.
			if srv.NATS() != nil {
				t.Fatalf("srv.NATS() = non-nil, want nil for this fixture")
			}
			var govStore *governance.GovernanceStore

			// The composition root reads the same predicate through the
			// atomic holder, so the fixture builds one here too.
			plane := &enforcementPlane{cfg: cfg}
			plane.install(govStore)
			if plane.unwired() != tc.wantUnwired {
				t.Fatalf("enforcementPlane.unwired = %v, want %v", plane.unwired(), tc.wantUnwired)
			}
			if unwired := budgetEnforcementUnwired(cfg, govStore); unwired != tc.wantUnwired {
				t.Fatalf("budgetEnforcementUnwired = %v, want %v", unwired, tc.wantUnwired)
			}

			rec := httptest.NewRecorder()
			makeReadyzHandler(plane, plane.unwired).ServeHTTP(
				rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			if rec.Code != tc.wantStatus {
				t.Fatalf("/readyz status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			var body struct {
				Status string            `json:"status"`
				Checks map[string]string `json:"checks"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not valid JSON: %s", rec.Body.String())
			}
			if body.Status != tc.wantBodyState {
				t.Errorf("status = %q, want %q", body.Status, tc.wantBodyState)
			}
			// The reason must be legible to whoever is looking at a drained
			// pod: "nats: unavailable" would send them to the breaker, which
			// is a different failure with a different remedy.
			if tc.wantUnwired && body.Checks["budget_enforcement"] != "unwired" {
				t.Errorf("checks[budget_enforcement] = %q, want %q",
					body.Checks["budget_enforcement"], "unwired")
			}
		})
	}
}
