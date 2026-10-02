// Package mcpoauth stores encrypted MCP OAuth credentials.
// Clients owns registration secrets. Tokens owns delegated access grants.
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
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrClientUnavailable = errors.New("DCR client unavailable")

// Binding must match exactly on every code exchange and refresh.
// Actor and project come from authenticated Main request context.
type Binding struct {
	ProjectID     int32
	ActorID       int32
	ClientID      string
	TokenEndpoint string
	Resource      string
}

// Clients is safe for concurrent use. PostgreSQL owns lifetime and replica visibility.
type Clients struct {
	queries *sqlcgen.Queries
	aead    cipher.AEAD
}

// NewClients requires the decoded 32-byte deployment master key.
// No unencrypted fallback is available when the key is absent.
func NewClients(pool *pgxpool.Pool, masterKey []byte) (*Clients, error) {
	if pool == nil || len(masterKey) != 32 {
		return nil, errors.New("DCR client storage requires a database and a 32-byte master key")
	}
	key, err := hkdf.Key(sha256.New, masterKey, nil, "elitea/mcp-oauth-clients/v1", 32)
	if err != nil {
		return nil, fmt.Errorf("derive DCR storage key: %w", err)
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create DCR storage cipher: %w", err)
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, fmt.Errorf("create DCR storage AEAD: %w", err)
	}
	return &Clients{queries: sqlcgen.New(pool), aead: aead}, nil
}

func validBinding(binding Binding) bool {
	if binding.ProjectID <= 0 || binding.ActorID <= 0 {
		return false
	}
	for _, value := range []string{binding.ClientID, binding.TokenEndpoint} {
		if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
			return false
		}
	}
	return len(binding.Resource) <= 4096 && !strings.ContainsAny(binding.Resource, "\x00\r\n")
}

// aad binds ciphertext to the record and every credential ownership field.
// JSON is authenticated data, not a signed wire canonicalization contract.
func aad(reference string, binding Binding) []byte {
	data, _ := json.Marshal(struct {
		Reference string
		Binding   Binding
	}{reference, binding})
	return data
}

func (c *Clients) Save(ctx context.Context, binding Binding, secret string, expires time.Time) (string, error) {
	if !validBinding(binding) || secret == "" || len(secret) > 16384 || strings.ContainsAny(secret, "\x00\r\n") {
		return "", ErrClientUnavailable
	}
	if !expires.IsZero() && !expires.After(time.Now()) {
		return "", ErrClientUnavailable
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("create DCR client reference: %w", err)
	}
	reference := base64.RawURLEncoding.EncodeToString(random[:])
	plaintext := []byte(secret)
	encrypted := c.aead.Seal(nil, nil, plaintext, aad(reference, binding))
	clear(plaintext)
	if err := c.queries.PruneMCPOAuthClients(ctx); err != nil {
		return "", fmt.Errorf("prune DCR clients: %w", err)
	}
	err := c.queries.InsertMCPOAuthClient(ctx, sqlcgen.InsertMCPOAuthClientParams{
		ID: reference, ProjectID: binding.ProjectID, ActorID: binding.ActorID,
		ClientID: binding.ClientID, TokenEndpoint: binding.TokenEndpoint, Resource: binding.Resource,
		EncryptedCredentials: encrypted,
		SecretExpiresAt:      pgtype.Timestamptz{Time: expires, Valid: !expires.IsZero()},
	})
	if err != nil {
		return "", fmt.Errorf("store DCR client: %w", err)
	}
	return reference, nil
}

func (c *Clients) Load(ctx context.Context, reference string, binding Binding) (string, error) {
	if !validBinding(binding) || len(reference) != 43 {
		return "", ErrClientUnavailable
	}
	encrypted, err := c.queries.LoadMCPOAuthClient(ctx, sqlcgen.LoadMCPOAuthClientParams{
		ID: reference, ProjectID: binding.ProjectID, ActorID: binding.ActorID,
		ClientID: binding.ClientID, TokenEndpoint: binding.TokenEndpoint, Resource: binding.Resource,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrClientUnavailable
	}
	if err != nil {
		return "", fmt.Errorf("load DCR client: %w", err)
	}
	plaintext, err := c.aead.Open(nil, nil, encrypted, aad(reference, binding))
	if err != nil {
		return "", ErrClientUnavailable
	}
	defer clear(plaintext)
	return string(plaintext), nil
}
