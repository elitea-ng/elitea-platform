package eliteacore

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

/* ── the approval token ───────────────────────────────────────────────────── */

// The publish route accepts a `validation_token` in place of running the
// pre-publish quality check again. The token used to be checked for its SHAPE
// alone — sixteen or more lowercase hexadecimal characters — and nothing else:
// it was never recorded when it was issued, never bound to a version, and
// never looked up. Any hexadecimal string a caller typed satisfied the check,
// so the gate that decides what may reach the catalogue could be skipped on any
// version by anyone who could reach the route (issue 855).
//
// The token is now a GRANT rather than a shape. It carries its own expiry and a
// fingerprint of the version the check judged, and it is signed with a secret
// only this process holds:
//
//	token = <expiry:16 hex><content fingerprint:64 hex><signature:64 hex>
//
// The whole token stays lowercase hexadecimal, which is what every client that
// stores it between the check and the publish already assumes.
//
// The signature covers the version id as well as the two carried fields, so a
// token issued for one version is refused on another, and a token nobody issued
// has no signature that can be reproduced. The fingerprint is re-read from the
// version at publish time: an agent edited after it passed the check is
// refused, because the check that passed no longer describes what would be
// published. This is the rule the skill publish path already applies
// (internal/api/v2/skillpublish/handler.go).
//
// The secret is generated per process rather than read from configuration. The
// token is only meaningful between a check and the publish that follows it,
// which is one user action against one process, and a restart is answered with
// the same "check it again" refusal a tampered token gets. A package-level
// secret rather than a per-handler one keeps a token valid across every handler
// this process builds, so a deployment that composes the routes more than once
// does not refuse its own tokens.
var publishValidationSecret = newPublishValidationSecret()

func newPublishValidationSecret() []byte {
	secret := make([]byte, 32)
	_, _ = rand.Read(secret) // crypto/rand.Read never fails on supported platforms
	return secret
}

// publishValidationTokenTTL is how long a passing check stays spendable. It is
// long enough for an author to read the findings the dialog shows them and
// short enough that a token copied out of a browser session is not a standing
// permission to publish.
const publishValidationTokenTTL = 30 * time.Minute

const (
	publishTokenExpiryChars    = 16 // uint64 seconds since the epoch
	publishTokenHashChars      = 64 // sha256, hex
	publishTokenSignatureChars = 64 // hmac-sha256, hex
	publishValidationTokenLen  = publishTokenExpiryChars + publishTokenHashChars + publishTokenSignatureChars
)

// publishTokenVerdict is why a token was refused, so the publish route can say
// which of the four things went wrong instead of answering every one of them
// with the same sentence.
type publishTokenVerdict int

const (
	publishTokenValid publishTokenVerdict = iota
	// publishTokenMalformed: not a token this platform ever mints.
	publishTokenMalformed
	// publishTokenExpired: minted here, but too long ago.
	publishTokenExpired
	// publishTokenForged: the signature does not verify — nobody issued this
	// token, or it was issued for a different version.
	publishTokenForged
	// publishTokenStale: issued for this version, but the version changed
	// after the check passed.
	publishTokenStale
)

// publishTokenRefusal is the message each verdict is refused with. The publish
// dialog shows it to the author, so each one names the thing they can do about
// it.
func publishTokenRefusal(verdict publishTokenVerdict) string {
	switch verdict {
	case publishTokenMalformed:
		return "The validation token is not a token this platform issued. Run the pre-publish check for this version and publish with the token it returns."
	case publishTokenExpired:
		return "The validation token has expired. Run the pre-publish check for this version again."
	case publishTokenForged:
		return "The validation token was not issued for this version. Run the pre-publish check for this version and publish with the token it returns."
	case publishTokenStale:
		return "The agent changed after the validation token was issued. Run the pre-publish check again."
	default:
		return ""
	}
}

// publishTokenContentHash fingerprints everything the deterministic half of the
// check reads off the version row. A change to any of it invalidates a token
// issued before the change.
//
// It returns the empty string when the version cannot be read, which no token
// can match: a token for a version that has since disappeared is refused rather
// than accepted by default.
func (h *Handler) publishTokenContentHash(ctx context.Context, schema, versionID string) string {
	var name, instructions, welcome, agentType, starters, llmSettings string
	if err := h.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT COALESCE(name, ''), COALESCE(instructions, ''), COALESCE(welcome_message, ''),
		       COALESCE(agent_type, ''), COALESCE(conversation_starters::text, '[]'),
		       COALESCE(llm_settings::text, '{}')
		FROM %s.application_versions WHERE id = $1`, schema), versionID).
		Scan(&name, &instructions, &welcome, &agentType, &starters, &llmSettings); err != nil {
		return ""
	}
	// A JSON array rather than a map: the order is the order written here, so
	// no field can move into another field's place without changing the hash.
	payload, _ := json.Marshal([]string{name, instructions, welcome, agentType, starters, llmSettings}) // []string always marshals
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func publishValidationSignature(versionID, expiryHex, contentHash string) string {
	mac := hmac.New(sha256.New, publishValidationSecret)
	mac.Write([]byte(versionID + ":" + expiryHex + ":" + contentHash))
	return hex.EncodeToString(mac.Sum(nil))
}

// issuePublishValidationToken mints the grant a passing check hands out.
func issuePublishValidationToken(versionID, contentHash string, expiresAt time.Time) string {
	expiryHex := fmt.Sprintf("%016x", uint64(expiresAt.Unix())) //nolint:gosec // a unix second is never negative here
	return expiryHex + contentHash + publishValidationSignature(versionID, expiryHex, contentHash)
}

// verifyPublishValidationToken says whether a token may stand in for the check
// on this version, and why not when it may not.
func verifyPublishValidationToken(token, versionID, contentHash string, now time.Time) publishTokenVerdict {
	if len(token) != publishValidationTokenLen || !isLowerHex(token) {
		return publishTokenMalformed
	}
	expiryHex := token[:publishTokenExpiryChars]
	carriedHash := token[publishTokenExpiryChars : publishTokenExpiryChars+publishTokenHashChars]
	signature := token[publishTokenExpiryChars+publishTokenHashChars:]

	expiry, err := strconv.ParseUint(expiryHex, 16, 64)
	if err != nil {
		return publishTokenMalformed
	}

	// The signature is checked BEFORE the expiry is believed and before the
	// carried fingerprint is compared: both fields are the caller's bytes until
	// this passes, and refusing an unsigned token as "expired" or as "the agent
	// changed" would tell a forger which part of their guess to change.
	if !hmac.Equal([]byte(signature), []byte(publishValidationSignature(versionID, expiryHex, carriedHash))) {
		return publishTokenForged
	}
	if now.Unix() > int64(expiry) { //nolint:gosec // the expiry this process signed fits an int64
		return publishTokenExpired
	}
	if contentHash == "" || !hmac.Equal([]byte(carriedHash), []byte(contentHash)) {
		return publishTokenStale
	}
	return publishTokenValid
}

func isLowerHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
