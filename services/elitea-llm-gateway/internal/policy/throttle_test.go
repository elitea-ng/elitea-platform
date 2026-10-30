package policy

// Throttles per agent and per user (ADR-0041 decision 4).

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func throttleRow(name string, scope map[string]any, rpm float64) Row {
	return Row{ID: name, Type: TypeRateLimit, Name: name, Enabled: true, Data: map[string]any{
		"scope":      scope,
		"rate_limit": map[string]any{"requests_per_min": rpm},
	}}
}

func TestCompileThrottleScope(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		row    Row
		reason string // empty: accepted
		class  RateClass
	}{
		{
			name:  "agent throttle with its one project",
			row:   throttleRow("a", map[string]any{"project_ids": []any{7.0}, "agent_ids": []any{3.0, 4.0}}, 5),
			class: RateClassAgent,
		},
		{
			name:  "user throttle across every project",
			row:   throttleRow("u", map[string]any{"user_ids": []any{11.0}}, 5),
			class: RateClassUser,
		},
		{
			name:  "agent and user is an agent throttle",
			row:   throttleRow("au", map[string]any{"project_ids": []any{7.0}, "agent_ids": []any{3.0}, "user_ids": []any{11.0}}, 5),
			class: RateClassAgent,
		},
		{
			name:   "agent ids with no project",
			row:    throttleRow("x", map[string]any{"agent_ids": []any{3.0}}, 5),
			reason: "exactly one entry in scope.project_ids",
		},
		{
			name:   "agent ids with two projects",
			row:    throttleRow("x", map[string]any{"project_ids": []any{7.0, 8.0}, "agent_ids": []any{3.0}}, 5),
			reason: "exactly one entry in scope.project_ids",
		},
		{
			name:   "a fractional id",
			row:    throttleRow("x", map[string]any{"user_ids": []any{1.5}}, 5),
			reason: "not a positive whole-number id",
		},
		{
			name:   "a zero id",
			row:    throttleRow("x", map[string]any{"user_ids": []any{0.0}}, 5),
			reason: "not a positive whole-number id",
		},
		{
			name:   "a text id",
			row:    throttleRow("x", map[string]any{"user_ids": []any{"bob"}}, 5),
			reason: "not a positive whole-number id",
		},
		{
			name:   "not a list",
			row:    throttleRow("x", map[string]any{"user_ids": "11"}, 5),
			reason: "must be a list",
		},
		{
			name: "throttle scope on a budget",
			row: Row{ID: "b", Type: TypeBudget, Name: "b", Enabled: true, Data: map[string]any{
				"scope":  map[string]any{"user_ids": []any{11.0}},
				"budget": map[string]any{"limit_usd": 5.0},
			}},
			reason: "rate_limit entries only",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			snap := Compile([]Row{tc.row}, testNow)
			if tc.reason == "" {
				if len(snap.Rejected) > 0 {
					t.Fatalf("rejected: %+v", snap.Rejected)
				}
				if got := snap.rateLimits[0].Class; got != tc.class {
					t.Fatalf("class = %v, want %v", got, tc.class)
				}
				return
			}
			if len(snap.Rejected) != 1 || !strings.Contains(snap.Rejected[0].Reason, tc.reason) {
				t.Fatalf("want one rejection naming %q, got %+v", tc.reason, snap.Rejected)
			}
		})
	}
}

func TestCompileRefusesAnOverlongThrottleList(t *testing.T) {
	t.Parallel()
	ids := make([]any, MaxThrottleScopeIDs+1)
	for i := range ids {
		ids[i] = float64(i + 1)
	}
	snap := Compile([]Row{throttleRow("x", map[string]any{"user_ids": ids}, 5)}, testNow)
	if len(snap.Rejected) != 1 || !strings.Contains(snap.Rejected[0].Reason, "the limit is") {
		t.Fatalf("an overlong list was not refused: %+v", snap.Rejected)
	}
}

// TestThrottlesAddToTheProjectLimit pins that a throttle never replaces the
// project rate limit: an agent that is throttled is still under the project
// limit too.
func TestThrottlesAddToTheProjectLimit(t *testing.T) {
	t.Parallel()
	snap := Compile([]Row{
		throttleRow("project", map[string]any{"project_ids": []any{7.0}}, 100),
		throttleRow("global", map[string]any{}, 1000),
		throttleRow("agent", map[string]any{"project_ids": []any{7.0}, "agent_ids": []any{3.0}}, 10),
		throttleRow("user", map[string]any{"user_ids": []any{11.0}}, 20),
	}, testNow)

	sub := Subject{ProjectID: 7, UserID: 11, Agent: AgentRef{ProjectID: 7, ApplicationID: 3}}
	defs := snap.RateLimits(sub)
	var names []string
	for _, d := range defs {
		names = append(names, d.Name)
	}
	if got := strings.Join(names, ","); got != "project,agent,user" {
		t.Fatalf("limits = %s, want project,agent,user", got)
	}
	if def, ok := snap.RateLimit(sub); !ok || def.Name != "project" {
		t.Fatalf("RateLimit = %+v %v, want the project row", def, ok)
	}
	if !snap.HasAgentThrottles() {
		t.Fatal("HasAgentThrottles = false with an agent row loaded")
	}

	// Another agent and another user get the project limit only.
	other := Subject{ProjectID: 7, UserID: 12, Agent: AgentRef{ProjectID: 7, ApplicationID: 4}}
	if defs := snap.RateLimits(other); len(defs) != 1 || defs[0].Name != "project" {
		t.Fatalf("limits for another agent and user = %+v, want the project row only", defs)
	}
	// Application 3 of ANOTHER project is another agent.
	foreign := Subject{ProjectID: 7, Agent: AgentRef{ProjectID: 9, ApplicationID: 3}}
	for _, d := range snap.RateLimits(foreign) {
		if d.Class == RateClassAgent {
			t.Fatal("an agent throttle matched application 3 of a different project")
		}
	}
	// The agent's own project decides, not the request's project header.
	elsewhere := Subject{ProjectID: 42, Agent: AgentRef{ProjectID: 7, ApplicationID: 3}}
	found := false
	for _, d := range snap.RateLimits(elsewhere) {
		found = found || d.Name == "agent"
	}
	if !found {
		t.Fatal("the agent throttle did not follow the agent into a run started from another project")
	}
	// An unknown agent and an unknown user match no throttle.
	for _, d := range snap.RateLimits(Subject{ProjectID: 7}) {
		if d.Class != RateClassProject {
			t.Fatalf("a throttle matched a request with no agent and no user: %+v", d)
		}
	}
}

func TestAgentThrottleRefusesOnlyThatAgent(t *testing.T) {
	t.Parallel()
	c := newFakeCounter()
	l := newTestLimiter(c, testNow)
	snap := Compile([]Row{
		throttleRow("agents", map[string]any{"project_ids": []any{7.0}, "agent_ids": []any{3.0, 4.0}}, 2),
	}, testNow)

	agentA := Subject{ProjectID: 7, Agent: AgentRef{ProjectID: 7, ApplicationID: 3}}
	agentB := Subject{ProjectID: 7, Agent: AgentRef{ProjectID: 7, ApplicationID: 4}}
	admit := func(sub Subject) RateDecision {
		defs := snap.RateLimits(sub)
		if len(defs) != 1 {
			t.Fatalf("limits = %+v, want the agent throttle", defs)
		}
		return l.Admit(context.Background(), defs[0], sub, true)
	}
	for i := 0; i < 2; i++ {
		if dec := admit(agentA); !dec.Allowed {
			t.Fatalf("agent A call %d was refused under a ceiling of 2", i+1)
		}
	}
	dec := admit(agentA)
	if dec.Allowed || dec.Class != RateClassAgent || dec.RetryAfter <= 0 {
		t.Fatalf("agent A's third call = %+v, want a refusal with a Retry-After", dec)
	}
	// Each listed agent has its own counter.
	if dec := admit(agentB); !dec.Allowed {
		t.Fatal("agent B was refused by agent A's counter")
	}
}

func TestUserThrottleRefusesOnlyThatUser(t *testing.T) {
	t.Parallel()
	c := newFakeCounter()
	l := newTestLimiter(c, testNow)
	snap := Compile([]Row{throttleRow("users", map[string]any{"user_ids": []any{11.0}}, 1)}, testNow)

	alice := Subject{ProjectID: 7, UserID: 11}
	if dec := l.Admit(context.Background(), snap.RateLimits(alice)[0], alice, true); !dec.Allowed {
		t.Fatal("the first call was refused under a ceiling of 1")
	}
	// The same user in another project shares the counter: it is a limit on
	// the person.
	aliceElsewhere := Subject{ProjectID: 8, UserID: 11}
	dec := l.Admit(context.Background(), snap.RateLimits(aliceElsewhere)[0], aliceElsewhere, true)
	if dec.Allowed || dec.Class != RateClassUser {
		t.Fatalf("the user's second call (another project) = %+v, want a refusal", dec)
	}
	if defs := snap.RateLimits(Subject{ProjectID: 7, UserID: 12}); len(defs) != 0 {
		t.Fatalf("a user the row does not name got %+v", defs)
	}
}

func TestThrottleFailsOpenOnABrokenCounter(t *testing.T) {
	t.Parallel()
	c := newFakeCounter()
	c.failIncr = errors.New("nats down")
	l := newTestLimiter(c, testNow)
	snap := Compile([]Row{throttleRow("users", map[string]any{"user_ids": []any{11.0}}, 1)}, testNow)
	sub := Subject{ProjectID: 7, UserID: 11}
	for i := 0; i < 3; i++ {
		if dec := l.Admit(context.Background(), snap.RateLimits(sub)[0], sub, true); !dec.Allowed {
			t.Fatalf("call %d was refused while the counter was broken; a throttle fails open", i+1)
		}
	}
	if l.Degraded() != 3 {
		t.Fatalf("Degraded = %d, want 3", l.Degraded())
	}
}

func TestBucketKeysKeepTheClassesApart(t *testing.T) {
	t.Parallel()
	sub := Subject{ProjectID: 7, UserID: 11, Agent: AgentRef{ProjectID: 7, ApplicationID: 3}}
	keys := map[string]bool{}
	for _, def := range []RateLimitDef{
		{Name: "r", Class: RateClassProject},
		{Name: "r", Class: RateClassAgent, Scope: Scope{AgentIDs: []int64{3}}},
		{Name: "r", Class: RateClassAgent, Scope: Scope{AgentIDs: []int64{3}, UserIDs: []int64{11}}},
		{Name: "r", Class: RateClassUser, Scope: Scope{UserIDs: []int64{11}}},
	} {
		k := BucketKey(def, sub)
		if keys[k] {
			t.Fatalf("two classes share the bucket key %q", k)
		}
		keys[k] = true
		if strings.ContainsAny(k, ". *>") {
			t.Fatalf("bucket key %q is not one NATS token", k)
		}
	}
}

// fakeAgentLookup is an AgentLookup over a map.
type fakeAgentLookup struct {
	mu     sync.Mutex
	agents map[string]AgentRef
	err    error
	calls  int
}

func (f *fakeAgentLookup) LookupAgent(_ context.Context, id string) (AgentRef, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return AgentRef{}, false, f.err
	}
	ref, ok := f.agents[id]
	return ref, ok, nil
}

func TestAgentResolverCachesAndFailsOpen(t *testing.T) {
	t.Parallel()
	now := testNow
	lookup := &fakeAgentLookup{agents: map[string]AgentRef{"run-1": {ProjectID: 7, ApplicationID: 3}}}
	l := NewLimiter(LimiterConfig{
		Counter: newFakeCounter(), Subject: testSubject, Logger: quietLogger(),
		Now: func() time.Time { return now }, Agents: lookup,
	})

	for i := 0; i < 3; i++ {
		if got := l.Agent(context.Background(), "run-1"); got != (AgentRef{ProjectID: 7, ApplicationID: 3}) {
			t.Fatalf("Agent = %+v", got)
		}
	}
	if lookup.calls != 1 {
		t.Fatalf("lookups = %d, want 1 (cached)", lookup.calls)
	}
	if got := l.Agent(context.Background(), "chat-1"); got.Known() {
		t.Fatalf("a run with no agent resolved to %+v", got)
	}
	if got := l.Agent(context.Background(), ""); got.Known() || lookup.calls != 2 {
		t.Fatalf("an empty execution id was looked up (calls=%d)", lookup.calls)
	}
	if got := l.Agent(context.Background(), strings.Repeat("x", maxExecutionIDBytes+1)); got.Known() || lookup.calls != 2 {
		t.Fatal("an overlong execution id was looked up")
	}

	lookup.err = errors.New("database down")
	if got := l.Agent(context.Background(), "run-2"); got.Known() {
		t.Fatalf("a failed lookup resolved to %+v", got)
	}
	if l.Degraded() != 1 {
		t.Fatalf("Degraded = %d, want 1 after a failed agent lookup", l.Degraded())
	}
	// The cached agent outlives the database failure until its TTL.
	if got := l.Agent(context.Background(), "run-1"); !got.Known() {
		t.Fatal("a cached agent was lost while the database was down")
	}
	now = now.Add(agentCacheTTL + time.Second)
	if got := l.Agent(context.Background(), "run-1"); got.Known() {
		t.Fatal("an expired entry was served while the lookup fails")
	}
}

// requestsIn returns the request count of def's bucket for sub in the test
// window.
func requestsIn(c *fakeCounter, def RateLimitDef, sub Subject) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.totals[testSubject(kindRequests, BucketKey(def, sub), windowStart(testNow))]
}

func defNamed(t *testing.T, defs []RateLimitDef, name string) RateLimitDef {
	t.Helper()
	for _, d := range defs {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("no limit named %q in %+v", name, defs)
	return RateLimitDef{}
}

// A throttled agent that retries after its 429 must not spend the project's
// allowance: the narrow throttle is checked first and refuses before the
// project bucket is touched.
func TestThrottledAgentRetriesDoNotConsumeTheProjectAllowance(t *testing.T) {
	t.Parallel()
	c := newFakeCounter()
	l := newTestLimiter(c, testNow)
	snap := Compile([]Row{
		throttleRow("project", map[string]any{"project_ids": []any{7.0}}, 5),
		throttleRow("agent", map[string]any{"project_ids": []any{7.0}, "agent_ids": []any{3.0}}, 1),
	}, testNow)
	ctx := context.Background()

	agent := Subject{ProjectID: 7, Agent: AgentRef{ProjectID: 7, ApplicationID: 3}}
	defs := snap.RateLimits(agent)
	if dec := l.AdmitAll(ctx, defs, agent, true); !dec.Allowed {
		t.Fatalf("the agent's first call was refused: %+v", dec)
	}
	for i := 0; i < 10; i++ {
		if dec := l.AdmitAll(ctx, defs, agent, true); dec.Allowed || dec.Class != RateClassAgent {
			t.Fatalf("retry %d = %+v, want a refusal by the agent throttle", i+1, dec)
		}
	}
	if got := requestsIn(c, defNamed(t, defs, "project"), agent); got != 1 {
		t.Fatalf("project bucket = %d after 1 admitted call and 10 refused retries, want 1", got)
	}

	// The rest of the project still has 4 of its 5 calls.
	other := Subject{ProjectID: 7, Agent: AgentRef{ProjectID: 7, ApplicationID: 4}}
	for i := 0; i < 4; i++ {
		if dec := l.AdmitAll(ctx, snap.RateLimits(other), other, true); !dec.Allowed {
			t.Fatalf("another agent's call %d was refused: %+v; the throttled agent ate the project allowance", i+1, dec)
		}
	}
	if dec := l.AdmitAll(ctx, snap.RateLimits(other), other, true); dec.Allowed || dec.Class != RateClassProject {
		t.Fatalf("the 6th project call = %+v, want a refusal by the project limit", dec)
	}
}

// When the project limit refuses after a throttle admitted, the throttle's
// slot is given back.
func TestProjectRefusalGivesBackTheThrottleSlot(t *testing.T) {
	t.Parallel()
	c := newFakeCounter()
	l := newTestLimiter(c, testNow)
	snap := Compile([]Row{
		throttleRow("project", map[string]any{"project_ids": []any{7.0}}, 1),
		throttleRow("user", map[string]any{"user_ids": []any{11.0}}, 10),
	}, testNow)
	ctx := context.Background()
	sub := Subject{ProjectID: 7, UserID: 11}
	defs := snap.RateLimits(sub)

	if dec := l.AdmitAll(ctx, defs, sub, true); !dec.Allowed {
		t.Fatalf("the first call was refused: %+v", dec)
	}
	for i := 0; i < 3; i++ {
		if dec := l.AdmitAll(ctx, defs, sub, true); dec.Allowed || dec.Class != RateClassProject {
			t.Fatalf("call %d = %+v, want a refusal by the project limit", i+2, dec)
		}
	}
	if got := requestsIn(c, defNamed(t, defs, "user"), sub); got != 1 {
		t.Fatalf("user bucket = %d, want 1: the project refusals must give back the user slot", got)
	}
	if l.Degraded() != 0 {
		t.Fatalf("Degraded = %d, want 0", l.Degraded())
	}
}

// A compensation that fails is counted and logged; the refusal stands.
func TestFailedCompensationIsCounted(t *testing.T) {
	t.Parallel()
	c := newFakeCounter()
	c.failDecr = errors.New("nats down")
	l := newTestLimiter(c, testNow)
	snap := Compile([]Row{
		throttleRow("project", map[string]any{"project_ids": []any{7.0}}, 1),
		throttleRow("user", map[string]any{"user_ids": []any{11.0}}, 10),
	}, testNow)
	sub := Subject{ProjectID: 7, UserID: 11}
	defs := snap.RateLimits(sub)
	l.AdmitAll(context.Background(), defs, sub, true)
	if dec := l.AdmitAll(context.Background(), defs, sub, true); dec.Allowed {
		t.Fatal("the second call was admitted over the project limit")
	}
	if l.Degraded() != 1 {
		t.Fatalf("Degraded = %d, want 1 for the failed give-back", l.Degraded())
	}
}
