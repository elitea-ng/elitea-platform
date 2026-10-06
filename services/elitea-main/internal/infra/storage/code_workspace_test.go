package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"io"
	"os"
	"strings"
	"testing"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	toolkitexecution "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
)

func workspaceSelectionFixture() CodeWorkspaceSelection {
	return CodeWorkspaceSelection{12, strings.Repeat("1", 64), "123", strings.Repeat("2", 40), CodeWorkspaceRead, []string{"src"}}
}
func workspaceFilesFixture() []CodeWorkspaceFile {
	digest := sha256.Sum256([]byte("hello"))
	return []CodeWorkspaceFile{{"src/greeting.txt", 5, hex.EncodeToString(digest[:]), false}}
}

func TestCodeWorkspaceCrossLanguageManifestAndPolicyIdentity(t *testing.T) {
	native, err := os.ReadFile("../../../../../libs/proto/elitea/runtime/v1/code_workspace_manifest_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var record workspaceRecord
	if err := json.Unmarshal(native, &record); err != nil {
		t.Fatal(err)
	}
	manifest, err := ParseCodeWorkspaceManifest(native, record.Root, DefaultCodeWorkspacePolicy())
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := NewCodeWorkspaceManifest(workspaceSelectionFixture(), DefaultCodeWorkspacePolicy(), workspaceFilesFixture())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(manifest.Bytes(), rebuilt.Bytes()) {
		t.Fatal("Go and Rust fixture bytes differ")
	}
	copy := manifest.Selection()
	copy.Include[0] = "mutated"
	files := manifest.Files()
	files[0].Path = "mutated"
	if manifest.Selection().Include[0] != "src" || manifest.Files()[0].Path != "src/greeting.txt" {
		t.Fatal("manifest reference escaped")
	}
}

func TestCodeWorkspaceRejectsUnsafePathsUnknownDuplicateFieldsAndPolicyBounds(t *testing.T) {
	policy := DefaultCodeWorkspacePolicy()
	for _, path := range []string{"../escape", "/root", "src\\file", "src/../file", "src//file", ".git/config", "src/.ELITEA-platform/key", "src/file:alternate"} {
		if err := workspacePath(path, policy); err == nil {
			t.Fatalf("accepted %s", path)
		}
	}
	manifest, err := NewCodeWorkspaceManifest(workspaceSelectionFixture(), policy, workspaceFilesFixture())
	if err != nil {
		t.Fatal(err)
	}
	for _, native := range [][]byte{
		bytes.Replace(manifest.Bytes(), []byte(`"revision":1,`), []byte(`"revision":1,"revision":1,`), 1),
		bytes.Replace(manifest.Bytes(), []byte(`"selection":{`), []byte(`"unexpected":true,"selection":{`), 1),
		bytes.Replace(manifest.Bytes(), []byte(`"mode":"read"`), []byte(`"mode":"readwrite"`), 1),
	} {
		if _, err := ParseCodeWorkspaceManifest(native, manifest.Digest(), policy); err == nil {
			t.Fatal("accepted changed immutable manifest")
		}
	}
	policy.MaxFileBytes = 4
	_, err = NewCodeWorkspaceManifest(workspaceSelectionFixture(), policy, workspaceFilesFixture())
	var bounds *CodeWorkspaceBoundsError
	if !errors.As(err, &bounds) || bounds.Bound != "max_file_bytes" || bounds.Limit != 4 {
		t.Fatalf("missing safe actionable bound: %v", err)
	}
}

type workspaceTestObjects struct {
	ObjectStore
	files  map[string][]byte
	writes int
	reads  int
	onRead func()
}

func (s *workspaceTestObjects) Put(_ context.Context, ref ObjectRef, body io.Reader, options PutOptions) (ObjectInfo, error) {
	b, err := io.ReadAll(body)
	if err != nil {
		return ObjectInfo{}, err
	}
	s.files[ref.Key()] = b
	s.writes++
	return ObjectInfo{Size: int64(len(b))}, nil
}
func (s *workspaceTestObjects) Get(_ context.Context, ref ObjectRef, _ *ByteRange) (io.ReadCloser, ObjectInfo, error) {
	s.reads++
	if s.onRead != nil {
		s.onRead()
	}
	b, ok := s.files[ref.Key()]
	if !ok {
		return nil, ObjectInfo{}, ErrContentNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), ObjectInfo{Size: int64(len(b))}, nil
}

type workspaceTestTransaction struct{}

func (*workspaceTestTransaction) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	panic("fixture receipt never calls SQL")
}
func (*workspaceTestTransaction) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("fixture receipt never calls SQL")
}

type workspaceTestAuthorizer struct {
	err      error
	calls    int
	refuseAt int
	active   bool
	visit    OriginalCodeVisit
}

func (a *workspaceTestAuthorizer) WithOriginalCodeVisit(ctx context.Context, claim ContentClaim, ref code.OriginalVisitRef, purpose string, apply func(context.Context, CodeTransaction, OriginalCodeVisit) error) error {
	a.calls++
	if a.err != nil {
		return a.err
	}
	if a.refuseAt > 0 && a.calls == a.refuseAt {
		return ErrContentUnauthorized
	}
	if purpose != CodeWorkspacePurpose || ref != a.visit.Reference || claim.ExecutionID != a.visit.ExecutionID {
		return ErrContentUnauthorized
	}
	if a.active {
		panic("nested transaction")
	}
	a.active = true
	defer func() { a.active = false }()
	return apply(ctx, &workspaceTestTransaction{}, a.visit)
}

type workspaceTestToolkit struct {
	row   toolkitexecution.CurrentMCPToolkitSnapshot
	calls int
}

func (r *workspaceTestToolkit) GetCurrentMCPToolkit(_ context.Context, p, a, id int32) (toolkitexecution.CurrentMCPToolkitSnapshot, bool, error) {
	r.calls++
	if p != 7 || a != 9 || id != 12 {
		return toolkitexecution.CurrentMCPToolkitSnapshot{}, false, nil
	}
	return r.row, true, nil
}

type workspaceTestSettings struct {
	references  map[string]any
	redemptions int
}

func (s *workspaceTestSettings) Resolve(_ context.Context, r configurationapp.CurrentToolkitSettingsRequest) (map[string]any, error) {
	if r.ProjectID != 7 || r.UserID != 9 {
		return nil, ErrContentUnauthorized
	}
	if r.Mode == configurationapp.CurrentToolkitSettingsClaimMode {
		s.redemptions++
	}
	return s.references, nil
}

type workspaceTestCapability struct {
	guard func()
	calls int
	files []CodeRepositoryFile
}

func (*workspaceTestCapability) Preflight(context.Context, map[string]any) error { return nil }
func (c *workspaceTestCapability) Acquire(context.Context, map[string]any, CodeWorkspaceSelection, CodeWorkspacePolicy) ([]CodeRepositoryFile, error) {
	c.calls++
	if c.guard != nil {
		c.guard()
	}
	return c.files, nil
}

type workspaceTestReceipts struct {
	key      CodeWorkspaceReceiptKey
	reserved bool
	root     string
	commits  int
	refuse   bool
}

func (r *workspaceTestReceipts) Reserve(_ context.Context, _ CodeTransaction, _ OriginalCodeVisit, key CodeWorkspaceReceiptKey) (string, bool, error) {
	if r.reserved && r.key != key {
		return "", false, ErrContentUnauthorized
	}
	r.key = key
	r.reserved = true
	return r.root, r.root != "", nil
}
func (r *workspaceTestReceipts) Commit(_ context.Context, _ CodeTransaction, _ OriginalCodeVisit, key CodeWorkspaceReceiptKey, root string) error {
	if r.refuse {
		return ErrContentUnauthorized
	}
	if !r.reserved || r.key != key || r.root != "" && r.root != root {
		return ErrContentUnauthorized
	}
	r.root = root
	r.commits++
	return nil
}
func (r *workspaceTestReceipts) Verify(_ context.Context, _ CodeTransaction, _ OriginalCodeVisit, key CodeWorkspaceReceiptKey, root string) error {
	if r.refuse || !r.reserved || r.key != key || r.root == "" || r.root != root {
		return ErrContentUnauthorized
	}
	return nil
}

type workspaceTestHarness struct {
	service    *CodeWorkspaceService
	request    CodeWorkspaceResolveRequest
	claim      ContentClaim
	authorizer *workspaceTestAuthorizer
	toolkit    *workspaceTestToolkit
	settings   *workspaceTestSettings
	capability *workspaceTestCapability
	objects    *workspaceTestObjects
	receipts   *workspaceTestReceipts
}

func newWorkspaceTestHarness(t *testing.T) workspaceTestHarness {
	t.Helper()
	authorizer := &workspaceTestAuthorizer{}
	toolkit := &workspaceTestToolkit{row: toolkitexecution.CurrentMCPToolkitSnapshot{ID: 12, Type: "github", Name: "saved repository", Settings: map[string]any{}, Meta: map[string]any{}}}
	settings := &workspaceTestSettings{references: map[string]any{"selected_tools": []any{"read_file"}}}
	capability := &workspaceTestCapability{files: []CodeRepositoryFile{{workspaceFilesFixture()[0], []byte("hello")}}}
	objects := &workspaceTestObjects{files: map[string][]byte{}}
	receipts := &workspaceTestReceipts{}
	spool := t.TempDir()
	if err := os.Chmod(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := NewSandboxBundleStore(objects, spool, 2)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewCodeRepositoryCapabilities(map[string]CodeRepositoryCapability{"github": capability})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewCodeWorkspaceService(authorizer, toolkit, settings, registry, store, receipts, DefaultCodeWorkspacePolicy(), 2)
	if err != nil {
		t.Fatal(err)
	}
	selection := workspaceSelectionFixture()
	selection.ToolkitReferenceSHA256, err = CodeWorkspaceToolkitReferenceSHA256(toolkit.row, settings.references)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../../../../libs/proto/elitea/runtime/v1/code_prepared_workspace_legacy_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := ParseCodePreparedRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	originalWorkspace, err := sandboxJSON(selection)
	if err != nil {
		t.Fatal(err)
	}
	reference := code.OriginalVisitRef{VisitID: strings.Repeat("c", 64), Revision: 1, DigestSHA256: strings.Repeat("d", 64)}
	authorizer.visit = OriginalCodeVisit{TenantID: "fixture-tenant", ResourceProjectID: 7, ProjectionProjectID: 7, ActorID: 9, Reference: reference, ExecutionID: "original-execution", OriginalGeneration: 3, ActivationID: strings.Repeat("a", 64), NodeID: "code", GraphThread: "original-thread", Step: 2, Attempt: 1,
		SourceSHA256: workspaceContentSHA256([]byte(prepared.Source)), InputSHA256: workspaceContentSHA256(prepared.Input), Language: prepared.Language, ImageDigest: prepared.ImageDigest, PolicyRevision: prepared.PolicyRevision, TimeoutSeconds: prepared.TimeoutSeconds,
		Declaration: SavedCodeDeclaration{Workspace: originalWorkspace}}
	return workspaceTestHarness{service, CodeWorkspaceResolveRequest{reference, base64.RawURLEncoding.EncodeToString(raw)}, ContentClaim{ExecutionID: "original-execution", Generation: 3}, authorizer, toolkit, settings, capability, objects, receipts}
}
func TestCodeWorkspaceReplacementReadsOriginalSnapshotWithoutCredentialOrProviderReplay(t *testing.T) {
	h := newWorkspaceTestHarness(t)
	first, err := h.service.Resolve(context.Background(), h.claim, h.request)
	if err != nil {
		t.Fatal(err)
	}
	beforeWrites := h.objects.writes
	// The provider's current view changes after the original immutable receipt.
	h.capability.files = []CodeRepositoryFile{{workspaceFilesFixture()[0], []byte("newer")}}
	second, err := h.service.Resolve(context.Background(), h.claim, h.request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest() != second.Digest() || h.capability.calls != 1 || h.settings.redemptions != 1 || h.objects.writes != beforeWrites || h.receipts.commits != 1 {
		t.Fatal("replacement replayed workspace acquisition")
	}
}
func TestCodeWorkspaceStaleReferenceClaimAndChangedReceiptFailBeforeAcquisition(t *testing.T) {
	for _, kind := range []string{"reference", "claim", "receipt"} {
		t.Run(kind, func(t *testing.T) {
			h := newWorkspaceTestHarness(t)
			switch kind {
			case "reference":
				selection := workspaceSelectionFixture()
				selection.ToolkitReferenceSHA256 = strings.Repeat("f", 64)
				h.authorizer.visit.Declaration.Workspace, _ = sandboxJSON(selection)
			case "claim":
				h.authorizer.err = ErrContentUnauthorized
			case "receipt":
				h.receipts.root = strings.Repeat("d", 64)
				h.receipts.key = CodeWorkspaceReceiptKey{ExecutionID: "foreign-execution"}
				h.receipts.reserved = true
			}
			if _, err := h.service.Resolve(context.Background(), h.claim, h.request); err == nil {
				t.Fatal("accepted stale workspace authority")
			}
			if h.capability.calls != 0 || h.settings.redemptions != 0 || h.objects.writes != 0 {
				t.Fatal("failure crossed the acquisition boundary")
			}
		})
	}
}
func TestCodeWorkspaceFencedCommitFailureNeverReturnsAnExecutionWorkspace(t *testing.T) {
	h := newWorkspaceTestHarness(t)
	h.receipts.refuse = true
	manifest, err := h.service.Resolve(context.Background(), h.claim, h.request)
	if manifest != nil || !errors.Is(err, ErrContentUnauthorized) || h.receipts.root != "" {
		t.Fatal("stale writer published an executable workspace receipt")
	}
}

func TestCodeWorkspaceAcquisitionDoesNotHoldOriginalVisitLocks(t *testing.T) {
	h := newWorkspaceTestHarness(t)
	h.capability.guard = func() {
		if h.authorizer.active {
			t.Fatal("provider read holds original visit locks")
		}
	}
	if _, err := h.service.Resolve(context.Background(), h.claim, h.request); err != nil {
		t.Fatal(err)
	}
	if h.authorizer.calls != 2 {
		t.Fatal("acquisition omitted fresh fenced publication")
	}
}

func TestCodeWorkspaceFreshFenceLossAndProfileDriftNeverReturnReady(t *testing.T) {
	for _, kind := range []string{"fresh_fence", "image", "policy", "timeout", "source", "input", "writable"} {
		t.Run(kind, func(t *testing.T) {
			h := newWorkspaceTestHarness(t)
			switch kind {
			case "fresh_fence":
				h.authorizer.refuseAt = 2
			case "image":
				h.authorizer.visit.ImageDigest = "sha256:" + strings.Repeat("f", 64)
			case "policy":
				h.authorizer.visit.PolicyRevision = "changed"
			case "timeout":
				h.authorizer.visit.TimeoutSeconds++
			case "source":
				h.authorizer.visit.SourceSHA256 = strings.Repeat("f", 64)
			case "input":
				h.authorizer.visit.InputSHA256 = strings.Repeat("f", 64)
			case "writable":
				var selected CodeWorkspaceSelection
				_ = json.Unmarshal(h.authorizer.visit.Declaration.Workspace, &selected)
				selected.Mode = CodeWorkspaceReadwrite
				h.authorizer.visit.Declaration.Workspace, _ = sandboxJSON(selected)
			}
			manifest, err := h.service.Resolve(context.Background(), h.claim, h.request)
			if err == nil || manifest != nil || h.receipts.root != "" {
				t.Fatal("invalid original visit produced a ready snapshot")
			}
			if kind != "fresh_fence" && (h.capability.calls != 0 || h.settings.redemptions != 0 || h.objects.writes != 0) {
				t.Fatal("profile failure crossed acquisition boundary")
			}
		})
	}
}

func TestCodeWorkspaceSelectedDependencyBaseCannotChangeAfterReservation(t *testing.T) {
	h := newWorkspaceTestHarness(t)
	if _, err := h.service.Resolve(context.Background(), h.claim, h.request); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../../../../libs/proto/elitea/runtime/v1/code_prepared_workspace_python_v2.json")
	if err != nil {
		t.Fatal(err)
	}
	h.request.SelectedPreparedBase64URL = base64.RawURLEncoding.EncodeToString(raw)
	if _, err := h.service.Resolve(context.Background(), h.claim, h.request); !errors.Is(err, ErrContentUnauthorized) {
		t.Fatal("changed dependency base silently reacquired")
	}
	if h.capability.calls != 1 || h.settings.redemptions != 1 {
		t.Fatal("changed selected base crossed provider boundary")
	}
}

func TestCodeWorkspaceFinalIntentRequiresCompletedExactSnapshot(t *testing.T) {
	h := newWorkspaceTestHarness(t)
	raw, _ := decodeCodeWorkspacePrepared(h.request.SelectedPreparedBase64URL)
	base, err := ParseCodePreparedRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	key, selection, err := h.service.requestKey(h.authorizer.visit, base)
	if err != nil {
		t.Fatal(err)
	}
	job := base
	job.Workspace = &CodeWorkspaceBinding{Revision: 1, Selection: selection, ManifestSHA256: strings.Repeat("e", 64), PolicySHA256: key.PolicySHA256}
	job.PreWorkspaceBytes = raw
	tx := &workspaceTestTransaction{}
	if err := h.service.VerifyOriginalCodeWorkspace(context.Background(), tx, h.claim, h.authorizer.visit, job); !errors.Is(err, ErrContentUnauthorized) {
		t.Fatal("unfinished acquisition authorized final intent")
	}
	manifest, err := h.service.Resolve(context.Background(), h.claim, h.request)
	if err != nil {
		t.Fatal(err)
	}
	job.Workspace.ManifestSHA256 = manifest.Digest()
	if err := h.service.VerifyOriginalCodeWorkspace(context.Background(), tx, h.claim, h.authorizer.visit, job); err != nil {
		t.Fatal(err)
	}
	job.Workspace.PolicySHA256 = strings.Repeat("f", 64)
	if err := h.service.VerifyOriginalCodeWorkspace(context.Background(), tx, h.claim, h.authorizer.visit, job); !errors.Is(err, ErrContentUnauthorized) {
		t.Fatal("changed policy authorized final intent")
	}
}
