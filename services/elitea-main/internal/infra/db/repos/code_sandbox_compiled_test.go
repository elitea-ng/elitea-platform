package repos

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	scope "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
	runtime "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/proto"
)

type codeOriginalSnapshotIndex struct {
	candidate                 runtime.SnapshotCandidate
	original                  *runtime.SnapshotExecution
	err                       error
	originalCalls, readyCalls int
}

func (i *codeOriginalSnapshotIndex) OriginalExecution(context.Context, runtime.SnapshotScope, string, string, string) (runtime.SnapshotExecution, bool, error) {
	i.originalCalls++
	if i.original != nil {
		return *i.original, true, nil
	}
	return runtime.SnapshotExecution{}, false, nil
}
func (i *codeOriginalSnapshotIndex) Ready(context.Context, runtime.SnapshotScope, string) (runtime.SnapshotCandidate, error) {
	i.readyCalls++
	return i.candidate, i.err
}
func (i *codeOriginalSnapshotIndex) Candidate(context.Context, runtime.SnapshotScope, string, string, bool) (runtime.SnapshotCandidate, error) {
	return i.candidate, i.err
}
func (*codeOriginalSnapshotIndex) Reserve(context.Context, runtime.SnapshotCandidate) error {
	return code.ErrRejected
}
func (*codeOriginalSnapshotIndex) WithPublishing(context.Context, runtime.SnapshotCandidate, func() error) error {
	return code.ErrRejected
}
func (*codeOriginalSnapshotIndex) WithReady(context.Context, runtime.SnapshotScope, string, string, func(runtime.SnapshotCandidate) error) error {
	return code.ErrRejected
}
func (*codeOriginalSnapshotIndex) CommitReady(context.Context, runtime.SnapshotCandidate, func() error) error {
	return code.ErrRejected
}

type originalCodeBrokerVerifierStub struct{ calls int }

func (s *originalCodeBrokerVerifierStub) VerifyOriginalCodeBroker(_ context.Context, _ storage.CodeTransaction, claim storage.ContentClaim, original storage.OriginalCodeVisit, job storage.CodePreparedRequest) error {
	s.calls++
	if !original.Declaration.PlatformClient || job.Broker == nil || job.PolicyRevision != "cargo-broker-execute-v1" || original.ExecutionID != claim.ExecutionID || job.Fingerprint == job.PreparedSHA256 {
		return code.ErrRejected
	}
	return nil
}

type originalCompiledCodeFixture struct {
	base     originalCodeFixture
	final    []byte
	binding  runtime.RustSnapshotBinding
	selector []byte
	relation storage.OriginalCompiledCodeExecute
	index    *codeOriginalSnapshotIndex
	profiles *runtime.RustSnapshotProfiles
}

func newOriginalCompiledCodeFixture(t *testing.T) originalCompiledCodeFixture {
	t.Helper()
	f := newOriginalCodeFixture(t)
	instructions := "entry_point: run\nstate:\n  count: int\n  answer: int\nnodes:\n  - id: run\n    type: code\n    language: rust\n    code: 'fn main(){}'\n    platform_client: true\n    input: []\n    output: [answer]\n    transition: END\n"
	configuration := []byte(`{"id":"run","type":"code","language":"rust","code":{"type":"fixed","value":"fn main(){}"},"input":[],"output":["answer"],"structured_output":false,"debug":false,"platform_client":true,"transition":"END"}`)
	f.prepared = []byte(`{"revision":1,"language":"rust","source":"fn main(){}","input":{"count":7,"input":"start"},"image_digest":"sha256:` + strings.Repeat("9", 64) + `","policy_revision":"cargo-broker-execute-v1","timeout_seconds":10}`)
	sourceVersion, _ := json.Marshal(map[string]any{"instructions": instructions, "agent_type": "pipeline"})
	wire, err := scope.NewSourceWire(7, 11, 17, 19, sourceVersion)
	if err != nil {
		t.Fatal(err)
	}
	f.source, err = scope.DecodeSourceWire(wire.CanonicalBytes(), sourceVersion, wire.Reference(), 7, 11)
	if err != nil {
		t.Fatal(err)
	}
	application, _ := json.Marshal(map[string]any{"id": 17, "version_id": 19, "source_definition": f.source.Reference, "version_details": map[string]any{"instructions": "untrusted runtime decoration"}})
	f.access.input, err = proto.Marshal(&runtimev1.AgentExecutionInputV1{ThreadId: proto.String("original-public-thread"), Application: application})
	if err != nil {
		t.Fatal(err)
	}
	f.access.digest = codeIntentDigestBytes(code.Digest(f.access.input))
	f.visit.InputSHA256 = code.Digest(f.access.input)
	f.visit.YAML = code.Digest([]byte(instructions))
	f.visit.OwningSource = f.source.Reference
	f.visit.PlatformClient = true
	policy, err := storage.OriginalRootSavedCodePolicy(instructions, f.visit.YAML, "run")
	if err != nil {
		t.Fatal(err)
	}
	declaration, err := storage.MatchOriginalSavedCodeConfiguration(policy, string(configuration))
	if err != nil {
		t.Fatal(err)
	}
	f.visit.NodeDigest = hex.EncodeToString(declaration.ConfigurationDigest[:])
	f.visit.PreWorkspace, err = storage.MatchCodePrepared(declaration, configuration, f.prepared)
	if err != nil {
		t.Fatal(err)
	}
	f.ref, f.visitWire, err = codeVisitRef(f.visit)
	if err != nil {
		t.Fatal(err)
	}
	final := bytes.Replace(f.prepared, []byte(`"revision":1`), []byte(`"revision":5`), 1)
	final = append(bytes.TrimSuffix(final, []byte("}")), []byte(`,"platform_client":{"revision":1,"policy_sha256":"`+strings.Repeat("6", 64)+`","max_calls":4,"max_total_bytes":4096}}`)...)
	job, err := storage.ParseCodePreparedRequest(final)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../../domain/runtime/testdata/compiled-snapshot-v1/binding.json")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := runtime.ParseRustSnapshotBinding(raw)
	if err != nil {
		t.Fatal(err)
	}
	binding.TenantID = f.access.tenant
	binding.ProjectID = int32(f.access.project)
	binding.PolicyRevision = job.PolicyRevision
	binding.ExecutionImageDigest = job.ImageDigest
	binding.CompilationImageDigest = job.ImageDigest
	binding.SourceSHA256 = code.Digest([]byte(job.Source))
	binding.BasePreparedRequestSHA256 = job.Fingerprint
	key, err := binding.Key()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := runtime.SnapshotJSON(runtime.RustSnapshotDescriptor{Revision: 1, Binding: binding, SnapshotKeySHA256: key, ExecutableSHA256: code.Digest([]byte("fixture executable")), ExecutableBytes: 18})
	if err != nil {
		t.Fatal(err)
	}
	root := code.Digest(descriptor)
	request, err := runtime.SnapshotJobDigest("execute", binding, root)
	if err != nil {
		t.Fatal(err)
	}
	bindingJSON, _ := runtime.SnapshotJSON(binding)
	encoded := base64.RawURLEncoding.EncodeToString(bindingJSON)
	f.intent.OriginalVisit = f.ref
	f.intent.RequestDigest = request
	f.intent.PreparedBase64URL = base64.RawURLEncoding.EncodeToString(final)
	f.intent.CompiledBindingBase64URL = &encoded
	f.intent.SelectedDescriptorSHA256 = &root
	f.binding.NodeDigest = f.visit.NodeDigest
	f.binding.Language = "rust"
	f.binding.SourceSHA256 = code.Digest([]byte(job.Source))
	f.binding.InputSHA256 = code.Digest(job.Input)
	f.binding.PreparedSHA256 = job.PreparedSHA256
	f.binding.RequestDigest = request
	f.bindingWire, _ = code.Canonical(f.binding)
	selector, _ := code.Canonical([]any{encoded, root, job.DependencyBundleSHA256})
	profiles, err := runtime.NewRustSnapshotProfiles([]runtime.RustSnapshotProfile{{Binding: binding, DependencyBundleSHA256: job.DependencyBundleSHA256}})
	if err != nil {
		t.Fatal(err)
	}
	index := &codeOriginalSnapshotIndex{candidate: runtime.SnapshotCandidate{Scope: runtime.SnapshotScope{TenantID: binding.TenantID, ProjectID: binding.ProjectID}, Key: key, Root: root, DescriptorJSON: descriptor}}
	return originalCompiledCodeFixture{base: f, final: final, binding: binding, selector: selector, relation: storage.OriginalCompiledCodeExecute{Binding: binding, DescriptorSHA256: root, SnapshotKeySHA256: key, DependencyBundleSHA256: job.DependencyBundleSHA256}, index: index, profiles: profiles}
}
func TestOriginalCompiledCodeFinalBindingKeepsBrokerAndExactSelector(t *testing.T) {
	for _, name := range []string{"Ready final", "original execution after cache expiry", "changed prepared fingerprint", "changed measured profile", "foreign descriptor", "cancel after lookup"} {
		t.Run(name, func(t *testing.T) {
			f := newOriginalCompiledCodeFixture(t)
			base := f.base
			request := base.intent
			if name == "original execution after cache expiry" {
				f.index.original = &runtime.SnapshotExecution{Key: f.relation.SnapshotKeySHA256, Root: f.relation.DescriptorSHA256, DescriptorJSON: f.index.candidate.DescriptorJSON}
				f.index.err = runtime.ErrSnapshotUnavailable
			}
			if name == "changed prepared fingerprint" {
				request.PreparedBase64URL = base64.RawURLEncoding.EncodeToString(bytes.Replace(f.final, []byte(`"count":7`), []byte(`"count":8`), 1))
			}
			if name == "changed measured profile" {
				binding := f.binding
				binding.AdapterSHA256 = strings.Repeat("a", 64)
				wire, _ := runtime.SnapshotJSON(binding)
				value := base64.RawURLEncoding.EncodeToString(wire)
				request.CompiledBindingBase64URL = &value
			}
			if name == "foreign descriptor" {
				root := strings.Repeat("a", 64)
				request.SelectedDescriptorSHA256 = &root
			}
			rows := append(originalCodeAccessRows(base.access), scriptedRow{values: []any{base.visitWire, []byte(base.ref.DigestSHA256)}}, scriptedRow{values: []any{base.bindingWire, f.selector}})
			after := originalCodeAccessRows(base.access)
			if name == "cancel after lookup" {
				after = []scriptedRow{{err: pgx.ErrNoRows}}
			}
			rows = append(rows, after...)
			e := &scriptedExecutor{rowResults: rows, execTags: []pgconn.CommandTag{pgconn.NewCommandTag("INSERT 0 1")}}
			s := &recoveryTxStore{scriptedExecutor: e}
			r := originalCodeRepo(t, s, base)
			r.snapshots = f.index
			r.profiles = f.profiles
			broker := &originalCodeBrokerVerifierStub{}
			r.broker = broker
			result, err := r.RegisterOriginalCodeIntent(t.Context(), base.claim, request)
			positive := name == "Ready final" || name == "original execution after cache expiry"
			if !positive {
				if err == nil || s.committed || result.Schema != "" {
					t.Fatal("forged/late cancelled final signed", result, err, s.committed)
				}
				return
			}
			if err != nil || !s.committed || broker.calls != 1 || f.index.originalCalls != 1 {
				t.Fatal(result, err, s.committed, broker.calls)
			}
			if name == "original execution after cache expiry" && f.index.readyCalls != 0 {
				t.Fatal("original descriptor replaced by current cache")
			}
			claimsRaw, _ := code.DecodeBase64(result.ClaimsBase64URL, 8192)
			var claims code.IntentClaims
			if code.Decode(claimsRaw, &claims, 8192) != nil || claims.RequestDigest != base.binding.RequestDigest || claims.RequestDigest == base.visit.PreWorkspace.Fingerprint || claims.PreparedSHA256 != base.binding.PreparedSHA256 {
				t.Fatal("compiled/raw/plain identities conflated")
			}
		})
	}
}
func TestCodePlatformStepDerivesOriginalCompiledDigestAndFencesCallback(t *testing.T) {
	for _, name := range []string{"compiled current", "same generation replacement", "wrong body fingerprint", "foreign dispatch", "cancel after callback"} {
		t.Run(name, func(t *testing.T) {
			f := newOriginalCompiledCodeFixture(t)
			base := f.base
			claim := base.claim
			access := base.access
			fingerprint := f.binding.BasePreparedRequestSHA256
			dispatch := base.binding.DispatchActivation
			if name == "same generation replacement" {
				claim.ClaimID = "3456789abcdef0123456789abcdef012"
				access.attempt++
				access.epoch++
			}
			if name == "wrong body fingerprint" {
				fingerprint = base.binding.PreparedSHA256
			}
			if name == "foreign dispatch" {
				dispatch = strings.Repeat("d", 64)
			}
			rows := append(originalCodeAccessRows(access), scriptedRow{values: []any{base.bindingWire}}, scriptedRow{values: []any{base.ref.VisitID, base.ref.DigestSHA256, base.bindingWire, code.Digest(base.bindingWire), f.selector}}, scriptedRow{values: []any{base.visitWire, []byte(base.ref.DigestSHA256)}})
			after := originalCodeAccessRows(access)
			if name == "cancel after callback" {
				after = []scriptedRow{{err: pgx.ErrNoRows}}
			}
			rows = append(rows, after...)
			e := &scriptedExecutor{rowResults: rows, execTags: []pgconn.CommandTag{pgconn.NewCommandTag("INSERT 0 1")}}
			s := &recoveryTxStore{scriptedExecutor: e}
			r := originalCodeRepo(t, s, base)
			r.snapshots = f.index
			r.profiles = f.profiles
			called := false
			err := r.WithRegisteredCodePlatformIntent(t.Context(), claim, dispatch, fingerprint, func(ctx context.Context, tx storage.CodeTransaction, intent storage.RegisteredCodeIntent) error {
				called = true
				if !intent.Compiled || intent.CompiledExecute == nil || *intent.CompiledExecute != f.relation || intent.PreparedFingerprint != fingerprint || intent.Binding.RequestDigest == fingerprint || intent.Binding.PreparedSHA256 != code.Digest(f.final) || intent.Access.ClaimID != claim.ClaimID || intent.Access.LeaseEpoch != access.epoch {
					t.Fatal("registered original selector/current authority missing", intent)
				}
				_, err := tx.Exec(ctx, "INSERT INTO private_broker_registration VALUES($1)", intent.Binding.DispatchActivation)
				return err
			})
			if name == "compiled current" || name == "same generation replacement" {
				if err != nil || !called || !s.committed {
					t.Fatal(err, called, s.committed)
				}
				return
			}
			if err == nil || s.committed {
				t.Fatal("stale/foreign platform selector committed", err, s.committed)
			}
			if name != "cancel after callback" && called {
				t.Fatal("selector mismatch reached broker")
			}
		})
	}
}
