package eliteanative

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/idna"
)

// Delegation assertions (ADR-0044 decision 4, #1501). A run that calls an
// agent outside the run (a remote MCP server, including another Elitea
// deployment's MCP endpoint) attaches a short-lived assertion signed with
// this ring. The audience is the remote endpoint; the claims name the run,
// the agent, the delegating user or mandate, and the run's effective scope.
// It is verified against the same JWKS as a federation assertion, but has
// its own token type (typ elitea-delegation+jwt), which no federation
// relying party accepts, and it is never signed for the audience of a
// configured relying party. So neither kind can stand in for the other.

// Bounds of a delegation assertion.
const (
	// DelegationMaxTTL is the longest delegation assertion: exp - iat.
	DelegationMaxTTL = 5 * time.Minute
	// DelegationTokenType is the JWS `typ` header of a delegation
	// assertion. It differs from TokenType (federation), so a federation
	// relying party that checks typ refuses a delegation assertion and an
	// inbound verifier refuses a federation assertion.
	DelegationTokenType = "elitea-delegation+jwt"
	// DelegationPurpose is the elitea_purpose claim of a delegation
	// assertion. A federation assertion has no elitea_purpose.
	DelegationPurpose = "delegation"
	// MaxDelegationAudienceBytes bounds the audience (an MCP endpoint URL).
	MaxDelegationAudienceBytes = 2048
	// MaxScopeToolkits bounds the toolkit list of the scope claim, so the
	// assertion stays small enough for an HTTP header.
	MaxScopeToolkits = 64
	// MaxScopeToolkitBytes bounds one entry of the toolkit list.
	MaxScopeToolkitBytes = 64
	// MaxDelegationTokenBytes bounds the compact JWS.
	MaxDelegationTokenBytes = 8 * 1024
)

// ErrInvalidDelegation refuses a delegation request before anything is
// signed. The text names the field, never a value.
var ErrInvalidDelegation = errors.New("elitea-native agent identity: invalid delegation assertion request")

// ErrDelegationAudienceReserved refuses a delegation assertion for the
// audience of a configured federation relying party.
var ErrDelegationAudienceReserved = errors.New("elitea-native agent identity: the audience is a federation relying party")

// DelegationScope is the elitea_scope claim: the effective scope of the
// calling run. Toolkits are the canonical toolkit types the run was
// admitted with (its sub-agents' included), sorted and unique, so a
// receiving Elitea deployment can intersect the agent it runs with them.
// Digest is "sha256:<hex>" over the run's admitted toolkit set.
type DelegationScope struct {
	Digest   string   `json:"digest"`
	Toolkits []string `json:"toolkits"`
}

// DelegationRequest is what the mint service asks the signer for. Every
// field is decided by Main from the run's own records, never by the worker.
type DelegationRequest struct {
	// Audience is the remote endpoint (NormalizeAudience).
	Audience string
	// Agent names the calling agent; the subject is "agent:" + Agent.
	Agent           string
	TenantProjectID int64
	ExecutionID     string
	Generation      uint64
	// UserID is the delegating user; empty for an autonomous run.
	UserID string
	// MandateID is the run's mandate; empty for a run without one.
	MandateID string
	Scope     DelegationScope
	// TTL is capped at DelegationMaxTTL; zero means DelegationMaxTTL.
	TTL time.Duration
}

// DelegationClaims is the assertion body. No email, display name, token or
// toolkit setting appears.
type DelegationClaims struct {
	Issuer          string          `json:"iss"`
	Subject         string          `json:"sub"`
	Audience        string          `json:"aud"`
	IssuedAt        int64           `json:"iat"`
	NotBefore       int64           `json:"nbf"`
	Expiry          int64           `json:"exp"`
	ID              string          `json:"jti"`
	SubjectProfile  string          `json:"sub_profile"`
	Purpose         string          `json:"elitea_purpose"`
	AgentInstanceID string          `json:"agent_instance_id"`
	Tenant          string          `json:"elitea_tenant"`
	Project         string          `json:"elitea_project"`
	Run             string          `json:"elitea_run"`
	User            *AssertedUser   `json:"elitea_user,omitempty"`
	Mandate         string          `json:"elitea_mandate,omitempty"`
	Scope           DelegationScope `json:"elitea_scope"`
}

// DelegationSigner signs delegation assertions with the agent-assertion
// ring. Safe for concurrent use.
type DelegationSigner struct {
	issuer    string
	jwksURI   string
	tenant    string
	ring      *KeyRing
	algorithm Algorithm
	now       func() time.Time
	newID     func() (string, error)
	// reserved are the audiences of the federation relying parties, raw
	// and normalized: never a delegation audience.
	reserved map[string]bool
}

// NewDelegationSigner builds the signer over a loaded ring. cfg must be
// validated (ParseConfig). The ring's ES256 key signs when it holds one,
// else its RS256 key.
func NewDelegationSigner(cfg Config, ring *KeyRing, opts ...Option) (*DelegationSigner, error) {
	if ring == nil {
		return nil, errors.New("elitea-native agent identity: the delegation signer needs the key ring")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	algorithms := ring.Algorithms()
	if len(algorithms) == 0 {
		return nil, ErrNoSigningKey
	}
	// Reuse the provider's options (the clock) through a throwaway value.
	clock := &Provider{now: func() time.Time { return time.Now().UTC() }}
	for _, opt := range opts {
		opt(clock)
	}
	reserved := make(map[string]bool, 2*len(cfg.RelyingParties))
	for _, party := range cfg.RelyingParties {
		reserved[party.Audience] = true
		if normalized, ok := NormalizeAudience(party.Audience); ok {
			reserved[normalized] = true
		}
	}
	return &DelegationSigner{
		issuer: cfg.Issuer(), jwksURI: cfg.JWKSURI(), tenant: cfg.Tenant, ring: ring,
		algorithm: algorithms[0], now: clock.now, reserved: reserved,
		newID: func() (string, error) {
			id, err := uuid.NewRandom()
			return id.String(), err
		},
	}, nil
}

// Issuer is the iss of every assertion; JWKSURI where its keys are.
func (s *DelegationSigner) Issuer() string  { return s.issuer }
func (s *DelegationSigner) JWKSURI() string { return s.jwksURI }

// WellKnown answers the public documents of the signer's issuer: a receiver
// verifies a delegation assertion against this JWKS.
func (s *DelegationSigner) WellKnown() *WellKnown {
	return &WellKnown{ring: s.ring, issuer: s.issuer, jwksURI: s.jwksURI, algorithms: s.ring.Algorithms(), now: s.now}
}

// Sign checks the request and signs one assertion. Every check runs before
// anything is signed.
func (s *DelegationSigner) Sign(ctx context.Context, in DelegationRequest) (string, DelegationClaims, error) {
	audience, ok := NormalizeAudience(in.Audience)
	if !ok || audience != in.Audience {
		return "", DelegationClaims{}, delegationInvalid("audience")
	}
	if s.reserved[audience] || s.reserved[in.Audience] {
		return "", DelegationClaims{}, ErrDelegationAudienceReserved
	}
	switch {
	case !validRef(in.Agent, 160):
		return "", DelegationClaims{}, delegationInvalid("agent")
	case in.TenantProjectID <= 0:
		return "", DelegationClaims{}, delegationInvalid("tenant project")
	case !validRef(in.ExecutionID, 128) || in.Generation == 0:
		return "", DelegationClaims{}, delegationInvalid("execution")
	case in.UserID != "" && !validRef(in.UserID, 128):
		return "", DelegationClaims{}, delegationInvalid("user")
	case in.MandateID != "" && !validRef(in.MandateID, 256):
		return "", DelegationClaims{}, delegationInvalid("mandate")
	case !ValidScope(in.Scope):
		return "", DelegationClaims{}, delegationInvalid("scope")
	}
	ttl := in.TTL
	if ttl <= 0 || ttl > DelegationMaxTTL {
		ttl = DelegationMaxTTL
	}
	seconds := int64(ttl / time.Second)
	if seconds < int64(MinTTL/time.Second) {
		return "", DelegationClaims{}, delegationInvalid("ttl")
	}
	jti, err := s.newID()
	if err != nil {
		return "", DelegationClaims{}, err
	}
	now := s.now().UTC()
	issued := now.Unix()
	claims := DelegationClaims{
		Issuer: s.issuer, Subject: "agent:" + in.Agent, Audience: audience,
		IssuedAt: issued, NotBefore: issued, Expiry: issued + seconds, ID: jti,
		SubjectProfile: "ai_agent", Purpose: DelegationPurpose,
		AgentInstanceID: in.ExecutionID + ":" + strconv.FormatUint(in.Generation, 10),
		Tenant:          s.tenant, Project: strconv.FormatInt(in.TenantProjectID, 10),
		Run: in.ExecutionID, Mandate: in.MandateID, Scope: in.Scope,
	}
	if in.UserID != "" {
		claims.User = &AssertedUser{Subject: "user:" + in.UserID, Evidence: EvidenceAsserted}
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", DelegationClaims{}, err
	}
	token, err := s.ring.sign(ctx, s.algorithm, now, func(kid string) ([]byte, error) {
		return json.Marshal(struct {
			Algorithm string `json:"alg"`
			KeyID     string `json:"kid"`
			Type      string `json:"typ"`
		}{string(s.algorithm), kid, DelegationTokenType})
	}, payload)
	if err != nil {
		return "", DelegationClaims{}, err
	}
	if len(token) > MaxDelegationTokenBytes {
		return "", DelegationClaims{}, delegationInvalid("size")
	}
	return token, claims, nil
}

// NormalizeAudience is the audience of a remote endpoint URL. It is the
// only normalization of an audience: Main applies it to the admitted
// toolkit URLs, to the endpoint a worker asks for, and to its own MCP
// endpoint when it verifies, and compares only its results. The form is
// "https://", the host in lower-case ASCII (an IDN host in punycode, an IPv6
// host in brackets and canonical form, one trailing dot dropped), the port
// when it is not 443, and the path with dot segments resolved (also when
// percent-encoded) and no trailing slash. A URL that is not https, or that
// has credentials, a query, a fragment or an IPv6 zone, has no audience.
func NormalizeAudience(raw string) (string, bool) {
	if raw == "" || len(raw) > MaxDelegationAudienceBytes || strings.ContainsAny(raw, " \t\r\n\x00\\") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" ||
		u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", false
	}
	host, ok := audienceHost(u.Hostname())
	if !ok {
		return "", false
	}
	if port := u.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", false
		}
		if number != 443 {
			host += ":" + strconv.Itoa(number)
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return "", false
	}
	return "https://" + host + audiencePath(u.EscapedPath()), true
}

func audienceHost(hostname string) (string, bool) {
	if hostname == "" || strings.Contains(hostname, "%") {
		return "", false
	}
	if strings.Contains(hostname, ":") {
		ip := net.ParseIP(hostname)
		if ip == nil || ip.To4() != nil {
			return "", false
		}
		return "[" + ip.String() + "]", true
	}
	if ip := net.ParseIP(hostname); ip != nil {
		return ip.String(), true
	}
	hostname = strings.TrimSuffix(hostname, ".")
	ascii, err := idna.Lookup.ToASCII(hostname)
	if err != nil || ascii == "" {
		return "", false
	}
	return strings.ToLower(ascii), true
}

// audiencePath resolves dot segments (RFC 3986 section 5.2.4), plain or
// percent-encoded, and drops a trailing slash; "/" is the empty path.
func audiencePath(escaped string) string {
	segments := strings.Split(escaped, "/")
	out := make([]string, 0, len(segments))
	for index, segment := range segments {
		if index == 0 {
			continue
		}
		switch strings.ToLower(segment) {
		case ".", "%2e":
			continue
		case "..", "%2e%2e", ".%2e", "%2e.":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
			continue
		}
		out = append(out, segment)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	if len(out) == 0 {
		return ""
	}
	return "/" + strings.Join(out, "/")
}

// AudienceOrigin answers the origin of a normalized audience: everything
// before its path.
func AudienceOrigin(audience string) string {
	rest := strings.TrimPrefix(audience, "https://")
	if index := strings.IndexByte(rest, '/'); index >= 0 {
		rest = rest[:index]
	}
	return "https://" + rest
}

// ValidScope checks the scope claim's bounds and shape.
func ValidScope(scope DelegationScope) bool {
	if !strings.HasPrefix(scope.Digest, "sha256:") || len(scope.Digest) != len("sha256:")+64 {
		return false
	}
	if len(scope.Toolkits) > MaxScopeToolkits {
		return false
	}
	for i, toolkit := range scope.Toolkits {
		if toolkit == "" || len(toolkit) > MaxScopeToolkitBytes || !printable(toolkit) ||
			(i > 0 && scope.Toolkits[i-1] >= toolkit) {
			return false
		}
	}
	return true
}

func validRef(value string, limit int) bool {
	return value != "" && len(value) <= limit && printable(value)
}

func printable(value string) bool {
	for _, c := range value {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

func delegationInvalid(field string) error {
	return errors.Join(ErrInvalidDelegation, errors.New(field))
}
