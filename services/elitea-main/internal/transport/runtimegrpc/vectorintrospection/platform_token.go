package vectorintrospection

// The platform token: how elitea-main itself calls elitea-vector as a TOKEN
// caller (issue 1657).
//
// elitea-main reaches elitea-vector as an administrator, which admits only
// Delete, DropProject, Count and ListIndexes. Embedding an agent's memory
// entry (Upsert) and ranking a turn's recall (Search) need a project-bound
// token that names its source. A worker claim token is minted per claim and a
// callback token per provider invocation; neither exists for elitea-main's own
// work, so this is a third, STATELESS bearer:
//
//	elvp_<base64url(project|expiry|source)>.<base64url(HMAC-SHA256)>
//
// The MAC key is derived from APPLICATION_SECRET_KEY, which every elitea-main
// replica shares, so any replica verifies what another minted and no row is
// written. A token names ONE project and ONE source, expires within
// MaxPlatformTokenLifetime, and is verified only for the sources in
// platformTokenSources. elitea-vector sees it as an engine callback token
// (TOKEN_KIND_ENGINE_CALLBACK) with no user identity, which is what it is:
// it reaches no memory scope and no toolkit.
//
// WITHOUT A SECRET there is no minter and no verifier: every platform token
// introspects inactive, and the callers fall back (recall to the keyword
// ranking, the journal keeps its rows).

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	vectorv1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/vector/v1"
)

const (
	// PlatformTokenPrefix starts every platform token.
	PlatformTokenPrefix = "elvp_"
	// MaxPlatformTokenLifetime bounds a token's expiry.
	MaxPlatformTokenLifetime = time.Hour
	// minPlatformSecretBytes is the shortest secret a key is derived from.
	minPlatformSecretBytes = 16
	// platformPrincipal is the principal elitea-vector sees: a service, never a
	// `user:<id>`, so no personal memory is reachable with it.
	platformPrincipal = "service:elitea-main"
)

// platformTokenSources are the sources a platform token may name.
var platformTokenSources = map[string]vectorv1.Source{
	"agent_memory": vectorv1.Source_SOURCE_AGENT_MEMORY,
}

// ErrNoPlatformSecret reports a platform token minter built without a usable
// secret.
var ErrNoPlatformSecret = errors.New("vector platform tokens need APPLICATION_SECRET_KEY of at least 16 bytes")

// PlatformTokens mints and verifies platform tokens.
type PlatformTokens struct {
	key []byte
	now func() time.Time
}

// NewPlatformTokens derives the MAC key from secret.
func NewPlatformTokens(secret string) (*PlatformTokens, error) {
	if len(secret) < minPlatformSecretBytes {
		return nil, ErrNoPlatformSecret
	}
	derive := hmac.New(sha256.New, []byte(secret))
	_, _ = derive.Write([]byte("elitea-vector-platform-token-v1"))
	return &PlatformTokens{key: derive.Sum(nil), now: time.Now}, nil
}

// Mint returns a token for project and the source keyword (for example
// "agent_memory"), valid for lifetime (at most MaxPlatformTokenLifetime), and
// its expiry.
func (p *PlatformTokens) Mint(projectID int64, source string, lifetime time.Duration) (string, time.Time, error) {
	if p == nil {
		return "", time.Time{}, ErrNoPlatformSecret
	}
	if _, known := platformTokenSources[source]; !known || projectID <= 0 {
		return "", time.Time{}, errors.New("vector platform token: a project and a platform source are required")
	}
	if lifetime <= 0 || lifetime > MaxPlatformTokenLifetime {
		lifetime = MaxPlatformTokenLifetime
	}
	expires := p.now().Add(lifetime).UTC().Truncate(time.Second)
	payload := strconv.FormatInt(projectID, 10) + "|" + strconv.FormatInt(expires.Unix(), 10) + "|" + source
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return PlatformTokenPrefix + encoded + "." + p.sign(encoded), expires, nil
}

func (p *PlatformTokens) sign(encoded string) string {
	mac := hmac.New(sha256.New, p.key)
	_, _ = mac.Write([]byte(encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verify answers the introspection of token: inactive for anything that is
// not a live, correctly signed platform token.
func (p *PlatformTokens) verify(token string) *vectorv1.IntrospectTokenResponse {
	inactive := &vectorv1.IntrospectTokenResponse{}
	if p == nil || !strings.HasPrefix(token, PlatformTokenPrefix) {
		return inactive
	}
	body := token[len(PlatformTokenPrefix):]
	encoded, signature, found := strings.Cut(body, ".")
	if !found || encoded == "" || !hmac.Equal([]byte(signature), []byte(p.sign(encoded))) {
		return inactive
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return inactive
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 {
		return inactive
	}
	projectID, projectErr := strconv.ParseInt(parts[0], 10, 64)
	expires, expiresErr := strconv.ParseInt(parts[1], 10, 64)
	source, known := platformTokenSources[parts[2]]
	now := p.now()
	if projectErr != nil || expiresErr != nil || !known || projectID <= 0 ||
		expires <= now.Unix() || expires > now.Add(MaxPlatformTokenLifetime+time.Minute).Unix() {
		return inactive
	}
	return &vectorv1.IntrospectTokenResponse{
		Active:         true,
		ProjectId:      projectID,
		Principal:      platformPrincipal,
		Kind:           vectorv1.TokenKind_TOKEN_KIND_ENGINE_CALLBACK,
		ExpiresAtUnix:  expires,
		AllowedSources: []vectorv1.Source{source},
	}
}
