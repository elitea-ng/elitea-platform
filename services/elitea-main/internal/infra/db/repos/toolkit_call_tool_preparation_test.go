package repos

import (
	"context"
	"errors"
	"testing"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	toolkit "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	runtime "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

type admissionToolkitPreparer struct {
	calls    int
	fail     bool
	dispatch toolkit.Dispatch
	envelope executionapp.PreparedCommandEnvelope
}

func (p *admissionToolkitPreparer) PrepareToolkitCallTool(_ context.Context, d toolkit.Dispatch) (executionapp.PreparedCommandEnvelope, error) {
	p.calls++
	p.dispatch = d
	if p.fail {
		return executionapp.PreparedCommandEnvelope{}, errors.New("simulated signing failure")
	}
	return p.envelope, nil
}

func TestPostgresToolkitAtomicPreparation(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	raw := []byte("durable admission envelope")
	preparer := &admissionToolkitPreparer{fail: true, envelope: executionapp.PreparedCommandEnvelope{Bytes: raw, Digest: runtime.SHA256(raw), SignatureProfile: 1, KeyID: "admission-key"}}
	repo, err := NewToolkitCallToolJobsRepository(pool, toolRunDispatchPolicy(), preparer)
	if err != nil {
		t.Fatal(err)
	}
	request := toolRunSubmitRequest("atomic", `{}`)
	if _, err := newToolRunAdmissionService(t, repo, "failed-preparation").Submit(ctx, request); err == nil {
		t.Fatal("signing failure admitted work")
	}
	for _, table := range []string{"execution_jobs", "command_outbox", "input_bundles"} {
		assertPostgresCount(t, ctx, pool, 0, "SELECT count(*) FROM elitea_runtime."+table)
	}
	preparer.fail = false
	admitted, err := newToolRunAdmissionService(t, repo, "prepared-recovery").Submit(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	d := preparer.dispatch
	if d.ExecutionID != admitted.Outcome.ExecutionID || d.CommandID != admitted.Outcome.CommandID || d.OutboxID != admitted.OutboxID || d.InputBundleID != admitted.InputBundle.ID || d.InputBundleDigest != admitted.InputBundle.Digest || d.ToolName != admitted.Binding.ToolName || d.SettingsEntryID != admitted.Binding.SettingsEntryID || d.ArgumentsEntryID != admitted.Binding.ArgumentsEntryID || !d.Deadline.Equal(admitted.Outcome.Deadline) {
		t.Fatal("prepared command differs from admitted binding")
	}
	// No foreground dispatch occurs. A new process can recover immediately after commit.
	replacement, err := NewToolkitCallToolJobsRepository(pool, toolRunDispatchPolicy())
	if err != nil {
		t.Fatal(err)
	}
	ids, err := replacement.ListPreparedToolkitCallToolRecovery(ctx, 32)
	if err != nil || len(ids) != 1 || ids[0] != admitted.OutboxID {
		t.Fatalf("admission lacks prepared command: %v %v", ids, err)
	}
	producer := &recoveryToolkitProducer{t: t, envelope: preparer.envelope}
	dispatcher, err := toolkit.NewDispatcher(replacement, producer)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.RecoverPrepared(ctx, ids[0]); err != nil {
		t.Fatal(err)
	}
	if producer.calls != 1 {
		t.Fatal("replacement did not publish")
	}
	replay, err := newToolRunAdmissionService(t, repo, "replay").Submit(ctx, request)
	if err != nil || replay.Outcome.Created || replay.Outcome.ExecutionID != admitted.Outcome.ExecutionID || preparer.calls != 2 {
		t.Fatalf("replay changed admission or signed again: %v", err)
	}
	request.Inputs.Arguments = []byte(`{"different":true}`)
	if _, err := newToolRunAdmissionService(t, repo, "conflict").Submit(ctx, request); !errors.Is(err, executionapp.ErrIdempotencyConflict) {
		t.Fatalf("changed input did not conflict: %v", err)
	}
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.execution_jobs`)
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.command_outbox WHERE prepared_signed_envelope_bytes IS NOT NULL AND published_at IS NOT NULL`)
}
