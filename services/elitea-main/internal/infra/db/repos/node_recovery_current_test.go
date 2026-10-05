package repos

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
)

func TestNodeRecoveryRepeatedVisitsRefuseHistoricalAckOnCurrentReplacementClaim(t *testing.T) {
	a, raw, digest := recoveryFixture(t)
	request := recoverySubmission().Request
	claim := recoveryClaim()
	claim.ClaimID = "current-replacement"
	ack := storage.NodeRecoveryAck{RequestID: request.RequestID, ActivationID: a.ActivationID, ExpectedRevision: a.JournalRevision, ReceiptSHA256: hex.EncodeToString(digest), AppliedRevision: a.JournalRevision + 1}
	action, _ := json.Marshal(recoveryAction{RequestID: request.RequestID, ActivationID: a.ActivationID, ExpectedRevision: a.JournalRevision, LastAttempt: a.Attempt, Action: "retry", ReceiptSHA256: ack.ReceiptSHA256})
	saved, _ := canonicalRecoveryAck(ack)
	for _, stage := range []string{"A resumed/current exact replay", "B suspended", "B authorized", "B resumed", "B stopped", "B cancelled", "A later revision"} {
		t.Run(stage, func(t *testing.T) {
			currentActivation, currentRevision := a.ActivationID, a.JournalRevision
			desired := "RUNNING"
			allow := stage == "A resumed/current exact replay"
			if !allow {
				currentActivation = strings.Repeat("b", 64)
				currentRevision = 1
			}
			if stage == "B suspended" || stage == "B authorized" {
				desired = "SUSPENDED"
			}
			if stage == "A later revision" {
				currentActivation = a.ActivationID
				currentRevision = a.JournalRevision + 1
			}
			rows := recoveryClaimRows()
			for i := range rows {
				rows[i].values[3] = desired
			}
			rows = append(rows, scriptedRow{values: []any{currentActivation, currentRevision}}, scriptedRow{values: []any{raw, digest, "RESUMED", action, "11", "prior-A-claim", ack.AppliedRevision, saved}})
			e := &scriptedExecutor{rowResults: rows}
			s := &recoveryTxStore{scriptedExecutor: e}
			out, err := recoveryRepo(t, s).AcknowledgeNodeRecovery(t.Context(), claim, ack)
			if allow {
				if err != nil || !s.committed || !out.Replay || !out.RecoveryResumeAuthorized || out.Resumption == nil || out.Resumption.ClaimID != claim.ClaimID {
					t.Fatal(out, err)
				}
			} else {
				if !errors.Is(err, storage.ErrContentRejected) || s.committed || out.Resumption != nil || out.RecoveryResumeAuthorized || out.TerminalAuthorization != nil || len(e.rowCalls) != 3 {
					t.Fatal("historical authority escaped", out, err)
				}
			}
			if len(e.execCalls) != 0 {
				t.Fatal("replay mutated")
			}
			q := e.rowCalls[2]
			if strings.Contains(q.sql, "status IN") || strings.Contains(q.sql, "status=") || q.args[0] != claim.ExecutionID || q.args[1] != int64(claim.Generation) {
				t.Fatal("current selection skipped a newer status", q)
			}
		})
	}
}

func TestNodeRecoveryResultReadDoesNotFallBackAcrossNewerStoppedVisit(t *testing.T) {
	a, _, _ := recoveryFixture(t)
	claim := recoveryClaim()
	e := &scriptedExecutor{rowResults: append(recoveryClaimRows(), scriptedRow{values: []any{strings.Repeat("b", 64), uint64(1)}}, scriptedRow{err: pgx.ErrNoRows})}
	s := &recoveryTxStore{scriptedExecutor: e}
	repo := recoveryRepo(t, s)
	owner := &recoveryProofStub{}
	repo.effects = owner
	_, err := repo.ReadNodeRecoveryResult(t.Context(), claim, strings.Repeat("3", 64), strings.Repeat("4", 64))
	if !errors.Is(err, storage.ErrContentRejected) || s.committed || owner.calls != 0 || len(e.execCalls) != 0 {
		t.Fatal("old result redeemed", err)
	}
	q := e.rowCalls[3]
	if len(q.args) != 4 || q.args[2] == a.ActivationID || q.args[2] != strings.Repeat("b", 64) || q.args[3] != int64(1) || strings.Contains(q.sql, "ORDER BY") {
		t.Fatal("eligible-history fallback", q)
	}
}
