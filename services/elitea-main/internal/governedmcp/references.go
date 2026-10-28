package governedmcp

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/provideroauth"
)

// ReferencePrefix starts an MCP authorization_reference. Like the Entra
// requirement's "car_" reference it is an opaque, URL-safe token bound to
// one user and tenant project; the prefix tells the two apart.
const ReferencePrefix = "mar_"

// DefaultReferenceTTL is how long an authorization_reference can start a
// consent. A test value, not a customer requirement (gate D5).
const DefaultReferenceTTL = time.Hour

const (
	referenceLabel   = "elitea/governed-mcp-authorization-reference/v1"
	maxReferenceSize = 2048
)

// ErrReferenceInvalid is a reference that is malformed, expired, sealed
// under another key, or for another user or tenant project.
var ErrReferenceInvalid = errors.New("governed MCP: the authorization reference is not valid")

// References mints and opens the authorization_reference of an
// AUTHORIZATION_REQUIRED denial for an MCP grant (#1123). The reference is
// stateless: the requirement is sealed into it with AES-256-GCM under a key
// derived from the deployment master key, so no table is written and a
// reference is useless outside this deployment.
//
// Compatibility with #1061: the field is the same `authorization_reference`
// the native MCP OAuth start answers, the value is an opaque printable
// token (the worker's bound is 4 KiB), and it resolves server side to the
// consent start (tenant project, agent binding, target binding) for the
// same user: POST /api/v2/credential_consent/mcp:start takes it as
// authorization_reference and opens it with Open (credentialconsent).
type References struct {
	aead cipher.AEAD
	ttl  time.Duration
	now  func() time.Time
}

var _ provideroauth.AuthorizationReferences = (*References)(nil)

// NewReferences derives the sealing key from the 32-byte master key.
func NewReferences(masterKey []byte, ttl time.Duration) (*References, error) {
	if len(masterKey) != 32 {
		return nil, errors.New("governed MCP: authorization references need the 32-byte master key")
	}
	key, err := hkdf.Key(sha256.New, masterKey, nil, referenceLabel, 32)
	if err != nil {
		return nil, fmt.Errorf("governed MCP: derive the reference key: %w", err)
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
	if ttl <= 0 {
		ttl = DefaultReferenceTTL
	}
	return &References{aead: aead, ttl: ttl, now: time.Now}, nil
}

type sealedRequirement struct {
	TenantProjectID   int64  `json:"t"`
	TargetBindingID   string `json:"tb"`
	AgentBindingID    string `json:"ab"`
	SubjectUserID     int64  `json:"u"`
	ExecutionID       string `json:"e"`
	Generation        uint64 `json:"g"`
	ConversationUUID  string `json:"c"`
	ResponseMessageID string `json:"m"`
	ExpiresAt         int64  `json:"exp"`
}

// Mint answers the reference of an MCP grant requirement. Any other
// provider is provideroauth.ErrProviderNotConfigured.
func (r *References) Mint(_ context.Context, req provideroauth.AuthorizationRequirement) (string, error) {
	if req.Key.Provider != ProviderType {
		return "", provideroauth.ErrProviderNotConfigured
	}
	if req.Key.TenantProjectID <= 0 || req.Key.SubjectUserID <= 0 || req.Key.TargetBindingID == "" ||
		req.Key.AgentBindingID == "" || req.ExecutionID == "" {
		return "", provideroauth.ErrInvalid
	}
	plaintext, err := json.Marshal(sealedRequirement{
		TenantProjectID: req.Key.TenantProjectID, TargetBindingID: req.Key.TargetBindingID,
		AgentBindingID: req.Key.AgentBindingID, SubjectUserID: req.Key.SubjectUserID,
		ExecutionID: req.ExecutionID, Generation: req.Generation,
		ConversationUUID: req.ConversationUUID, ResponseMessageID: req.ResponseMessageID,
		ExpiresAt: r.now().Add(r.ttl).UnixMilli(),
	})
	if err != nil {
		return "", provideroauth.ErrInvalid
	}
	sealed := r.aead.Seal(nil, nil, plaintext, []byte(referenceLabel))
	reference := ReferencePrefix + base64.RawURLEncoding.EncodeToString(sealed)
	if len(reference) > maxReferenceSize {
		return "", provideroauth.ErrInvalid
	}
	return reference, nil
}

// Open resolves a reference for the authenticated user in the tenant
// project it names. Another user, another project, an expired reference
// or a foreign key is ErrReferenceInvalid.
func (r *References) Open(reference string, tenantProjectID, userID int64) (provideroauth.AuthorizationRequirement, error) {
	encoded, ok := strings.CutPrefix(reference, ReferencePrefix)
	if !ok || len(reference) > maxReferenceSize {
		return provideroauth.AuthorizationRequirement{}, ErrReferenceInvalid
	}
	sealed, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return provideroauth.AuthorizationRequirement{}, ErrReferenceInvalid
	}
	plaintext, err := r.aead.Open(nil, nil, sealed, []byte(referenceLabel))
	if err != nil {
		return provideroauth.AuthorizationRequirement{}, ErrReferenceInvalid
	}
	var s sealedRequirement
	if json.Unmarshal(plaintext, &s) != nil || s.TenantProjectID != tenantProjectID || s.SubjectUserID != userID ||
		!r.now().Before(time.UnixMilli(s.ExpiresAt)) {
		return provideroauth.AuthorizationRequirement{}, ErrReferenceInvalid
	}
	return provideroauth.AuthorizationRequirement{
		Key: provideroauth.GrantKey{
			TenantProjectID: s.TenantProjectID, Provider: ProviderType, TargetBindingID: s.TargetBindingID,
			SubjectUserID: s.SubjectUserID, AgentBindingID: s.AgentBindingID,
		},
		ExecutionID: s.ExecutionID, Generation: s.Generation,
		ConversationUUID: s.ConversationUUID, ResponseMessageID: s.ResponseMessageID,
		ExpiresAt: time.UnixMilli(s.ExpiresAt).UTC(),
	}, nil
}
