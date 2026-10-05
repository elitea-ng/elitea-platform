package repos

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
	runtime "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type codeOwnerEvidenceStub struct {
	wire   []byte
	err    error
	calls  int
	before func(code.GrantClaims)
}

func (s *codeOwnerEvidenceStub) Read(_ context.Context, c code.GrantClaims, _ code.SignedGrant) ([]byte, error) {
	s.calls++
	if s.before != nil {
		s.before(c)
	}
	return bytes.Clone(s.wire), s.err
}

type codeBrokerFactsStub struct {
	facts               storage.CodeBrokerEffectFacts
	err                 error
	calls               int
	expectedFingerprint string
}

func (s *codeBrokerFactsStub) VerifyOriginalCodeBroker(context.Context, storage.CodeTransaction, storage.ContentClaim, storage.OriginalCodeVisit, storage.CodePreparedRequest) error {
	return code.ErrRejected
}
func (s *codeBrokerFactsStub) ReadOriginalCodeBrokerEffects(_ context.Context, _ storage.CodeTransaction, _ string, _ uint64, _ string, _ string, fingerprint string) (storage.CodeBrokerEffectFacts, error) {
	s.calls++
	if fingerprint != s.expectedFingerprint {
		return storage.CodeBrokerEffectFacts{}, code.ErrRejected
	}
	return s.facts, s.err
}

func codeRecoveryFixture(t *testing.T, f originalCodeFixture, kind string) (domain.Receipt, []byte, code.Visit, []byte) {
	t.Helper()
	journal := domain.Receipt{Schema: domain.Schema, ActivationID: f.visit.Activation, JournalRevision: 3, NodeID: f.visit.Node, GraphThread: f.visit.Thread, Step: f.visit.Step, Attempt: f.visit.Attempt, FailureClass: "attempt_timeout", StopReason: "effect_reconciliation_required", ReplaySafety: domain.ReplaySafety{Kind: "unknown_external_effect", EffectID: f.binding.DispatchActivation}, AllowedActions: []string{"reconcile"}}
	if kind == "committed_result" {
		journal.ReplaySafety = domain.ReplaySafety{Kind: "completed_external_effect", ReceiptID: f.binding.DispatchActivation}
		journal.AllowedActions = []string{"resume_result"}
	}
	raw, err := domain.CanonicalReceipt(mustRecoveryJSON(journal))
	if err != nil {
		t.Fatal(err)
	}
	visit := code.Visit{ActivationID: journal.ActivationID, NodeID: journal.NodeID, GraphThread: journal.GraphThread, Step: journal.Step, Attempt: journal.Attempt, ExpectedRevision: journal.JournalRevision, ReceiptSHA256: code.Digest(raw)}
	receipt := code.Receipt{Schema: "elitea.sandbox.whole-code-recovery-receipt.v1", Kind: kind, Binding: f.binding, Visit: visit}
	if kind == "committed_result" {
		result := []byte(`{"answer":7}`)
		receipt.ResultBase64URL = base64.RawURLEncoding.EncodeToString(result)
		receipt.ResultSHA256 = code.Digest(result)
	} else {
		tuple, _ := code.Canonical([]any{f.binding, visit})
		receipt.SealID = runtime.SnapshotHash("elitea.sandbox.whole-code-no-effect-seal.v1\x00", tuple)
	}
	wire, err := code.Canonical(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = code.DecodeReceipt(wire, f.binding, visit); err != nil {
		t.Fatal(err)
	}
	return journal, raw, visit, wire
}
func codeRecoveryOwnerRows(f originalCodeFixture, journal domain.Receipt, selector []byte) []scriptedRow {
	return []scriptedRow{{values: []any{f.visitWire, f.bindingWire, code.Digest(f.bindingWire), selector}}, {values: []any{journal.ActivationID, journal.JournalRevision}}, {values: []any{codeIntentDigestBytes(f.visit.InputSHA256)}}}
}
func codeRecoveryFenceRow(f originalCodeFixture) scriptedRow {
	return scriptedRow{values: []any{f.claim.ClaimID, f.access.peer, f.access.attempt, f.access.epoch, bytes.Clone(f.claim.FenceToken), f.access.now, f.access.lease, f.access.deadline}}
}

func TestCodeOwnerProofCommitsBeforeExactOperatorOutboxAndNeverDispatches(t *testing.T) {
	for _, kind := range []string{"committed_result", "verified_no_effect"} {
		t.Run(kind, func(t *testing.T) {
			f := newOriginalCodeFixture(t)
			journal, raw, _, wire := codeRecoveryFixture(t, f, kind)
			sub := recoverySubmission()
			sub.Request.ActivationID = journal.ActivationID
			sub.Request.Action = journal.AllowedActions[0]
			selector, _ := code.Canonical([]any{nil, nil})
			rows := []scriptedRow{{values: []any{f.claim.ExecutionID, uint64(1), "SUSPENDED"}}, {err: pgx.ErrNoRows}, {values: []any{raw, codeIntentDigestBytes(code.Digest(raw)), "SUSPENDED"}}}
			rows = append(rows, codeRecoveryOwnerRows(f, journal, selector)...)
			rows = append(rows, codeRecoveryFenceRow(f), scriptedRow{err: pgx.ErrNoRows}, codeRecoveryFenceRow(f))
			e := &scriptedExecutor{rowResults: rows, execTags: []pgconn.CommandTag{pgconn.NewCommandTag("INSERT 0 1"), pgconn.NewCommandTag("INSERT 0 1"), pgconn.NewCommandTag("UPDATE 1"), pgconn.NewCommandTag("INSERT 0 1")}}
			s := &recoveryTxStore{scriptedExecutor: e}
			owner := &codeOwnerEvidenceStub{wire: wire}
			codeRepo := originalCodeRepo(t, s, f)
			codeRepo.owner = owner
			owner.before = func(c code.GrantClaims) {
				if !s.entered || s.committed || len(e.execCalls) != 0 || c.BindingSHA256 != code.Digest(f.bindingWire) || c.ClaimID != f.claim.ClaimID || c.RequestDigest != f.binding.RequestDigest || c.RequesterWorkloadIdentity != "spiffe://elitea/main" {
					t.Fatal("owner grant escaped original record/current transaction", c)
				}
				expected := "read"
				if kind == "verified_no_effect" {
					expected = "seal_no_effect"
				}
				if c.Operation != expected {
					t.Fatal("owner role changed")
				}
			}
			repository := recoveryRepo(t, s)
			repository.effects = codeRepo
			result, err := repository.Submit(t.Context(), sub)
			if err != nil || !s.committed || owner.calls != 1 || result.Generation != 1 || result.ExecutionID != f.claim.ExecutionID || len(e.execCalls) != 4 {
				t.Fatal(result, err, s.committed, owner.calls, e.execCalls)
			}
			if !strings.Contains(e.execCalls[0].sql, "original_code_owner_receipts") || !strings.Contains(e.execCalls[1].sql, "node_recovery_control_outbox") || !strings.Contains(e.execCalls[3].sql, "node_recovery_audit") {
				t.Fatal("proof/outbox/audit owning order changed")
			}
			var action recoveryAction
			actionRaw, ok := e.execCalls[1].args[6].([]byte)
			if !ok || json.Unmarshal(actionRaw, &action) != nil {
				t.Fatal("stored exact operator action absent")
			}
			proof, err := domain.DecodeOwnerProof(action.OwnerProof, f.claim.ExecutionID, 1, journal, sub.Request.Action)
			if err != nil || proof.EffectID != f.binding.DispatchActivation || proof.OwnerReceiptSHA256 != code.Digest(wire) {
				t.Fatal("exact owning seal/result lost", proof, err)
			}
			for _, call := range e.execCalls {
				if strings.Contains(call.sql, "sandbox_dispatches") || strings.Contains(call.sql, "command_outbox") || strings.Contains(call.sql, "input_bundles") {
					t.Fatal("reconciliation dispatched or minted a new turn")
				}
			}
		})
	}
}
func TestCodeOwnerProofRefusesMissingForeignStaleAndAmbiguousOwnerEvidence(t *testing.T) {
	for _, name := range []string{"missing original", "newer visit", "different immutable input", "owner unknown", "cancel before owner", "takeover after owner", "changed request", "partial call binding", "tampered seal", "missing cached bytes"} {
		t.Run(name, func(t *testing.T) {
			f := newOriginalCodeFixture(t)
			journal, _, _, wire := codeRecoveryFixture(t, f, "verified_no_effect")
			selector, _ := code.Canonical([]any{nil, nil})
			rows := codeRecoveryOwnerRows(f, journal, selector)
			rows = append(rows, codeRecoveryFenceRow(f), scriptedRow{err: pgx.ErrNoRows}, codeRecoveryFenceRow(f))
			owner := &codeOwnerEvidenceStub{wire: wire}
			switch name {
			case "missing original":
				rows[0] = scriptedRow{err: pgx.ErrNoRows}
			case "newer visit":
				rows[1] = scriptedRow{values: []any{strings.Repeat("b", 64), uint64(3)}}
			case "different immutable input":
				rows[2] = scriptedRow{values: []any{bytes.Repeat([]byte{9}, 32)}}
			case "owner unknown":
				owner.err = code.ErrRejected
			case "cancel before owner":
				rows[3] = scriptedRow{err: pgx.ErrNoRows}
			case "takeover after owner":
				after := f
				after.access.epoch++
				rows[5] = codeRecoveryFenceRow(after)
			case "changed request":
				owner.wire = bytes.Replace(wire, []byte(f.binding.RequestDigest), []byte(strings.Repeat("c", 64)), 1)
			case "partial call binding":
				owner.wire = bytes.Replace(wire, []byte(f.binding.DispatchActivation), []byte(strings.Repeat("d", 64)), 1)
			case "tampered seal":
				var receipt code.Receipt
				_ = json.Unmarshal(wire, &receipt)
				receipt.SealID = strings.Repeat("e", 64)
				owner.wire, _ = code.Canonical(receipt)
			case "missing cached bytes":
				rows[4] = scriptedRow{values: []any{[]byte{}, code.Digest(wire)}}
			}
			e := &scriptedExecutor{rowResults: rows, execTags: []pgconn.CommandTag{pgconn.NewCommandTag("INSERT 0 1")}}
			s := &recoveryTxStore{scriptedExecutor: e}
			r := originalCodeRepo(t, s, f)
			r.owner = owner
			var proof json.RawMessage
			err := s.WithinTx(t.Context(), pgx.TxOptions{}, func(tx sqlExecutor) error {
				var err error
				proof, err = r.VerifyNodeRecoveryEffect(t.Context(), tx, f.claim.ExecutionID, 1, journal, "reconcile")
				return err
			})
			if err == nil || s.committed || len(proof) != 0 || len(e.execCalls) != 0 {
				t.Fatal("unproven/stale owner evidence authorized", name, err, s.committed, e.execCalls)
			}
			if (name == "missing original" || name == "newer visit" || name == "different immutable input" || name == "cancel before owner" || name == "missing cached bytes") && owner.calls != 0 {
				t.Fatal("denied identity performed an owner call")
			}
		})
	}
}
func TestCodeNoEffectRequiresOriginalCompiledBrokerFactsAndNoObservedFrame(t *testing.T) {
	for _, name := range []string{"sealed no observed calls", "observed prepared frame", "dispatched call", "uncertain call", "pending toolkit child", "missing registration", "missing journal reader"} {
		t.Run(name, func(t *testing.T) {
			compiled := newOriginalCompiledCodeFixture(t)
			f := compiled.base
			journal, _, _, wire := codeRecoveryFixture(t, f, "verified_no_effect")
			rows := codeRecoveryOwnerRows(f, journal, compiled.selector)
			rows = append(rows, codeRecoveryFenceRow(f), scriptedRow{err: pgx.ErrNoRows}, codeRecoveryFenceRow(f))
			e := &scriptedExecutor{rowResults: rows, execTags: []pgconn.CommandTag{pgconn.NewCommandTag("INSERT 0 1")}}
			s := &recoveryTxStore{scriptedExecutor: e}
			r := originalCodeRepo(t, s, f)
			r.owner = &codeOwnerEvidenceStub{wire: wire}
			r.profiles = compiled.profiles
			r.snapshots = compiled.index
			facts := &codeBrokerFactsStub{facts: storage.CodeBrokerEffectFacts{Registered: true}, expectedFingerprint: compiled.binding.BasePreparedRequestSHA256}
			switch name {
			case "observed prepared frame":
				facts.facts.HasObservedCalls = true
			case "dispatched call":
				facts.facts.HasDispatchedEffects = true
			case "uncertain call":
				facts.facts.HasUncertainEffects = true
			case "pending toolkit child":
				facts.facts.HasPendingToolkitChildren = true
			case "missing registration":
				facts.facts.Registered = false
			}
			if name != "missing journal reader" {
				r.broker = facts
			}
			err := s.WithinTx(t.Context(), pgx.TxOptions{}, func(tx sqlExecutor) error {
				_, err := r.VerifyNodeRecoveryEffect(t.Context(), tx, f.claim.ExecutionID, 1, journal, "reconcile")
				return err
			})
			if name == "sealed no observed calls" {
				if err != nil || !s.committed || facts.calls != 2 || len(e.execCalls) != 1 {
					t.Fatal("exact original compiled no-effect facts refused", err, s.committed, facts.calls)
				}
				return
			}
			if err == nil || s.committed || len(e.execCalls) != 0 {
				t.Fatal("observed/unknown platform effect granted no-effect", err, s.committed, e.execCalls)
			}
		})
	}
}
