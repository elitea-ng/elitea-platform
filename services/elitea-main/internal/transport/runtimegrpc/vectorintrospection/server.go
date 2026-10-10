// Package vectorintrospection serves elitea.vector.v1.TokenIntrospectionService
// on elitea-main's private mTLS control listener (ADR-0031 decision 1), and
// mints the per-claim worker token it verifies.
//
// elitea-vector calls it to learn which project a request's token is bound
// to, and which sources the token may use. It answers only the client
// certificate identities it is configured with, and it admits two kinds of
// token:
//
//   - A worker claim token ("elvc_…", TOKEN_KIND_WORKER_CLAIM), minted by
//     ClaimTokenIssuer when a worker claims an execution. Main keeps only its
//     SHA-256 (elitea_runtime.vector_claim_tokens, shared/0160). It is active
//     only while its claim is live and its execution is CLAIMED or RUNNING
//     with desired state RUNNING; it may use the sources recorded with it
//     (toolkit_index).
//   - A provider callback token (TOKEN_KIND_ENGINE_CALLBACK): a valid
//     signature, an active owner, a binding to an active project, an expiry
//     in the future, and a callback grant recorded for that project by the
//     minting facade (elitea_identity.callback_token_grant, shared/0159). A
//     personal access token with the same name, binding and expiry has no
//     grant and is inactive here. The grant's provider decides the one source
//     the token may use (ProviderSource); a provider with no source is
//     inactive.
package vectorintrospection

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	vectorv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/vector/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/workloadidentity"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// maxTokenBytes bounds a bearer string; elitea-vector sends no longer one.
const maxTokenBytes = 4096

// TokenValidator is the PAT validator (authcomposition.FormGraph): the
// signature, the token row and its owner, and the project binding.
type TokenValidator interface {
	ValidateToken(ctx context.Context, token string) (auth.User, error)
}

// CallbackFacts reads what makes a token a live callback token
// (*repos.CallbackTokenGrants).
type CallbackFacts interface {
	Facts(ctx context.Context, tokenID int64) (repos.CallbackTokenFacts, error)
}

// ClaimTokenFacts reads a live worker claim token by its hash
// (*repos.VectorClaimTokens).
type ClaimTokenFacts interface {
	Facts(ctx context.Context, tokenSHA256 [32]byte) (repos.VectorClaimTokenFacts, error)
}

var (
	_ CallbackFacts   = (*repos.CallbackTokenGrants)(nil)
	_ ClaimTokenFacts = (*repos.VectorClaimTokens)(nil)
)

// providerSources maps the provider a callback grant records to the one
// source its tokens may use. DeepWiki records no grant today, so its
// callback tokens stay inactive until its facade records one; Inventory's
// source is reserved and refused by elitea-vector (ADR-0031 decision 7).
var providerSources = map[string]vectorv1.Source{
	"deepwiki":  vectorv1.Source_SOURCE_DEEPWIKI,
	"inventory": vectorv1.Source_SOURCE_INVENTORY,
}

// ProviderSource is the source a callback token minted by provider may use.
func ProviderSource(provider string) (vectorv1.Source, bool) {
	source, ok := providerSources[provider]
	return source, ok
}

// sourceKeywords maps the payload keyword stored with a claim token to the
// wire enum.
var sourceKeywords = map[string]vectorv1.Source{
	"toolkit_index": vectorv1.Source_SOURCE_TOOLKIT_INDEX,
	"deepwiki":      vectorv1.Source_SOURCE_DEEPWIKI,
	"inventory":     vectorv1.Source_SOURCE_INVENTORY,
}

// Server is the introspection service.
type Server struct {
	vectorv1.UnimplementedTokenIntrospectionServiceServer

	validator   TokenValidator
	facts       CallbackFacts
	claimTokens ClaimTokenFacts
	clients     map[string]struct{}
	logger      *slog.Logger
}

// NewServer builds a server that answers only the client identities in
// clients ("dns:<name>" or a spiffe:// URI, as workloadidentity spells
// them).
func NewServer(validator TokenValidator, facts CallbackFacts, claimTokens ClaimTokenFacts, clients []string, logger *slog.Logger) (*Server, error) {
	if validator == nil || facts == nil || claimTokens == nil {
		return nil, errors.New("vector token introspection requires a token validator, callback facts and claim token facts")
	}
	allowed := make(map[string]struct{}, len(clients))
	for _, client := range clients {
		client = strings.TrimSpace(client)
		if !strings.HasPrefix(client, "dns:") && !strings.HasPrefix(client, "spiffe://") {
			return nil, fmt.Errorf("vector introspection client %q is not a dns: or spiffe:// identity", client)
		}
		allowed[client] = struct{}{}
	}
	if len(allowed) == 0 {
		return nil, errors.New("vector token introspection requires at least one client identity")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{validator: validator, facts: facts, claimTokens: claimTokens, clients: allowed, logger: logger}, nil
}

// IntrospectToken answers whether request.Token is usable for vector access.
func (s *Server) IntrospectToken(ctx context.Context, request *vectorv1.IntrospectTokenRequest) (*vectorv1.IntrospectTokenResponse, error) {
	identity, err := peerIdentity(ctx)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "A verified client certificate is required.")
	}
	if _, ok := s.clients[identity]; !ok {
		return nil, status.Error(codes.PermissionDenied, "This client may not introspect tokens.")
	}
	token := request.GetToken()
	if token == "" || len(token) > maxTokenBytes {
		return &vectorv1.IntrospectTokenResponse{}, nil
	}
	// A claim token never reaches the PAT validator, and a PAT never reaches
	// the claim token store.
	if strings.HasPrefix(token, ClaimTokenPrefix) {
		return s.introspectClaimToken(ctx, token)
	}
	return s.introspectCallbackToken(ctx, token)
}

func (s *Server) introspectClaimToken(ctx context.Context, token string) (*vectorv1.IntrospectTokenResponse, error) {
	inactive := &vectorv1.IntrospectTokenResponse{}
	hash, ok := HashClaimToken(token)
	if !ok {
		return inactive, nil
	}
	facts, err := s.claimTokens.Facts(ctx, hash)
	if err != nil {
		if errors.Is(err, repos.ErrVectorClaimTokenFactsNotFound) {
			return inactive, nil
		}
		s.logger.WarnContext(ctx, "vector token introspection: claim token facts unavailable", "error", err)
		return nil, status.Error(codes.Unavailable, "Token validation is unavailable.")
	}
	sources := make([]vectorv1.Source, 0, len(facts.Sources))
	for _, keyword := range facts.Sources {
		source, known := sourceKeywords[keyword]
		if !known {
			// A keyword this build cannot name admits nothing rather than less.
			return inactive, nil
		}
		sources = append(sources, source)
	}
	if facts.ProjectID <= 0 || facts.ActorID <= 0 || len(sources) == 0 {
		return inactive, nil
	}
	return &vectorv1.IntrospectTokenResponse{
		Active:         true,
		ProjectId:      facts.ProjectID,
		Principal:      "user:" + strconv.FormatInt(facts.ActorID, 10),
		Kind:           vectorv1.TokenKind_TOKEN_KIND_WORKER_CLAIM,
		ExpiresAtUnix:  facts.ExpiresAt.Unix(),
		AllowedSources: sources,
	}, nil
}

func (s *Server) introspectCallbackToken(ctx context.Context, token string) (*vectorv1.IntrospectTokenResponse, error) {
	inactive := &vectorv1.IntrospectTokenResponse{}
	// A native token, refresh token or code is never a callback token.
	if strings.HasPrefix(token, "eln") {
		return inactive, nil
	}
	user, err := s.validator.ValidateToken(ctx, token)
	if err != nil {
		if errors.Is(err, auth.ErrCredentialRejected) {
			return inactive, nil
		}
		s.logger.WarnContext(ctx, "vector token introspection: validation unavailable", "error", err)
		return nil, status.Error(codes.Unavailable, "Token validation is unavailable.")
	}
	if user.TokenProjectID == nil || *user.TokenProjectID <= 0 ||
		user.TokenProjectActive == nil || !*user.TokenProjectActive {
		return inactive, nil
	}
	tokenID, err := strconv.ParseInt(user.TokenID, 10, 64)
	if err != nil || tokenID <= 0 {
		return inactive, nil
	}
	userID, err := strconv.ParseInt(user.UserID, 10, 64)
	if err != nil || userID <= 0 {
		return inactive, nil
	}

	facts, err := s.facts.Facts(ctx, tokenID)
	if err != nil {
		if errors.Is(err, repos.ErrCallbackTokenFactsNotFound) {
			return inactive, nil
		}
		s.logger.WarnContext(ctx, "vector token introspection: callback facts unavailable", "error", err)
		return nil, status.Error(codes.Unavailable, "Token validation is unavailable.")
	}
	// The grant and the binding the validator read must agree.
	if facts.ProjectID != *user.TokenProjectID || facts.UserID != userID {
		return inactive, nil
	}
	source, ok := ProviderSource(facts.Provider)
	if !ok {
		return inactive, nil
	}
	return &vectorv1.IntrospectTokenResponse{
		Active:         true,
		ProjectId:      facts.ProjectID,
		Principal:      "user:" + strconv.FormatInt(userID, 10),
		Kind:           vectorv1.TokenKind_TOKEN_KIND_ENGINE_CALLBACK,
		ExpiresAtUnix:  facts.ExpiresAt.Unix(),
		AllowedSources: []vectorv1.Source{source},
	}, nil
}

// peerIdentity is the canonical identity of the verified mTLS peer, by the
// same rule the worker planes use (workloadauth).
func peerIdentity(ctx context.Context) (string, error) {
	info, ok := peer.FromContext(ctx)
	if !ok || info == nil || info.AuthInfo == nil {
		return "", errors.New("no peer")
	}
	var state credentials.TLSInfo
	switch tlsInfo := info.AuthInfo.(type) {
	case credentials.TLSInfo:
		state = tlsInfo
	case *credentials.TLSInfo:
		if tlsInfo == nil {
			return "", errors.New("no TLS peer")
		}
		state = *tlsInfo
	default:
		return "", errors.New("no TLS peer")
	}
	if len(state.State.VerifiedChains) == 0 || len(state.State.VerifiedChains[0]) == 0 {
		return "", errors.New("unverified peer")
	}
	return workloadidentity.Certificate(state.State.VerifiedChains[0][0])
}
