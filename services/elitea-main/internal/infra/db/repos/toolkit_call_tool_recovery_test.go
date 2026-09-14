package repos

import (
	"bytes"
	"context"
	"errors"
	"testing"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	toolkit "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	runtime "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

type recoveryToolkitProducer struct {
	t        *testing.T
	envelope executionapp.PreparedCommandEnvelope
	calls    int
	fail     bool
}

func (p *recoveryToolkitProducer) PrepareToolkitCallTool(context.Context, toolkit.Dispatch) (executionapp.PreparedCommandEnvelope, error) {
	p.t.Fatal("recovery attempted to reconstruct or sign a command")
	return executionapp.PreparedCommandEnvelope{}, nil
}
func (p *recoveryToolkitProducer) AppendPrepared(_ context.Context, id string, envelope executionapp.PreparedCommandEnvelope) error {
	p.calls++
	if id != "prepared-recovery-outbox" || !bytes.Equal(envelope.Bytes, p.envelope.Bytes) || envelope.Digest != p.envelope.Digest || envelope.KeyID != p.envelope.KeyID {
		p.t.Fatal("recovery changed signed command identity")
	}
	if p.fail {
		return errors.New("simulated Redis connection loss")
	}
	return nil
}

func TestPostgresToolkitPreparedRecovery(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	repo, err := NewToolkitCallToolJobsRepository(pool, toolRunDispatchPolicy())
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := newToolRunAdmissionService(t, repo, "prepared-recovery").Submit(ctx, toolRunSubmitRequest("prepared-recovery", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	ids, err := repo.ListPreparedToolkitCallToolRecovery(ctx, 32)
	if err != nil || len(ids) != 0 {
		t.Fatalf("unprepared command selected: %v %v", ids, err)
	}
	raw := []byte("exact signed command selected by the prior process")
	envelope := executionapp.PreparedCommandEnvelope{Bytes: raw, Digest: runtime.SHA256(raw), SignatureProfile: 1, KeyID: "original-key"}
	if _, err := repo.StorePreparedToolkitCallTool(ctx, admitted.OutboxID, envelope); err != nil {
		t.Fatal(err)
	}
	// Create fresh repository and dispatcher instances. Recovery uses no admission object.
	replacement, err := NewToolkitCallToolJobsRepository(pool, toolRunDispatchPolicy())
	if err != nil {
		t.Fatal(err)
	}
	ids, err = replacement.ListPreparedToolkitCallToolRecovery(ctx, 32)
	if err != nil || len(ids) != 1 || ids[0] != admitted.OutboxID {
		t.Fatalf("prepared recovery missing: %v %v", ids, err)
	}
	producer := &recoveryToolkitProducer{t: t, envelope: envelope, fail: true}
	dispatcher, err := toolkit.NewDispatcher(replacement, producer)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.RecoverPrepared(ctx, ids[0]); err == nil {
		t.Fatal("publication failure suppressed")
	}
	ids, err = replacement.ListPreparedToolkitCallToolRecovery(ctx, 32)
	if err != nil || len(ids) != 1 {
		t.Fatal("failed append lost its durable recovery command")
	}
	producer.fail = false
	if err := dispatcher.RecoverPrepared(ctx, ids[0]); err != nil {
		t.Fatal(err)
	}
	ids, err = replacement.ListPreparedToolkitCallToolRecovery(ctx, 32)
	if err != nil || len(ids) != 0 || producer.calls != 2 {
		t.Fatalf("published command remains pending: %v %v", ids, err)
	}
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.execution_jobs`)
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.command_outbox WHERE published_at IS NOT NULL`)
	// A stale recovery candidate must respect a subsequent cancellation.
	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.execution_jobs SET desired_state='CANCELLED' WHERE execution_id=$1`, admitted.Outcome.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.RecoverPrepared(ctx, admitted.OutboxID); err == nil {
		t.Fatal("cancelled binding was still available")
	}
	if producer.calls != 2 {
		t.Fatal("cancelled command appended")
	}
}
