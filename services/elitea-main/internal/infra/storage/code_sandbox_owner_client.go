package storage

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/workloadidentity"
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	recovery "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
)

// ErrCodePlatformNotReady is a verified read-only admission observation.
// It never represents an HTTP refusal, a missing lease, or runtime authority.
var ErrCodePlatformNotReady = errors.New("code platform runtime is not ready")

// ErrCodePlatformCompleted is an authenticated read-only completion observation.
// It carries no runtime authority, execution result, or publication permission.
var ErrCodePlatformCompleted = errors.New("code platform runtime is completed")

// ErrCodePlatformCompleting observes a stopped runtime awaiting its Submit receipt.
// It grants no runtime authority, execution result, or publication permission.
var ErrCodePlatformCompleting = errors.New("code platform runtime is completing")

// CodeOwnerGrantSigner owns a copied key and one configured Main certificate identity.
// It signs no ordinary invocation, sandbox execution, content or checkpoint grants.
type CodeOwnerGrantSigner struct {
	keyID     string
	key       ed25519.PrivateKey
	requester string
	audiences map[string]bool
}

func NewCodeOwnerGrantSigner(keyID string, key ed25519.PrivateKey, requester string, audiences []string) (*CodeOwnerGrantSigner, error) {
	if !code.Identity(keyID) || len(key) != ed25519.PrivateKeySize || !code.Identity(requester) || len(audiences) == 0 || len(audiences) > 16 {
		return nil, code.ErrRejected
	}
	allowed := map[string]bool{}
	for _, audience := range audiences {
		if !code.Identity(audience) || allowed[audience] {
			return nil, code.ErrRejected
		}
		allowed[audience] = true
	}
	return &CodeOwnerGrantSigner{keyID: keyID, key: append(ed25519.PrivateKey(nil), key...), requester: requester, audiences: allowed}, nil
}

func (s *CodeOwnerGrantSigner) sign(schema, domain string, claims any) (code.SignedGrant, error) {
	if s == nil {
		return code.SignedGrant{}, code.ErrRejected
	}
	raw, err := code.Canonical(claims)
	if err != nil || len(raw) == 0 || len(raw) > 8192 {
		return code.SignedGrant{}, code.ErrRejected
	}
	input := append([]byte(domain), make([]byte, 8)...)
	binary.BigEndian.PutUint64(input[len(domain):], uint64(len(raw)))
	input = append(input, raw...)
	return code.SignedGrant{Schema: schema, KeyID: s.keyID, ClaimsBase64URL: base64.RawURLEncoding.EncodeToString(raw), SignatureBase64URL: base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.key, input))}, nil
}

func (s *CodeOwnerGrantSigner) RequesterIdentity() string {
	if s == nil {
		return ""
	}
	return s.requester
}
func (s *CodeOwnerGrantSigner) AllowsSupervisorAudience(audience string) bool {
	return s != nil && s.audiences[audience]
}

func (s *CodeOwnerGrantSigner) SignIntent(c code.IntentClaims) (code.SignedGrant, error) {
	if s == nil || !s.audiences[c.SupervisorAudience] || c.Schema != "elitea.sandbox.original-code-intent.v1" || c.Purpose != "whole_code_execute" || !validCodeClaim(c.ClaimID, c.ClaimAttempt, c.LeaseEpoch, c.FenceSHA256, c.IssuedAtMillis, c.ExpiresAtMillis) || !code.Identity(c.SubmitterWorkloadIdentity) {
		return code.SignedGrant{}, code.ErrRejected
	}
	b := code.Binding{Schema: "elitea.sandbox.whole-code-binding.v1", Purpose: c.Purpose, ExecutionID: c.ExecutionID, OriginalGeneration: c.OriginalGeneration, ActivationID: c.ActivationID, NodeID: c.NodeID, GraphThread: c.GraphThread, Step: c.Step, Attempt: c.Attempt, DispatchActivation: c.DispatchActivation, JobKey: c.JobKey, RequestDigest: c.RequestDigest, SupervisorAudience: c.SupervisorAudience, NodeDigest: c.NodeDigest, Language: c.Language, PreparedSHA256: c.PreparedSHA256, SourceSHA256: c.SourceSHA256, InputSHA256: c.InputSHA256}
	if b.Validate() != nil || !validCodeVisit(c.ActivationID, c.NodeID, c.GraphThread, c.Step, c.Attempt) || !code.Identity(c.TenantID) || c.ProjectID <= 0 || c.ProjectID > math.MaxInt32 {
		return code.SignedGrant{}, code.ErrRejected
	}
	return s.sign("elitea.sandbox.original-code-intent-signed.v1", code.IntentDomain, c)
}

func (s *CodeOwnerGrantSigner) SignRecovery(c code.GrantClaims) (code.SignedGrant, error) {
	if s == nil || !s.audiences[c.SupervisorAudience] || !recovery.ValidExecutionID(c.ExecutionID) || c.OriginalGeneration == 0 || c.OriginalGeneration > math.MaxInt64 || !code.Identity(c.TenantID) || c.ProjectID <= 0 || c.ProjectID > math.MaxInt32 || !validCodeVisit(c.ActivationID, c.NodeID, c.GraphThread, c.Step, c.Attempt) || c.Schema != "elitea.sandbox.node-code-recovery-grant.v1" || c.RequesterWorkloadIdentity != s.requester || c.Operation != "read" && c.Operation != "seal_no_effect" || !validCodeClaim(c.ClaimID, c.ClaimAttempt, c.LeaseEpoch, c.FenceSHA256, c.IssuedAtMillis, c.ExpiresAtMillis) || !code.NonzeroDigest(c.ActivationID) || !code.NonzeroDigest(c.ReceiptSHA256) || !code.NonzeroDigest(c.BindingSHA256) || c.ExpectedRevision == 0 || c.ExpectedRevision > math.MaxInt64 || !code.NonzeroDigest(c.DispatchActivation) || c.JobKey != code.JobKey(c.ExecutionID, c.DispatchActivation) || !code.NonzeroDigest(c.RequestDigest) {
		return code.SignedGrant{}, code.ErrRejected
	}
	return s.sign("elitea.sandbox.node-code-recovery-signed-grant.v1", code.RecoveryDomain, c)
}
func validCodeVisit(activation, node, thread string, step uint64, attempt uint16) bool {
	return code.VisitBounds(activation, node, thread, step, attempt)
}
func validCodeClaim(id string, attempt, epoch uint64, fence string, issued, expires int64) bool {
	return recovery.ValidExecutionID(id) && attempt > 0 && attempt <= math.MaxInt64 && epoch > 0 && epoch <= math.MaxInt64 && code.NonzeroDigest(fence) && issued > 0 && expires > issued && expires-issued <= 30000
}

type CodeOwnerEndpoint struct {
	Audience string
	// Origin is the supervisor's https://host:port gRPC listener.
	Origin string
	TLS    *tls.Config
}
type codeOwnerEndpoint struct {
	conn   *grpc.ClientConn
	client runtimev1.SandboxSupervisorServiceClient
	tls    *tls.Config
}

// codeOwnerMessageBytes caps one owner RPC message in either direction.
const codeOwnerMessageBytes = 16 << 20

// CodeOwnerClient calls only the closed recovery and retained-runtime owner
// RPCs of SandboxSupervisorService. It never submits executable jobs.
type CodeOwnerClient struct {
	endpoints map[string]codeOwnerEndpoint
	requester string
}

func NewCodeOwnerClient(requester string, configs []CodeOwnerEndpoint) (*CodeOwnerClient, error) {
	if !code.Identity(requester) || len(configs) == 0 || len(configs) > 16 {
		return nil, code.ErrRejected
	}
	c := &CodeOwnerClient{endpoints: map[string]codeOwnerEndpoint{}, requester: requester}
	for _, cfg := range configs {
		u, err := url.Parse(cfg.Origin)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.Port() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" || !code.Identity(cfg.Audience) || cfg.TLS == nil || cfg.TLS.InsecureSkipVerify || cfg.TLS.GetClientCertificate != nil || cfg.TLS.RootCAs == nil || len(cfg.TLS.Certificates) != 1 || len(cfg.TLS.Certificates[0].Certificate) == 0 || cfg.TLS.Certificates[0].PrivateKey == nil {
			c.Close()
			return nil, code.ErrRejected
		}
		if _, ok := c.endpoints[cfg.Audience]; ok {
			c.Close()
			return nil, code.ErrRejected
		}
		leaf, err := x509.ParseCertificate(cfg.TLS.Certificates[0].Certificate[0])
		if err != nil {
			c.Close()
			return nil, code.ErrRejected
		}
		peer, err := workloadidentity.Certificate(leaf)
		if err != nil || peer != requester {
			c.Close()
			return nil, code.ErrRejected
		}
		tlsConfig := cfg.TLS.Clone()
		if tlsConfig.MinVersion < tls.VersionTLS13 {
			tlsConfig.MinVersion = tls.VersionTLS13
		}
		previous := tlsConfig.VerifyConnection
		audience := cfg.Audience
		tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
			if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
				return code.ErrRejected
			}
			identity, err := workloadidentity.Certificate(state.PeerCertificates[0])
			if err != nil || identity != audience {
				return code.ErrRejected
			}
			if previous != nil {
				return previous(state)
			}
			return nil
		}
		conn, err := grpc.NewClient("dns:///"+u.Host,
			grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
			grpc.WithNoProxy(),
			grpc.WithDisableRetry(),
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(codeOwnerMessageBytes), grpc.MaxCallSendMsgSize(codeOwnerMessageBytes)),
		)
		if err != nil {
			c.Close()
			return nil, code.ErrRejected
		}
		c.endpoints[audience] = codeOwnerEndpoint{conn: conn, client: runtimev1.NewSandboxSupervisorServiceClient(conn), tls: tlsConfig}
	}
	return c, nil
}

func (c *CodeOwnerClient) Close() {
	if c != nil {
		for _, endpoint := range c.endpoints {
			_ = endpoint.conn.Close()
		}
	}
}

// Read makes one bounded request. The caller owns the transaction and current claim recheck.
func (c *CodeOwnerClient) Read(ctx context.Context, claims code.GrantClaims, grant code.SignedGrant) ([]byte, error) {
	if c == nil || !signedCodeOwnerGrantMatches(grant, "elitea.sandbox.node-code-recovery-signed-grant.v1", claims) || claims.RequesterWorkloadIdentity != c.requester || !code.NonzeroDigest(claims.JobKey) || claims.Operation != "read" && claims.Operation != "seal_no_effect" {
		return nil, code.ErrRejected
	}
	endpoint, ok := c.endpoints[claims.SupervisorAudience]
	if !ok {
		return nil, code.ErrRejected
	}
	body, err := code.Canonical(code.OwnerRequest{Schema: "elitea.sandbox.node-code-recovery-request.v1", Grant: grant})
	if err != nil || len(body) > 16*1024 {
		return nil, code.ErrRejected
	}
	raw, err := boundedCodeOwnerCall(ctx, code.MaxReceiptBytes, 3*time.Second, func(ctx context.Context) ([]byte, error) {
		if claims.Operation == "seal_no_effect" {
			response, err := endpoint.client.CodeOwnerRecoverySealNoEffect(ctx, &runtimev1.CodeOwnerRecoverySealNoEffectRequestV1{JobKey: claims.JobKey, RequestJson: body})
			return response.GetResponseJson(), err
		}
		response, err := endpoint.client.CodeOwnerRecoveryRead(ctx, &runtimev1.CodeOwnerRecoveryReadRequestV1{JobKey: claims.JobKey, RequestJson: body})
		return response.GetResponseJson(), err
	})
	if err != nil {
		return nil, err
	}
	var result code.Response
	if code.Decode(raw, &result, code.MaxReceiptBytes) != nil || !code.RequiredFields(raw, []string{"schema", "state", "receipt"}, "receipt") || result.Schema != "elitea.sandbox.node-code-recovery-response.v1" {
		return nil, code.ErrRejected
	}
	if result.State != "completed" && result.State != "verified_no_effect" {
		return nil, code.ErrRejected
	}
	if len(result.Receipt) == 0 || bytes.Equal(bytes.TrimSpace(result.Receipt), []byte("null")) {
		return nil, code.ErrRejected
	}
	var receipt code.Receipt
	if code.Decode(result.Receipt, &receipt, code.MaxReceiptBytes) != nil || (result.State == "completed") != (receipt.Kind == "committed_result") || (result.State == "verified_no_effect") != (receipt.Kind == "verified_no_effect") {
		return nil, code.ErrRejected
	}
	return bytes.Clone(result.Receipt), nil
}

// boundedCodeOwnerCall is private: callers below choose fixed purpose-specific
// RPCs. No retry, caller origin or automatic credential forwarding is exposed,
// and any refusal from the supervisor is a refusal here.
func boundedCodeOwnerCall(ctx context.Context, max int, deadline time.Duration, call func(context.Context) ([]byte, error)) ([]byte, error) {
	limited, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	raw, err := call(limited)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, code.ErrRejected
	}
	if len(raw) == 0 || len(raw) > max {
		return nil, code.ErrRejected
	}
	return raw, nil
}
func signedCodeOwnerGrantMatches(g code.SignedGrant, schema string, claims any) bool {
	if g.Schema != schema || !code.Identity(g.KeyID) {
		return false
	}
	raw, err := code.DecodeBase64(g.ClaimsBase64URL, 8192)
	if err != nil {
		return false
	}
	sig, err := code.DecodeBase64(g.SignatureBase64URL, ed25519.SignatureSize)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	expected, err := code.Canonical(claims)
	return err == nil && bytes.Equal(raw, expected)
}
func (s *CodeOwnerGrantSigner) SignPlatform(c code.PlatformGrantClaims) (code.SignedGrant, error) {
	if s == nil || c.Validate() != nil || c.RequesterWorkloadIdentity != s.requester || !s.audiences[c.SupervisorAudience] {
		return code.SignedGrant{}, code.ErrRejected
	}
	return s.sign("elitea.sandbox.code-platform-owner-signed-grant.v1", code.PlatformDomain, c)
}

func (c *CodeOwnerClient) ReadRetainedRuntime(ctx context.Context, claims code.PlatformGrantClaims, grant code.SignedGrant) (code.RetainedRuntime, error) {
	if claims.Operation != "read_retained_runtime" {
		return code.RetainedRuntime{}, code.ErrRejected
	}
	response, err := c.platformOperation(ctx, claims, grant, nil)
	return response.Runtime, err
}
func (c *CodeOwnerClient) ReadPendingPlatformCall(ctx context.Context, claims code.PlatformGrantClaims, grant code.SignedGrant) (code.RetainedRuntime, []byte, error) {
	if claims.Operation != "read_pending_platform_call" {
		return code.RetainedRuntime{}, nil, code.ErrRejected
	}
	response, err := c.platformOperation(ctx, claims, grant, nil)
	if err != nil {
		return code.RetainedRuntime{}, nil, err
	}
	if response.PendingCallBase64URL == nil {
		return response.Runtime, nil, nil
	}
	raw, err := code.DecodeBase64(*response.PendingCallBase64URL, code.MaxPlatformCallBytes)
	if err != nil {
		return code.RetainedRuntime{}, nil, err
	}
	return response.Runtime, raw, nil
}
func (c *CodeOwnerClient) PublishCommittedPlatformReply(ctx context.Context, claims code.PlatformGrantClaims, grant code.SignedGrant, reply []byte) (code.RetainedRuntime, error) {
	if claims.Operation != "publish_committed_platform_reply" || len(reply) == 0 || len(reply) > code.MaxCommittedPlatformReplyBytes || claims.CommittedReplySHA256 == nil || code.Digest(reply) != *claims.CommittedReplySHA256 {
		return code.RetainedRuntime{}, code.ErrRejected
	}
	encoded := base64.RawURLEncoding.EncodeToString(reply)
	response, err := c.platformOperation(ctx, claims, grant, &encoded)
	return response.Runtime, err
}
func (c *CodeOwnerClient) platformOperation(ctx context.Context, claims code.PlatformGrantClaims, grant code.SignedGrant, reply *string) (code.PlatformOwnerResponse, error) {
	if c == nil || claims.Validate() != nil || claims.RequesterWorkloadIdentity != c.requester || !signedCodeOwnerGrantMatches(grant, "elitea.sandbox.code-platform-owner-signed-grant.v1", claims) {
		return code.PlatformOwnerResponse{}, code.ErrRejected
	}
	if (claims.Operation == "publish_committed_platform_reply") != (reply != nil) {
		return code.PlatformOwnerResponse{}, code.ErrRejected
	}
	endpoint, ok := c.endpoints[claims.SupervisorAudience]
	if !ok {
		return code.PlatformOwnerResponse{}, code.ErrRejected
	}
	body, err := code.Canonical(code.PlatformOwnerRequest{Schema: "elitea.sandbox.code-platform-owner-request.v1", Grant: grant, CommittedReplyBase64URL: reply})
	limit := 16 * 1024
	if reply != nil {
		limit = code.MaxPlatformResponseBytes
	}
	if err != nil || len(body) > limit {
		return code.PlatformOwnerResponse{}, code.ErrRejected
	}
	raw, err := boundedCodeOwnerCall(ctx, code.MaxPlatformResponseBytes, 10*time.Second, func(ctx context.Context) ([]byte, error) {
		switch claims.Operation {
		case "read_retained_runtime":
			response, err := endpoint.client.CodePlatformOwnerReadRetainedRuntime(ctx, &runtimev1.CodePlatformOwnerReadRetainedRuntimeRequestV1{JobKey: claims.JobKey, RequestJson: body})
			return response.GetResponseJson(), err
		case "read_pending_platform_call":
			response, err := endpoint.client.CodePlatformOwnerReadPendingCall(ctx, &runtimev1.CodePlatformOwnerReadPendingCallRequestV1{JobKey: claims.JobKey, RequestJson: body})
			return response.GetResponseJson(), err
		case "publish_committed_platform_reply":
			response, err := endpoint.client.CodePlatformOwnerPublishReply(ctx, &runtimev1.CodePlatformOwnerPublishReplyRequestV1{JobKey: claims.JobKey, RequestJson: body})
			return response.GetResponseJson(), err
		}
		return nil, code.ErrRejected
	})
	if err != nil {
		return code.PlatformOwnerResponse{}, err
	}
	var observation struct {
		Schema               string          `json:"schema"`
		State                string          `json:"state"`
		Runtime              json.RawMessage `json:"runtime"`
		PendingCallBase64URL json.RawMessage `json:"pending_call_base64url"`
		ReplyPublished       json.RawMessage `json:"reply_published"`
	}
	if code.Decode(raw, &observation, code.MaxPlatformResponseBytes) != nil {
		return code.PlatformOwnerResponse{}, code.ErrRejected
	}
	if observation.State == "not_ready" {
		fields := []string{"schema", "state", "runtime", "pending_call_base64url", "reply_published"}
		if claims.Operation != "read_retained_runtime" || observation.Schema != "elitea.sandbox.code-platform-owner-response.v1" ||
			!code.RequiredFields(raw, fields, "runtime", "pending_call_base64url", "reply_published") ||
			!bytes.Equal(bytes.TrimSpace(observation.Runtime), []byte("null")) ||
			!bytes.Equal(bytes.TrimSpace(observation.PendingCallBase64URL), []byte("null")) ||
			!bytes.Equal(bytes.TrimSpace(observation.ReplyPublished), []byte("null")) {
			return code.PlatformOwnerResponse{}, code.ErrRejected
		}
		return code.PlatformOwnerResponse{}, ErrCodePlatformNotReady
	}
	if observation.State == "completed" || observation.State == "completing" {
		fields := []string{"schema", "state", "runtime", "pending_call_base64url", "reply_published"}
		if (claims.Operation != "read_retained_runtime" && claims.Operation != "read_pending_platform_call") ||
			observation.Schema != "elitea.sandbox.code-platform-owner-response.v1" ||
			!code.RequiredFields(raw, fields, "runtime", "pending_call_base64url", "reply_published") ||
			!bytes.Equal(bytes.TrimSpace(observation.Runtime), []byte("null")) ||
			!bytes.Equal(bytes.TrimSpace(observation.PendingCallBase64URL), []byte("null")) ||
			!bytes.Equal(bytes.TrimSpace(observation.ReplyPublished), []byte("null")) {
			return code.PlatformOwnerResponse{}, code.ErrRejected
		}
		if observation.State == "completing" {
			return code.PlatformOwnerResponse{}, ErrCodePlatformCompleting
		}
		return code.PlatformOwnerResponse{}, ErrCodePlatformCompleted
	}
	var response code.PlatformOwnerResponse

	if code.Decode(raw, &response, code.MaxPlatformResponseBytes) != nil || !code.RequiredFields(raw, []string{"schema", "state", "runtime", "pending_call_base64url", "reply_published"}, "pending_call_base64url", "reply_published") || response.Schema != "elitea.sandbox.code-platform-owner-response.v1" || response.State != "running" || !response.Runtime.Matches(claims) {
		return code.PlatformOwnerResponse{}, code.ErrRejected
	}
	var runtimeWire struct {
		Runtime json.RawMessage `json:"runtime"`
	}
	if json.Unmarshal(raw, &runtimeWire) != nil || !code.RequiredFields(runtimeWire.Runtime, []string{"kind", "runtime_id", "owner_epoch", "binding_sha256", "prepared_job_sha256", "prepared_fingerprint", "compiled_execute", "policy_sha256", "max_calls", "max_total_bytes", "lifecycle"}, "compiled_execute") {
		return code.PlatformOwnerResponse{}, code.ErrRejected
	}
	switch claims.Operation {
	case "read_retained_runtime":
		if response.PendingCallBase64URL != nil || response.ReplyPublished != nil {
			return code.PlatformOwnerResponse{}, code.ErrRejected
		}
	case "read_pending_platform_call":
		if response.ReplyPublished != nil {
			return code.PlatformOwnerResponse{}, code.ErrRejected
		}
		if response.PendingCallBase64URL != nil {
			frame, err := code.DecodeBase64(*response.PendingCallBase64URL, code.MaxPlatformCallBytes)
			if err != nil || len(frame) == 0 {
				return code.PlatformOwnerResponse{}, code.ErrRejected
			}
		}
	case "publish_committed_platform_reply":
		if response.PendingCallBase64URL != nil || response.ReplyPublished == nil || !*response.ReplyPublished {
			return code.PlatformOwnerResponse{}, code.ErrRejected
		}
	}
	return response, nil
}
