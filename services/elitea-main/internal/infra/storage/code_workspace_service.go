package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"math"
	"sort"
	"time"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	toolkitexecution "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
)

const CodeWorkspacePurpose = "code_workspace"

// One original visit owns one selected base and one immutable snapshot.
// The base is immutable row data. It is not a second acquisition key.
type CodeWorkspaceReceiptKey struct {
	ExecutionID        string
	Generation         uint64
	VisitID            string
	VisitDigestSHA256  string
	ActivationID       string
	BasePreparedSHA256 string
	SelectionSHA256    string
	PolicySHA256       string
}

type CodeWorkspaceReceipts interface {
	// Reserve and Commit run only inside the current original-visit transaction.
	Reserve(context.Context, CodeTransaction, OriginalCodeVisit, CodeWorkspaceReceiptKey) (string, bool, error)
	Commit(context.Context, CodeTransaction, OriginalCodeVisit, CodeWorkspaceReceiptKey, string) error
	Verify(context.Context, CodeTransaction, OriginalCodeVisit, CodeWorkspaceReceiptKey, string) error
}

// The original declaration supplies the selection. The caller cannot replace it.
type CodeWorkspaceResolveRequest struct {
	OriginalVisit             code.OriginalVisitRef `json:"original_visit"`
	SelectedPreparedBase64URL string                `json:"selected_pre_workspace_prepared_job_json_base64url"`
}

type CodeWorkspaceService struct {
	visits       OriginalCodeVisitConsumer
	toolkits     toolkitexecution.CurrentMCPToolkitReader
	settings     toolkitexecution.CurrentToolkitSettingsResolver
	capabilities *CodeRepositoryCapabilities
	store        *SandboxBundleStore
	receipts     CodeWorkspaceReceipts
	policy       CodeWorkspacePolicy
	slots        chan struct{}
}

func NewCodeWorkspaceService(visits OriginalCodeVisitConsumer, toolkits toolkitexecution.CurrentMCPToolkitReader,
	settings toolkitexecution.CurrentToolkitSettingsResolver, capabilities *CodeRepositoryCapabilities,
	store *SandboxBundleStore, receipts CodeWorkspaceReceipts, policy CodeWorkspacePolicy, capacity int) (*CodeWorkspaceService, error) {
	if visits == nil || toolkits == nil || settings == nil || capabilities == nil || store == nil || receipts == nil || policy.Validate() != nil || capacity < 1 || capacity > 16 {
		return nil, ErrCodeWorkspaceInvalid
	}
	return &CodeWorkspaceService{visits, toolkits, settings, capabilities, store, receipts, policy, make(chan struct{}, capacity)}, nil
}

// This digest identifies exact saved references before credential redemption.
func CodeWorkspaceToolkitReferenceSHA256(toolkit toolkitexecution.CurrentMCPToolkitSnapshot, references map[string]any) (string, error) {
	if toolkit.ID <= 0 || toolkit.Type == "" || toolkit.Name == "" || references == nil || toolkit.Meta == nil {
		return "", ErrCodeWorkspaceInvalid
	}
	canonical, err := sandboxJSON(struct {
		ID       int32          `json:"id"`
		Type     string         `json:"type"`
		Name     string         `json:"name"`
		Settings map[string]any `json:"settings"`
		Meta     map[string]any `json:"meta"`
	}{toolkit.ID, toolkit.Type, toolkit.Name, references, toolkit.Meta})
	if err != nil || len(canonical) > toolkitexecution.MaxCurrentReadToolSnapshotBytes {
		return "", ErrCodeWorkspaceInvalid
	}
	return workspaceIdentity("elitea.code.workspace-toolkit-reference.v1\x00", canonical), nil
}

func (s *CodeWorkspaceService) requestKey(visit OriginalCodeVisit, job CodePreparedRequest) (CodeWorkspaceReceiptKey, CodeWorkspaceSelection, error) {
	empty := CodeWorkspaceReceiptKey{}
	if s == nil || visit.Reference.Validate() != nil || visit.ExecutionID == "" || visit.OriginalGeneration == 0 || visit.ResourceProjectID <= 0 || visit.ResourceProjectID > math.MaxInt32 || visit.ActorID <= 0 || visit.ActorID > math.MaxInt32 || visit.TenantID == "" || !workspaceHex(visit.ActivationID, 64) || job.Workspace != nil {
		return empty, CodeWorkspaceSelection{}, ErrContentUnauthorized
	}
	if job.Language != visit.Language || workspaceContentSHA256([]byte(job.Source)) != visit.SourceSHA256 || workspaceContentSHA256(job.Input) != visit.InputSHA256 || job.ImageDigest != visit.ImageDigest || job.PolicyRevision != visit.PolicyRevision || job.TimeoutSeconds != visit.TimeoutSeconds {
		return empty, CodeWorkspaceSelection{}, ErrContentUnauthorized
	}
	var selection CodeWorkspaceSelection
	if len(visit.Declaration.Workspace) == 0 || bytes.Equal(visit.Declaration.Workspace, []byte("null")) || codePreparedDecode(visit.Declaration.Workspace, &selection) != nil {
		return empty, selection, ErrContentUnauthorized
	}
	if err := selection.Validate(s.policy); err != nil {
		return empty, selection, err
	}
	// Mutable repository mounts require a durable quota backend. This adapter has none.
	if selection.Mode != CodeWorkspaceRead {
		return empty, selection, ErrCodeWorkspaceWritableUnavailable
	}
	native, err := sandboxJSON(selection)
	if err != nil {
		return empty, selection, ErrCodeWorkspaceInvalid
	}
	policy, err := s.policy.SHA256()
	if err != nil {
		return empty, selection, err
	}
	key := CodeWorkspaceReceiptKey{visit.ExecutionID, visit.OriginalGeneration, visit.Reference.VisitID, visit.Reference.DigestSHA256, visit.ActivationID, job.PreparedSHA256, workspaceIdentity("elitea.code.workspace-selection.v1\x00", native), policy}
	return key, selection, nil
}

func (s *CodeWorkspaceService) Resolve(ctx context.Context, claim ContentClaim, request CodeWorkspaceResolveRequest) (*CodeWorkspaceManifest, error) {
	if s == nil || ctx == nil || claim.ExecutionID == "" || claim.Generation == 0 || request.OriginalVisit.Validate() != nil {
		return nil, ErrCodeWorkspaceInvalid
	}
	raw, err := decodeCodeWorkspacePrepared(request.SelectedPreparedBase64URL)
	if err != nil {
		return nil, err
	}
	job, err := ParseCodePreparedRequest(raw)
	if err != nil || job.Workspace != nil {
		return nil, ErrCodeWorkspaceInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return nil, ErrContentUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.policy.MaxAcquisitionSeconds)*time.Second)
	defer cancel()
	var visit OriginalCodeVisit
	var key CodeWorkspaceReceiptKey
	var selection CodeWorkspaceSelection
	var root string
	var ready bool
	// This callback performs metadata admission only. It releases locks before reads.
	err = s.visits.WithOriginalCodeVisit(ctx, claim, request.OriginalVisit, CodeWorkspacePurpose, func(ctx context.Context, tx CodeTransaction, original OriginalCodeVisit) error {
		var e error
		key, selection, e = s.requestKey(original, job)
		if e != nil {
			return e
		}
		root, ready, e = s.receipts.Reserve(ctx, tx, original, key)
		if e == nil {
			visit = original
		}
		return e
	})
	if err != nil {
		return nil, err
	}
	project, actor := int32(visit.ResourceProjectID), int32(visit.ActorID)
	toolkit, found, err := s.toolkits.GetCurrentMCPToolkit(ctx, project, actor, selection.ToolkitID)
	if err != nil {
		return nil, ErrCodeWorkspaceUnavailable
	}
	if !found || toolkit.ID != selection.ToolkitID {
		return nil, ErrContentUnauthorized
	}
	references, err := s.settings.Resolve(ctx, configurationapp.CurrentToolkitSettingsRequest{ToolkitType: toolkit.Type, Settings: toolkit.Settings, ProjectID: project, UserID: actor, Mode: configurationapp.CurrentToolkitSettingsReferenceMode})
	if err != nil {
		return nil, ErrContentUnauthorized
	}
	reference, err := CodeWorkspaceToolkitReferenceSHA256(toolkit, references)
	if err != nil || reference != selection.ToolkitReferenceSHA256 {
		return nil, ErrContentUnauthorized
	}
	capability, err := s.capabilities.capability(toolkit.Type)
	if err != nil {
		return nil, err
	}
	if err := capability.Preflight(ctx, references); err != nil {
		return nil, err
	}
	scope, err := NewSandboxBundleScope(visit.TenantID, project)
	if err != nil {
		return nil, err
	}
	if ready {
		manifest, err := s.store.OpenCodeWorkspace(ctx, scope, root, s.policy)
		if err != nil {
			return nil, err
		}
		if err := s.matchManifest(manifest, selection, key); err != nil {
			return nil, err
		}
		// Reading storage can take time. Recheck the current fence before returning it.
		err = s.visits.WithOriginalCodeVisit(ctx, claim, request.OriginalVisit, CodeWorkspacePurpose, func(ctx context.Context, tx CodeTransaction, current OriginalCodeVisit) error {
			currentKey, _, e := s.requestKey(current, job)
			if e != nil {
				return e
			}
			if currentKey != key {
				return ErrContentUnauthorized
			}
			return s.receipts.Verify(ctx, tx, current, key, root)
		})
		if err != nil {
			return nil, err
		}
		return manifest, nil
	}
	settings, err := s.settings.Resolve(ctx, configurationapp.CurrentToolkitSettingsRequest{ToolkitType: toolkit.Type, Settings: references, ProjectID: project, UserID: actor, Mode: configurationapp.CurrentToolkitSettingsClaimMode})
	if err != nil {
		return nil, ErrContentUnauthorized
	}
	files, err := capability.Acquire(ctx, settings, selection, s.policy)
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].File.Path < files[j].File.Path })
	entries := make([]CodeWorkspaceFile, len(files))
	for i, file := range files {
		entries[i] = file.File
	}
	manifest, err := NewCodeWorkspaceManifest(selection, s.policy, entries)
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		if err := s.store.PutFile(ctx, scope, manifest, "data/"+file.File.SHA256, bytes.NewReader(file.Content)); err != nil {
			return nil, err
		}
	}
	if err := s.store.Publish(ctx, scope, manifest); err != nil {
		return nil, err
	}
	// Immutable object publication grants no execution authority.
	err = s.visits.WithOriginalCodeVisit(ctx, claim, request.OriginalVisit, CodeWorkspacePurpose, func(ctx context.Context, tx CodeTransaction, current OriginalCodeVisit) error {
		currentKey, _, e := s.requestKey(current, job)
		if e != nil {
			return e
		}
		if currentKey != key {
			return ErrContentUnauthorized
		}
		return s.receipts.Commit(ctx, tx, current, key, manifest.Digest())
	})
	if err != nil {
		return nil, err
	}
	return manifest, nil
}

func (s *CodeWorkspaceService) matchManifest(manifest *CodeWorkspaceManifest, selection CodeWorkspaceSelection, key CodeWorkspaceReceiptKey) error {
	if manifest == nil {
		return ErrCodeWorkspaceInvalid
	}
	exact, err := sandboxJSON(manifest.Selection())
	expected, _ := sandboxJSON(selection)
	policy, e := manifest.Policy().SHA256()
	if err != nil || e != nil || !bytes.Equal(exact, expected) || policy != key.PolicySHA256 {
		return ErrCodeWorkspaceInvalid
	}
	return nil
}

// The final intent owner calls this under its existing current-claim transaction.
// It never reads objects or credentials while that transaction owns row locks.
func (s *CodeWorkspaceService) VerifyOriginalCodeWorkspace(ctx context.Context, tx CodeTransaction, claim ContentClaim, visit OriginalCodeVisit, job CodePreparedRequest) error {
	if job.Workspace == nil {
		return nil
	}
	if s == nil || tx == nil || ctx == nil || claim.ExecutionID != visit.ExecutionID {
		return ErrContentUnauthorized
	}
	return s.verifyWorkspaceVisit(ctx, tx, visit, job)
}

// The caller owns the original/current claim join. This helper grants no authority.
func (s *CodeWorkspaceService) verifyWorkspaceVisit(ctx context.Context, tx CodeTransaction, visit OriginalCodeVisit, job CodePreparedRequest) error {
	if s == nil || tx == nil || ctx == nil || job.Workspace == nil {
		return ErrContentUnauthorized
	}
	base, err := ParseCodePreparedRequest(job.PreWorkspaceBytes)
	if err != nil {
		return err
	}
	key, selection, err := s.requestKey(visit, base)
	if err != nil {
		return err
	}
	expected, _ := sandboxJSON(selection)
	actual, _ := sandboxJSON(job.Workspace.Selection)
	if job.Workspace.Revision != 1 || !bytes.Equal(expected, actual) || job.Workspace.PolicySHA256 != key.PolicySHA256 || !workspaceHex(job.Workspace.ManifestSHA256, 64) {
		return ErrContentUnauthorized
	}
	return s.receipts.Verify(ctx, tx, visit, key, job.Workspace.ManifestSHA256)
}

func decodeCodeWorkspacePrepared(value string) ([]byte, error) {
	if value == "" || len(value) > base64.RawURLEncoding.EncodedLen(1024*1024) {
		return nil, ErrCodeWorkspaceInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != value || len(raw) > 1024*1024 {
		return nil, ErrCodeWorkspaceInvalid
	}
	return raw, nil
}

func (s *SandboxBundleStore) OpenCodeWorkspace(ctx context.Context, scope SandboxBundleScope, root string, policy CodeWorkspacePolicy) (manifest *CodeWorkspaceManifest, result error) {
	if scope.key == "" || !workspaceHex(root, 64) || policy.Validate() != nil {
		return nil, ErrContentUnauthorized
	}
	release, err := s.admit(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	ref, err := NewPlatformObjectRef(sandboxBundleBucket, scope.key+"/workspaces/"+root+"/elitea-code-workspace.json")
	if err != nil {
		return nil, err
	}
	body, info, err := s.store.Get(ctx, ref, nil)
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, body.Close()) }()
	if info.Size < 0 || info.Size > int64(policy.MaxManifestBytes) {
		return nil, ErrContentRejected
	}
	native, err := io.ReadAll(io.LimitReader(sandboxContextReader{ctx, body}, int64(policy.MaxManifestBytes)+1))
	if err != nil || int64(len(native)) != info.Size {
		return nil, ErrContentRejected
	}
	return ParseCodeWorkspaceManifest(native, root, policy)
}

// Exact request decoding rejects null, unknown fields, and extra documents.
func ParseCodeWorkspaceResolveRequest(body []byte, policy CodeWorkspacePolicy) (CodeWorkspaceResolveRequest, error) {
	if policy.Validate() != nil || len(body) == 0 || len(body) > 2*1024*1024 || codePreparedTokens(body) != nil {
		return CodeWorkspaceResolveRequest{}, ErrCodeWorkspaceInvalid
	}
	var request CodeWorkspaceResolveRequest
	if codePreparedDecode(body, &request) != nil || request.OriginalVisit.Validate() != nil {
		return CodeWorkspaceResolveRequest{}, ErrCodeWorkspaceInvalid
	}
	raw, err := decodeCodeWorkspacePrepared(request.SelectedPreparedBase64URL)
	if err != nil {
		return CodeWorkspaceResolveRequest{}, err
	}
	parsed, err := ParseCodePreparedRequest(raw)
	if err != nil || parsed.Workspace != nil {
		return CodeWorkspaceResolveRequest{}, ErrCodeWorkspaceInvalid
	}
	canonical, err := sandboxJSON(request)
	if err != nil || !bytes.Equal(canonical, body) {
		return CodeWorkspaceResolveRequest{}, ErrCodeWorkspaceInvalid
	}
	return request, nil
}
