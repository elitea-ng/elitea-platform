package heartbeat

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runbrief"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/proactivity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

type fakeStore struct {
	release   Release
	wakes     []Wake
	posts     []Post
	skips     []string
	discarded int
}

func (f *fakeStore) Maintenance(context.Context) bool { return false }
func (f *fakeStore) Settings(context.Context) (platformconfig.ProactivitySettings, error) {
	return platformconfig.DefaultProactivitySettings(), nil
}
func (f *fakeStore) Projects(context.Context) ([]int64, error)           { return []int64{1}, nil }
func (f *fakeStore) ProjectEnabled(context.Context, int64) (bool, error) { return true, nil }
func (f *fakeStore) DueAgents(context.Context, int64, time.Time, int) ([]Agent, error) {
	return []Agent{{ProjectID: 1, ApplicationID: 31, OwnerUserID: 11, MandateID: "m"}}, nil
}
func (f *fakeStore) CurrentRelease(context.Context, Agent) (Release, bool, error) {
	return f.release, true, nil
}
func (f *fakeStore) Reschedule(_ context.Context, _ Agent, _ time.Time, reason string) error {
	f.skips = append(f.skips, reason)
	return nil
}
func (f *fakeStore) RunActive(context.Context, Agent) (bool, error)             { return false, nil }
func (f *fakeStore) DayEnvelope(context.Context, Agent, time.Time) (Day, error) { return Day{}, nil }
func (f *fakeStore) Material(context.Context, Agent, proactivity.Block) (Material, error) {
	at := time.Now()
	return Material{NewestAt: &at}, nil
}
func (f *fakeStore) RecordWake(_ context.Context, wake Wake) error {
	f.wakes = append(f.wakes, wake)
	return nil
}
func (f *fakeStore) PostToOwner(_ context.Context, post Post) (string, error) {
	f.posts = append(f.posts, post)
	return "c", nil
}
func (f *fakeStore) PrepareAct(context.Context, Agent, Release, string) (ActTarget, error) {
	return ActTarget{ConversationUUID: "c", ParticipantID: 21, Created: true}, nil
}
func (f *fakeStore) DiscardAct(context.Context, Agent, ActTarget) { f.discarded++ }

type answer struct {
	text string
	err  error
}

func (a answer) Complete(context.Context, runbrief.CompletionRequest) (string, error) {
	return a.text, a.err
}

type gate struct{ calls, failAt int }

func (g *gate) Check(context.Context, safety.Subject) error {
	g.calls++
	if g.failAt > 0 && g.calls >= g.failAt {
		return &safety.SuspendedError{SuspensionID: 1}
	}
	return nil
}

type mandates struct{}

func (mandates) Admit(context.Context, string, runmandate.Target) (securitycontext.RunAuthority, error) {
	return securitycontext.RunAuthority{MandateID: "m", SubjectUserID: 11}, nil
}
func (mandates) Refuse(context.Context, runmandate.Target, string, error) error { return nil }

type starter struct{ err error }

func (s starter) StartCurrentApplication(context.Context, agentexecution.CurrentApplicationStartRequest) (agentexecution.CurrentApplicationStartOutcome, error) {
	return agentexecution.CurrentApplicationStartOutcome{ExecutionID: "0123456789abcdef0123456789abcdef"}, s.err
}

func run(t *testing.T, store *fakeStore, completer runbrief.Completer, g *gate, start Starter) {
	t.Helper()
	service, err := New(Config{Store: store, Gate: g, Completer: completer, Mandates: mandates{}, Start: start,
		Unit: func() float64 { return 0.5 }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func personalRelease() Release {
	return Release{ReleaseID: 5, VersionID: 42, Personal: true,
		Meta: map[string]any{"proactivity": map[string]any{"mode": "heartbeat"}}}
}

func TestTriageFailureKeepsTheCursor(t *testing.T) {
	store := &fakeStore{release: personalRelease()}
	run(t, store, answer{err: errors.New("gateway down")}, &gate{}, nil)
	if len(store.wakes) != 1 || store.wakes[0].Outcome != OutcomeFailed || store.wakes[0].ReasonCode != ReasonTriageFailed ||
		store.wakes[0].CursorAt != nil {
		t.Fatalf("wakes = %+v", store.wakes)
	}
	store = &fakeStore{release: personalRelease()}
	run(t, store, answer{err: runbrief.ErrNoModelPlane}, &gate{}, nil)
	if store.wakes[0].ReasonCode != ReasonNoModel {
		t.Fatalf("no model plane = %+v", store.wakes[0])
	}
	store = &fakeStore{release: personalRelease()}
	run(t, store, answer{text: "I think we should act."}, &gate{}, nil)
	if store.wakes[0].ReasonCode != ReasonTriageUnreadable {
		t.Fatalf("unreadable = %+v", store.wakes[0])
	}
}

func TestRefusedStartDiscardsAndDegrades(t *testing.T) {
	store := &fakeStore{release: personalRelease()}
	run(t, store, answer{text: `{"decision":"act","reason":"r","message":"Do it."}`}, &gate{}, starter{err: errors.New("no")})
	if store.discarded != 1 || store.wakes[0].Outcome != OutcomeActDegraded || store.wakes[0].ReasonCode != ReasonStartRefused {
		t.Fatalf("wakes = %+v, discarded %d", store.wakes, store.discarded)
	}
}

func TestSuspensionDuringTheTriageStopsTheAct(t *testing.T) {
	store := &fakeStore{release: personalRelease()}
	run(t, store, answer{text: `{"decision":"act","reason":"r","message":"Do it."}`}, &gate{failAt: 2}, starter{})
	if len(store.wakes) != 1 || store.wakes[0].Outcome != OutcomeActDegraded || store.wakes[0].ReasonCode != ReasonSuspendedMidCycle {
		t.Fatalf("wakes = %+v", store.wakes)
	}
}

func TestActWithNoStarterDegrades(t *testing.T) {
	store := &fakeStore{release: personalRelease()}
	run(t, store, answer{text: `{"decision":"act","reason":"r","message":"Do it."}`}, &gate{}, nil)
	if store.wakes[0].ReasonCode != ReasonStartRefused || len(store.posts) != 1 {
		t.Fatalf("wakes = %+v", store.wakes)
	}
}

func TestAnInvalidStoredBlockDoesNotWake(t *testing.T) {
	store := &fakeStore{release: Release{ReleaseID: 5, Personal: true,
		Meta: map[string]any{"proactivity": map[string]any{"mode": "heartbeat", "interval": "1m"}}}}
	run(t, store, answer{text: `{"decision":"no_op","reason":"r"}`}, &gate{}, nil)
	if len(store.wakes) != 0 || len(store.skips) != 1 || store.skips[0] != ReasonProactivityOff {
		t.Fatalf("wakes %+v skips %v", store.wakes, store.skips)
	}
}
