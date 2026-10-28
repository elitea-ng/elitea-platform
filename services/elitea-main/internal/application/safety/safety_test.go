package safety

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// memoryStore is an in-memory Store. It records what a create would have
// cancelled so the service tests can assert the cancel runs on create.
type memoryStore struct {
	mu        sync.Mutex
	rows      []Suspension
	loads     int
	loadErr   error
	cancelled []Target
}

func (m *memoryStore) LoadActiveSuspensions(context.Context) ([]Suspension, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loads++
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	var out []Suspension
	for _, row := range m.rows {
		if row.Active() {
			out = append(out, row)
		}
	}
	return out, nil
}

func (m *memoryStore) CreateSuspension(_ context.Context, s NewSuspension) (Suspension, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, row := range m.rows {
		if row.Active() && row.Target == s.Target {
			return Suspension{}, &AlreadySuspendedError{Existing: row}
		}
	}
	created := Suspension{
		ID: int64(len(m.rows) + 1), Target: s.Target, Reason: s.Reason, ActorID: s.ActorID,
		Source: s.Source, ResumeRequires: s.ResumeRequires, CreatedAt: time.Now(),
	}
	m.cancelled = append(m.cancelled, s.Target)
	m.rows = append(m.rows, created)
	return created, nil
}

func (m *memoryStore) ResumeSuspension(_ context.Context, r ResumeRecord) (Suspension, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.rows {
		if m.rows[i].ID != r.ID {
			continue
		}
		if !m.rows[i].Active() {
			return Suspension{}, ErrNotActive
		}
		now, by := time.Now(), r.ResumedBy
		m.rows[i].ResumedAt, m.rows[i].ResumedBy, m.rows[i].ResumeReason = &now, &by, r.Reason
		return m.rows[i], nil
	}
	return Suspension{}, ErrNotFound
}

func (m *memoryStore) GetSuspension(_ context.Context, id int64) (Suspension, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, row := range m.rows {
		if row.ID == id {
			return row, nil
		}
	}
	return Suspension{}, ErrNotFound
}

func (m *memoryStore) ListSuspensions(_ context.Context, f ListFilter) ([]Suspension, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Suspension
	for _, row := range m.rows {
		if f.ActiveOnly && !row.Active() {
			continue
		}
		if f.ProjectID > 0 && row.Target.ProjectID != f.ProjectID {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time          { return c.now }
func (c *clock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newFixture(t *testing.T) (*memoryStore, *Gate, *Service, *clock) {
	t.Helper()
	store := &memoryStore{}
	c := &clock{now: time.Unix(1_800_000_000, 0)}
	gate, err := NewGate(store, GateOptions{MaxAge: time.Minute, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(store, gate)
	if err != nil {
		t.Fatal(err)
	}
	return store, gate, service, c
}

var platformAdmin = Actor{UserID: 1, PlatformAdmin: true}

func TestAdmissionRefusesEachScopeAndOnlyWhatItCovers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		target  Target
		covered []Subject
		free    []Subject
	}{
		{
			name:    "platform",
			target:  Target{Scope: ScopePlatform},
			covered: []Subject{{ProjectID: 1}, {ProjectID: 2, ApplicationID: 9}, {ProjectID: 3, ToolkitType: "github"}},
		},
		{
			name:    "project",
			target:  Target{Scope: ScopeProject, ProjectID: 7},
			covered: []Subject{{ProjectID: 7}, {ProjectID: 7, ApplicationID: 3}},
			free:    []Subject{{ProjectID: 8}, {ProjectID: 8, ApplicationID: 3}},
		},
		{
			name:    "agent",
			target:  Target{Scope: ScopeAgent, ProjectID: 7, ApplicationID: 3},
			covered: []Subject{{ProjectID: 7, ApplicationID: 3}},
			// Application ids are per tenant schema: agent 3 of project 8 is
			// another agent. An ad hoc chat (no agent) is not covered.
			free: []Subject{{ProjectID: 8, ApplicationID: 3}, {ProjectID: 7, ApplicationID: 4}, {ProjectID: 7}},
		},
		{
			name:    "toolkit_type",
			target:  Target{Scope: ScopeToolkitType, ToolkitType: "Git Hub"},
			covered: []Subject{{ProjectID: 1, ToolkitType: "github"}, {ProjectID: 2, ToolkitType: "git-hub"}},
			free:    []Subject{{ProjectID: 1}, {ProjectID: 1, ApplicationID: 3}, {ProjectID: 1, ToolkitType: "jira"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, gate, service, _ := newFixture(t)
			created, err := service.Create(context.Background(), platformAdmin, CreateRequest{
				Target: tc.target, Reason: "incident 42", Confirmation: PlatformStopConfirmation,
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, subject := range tc.covered {
				err := gate.Check(context.Background(), subject)
				refusal, ok := AsSuspended(err)
				if !ok || !errors.Is(err, ErrExecutionSuspended) {
					t.Fatalf("%+v: err = %v, want execution_suspended", subject, err)
				}
				if refusal.SuspensionID != created.ID || refusal.Scope != tc.target.Scope {
					t.Fatalf("%+v: refusal = %+v", subject, refusal)
				}
				status, body, ok := HTTPRefusal(err)
				if !ok || status != 409 || body["code"] != ErrorCode || body["scope"] != string(tc.target.Scope) {
					t.Fatalf("HTTP refusal = %d %v", status, body)
				}
				if _, leaked := body["reason"]; leaked {
					t.Fatal("the operator's reason must not reach the caller")
				}
			}
			for _, subject := range tc.free {
				if err := gate.Check(context.Background(), subject); err != nil {
					t.Fatalf("%+v: err = %v, want admitted", subject, err)
				}
			}
		})
	}
}

func TestMostSpecificSuspensionIsNamed(t *testing.T) {
	t.Parallel()
	active := []Suspension{
		{ID: 1, Target: Target{Scope: ScopePlatform}},
		{ID: 2, Target: Target{Scope: ScopeProject, ProjectID: 7}},
		{ID: 3, Target: Target{Scope: ScopeAgent, ProjectID: 7, ApplicationID: 3}},
	}
	refusal, _ := AsSuspended(CheckSnapshot(active, Subject{ProjectID: 7, ApplicationID: 3}))
	if refusal == nil || refusal.SuspensionID != 3 {
		t.Fatalf("refusal = %+v, want the agent suspension", refusal)
	}
	refusal, _ = AsSuspended(CheckSnapshot(active, Subject{ProjectID: 7, ApplicationID: 4}))
	if refusal == nil || refusal.SuspensionID != 2 {
		t.Fatalf("refusal = %+v, want the project suspension", refusal)
	}
}

func TestGateCachesUntilInvalidatedOrOld(t *testing.T) {
	t.Parallel()
	store, gate, _, c := newFixture(t)
	ctx := context.Background()
	for range 3 {
		if err := gate.Check(ctx, Subject{ProjectID: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if store.loads != 1 {
		t.Fatalf("loads = %d, want one read for a fresh snapshot", store.loads)
	}

	// A row written behind the gate's back (another replica) is not seen
	// until an invalidation arrives...
	store.rows = append(store.rows, Suspension{ID: 9, Target: Target{Scope: ScopePlatform}})
	if err := gate.Check(ctx, Subject{ProjectID: 1}); err != nil {
		t.Fatal("the snapshot is fresh; the remote row is not read yet")
	}
	// ...a remote invalidation over NATS makes the next check reload.
	gate.receive([]byte(`{"v":1,"origin":"another-replica","kind":"suspension","id":9}`))
	if !errors.Is(gate.Check(ctx, Subject{ProjectID: 1}), ErrExecutionSuspended) {
		t.Fatal("an invalidation must make the next check see the suspension")
	}
	loads := store.loads

	// Own, foreign-version and malformed messages are ignored.
	gate.receive([]byte(`{"v":1,"origin":"` + gate.origin + `","kind":"suspension","id":9}`))
	gate.receive([]byte(`{"v":2,"origin":"x","kind":"suspension","id":9}`))
	gate.receive([]byte(`not json`))
	_ = gate.Check(ctx, Subject{ProjectID: 1})
	if store.loads != loads {
		t.Fatalf("loads = %d, want %d: ignored messages must not invalidate", store.loads, loads)
	}

	// Without any message, the snapshot age is the backstop.
	store.rows[0].ResumedAt = &c.now
	c.advance(2 * time.Minute)
	if err := gate.Check(ctx, Subject{ProjectID: 1}); err != nil {
		t.Fatalf("after MaxAge the resumed row must be gone: %v", err)
	}
}

func TestGateKeepsLastSnapshotOnRefreshFailureAndFailsClosedCold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Cold: no snapshot and the read fails. The start is refused.
	cold := &memoryStore{loadErr: errors.New("db down")}
	gate, _ := NewGate(cold, GateOptions{})
	err := gate.Check(ctx, Subject{ProjectID: 1})
	if !errors.Is(err, ErrStateUnavailable) {
		t.Fatalf("cold err = %v, want ErrStateUnavailable", err)
	}
	if status, _, ok := HTTPRefusal(err); !ok || status != 503 {
		t.Fatalf("cold HTTP = %d %t", status, ok)
	}
	// Cold reads through on every check: it recovers as soon as the database does.
	cold.loadErr = nil
	if err := gate.Check(ctx, Subject{ProjectID: 1}); err != nil {
		t.Fatalf("cold recovery: %v", err)
	}

	// Warm: a failed refresh keeps the last snapshot (a suspension stays
	// enforced) and backs off before trying again.
	store, warm, service, c := newFixture(t)
	if _, err := service.Create(ctx, platformAdmin, CreateRequest{
		Target: Target{Scope: ScopeProject, ProjectID: 5}, Reason: "x",
	}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(warm.Check(ctx, Subject{ProjectID: 5}), ErrExecutionSuspended) {
		t.Fatal("the warm snapshot holds the suspension")
	}
	store.loadErr = errors.New("db blip")
	warm.Invalidate()
	if !errors.Is(warm.Check(ctx, Subject{ProjectID: 5}), ErrExecutionSuspended) {
		t.Fatal("a failed refresh must keep enforcing the last snapshot")
	}
	loads := store.loads
	_ = warm.Check(ctx, Subject{ProjectID: 5})
	if store.loads != loads {
		t.Fatal("a failed refresh must back off, not hammer the database")
	}
	c.advance(2 * time.Second)
	store.loadErr = nil
	_ = warm.Check(ctx, Subject{ProjectID: 5})
	if store.loads != loads+1 {
		t.Fatal("after the back-off the gate must read again")
	}
}

func TestCreateCancelsAndInvalidatesAndRefusesDuplicates(t *testing.T) {
	t.Parallel()
	store, gate, service, _ := newFixture(t)
	ctx := context.Background()
	_ = gate.Check(ctx, Subject{ProjectID: 3}) // warm the snapshot

	target := Target{Scope: ScopeAgent, ProjectID: 3, ApplicationID: 11}
	created, err := service.Create(ctx, platformAdmin, CreateRequest{Target: target, Reason: "loops on a tool"})
	if err != nil {
		t.Fatal(err)
	}
	if len(store.cancelled) != 1 || store.cancelled[0] != target {
		t.Fatalf("cancelled = %v, want the agent's runs", store.cancelled)
	}
	if created.Source != SourceManual || created.ResumeRequires != ResumeAnyAdmin {
		t.Fatalf("defaults = %s %s", created.Source, created.ResumeRequires)
	}
	// The local replica sees it at once, without waiting for MaxAge.
	if !errors.Is(gate.Check(ctx, Subject{ProjectID: 3, ApplicationID: 11}), ErrExecutionSuspended) {
		t.Fatal("a create must invalidate this replica's snapshot")
	}
	// The remote copy is queued for the other replicas.
	select {
	case id := <-gate.queue:
		if id != created.ID {
			t.Fatalf("queued %d", id)
		}
	default:
		t.Fatal("a create must queue the invalidation for the other replicas")
	}

	var already *AlreadySuspendedError
	if _, err := service.Create(ctx, platformAdmin, CreateRequest{Target: target, Reason: "again"}); !errors.As(err, &already) {
		t.Fatalf("err = %v, want AlreadySuspendedError", err)
	}
}

func TestCreateValidation(t *testing.T) {
	t.Parallel()
	_, _, service, _ := newFixture(t)
	ctx := context.Background()
	projectAdmin := Actor{UserID: 2, ProjectID: 7}
	cases := []struct {
		name    string
		actor   Actor
		request CreateRequest
		want    error
	}{
		{"platform needs STOP", platformAdmin, CreateRequest{Target: Target{Scope: ScopePlatform}, Reason: "x", Confirmation: "stop"}, ErrConfirmationRequired},
		{"reason required", platformAdmin, CreateRequest{Target: Target{Scope: ScopeProject, ProjectID: 1}, Reason: "  "}, ErrInvalidTarget},
		{"unknown scope", platformAdmin, CreateRequest{Target: Target{Scope: "tenant"}, Reason: "x"}, ErrInvalidTarget},
		{"empty toolkit key", platformAdmin, CreateRequest{Target: Target{Scope: ScopeToolkitType, ToolkitType: "--"}, Reason: "x"}, ErrInvalidTarget},
		{"unknown resume rule", platformAdmin, CreateRequest{Target: Target{Scope: ScopeProject, ProjectID: 1}, Reason: "x", ResumeRequires: "three_person"}, ErrInvalidTarget},
		{"project admin: platform", projectAdmin, CreateRequest{Target: Target{Scope: ScopePlatform}, Reason: "x", Confirmation: "STOP"}, ErrScopeForbidden},
		{"project admin: toolkit type", projectAdmin, CreateRequest{Target: Target{Scope: ScopeToolkitType, ToolkitType: "github"}, Reason: "x"}, ErrScopeForbidden},
		{"project admin: another project", projectAdmin, CreateRequest{Target: Target{Scope: ScopeProject, ProjectID: 8}, Reason: "x"}, ErrScopeForbidden},
		{"a breaker row is not written here", platformAdmin, CreateRequest{Target: Target{Scope: ScopeProject, ProjectID: 1}, Reason: "x", Source: SourceBreaker}, ErrInvalidTarget},
		{"no user, no self-test", Actor{PlatformAdmin: true}, CreateRequest{Target: Target{Scope: ScopeProject, ProjectID: 1}, Reason: "x"}, ErrScopeForbidden},
	}
	for _, tc := range cases {
		if _, err := service.Create(ctx, tc.actor, tc.request); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if _, err := service.Create(ctx, projectAdmin, CreateRequest{
		Target: Target{Scope: ScopeAgent, ProjectID: 7, ApplicationID: 3}, Reason: "x",
	}); err != nil {
		t.Errorf("a project admin suspends an agent of the project: %v", err)
	}
}

func TestResumePermissions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	creator := Actor{UserID: 10, PlatformAdmin: true}
	otherPlatform := Actor{UserID: 11, PlatformAdmin: true}
	projectAdmin := Actor{UserID: 12, ProjectID: 7}
	otherProjectAdmin := Actor{UserID: 13, ProjectID: 8}
	agent := Target{Scope: ScopeAgent, ProjectID: 7, ApplicationID: 3}

	cases := []struct {
		name     string
		requires ResumeRequirement
		target   Target
		actor    Actor
		want     error
	}{
		{"any_admin: project admin", ResumeAnyAdmin, agent, projectAdmin, nil},
		{"any_admin: the suspender", ResumeAnyAdmin, agent, creator, nil},
		{"any_admin: another project's admin", ResumeAnyAdmin, agent, otherProjectAdmin, ErrNotFound},
		{"any_admin: project admin, platform scope", ResumeAnyAdmin, Target{Scope: ScopePlatform}, projectAdmin, ErrNotFound},
		{"governance_role: project admin", ResumeGovernanceRole, agent, projectAdmin, ErrResumeForbidden},
		{"governance_role: platform admin", ResumeGovernanceRole, agent, creator, nil},
		{"two_person: the suspender", ResumeTwoPerson, agent, creator, ErrResumeForbidden},
		{"two_person: project admin", ResumeTwoPerson, agent, projectAdmin, ErrResumeForbidden},
		{"two_person: a second platform admin", ResumeTwoPerson, agent, otherPlatform, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, gate, service, _ := newFixture(t)
			created, err := service.Create(ctx, creator, CreateRequest{
				Target: tc.target, Reason: "x", ResumeRequires: tc.requires, Confirmation: PlatformStopConfirmation,
			})
			if err != nil {
				t.Fatal(err)
			}
			resumed, err := service.Resume(ctx, tc.actor, ResumeRequest{ID: created.ID, Reason: "fixed"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.want != nil {
				return
			}
			if resumed.Active() || *resumed.ResumedBy != tc.actor.UserID || resumed.ResumeReason != "fixed" {
				t.Fatalf("resumed = %+v", resumed)
			}
			if err := gate.Check(ctx, Subject{ProjectID: 7, ApplicationID: 3}); err != nil {
				t.Fatalf("after the resume the start is admitted again: %v", err)
			}
			if _, err := service.Resume(ctx, tc.actor, ResumeRequest{ID: created.ID}); !errors.Is(err, ErrNotActive) {
				t.Fatalf("a second resume: %v", err)
			}
		})
	}
}

func TestSelfTestPassesAndLeavesNothingActive(t *testing.T) {
	t.Parallel()
	store, gate, service, _ := newFixture(t)
	var recorded []SelfTestResult
	selfTest, err := NewSelfTest(service, gate, SelfTestOptions{
		Record: func(_ context.Context, r SelfTestResult) { recorded = append(recorded, r) },
	})
	if err != nil {
		t.Fatal(err)
	}
	// A real platform suspension is active: the self-test must still name
	// its own synthetic suspension and must not resume the real one.
	if _, err := service.Create(context.Background(), platformAdmin, CreateRequest{
		Target: Target{Scope: ScopePlatform}, Reason: "real incident", Confirmation: PlatformStopConfirmation,
	}); err != nil {
		t.Fatal(err)
	}

	outcome, err := selfTest.Pass(context.Background())
	if err != nil || outcome != "ok" {
		t.Fatalf("outcome = %s, err = %v", outcome, err)
	}
	if len(recorded) != 1 || !recorded[0].Refused || !recorded[0].Resumed || recorded[0].SuspensionID == 0 {
		t.Fatalf("recorded = %+v", recorded)
	}
	active, _ := store.LoadActiveSuspensions(context.Background())
	if len(active) != 1 || active[0].Target.Scope != ScopePlatform {
		t.Fatalf("active after the self-test = %+v, want only the real platform stop", active)
	}
	for _, row := range store.rows {
		if row.Source == SourceSelfTest && row.Active() {
			t.Fatal("the synthetic suspension outlived the pass")
		}
	}
	// A synthetic row left active by a crashed pass is cleaned up.
	if _, err := service.Create(context.Background(), selfTestActor, CreateRequest{
		Target: selfTestTarget, Reason: SelfTestReason, Source: SourceSelfTest,
	}); err != nil {
		t.Fatal(err)
	}
	if outcome, err := selfTest.Pass(context.Background()); err != nil || outcome != "ok" {
		t.Fatalf("after a crashed pass: %s %v", outcome, err)
	}
}

func TestSelfTestSkipsUnderMaintenanceAndReportsAFailure(t *testing.T) {
	t.Parallel()
	_, gate, service, _ := newFixture(t)
	selfTest, _ := NewSelfTest(service, gate, SelfTestOptions{
		Maintenance: func(context.Context) (bool, error) { return true, nil },
	})
	if outcome, err := selfTest.Pass(context.Background()); outcome != "maintenance" || err != nil {
		t.Fatalf("maintenance: %s %v", outcome, err)
	}

	store := &memoryStore{}
	broken, _ := NewGate(store, GateOptions{})
	service, _ = NewService(store, broken)
	var recorded []SelfTestResult
	selfTest, _ = NewSelfTest(service, broken, SelfTestOptions{
		SLO:    time.Nanosecond,
		Now:    func() time.Time { return time.Now().Add(time.Duration(len(recorded)) * time.Hour) },
		Record: func(_ context.Context, r SelfTestResult) { recorded = append(recorded, r) },
	})
	// A gate whose store cannot read fails the pass: admission would admit
	// nothing, and the self-test must say the switch is not proven.
	store.loadErr = errors.New("db down")
	outcome, err := selfTest.Pass(context.Background())
	if outcome != "error" || err == nil || len(recorded) != 1 || recorded[0].Refused {
		t.Fatalf("outcome = %s, err = %v, recorded = %+v", outcome, err, recorded)
	}
	store.loadErr = nil
	active, _ := store.LoadActiveSuspensions(context.Background())
	if len(active) != 0 {
		t.Fatal("a failed pass must still resume its synthetic suspension")
	}
}

// An invalidation after a failed refresh is not swallowed by the back-off:
// it may be the suspension this replica must see, so the next read reloads
// even inside the second the back-off would otherwise hold.
func TestGateInvalidationAfterFailedRefreshForcesReload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, gate, _, _ := newFixture(t)
	if err := gate.Check(ctx, Subject{ProjectID: 5}); err != nil {
		t.Fatal(err)
	}
	store.loadErr = errors.New("db blip")
	gate.Invalidate()
	_ = gate.Check(ctx, Subject{ProjectID: 5}) // fails, backs off
	loads := store.loads

	// Inside the back-off, the database recovers and a suspension lands.
	store.loadErr = nil
	store.rows = append(store.rows, Suspension{ID: 99, Target: Target{Scope: ScopeProject, ProjectID: 5}})
	gate.Invalidate()
	if !errors.Is(gate.Check(ctx, Subject{ProjectID: 5}), ErrExecutionSuspended) {
		t.Fatal("an invalidation during the back-off must force a reload")
	}
	if store.loads != loads+1 {
		t.Fatalf("loads = %d, want %d", store.loads, loads+1)
	}
}

// The admission subject travels on the context from the decorator to the
// store's transaction; an unmarked context carries none.
func TestAdmissionSubjectRoundTrips(t *testing.T) {
	t.Parallel()
	if _, ok := AdmissionSubject(context.Background()); ok {
		t.Fatal("an unmarked context carries a subject")
	}
	want := Subject{ProjectID: 3, ApplicationID: 4, ToolkitType: "github"}
	got, ok := AdmissionSubject(WithAdmissionSubject(context.Background(), want))
	if !ok || got != want {
		t.Fatalf("subject = %+v %t", got, ok)
	}
}
