package repos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/noderecovery"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type recoveryTxStore struct {
	*scriptedExecutor
	entered, committed bool
	project            int64
}

func (s *recoveryTxStore) WithinProjectTx(ctx context.Context, project int64, _ pgx.TxOptions, fn func(sqlExecutor) error) error {
	s.project = project
	return s.WithinTx(ctx, pgx.TxOptions{}, fn)
}
func (s *recoveryTxStore) WithinTx(ctx context.Context, _ pgx.TxOptions, fn func(sqlExecutor) error) error {
	s.entered = true
	err := fn(s.scriptedExecutor)
	s.committed = err == nil
	return err
}

func recoveryFixture(t *testing.T) (domain.Receipt, []byte, []byte) {
	t.Helper()
	r := domain.Receipt{Schema: domain.Schema, ActivationID: strings.Repeat("1", 64), JournalRevision: 3, NodeID: "fetch", GraphThread: "fixture-root-thread", Step: 7, Attempt: 1, FailureClass: "dependency_unavailable", StopReason: "operator_approval_required", ReplaySafety: domain.ReplaySafety{Kind: "no_external_effect"}, AllowedActions: []string{"retry"}}
	raw, _ := json.Marshal(r)
	raw, err := domain.CanonicalReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	d := sha256.Sum256(raw)
	return r, raw, d[:]
}
func recoverySubmission() app.Submission {
	return app.Submission{Selector: app.Selector{ProjectID: 7, ActorUserID: 11, ResponseMessageID: "10000000-0000-4000-8000-000000000043"}, Request: domain.Request{RequestID: strings.Repeat("2", 64), ExecutionID: "0123456789abcdef0123456789abcdef", Generation: 1, ActivationID: strings.Repeat("1", 64), ExpectedRevision: 3, Action: "retry"}}
}
func recoveryRepo(t *testing.T, s *recoveryTxStore) *NodeRecoveryRepository {
	t.Helper()
	return &NodeRecoveryRepository{projects: s, shared: s, permissions: func(_ context.Context, tx sqlExecutor, selector app.Selector, permission string) error {
		if !s.entered || tx != s.scriptedExecutor || selector.ProjectID != 7 || selector.ActorUserID != 11 || (permission != "models.chat.messages.create" && permission != "models.applications.task.get") {
			t.Fatal("authorization not inside exact transaction")
		}
		return nil
	}}
}

func TestNodeRecoveryOperatorTransactionAndExactReplay(t *testing.T) {
	sub := recoverySubmission()
	_, raw, digest := recoveryFixture(t)
	requestRaw, _ := json.Marshal(sub.Request)
	for _, spec := range []struct {
		name             string
		existing         []byte
		existingErr      error
		replay, conflict bool
	}{{"new", nil, pgx.ErrNoRows, false, false}, {"exact replay", requestRaw, nil, true, false}, {"conflicting reuse", []byte(`{"action":"reconcile"}`), nil, false, true}} {
		t.Run(spec.name, func(t *testing.T) {
			e := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{sub.Request.ExecutionID, uint64(1), "SUSPENDED"}}, {values: []any{spec.existing}, err: spec.existingErr}, {values: []any{raw, digest, "SUSPENDED"}}}, execTags: []pgconn.CommandTag{pgconn.NewCommandTag("INSERT 0 1"), pgconn.NewCommandTag("UPDATE 1"), pgconn.NewCommandTag("INSERT 0 1")}}
			s := &recoveryTxStore{scriptedExecutor: e}
			r := recoveryRepo(t, s)
			out, err := r.Submit(t.Context(), sub)
			if spec.conflict {
				if !errors.Is(err, app.ErrNotAllowed) || s.committed || len(e.execCalls) != 0 {
					t.Fatalf("err=%v committed=%v writes=%d", err, s.committed, len(e.execCalls))
				}
				return
			}
			if err != nil || !s.committed || out.Replay != spec.replay || out.Generation != 1 || out.ExecutionID != sub.Request.ExecutionID {
				t.Fatal(out, err, s.committed)
			}
			if spec.replay {
				if len(e.execCalls) != 0 {
					t.Fatal("replay dispatched again")
				}
				return
			}
			if len(e.execCalls) != 3 || !strings.Contains(e.execCalls[0].sql, "node_recovery_control_outbox") || !strings.Contains(e.execCalls[2].sql, "node_recovery_audit") {
				t.Fatal(e.execCalls)
			}
			for _, call := range e.execCalls {
				if strings.Contains(call.sql, "command_outbox") || strings.Contains(call.sql, "input_bundles") {
					t.Fatal("new execution/effect dispatched")
				}
			}
		})
	}
}

func TestNodeRecoveryOperatorRefusesStaleForeignCancelledAndUnprovenEffects(t *testing.T) {
	sub := recoverySubmission()
	receipt, raw, digest := recoveryFixture(t)
	for _, spec := range []struct {
		name            string
		scope           []any
		scopeErr        error
		status          string
		modify          func(*domain.Receipt)
		permissionErr   bool
		revisionMissing bool
	}{{name: "foreign project", scopeErr: pgx.ErrNoRows}, {name: "other execution", scope: []any{"fedcba9876543210fedcba9876543210", uint64(1), "SUSPENDED"}}, {name: "new generation", scope: []any{sub.Request.ExecutionID, uint64(2), "SUSPENDED"}}, {name: "cancelled", scopeErr: pgx.ErrNoRows}, {name: "stale visit", revisionMissing: true}, {name: "already authorized", status: "AUTHORIZED"}, {name: "rights revoked", permissionErr: true}, {name: "ambiguous effect", modify: func(r *domain.Receipt) {
		r.ReplaySafety = domain.ReplaySafety{Kind: "unknown_external_effect", EffectID: strings.Repeat("3", 64)}
		r.StopReason = "effect_reconciliation_required"
		r.AllowedActions = []string{"reconcile"}
	}}, {name: "completed effect", modify: func(r *domain.Receipt) {
		r.ReplaySafety = domain.ReplaySafety{Kind: "completed_external_effect", ReceiptID: strings.Repeat("4", 64)}
		r.StopReason = "effect_reconciliation_required"
		r.AllowedActions = []string{"resume_result"}
	}}} {
		t.Run(spec.name, func(t *testing.T) {
			request := sub
			r := receipt
			thisRaw, thisDigest := raw, digest
			if spec.modify != nil {
				spec.modify(&r)
				request.Request.Action = r.AllowedActions[0]
				thisRaw, _ = json.Marshal(r)
				thisRaw, _ = domain.CanonicalReceipt(thisRaw)
				d := sha256.Sum256(thisRaw)
				thisDigest = d[:]
			}
			scope := spec.scope
			if scope == nil {
				scope = []any{sub.Request.ExecutionID, uint64(1), "SUSPENDED"}
			}
			status := spec.status
			if status == "" {
				status = "SUSPENDED"
			}
			visit := scriptedRow{values: []any{thisRaw, thisDigest, status}}
			if spec.revisionMissing {
				visit = scriptedRow{err: pgx.ErrNoRows}
			}
			e := &scriptedExecutor{rowResults: []scriptedRow{{values: scope, err: spec.scopeErr}, {err: pgx.ErrNoRows}, visit}}
			s := &recoveryTxStore{scriptedExecutor: e}
			repo := recoveryRepo(t, s)
			if spec.permissionErr {
				repo.permissions = func(context.Context, sqlExecutor, app.Selector, string) error { return app.ErrNotAllowed }
			}
			_, err := repo.Submit(t.Context(), request)
			if !errors.Is(err, app.ErrNotAllowed) || s.committed || len(e.execCalls) != 0 {
				t.Fatalf("err=%v committed=%v writes=%d", err, s.committed, len(e.execCalls))
			}
		})
	}
}

func recoveryClaim() storage.ContentClaim {
	u, _ := url.Parse("spiffe://elitea.test/worker/native")
	return storage.ContentClaim{PeerCertificate: &x509.Certificate{URIs: []*url.URL{u}}, ExecutionID: recoverySubmission().Request.ExecutionID, Generation: 1, ClaimID: "claim-recovery-2", FenceToken: bytes.Repeat([]byte{7}, 32)}
}
func recoveryClaimRows() []scriptedRow {
	row := scriptedRow{values: []any{int64(7), int64(11), recoverySubmission().ResponseMessageID, "SUSPENDED", "bundle-1", bytes.Repeat([]byte{9}, 32)}}
	return []scriptedRow{row, row}
}

func TestNodeRecoveryClaimAckCommitsSameGenerationRestoreAndAudit(t *testing.T) {
	receipt, raw, digest := recoveryFixture(t)
	sub := recoverySubmission()
	claim := recoveryClaim()
	sha := hex.EncodeToString(digest)
	action, _ := json.Marshal(recoveryAction{RequestID: sub.Request.RequestID, ActivationID: receipt.ActivationID, ExpectedRevision: 3, LastAttempt: 1, Action: "retry", ReceiptSHA256: sha})
	for _, spec := range []struct {
		name   string
		failAt int
	}{{"commit", -1}, {"consume failure", 0}, {"resume failure", 2}, {"audit failure", 3}} {
		t.Run(spec.name, func(t *testing.T) {
			rows := append(recoveryClaimRows(), scriptedRow{values: []any{receipt.ActivationID, receipt.JournalRevision}}, scriptedRow{values: []any{raw, digest, "AUTHORIZED", action, "11", "", uint64(0), []byte(nil)}})
			e := &scriptedExecutor{rowResults: rows, execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 1"), pgconn.NewCommandTag("UPDATE 1"), pgconn.NewCommandTag("UPDATE 1"), pgconn.NewCommandTag("INSERT 0 1")}}
			if spec.failAt >= 0 {
				e.execErrors = make([]error, spec.failAt+1)
				e.execErrors[spec.failAt] = errors.New("private injected failure")
			}
			s := &recoveryTxStore{scriptedExecutor: e}
			repo := recoveryRepo(t, s)
			ack := storage.NodeRecoveryAck{RequestID: sub.Request.RequestID, ActivationID: receipt.ActivationID, ExpectedRevision: 3, ReceiptSHA256: sha, AppliedRevision: 4}
			out, err := repo.AcknowledgeNodeRecovery(t.Context(), claim, ack)
			if spec.failAt >= 0 {
				if err == nil || s.committed || out.RecoveryResumeAuthorized {
					t.Fatal(out, err, s.committed)
				}
				return
			}
			if err != nil || !s.committed || !out.RecoveryResumeAuthorized || out.Resumption.ClaimID != claim.ClaimID || out.Resumption.Generation != 1 || out.Resumption.JournalRevision != 4 || out.Resumption.InputBundleID != "bundle-1" {
				t.Fatal(out, err)
			}
			for _, call := range e.execCalls {
				if strings.Contains(call.sql, "command_outbox") || strings.Contains(call.sql, "invocation_state") {
					t.Fatal("ordinary invocation granted")
				}
			}
		})
	}
}

func TestNodeRecoveryClaimAckRejectsExpiredFenceAndRevisionMismatch(t *testing.T) {
	claim := recoveryClaim()
	sub := recoverySubmission()
	for _, spec := range []struct {
		name    string
		expired bool
		applied uint64
	}{{"expired", true, 4}, {"wrong journal revision", false, 5}} {
		t.Run(spec.name, func(t *testing.T) {
			e := &scriptedExecutor{rowResults: []scriptedRow{{err: pgx.ErrNoRows}}}
			s := &recoveryTxStore{scriptedExecutor: e}
			r := recoveryRepo(t, s)
			ack := storage.NodeRecoveryAck{RequestID: sub.Request.RequestID, ActivationID: sub.Request.ActivationID, ExpectedRevision: 3, ReceiptSHA256: strings.Repeat("5", 64), AppliedRevision: spec.applied}
			_, err := r.AcknowledgeNodeRecovery(t.Context(), claim, ack)
			if err == nil || s.committed || len(e.execCalls) != 0 {
				t.Fatal(err, s.committed, e.execCalls)
			}
		})
	}
}

func TestNodeRecoveryReadReturnsOnlyCurrentAuthorizedScopeAndIntactReceipt(t *testing.T) {
	_, raw, digest := recoveryFixture(t)
	for _, tc := range []struct {
		name, status string
		digest       []byte
		foreign      bool
		deny         bool
	}{{name: "current suspended", status: "SUSPENDED", digest: digest}, {name: "pending authorized", status: "AUTHORIZED", digest: digest}, {name: "foreign target", foreign: true, deny: true}, {name: "tampered receipt digest", status: "SUSPENDED", digest: bytes.Repeat([]byte{9}, 32), deny: true}, {name: "cancelled", status: "CANCELLED", digest: digest, deny: true}} {
		t.Run(tc.name, func(t *testing.T) {
			scope := scriptedRow{values: []any{recoverySubmission().Request.ExecutionID, uint64(1), "SUSPENDED"}}
			if tc.foreign {
				scope = scriptedRow{err: pgx.ErrNoRows}
			}
			e := &scriptedExecutor{rowResults: []scriptedRow{scope, {values: []any{tc.status, raw, tc.digest}}}}
			s := &recoveryTxStore{scriptedExecutor: e}
			out, err := recoveryRepo(t, s).Read(t.Context(), recoverySubmission().Selector)
			if tc.deny {
				if err == nil || s.committed {
					t.Fatal(out, err)
				}
				return
			}
			if err != nil || !s.committed || out.Generation != 1 || string(out.Receipt) != string(raw) || len(e.execCalls) != 0 {
				t.Fatal(out, err)
			}
		})
	}
}
