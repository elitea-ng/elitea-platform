package repos

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	httpapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/httpaction"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"strings"
	"testing"
)

// The HTTP owner fixture is actual frozen request/receipt input, pinned externally.
func owningHTTPRecordRow(r httpRecoveryRecord) scriptedRow {
	return scriptedRow{values: []any{r.state, r.activation, r.effect, r.requestDigest, r.bindingDigest, r.policyDigest, r.outputBucket, r.request, r.receiptWire, r.receiptSHA, r.input, r.tenant, r.project, r.projection}}
}
func TestNodeRecoveryMainTransactionUsesActualHTTPCommittedProof(t *testing.T) {
	execution, generation, journal, record := productionHTTPRecoveryFixture(t)
	raw, _ := json.Marshal(journal)
	raw, _ = domain.CanonicalReceipt(raw)
	hash := sha256.Sum256(raw)
	sha := hex.EncodeToString(hash[:])
	sub := recoverySubmission()
	sub.Request.ExecutionID = execution
	sub.Request.Generation = generation
	sub.Request.ActivationID = journal.ActivationID
	sub.Request.ExpectedRevision = journal.JournalRevision
	sub.Request.Action = "resume_result"
	for _, state := range []string{"completed", "uncertain", "dispatching", "failed", "missing"} {
		t.Run(state, func(t *testing.T) {
			changed := record
			changed.state = state
			ownerRow := owningHTTPRecordRow(changed)
			if state == "missing" {
				ownerRow = scriptedRow{err: pgx.ErrNoRows}
			}
			e := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{execution, generation, "SUSPENDED"}}, {err: pgx.ErrNoRows}, {values: []any{raw, hash[:], "SUSPENDED"}}, ownerRow}, execTags: []pgconn.CommandTag{pgconn.NewCommandTag("INSERT 0 1"), pgconn.NewCommandTag("UPDATE 1"), pgconn.NewCommandTag("INSERT 0 1")}}
			s := &recoveryTxStore{scriptedExecutor: e}
			repo := recoveryRepo(t, s)
			repo.effects = NewHTTPActionRecoveryProofProvider()
			out, err := repo.Submit(t.Context(), sub)
			if state != "completed" {
				if err == nil || s.committed || len(e.execCalls) != 0 {
					t.Fatal(out, err)
				}
				return
			}
			if err != nil || !s.committed || out.Action != "resume_result" {
				t.Fatal(out, err)
			}
			actionBytes := e.execCalls[0].args[6].([]byte)
			claim := recoveryClaim()
			claim.ExecutionID = execution
			claim.Generation = generation
			rows := append(recoveryClaimRows(), scriptedRow{values: []any{journal.ActivationID, journal.JournalRevision}}, scriptedRow{values: []any{raw, hash[:], actionBytes, "11", "AUTHORIZED"}}, owningHTTPRecordRow(record))
			readExecutor := &scriptedExecutor{rowResults: rows}
			readStore := &recoveryTxStore{scriptedExecutor: readExecutor}
			readRepo := recoveryRepo(t, readStore)
			readRepo.effects = NewHTTPActionRecoveryProofProvider()
			data, err := readRepo.ReadNodeRecoveryResult(t.Context(), claim, record.effect, *record.receiptSHA)
			if err != nil || !bytes.Equal(data, record.receiptWire) || !readStore.committed || len(readExecutor.execCalls) != 0 {
				t.Fatal("actual owner result redemption", err)
			}
			ack := storage.NodeRecoveryAck{RequestID: sub.Request.RequestID, ActivationID: journal.ActivationID, ExpectedRevision: journal.JournalRevision, ReceiptSHA256: sha, AppliedRevision: journal.JournalRevision + 1}
			ackRows := append(recoveryClaimRows(), scriptedRow{values: []any{journal.ActivationID, journal.JournalRevision}}, scriptedRow{values: []any{raw, hash[:], "AUTHORIZED", actionBytes, "11", "", uint64(0), []byte(nil)}}, owningHTTPRecordRow(record))
			ackExecutor := &scriptedExecutor{rowResults: ackRows, execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 1"), pgconn.NewCommandTag("UPDATE 1"), pgconn.NewCommandTag("UPDATE 1"), pgconn.NewCommandTag("INSERT 0 1")}}
			ackStore := &recoveryTxStore{scriptedExecutor: ackExecutor}
			ackRepo := recoveryRepo(t, ackStore)
			ackRepo.effects = NewHTTPActionRecoveryProofProvider()
			decision, err := ackRepo.AcknowledgeNodeRecovery(t.Context(), claim, ack)
			if err != nil || !ackStore.committed || !decision.RecoveryResumeAuthorized || decision.Resumption.ExecutionID != execution || decision.Resumption.Generation != generation {
				t.Fatal("actual owner restoration", decision, err)
			}
			for _, c := range append(e.execCalls, ackExecutor.execCalls...) {
				if strings.Contains(c.sql, "execution_http_effects") || strings.Contains(c.sql, "command_outbox") {
					t.Fatal("Main recovery dispatched another effect")
				}
			}
		})
	}
}

// Preserve original frozen application/request bytes; bind a production-form
// execution and recompute the owning effect/receipt through real HTTP contracts.
func productionHTTPRecoveryFixture(t *testing.T) (string, uint64, domain.Receipt, httpRecoveryRecord) {
	_, generation, journal, record := httpProofFixture(t)
	execution := "0123456789abcdef0123456789abcdef"
	invocation := httpapp.Invocation{ActivationID: record.activation, RequestDigest: record.requestDigest, BindingDigest: record.bindingDigest}
	record.effect = httpapp.EffectID(execution, generation, invocation)
	journal.ReplaySafety.ReceiptID = record.effect
	var receipt httpapp.Receipt
	if json.Unmarshal(record.receiptWire, &receipt) != nil {
		t.Fatal("owner fixture receipt")
	}
	receipt.EffectID = record.effect
	record.receiptWire, _ = json.Marshal(receipt)
	digest := httpapp.Digest(record.receiptWire)
	record.receiptSHA = &digest
	return execution, generation, journal, record
}
