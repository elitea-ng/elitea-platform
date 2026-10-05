package repos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5/pgconn"
	"strings"
	"testing"
)

type recoveryProofStub struct {
	proof json.RawMessage
	data  []byte
	calls int
}

func (s *recoveryProofStub) VerifyNodeRecoveryEffect(_ context.Context, _ sqlExecutor, _ string, _ uint64, _ domain.Receipt, _ string) (json.RawMessage, error) {
	s.calls++
	return bytes.Clone(s.proof), nil
}
func (s *recoveryProofStub) ReadNodeRecoveryHTTPResult(_ context.Context, _ sqlExecutor, _ string, _ uint64, _ domain.Receipt, _ json.RawMessage) ([]byte, error) {
	s.calls++
	return bytes.Clone(s.data), nil
}

func recoveryEffectProof(receipt domain.Receipt, kind string) ([]byte, []byte) {
	data := []byte(`{"schema":"fixture-committed-result"}`)
	sha := sha256.Sum256(data)
	digest := hex.EncodeToString(sha[:])
	effect := receipt.ReplaySafety.EffectID
	if effect == "" {
		effect = strings.Repeat("3", 64)
	}
	proof := domain.OwnerProof{Schema: "elitea.pipeline.node-recovery-owner-proof.v1", Kind: kind, ExecutionID: recoveryClaim().ExecutionID, Generation: 1, ActivationID: receipt.ActivationID, Attempt: receipt.Attempt, ExpectedRevision: receipt.JournalRevision, EffectID: effect, OwnerReceiptSHA256: digest}
	if kind == "committed_result" {
		proof.ResultRef = &domain.ResultReference{ContentID: effect, ImmutableVersion: digest, DigestSHA256: digest, ByteLength: uint64(len(data)), MediaType: "application/json", RequiredGrantAudience: domain.HTTPResultAudience}
	}
	raw, _ := json.Marshal(proof)
	return raw, data
}
func TestNodeRecoveryAckReconciliationNeverAutomaticallyRestores(t *testing.T) {
	r, _, _ := recoveryFixture(t)
	r.ReplaySafety = domain.ReplaySafety{Kind: "unknown_external_effect", EffectID: strings.Repeat("3", 64)}
	r.StopReason = "effect_reconciliation_required"
	r.AllowedActions = []string{"reconcile"}
	raw, _ := json.Marshal(r)
	raw, _ = domain.CanonicalReceipt(raw)
	digest := sha256.Sum256(raw)
	sha := hex.EncodeToString(digest[:])
	proof, _ := recoveryEffectProof(r, "verified_no_effect")
	next := r
	next.JournalRevision = 4
	next.ReplaySafety = domain.ReplaySafety{Kind: "idempotent_effect_not_committed", EffectID: r.ReplaySafety.EffectID}
	next.StopReason = "operator_approval_required"
	next.AllowedActions = []string{"retry"}
	nextRaw, _ := json.Marshal(next)
	action, _ := json.Marshal(recoveryAction{RequestID: strings.Repeat("2", 64), ActivationID: r.ActivationID, ExpectedRevision: 3, LastAttempt: 1, Action: "reconcile", ReceiptSHA256: sha, OwnerProof: proof})
	for _, tc := range []struct {
		name         string
		continuation []byte
		mutate       func(*domain.Receipt)
		failure      bool
	}{{name: "evidence only", continuation: nextRaw}, {name: "null cannot restore", failure: true}, {name: "changed failed attempt", mutate: func(n *domain.Receipt) { n.Attempt = 2 }, failure: true}, {name: "changed node", mutate: func(n *domain.Receipt) { n.NodeID = "other" }, failure: true}, {name: "changed failure", mutate: func(n *domain.Receipt) { n.FailureClass = "rate_limited" }, failure: true}, {name: "dropped effect", mutate: func(n *domain.Receipt) { n.ReplaySafety = domain.ReplaySafety{Kind: "no_external_effect"} }, failure: true}} {
		t.Run(tc.name, func(t *testing.T) {
			cont := tc.continuation
			if tc.mutate != nil {
				n := next
				tc.mutate(&n)
				cont, _ = json.Marshal(n)
			}
			rows := append(recoveryClaimRows(), scriptedRow{values: []any{r.ActivationID, r.JournalRevision}}, scriptedRow{values: []any{raw, digest[:], "AUTHORIZED", action, "11", "", uint64(0), []byte(nil)}})
			e := &scriptedExecutor{rowResults: rows, execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 1"), pgconn.NewCommandTag("UPDATE 1"), pgconn.NewCommandTag("INSERT 0 1"), pgconn.NewCommandTag("UPDATE 1"), pgconn.NewCommandTag("INSERT 0 1")}}
			s := &recoveryTxStore{scriptedExecutor: e}
			repo := recoveryRepo(t, s)
			repo.effects = &recoveryProofStub{proof: proof}
			ack := storage.NodeRecoveryAck{RequestID: strings.Repeat("2", 64), ActivationID: r.ActivationID, ExpectedRevision: 3, ReceiptSHA256: sha, AppliedRevision: 4, ContinuationReceipt: cont}
			out, err := repo.AcknowledgeNodeRecovery(t.Context(), recoveryClaim(), ack)
			if tc.failure {
				if err == nil || s.committed || len(e.execCalls) > 0 {
					t.Fatal(out, err, s.committed)
				}
				return
			}
			if err != nil || !s.committed || out.RecoveryResumeAuthorized || out.Resumption != nil || out.TerminalSettlementAuthorized {
				t.Fatal(out, err)
			}
			for _, c := range e.execCalls {
				if strings.Contains(c.sql, "SET desired_state='RUNNING'") {
					t.Fatal("reconciliation restored")
				}
			}
		})
	}
}
func TestNodeRecoveryStoppedAckHasOnlyTerminalAuthority(t *testing.T) {
	r, raw, digest := recoveryFixture(t)
	sha := hex.EncodeToString(digest)
	action, _ := json.Marshal(recoveryAction{RequestID: strings.Repeat("2", 64), ActivationID: r.ActivationID, ExpectedRevision: 3, LastAttempt: 1, Action: "retry", ReceiptSHA256: sha})
	reason := "elapsed_limit"
	ack := storage.NodeRecoveryAck{RequestID: strings.Repeat("2", 64), ActivationID: r.ActivationID, ExpectedRevision: 3, ReceiptSHA256: sha, AppliedRevision: 4, TerminalStopReason: &reason}
	ackBytes, _ := canonicalRecoveryAck(ack)
	for _, replay := range []bool{false, true} {
		rows := recoveryClaimRows()
		status, consumed := "AUTHORIZED", ""
		applied := uint64(0)
		var saved []byte
		if replay {
			status, consumed, applied, saved = "STOPPED", "prior-claim", 4, ackBytes
			for i := range rows {
				rows[i].values[3] = "RUNNING"
			}
		}
		rows = append(rows, scriptedRow{values: []any{r.ActivationID, r.JournalRevision}}, scriptedRow{values: []any{raw, digest, status, action, "11", consumed, applied, saved}})
		e := &scriptedExecutor{rowResults: rows, execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 1"), pgconn.NewCommandTag("UPDATE 1"), pgconn.NewCommandTag("UPDATE 1"), pgconn.NewCommandTag("INSERT 0 1")}}
		s := &recoveryTxStore{scriptedExecutor: e}
		out, err := recoveryRepo(t, s).AcknowledgeNodeRecovery(t.Context(), recoveryClaim(), ack)
		if err != nil || !s.committed || out.Replay != replay || out.RecoveryResumeAuthorized || out.Resumption != nil || !out.TerminalSettlementAuthorized || out.TerminalAuthorization.StopReason != reason || out.TerminalAuthorization.ClaimID != recoveryClaim().ClaimID {
			t.Fatal(out, err)
		}
		if replay && len(e.execCalls) != 0 {
			t.Fatal("replay wrote")
		}
	}
}
func TestNodeRecoveryAckRejectsConflictingAppliedReplay(t *testing.T) {
	r, raw, digest := recoveryFixture(t)
	sha := hex.EncodeToString(digest)
	action, _ := json.Marshal(recoveryAction{RequestID: strings.Repeat("2", 64), ActivationID: r.ActivationID, ExpectedRevision: 3, LastAttempt: 1, Action: "retry", ReceiptSHA256: sha})
	rows := recoveryClaimRows()
	for i := range rows {
		rows[i].values[3] = "RUNNING"
	}
	rows = append(rows, scriptedRow{values: []any{r.ActivationID, r.JournalRevision}}, scriptedRow{values: []any{raw, digest, "RESUMED", action, "11", "prior", uint64(4), []byte(`{}`)}})
	e := &scriptedExecutor{rowResults: rows}
	s := &recoveryTxStore{scriptedExecutor: e}
	_, err := recoveryRepo(t, s).AcknowledgeNodeRecovery(t.Context(), recoveryClaim(), storage.NodeRecoveryAck{RequestID: strings.Repeat("2", 64), ActivationID: r.ActivationID, ExpectedRevision: 3, ReceiptSHA256: sha, AppliedRevision: 4})
	if !errors.Is(err, storage.ErrContentRejected) || s.committed || len(e.execCalls) != 0 {
		t.Fatal(err)
	}
}
