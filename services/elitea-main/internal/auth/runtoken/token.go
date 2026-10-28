// Package runtoken is the run-scoped client token of a governed run
// (ADR-0039 follow-up, #1461).
//
// A governed run's worker uses its client token for two things only: the
// model path (LLM calls, embeddings, images and metering, all /llm routes)
// and the run project's artifact store (the Office host and indexing from
// platform artifacts, through ArtifactObjectService). Its toolkit
// credentials come from RedeemCredential (#1118). Handing it the execution
// actor's ordinary personal access token therefore gave it far more than it
// uses, so GetEliteaClientToken serves a governed-pinned run this token
// instead:
//
//   - AUDIENCES. The token lists its audiences (AudienceModelGateway,
//     AudienceArtifacts), and each route group that accepts it names the one
//     audience it serves; the validator checks that audience per request.
//     Every other route of the Auth middleware answers 403 for it, and any
//     other reader of bearer credentials does not know the format and
//     refuses it.
//   - CLAIM-BOUND. It names the run's claim (claim id, execution, generation
//     and fence) and is accepted only while that claim is unreleased and its
//     lease unexpired, read from PostgreSQL on every request. It therefore
//     expires with the lease, and a renewed lease keeps it valid: renewal
//     extends lease_expires_at and keeps the claim id and fence token, so the
//     one token the worker redeemed at assembly carries a long turn through.
//     A signed ceiling (MaxLifetime) bounds it even if a claim row lingers.
//   - ONE PROJECT. It names the run's project; the edge refuses a request
//     that names any other.
//   - SAME ATTRIBUTION. It resolves to the execution actor and the actor's
//     personal access token row the ordinary token would have named, so the
//     principal check, the RBAC and membership checks, the execution-id
//     check and the gateway's signed identity are the ones a PAT caller
//     gets, and usage bills the same project and actor.
//
// The token is `elmrt_<payload>.<mac>`: base64url JSON claims and an
// HMAC-SHA256 over the prefixed payload. The key is derived from the
// deployment's personal access token signing key (DeriveKey), so it needs no
// new secret, and a PAT JWT verifier can never accept it (and vice versa):
// the formats and keys differ.
package runtoken

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// Prefix marks a run token.
const Prefix = "elmrt_"

// The audiences a run token carries, one per route group that accepts it.
const (
	// AudienceModelGateway is the /llm route group.
	AudienceModelGateway = "elitea-model-gateway"
	// AudienceArtifacts is the run project's artifact routes the worker's
	// ArtifactObjectService calls reach.
	AudienceArtifacts = "elitea-artifacts"
)

// audiences is what Issue signs: every audience, in this order.
var audiences = []string{AudienceModelGateway, AudienceArtifacts}

// MaxLifetime is the signed ceiling. The lease is the real expiry; this only
// bounds a token whose claim row outlived its run. It equals the runtime's
// agent deadline, so no admitted turn outlives its token.
const MaxLifetime = 24 * time.Hour

// minKeyBytes is the smallest signing key accepted.
const minKeyBytes = 32

// maxTokenBytes bounds what Verify reads before decoding anything.
const maxTokenBytes = 4096

const tokenVersion = 2

// keyDerivationLabel separates this key from every other use of the secret
// it is derived from.
const keyDerivationLabel = "elitea/run-token/v1"

// Binding is what a run token names: the run's claim, its project and its
// actor (with the actor's personal access token row).
type Binding struct {
	ExecutionID string
	Generation  uint64
	ClaimID     string
	FenceToken  []byte
	ProjectID   int64
	UserID      int64
	TokenID     int64
}

func (b Binding) valid() bool {
	return b.ExecutionID != "" && len(b.ExecutionID) <= 256 &&
		b.Generation > 0 && b.Generation <= math.MaxInt64 &&
		b.ClaimID != "" && len(b.ClaimID) <= 256 &&
		len(b.FenceToken) == 32 &&
		b.ProjectID > 0 && b.UserID > 0 && b.TokenID > 0
}

type claims struct {
	Version     int      `json:"v"`
	Audiences   []string `json:"aud"`
	ExecutionID string   `json:"exe"`
	Generation  uint64   `json:"gen"`
	ClaimID     string   `json:"cid"`
	Fence       string   `json:"fen"`
	ProjectID   int64    `json:"prj"`
	UserID      int64    `json:"uid"`
	TokenID     int64    `json:"tid"`
	IssuedAt    int64    `json:"iat"`
	ExpiresAt   int64    `json:"exp"`
}

// DeriveKey derives the run token key from secret (the deployment's personal
// access token signing key). It returns nil for an empty secret.
func DeriveKey(secret []byte) []byte {
	if len(secret) == 0 {
		return nil
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(keyDerivationLabel))
	return mac.Sum(nil)
}

// Signer issues and verifies run tokens under one key.
type Signer struct {
	key []byte
	now func() time.Time
}

// NewSigner returns a signer over key (see DeriveKey).
func NewSigner(key []byte) (*Signer, error) {
	if len(key) < minKeyBytes {
		return nil, errors.New("runtoken: the signing key is too short")
	}
	return &Signer{key: append([]byte(nil), key...), now: time.Now}, nil
}

// Issue mints the token for binding, for every audience.
func (s *Signer) Issue(binding Binding) (string, error) {
	if s == nil {
		return "", errors.New("runtoken: no signer")
	}
	if !binding.valid() {
		return "", errors.New("runtoken: the binding is incomplete")
	}
	now := s.now()
	payload, err := json.Marshal(claims{
		Version:     tokenVersion,
		Audiences:   audiences,
		ExecutionID: binding.ExecutionID,
		Generation:  binding.Generation,
		ClaimID:     binding.ClaimID,
		Fence:       base64.RawURLEncoding.EncodeToString(binding.FenceToken),
		ProjectID:   binding.ProjectID,
		UserID:      binding.UserID,
		TokenID:     binding.TokenID,
		IssuedAt:    now.Unix(),
		ExpiresAt:   now.Add(MaxLifetime).Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("runtoken: encode: %w", err)
	}
	body := Prefix + base64.RawURLEncoding.EncodeToString(payload)
	return body + "." + base64.RawURLEncoding.EncodeToString(s.mac(body)), nil
}

// Verify checks the token's signature, that it carries audience, and its
// signed ceiling. It does not read the lease: Validator does. Every refusal
// wraps auth.ErrCredentialRejected.
func (s *Signer) Verify(token, audience string) (Binding, error) {
	if s == nil || !HasPrefix(token) || len(token) > maxTokenBytes {
		return Binding{}, rejected("malformed")
	}
	dot := strings.LastIndexByte(token, '.')
	if dot <= len(Prefix) {
		return Binding{}, rejected("malformed")
	}
	body, signature := token[:dot], token[dot+1:]
	mac, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || !hmac.Equal(mac, s.mac(body)) {
		return Binding{}, rejected("signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(body[len(Prefix):])
	if err != nil {
		return Binding{}, rejected("malformed")
	}
	var decoded claims
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return Binding{}, rejected("malformed")
	}
	if decoded.Version != tokenVersion || audience == "" || !slices.Contains(decoded.Audiences, audience) {
		return Binding{}, rejected("audience")
	}
	now := s.now().Unix()
	if decoded.ExpiresAt <= now || decoded.IssuedAt > now+60 ||
		decoded.ExpiresAt-decoded.IssuedAt > int64(MaxLifetime/time.Second) {
		return Binding{}, rejected("expired")
	}
	fence, err := base64.RawURLEncoding.DecodeString(decoded.Fence)
	if err != nil {
		return Binding{}, rejected("malformed")
	}
	binding := Binding{
		ExecutionID: decoded.ExecutionID,
		Generation:  decoded.Generation,
		ClaimID:     decoded.ClaimID,
		FenceToken:  fence,
		ProjectID:   decoded.ProjectID,
		UserID:      decoded.UserID,
		TokenID:     decoded.TokenID,
	}
	if !binding.valid() {
		return Binding{}, rejected("malformed")
	}
	return binding, nil
}

func (s *Signer) mac(body string) []byte {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(body))
	return mac.Sum(nil)
}

// HasPrefix reports whether token is shaped as a run token. It is the test
// the Auth middleware applies before any validator runs.
func HasPrefix(token string) bool { return strings.HasPrefix(token, Prefix) }

func rejected(reason string) error {
	return fmt.Errorf("%w: run token %s", auth.ErrCredentialRejected, reason)
}

// LeaseReader answers whether the binding's claim is live: unreleased, its
// lease unexpired, for the bound execution, generation, fence, project and
// actor. An error means the store did not answer.
type LeaseReader interface {
	ClaimLive(ctx context.Context, binding Binding) (bool, error)
}

// Validator authenticates a run token: signature, audience and ceiling
// (Signer), then the live claim (LeaseReader) on every request.
type Validator struct {
	signer *Signer
	leases LeaseReader
}

// NewValidator composes the validator.
func NewValidator(signer *Signer, leases LeaseReader) (*Validator, error) {
	if signer == nil || leases == nil {
		return nil, errors.New("runtoken: the signer and the lease reader are required")
	}
	return &Validator{signer: signer, leases: leases}, nil
}

// ValidateRunToken returns the principal the token names and its binding,
// for a route group serving audience. The principal is shaped as a validated
// personal access token principal (the actor and the actor's token row), so
// the principal validator re-reads both and a suspended actor or revoked
// token loses access too. A refusal wraps auth.ErrCredentialRejected; a
// store fault wraps auth.ErrCredentialValidationUnavailable.
func (v *Validator) ValidateRunToken(ctx context.Context, token, audience string) (auth.User, Binding, error) {
	if v == nil {
		return auth.User{}, Binding{}, rejected("validator absent")
	}
	binding, err := v.signer.Verify(token, audience)
	if err != nil {
		return auth.User{}, Binding{}, err
	}
	live, err := v.leases.ClaimLive(ctx, binding)
	if err != nil {
		return auth.User{}, Binding{}, fmt.Errorf("%w: run token lease: %w",
			auth.ErrCredentialValidationUnavailable, err)
	}
	if !live {
		return auth.User{}, Binding{}, rejected("lease")
	}
	userID := strconv.FormatInt(binding.UserID, 10)
	return auth.User{
		ID:       userID,
		UserID:   userID,
		TokenID:  strconv.FormatInt(binding.TokenID, 10),
		AuthType: "token",
	}, binding, nil
}
