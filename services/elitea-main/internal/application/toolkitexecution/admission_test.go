package toolkitexecution

import (
	"context"
	"errors"
	"testing"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

type toolkitReadAdmissionStoreStub struct {
	admission Admission
	outcome   executionapp.AdmissionOutcome
	err       error
}

func (s *toolkitReadAdmissionStoreStub) AdmitToolkitExecuteRead(
	_ context.Context,
	admission Admission,
) (executionapp.AdmissionOutcome, error) {
	s.admission = admission
	return s.outcome, s.err
}

func TestToolkitExecuteReadAdmissionBuildsDurableReferenceOnlyRecord(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	store := &toolkitReadAdmissionStoreStub{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "execution-1", CommandID: "command-1", Created: true,
		AdmittedAt: now, Deadline: now.Add(time.Minute),
	}}
	service := toolkitReadAdmissionService(t, store, now,
		"bundle-1", "content-1", "execution-1", "command-1", "outbox-1")
	outcome, err := service.Submit(context.Background(), SubmitRequest{
		Identity: executionapp.AdmissionIdentity{
			TenantID: "tenant-1", ResourceProjectID: "17",
			ProjectionProjectID: "17", ActorID: "29",
		},
		IdempotencyKey: "mcp-request-1",
		Frozen:         validFrozenCurrentReadTool(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.ExecutionID != "execution-1" || store.admission.Record.Job.CapabilityID != executiondomain.ToolkitExecuteReadCapability ||
		store.admission.Record.Job.ID != "execution-1" || store.admission.Record.Outbox.ID != "outbox-1" ||
		store.admission.Binding.RequestEntryID != toolkitReadRequestEntryID ||
		store.admission.Record.IdempotencyScope != "tenant-1/17/29" ||
		store.admission.Record.RequestDigest.IsZero() {
		t.Fatalf("admission = %#v outcome = %#v", store.admission, outcome)
	}
	if len(store.admission.Record.InputBundle.Entries) != 1 ||
		len(store.admission.Record.InputBundle.Entries[0].Content) == 0 {
		t.Fatal("authoritative input was not durably bound")
	}
}

func TestToolkitExecuteReadAdmissionDigestChangesWithAuthorityOrInvocation(t *testing.T) {
	identity := executionapp.AdmissionIdentity{
		TenantID: "tenant-1", ResourceProjectID: "17", ProjectionProjectID: "17", ActorID: "29",
	}
	content := []byte("invocation")
	base := toolkitReadRequestDigest(identity, content)
	changed := identity
	changed.ActorID = "30"
	if base == toolkitReadRequestDigest(changed, content) ||
		base == toolkitReadRequestDigest(identity, []byte("other")) {
		t.Fatal("request digest failed to bind authority or immutable input")
	}
}

func TestToolkitExecuteReadAdmissionRejectsUnsafeIdentityAndPreservesCancellation(t *testing.T) {
	now := time.Now().UTC()
	store := &toolkitReadAdmissionStoreStub{}
	service := toolkitReadAdmissionService(t, store, now,
		"bundle-1", "content-1", "execution-1", "command-1", "outbox-1")
	_, err := service.Submit(context.Background(), SubmitRequest{
		Identity: executionapp.AdmissionIdentity{
			TenantID: "tenant\nforged", ResourceProjectID: "17", ProjectionProjectID: "17", ActorID: "29",
		},
		IdempotencyKey: "request", Frozen: validFrozenCurrentReadTool(),
	})
	if !errors.Is(err, ErrInvalidToolkitExecuteReadAdmission) {
		t.Fatalf("unsafe identity error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = service.Submit(ctx, SubmitRequest{
		Identity: executionapp.AdmissionIdentity{
			TenantID: "tenant", ResourceProjectID: "17", ProjectionProjectID: "17", ActorID: "29",
		},
		IdempotencyKey: "request", Frozen: validFrozenCurrentReadTool(),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled admission error = %v", err)
	}
}

func toolkitReadAdmissionService(
	t *testing.T,
	store AtomicAdmissionStore,
	now time.Time,
	ids ...string,
) *AdmissionService {
	t.Helper()
	index := 0
	next := func() (string, error) {
		value := ids[index]
		index++
		return value, nil
	}
	factory, err := NewInputBundleFactory(InputProfile{
		Classification: "tenant-confidential", RequiredGrantAudience: "elitea.runtime.input.read.v1",
	}, next)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewAdmissionService(store, factory, func() time.Time { return now }, next)
	if err != nil {
		t.Fatal(err)
	}
	return service
}
