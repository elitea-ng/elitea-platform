package browserauth

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/netip"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MaxBrowserAttemptClientKeyBytes = 512
	MaxBrowserAttemptRetryAfter     = 24 * time.Hour
)

var (
	ErrInvalidBrowserAttempt = errors.New("invalid browser authentication attempt")
	ErrAttemptLimited        = errors.New("browser authentication attempt limit reached")
)

type BrowserAttemptStage string

const (
	BrowserAttemptFormBegin      BrowserAttemptStage = "form_begin"
	BrowserAttemptFormCredential BrowserAttemptStage = "form_credential"
	// The single-sign-on planes (internal/api/v2/auth, #1078). Each counts
	// per client only, like a Form begin: none verifies a credential the
	// caller chose, so none counts against the global credential row.
	BrowserAttemptOIDCBegin    BrowserAttemptStage = "oidc_begin"
	BrowserAttemptOIDCCallback BrowserAttemptStage = "oidc_callback"
	BrowserAttemptSAMLBegin    BrowserAttemptStage = "saml_begin"
	BrowserAttemptSAMLACS      BrowserAttemptStage = "saml_acs"
)

// ipv6ClientPrefixBits is the IPv6 grouping of AttemptClientKey: the
// smallest prefix one end site is normally assigned (RFC 6177).
const ipv6ClientPrefixBits = 64

// AttemptClientKey is the client key of a resolved client address: the
// address for IPv4 and its /64 for IPv6. One IPv6 subscriber is routinely
// given a whole /64 (SLAAC, privacy addresses) and can rotate through it at
// will, so a per-/128 key would give one client 2^64 independent limits. An
// IPv4-mapped IPv6 address keys as the IPv4 address it is.
func AttemptClientKey(address netip.Addr) string {
	address = address.Unmap()
	if address.Is4() {
		return address.String()
	}
	prefix, err := address.WithZone("").Prefix(ipv6ClientPrefixBits)
	if err != nil {
		// Unreachable for a valid IPv6 address; the full address still keys.
		return address.String()
	}
	return prefix.String()
}

// BrowserAttempt contains only the bounded material needed for shared
// cross-replica admission. ClientKey is canonical trusted-proxy output, not a
// caller-selected forwarded header. LoginDigest is required only for Form
// credentials and must be a one-way digest of the submitted login.
type BrowserAttempt struct {
	ClientKey   string
	Stage       BrowserAttemptStage
	LoginDigest [sha256.Size]byte
}

func (attempt BrowserAttempt) Validate() error {
	if attempt.ClientKey == "" || len(attempt.ClientKey) > MaxBrowserAttemptClientKeyBytes ||
		!utf8.ValidString(attempt.ClientKey) || attempt.ClientKey != strings.TrimSpace(attempt.ClientKey) ||
		stringsContainControl(attempt.ClientKey) || strings.ContainsFunc(attempt.ClientKey, unicode.IsSpace) {
		return ErrInvalidBrowserAttempt
	}
	zeroDigest := attempt.LoginDigest == [sha256.Size]byte{}
	switch attempt.Stage {
	case BrowserAttemptFormCredential:
		if zeroDigest {
			return ErrInvalidBrowserAttempt
		}
	case BrowserAttemptFormBegin, BrowserAttemptOIDCBegin, BrowserAttemptOIDCCallback,
		BrowserAttemptSAMLBegin, BrowserAttemptSAMLACS:
		if !zeroDigest {
			return ErrInvalidBrowserAttempt
		}
	default:
		return ErrInvalidBrowserAttempt
	}
	return nil
}

func stringsContainControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

// AttemptAdmitter is called before login state allocation and before expensive
// provider assertion work. Implementations must apply atomic stage-specific
// limits shared by all service replicas.
type AttemptAdmitter interface {
	Admit(context.Context, BrowserAttempt) (retryAfter time.Duration, err error)
}
