package control

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"strconv"
	"strings"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const sandboxGrantDomain = "elitea.sandbox.job-grant.ed25519.v1\x00"

// SandboxGrantIssuer snapshots the signing key and exact supervisor audiences.
// It is optional: an unconfigured server cannot issue sandbox grants.
type SandboxGrantIssuer struct {
	keyID     string
	key       ed25519.PrivateKey
	audiences map[string]struct{}
	now       func() time.Time
}

func NewSandboxGrantIssuer(keyID string, key ed25519.PrivateKey, audiences []string, now func() time.Time) (*SandboxGrantIssuer, error) {
	if !sandboxIdentity(keyID) || len(key) != ed25519.PrivateKeySize || len(audiences) == 0 || len(audiences) > 16 || now == nil {
		return nil, errors.New("sandbox grant signer configuration is invalid")
	}
	allowed := make(map[string]struct{}, len(audiences))
	for _, audience := range audiences {
		if !sandboxIdentity(audience) {
			return nil, errors.New("sandbox grant audience is invalid")
		}
		allowed[audience] = struct{}{}
	}
	return &SandboxGrantIssuer{keyID: keyID, key: append(ed25519.PrivateKey(nil), key...), audiences: allowed, now: now}, nil
}

func sandboxIdentity(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\r\n\x00")
}

func (s *Server) AuthorizeSandboxJob(ctx context.Context, request *runtimev1.AuthorizeSandboxJobRequestV1) (*runtimev1.AuthorizeSandboxJobResponseV1, error) {
	issuer := s.config.SandboxGrants
	if issuer == nil {
		return nil, status.Error(codes.Unimplemented, "Sandbox admission is not enabled.")
	}
	if request == nil || hasUnknownFields(request.ProtoReflect()) || request.GetFence() == nil || request.GetIdentity() == nil || !sandboxIdentity(request.GetActivationId()) || len(request.GetRequestDigest()) != 32 {
		return nil, status.Error(codes.InvalidArgument, "The sandbox authorization request is malformed.")
	}
	if _, ok := issuer.audiences[request.GetAudience()]; !ok {
		return nil, status.Error(codes.PermissionDenied, "The sandbox supervisor is not authorized.")
	}
	peer, err := s.authorizer.AuthorizeWorkload(ctx, request.GetFence().GetWorkloadSessionId(), request.GetFence().GetProducerId())
	if err != nil || !sandboxIdentity(peer) {
		return nil, status.Error(codes.Unauthenticated, "The workload session is not accepted.")
	}
	fence, err := fenceDomain(request.GetIdentity(), request.GetFence(), peer)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "The execution fence is malformed.")
	}
	desired, err := s.claims.ObserveDesiredState(ctx, fence)
	if err != nil || (desired != runtimedomain.DesiredRunning && !(request.GetCancelOnly() && desired == runtimedomain.DesiredCancelled)) {
		return nil, status.Error(codes.PermissionDenied, "The execution is no longer authorized to start sandbox work.")
	}
	command, err := s.verifier.Verify(ctx, request.GetSignedCommand())
	if err != nil || command == nil || command.GetCommandId() != fence.CommandID || command.GetExecutionId() != fence.ExecutionID || command.GetGeneration() != fence.Generation || command.GetTenantId() != request.GetIdentity().GetTenantId() || command.GetResourceProjectId() != request.GetIdentity().GetResourceProjectId() {
		return nil, status.Error(codes.PermissionDenied, "The sandbox request does not match its signed execution scope.")
	}
	if command.GetCommandType() != runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_APPLICATION && command.GetCommandType() != runtimev1.WorkerCommandTypeV1_WORKER_COMMAND_TYPE_V1_AGENT_EXECUTE_ADHOC {
		return nil, status.Error(codes.PermissionDenied, "The execution capability does not permit sandbox work.")
	}
	project, err := strconv.ParseInt(command.GetResourceProjectId(), 10, 32)
	if err != nil || project <= 0 || !sandboxIdentity(command.GetTenantId()) || !sandboxIdentity(fence.ExecutionID) {
		return nil, status.Error(codes.PermissionDenied, "The sandbox execution scope is invalid.")
	}
	revision := uint32(1)
	if request.GetCancelOnly() {
		revision = 2
	}
	now := issuer.now().UTC()
	claims := &runtimev1.SandboxJobGrantClaimsV1{
		Revision: revision, CancelOnly: request.GetCancelOnly(), TenantId: command.GetTenantId(), ProjectId: int32(project), ExecutionId: fence.ExecutionID,
		ActivationId: request.GetActivationId(), RequestDigest: append([]byte(nil), request.GetRequestDigest()...),
		SubmitterWorkloadIdentity: peer, Audience: request.GetAudience(), IssuedAtUnixMillis: now.UnixMilli(),
		ExpiresAtUnixMillis: now.Add(30 * time.Second).UnixMilli(), Generation: fence.Generation,
	}
	exact, err := proto.MarshalOptions{Deterministic: true}.Marshal(claims)
	if err != nil || len(exact) > 4096 {
		return nil, status.Error(codes.Internal, "The sandbox authorization could not be encoded.")
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	input := make([]byte, len(sandboxGrantDomain)+8+len(exact))
	offset := copy(input, sandboxGrantDomain)
	binary.BigEndian.PutUint64(input[offset:offset+8], uint64(len(exact)))
	copy(input[offset+8:], exact)
	return &runtimev1.AuthorizeSandboxJobResponseV1{Grant: &runtimev1.SignedSandboxJobGrantV1{
		KeyId: issuer.keyID, ClaimsBytes: exact, Signature: ed25519.Sign(issuer.key, input),
	}}, nil
}
