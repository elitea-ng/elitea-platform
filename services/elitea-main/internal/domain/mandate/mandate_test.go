package mandate

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

type ttl time.Duration

func (d ttl) MaxMandateTTL() time.Duration { return time.Duration(d) }

const (
	testMandateID = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f31"
	testBindingID = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f11"
)

var testNow = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func autonomousRequest() Request {
	return Request{
		TenantProjectID: 7, Kind: KindAutonomous, AgentBindingID: testBindingID,
		OperationScopes: []string{"github.issues.read"}, ExpiresAt: testNow.Add(time.Hour),
	}
}

func mandateIn(t *testing.T, state State) Mandate {
	t.Helper()
	m, err := New(testMandateID, autonomousRequest(), ttl(24*time.Hour), "rev-1", "100", testNow)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.RevocationEpoch = 3
	m.State = state
	if state == StateExpired {
		m.State = StateActive
		m.ExpiresAt = testNow.Add(-time.Second)
	}
	return m
}

func TestMandateTransitionTableIsComplete(t *testing.T) {
	allowed := map[State]map[Action]State{
		StateProposed:  {ActionApprove: StateActive, ActionRevoke: StateRevoked},
		StateActive:    {ActionSuspend: StateSuspended, ActionRevoke: StateRevoked},
		StateSuspended: {ActionApprove: StateActive, ActionRevoke: StateRevoked},
		StateExpired:   {ActionRevoke: StateRevoked},
		StateRevoked:   {},
	}
	pairs := 0
	for _, state := range States() {
		for _, action := range Actions() {
			pairs++
			current := mandateIn(t, state)
			if got := current.EffectiveState(testNow); got != state {
				t.Fatalf("fixture %s has effective state %s", state, got)
			}
			next, err := current.Transition(action, "300", "rev-2", testNow)
			want, ok := allowed[state][action]
			if !ok {
				if !errors.Is(err, ErrInvalidTransition) {
					t.Errorf("%s --%s--> %v, want ErrInvalidTransition", state, action, err)
				}
				continue
			}
			if err != nil || next.State != want {
				t.Errorf("%s --%s--> %s %v, want %s", state, action, next.State, err, want)
				continue
			}
			if next.RevocationEpoch != current.RevocationEpoch+1 {
				t.Errorf("%s --%s--> epoch %d, want +1", state, action, next.RevocationEpoch)
			}
		}
	}
	if pairs != 15 {
		t.Fatalf("covered %d pairs, want 15", pairs)
	}
}

func TestMandateApprovalByTheIssuerIsRefused(t *testing.T) {
	m := mandateIn(t, StateProposed)
	if _, err := m.Transition(ActionApprove, m.IssuedBy, "rev-1", testNow); !errors.Is(err, ErrSelfApproval) {
		t.Errorf("approval by issuer: %v", err)
	}
}

func TestMandateExpiryRules(t *testing.T) {
	policy := ttl(2 * time.Hour)
	req := autonomousRequest()
	req.ExpiresAt = time.Time{}
	if _, err := New(testMandateID, req, policy, "rev-1", "100", testNow); !errors.Is(err, ErrExpiresAtRequired) {
		t.Errorf("no expires_at: %v", err)
	}
	req.ExpiresAt = testNow.Add(2*time.Hour + time.Second)
	if _, err := New(testMandateID, req, policy, "rev-1", "100", testNow); !errors.Is(err, ErrTTLExceedsPolicy) {
		t.Errorf("beyond policy max: %v", err)
	}
	req.ExpiresAt = testNow.Add(2 * time.Hour)
	if _, err := New(testMandateID, req, policy, "rev-1", "100", testNow); err != nil {
		t.Errorf("exactly the policy max: %v", err)
	}
	if _, err := New(testMandateID, req, ttl(0), "rev-1", "100", testNow); !errors.Is(err, ErrTTLExceedsPolicy) {
		t.Errorf("a zero policy max allows nothing: %v", err)
	}
	if _, err := New(testMandateID, req, nil, "rev-1", "100", testNow); !errors.Is(err, ErrTTLExceedsPolicy) {
		t.Errorf("no policy allows nothing: %v", err)
	}
	req.ExpiresAt = testNow.Add(-time.Minute)
	if _, err := New(testMandateID, req, policy, "rev-1", "100", testNow); !errors.Is(err, ErrInvalidMandate) {
		t.Errorf("an expiry in the past: %v", err)
	}
	req.ExpiresAt, req.NotBefore = testNow.Add(time.Hour), testNow.Add(90*time.Minute)
	if _, err := New(testMandateID, req, policy, "rev-1", "100", testNow); !errors.Is(err, ErrInvalidMandate) {
		t.Errorf("expiry before not_before: %v", err)
	}
	m := mandateIn(t, StateActive)
	m.ExpiresAt = testNow
	if m.EffectiveState(testNow) != StateExpired {
		t.Errorf("a mandate at its expiry instant is expired")
	}
}

func TestMandateKindRules(t *testing.T) {
	user := int64(42)
	delegation := "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f41"
	cases := map[string]struct {
		mutate func(*Request)
		ok     bool
	}{
		"autonomous with a subject":               {func(r *Request) { r.SubjectUserID = &user }, false},
		"autonomous with a delegation":            {func(r *Request) { r.DelegationID = &delegation }, false},
		"background without a subject":            {func(r *Request) { r.Kind = KindDelegatedBackground }, false},
		"background with a subject":               {func(r *Request) { r.Kind, r.SubjectUserID = KindDelegatedBackground, &user }, true},
		"interactive_delegation is not a mandate": {func(r *Request) { r.Kind = "interactive_delegation" }, false},
		"no operation scope":                      {func(r *Request) { r.OperationScopes = nil }, false},
		"a repeated scope":                        {func(r *Request) { r.OperationScopes = []string{"a", "a"} }, false},
		"a scope with whitespace":                 {func(r *Request) { r.ResourceScopes = []string{"repo x"} }, false},
		"binding id not a uuid":                   {func(r *Request) { r.AgentBindingID = "7" }, false},
	}
	for name, tc := range cases {
		req := autonomousRequest()
		tc.mutate(&req)
		_, err := New(testMandateID, req, ttl(24*time.Hour), "rev-1", "100", testNow)
		if tc.ok != (err == nil) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestMandateAndDelegationFieldSetsHoldNoSecretMaterial(t *testing.T) {
	assertFieldSet(t, reflect.TypeOf(Mandate{}), []string{
		"AgentBindingID", "ApprovedBy", "CreatedAt", "DelegationID", "ExpiresAt", "ID", "IssuedBy", "Kind",
		"NotBefore", "OperationScopes", "PolicyRevisionID", "ResourceScopes", "RevocationEpoch", "State",
		"SubjectUserID", "TenantProjectID", "UpdatedAt",
	})
	assertFieldSet(t, reflect.TypeOf(Delegation{}), []string{
		"AgentBindingID", "Audiences", "ConsentRecordedAt", "ExpiresAt", "ID", "Issuer", "RevocationEpoch",
		"Scopes", "State", "SubjectObjectID", "TenantID", "TenantProjectID",
	})
}

func assertFieldSet(t *testing.T, typ reflect.Type, want []string) {
	t.Helper()
	var got []string
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		got = append(got, name)
		lower := strings.ToLower(name)
		for _, banned := range []string{"token", "secret", "password", "assertion", "credential", "key", "cipher"} {
			if strings.Contains(lower, banned) {
				t.Errorf("%s.%s looks like secret material", typ.Name(), name)
			}
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s fields = %v, want %v", typ.Name(), got, want)
	}
}
