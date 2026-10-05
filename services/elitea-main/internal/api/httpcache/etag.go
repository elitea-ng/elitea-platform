// Package httpcache holds the conditional-request helpers the anonymous,
// ETag'd documents share: the branding bootstrap script and brand pack JSON
// (ADR-0024, ADR-0025 decision 2) and the discovery document (ADR-0025
// decision 1). One copy, because the parsing below carries an RFC 9110 fix
// that a second copy would silently miss.
package httpcache

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// StrongETag returns the strong, quoted entity tag of body and its unquoted
// hex value: `"<sha256-hex>"`, `<sha256-hex>`.
func StrongETag(body []byte) (quoted, value string) {
	sum := sha256.Sum256(body)
	value = hex.EncodeToString(sum[:])
	return `"` + value + `"`, value
}

// ETagMatches implements If-None-Match evaluation (RFC 9110 §13.1.2) against
// one current entity tag: a list of entity-tags or "*", weak comparison (a
// W/ prefix on the client's copy still matches — permitted for GET 304
// revalidation).
//
// Tokenization follows RFC 9110 §8.8.3: an entity-tag is a quoted string, so
// list splitting happens only on commas OUTSIDE quotes — the single
// entity-tag `"abc,*,def"` is one (non-matching) tag, not three candidates.
// "*" is only valid as the ENTIRE field value, never as a list member.
func ETagMatches(ifNoneMatch, etag string) bool {
	if ifNoneMatch == "" {
		return false
	}
	if strings.TrimSpace(ifNoneMatch) == "*" {
		return true
	}
	for _, candidate := range splitETagList(ifNoneMatch) {
		candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
		if candidate == etag {
			return true
		}
	}
	return false
}

// splitETagList splits an If-None-Match field value on commas that sit
// outside quoted strings. Entity-tags cannot contain a DQUOTE (etagc
// excludes it, RFC 9110 §8.8.3), so a plain quote toggle is exact — there is
// no escaping to worry about.
func splitETagList(v string) []string {
	var out []string
	inQuote := false
	start := 0
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '"':
			inQuote = !inQuote
		case ',':
			if !inQuote {
				out = append(out, v[start:i])
				start = i + 1
			}
		}
	}
	return append(out, v[start:])
}
