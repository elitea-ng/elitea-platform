package llmproxy

// budget_member_alert_test.go — the member soft alert (issue #510).
//
// The defect: #321 gave the gateway a per-member ceiling that refuses a call
// with 402 `member_budget_exceeded`, and the soft-alert path stayed
// project-scoped. A project crossed its threshold and an alert went out; a
// member crossed the same threshold and nothing did, so the refusal was the
// first signal that a cap applied to that member at all.
//
// Both directions are asserted against the same handler and the same fake,
// with only the member's post-increment state differing. Without the "below"
// case, a path that alerted on EVERY billed request would pass the crossing
// test.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
)

// memberAlertGate answers CheckBudget with a DIFFERENT decision before and
// after the increment for the scope under test, which is what a crossing is.
// A fake that returned one fixed decision per scope could not tell "crossed on
// this request" from "was already over", and those two must not alert alike.
//
// The project scope never crosses here. Any alert this fake records is
// therefore the member's, and a project alert leaking into the assertion is
// impossible rather than merely unlikely.
type memberAlertGate struct {
	// memberCrossesOnIncrement decides what the member's POST-increment read
	// reports. false is the negative control: the member is billed and stays
	// below their threshold.
	memberCrossesOnIncrement bool

	mu           sync.Mutex
	memberBilled bool
	cooldowns    []scopeCall
	billed       []scopeCall
}

func (g *memberAlertGate) CheckBudget(_ context.Context, _ int, scope, _ string, _, _ int64) (failmode.Decision, error) {
	if scope != failmode.ScopeUser {
		// The project ceiling: admits, and never near its threshold.
		return failmode.Decision{Verdict: failmode.Allow, State: failmode.StateNATSHealthy}, nil
	}
	g.mu.Lock()
	post := g.memberBilled
	g.mu.Unlock()
	return failmode.Decision{
		Verdict:           failmode.Allow,
		State:             failmode.StateNATSHealthy,
		SoftThresholdNear: post && g.memberCrossesOnIncrement,
	}, nil
}

func (g *memberAlertGate) UpdateUsage(_ context.Context, _ int, scope, scopeID, _ string, _, _, _ int64, _ *failmode.UsageDimensions) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.billed = append(g.billed, scopeCall{scope: scope, scopeID: scopeID})
	if scope == failmode.ScopeUser {
		g.memberBilled = true
	}
	return nil
}

func (g *memberAlertGate) TryAlertCooldown(_ context.Context, scope, scopeID string, _ int64) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cooldowns = append(g.cooldowns, scopeCall{scope: scope, scopeID: scopeID})
	return true, nil
}

func (g *memberAlertGate) claimedCooldowns() []scopeCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]scopeCall(nil), g.cooldowns...)
}

// recordingAlertPublisher captures every published envelope with the subject
// token it was published on. The token is load-bearing: it becomes
// gateway.events.project.<token>.events, so a member scope_id there would
// build a subject nothing subscribes to.
type recordingAlertPublisher struct {
	mu     sync.Mutex
	events []publishedAlert
}

type publishedAlert struct {
	subjectToken string
	envelope     []byte
}

func (p *recordingAlertPublisher) PublishSoftAlertEvent(_ context.Context, projectID string, event []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, publishedAlert{subjectToken: projectID, envelope: append([]byte(nil), event...)})
	return nil
}

func (p *recordingAlertPublisher) published() []publishedAlert {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]publishedAlert(nil), p.events...)
}

// runMemberAlertRequest serves one billed chat request for the given member of
// project 42 and waits for the billing goroutine to finish, so every alert the
// request produces has already been recorded when the assertions run. An empty
// userID sends the request with no member header.
func runMemberAlertRequest(t *testing.T, gate BudgetChecker, pub *recordingAlertPublisher, userID string) {
	t.Helper()
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{
		ID:    "served",
		Usage: &schemas.BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 20},
	}
	h := NewHandler(router, nil, nil,
		WithBudgetGate(gate, &fakeCostEstimator{totalNano: 500_000}),
		WithAlertEventPublisher(pub))

	rec := httptest.NewRecorder()
	h.Chat(rec, memberChatRequest(t, "42", userID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	// DrainBilling waits for the detached billing goroutine, which is where
	// both increments and both soft-alert checks run.
	h.DrainBilling()
}

// decodeSoftAlert unwraps the natsbus envelope and returns the payload.
func decodeSoftAlert(t *testing.T, envelope []byte) softAlertPayload {
	t.Helper()
	var env softAlertEnvelope
	if err := json.Unmarshal(envelope, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Type != softAlertEventType {
		t.Fatalf("envelope.type = %q, want %q", env.Type, softAlertEventType)
	}
	var payload softAlertPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return payload
}

// TestMemberSoftAlert_CrossingEmitsMemberScopedEvent is the defect in #510:
// before the member scope reached trySoftAlert this produced no event at all.
func TestMemberSoftAlert_CrossingEmitsMemberScopedEvent(t *testing.T) {
	gate := &memberAlertGate{memberCrossesOnIncrement: true}
	pub := &recordingAlertPublisher{}
	runMemberAlertRequest(t, gate, pub, "7")

	// The cooldown must be claimed for the MEMBER's own key. A project-scoped
	// claim would let one project crossing suppress every member's warning for
	// the rest of the cooldown window.
	claims := gate.claimedCooldowns()
	memberClaims := 0
	for _, c := range claims {
		if c.scope == failmode.ScopeUser {
			memberClaims++
			if c.scopeID != "42:7" {
				t.Fatalf("member cooldown scope_id = %q, want %q", c.scopeID, "42:7")
			}
		}
	}
	if memberClaims != 1 {
		t.Fatalf("member cooldown claimed %d times, want 1; claims = %+v", memberClaims, claims)
	}

	events := pub.published()
	if len(events) != 1 {
		t.Fatalf("published %d soft alerts, want exactly 1", len(events))
	}
	// The subject token is the PROJECT id even for a member alert: it selects
	// gateway.events.project.42.events, the channel elitea-main relays to the
	// members of project 42.
	if events[0].subjectToken != "42" {
		t.Fatalf("published on subject token %q, want %q — a member scope_id builds a subject nothing subscribes to",
			events[0].subjectToken, "42")
	}

	payload := decodeSoftAlert(t, events[0].envelope)
	if payload.Scope != budgetScopeUser {
		t.Fatalf("payload.scope = %q, want %q — the event must name the member scope",
			payload.Scope, budgetScopeUser)
	}
	if payload.ScopeID != "42:7" {
		t.Fatalf("payload.scope_id = %q, want %q", payload.ScopeID, "42:7")
	}
	if payload.UserID != 7 {
		t.Fatalf("payload.user_id = %d, want 7", payload.UserID)
	}
	if payload.ProjectID != "42" {
		t.Fatalf("payload.project_id = %q, want %q — never the member scope_id", payload.ProjectID, "42")
	}
	if payload.CostJustBilledNano != 500_000 {
		t.Fatalf("payload.cost_just_billed_nano = %d, want 500000", payload.CostJustBilledNano)
	}
}

// TestMemberSoftAlert_BelowThresholdEmitsNothing is the negative control. A
// path that alerted on every billed request would pass the test above.
func TestMemberSoftAlert_BelowThresholdEmitsNothing(t *testing.T) {
	gate := &memberAlertGate{memberCrossesOnIncrement: false}
	pub := &recordingAlertPublisher{}
	runMemberAlertRequest(t, gate, pub, "7")

	for _, c := range gate.claimedCooldowns() {
		if c.scope == failmode.ScopeUser {
			t.Fatalf("member cooldown claimed for %q although the member stayed below their threshold", c.scopeID)
		}
	}
	if events := pub.published(); len(events) != 0 {
		t.Fatalf("published %d soft alerts, want 0 for a member below their threshold", len(events))
	}
}

// TestMemberSoftAlert_NoMemberIDEmitsNothing keeps token-authenticated
// integrations out of the member path. They bill no member scope, so there is
// no member crossing to report and nobody the alert could be addressed to.
func TestMemberSoftAlert_NoMemberIDEmitsNothing(t *testing.T) {
	gate := &memberAlertGate{memberCrossesOnIncrement: true}
	pub := &recordingAlertPublisher{}
	runMemberAlertRequest(t, gate, pub, "")

	for _, c := range gate.claimedCooldowns() {
		if c.scope == failmode.ScopeUser {
			t.Fatal("the member cooldown was claimed for a request that names no member")
		}
	}
	if events := pub.published(); len(events) != 0 {
		t.Fatalf("published %d soft alerts, want 0 for a request with no member", len(events))
	}
}

// TestMemberSoftAlert_PlatformSwitchSuppressesMember pins the #322 switch over
// the member scope too. The switch is platform-wide; a member alert that
// ignored it would keep firing after an operator turned alerts off, which is
// the defect #322 fixed for the project scope.
func TestMemberSoftAlert_PlatformSwitchSuppressesMember(t *testing.T) {
	// The SAME crossing as TestMemberSoftAlert_CrossingEmitsMemberScopedEvent,
	// with only the switch changed.
	off := &alertsOffMemberGate{memberAlertGate: memberAlertGate{memberCrossesOnIncrement: true}}
	offPub := &recordingAlertPublisher{}
	runMemberAlertRequest(t, off, offPub, "7")

	for _, c := range off.claimedCooldowns() {
		if c.scope == failmode.ScopeUser {
			t.Fatal("the member cooldown was claimed although the operator turned soft alerts off")
		}
	}
	if events := offPub.published(); len(events) != 0 {
		t.Fatalf("published %d soft alerts, want 0 while the platform switch is off", len(events))
	}
}

// alertsOffMemberGate is memberAlertGate with the platform switch set on every
// decision, the way the snapshot query sets it from the global config row.
type alertsOffMemberGate struct {
	memberAlertGate
}

func (g *alertsOffMemberGate) CheckBudget(ctx context.Context, pid int, scope, scopeID string, periodStart, reqCost int64) (failmode.Decision, error) {
	dec, err := g.memberAlertGate.CheckBudget(ctx, pid, scope, scopeID, periodStart, reqCost)
	dec.SoftAlertsDisabled = true
	return dec, err
}

// compile-time assertions that both fakes still satisfy their ports.
var (
	_ BudgetChecker       = (*memberAlertGate)(nil)
	_ BudgetChecker       = (*alertsOffMemberGate)(nil)
	_ AlertEventPublisher = (*recordingAlertPublisher)(nil)
)

// TestMemberSoftAlert_MalformedScopeIDOmitsUserID pins what the event says when
// the member key is not "{project}:{member}". UserID stays absent rather than
// reporting member 0, and ScopeID still carries the raw key, so the alert names
// nobody instead of naming the wrong person.
func TestMemberSoftAlert_MalformedScopeIDOmitsUserID(t *testing.T) {
	pub := &recordingAlertPublisher{}
	h := NewHandler(&trackingRouter{}, nil, nil, WithAlertEventPublisher(pub))

	h.publishSoftAlertEvent(context.Background(), h.budget().alerts, 42,
		budgetScopeUser, "not-a-member-key", 500_000, 1_750_000_000)

	events := pub.published()
	if len(events) != 1 {
		t.Fatalf("published %d soft alerts, want 1", len(events))
	}
	payload := decodeSoftAlert(t, events[0].envelope)
	if payload.UserID != 0 {
		t.Fatalf("payload.user_id = %d, want it absent for a malformed member key", payload.UserID)
	}
	if payload.ScopeID != "not-a-member-key" {
		t.Fatalf("payload.scope_id = %q, want the raw key so the alert stays diagnosable", payload.ScopeID)
	}
	if payload.ProjectID != "42" {
		t.Fatalf("payload.project_id = %q, want 42", payload.ProjectID)
	}
}
