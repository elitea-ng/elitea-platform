package vectorintrospection

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

const (
	// ClaimTokenPrefix starts every worker claim token.
	ClaimTokenPrefix = "elvc_"
	// claimTokenRandomBytes is the token's entropy.
	claimTokenRandomBytes = 32
	// ClaimTokenLength is the full length: the prefix and 43 base64url
	// characters.
	ClaimTokenLength = len(ClaimTokenPrefix) + 43
	// MaxClaimTokenLifetime caps a token's expiry when the command deadline
	// is later. The token is revoked well before that by its claim (a lease
	// is 30 s and must be renewed); the cap bounds a token whose revocation
	// read could not run.
	MaxClaimTokenLifetime = 6 * time.Hour
	// sourceToolkitIndex is the payload keyword of SOURCE_TOOLKIT_INDEX.
	sourceToolkitIndex = "toolkit_index"
)

// claimTokenCapabilities are the execution capabilities whose worker may use
// vectors: an index ingest writes a toolkit index, and a toolkit call or an
// agent reads one through the index toolkit (ADR-0030). Every other
// capability gets no token.
var claimTokenCapabilities = []string{
	executiondomain.IndexIngestCapability,
	executiondomain.ToolkitCallToolCapability,
	executiondomain.AgentApplicationCapability,
	executiondomain.AgentAdhocCapability,
}

// claimTokenSources is what a worker claim token may read and write.
var claimTokenSources = []string{sourceToolkitIndex}

// HashClaimToken is the stored form of a well-formed claim token. A token of
// the wrong length, prefix or alphabet is not well formed.
func HashClaimToken(token string) ([32]byte, bool) {
	if len(token) != ClaimTokenLength || token[:len(ClaimTokenPrefix)] != ClaimTokenPrefix {
		return [32]byte{}, false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(token[len(ClaimTokenPrefix):])
	if err != nil || len(decoded) != claimTokenRandomBytes {
		return [32]byte{}, false
	}
	return sha256.Sum256([]byte(token)), true
}

// ClaimTokenStore records token hashes (*repos.VectorClaimTokens).
type ClaimTokenStore interface {
	Mint(ctx context.Context, request repos.VectorClaimTokenMint) (repos.VectorClaimTokenMinted, error)
}

var _ ClaimTokenStore = (*repos.VectorClaimTokens)(nil)

// ClaimTokenIssuer mints the per-claim worker token (ADR-0031 decision 1).
type ClaimTokenIssuer struct {
	store       ClaimTokenStore
	random      io.Reader
	maxLifetime time.Duration
}

// NewClaimTokenIssuer builds an issuer over store.
func NewClaimTokenIssuer(store ClaimTokenStore) (*ClaimTokenIssuer, error) {
	if store == nil {
		return nil, errors.New("vector claim token issuer requires a store")
	}
	return &ClaimTokenIssuer{store: store, random: rand.Reader, maxLifetime: MaxClaimTokenLifetime}, nil
}

// IssueVectorClaimToken mints a token for the live claim fence names. It
// returns nil and no error when the claim's execution gets no token, and
// runtimedomain.ErrStaleFence when the claim is no longer live.
func (i *ClaimTokenIssuer) IssueVectorClaimToken(ctx context.Context, fence runtimedomain.Fence) (*runtimev1.VectorClaimTokenV1, error) {
	var secret [claimTokenRandomBytes]byte
	if _, err := io.ReadFull(i.random, secret[:]); err != nil {
		return nil, fmt.Errorf("generate vector claim token: %w", err)
	}
	token := ClaimTokenPrefix + base64.RawURLEncoding.EncodeToString(secret[:])
	hash, ok := HashClaimToken(token)
	if !ok {
		return nil, errors.New("generate vector claim token: malformed token")
	}
	minted, err := i.store.Mint(ctx, repos.VectorClaimTokenMint{
		Fence:        fence,
		TokenSHA256:  hash,
		Capabilities: claimTokenCapabilities,
		Sources:      claimTokenSources,
		MaxLifetime:  i.maxLifetime,
	})
	if err != nil {
		return nil, err
	}
	if !minted.Minted {
		return nil, nil
	}
	return &runtimev1.VectorClaimTokenV1{
		Bearer:              token,
		ExpiresAtUnixMillis: minted.ExpiresAt.UnixMilli(),
		AllowedSources:      append([]string(nil), minted.Sources...),
	}, nil
}
