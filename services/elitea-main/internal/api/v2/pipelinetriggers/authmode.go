package pipelinetriggers

// How an inbound call is authenticated — issue 970.
//
// 0133 gave the trigger one credential shape: a bearer secret in one of three
// carriers. The senders people actually have cannot produce it. A GitHub
// repository webhook sends no `Authorization` header and cannot be configured
// to; it signs the RAW BODY with a shared secret and sends
// `X-Hub-Signature-256: sha256=<hex>`. So the trigger grew a per-row MODE, and
// this file is the whole of it: the vocabulary, the create-time parse, and the
// one verification function the inbound path calls.
//
// THE MODE IS STORED, NEVER PRESENTED. The row decides how its own calls are
// authenticated, exactly as the row already decides which pipeline runs
// (package rule 1). A request cannot ask to be checked a different way: the
// only thing the caller's URL may carry is the provider SUFFIX, which is
// decoration for the sender's configuration screen and is validated against the
// stored provider rather than trusted.
//
// A SIGNATURE TRIGGER DOES NOT ALSO ACCEPT THE BEARER. The two carry the same
// secret, so accepting both would mean the stricter setting bought nothing: a
// URL leaked to a proxy log, together with the secret a sender pasted into
// their provider's form, would still start runs. A trigger is in one mode.

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// The stored `auth_mode` vocabulary. Both values are in 0138's CHECK
// constraint, so a value invented here is refused by the database rather than
// falling through to the bearer path.
const (
	// AuthModeToken is the bearer secret every trigger had before #970 and
	// the default for every new one.
	AuthModeToken = "token"
	// AuthModeHMACSHA256 verifies HMAC-SHA256 of the RAW request body under
	// the trigger secret, read from the provider's own header.
	AuthModeHMACSHA256 = "hmac_sha256"
)

// The stored `provider` vocabulary — the URL suffix, and the preset a create
// may name instead of spelling the mode out.
const (
	ProviderCustom = "custom"
	ProviderGitHub = "github"
)

// GitHubSignatureHeader is what GitHub sends, and the preset's header.
const GitHubSignatureHeader = "X-Hub-Signature-256" //nolint:gosec // header NAME, not a credential

// gitHubSignaturePrefix is the algorithm label GitHub puts in front of the
// hex. A bare hex value is accepted too, because a generic HMAC sender
// configured with a custom header usually sends one.
const gitHubSignaturePrefix = "sha256="

// maxSignatureHeaderName bounds a caller-chosen header name. It is stored in a
// varchar(128) column and rendered in the settings dialog.
const maxSignatureHeaderName = 128

// errInvalidAuthMode is the create-time refusal. Unlike the inbound path, the
// settings route SAYS what is wrong: its caller is an authenticated person
// configuring their own pipeline, not an anonymous sender, so a precise message
// is a help rather than an oracle.
var errInvalidAuthMode = errors.New("pipelinetriggers: invalid trigger authentication mode")

// triggerAuthMode is the three stored facts, together.
type triggerAuthMode struct {
	AuthMode        string
	SignatureHeader string
	Provider        string
}

// defaultAuthMode is what a create with no body asks for — the bearer trigger,
// unchanged, which is what every existing caller of this route gets.
func defaultAuthMode() triggerAuthMode {
	return triggerAuthMode{AuthMode: AuthModeToken, Provider: ProviderCustom}
}

// signs reports whether this mode verifies a body signature.
func (m triggerAuthMode) signs() bool { return m.AuthMode == AuthModeHMACSHA256 }

// createTriggerBody is what the create/rotate route accepts. Every field is
// optional; a body that carries none of them (or no body at all) asks for the
// bearer trigger.
//
// `type` is the PRESET name — the word the trigger-type selector and the source
// cases use ("github"), which expands to a mode, a header and a provider.
// `auth_mode`/`signature_header` are the explicit form, for a sender that signs
// the same way under a header of its own. The preset is applied first and the
// explicit fields override it, so `{"type":"github"}` and
// `{"auth_mode":"hmac_sha256","signature_header":"X-Hub-Signature-256"}` are
// the same trigger except for the URL suffix.
type createTriggerBody struct {
	Type            string `json:"type"`
	Provider        string `json:"provider"`
	AuthMode        string `json:"auth_mode"`
	SignatureHeader string `json:"signature_header"`
}

// parseAuthMode reads the create body into the three stored facts.
//
// An unreadable body is NOT an error: the route accepted any body at all
// before #970, and a caller that sends something unrelated must keep getting
// the bearer trigger rather than a new 400. A body that is readable and names
// something this service does not implement IS an error — silently storing a
// weaker mode than the one asked for is the failure this refusal exists to
// prevent.
func parseAuthMode(raw []byte) (triggerAuthMode, error) {
	mode := defaultAuthMode()
	if len(raw) == 0 {
		return mode, nil
	}
	var body createTriggerBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return mode, nil
	}

	preset := strings.ToLower(strings.TrimSpace(firstNonEmpty(body.Type, body.Provider)))
	switch preset {
	case "", ProviderCustom:
		// The explicit form decides below.
	case ProviderGitHub:
		mode = triggerAuthMode{
			AuthMode:        AuthModeHMACSHA256,
			SignatureHeader: GitHubSignatureHeader,
			Provider:        ProviderGitHub,
		}
	default:
		return triggerAuthMode{}, errInvalidAuthMode
	}

	if requested := strings.ToLower(strings.TrimSpace(body.AuthMode)); requested != "" {
		switch requested {
		case AuthModeToken:
			// An explicit `token` beside a signing preset is a contradiction,
			// and the one direction that must not be resolved silently: it
			// would turn the stricter request into the weaker setting.
			if mode.signs() {
				return triggerAuthMode{}, errInvalidAuthMode
			}
			mode.AuthMode = AuthModeToken
		case AuthModeHMACSHA256:
			mode.AuthMode = AuthModeHMACSHA256
		default:
			return triggerAuthMode{}, errInvalidAuthMode
		}
	}

	if header := strings.TrimSpace(body.SignatureHeader); header != "" {
		if !validHeaderName(header) {
			return triggerAuthMode{}, errInvalidAuthMode
		}
		mode.SignatureHeader = header
	}

	if mode.signs() && mode.SignatureHeader == "" {
		// A signature mode with no header to read is unusable: the inbound
		// path would have nothing to look at and would refuse every call. The
		// database refuses this too (0138's CHECK); saying so here names the
		// field instead.
		return triggerAuthMode{}, errInvalidAuthMode
	}
	if !mode.signs() {
		// A header on a bearer trigger would be stored and never read, and
		// would then be rendered by the settings dialog as a configuration the
		// sender should make. Dropped rather than kept.
		mode.SignatureHeader = ""
	}
	return mode, nil
}

// readSettingsBody reads a bounded settings-route body.
//
// It shares `maxInboundBody` with the inbound path and with SaveSchedule
// rather than declaring a third limit: the three bodies are the same kind of
// thing (a small JSON object a person or a sender sent), and a bound that
// differs per route is a bound nobody can state.
//
// An absent body is not an error — the create route took none before #970 and
// still takes none.
func readSettingsBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	return io.ReadAll(io.LimitReader(r.Body, maxInboundBody))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// validHeaderName admits the RFC 7230 token characters a header name may use.
// It is deliberately strict: the name is echoed to the settings dialog and read
// back out of `http.Header`, and a name carrying a space or a control character
// could never match an incoming header anyway.
func validHeaderName(name string) bool {
	if name == "" || len(name) > maxSignatureHeaderName {
		return false
	}
	for _, character := range name {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '-', character == '_':
		default:
			return false
		}
	}
	return true
}

// signatureMatches reports whether `presented` is a valid HMAC-SHA256 of
// `body` under `secret`.
//
// CONSTANT TIME on the digest, the same rule the bearer path follows: the
// comparison is `hmac.Equal` on two fixed 32-byte values, never `==` on the
// hex. What is NOT constant time is the shape check in front of it — an absent
// header, a non-hex value or a wrong-length digest each return early. That
// branches on the CALLER'S OWN INPUT and not on the secret, so it leaks
// nothing an attacker does not already hold; the same reasoning inbound.go
// records for the unknown-token lookup.
func signatureMatches(presented string, body []byte, secret string) bool {
	value := strings.TrimSpace(presented)
	if value == "" || secret == "" {
		return false
	}
	// GitHub's `sha256=` label, case-insensitively, or a bare hex digest.
	if lowered := strings.ToLower(value); strings.HasPrefix(lowered, gitHubSignaturePrefix) {
		value = value[len(gitHubSignaturePrefix):]
	}
	presentedDigest, err := hex.DecodeString(strings.TrimSpace(value))
	if err != nil || len(presentedDigest) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	// hash.Hash's Write never returns an error, which is why the result is
	// dropped here rather than turned into a refusal that would report a
	// signature failure for a runtime fault.
	_, _ = mac.Write(body)
	expected := mac.Sum(nil)
	return subtle.ConstantTimeCompare(expected, presentedDigest) == 1
}
