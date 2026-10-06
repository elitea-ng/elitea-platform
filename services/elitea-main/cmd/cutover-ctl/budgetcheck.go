package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/EliteaAI/elitea-platform/libs/go/natsconn"
)

// budget-check (spec §8.5 / §8.8, gate BFF.9e):
//
// The migration introduces a project budget enforcement layer (GovernanceStore)
// that replaces the pylon platform's soft limits with hard int64 nano-USD counters
// in NATS JetStream (design §8). Two properties MUST hold before the big-bang
// cutover:
//
//  1. Hard block: a request to an over-budget project receives HTTP 402 with
//     error.type=budget_exceeded and error.code=insufficient_quota. The LLM
//     provider is never called — the gate blocks at admission.
//
//  2. Soft alert: when a project's running spend crosses the soft-alert percentage
//     (default 80%) during a billing increment, an alert is recorded (via the
//     NATS TryAlertCooldown path) within --alert-latency-s seconds.
//
// Because this check normally runs against live infra (a test project with a
// finite budget cap), the subcommand's core enforcement logic is encapsulated in
// checkBudgetResult, a pure function that accepts injectable results (HTTP status
// observed, alert-fired observation, latency). The main entrypoint (cmdBudgetCheck)
// wraps the live HTTP+NATS plumbing around this core; tests bypass the live path
// and invoke checkBudgetResult directly.
//
// Test project requirements (live mode):
//   - A project whose balance meets or exceeds its hard_limit_nano (seed via
//     elitea-admin or gateway_models.llm_budget SQL).
//   - A project starting just below 80% of its limit so a single request tips
//     over the threshold; the alert must appear in the NATS KV cooldown key
//     within --alert-latency-s seconds.
//
// Exit codes:
//   0  all checks pass.
//   1  one or more checks failed (hard-block missing, alert late/missing).
//   2  flag/invocation error.

// budgetCheckResult is the injectable result set that checkBudgetResult evaluates.
// Tests inject synthetic values; the live path populates it from real HTTP responses
// and NATS observations.
type budgetCheckResult struct {
	// HardBlockStatus is the HTTP status code observed for the over-budget request.
	// Must be 402 for the check to pass.
	HardBlockStatus int
	// HardBlockType is the error.type from the 402 response body.
	// Must be "budget_exceeded".
	HardBlockType string
	// HardBlockCode is the error.code from the 402 response body.
	// Must be "insufficient_quota".
	HardBlockCode string
	// RouterCalled reports whether the mock/live LLM provider was called before the
	// 402 was returned. Must be false — the gate should block at admission.
	RouterCalled bool

	// SoftAlertFired reports whether the 80% soft-alert side-effect was observed
	// (i.e. TryAlertCooldown returned fired=true or a NATS delta was published on
	// the alert-cooldown key) after a billing increment that crossed the threshold.
	SoftAlertFired bool
	// SoftAlertLatencyS is the observed wall-clock delay (in seconds) between the
	// 80% crossing and the alert observation. Must be <= alertLatencyS.
	SoftAlertLatencyS float64

	// UnderBudgetStatus is the HTTP status code observed for a request from a
	// project with spend well below its limit. Must be 200.
	UnderBudgetStatus int

	// BreakerTripped reports whether the §2.6 circular-routing guard opened
	// (a burst at one (project, model) tuple flipped to 429
	// rate_limit_exceeded). Must be true — a gateway without the loop breaker
	// must not pass the pre-flight gate.
	BreakerTripped bool
}

// budgetCheckOutcome records the pass/fail of each gate within one evaluation.
type budgetCheckOutcome struct {
	HardBlockPass     bool
	SoftAlertPass     bool
	UnderBudgetPass   bool
	CircularGuardPass bool

	HardBlockReason     string
	SoftAlertReason     string
	UnderBudgetReason   string
	CircularGuardReason string
}

// allPass reports whether every gate held.
func (o budgetCheckOutcome) allPass() bool {
	return o.HardBlockPass && o.SoftAlertPass && o.UnderBudgetPass && o.CircularGuardPass
}

// checkBudgetResult is the pure, injectable core of the budget-check gate.
// It is fully unit-testable: no I/O, no os.Exit.
//
// alertLatencyS is the operator-configured maximum seconds from the 80% crossing
// to alert observation.
func checkBudgetResult(r budgetCheckResult, alertLatencyS float64) budgetCheckOutcome {
	var out budgetCheckOutcome

	// 1. Hard-block gate.
	switch {
	case r.HardBlockStatus != 402:
		out.HardBlockReason = fmt.Sprintf("over-budget request returned HTTP %d, want 402", r.HardBlockStatus)
	case r.HardBlockType != "budget_exceeded":
		out.HardBlockReason = fmt.Sprintf("error.type=%q, want \"budget_exceeded\"", r.HardBlockType)
	case r.HardBlockCode != "insufficient_quota":
		out.HardBlockReason = fmt.Sprintf("error.code=%q, want \"insufficient_quota\"", r.HardBlockCode)
	case r.RouterCalled:
		out.HardBlockReason = "LLM provider was called before the 402 was returned (gate did not block at admission)"
	default:
		out.HardBlockPass = true
	}

	// 2. Soft-alert gate.
	switch {
	case !r.SoftAlertFired:
		out.SoftAlertReason = "soft-alert was not fired after the 80% crossing (TryAlertCooldown/delta not observed)"
	case r.SoftAlertLatencyS > alertLatencyS:
		out.SoftAlertReason = fmt.Sprintf(
			"soft-alert latency %.2f s exceeds --alert-latency-s %.2f s", r.SoftAlertLatencyS, alertLatencyS)
	default:
		out.SoftAlertPass = true
	}

	// 3. Under-budget control case.
	if r.UnderBudgetStatus == 200 {
		out.UnderBudgetPass = true
	} else {
		out.UnderBudgetReason = fmt.Sprintf("under-budget request returned HTTP %d, want 200", r.UnderBudgetStatus)
	}

	// 4. Circular-routing guard (spec §2.6 guard #2).
	if r.BreakerTripped {
		out.CircularGuardPass = true
	} else {
		out.CircularGuardReason = "loop breaker did not open under a same-(project, model) burst (429 rate_limit_exceeded never observed)"
	}

	return out
}

// cmdBudgetCheck is the `cutover-ctl budget-check` entrypoint (gate BFF.9e).
//
// Live mode requires an operator-seeded projects fixture (--projects-file /
// $LLM_BUDGET_PROJECTS_FILE) and a reachable gateway + NATS. Without the
// fixture the gate exits 2 — it must NEVER exit 0 having verified nothing
// (that failure mode is exactly what BFF.9d's faked validator taught us).
//
// Flags:
//
//	--alert-latency-s   Max seconds from 80% crossing to alert (default 10).
//	--gateway-url       Gateway base URL (default http://localhost:8083).
//	--nats-url          NATS URL for the gateway.events.* subscription.
//	--projects-file     JSON fixture: {over_budget, soft_alert, under_budget}
//	                    each {project_id, user_id, tenant_id, model}.
//	--identity-secret   Edge identity HMAC secret (default $GATEWAY_IDENTITY_SECRET).
//
// Exit codes: 0 all gates pass; 1 gate failure; 2 config/transport error.
func cmdBudgetCheck(args []string) {
	fs := flag.NewFlagSet("budget-check", flag.ExitOnError)
	alertLatencyS := fs.Float64("alert-latency-s", 10, "maximum seconds from 80% spend crossing to soft-alert observation")
	gatewayURL := fs.String("gateway-url", "http://localhost:8083", "gateway base URL")
	natsURL := fs.String("nats-url", "nats://localhost:4222", "NATS URL for the gateway.events.* soft-alert subscription")
	projectsFile := fs.String("projects-file", os.Getenv("LLM_BUDGET_PROJECTS_FILE"), "path to the seeded projects fixture (JSON)")
	identitySecret := fs.String("identity-secret", os.Getenv("GATEWAY_IDENTITY_SECRET"), "edge identity HMAC secret (empty = unsigned headers)")
	_ = fs.Parse(args)

	if *projectsFile == "" {
		fmt.Fprintln(os.Stderr, `budget-check: --projects-file (or $LLM_BUDGET_PROJECTS_FILE) is required.

Seed three projects and describe them in a JSON fixture:
  {
    "over_budget":  {"project_id": "9101", "user_id": "u", "tenant_id": "t"},
    "soft_alert":   {"project_id": "9102", "user_id": "u", "tenant_id": "t"},
    "under_budget": {"project_id": "9103", "user_id": "u", "tenant_id": "t"}
  }
over_budget: spend >= hard limit. soft_alert: spend ~79% of limit so one
request tips over 80%. under_budget: far below limit. The over_budget tuple
is also burst to assert the §2.6 loop breaker (its circuit opens for 30 s).

Hermetic equivalent (no live infra):
  GOWORK=off go test ./services/elitea-llm-gateway/internal/preflight/ -run 'BFF9E|CircularRouting' -v`)
		os.Exit(2)
	}

	fixture, err := loadBudgetFixture(*projectsFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(2)
	}

	nc, err := connectBudgetCheckNATS(*natsURL, os.LookupEnv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "budget-check: NATS connect %q: %v\n", *natsURL, err)
		os.Exit(2)
	}
	defer nc.Close()

	result, err := runLiveBudgetCheck(liveBudgetCheckConfig{
		gatewayURL:    *gatewayURL,
		secret:        []byte(*identitySecret),
		fixture:       fixture,
		alertLatencyS: *alertLatencyS,
		client:        &http.Client{Timeout: 30 * time.Second},
		waiter:        &natsAlertWaiter{conn: nc},
		logf: func(format string, a ...any) {
			fmt.Printf(format+"\n", a...)
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(2)
	}

	out := checkBudgetResult(result, *alertLatencyS)
	report := func(name string, pass bool, reason string) {
		if pass {
			fmt.Printf("  ✓ %s\n", name)
		} else {
			fmt.Printf("  ✗ %s — %s\n", name, reason)
		}
	}
	fmt.Println("budget-check results:")
	report("hard block (402 budget_exceeded/insufficient_quota)", out.HardBlockPass, out.HardBlockReason)
	report(fmt.Sprintf("soft alert on gateway.events.* within %.0f s", *alertLatencyS), out.SoftAlertPass, out.SoftAlertReason)
	report("under-budget control (200)", out.UnderBudgetPass, out.UnderBudgetReason)
	report("circular-routing guard (429 on same-tuple burst)", out.CircularGuardPass, out.CircularGuardReason)

	if !out.allPass() {
		fmt.Fprintln(os.Stderr, "\n✗ budget-check: one or more gates failed (spec §8.5/§8.8/§2.6, gate BFF.9e)")
		os.Exit(1)
	}
	fmt.Println("\n✓ budget-check: all gates pass")
}

// budgetCheckNATSPrefix names the client identity budget-check presents on a
// secured NATS: elitea-main's (ELITEA_EVENTS_NATS_TLS_{CA,CERT,KEY}_FILE).
// budget-check only SUBSCRIBES to the project event subjects, which is
// exactly what that identity may do (deploy/helm/nats/values.yaml), so an
// operator runs it with the elitea-main client certificate instead of a new
// identity widening the permission table. With none of the three set it
// connects plaintext, for the compose posture.
const budgetCheckNATSPrefix = "ELITEA_EVENTS"

func connectBudgetCheckNATS(url string, lookup func(string) (string, bool)) (*nats.Conn, error) {
	material, err := natsconn.FromEnv(budgetCheckNATSPrefix, lookup)
	if err != nil {
		return nil, err
	}
	if err := material.CheckURL(url); err != nil {
		return nil, err
	}
	if err := material.Check(); err != nil {
		return nil, err
	}
	opts := []nats.Option{
		nats.Name("cutover-ctl budget-check"),
		nats.Timeout(2 * time.Second),
		// The identity may subscribe only to its own inbox prefix.
		nats.CustomInboxPrefix(natsconn.InboxPrefix(natsconn.IdentityMain)),
	}
	if material.Enabled() {
		opts = append(opts,
			nats.Secure(natsconn.BaseTLSConfig()),
			nats.ClientTLSConfig(material.ClientCertificate, material.RootCAs),
		)
	}
	return nats.Connect(url, opts...)
}
