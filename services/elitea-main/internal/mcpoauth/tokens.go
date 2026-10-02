package mcpoauth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrTokenUnavailable = errors.New("delegated authorization unavailable")

// TokenBinding comes from authenticated identity and saved toolkit settings.
type TokenBinding struct {
	ProjectID int32
	ActorID   int32
	ToolkitID int64
	Resource  string
}

// TokenReference identifies one immutable grant. Refresh creates another reference.
type TokenReference struct {
	Reference string
	Revision  int64
	ExpiresAt time.Time
}

// AccessToken is transient credential material. Never put it in durable input.
type AccessToken struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	SessionID   string `json:"session_id,omitempty"`
}

// Tokens shares deployment key ownership with Clients, with a separate derived key.
// PostgreSQL owns expiry, revocation, and visibility across Main replicas.
type Tokens struct {
	pool *pgxpool.Pool
	aead cipher.AEAD
}

func NewTokens(pool *pgxpool.Pool, masterKey []byte) (*Tokens, error) {
	if pool == nil || len(masterKey) != 32 {
		return nil, errors.New("delegated token storage requires a database and a 32-byte master key")
	}
	key, err := hkdf.Key(sha256.New, masterKey, nil, "elitea/mcp-oauth-tokens/v1", 32)
	if err != nil {
		return nil, fmt.Errorf("derive delegated token key: %w", err)
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	return &Tokens{pool: pool, aead: aead}, nil
}

func validTokenBinding(b TokenBinding) bool {
	if b.ProjectID <= 0 || b.ActorID <= 0 || b.ToolkitID <= 0 || len(b.Resource) > 4096 || strings.ContainsAny(b.Resource, "\x00\r\n") {
		return false
	}
	u, err := url.Parse(b.Resource)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Fragment == ""
}

func tokenAAD(reference string, binding TokenBinding, expires time.Time) []byte {
	data, _ := json.Marshal(struct {
		Reference string
		Revision  int64
		Binding   TokenBinding
		Expires   int64
	}{reference, 1, binding, expires.UnixMicro()})
	return data
}

func validToken(t AccessToken) bool {
	return t.AccessToken != "" && len(t.AccessToken) <= 16384 && !strings.ContainsAny(t.AccessToken, "\x00\r\n") && strings.EqualFold(t.TokenType, "Bearer") && len(t.SessionID) <= 4096 && !strings.ContainsAny(t.SessionID, "\x00\r\n")
}

// Save accepts only a verified provider response. It never stores refresh tokens.
func (s *Tokens) Save(ctx context.Context, binding TokenBinding, token AccessToken, expires time.Time) (TokenReference, error) {
	if s == nil || s.pool == nil || s.aead == nil {
		return TokenReference{}, ErrTokenUnavailable
	}
	expires = expires.UTC().Truncate(time.Microsecond)
	if !validTokenBinding(binding) || !validToken(token) || !expires.After(time.Now()) || expires.After(time.Now().Add(24*time.Hour)) {
		return TokenReference{}, ErrTokenUnavailable
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return TokenReference{}, err
	}
	ref := TokenReference{Reference: base64.RawURLEncoding.EncodeToString(random[:]), Revision: 1, ExpiresAt: expires}
	plaintext, err := json.Marshal(token)
	if err != nil {
		return TokenReference{}, ErrTokenUnavailable
	}
	encrypted := s.aead.Seal(nil, nil, plaintext, tokenAAD(ref.Reference, binding, expires))
	clear(plaintext)
	// Bound cleanup work per exchange. References are never renewed in place.
	_, err = s.pool.Exec(ctx, `WITH expired AS (
 SELECT id FROM elitea_auth.mcp_oauth_tokens WHERE expires_at <= now() ORDER BY expires_at,id LIMIT 128 FOR UPDATE SKIP LOCKED
 ) DELETE FROM elitea_auth.mcp_oauth_tokens WHERE id IN (SELECT id FROM expired)`)
	if err != nil {
		return TokenReference{}, fmt.Errorf("prune delegated grants: %w", err)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO elitea_auth.mcp_oauth_tokens
 (id,project_id,actor_id,toolkit_id,resource,revision,encrypted_token,expires_at) VALUES ($1,$2,$3,$4,$5,1,$6,$7)`, ref.Reference, binding.ProjectID, binding.ActorID, binding.ToolkitID, binding.Resource, encrypted, expires)
	if err != nil {
		return TokenReference{}, fmt.Errorf("store delegated grant: %w", err)
	}
	return ref, nil
}

// Validate checks admission metadata without returning or decrypting a token.
func (s *Tokens) Validate(ctx context.Context, reference string, binding TokenBinding) (TokenReference, error) {
	ref, _, err := s.read(ctx, reference, binding, false)
	return ref, err
}

// Load is an internal materializer operation after live execution claim authorization.
// Possession of a browser reference alone must never reach this method.
func (s *Tokens) Load(ctx context.Context, reference string, binding TokenBinding) (AccessToken, error) {
	ref, encrypted, err := s.read(ctx, reference, binding, true)
	if err != nil {
		return AccessToken{}, err
	}
	plaintext, err := s.aead.Open(nil, nil, encrypted, tokenAAD(reference, binding, ref.ExpiresAt))
	if err != nil {
		return AccessToken{}, ErrTokenUnavailable
	}
	defer clear(plaintext)
	var token AccessToken
	if json.Unmarshal(plaintext, &token) != nil || !validToken(token) {
		return AccessToken{}, ErrTokenUnavailable
	}
	return token, nil
}

func (s *Tokens) read(ctx context.Context, reference string, binding TokenBinding, includeToken bool) (TokenReference, []byte, error) {
	if s == nil || s.pool == nil || s.aead == nil {
		return TokenReference{}, nil, ErrTokenUnavailable
	}
	raw, err := base64.RawURLEncoding.DecodeString(reference)
	if !validTokenBinding(binding) || len(raw) != 32 || err != nil {
		return TokenReference{}, nil, ErrTokenUnavailable
	}
	ref := TokenReference{Reference: reference}
	var encrypted []byte
	err = s.pool.QueryRow(ctx, `SELECT revision,expires_at,CASE WHEN $6 THEN encrypted_token ELSE NULL END
 FROM elitea_auth.mcp_oauth_tokens WHERE id=$1 AND project_id=$2 AND actor_id=$3 AND toolkit_id=$4 AND resource=$5
 AND revoked_at IS NULL AND expires_at > now() AND revision=1`, reference, binding.ProjectID, binding.ActorID, binding.ToolkitID, binding.Resource, includeToken).Scan(&ref.Revision, &ref.ExpiresAt, &encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenReference{}, nil, ErrTokenUnavailable
	}
	if err != nil {
		return TokenReference{}, nil, fmt.Errorf("read delegated grant: %w", err)
	}
	return ref, encrypted, nil
}

// Revoke is idempotent and cannot affect another actor or resource.
func (s *Tokens) Revoke(ctx context.Context, reference string, binding TokenBinding) error {
	if s == nil || s.pool == nil || s.aead == nil || !validTokenBinding(binding) || len(reference) != 43 {
		return ErrTokenUnavailable
	}
	_, err := s.pool.Exec(ctx, `UPDATE elitea_auth.mcp_oauth_tokens SET revoked_at=COALESCE(revoked_at,now())
 WHERE id=$1 AND project_id=$2 AND actor_id=$3 AND toolkit_id=$4 AND resource=$5`, reference, binding.ProjectID, binding.ActorID, binding.ToolkitID, binding.Resource)
	return err
}
