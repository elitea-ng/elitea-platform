package main

// budgetcheck_live.go — the live plumbing for `cutover-ctl budget-check`
// (gate BFF.9e, spec §8.5/§8.8 + the §2.6 circular-guard assertion).
//
// The gate calls the gateway DIRECTLY, through the same GatewayService client
// elitea-main uses (llmproxy.GatewayClient: gRPC over mutual TLS with
// elitea-main's client certificate), signing the fixture identity v1. The
// signing secret comes from --identity-secret or $GATEWAY_IDENTITY_SECRET; an
// empty secret is valid when the gateway runs without one (the identity is
// then trusted off the mTLS hop alone).
//
// Four live probes populate budgetCheckResult; checkBudgetResult stays the
// single evaluator (unit-tested against fixtures):
//
//  1. hard-block  — one chat request as the over-budget project → 402 with
//     type=budget_exceeded / code=insufficient_quota.
//  2. under-budget — one chat request as the control project → 200.
//  3. soft-alert  — subscribe gateway.events.project.<id>.events, send the
//     tipping request as the ~79% project, measure seconds until the
//     budget.soft_alert event arrives (must be <= --alert-latency-s).
//  4. circular guard — burst the over-budget tuple: the breaker admission
//     check runs BEFORE the budget gate, so rapid requests must flip from
//     402 to 429 rate_limit_exceeded within the burst (spec §2.6 guard #2).
//     NOTE: this opens the tuple's circuit for 30 s on the target gateway —
//     use a disposable test project.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy"
	"github.com/nats-io/nats.go"
)

// budgetProject is one seeded fixture project.
type budgetProject struct {
	ProjectID string `json:"project_id"`
	UserID    string `json:"user_id"`
	TenantID  string `json:"tenant_id"`
	Model     string `json:"model"` // defaults to openai/gpt-4o
}

func (p budgetProject) model() string {
	if p.Model == "" {
		return "openai/gpt-4o"
	}
	return p.Model
}

// budgetFixture is the --projects-file shape the operator seeds (BFF.9e):
// over_budget at/above its hard limit; soft_alert at ~79% of its limit so one
// request tips it over 80%; under_budget far below its limit.
type budgetFixture struct {
	OverBudget  budgetProject `json:"over_budget"`
	SoftAlert   budgetProject `json:"soft_alert"`
	UnderBudget budgetProject `json:"under_budget"`
}

// loadBudgetFixture reads and validates the fixture file.
func loadBudgetFixture(path string) (budgetFixture, error) {
	var f budgetFixture
	data, err := os.ReadFile(path)
	if err != nil {
		return f, fmt.Errorf("budget-check: cannot read projects file %q: %w", path, err)
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return f, fmt.Errorf("budget-check: malformed projects file %q: %w", path, err)
	}
	for name, p := range map[string]budgetProject{
		"over_budget": f.OverBudget, "soft_alert": f.SoftAlert, "under_budget": f.UnderBudget,
	} {
		if p.ProjectID == "" {
			return f, fmt.Errorf("budget-check: projects file %q: %s.project_id is required", path, name)
		}
	}
	return f, nil
}

// chatBody builds a minimal non-streaming chat request for model. max_tokens
// bounds generation so a long-form (e.g. reasoning) model cannot stall the
// probe past the HTTP client timeout; the exact token count is irrelevant to
// every gate this probe feeds.
func chatBody(model string) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 64,
		"messages":   []map[string]string{{"role": "user", "content": "budget-check probe"}},
	})
	return b
}

// probeTimeout bounds one probe, as the HTTP client timeout did before.
const probeTimeout = 30 * time.Second

// probeResponse is the decoded outcome of one gateway probe.
type probeResponse struct {
	Status  int
	ErrType string
	ErrCode string
}

// sendChatProbe sends one signed chat request and decodes the OpenAI error
// envelope (if any).
func sendChatProbe(gateway llmproxy.Gateway, p budgetProject) (probeResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	resp, err := gateway.ChatCompletions(ctx, &llmproxy.Request{
		Header:   http.Header{"Content-Type": {"application/json"}},
		Body:     bytes.NewReader(chatBody(p.model())),
		Identity: llmproxy.Identity{ProjectID: p.ProjectID, UserID: p.UserID, TenantID: p.TenantID},
	})
	if err != nil {
		return probeResponse{}, fmt.Errorf("budget-check: gateway request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	out := probeResponse{Status: resp.StatusCode}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var envelope struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		out.ErrType = envelope.Error.Type
		out.ErrCode = envelope.Error.Code
	}
	return out, nil
}

// probeCircularGuard bursts requests at the same (project, model) tuple and
// reports whether the gateway flipped to 429 rate_limit_exceeded within the
// burst (spec §2.6: >= 5 requests within 1 s must open the circuit). The
// burst sends up to 2× the spec threshold to absorb request latency jitter.
func probeCircularGuard(gateway llmproxy.Gateway, p budgetProject) (bool, error) {
	const burst = 10
	for i := 0; i < burst; i++ {
		resp, err := sendChatProbe(gateway, p)
		if err != nil {
			return false, err
		}
		if resp.Status == http.StatusTooManyRequests && resp.ErrCode == "rate_limit_exceeded" {
			return true, nil
		}
	}
	return false, nil
}

// alertWaiter abstracts the NATS soft-alert observation so the decision logic
// is unit-testable without a broker.
type alertWaiter interface {
	// WaitForAlert blocks until a budget.soft_alert event for projectID
	// arrives or timeout elapses. It returns the instant the alert was
	// observed; the caller derives the latency from its own tip instant.
	//
	// ready is closed exactly once, as soon as the subscription is live (or
	// before returning if it could never be established). The caller MUST NOT
	// send the tipping request until ready is closed: an alert published while
	// the subscription is still being set up is lost, which would fail the gate
	// for a healthy gateway.
	WaitForAlert(projectID string, timeout time.Duration, ready chan<- struct{}) (fired bool, at time.Time, err error)
}

// natsAlertWaiter subscribes to the project's gateway.events subject.
type natsAlertWaiter struct {
	conn *nats.Conn
}

// eventSubject mirrors elitea-main natsbus subjectFor("project:<id>:events")
// and the gateway's eventSubjectForProject.
func eventSubject(projectID string) string {
	return "gateway.events.project." + projectID + ".events"
}

func (w *natsAlertWaiter) WaitForAlert(projectID string, timeout time.Duration, ready chan<- struct{}) (bool, time.Time, error) {
	// The caller blocks on ready before tipping, so every return path must
	// signal it — including the setup failures below, which would otherwise
	// hang the gate.
	var readyOnce sync.Once
	signalReady := func() { readyOnce.Do(func() { close(ready) }) }
	defer signalReady()

	ch := make(chan *nats.Msg, 16)
	sub, err := w.conn.ChanSubscribe(eventSubject(projectID), ch)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("budget-check: NATS subscribe: %w", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	// ChanSubscribe only queues the SUB protocol message locally; Flush
	// round-trips to the server, so once it returns the interest is registered
	// broker-side and no alert published from here on can be missed.
	if err := w.conn.Flush(); err != nil {
		return false, time.Time{}, fmt.Errorf("budget-check: NATS flush after subscribe: %w", err)
	}
	signalReady()

	// The deadline starts here, not at call time: the caller tips immediately
	// after readiness, so a slow subscribe must not eat the alert budget.
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case <-deadline.C:
			return false, time.Time{}, nil
		case msg := <-ch:
			var evt struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(msg.Data, &evt) == nil && evt.Type == "budget.soft_alert" {
				return true, time.Now(), nil
			}
		}
	}
}

// liveBudgetCheckConfig carries everything the live gate needs; the gateway
// client and waiter are injectable for tests.
type liveBudgetCheckConfig struct {
	gateway       llmproxy.Gateway
	fixture       budgetFixture
	alertLatencyS float64
	waiter        alertWaiter
	logf          func(format string, args ...any)
}

// runLiveBudgetCheck executes the four probes and returns the populated
// budgetCheckResult for checkBudgetResult to evaluate.
func runLiveBudgetCheck(cfg liveBudgetCheckConfig) (budgetCheckResult, error) {
	var r budgetCheckResult

	// 1. Hard block.
	cfg.logf("budget-check: probing hard block (project %s) ...", cfg.fixture.OverBudget.ProjectID)
	hb, err := sendChatProbe(cfg.gateway, cfg.fixture.OverBudget)
	if err != nil {
		return r, err
	}
	r.HardBlockStatus = hb.Status
	r.HardBlockType = hb.ErrType
	r.HardBlockCode = hb.ErrCode
	// RouterCalled cannot be observed from outside the gateway; the hermetic
	// preflight gate (TestBFF9E) proves admission-time blocking. Live mode
	// asserts the wire contract only.
	r.RouterCalled = false

	// 2. Under-budget control.
	cfg.logf("budget-check: probing under-budget control (project %s) ...", cfg.fixture.UnderBudget.ProjectID)
	ub, err := sendChatProbe(cfg.gateway, cfg.fixture.UnderBudget)
	if err != nil {
		return r, err
	}
	r.UnderBudgetStatus = ub.Status

	// 3. Soft alert: subscribe first, then tip the ~79% project over 80%.
	// Readiness is explicit — the waiter closes readyCh once its subscription
	// is registered broker-side. Tipping before that could publish the alert
	// into a subject nobody is listening on yet and fail a healthy gateway.
	cfg.logf("budget-check: tipping project %s over the 80%% threshold ...", cfg.fixture.SoftAlert.ProjectID)
	type waitOut struct {
		fired bool
		at    time.Time
		err   error
	}
	waitCh := make(chan waitOut, 1)
	readyCh := make(chan struct{})
	go func() {
		fired, at, werr := cfg.waiter.WaitForAlert(
			cfg.fixture.SoftAlert.ProjectID,
			time.Duration(cfg.alertLatencyS*float64(time.Second)),
			readyCh)
		waitCh <- waitOut{fired, at, werr}
	}()
	<-readyCh
	tipAt := time.Now()
	if _, err := sendChatProbe(cfg.gateway, cfg.fixture.SoftAlert); err != nil {
		return r, err
	}
	wo := <-waitCh
	if wo.err != nil {
		return r, wo.err
	}
	r.SoftAlertFired = wo.fired
	if wo.fired {
		r.SoftAlertLatencyS = wo.at.Sub(tipAt).Seconds()
	}

	// 4. Circular-routing guard (spec §2.6): burst the over-budget tuple.
	cfg.logf("budget-check: bursting (project %s, %s) to assert the loop breaker ...",
		cfg.fixture.OverBudget.ProjectID, cfg.fixture.OverBudget.model())
	tripped, err := probeCircularGuard(cfg.gateway, cfg.fixture.OverBudget)
	if err != nil {
		return r, err
	}
	r.BreakerTripped = tripped

	return r, nil
}
