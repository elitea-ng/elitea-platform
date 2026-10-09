package browserauth

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// IdentitySignatureHeader carries EdgeAuth's proof that it produced the
// X-Auth-* projection on this request. EdgeAuth is elitea-main's own
// forward-auth handler, so the key never leaves elitea-main: the edge only
// copies the header, the way it copies X-Auth-ID.
//
// A socket peer inside trusted_proxy_cidrs is not that proof: an address range
// identifies a network, not the component that chose the headers.
const IdentitySignatureHeader = "X-Auth-Signature"

const (
	// IdentityProjectionLifetime bounds how long one projection verifies.
	// The edge calls EdgeAuth and forwards the request immediately; the window
	// only has to cover that hop plus the clock skew below.
	IdentityProjectionLifetime = 30 * time.Second
	// identityProjectionClockSkew is the tolerated difference between the
	// elitea-main replica that answered EdgeAuth and the one that serves the
	// request.
	identityProjectionClockSkew = 5 * time.Second
	// minIdentityProjectionSecretBytes is the smallest input key accepted.
	minIdentityProjectionSecretBytes = 32
	identityProjectionKeyBytes       = 32
	identityProjectionVersion        = "v1"
	// identityProjectionKeyInfo separates this key from every other use of
	// the input secret (HKDF, RFC 5869).
	identityProjectionKeyInfo = "elitea-main edge identity projection v1"
)

var errUnsignableProjection = errors.New("identity projection cannot be signed")

func deriveIdentityProjectionKey(secret []byte) ([]byte, error) {
	if len(secret) < minIdentityProjectionSecretBytes {
		return nil, ErrInvalidForwardedRequest
	}
	return hkdf.Key(sha256.New, secret, nil, identityProjectionKeyInfo, identityProjectionKeyBytes)
}

// SignIdentityProjection binds the X-Auth-Type, X-Auth-ID and X-Auth-User-ID
// values already in header to the request EdgeAuth authorized (method and
// request URI) and to a short expiry, and sets IdentitySignatureHeader. It
// sets nothing when it refuses.
func (r *TrustedProxyResolver) SignIdentityProjection(header http.Header, method, uri string) error {
	if r == nil || len(r.projectionKey) == 0 {
		return errUnsignableProjection
	}
	authType, authID, userID, ok := projectedIdentity(header)
	if !ok || authType == "" || authID == "" {
		return errUnsignableProjection
	}
	expiry := r.now().Add(IdentityProjectionLifetime).Unix()
	mac, ok := r.projectionMAC(expiry, method, uri, authType, authID, userID)
	if !ok {
		return errUnsignableProjection
	}
	header.Set(IdentitySignatureHeader, identityProjectionVersion+"."+strconv.FormatInt(expiry, 10)+"."+
		base64.RawURLEncoding.EncodeToString(mac))
	return nil
}

// verifyIdentityProjection reports whether request carries an unexpired
// signature over exactly its own identity headers, method and request URI.
func (r *TrustedProxyResolver) verifyIdentityProjection(request *http.Request) bool {
	if len(r.projectionKey) == 0 {
		return false
	}
	values := request.Header.Values(IdentitySignatureHeader)
	if len(values) != 1 {
		return false
	}
	version, rest, found := strings.Cut(values[0], ".")
	if !found || version != identityProjectionVersion {
		return false
	}
	rawExpiry, rawMAC, found := strings.Cut(rest, ".")
	if !found {
		return false
	}
	expiry, err := strconv.ParseInt(rawExpiry, 10, 64)
	if err != nil {
		return false
	}
	now := r.now()
	if now.Unix() > expiry ||
		time.Unix(expiry, 0).Sub(now) > IdentityProjectionLifetime+identityProjectionClockSkew {
		return false
	}
	presented, err := base64.RawURLEncoding.DecodeString(rawMAC)
	if err != nil || len(presented) != sha256.Size {
		return false
	}
	authType, authID, userID, ok := projectedIdentity(request.Header)
	if !ok {
		return false
	}
	expected, ok := r.projectionMAC(expiry, request.Method, request.RequestURI, authType, authID, userID)
	return ok && hmac.Equal(presented, expected)
}

func (r *TrustedProxyResolver) projectionMAC(
	expiry int64,
	method, uri, authType, authID, userID string,
) ([]byte, bool) {
	fields := []string{method, uri, authType, authID, userID}
	if method == "" || uri == "" {
		return nil, false
	}
	for _, field := range fields {
		// Newline separates the fields, so no field may contain one.
		if strings.ContainsAny(field, "\x00\r\n") {
			return nil, false
		}
	}
	mac := hmac.New(sha256.New, r.projectionKey)
	_, _ = mac.Write([]byte(identityProjectionVersion + "\n" + strconv.FormatInt(expiry, 10) + "\n" +
		strings.Join(fields, "\n")))
	return mac.Sum(nil), true
}

// projectedIdentity reads the signed header set. Each name may appear at most
// once; an absent X-Auth-User-ID signs and verifies as empty.
func projectedIdentity(header http.Header) (string, string, string, bool) {
	var values [3]string
	for index, name := range []string{"X-Auth-Type", "X-Auth-ID", "X-Auth-User-ID"} {
		current := header.Values(name)
		if len(current) > 1 {
			return "", "", "", false
		}
		if len(current) == 1 {
			values[index] = current[0]
		}
	}
	return values[0], values[1], values[2], true
}
