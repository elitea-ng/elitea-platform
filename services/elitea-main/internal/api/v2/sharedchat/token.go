// Package sharedchat implements "share a conversation by link": the
// owner-facing management of share links, and the ANONYMOUS read those links
// grant.
//
// The anonymous read is the reason this package exists as its own boundary
// rather than as five more methods on the conversations handler. Every route in
// that handler runs behind authentication, project membership and a legacy
// permission; these two do not run behind any of them, and mixing the two
// classes in one file is how a route ends up on the wrong side of a middleware
// by accident.
package sharedchat

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"

	"crypto/pbkdf2"
)

// tokenBytes is the entropy of a share token, in bytes.
//
// 32 bytes = 256 bits. The token is the ONLY thing standing between an
// anonymous caller and a conversation transcript, so the question it has to
// answer is "can this be found by guessing", and the answer has to remain no
// without relying on request throttling (Unlock is bounded by admission.go,
// but View is not). A sequential id, a conversation uuid, or
// anything derived from the conversation would all fail that test: the first is
// enumerable outright, and the last two are values that already travel through
// logs, referrer headers and support tickets attached to the very conversation
// the link exposes.
const tokenBytes = 32

// tokenAlphabetSize bounds what View will even hash. A token is base64url of 32
// random bytes, which is 43 characters; anything materially longer is not a
// token this server issued, and refusing it early keeps an arbitrarily long
// path segment out of the hash and the query.
const maxTokenLength = 128

// pbkdf2Iterations is the work factor for a link password.
//
// The threat is offline: an attacker with the table wants the password so they
// can use a token they also stole, and an attacker with only the token wants to
// guess the password online. 600_000 iterations of HMAC-SHA256 is OWASP's 2023
// PBKDF2-SHA256 floor and costs on the order of a few hundred milliseconds per
// attempt here, which is the point in BOTH directions: it slows online
// guessing and it makes a leaked table expensive rather than instant. Because
// the cost lands on the server for every unlock request, Handler.Unlock caps
// how many run at once and how many one client may start (admission.go).
const pbkdf2Iterations = 600_000

const pbkdf2KeyLength = 32
const saltBytes = 16

// ErrInvalidToken is returned for a token that is not the shape this server
// issues. It is never surfaced with a distinct status: see Handler.View.
var ErrInvalidToken = errors.New("sharedchat: invalid token")

// newToken mints one share token and returns it together with the SHA-256 that
// is all the database ever sees.
func newToken() (token string, hash []byte, err error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("sharedchat: mint token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, hashToken(token), nil
}

// hashToken is the one-way projection stored in shared_chat_links.token_hash.
//
// Plain SHA-256, deliberately: the input is 256 bits of crypto/rand, so there
// is no dictionary and iteration buys nothing, while this runs on every
// anonymous page load. The migration header records the same reasoning next to
// the column.
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// validToken rejects a path segment that cannot be a token this server issued,
// before it reaches the database.
func validToken(token string) bool {
	if token == "" || len(token) > maxTokenLength {
		return false
	}
	for i := 0; i < len(token); i++ {
		c := token[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// hashPassword derives the stored verifier for a link password.
func hashPassword(password string) (hash, salt []byte, err error) {
	salt = make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, fmt.Errorf("sharedchat: password salt: %w", err)
	}
	hash, err = pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, pbkdf2KeyLength)
	if err != nil {
		return nil, nil, fmt.Errorf("sharedchat: derive password: %w", err)
	}
	return hash, salt, nil
}

// verifyPassword answers whether `password` derives the stored verifier.
//
// subtle.ConstantTimeCompare, not `bytes.Equal`: the comparison is over a value
// an anonymous caller controls one side of, and a byte-at-a-time early return
// leaks the length of the matching prefix of the DERIVED key. That is a weaker
// oracle than comparing raw passwords would be, but it is still an oracle, and
// there is no reason to keep it.
//
// The KDF runs even when the stored verifier is absent — see
// Handler.Unlock, which calls this with a decoy salt precisely so that a link
// WITHOUT a password and a link that does not exist take the same time as one
// whose password was wrong.
func verifyPassword(password string, hash, salt []byte) bool {
	derived, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, pbkdf2KeyLength)
	if err != nil {
		return false
	}
	if len(hash) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare(derived, hash) == 1
}

// grantValue is the value of the unlock cookie: an HMAC over the token hash,
// keyed by the deployment's session secret.
//
// It is NOT the password, not the token, and not a random value the server has
// to remember. A random value would need server-side storage keyed by an
// anonymous visitor, which is a session store for people who are by definition
// not users; an HMAC needs none and is verifiable by any replica.
//
// It is bound to the token hash so that unlocking one link cannot unlock
// another, and it carries no expiry of its own because the cookie is a session
// cookie and the LINK's expiry is re-checked on every view regardless — a
// grant outliving its link would still read nothing.
func grantValue(secret, tokenHash []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("shared-chat-unlock\x00"))
	mac.Write(tokenHash)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// grantValid checks a presented grant in constant time.
func grantValid(secret, tokenHash []byte, presented string) bool {
	if len(secret) == 0 || presented == "" {
		return false
	}
	want := grantValue(secret, tokenHash)
	return subtle.ConstantTimeCompare([]byte(want), []byte(presented)) == 1
}
