package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
)

// CredentialRedeemer is the governed credential broker (#1118,
// *credentialbroker.Broker). The content server proves the claim binding and
// hands it the claimed run.
type CredentialRedeemer interface {
	Redeem(ctx context.Context, req credentialbroker.RedeemRequest) (credentialbroker.Result, error)
}

// CredentialRedemptionAuthorizer binds a redemption to the caller's live
// claim: the verified workload, claim id, fence, live workload session and a
// RUNNING job. It answers the claimed run with its stored security context
// (nil bytes when the run has none). The content authorizer implements it.
type CredentialRedemptionAuthorizer interface {
	AuthorizeCredentialRedemption(ctx context.Context, claim ContentClaim) (credentialbroker.ClaimedRun, error)
}

// AuthorizeCredentialRedemption is the claim binding of RedeemCredential.
// It uses the predicates every claim-bound content read uses, but requires
// RUNNING (a suspended run redeems nothing), and LEFT JOINs the security
// context so a legacy run reaches the broker's NOT_GOVERNED answer instead
// of reading as a rejected claim.
func (r *PostgresContentRepository) AuthorizeCredentialRedemption(ctx context.Context, claim ContentClaim) (credentialbroker.ClaimedRun, error) {
	workloadID, err := workloadIdentity(claim.PeerCertificate)
	if err != nil {
		return credentialbroker.ClaimedRun{}, ErrContentUnauthorized
	}
	run := credentialbroker.ClaimedRun{
		ExecutionID: claim.ExecutionID, Generation: claim.Generation, ClaimID: claim.ClaimID, WorkloadID: workloadID,
	}
	var digest []byte
	err = r.store.QueryRow(ctx, `
SELECT j.resource_project_id,
       j.actor_id,
       COALESCE(a.client_stream_id, ''),
       COALESCE(a.client_message_id, ''),
       sc.context_bytes,
       sc.digest
FROM elitea_runtime.execution_claims AS c
JOIN elitea_runtime.execution_jobs AS j
  ON j.execution_id = c.execution_id AND j.generation = c.generation
JOIN elitea_runtime.workload_sessions AS ws
  ON ws.workload_session_id = c.workload_session_id
 AND ws.workload_identity = c.workload_identity
 AND ws.producer_id = c.producer_id
LEFT JOIN elitea_runtime.agent_execution_jobs AS a
  ON a.execution_id = j.execution_id AND a.generation = j.generation
LEFT JOIN elitea_runtime.execution_security_contexts AS sc
  ON sc.execution_id = c.execution_id
WHERE c.claim_id = $1
  AND c.execution_id = $2
  AND c.generation = $3
  AND c.workload_identity = $4
  AND c.fence_token = $5
  AND c.released_at IS NULL
  AND c.lease_expires_at > clock_timestamp()
  AND ws.issued_at <= clock_timestamp()
  AND ws.expires_at > clock_timestamp()
  AND ws.revoked_at IS NULL
  AND j.desired_state = 'RUNNING'
  -- A node-recovery inspection claim reads; it never runs a tool. Only the
  -- agent lanes run governed toolkits (the worker refuses the toolkit lane).
  AND c.recovery_mode <> 'NODE_RECOVERY'
  AND j.capability_id IN ('agent.execute.application.v1', 'agent.execute.adhoc.v1')`,
		claim.ClaimID, claim.ExecutionID, claim.Generation, workloadID, claim.FenceToken,
	).Scan(&run.TenantProjectID, &run.ActorID, &run.ConversationUUID, &run.ResponseMessageID, &run.ContextBytes, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return credentialbroker.ClaimedRun{}, ErrContentUnauthorized
	}
	if err != nil {
		return credentialbroker.ClaimedRun{}, fmt.Errorf("authorize credential redemption: %w", err)
	}
	if run.ContextBytes != nil {
		if len(digest) != len(run.ContextDigest) {
			return credentialbroker.ClaimedRun{}, errors.New("authorize credential redemption: stored context digest is malformed")
		}
		copy(run.ContextDigest[:], digest)
	}
	return run, nil
}

// WithCredentialBroker enables RedeemCredential (#1118). It is set only with
// credential governance on; without it the RPC answers Unimplemented and no
// statement runs. It must be called before Register.
func (s *ContentServer) WithCredentialBroker(broker CredentialRedeemer) *ContentServer {
	if s != nil {
		s.credentialBroker = broker
	}
	return s
}

// WithGovernedPins makes GetEliteaClientToken read the claimed run's
// credential profile pin (#1118). A governed run's token response carries
// no secrets-header value: its toolkit credentials come only from
// RedeemCredential. It is set only while governed pins may exist.
func (s *ContentServer) WithGovernedPins(pins profile.PinReader) *ContentServer {
	if s != nil {
		s.governedPins = pins
	}
	return s
}

// RedeemCredential releases the material of one bound tool invocation of a
// governed run. Claim binding failures are gRPC statuses, checked before
// the broker runs, so no secret is read for them; a policy refusal is a
// typed denial in the response.
func (s *ContentServer) RedeemCredential(ctx context.Context, request *runtimev1.RedeemCredentialRequestV1) (*runtimev1.RedeemCredentialResponseV1, error) {
	if s.credentialBroker == nil {
		return nil, contentStatus(codes.Unimplemented)
	}
	release, err := acquireContentSlot(s.requests)
	if err != nil {
		return nil, err
	}
	defer release()
	claim, err := parseExecutionClaim(ctx, request.GetScope())
	if err != nil {
		return nil, contentStatus(codes.Unauthenticated)
	}
	invocation, digest, ok := redeemRequestFields(request)
	if !ok {
		return nil, contentStatus(codes.InvalidArgument)
	}
	authorizer, ok := s.authorizer.(CredentialRedemptionAuthorizer)
	if !ok {
		return nil, contentStatus(codes.Unimplemented)
	}
	run, err := authorizer.AuthorizeCredentialRedemption(ctx, claim)
	if errors.Is(err, ErrContentUnauthorized) {
		return nil, contentStatus(codes.PermissionDenied)
	}
	if err != nil {
		s.logger.WarnContext(ctx, "credential redemption claim binding unavailable",
			"execution_id", claim.ExecutionID, "generation", claim.Generation)
		return nil, contentStatus(codes.Unavailable)
	}
	result, err := s.credentialBroker.Redeem(ctx, credentialbroker.RedeemRequest{
		Run: run, Invocation: invocation, SecurityContextDigest: digest,
	})
	if errors.Is(err, credentialbroker.ErrInvalidRequest) {
		return nil, contentStatus(codes.InvalidArgument)
	}
	if err != nil {
		s.logger.WarnContext(ctx, "credential redemption unavailable",
			"execution_id", claim.ExecutionID, "generation", claim.Generation)
		return nil, contentStatus(codes.Unavailable)
	}
	if result.Denial != nil {
		return &runtimev1.RedeemCredentialResponseV1{Result: &runtimev1.RedeemCredentialResponseV1_Denial{
			Denial: &runtimev1.CredentialRedeemDenialV1{
				Code:                   string(result.Denial.Code),
				DecisionId:             result.Denial.DecisionID,
				AuthorizationReference: result.Denial.AuthorizationReference,
			},
		}}, nil
	}
	envelope := result.Envelope
	if envelope == nil {
		return nil, contentStatus(codes.Unavailable)
	}
	// The proto map holds strings: one unavoidable copy per value. The
	// broker's byte slices are zeroed as soon as the response is built.
	defer envelope.Destroy()
	material := make(map[string]string, len(envelope.Material))
	for key, value := range envelope.Material {
		material[key] = string(value)
	}
	return &runtimev1.RedeemCredentialResponseV1{Result: &runtimev1.RedeemCredentialResponseV1_Envelope{
		Envelope: &runtimev1.CredentialEnvelopeV1{
			RedemptionId:       envelope.RedemptionID,
			Kind:               envelopeKindProto(envelope.Kind),
			Material:           material,
			NotAfterUnixMillis: envelope.NotAfter.UnixMilli(),
			TargetIdentity:     envelope.TargetIdentity,
			Obligations:        envelope.Obligations,
			DecisionId:         envelope.DecisionID,
		},
	}}, nil
}

func redeemRequestFields(request *runtimev1.RedeemCredentialRequestV1) (credentialbroker.BoundToolInvocation, [32]byte, bool) {
	var digest [32]byte
	contextDigest := request.GetSecurityContextDigest()
	if contextDigest.GetAlgorithm() != runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256 || len(contextDigest.GetValue()) != len(digest) {
		return credentialbroker.BoundToolInvocation{}, digest, false
	}
	copy(digest[:], contextDigest.GetValue())
	message := request.GetInvocation()
	arguments := message.GetArgumentsDigest()
	if message == nil || arguments.GetAlgorithm() != runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256 ||
		len(arguments.GetValue()) != len(digest) {
		return credentialbroker.BoundToolInvocation{}, digest, false
	}
	invocation := credentialbroker.BoundToolInvocation{
		InvocationID: message.GetInvocationId(), ToolCallID: message.GetToolCallId(), ToolkitID: message.GetToolkitId(),
		OperationID: message.GetOperationId(), ResourceRef: message.GetResourceRef(),
		CredentialReferenceID: message.GetCredentialReferenceId(),
	}
	copy(invocation.ArgumentsDigest[:], arguments.GetValue())
	return invocation, digest, true
}

func envelopeKindProto(kind credentialbroker.EnvelopeKind) runtimev1.CredentialEnvelopeKindV1 {
	switch kind {
	case credentialbroker.KindStatic:
		return runtimev1.CredentialEnvelopeKindV1_CREDENTIAL_ENVELOPE_KIND_V1_STATIC
	case credentialbroker.KindOAuthAccess:
		return runtimev1.CredentialEnvelopeKindV1_CREDENTIAL_ENVELOPE_KIND_V1_OAUTH_ACCESS
	case credentialbroker.KindDynamicLease:
		return runtimev1.CredentialEnvelopeKindV1_CREDENTIAL_ENVELOPE_KIND_V1_DYNAMIC_LEASE
	}
	return runtimev1.CredentialEnvelopeKindV1_CREDENTIAL_ENVELOPE_KIND_V1_UNSPECIFIED
}
