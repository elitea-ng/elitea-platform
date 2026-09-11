package repos

import (
	"context"
	"testing"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

// This gate exercises stored terminal recovery in PostgreSQL. It does not
// exercise provider invocation or the toolkit payload decoder.
func TestPostgresToolkitInboxTerminalRecovery(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	for _, tc := range []struct {
		name, capability, payloadType string
		recover                       bool
	}{
		{"call", executiondomain.ToolkitCallToolCapability, payloadTypeToolkitCallToolResult, true},
		{"discovery", executiondomain.ToolkitAvailableToolsCapability, payloadTypeToolkitAvailableToolsResult, true},
		{"validation", executiondomain.ConfigurationValidationCapability, payloadTypeConfigurationValidation, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			frame := postgresValidationFrame(t, "inbox-recovery-"+tc.name)
			seed := seedPostgresValidationExecution(t, pool, frame, runtimedomain.DesiredRunning)
			if tc.recover {
				_, err := pool.Exec(ctx, `UPDATE elitea_runtime.execution_jobs SET capability_id=$2,
                    configuration_revision_id=NULL, configuration_type=NULL, catalog_revision=NULL,
                    catalog_digest=NULL, schema_id=NULL, schema_revision=NULL, schema_digest=NULL,
                    settings_entry_id=NULL WHERE execution_id=$1`, frame.Fence.ExecutionID, tc.capability)
				if err != nil {
					t.Fatal(err)
				}
			}
			record, _, err := validationOutputRecord(frame)
			if err != nil {
				t.Fatal(err)
			}
			record.PayloadType = tc.payloadType
			executor := pgxExecutor{queryer: pool}
			inserted, err := insertOutputInbox(ctx, executor, record)
			if err != nil || !inserted.Inserted {
				t.Fatalf("seed durable output: %+v %v", inserted, err)
			}
			expirePostgresClaim(t, pool, seed.claimID)
			repository, err := NewClaimsRepository(pool)
			if err != nil {
				t.Fatal(err)
			}
			request := executionapp.ClaimRequest{
				CommandID: frame.Fence.CommandID, OutboxID: seed.outboxID,
				ExecutionID: frame.Fence.ExecutionID, Generation: frame.Fence.Generation,
				CapabilityID: tc.capability, SignedEnvelopeDigest: seed.envelopeDigest,
				WorkloadIdentity:  "spiffe://elitea.test/worker/replacement",
				WorkloadSessionID: "replacement-session", ProducerID: "replacement-producer",
			}
			decision, err := repository.ClaimValidation(ctx, request, executionapp.MaxClaimLeaseTTLMillis)
			if err != nil {
				t.Fatal(err)
			}
			recovered := decision.Disposition == executionapp.ClaimRecoverTerminalACK
			if recovered != tc.recover {
				t.Fatalf("disposition=%s, want recovery=%v", decision.Disposition, tc.recover)
			}
			var marked bool
			if err := pool.QueryRow(ctx, `SELECT projected_at IS NOT NULL FROM elitea_runtime.output_inbox WHERE event_id=$1`, record.EventID).Scan(&marked); err != nil {
				t.Fatal(err)
			}
			if marked != tc.recover {
				t.Fatalf("projection marker=%v, want %v", marked, tc.recover)
			}
			if !tc.recover {
				return
			}
			proposal := decision.SettlementRecovery.Proposal
			if proposal.TerminalEventID != record.EventID || proposal.TerminalPayloadDigest != record.PayloadDigest {
				t.Fatal("recovery changed the durable result")
			}
			settlements, err := NewSettlementsRepository(pool)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := settlements.PrepareSettlement(ctx, *proposal); err != nil {
				t.Fatalf("prepare recovered settlement: %v", err)
			}
			var before, after time.Time
			if err := pool.QueryRow(ctx, `SELECT projected_at FROM elitea_runtime.output_inbox WHERE event_id=$1`, record.EventID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if err := markOutputProjected(ctx, executor, record.EventID); err != nil {
				t.Fatalf("replay projection: %v", err)
			}
			if err := pool.QueryRow(ctx, `SELECT projected_at FROM elitea_runtime.output_inbox WHERE event_id=$1`, record.EventID).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if !before.Equal(after) {
				t.Fatal("replay changed the projection timestamp")
			}
			if err := markOutputProjected(ctx, executor, "missing-event"); err == nil {
				t.Fatal("missing result accepted")
			}
		})
	}
}
