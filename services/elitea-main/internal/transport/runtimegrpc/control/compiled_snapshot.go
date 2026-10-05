package control

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	grpcpeer "google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func (s *Server) AuthorizeRustCompiledSnapshot(ctx context.Context, r *runtimev1.AuthorizeRustCompiledSnapshotRequestV1) (*runtimev1.AuthorizeRustCompiledSnapshotResponseV1, error) {
	issuer := s.config.SandboxGrants
	index := s.config.CompiledSnapshots
	profiles := s.config.CompiledProfiles
	if issuer == nil || index == nil || profiles == nil {
		return nil, status.Error(codes.Unimplemented, "Compiled snapshot reuse is not enabled.")
	}
	if r == nil || hasUnknownFields(r.ProtoReflect()) || r.Identity == nil || r.Fence == nil || !sandboxIdentity(r.ActivationId) || len(r.PreparedJobJson) == 0 || len(r.PreparedJobJson) > 1024*1024 || len(r.BindingJson) == 0 || len(r.BindingJson) > domain.SnapshotDescriptorLimit {
		return nil, status.Error(codes.InvalidArgument, "The compiled snapshot request is malformed.")
	}
	compile := runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_COMPILE
	publish := runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_PUBLISH
	read := runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_READ
	execute := runtimev1.RustCompiledSnapshotPurposeV1_RUST_COMPILED_SNAPSHOT_PURPOSE_V1_EXECUTE
	if r.Purpose != compile && r.Purpose != publish && r.Purpose != read && r.Purpose != execute || r.Purpose == publish && len(r.CompilationJobKey) != 32 || r.Purpose != publish && len(r.CompilationJobKey) != 0 || r.Purpose == execute && len(r.SelectedDescriptorSha256) != 32 || r.Purpose == read && len(r.SelectedDescriptorSha256) != 0 && len(r.SelectedDescriptorSha256) != 32 || (r.Purpose == compile || r.Purpose == publish) && len(r.SelectedDescriptorSha256) != 0 {
		return nil, status.Error(codes.InvalidArgument, "The compiled snapshot purpose is malformed.")
	}
	if _, ok := issuer.audiences[r.Audience]; !ok {
		return nil, status.Error(codes.PermissionDenied, "The supervisor is not authorized.")
	}
	peer, err := s.authorizer.AuthorizeWorkload(ctx, r.Fence.GetWorkloadSessionId(), r.Fence.GetProducerId())
	if err != nil || !sandboxIdentity(peer) {
		return nil, status.Error(codes.Unauthenticated, "The workload session is not accepted.")
	}
	fence, err := fenceDomain(r.Identity, r.Fence, peer)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "The execution fence is malformed.")
	}
	desired, err := s.claims.ObserveDesiredState(ctx, fence)
	if err != nil || desired != domain.DesiredRunning {
		return nil, status.Error(codes.PermissionDenied, "The active execution does not permit compiled snapshot work.")
	}
	command, err := s.verifier.Verify(ctx, r.SignedCommand)
	if err != nil || command == nil || command.GetCommandId() != fence.CommandID || command.GetExecutionId() != fence.ExecutionID || command.GetGeneration() != fence.Generation || command.GetTenantId() != r.Identity.GetTenantId() || command.GetResourceProjectId() != r.Identity.GetResourceProjectId() || (command.GetCommandType() != runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_APPLICATION && command.GetCommandType() != runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_ADHOC) {
		return nil, status.Error(codes.PermissionDenied, "The snapshot request does not match its signed execution scope.")
	}
	project, err := strconv.ParseInt(command.GetResourceProjectId(), 10, 32)
	if err != nil || project <= 0 {
		return nil, status.Error(codes.PermissionDenied, "The snapshot project is invalid.")
	}
	binding, err := domain.ParseRustSnapshotBinding(r.BindingJson)
	if err != nil || binding.TenantID != command.GetTenantId() || binding.ProjectID != int32(project) {
		return nil, status.Error(codes.PermissionDenied, "The snapshot binding scope is invalid.")
	}
	bundle, err := binding.MatchPrepared(r.PreparedJobJson)
	var extensions struct {
		Workspace json.RawMessage `json:"workspace"`
		Broker    json.RawMessage `json:"platform_client"`
	}
	_ = json.Unmarshal(r.PreparedJobJson, &extensions)
	var parsed storage.CodePreparedRequest
	var workspaceAccess *storage.CodeCompileVisitAccess
	if len(extensions.Workspace) > 0 || len(extensions.Broker) > 0 || r.OriginalCodeVisit != nil {
		var parseErr error
		parsed, parseErr = storage.ParseCodePreparedRequest(r.PreparedJobJson)
		if parseErr != nil {
			return nil, status.Error(codes.PermissionDenied, "The prepared Code request is invalid.")
		}
		bundle, err = storage.MatchCodeSnapshotPrepared(binding, r.PreparedJobJson)
		if err != nil || profiles.Validate(binding, bundle) != nil || parsed.Broker != nil && binding.PolicyRevision != "cargo-broker-execute-v1" {
			return nil, status.Error(codes.PermissionDenied, "The immutable snapshot profile is not accepted.")
		}

		if r.OriginalCodeVisit != nil && (r.Purpose != compile || parsed.Workspace == nil) {
			return nil, status.Error(codes.InvalidArgument, "The original visit is only admitted for workspace compilation.")
		}
		if parsed.Workspace != nil && r.Purpose == compile {
			if s.config.OriginalCodeWorkspaces == nil || r.OriginalCodeVisit == nil || hasUnknownFields(r.OriginalCodeVisit.ProtoReflect()) {
				return nil, status.Error(codes.PermissionDenied, "Workspace compilation requires its original visit.")
			}
			connection, ok := grpcpeer.FromContext(ctx)
			if !ok {
				return nil, status.Error(codes.Unauthenticated, "Verified workspace peer is required.")
			}
			tlsInfo, ok := connection.AuthInfo.(credentials.TLSInfo)
			if !ok || len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.PeerCertificates) == 0 {
				return nil, status.Error(codes.Unauthenticated, "Verified workspace peer is required.")
			}
			reference := code.OriginalVisitRef{VisitID: r.OriginalCodeVisit.VisitId, Revision: uint8(r.OriginalCodeVisit.Revision), DigestSHA256: r.OriginalCodeVisit.DigestSha256}
			if r.OriginalCodeVisit.Revision != 1 || reference.Validate() != nil {
				return nil, status.Error(codes.InvalidArgument, "The original visit is malformed.")
			}
			admitted, e := s.config.OriginalCodeWorkspaces.AuthorizeCodeWorkspaceCompile(ctx, tlsInfo.State.PeerCertificates[0], fence, reference, r.PreparedJobJson)
			if e != nil {
				return nil, status.Error(codes.PermissionDenied, "The original workspace is unavailable.")
			}
			workspaceAccess = &admitted
		}
	}
	if err != nil || profiles.Validate(binding, bundle) != nil {
		return nil, status.Error(codes.PermissionDenied, "The immutable snapshot profile is not accepted.")
	}
	key, _ := binding.Key()
	scope := domain.SnapshotScope{TenantID: binding.TenantID, ProjectID: binding.ProjectID}
	var c domain.SnapshotCandidate
	var digest string
	if r.Purpose == compile {
		digest, err = domain.SnapshotJobDigest("compile", binding, "")
	} else if r.Purpose == publish {
		job := hex.EncodeToString(r.CompilationJobKey)
		if job != domain.SnapshotActivationKey(fence.ExecutionID, r.ActivationId) {
			return nil, status.Error(codes.PermissionDenied, "Publication must use its original compilation activation.")
		}
		c, err = index.Candidate(ctx, scope, key, job, false)
		if err == nil {
			digest = c.CompilationRequestDigest
		}
	} else {
		var original domain.SnapshotExecution
		recovered := false
		if r.Purpose == execute {
			original, recovered, err = index.OriginalExecution(ctx, scope, key, domain.SnapshotActivationKey(fence.ExecutionID, r.ActivationId), hex.EncodeToString(r.SelectedDescriptorSha256))
			if recovered && err == nil {
				d, e := domain.ParseRustSnapshotDescriptor(original.DescriptorJSON, original.Root)
				if e != nil || d.Binding != binding {
					err = domain.ErrSnapshotConflict
				} else {
					c.Root = original.Root
					c.DescriptorJSON = original.DescriptorJSON
					digest, err = domain.SnapshotJobDigest("execute", binding, c.Root)
				}
			}
		}
		if !recovered && err == nil {
			c, err = index.Ready(ctx, scope, key)
		}
		if errors.Is(err, domain.ErrSnapshotMiss) && r.Purpose == read && len(r.SelectedDescriptorSha256) == 0 {
			return nil, status.Error(codes.NotFound, "No compiled snapshot is indexed for this exact request.")
		}
		if err == nil && len(r.SelectedDescriptorSha256) != 0 && hex.EncodeToString(r.SelectedDescriptorSha256) != c.Root {
			err = domain.ErrSnapshotConflict
		}
		if err == nil {
			digest, err = domain.SnapshotJobDigest("execute", binding, c.Root)
		}
	}
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "The selected compiled snapshot is unavailable or does not match its original provenance.")
	}
	if r.Purpose != compile {
		d, e := domain.ParseRustSnapshotDescriptor(c.DescriptorJSON, c.Root)
		if e != nil || d.Binding != binding {
			return nil, status.Error(codes.FailedPrecondition, "The indexed snapshot binding does not match this request.")
		}
	}
	now := issuer.now().UTC()
	claims := &runtimev1.RustCompiledSnapshotGrantClaimsV1{Revision: 4, TenantId: binding.TenantID, ProjectId: binding.ProjectID, ExecutionId: fence.ExecutionID, ActivationId: r.ActivationId, RequestDigest: snapshotClaimDigest(digest), SubmitterWorkloadIdentity: peer, Audience: r.Audience, IssuedAtUnixMillis: now.UnixMilli(), ExpiresAtUnixMillis: now.Add(30 * time.Second).UnixMilli(), Generation: fence.Generation, Purpose: r.Purpose, BasePreparedRequestSha256: snapshotClaimDigest(binding.BasePreparedRequestSHA256), SnapshotKeySha256: snapshotClaimDigest(key), DescriptorSha256: snapshotClaimDigest(c.Root)}
	if workspaceAccess != nil {
		if workspaceAccess.ExpiresAtUnixMillis <= now.UnixMilli() {
			return nil, status.Error(codes.PermissionDenied, "The original workspace claim expired.")
		}
		if workspaceAccess.ExpiresAtUnixMillis < claims.ExpiresAtUnixMillis {
			claims.ExpiresAtUnixMillis = workspaceAccess.ExpiresAtUnixMillis
		}
		claims.OriginalCodeVisitAccess = &runtimev1.OriginalCodeVisitAccessV1{OriginalVisit: &runtimev1.OriginalCodeVisitRefV1{VisitId: workspaceAccess.OriginalVisit.VisitID, Revision: uint64(workspaceAccess.OriginalVisit.Revision), DigestSha256: workspaceAccess.OriginalVisit.DigestSHA256}, ClaimId: workspaceAccess.ClaimID, ClaimAttempt: workspaceAccess.ClaimAttempt, LeaseEpoch: workspaceAccess.LeaseEpoch, FenceSha256: append([]byte(nil), workspaceAccess.FenceSHA256...)}
	}

	if r.Purpose == publish {
		claims.CompilationJobKey = snapshotClaimDigest(c.CompilationJobKey)
		claims.CompilationRuntimeId = c.CompilationRuntimeID
		claims.CompilationRequestDigest = snapshotClaimDigest(c.CompilationRequestDigest)
		claims.CompilationLeaseEpoch = c.CompilationLeaseEpoch
	}
	exact, err := proto.MarshalOptions{Deterministic: true}.Marshal(claims)
	if err != nil || len(exact) > 4096 {
		return nil, status.Error(codes.Internal, "The snapshot authority could not be encoded.")
	}
	if err = ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	input := make([]byte, len(sandboxGrantDomain)+8+len(exact))
	offset := copy(input, sandboxGrantDomain)
	binary.BigEndian.PutUint64(input[offset:offset+8], uint64(len(exact)))
	copy(input[offset+8:], exact)
	return &runtimev1.AuthorizeRustCompiledSnapshotResponseV1{Grant: &runtimev1.SignedSandboxJobGrantV1{KeyId: issuer.keyID, ClaimsBytes: exact, Signature: ed25519.Sign(issuer.key, input)}, DescriptorJson: append([]byte(nil), c.DescriptorJSON...)}, nil
}
func snapshotClaimDigest(value string) []byte { native, _ := hex.DecodeString(value); return native }
