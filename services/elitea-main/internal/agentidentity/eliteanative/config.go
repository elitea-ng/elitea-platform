// Package eliteanative is the Elitea-native agent identity provider
// (ADR-0039 decision 12, issue #1153): Elitea is the issuer of a short-lived,
// per-run agent assertion for exactly one relying party. It is the
// `elitea_native` identity mode and the federation credential that relying
// parties such as AWS AssumeRoleWithWebIdentity, GCP Workload Identity
// Federation, Keycloak federated client authentication, Vault JWT auth and
// MCP servers accept.
//
// The signing key ring is dedicated to this purpose: it signs nothing else,
// and boot refuses a ring that reuses another purpose's key id or key
// material. The private key never leaves its KeySigner. Only the Provider
// signs, and only agentidentity.Broker reaches the Provider; no HTTP route
// signs on request. The JWKS and the issuer metadata are public and carry no
// tenant data.
package eliteanative

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Fixed names of the provider.
const (
	// ProviderName is the registry key and ExternalRef.Provider.
	ProviderName = "elitea"
	// RefKind is ExternalRef.Kind of every Elitea principal.
	RefKind = "elitea.agent"
	// Purpose names the key ring in boot checks and errors.
	Purpose = "agent-assertion"
	// TokenType is the JWS `typ` header of every assertion.
	TokenType = "elitea-agent+jwt"
	// IssuerPath is appended to the origin to form the issuer, so the
	// issuer metadata sits where OIDC discovery looks for it.
	IssuerPath = "/agent-issuer"
	// MetadataPath serves the issuer metadata document.
	MetadataPath = IssuerPath + "/.well-known/openid-configuration"
	// JWKSPath serves the public key set.
	JWKSPath = "/.well-known/elitea-agent-jwks.json"
	// EnvConfig names the JSON configuration (ParseConfig). Read only with
	// credential governance on; empty leaves the provider unconfigured.
	EnvConfig = "ELITEA_AGENT_ASSERTION_CONFIG"
)

// Bounds of the configuration.
const (
	// MaxTTL is the longest assertion any relying party may be configured
	// for.
	MaxTTL = 15 * time.Minute
	// DefaultTTL is a relying party's max_ttl when it sets none.
	DefaultTTL = 10 * time.Minute
	// MinTTL is the shortest assertion signed, and the shortest max_ttl.
	MinTTL = 30 * time.Second
	// DefaultRotationOverlap is how long a new key is published before it
	// signs.
	DefaultRotationOverlap = 24 * time.Hour
	// CacheMaxAge is how long a relying party may cache the JWKS and the
	// issuer metadata. The rotation overlap must be strictly longer, so a
	// cache that fetched just before a key was published has refreshed
	// before that key signs.
	CacheMaxAge        = 5 * time.Minute
	maxRotationOverlap = 30 * 24 * time.Hour
	// retentionSkew keeps a retired key published a little past the expiry
	// of its last assertion, for relying parties with clock skew.
	retentionSkew = time.Minute

	maxConfigBytes    = 64 * 1024
	maxRelyingParties = 64
	maxKeys           = 16
	maxAudienceBytes  = 256
	maxTenantBytes    = 128
)

// Algorithm is a JWS signing algorithm. EdDSA is not offered: AWS and GCP
// federation do not accept it.
type Algorithm string

const (
	ES256 Algorithm = "ES256"
	RS256 Algorithm = "RS256"
)

func (a Algorithm) valid() bool { return a == ES256 || a == RS256 }

// ClaimsProfile names the relying party family. It selects the exchange
// adapter (#1155); the assertion's claims are the same for every profile.
type ClaimsProfile string

const (
	ProfileAWS      ClaimsProfile = "aws"
	ProfileGCPWIF   ClaimsProfile = "gcp_wif"
	ProfileKeycloak ClaimsProfile = "keycloak"
	ProfileVaultJWT ClaimsProfile = "vault_jwt"
	ProfileMCP      ClaimsProfile = "mcp"
)

func (p ClaimsProfile) valid() bool {
	switch p {
	case ProfileAWS, ProfileGCPWIF, ProfileKeycloak, ProfileVaultJWT, ProfileMCP:
		return true
	}
	return false
}

// RelyingParty is one party that accepts Elitea assertions. Audience is
// matched exactly; a credential request for any other audience is denied.
type RelyingParty struct {
	ID            string
	Audience      string
	Algorithm     Algorithm
	MaxTTL        time.Duration
	ClaimsProfile ClaimsProfile
}

// KeySpec is one key of the ring as configured. Exactly one source is set.
type KeySpec struct {
	KeyID     string
	Algorithm Algorithm
	// NotBefore is when the key enters the JWKS. It signs from NotBefore +
	// the rotation overlap. Zero: an established key, published and usable
	// at once. At most one key per algorithm may leave it zero.
	NotBefore time.Time
	// File is a mounted PKCS#8 PEM private key (a Kubernetes Secret).
	File string
	// Vault is a KV v2 secret read through the SecretStore seam.
	Vault *VaultKeyRef
}

// VaultKeyRef locates a PKCS#8 PEM private key in Vault/OpenBao KV v2. The
// version is explicit: SecretStore has no "latest" read.
type VaultKeyRef struct {
	Mount   string
	Path    string
	Version int64
}

// Config is the operator configuration of the provider.
type Config struct {
	// Origin is the stable https origin the issuer and the JWKS URL are
	// built on.
	Origin string
	// Tenant is the elitea_tenant claim. Defaults to the origin's host.
	Tenant          string
	RotationOverlap time.Duration
	Keys            []KeySpec
	RelyingParties  []RelyingParty
}

// Issuer is the `iss` of every assertion.
func (c Config) Issuer() string { return c.Origin + IssuerPath }

// JWKSURI is the jwks_uri of the issuer metadata.
func (c Config) JWKSURI() string { return c.Origin + JWKSPath }

// retention is how long a superseded key stays in the JWKS: the longest
// assertion any relying party accepts, plus skew.
func (c Config) retention() time.Duration {
	longest := time.Duration(0)
	for _, rp := range c.RelyingParties {
		longest = max(longest, rp.MaxTTL)
	}
	return longest + retentionSkew
}

// validOverlap: strictly longer than the cache max-age, at most 30 days.
func validOverlap(d time.Duration) bool { return d > CacheMaxAge && d <= maxRotationOverlap }

// ErrInvalidConfig wraps every configuration refusal. The text names the
// field, never a key or a path's contents.
var ErrInvalidConfig = errors.New("elitea-native agent identity: invalid configuration")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidConfig, fmt.Sprintf(format, args...))
}

var (
	relyingPartyIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	keyIDPattern          = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	vaultPathPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,255}$`)
)

type rawConfig struct {
	Origin          string            `json:"origin"`
	Tenant          string            `json:"tenant"`
	RotationOverlap string            `json:"rotation_overlap"`
	Keys            []rawKey          `json:"keys"`
	RelyingParties  []rawRelyingParty `json:"relying_parties"`
}

type rawKey struct {
	KeyID     string       `json:"kid"`
	Algorithm string       `json:"alg"`
	NotBefore string       `json:"not_before"`
	File      string       `json:"file"`
	Vault     *rawVaultKey `json:"vault"`
}

type rawVaultKey struct {
	Mount   string `json:"mount"`
	Path    string `json:"path"`
	Version int64  `json:"version"`
}

type rawRelyingParty struct {
	ID            string `json:"id"`
	Audience      string `json:"audience"`
	Algorithm     string `json:"algorithm"`
	MaxTTL        string `json:"max_ttl"`
	ClaimsProfile string `json:"claims_profile"`
}

// ParseConfig reads and validates the JSON configuration. Unknown fields
// are refused, so a misspelt field never silently takes a default.
// defaultOrigin is used when the document names no origin (the
// deployment's public origin; empty when it has none).
func ParseConfig(raw []byte, defaultOrigin string) (Config, error) {
	if len(raw) == 0 || len(raw) > maxConfigBytes {
		return Config{}, invalid("the document must be 1..%d bytes", maxConfigBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var in rawConfig
	if err := decoder.Decode(&in); err != nil {
		return Config{}, invalid("the document is not valid JSON of the expected shape")
	}
	if decoder.More() {
		return Config{}, invalid("the document holds more than one JSON value")
	}
	cfg := Config{Origin: strings.TrimSpace(in.Origin), Tenant: strings.TrimSpace(in.Tenant), RotationOverlap: DefaultRotationOverlap}
	if cfg.Origin == "" {
		cfg.Origin = defaultOrigin
	}
	if in.RotationOverlap != "" {
		d, err := time.ParseDuration(in.RotationOverlap)
		if err != nil {
			return Config{}, invalid("rotation_overlap is not a duration")
		}
		cfg.RotationOverlap = d
	}
	for i, k := range in.Keys {
		spec := KeySpec{KeyID: k.KeyID, Algorithm: Algorithm(k.Algorithm), File: k.File}
		if k.NotBefore != "" {
			t, err := time.Parse(time.RFC3339, k.NotBefore)
			if err != nil {
				return Config{}, invalid("keys[%d].not_before is not an RFC 3339 time", i)
			}
			spec.NotBefore = t.UTC()
		}
		if k.Vault != nil {
			spec.Vault = &VaultKeyRef{Mount: k.Vault.Mount, Path: k.Vault.Path, Version: k.Vault.Version}
		}
		cfg.Keys = append(cfg.Keys, spec)
	}
	for i, rp := range in.RelyingParties {
		party := RelyingParty{ID: rp.ID, Audience: rp.Audience, Algorithm: Algorithm(rp.Algorithm),
			MaxTTL: DefaultTTL, ClaimsProfile: ClaimsProfile(rp.ClaimsProfile)}
		if party.Algorithm == "" {
			party.Algorithm = ES256
		}
		if rp.MaxTTL != "" {
			d, err := time.ParseDuration(rp.MaxTTL)
			if err != nil {
				return Config{}, invalid("relying_parties[%d].max_ttl is not a duration", i)
			}
			party.MaxTTL = d
		}
		cfg.RelyingParties = append(cfg.RelyingParties, party)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks the configuration and fills Tenant's default.
func (c *Config) Validate() error {
	origin, err := url.Parse(c.Origin)
	if c.Origin == "" || err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil ||
		(origin.Path != "" && origin.Path != "/") || origin.RawQuery != "" || origin.Fragment != "" {
		return invalid("origin must be an https origin with no path; set it or DEPLOYMENT_URL")
	}
	c.Origin = "https://" + origin.Host
	if c.Tenant == "" {
		c.Tenant = origin.Hostname()
	}
	if len(c.Tenant) > maxTenantBytes || strings.ContainsAny(c.Tenant, " \t\r\n\x00") {
		return invalid("tenant must be at most %d bytes without whitespace", maxTenantBytes)
	}
	if !validOverlap(c.RotationOverlap) {
		return invalid("rotation_overlap must be more than the JWKS cache max-age (%s) and at most %s", CacheMaxAge, maxRotationOverlap)
	}
	if len(c.RelyingParties) == 0 || len(c.RelyingParties) > maxRelyingParties {
		return invalid("1..%d relying_parties are required", maxRelyingParties)
	}
	if len(c.Keys) == 0 || len(c.Keys) > maxKeys {
		return invalid("1..%d keys are required", maxKeys)
	}
	kids := map[string]bool{}
	established := map[Algorithm]bool{}
	algorithms := map[Algorithm]bool{}
	for i, k := range c.Keys {
		switch {
		case !keyIDPattern.MatchString(k.KeyID):
			return invalid("keys[%d].kid must be 1..64 of [A-Za-z0-9._-]", i)
		case kids[k.KeyID]:
			return invalid("keys[%d].kid is a duplicate", i)
		case !k.Algorithm.valid():
			return invalid("keys[%d].alg must be ES256 or RS256", i)
		case (k.File == "") == (k.Vault == nil):
			return invalid("keys[%d] needs exactly one source: file or vault", i)
		case k.File != "" && !strings.HasPrefix(k.File, "/"):
			return invalid("keys[%d].file must be an absolute path", i)
		case k.Vault != nil && (!vaultPathPattern.MatchString(k.Vault.Mount) || !vaultPathPattern.MatchString(k.Vault.Path) || k.Vault.Version <= 0):
			return invalid("keys[%d].vault needs a mount, a path and an explicit version", i)
		}
		if k.NotBefore.IsZero() {
			if established[k.Algorithm] {
				return invalid("keys[%d]: at most one %s key may omit not_before", i, k.Algorithm)
			}
			established[k.Algorithm] = true
		}
		kids[k.KeyID] = true
		algorithms[k.Algorithm] = true
	}
	ids := map[string]bool{}
	audiences := map[string]bool{}
	for i, rp := range c.RelyingParties {
		switch {
		case !relyingPartyIDPattern.MatchString(rp.ID):
			return invalid("relying_parties[%d].id must be 1..63 of [a-z0-9_-]", i)
		case ids[rp.ID]:
			return invalid("relying_parties[%d].id is a duplicate", i)
		case rp.Audience == "" || len(rp.Audience) > maxAudienceBytes || strings.ContainsAny(rp.Audience, " \t\r\n\x00"):
			return invalid("relying_parties[%d].audience must be 1..%d bytes without whitespace", i, maxAudienceBytes)
		case audiences[rp.Audience]:
			return invalid("relying_parties[%d].audience is configured twice; an audience names one relying party", i)
		case !rp.Algorithm.valid():
			return invalid("relying_parties[%d].algorithm must be ES256 or RS256", i)
		case !algorithms[rp.Algorithm]:
			return invalid("relying_parties[%d] uses %s but the key ring has no %s key", i, rp.Algorithm, rp.Algorithm)
		case rp.MaxTTL < MinTTL || rp.MaxTTL > MaxTTL:
			return invalid("relying_parties[%d].max_ttl must be %s..%s", i, MinTTL, MaxTTL)
		case !rp.ClaimsProfile.valid():
			return invalid("relying_parties[%d].claims_profile must be aws, gcp_wif, keycloak, vault_jwt or mcp", i)
		}
		ids[rp.ID] = true
		audiences[rp.Audience] = true
	}
	return nil
}
