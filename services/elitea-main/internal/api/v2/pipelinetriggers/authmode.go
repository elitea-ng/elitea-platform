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
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The stored `auth_mode` vocabulary. Every value is in the CHECK constraint
// (0138, widened by 0139), so a value invented here is refused by the database
// rather than falling through to the bearer path.
const (
	// AuthModeToken is the bearer secret every trigger had before #970 and
	// the default for every new one.
	AuthModeToken = "token"
	// AuthModeHMACSHA256 verifies HMAC-SHA256 of the RAW request body under
	// the trigger secret, read from the provider's own header.
	AuthModeHMACSHA256 = "hmac_sha256"
	// AuthModeStandardWebhooks verifies a Standard Webhooks signature
	// (standardwebhooks.com): HMAC-SHA256 of `webhook-id.webhook-timestamp.body`
	// under the trigger secret, sent as `webhook-signature: v1,<base64>`, with
	// a timestamp inside StandardWebhooksTolerance. It is what a GitLab
	// webhook with a SIGNING TOKEN sends (legacy issue 6664).
	AuthModeStandardWebhooks = "standard_webhooks_hmac"
)

// The stored `provider` vocabulary — the URL suffix, and the preset a create
// may name instead of spelling the mode out.
const (
	ProviderCustom = "custom"
	ProviderGitHub = "github"
	// ProviderGitLab is a GitLab project or group webhook. Its preset is the
	// bearer mode, because GitLab's SECRET TOKEN is the trigger secret sent
	// verbatim in `X-Gitlab-Token` — a carrier, not a signature. A GitLab
	// webhook configured with a SIGNING TOKEN instead asks for
	// `auth_mode: standard_webhooks_hmac` beside this preset.
	ProviderGitLab = "gitlab"
)

// GitLabTokenHeader is the header a GitLab webhook sends its secret token in.
// It is a fourth bearer carrier (inbound.go presentedSecret).
const GitLabTokenHeader = "X-Gitlab-Token" //nolint:gosec // header NAME, not a credential

// The three Standard Webhooks headers. The signature header is what a
// standard_webhooks_hmac row stores as its signature_header.
const (
	StandardWebhooksIDHeader        = "webhook-id"
	StandardWebhooksTimestampHeader = "webhook-timestamp"
	StandardWebhooksSignatureHeader = "webhook-signature"
)

// StandardWebhooksTolerance is how far a signed timestamp may be from this
// server's clock, in either direction. The specification's reference
// libraries use five minutes. A delivery outside it is refused, which stops a
// captured request from being replayed AFTER the window. It does nothing
// inside the window: a copy sent within five minutes carries a timestamp that
// is still good. What stops that copy is the delivery log (deliveries.go),
// keyed on the signed `webhook-id`, which answers a repeat with the run the
// first copy started instead of starting another.
const StandardWebhooksTolerance = 5 * time.Minute

// standardWebhooksSecretPrefix marks a Standard Webhooks secret: the rest is
// the base64 of the key bytes. Secrets minted for this mode carry it.
const standardWebhooksSecretPrefix = "whsec_"

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
func (m triggerAuthMode) signs() bool { return modeSigns(m.AuthMode) }

// modeSigns reports whether a stored `auth_mode` verifies a body signature
// rather than a presented bearer secret. Every place that branches on "is
// this a signing trigger" asks this one function, so a third signing mode
// cannot be added to one branch and forgotten in another.
func modeSigns(authMode string) bool {
	return authMode == AuthModeHMACSHA256 || authMode == AuthModeStandardWebhooks
}

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

// parseAuthMode reads the create body into the three stored facts, and says
// whether the BODY NAMED THEM.
//
// An unreadable body is NOT an error: the route accepted any body at all
// before #970, and a caller that sends something unrelated must keep getting
// the bearer trigger rather than a new 400. A body that is readable and names
// something this service does not implement IS an error — silently storing a
// weaker mode than the one asked for is the failure this refusal exists to
// prevent.
//
// THE SECOND RETURN IS WHY THIS FUNCTION IS NOT ENOUGH ON ITS OWN. The mode it
// answers for a body that names nothing is the DEFAULT — the bearer trigger a
// first creation gets — and the create/rotate route is one operation for both
// acts. A rotation therefore arrived here with no body (the EditPipeline card
// sends none: `usePipelineTriggerSettings.ts`), took the default, and the
// upsert wrote it over the stored columns: a GitHub trigger came back as
// token/custom and the next signed delivery was refused. `named` is false in
// exactly that case, and the caller keeps the stored mode instead. A body that
// DOES name a mode still replaces it — "rotate this as a plain token trigger"
// is a change the settings dialog offers through this same route.
func parseAuthMode(raw []byte) (triggerAuthMode, bool, error) {
	mode := defaultAuthMode()
	if len(raw) == 0 {
		return mode, false, nil
	}
	var body createTriggerBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return mode, false, nil
	}
	// Whitespace is not a name. `firstNonEmpty` and the trims below read the
	// same fields the same way, so a body of `{"type":"  "}` asks for nothing
	// here and nothing there.
	named := strings.TrimSpace(body.Type) != "" || strings.TrimSpace(body.Provider) != "" ||
		strings.TrimSpace(body.AuthMode) != "" || strings.TrimSpace(body.SignatureHeader) != ""

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
	case ProviderGitLab:
		mode = triggerAuthMode{AuthMode: AuthModeToken, Provider: ProviderGitLab}
	default:
		return triggerAuthMode{}, named, errInvalidAuthMode
	}

	if requested := strings.ToLower(strings.TrimSpace(body.AuthMode)); requested != "" {
		switch requested {
		case AuthModeToken:
			// An explicit `token` beside a signing preset is a contradiction,
			// and the one direction that must not be resolved silently: it
			// would turn the stricter request into the weaker setting.
			if mode.signs() {
				return triggerAuthMode{}, named, errInvalidAuthMode
			}
			mode.AuthMode = AuthModeToken
		case AuthModeHMACSHA256:
			// GitLab signs no raw body digest. A GitLab trigger in this
			// mode would refuse every delivery GitLab can send.
			if mode.Provider == ProviderGitLab {
				return triggerAuthMode{}, named, errInvalidAuthMode
			}
			mode.AuthMode = AuthModeHMACSHA256
		case AuthModeStandardWebhooks:
			// GitHub does not send Standard Webhooks headers; the same
			// reasoning in the other direction.
			if mode.Provider == ProviderGitHub {
				return triggerAuthMode{}, named, errInvalidAuthMode
			}
			mode.AuthMode = AuthModeStandardWebhooks
			mode.SignatureHeader = StandardWebhooksSignatureHeader
		default:
			return triggerAuthMode{}, named, errInvalidAuthMode
		}
	}

	if header := strings.TrimSpace(body.SignatureHeader); header != "" {
		if !validHeaderName(header) {
			return triggerAuthMode{}, named, errInvalidAuthMode
		}
		// The Standard Webhooks headers are fixed by the specification. A
		// different name could never match a conforming sender.
		if mode.AuthMode == AuthModeStandardWebhooks &&
			!strings.EqualFold(header, StandardWebhooksSignatureHeader) {
			return triggerAuthMode{}, named, errInvalidAuthMode
		}
		if mode.AuthMode != AuthModeStandardWebhooks {
			mode.SignatureHeader = header
		}
	}

	if mode.signs() && mode.SignatureHeader == "" {
		// A signature mode with no header to read is unusable: the inbound
		// path would have nothing to look at and would refuse every call. The
		// database refuses this too (0138's CHECK); saying so here names the
		// field instead.
		return triggerAuthMode{}, named, errInvalidAuthMode
	}
	if !mode.signs() {
		// A header on a bearer trigger would be stored and never read, and
		// would then be rendered by the settings dialog as a configuration the
		// sender should make. Dropped rather than kept.
		mode.SignatureHeader = ""
	}
	return mode, named, nil
}

// storedAuthMode is the mode a row already carries, for a rotation whose body
// names none.
//
// A row from before 0138's backfill, or one whose columns a migration left
// blank, falls back to the default rather than writing an empty `auth_mode`
// the CHECK constraint would refuse: the failure to preserve is worth less
// than the failure to rotate.
func storedAuthMode(previous triggerRow) triggerAuthMode {
	if strings.TrimSpace(previous.AuthMode) == "" {
		return defaultAuthMode()
	}
	return triggerAuthMode{
		AuthMode:        previous.AuthMode,
		SignatureHeader: previous.SignatureHeader,
		Provider:        previous.Provider,
	}
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

// standardWebhooksSignatureMatches reports whether the request carries a valid
// Standard Webhooks signature of `body` under `secret`, at `now`.
//
// The signed content is `webhook-id + "." + webhook-timestamp + "." + body`,
// and `webhook-signature` is a space-separated list of `v1,<base64>` entries.
// One matching entry is enough: a sender rotating its key sends both
// signatures for a while. Entries with another version label are skipped, not
// refused, because the specification reserves them.
//
// The key is the secret's bytes after a `whsec_` prefix, base64-decoded. A
// secret without the prefix is used as its raw bytes, so a sender that takes
// an arbitrary string (and signs with it as given) also verifies.
//
// CONSTANT TIME on the digest, the same rule signatureMatches follows. The
// shape checks in front of it branch on the caller's own input only.
func standardWebhooksSignatureMatches(headers http.Header, body []byte, secret string, now time.Time) bool {
	id := strings.TrimSpace(headers.Get(StandardWebhooksIDHeader))
	timestamp := strings.TrimSpace(headers.Get(StandardWebhooksTimestampHeader))
	signatures := strings.TrimSpace(headers.Get(StandardWebhooksSignatureHeader))
	if id == "" || timestamp == "" || signatures == "" || secret == "" {
		return false
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	signedAt := time.Unix(seconds, 0)
	if signedAt.Before(now.Add(-StandardWebhooksTolerance)) || signedAt.After(now.Add(StandardWebhooksTolerance)) {
		return false
	}
	key, ok := standardWebhooksKey(secret)
	if !ok {
		return false
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(id + "." + timestamp + "."))
	_, _ = mac.Write(body)
	expected := mac.Sum(nil)

	matched := false
	for _, entry := range strings.Fields(signatures) {
		version, encoded, found := strings.Cut(entry, ",")
		if !found || version != "v1" {
			continue
		}
		presented, decodeErr := base64.StdEncoding.DecodeString(encoded)
		if decodeErr != nil || len(presented) != sha256.Size {
			continue
		}
		if subtle.ConstantTimeCompare(expected, presented) == 1 {
			matched = true
		}
	}
	return matched
}

// standardWebhooksKey derives the HMAC key from a stored trigger secret.
func standardWebhooksKey(secret string) ([]byte, bool) {
	encoded, prefixed := strings.CutPrefix(secret, standardWebhooksSecretPrefix)
	if !prefixed {
		return []byte(secret), true
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(key) == 0 {
		return nil, false
	}
	return key, true
}
