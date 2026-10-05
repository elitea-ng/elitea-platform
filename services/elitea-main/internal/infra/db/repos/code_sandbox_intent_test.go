package repos

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	httpapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/httpaction"
	recoveryapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/noderecovery"
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	scope "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/proto"
)

type originalCodeFixture struct {
	source      scope.SourceDefinition
	claim       storage.ContentClaim
	access      codeAccess
	visit       originalCodeVisitRecord
	ref         code.OriginalVisitRef
	visitWire   []byte
	prepared    []byte
	request     code.VisitRequest
	intent      code.IntentRequest
	binding     code.Binding
	bindingWire []byte
}

func newOriginalCodeFixture(t *testing.T) originalCodeFixture {
	t.Helper()
	claim := recoveryClaim()
	claim.ClaimID = "23456789abcdef0123456789abcdef01"
	instructions := "entry_point: run\nstate:\n  count: int\n  answer: int\nnodes:\n  - id: run\n    type: code\n    language: python\n    code: '7'\n    input: []\n    output: [answer]\n    transition: END\n"
	configuration := []byte(`{"id":"run","type":"code","language":"python","code":{"type":"fixed","value":"7"},"input":[],"output":["answer"],"structured_output":false,"debug":false,"transition":"END"}`)
	sourceVersion, _ := json.Marshal(map[string]any{"instructions": instructions, "agent_type": "pipeline"})
	sourceWire, err := scope.NewSourceWire(7, 11, 17, 19, sourceVersion)
	if err != nil {
		t.Fatal(err)
	}
	source, err := scope.DecodeSourceWire(sourceWire.CanonicalBytes(), sourceVersion, sourceWire.Reference(), 7, 11)
	if err != nil {
		t.Fatal(err)
	}
	// Runtime instructions differ intentionally; only the Main captured source owns admission.
	application, _ := json.Marshal(map[string]any{"id": 17, "version_id": 19, "source_definition": source.Reference, "version_details": map[string]any{"instructions": "untrusted runtime decoration"}})
	input, err := proto.Marshal(&runtimev1.AgentExecutionInputV1{ThreadId: proto.String("original-public-thread"), Application: application})
	if err != nil {
		t.Fatal(err)
	}
	peer := "spiffe://elitea.test/worker/native"
	now := time.Unix(2000, 0).UTC()
	a := codeAccess{tenant: "7", project: 7, projection: 7, actor: "11", actorID: 11, response: recoverySubmission().ResponseMessageID, inputBundle: "original-bundle", peer: peer, attempt: 2, epoch: 3, input: input, digest: codeIntentDigestBytes(code.Digest(input)), now: now, lease: now.Add(20 * time.Second), deadline: now.Add(time.Minute), desired: "RUNNING", mode: "NONE"}
	yamlSHA := code.Digest([]byte(instructions))
	policy, err := storage.OriginalRootSavedCodePolicy(instructions, yamlSHA, "run")
	if err != nil {
		t.Fatal(err)
	}
	declaration, err := storage.MatchOriginalSavedCodeConfiguration(policy, string(configuration))
	if err != nil {
		t.Fatal(err)
	}
	prepared := []byte(`{"revision":1,"language":"python","source":"7","input":{"count":7,"input":"start"},"image_digest":"sha256:` + strings.Repeat("9", 64) + `","policy_revision":"code-v1","timeout_seconds":10}`)
	metadata, err := storage.MatchCodePrepared(declaration, configuration, prepared)
	if err != nil {
		t.Fatal(err)
	}
	v := originalCodeVisitRecord{Schema: "elitea.sandbox.original-code-visit-record.v1", ExecutionID: claim.ExecutionID, Generation: 1, Tenant: a.tenant, Project: a.project, Projection: a.projection, Actor: a.actor, InputSHA256: code.Digest(input), Activation: strings.Repeat("1", 64), Node: "run", Thread: httpapp.RootGraphThread(a.tenant, a.project, a.projection, "original-public-thread"), Step: 7, Attempt: 1, NodeDigest: hex.EncodeToString(declaration.ConfigurationDigest[:]), YAML: yamlSHA, Scope: json.RawMessage("null"), OwningSource: source.Reference, PreWorkspace: metadata}
	ref, visitWire, err := codeVisitRef(v)
	if err != nil {
		t.Fatal(err)
	}
	request := code.VisitRequest{Schema: "elitea.sandbox.original-code-visit-request.v1", ActivationID: v.Activation, NodeID: v.Node, GraphThread: v.Thread, Step: v.Step, Attempt: v.Attempt, NodeDigest: v.NodeDigest, OwningYAMLSHA256: v.YAML, ConfigurationBase64URL: base64.RawURLEncoding.EncodeToString(configuration), PreWorkspacePreparedBase64URL: base64.RawURLEncoding.EncodeToString(prepared), SavedChildScope: json.RawMessage("null")}
	// An omitted policy's original legacy dispatch need not equal logical attempt hash.
	dispatch := strings.Repeat("a", 64)
	intent := code.IntentRequest{Schema: "elitea.sandbox.original-code-intent-request.v1", OriginalVisit: ref, DispatchActivation: dispatch, RequestDigest: metadata.Fingerprint, SupervisorAudience: "spiffe://elitea/supervisor/one", PreparedBase64URL: base64.RawURLEncoding.EncodeToString(prepared)}
	binding := code.Binding{Schema: "elitea.sandbox.whole-code-binding.v1", Purpose: "whole_code_execute", ExecutionID: v.ExecutionID, OriginalGeneration: 1, ActivationID: v.Activation, NodeID: v.Node, GraphThread: v.Thread, Step: v.Step, Attempt: v.Attempt, DispatchActivation: dispatch, JobKey: code.JobKey(v.ExecutionID, dispatch), RequestDigest: metadata.Fingerprint, SupervisorAudience: intent.SupervisorAudience, NodeDigest: v.NodeDigest, Language: metadata.Language, PreparedSHA256: metadata.PreparedSHA256, SourceSHA256: metadata.SourceSHA256, InputSHA256: metadata.InputSHA256}
	bindingWire, _ := code.Canonical(binding)
	return originalCodeFixture{source, claim, a, v, ref, visitWire, prepared, request, intent, binding, bindingWire}
}
func originalCodeAccessRows(a codeAccess) []scriptedRow {
	row := scriptedRow{values: []any{a.tenant, a.project, a.projection, a.actor, a.response, a.inputBundle, a.peer, a.attempt, a.epoch, a.desired, a.mode, bytes.Clone(a.input), bytes.Clone(a.digest), a.now, a.lease, a.deadline}}
	return []scriptedRow{row, row}
}
func originalCodeRepo(t *testing.T, s *recoveryTxStore, f originalCodeFixture) *CodeIntentRepository {
	t.Helper()
	signer, err := storage.NewCodeOwnerGrantSigner("key-1", ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32)), "spiffe://elitea/main", []string{"spiffe://elitea/supervisor/one"})
	if err != nil {
		t.Fatal(err)
	}
	return &CodeIntentRepository{shared: s, signer: signer, sources: originalCodeSourceStub{source: f.source}, permissions: func(_ context.Context, tx sqlExecutor, selector recoveryapp.Selector, permission string) error {
		if !s.entered || tx != s.scriptedExecutor || selector.ProjectID != 7 || selector.ActorUserID != 11 || permission != "models.chat.messages.create" {
			t.Fatal("actor/claim authorization escaped owning transaction")
		}
		return nil
	}}
}
func TestOriginalCodeVisitCommitsIdentityWithoutExecutionGrant(t *testing.T) {
	f := newOriginalCodeFixture(t)
	for _, name := range []string{"new", "exact replay", "changed preparation", "changed declaration", "cancelled claim", "actor revoked", "visit budget"} {
		t.Run(name, func(t *testing.T) {
			request := f.request
			rows := originalCodeAccessRows(f.access)
			count := int64(0)
			if name == "changed preparation" {
				request.PreWorkspacePreparedBase64URL = base64.RawURLEncoding.EncodeToString(bytes.Replace(f.prepared, []byte(`"source":"7"`), []byte(`"source":"8"`), 1))
			}
			if name == "changed declaration" {
				request.NodeDigest = strings.Repeat("e", 64)
			}
			if name == "cancelled claim" {
				rows = []scriptedRow{{err: pgx.ErrNoRows}}
			}
			if name == "visit budget" {
				count = 4096
			}
			rows = append(rows, scriptedRow{values: []any{count}}, scriptedRow{values: []any{f.visitWire, []byte(f.ref.DigestSHA256)}})
			e := &scriptedExecutor{rowResults: rows, execTags: []pgconn.CommandTag{pgconn.NewCommandTag("INSERT 0 1")}}
			s := &recoveryTxStore{scriptedExecutor: e}
			repo := originalCodeRepo(t, s, f)
			if name == "actor revoked" {
				repo.permissions = func(context.Context, sqlExecutor, recoveryapp.Selector, string) error {
					return storage.ErrContentUnauthorized
				}
			}
			out, err := repo.RegisterOriginalCodeVisit(t.Context(), f.claim, request)
			if name != "new" && name != "exact replay" {
				if err == nil || s.committed || len(e.execCalls) != 0 {
					t.Fatal("denied preparation wrote authority", out, err, s.committed, e.execCalls)
				}
				return
			}
			if err != nil || !s.committed || out.OriginalVisit != f.ref || out.OriginalGeneration != 1 || out.ExecutionID != f.claim.ExecutionID || len(e.execCalls) != 1 || !strings.Contains(e.execCalls[0].sql, "original_code_visits") {
				t.Fatal(out, err, s.committed, e.execCalls)
			}
			if out.Schema != "elitea.sandbox.original-code-visit-response.v1" {
				t.Fatal("wrong phase response")
			}
			for _, call := range e.execCalls {
				if strings.Contains(call.sql, "command_outbox") || strings.Contains(call.sql, "original_code_intents") || strings.Contains(call.sql, "sandbox_dispatch") {
					t.Fatal("preparation minted execution authority")
				}
			}
		})
	}
}
func TestOriginalCodeFinalCASReplayConflictAndClaimTakeover(t *testing.T) {
	f := newOriginalCodeFixture(t)
	for _, name := range []string{"commit", "exact current replay", "conflicting final", "cancelled before commit", "replacement fence", "unknown supervisor"} {
		t.Run(name, func(t *testing.T) {
			request := f.intent
			rows := append(originalCodeAccessRows(f.access), scriptedRow{values: []any{f.visitWire, []byte(f.ref.DigestSHA256)}})
			selector, _ := code.Canonical([]any{request.CompiledBindingBase64URL, request.SelectedDescriptorSHA256})
			saved := f.bindingWire
			if name == "conflicting final" {
				saved = bytes.Replace(saved, []byte(f.binding.SourceSHA256), []byte(strings.Repeat("b", 64)), 1)
			}
			rows = append(rows, scriptedRow{values: []any{saved, selector}})
			after := originalCodeAccessRows(f.access)
			if name == "cancelled before commit" {
				after = []scriptedRow{{err: pgx.ErrNoRows}}
			}
			if name == "replacement fence" {
				a := f.access
				a.epoch++
				after = originalCodeAccessRows(a)
			}
			rows = append(rows, after...)
			if name == "unknown supervisor" {
				request.SupervisorAudience = "spiffe://foreign/supervisor"
			}
			e := &scriptedExecutor{rowResults: rows, execTags: []pgconn.CommandTag{pgconn.NewCommandTag("INSERT 0 1")}}
			s := &recoveryTxStore{scriptedExecutor: e}
			repo := originalCodeRepo(t, s, f)
			out, err := repo.RegisterOriginalCodeIntent(t.Context(), f.claim, request)
			if name != "commit" && name != "exact current replay" {
				if err == nil || s.committed || out.Schema != "" {
					t.Fatal("stale/conflicting scope signed", out, err, s.committed)
				}
				if name == "unknown supervisor" && (s.entered || len(e.execCalls) != 0) {
					t.Fatal("unconfigured audience poisoned immutable final intent")
				}
				return
			}
			if err != nil || !s.committed || len(e.execCalls) != 1 {
				t.Fatal(out, err, s.committed, e.execCalls)
			}
			raw, _ := code.DecodeBase64(out.ClaimsBase64URL, 8192)
			var claims code.IntentClaims
			if code.Decode(raw, &claims, 8192) != nil || claims.ClaimID != f.claim.ClaimID || claims.LeaseEpoch != f.access.epoch || claims.DispatchActivation != f.binding.DispatchActivation || claims.ActivationID != f.visit.Activation || claims.ExpiresAtMillis != f.access.lease.UnixMilli() {
				t.Fatal("final signature lost original or current identity")
			}
		})
	}
}
func TestOriginalCodeDispatchMigrationUsesSavedPolicyPresence(t *testing.T) {
	activation := strings.Repeat("1", 64)
	legacy := strings.Repeat("a", 64)
	explicit := storage.SavedCodeDeclaration{Recovery: json.RawMessage(`{"max_attempts":1}`)}
	if matchOriginalCodeDispatch(storage.SavedCodeDeclaration{}, activation, 1, legacy) != nil || matchOriginalCodeDispatch(storage.SavedCodeDeclaration{}, activation, 2, legacy) == nil || matchOriginalCodeDispatch(explicit, activation, 1, legacy) == nil || matchOriginalCodeDispatch(explicit, activation, 1, code.DispatchActivation(activation, 1)) != nil {
		t.Fatal("default Stop/explicit policy dispatch contract changed")
	}
}
func TestOriginalCodeResourceCallbackFencesCurrentWriterBeforeCommit(t *testing.T) {
	f := newOriginalCodeFixture(t)
	for _, name := range []string{"commit", "late cancel", "late takeover", "foreign generation", "unknown purpose"} {
		t.Run(name, func(t *testing.T) {
			claim := f.claim
			purpose := "code_debug"
			rows := append(originalCodeAccessRows(f.access), scriptedRow{values: []any{f.visitWire, []byte(f.ref.DigestSHA256)}})
			after := originalCodeAccessRows(f.access)
			if name == "late cancel" || name == "late takeover" {
				after = []scriptedRow{{err: pgx.ErrNoRows}}
			}
			rows = append(rows, after...)
			if name == "foreign generation" {
				claim.Generation = 2
			}
			if name == "unknown purpose" {
				purpose = "execute"
			}
			e := &scriptedExecutor{rowResults: rows, execTags: []pgconn.CommandTag{pgconn.NewCommandTag("INSERT 0 1")}}
			s := &recoveryTxStore{scriptedExecutor: e}
			r := originalCodeRepo(t, s, f)
			called := false
			err := r.WithOriginalCodeVisit(t.Context(), claim, f.ref, purpose, func(ctx context.Context, tx storage.CodeTransaction, visit storage.OriginalCodeVisit) error {
				called = true
				if visit.OwningSourceDefinition != f.source.Reference || visit.ExecutionID != f.claim.ExecutionID || visit.OriginalGeneration != 1 || visit.CurrentClaimID != f.claim.ClaimID || visit.SourceSHA256 != f.visit.PreWorkspace.SourceSHA256 {
					t.Fatal("original/current resource facts drifted")
				}
				_, err := tx.Exec(ctx, "INSERT INTO private_debug_reservation VALUES($1)", visit.Reference.VisitID)
				return err
			})
			if name == "commit" {
				if err != nil || !called || !s.committed {
					t.Fatal(err, called, s.committed)
				}
				return
			}
			if err == nil || s.committed {
				t.Fatal("stale/current writer resource commit survived", err, s.committed)
			}
			if (name == "foreign generation" || name == "unknown purpose") && called {
				t.Fatal("unauthorized resource callback ran")
			}
		})
	}
}

// This fake is a captured-source fixture only. The real source adapter has its
// own locked source-row/registered-family tests and never reads runtime decoration.
type originalCodeSourceStub struct{ source scope.SourceDefinition }

func (stub originalCodeSourceStub) readOriginalCodeSource(_ context.Context, _ sqlExecutor, execution string, generation uint64, access codeAccess, rawScope json.RawMessage, purpose, thread, node string) (scope.SourceDefinition, error) {
	if !originalCodePurpose(purpose) || execution == "" || generation == 0 || access.project != stub.source.ResourceProjectID || access.actorID != stub.source.ActorID || !bytes.Equal(bytes.TrimSpace(rawScope), []byte("null")) || node != "run" {
		return scope.SourceDefinition{}, code.ErrRejected
	}
	return stub.source, nil
}
func TestOriginalCodeVisitInputAdmissionUsesSavedGraphRatherThanSelectorCardinality(t *testing.T) {
	f := newOriginalCodeFixture(t)
	for _, name := range []string{"present business roots", "missing approved values", "messages smuggling", "undeclared root", "wrong declared type", "changed captured source"} {
		t.Run(name, func(t *testing.T) {
			request := f.request
			prepared := bytes.Clone(f.prepared)
			switch name {
			case "missing approved values":
				prepared = bytes.Replace(prepared, []byte(`"input":{"count":7,"input":"start"}`), []byte(`"input":{}`), 1)
			case "messages smuggling":
				prepared = bytes.Replace(prepared, []byte(`"input":{"count":7,"input":"start"}`), []byte(`"input":{"messages":[]}`), 1)
			case "undeclared root":
				prepared = bytes.Replace(prepared, []byte(`"input":{"count":7,"input":"start"}`), []byte(`"input":{"undeclared":7}`), 1)
			case "wrong declared type":
				prepared = bytes.Replace(prepared, []byte(`"input":{"count":7,"input":"start"}`), []byte(`"input":{"count":"7"}`), 1)
			}
			request.PreWorkspacePreparedBase64URL = base64.RawURLEncoding.EncodeToString(prepared)
			// Stage's expected row is regenerated only from this fixture's original saved
			// declaration; this is not a replacement for exported real Worker bytes.
			policy, _ := storage.OriginalRootSavedCodePolicy(f.source.Instructions, f.visit.YAML, "run")
			configuration, _ := code.DecodeBase64(request.ConfigurationBase64URL, 2*1024*1024)
			declaration, err := storage.MatchOriginalSavedCodeConfiguration(policy, string(configuration))
			if err != nil {
				t.Fatal(err)
			}
			newMetadata, err := storage.MatchCodePrepared(declaration, configuration, prepared)
			if err != nil {
				t.Fatal(err)
			}
			v := f.visit
			v.PreWorkspace = newMetadata
			ref, wire, _ := codeVisitRef(v)
			rows := append(originalCodeAccessRows(f.access), scriptedRow{values: []any{int64(0)}}, scriptedRow{values: []any{wire, []byte(ref.DigestSHA256)}})
			e := &scriptedExecutor{rowResults: rows, execTags: []pgconn.CommandTag{pgconn.NewCommandTag("INSERT 0 1")}}
			store := &recoveryTxStore{scriptedExecutor: e}
			repo := originalCodeRepo(t, store, f)
			if name == "changed captured source" {
				changed := f.source
				changed.Reference.SourceDefinitionSHA256 = strings.Repeat("e", 64)
				repo.sources = originalCodeSourceStub{source: changed}
			}
			out, err := repo.RegisterOriginalCodeVisit(t.Context(), f.claim, request)
			admitted := name == "present business roots" || name == "missing approved values"
			if admitted {
				if err != nil || !store.committed || out.OriginalVisit != ref {
					t.Fatal(out, err, store.committed)
				}
				return
			}
			if err == nil || store.committed || len(e.execCalls) != 0 {
				t.Fatal("invalid saved input/source reached immutable stage", out, err, store.committed, e.execCalls)
			}
		})
	}
}
