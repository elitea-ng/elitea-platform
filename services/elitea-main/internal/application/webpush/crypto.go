package webpush

// Message encryption (RFC 8291, aes128gcm per RFC 8188) and VAPID (RFC 8292).
//
// Why not a library: the Go web push packages build on the deprecated
// crypto/elliptic point arithmetic, and the most used one
// (SherClockHolmes/webpush-go v1.4.0, January 2025) also prefixes "mailto:"
// to a subject that already has it and ignores its own expiration option.
// The standard library now has every primitive (crypto/ecdh, crypto/hkdf,
// crypto/ecdsa raw keys), so the whole protocol is the short code below. The
// test checks it against the worked example of RFC 8291 section 5 and
// decrypts a round trip.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// recordSize is the aes128gcm record size: one record holds the whole
// payload (RFC 8291 section 4: at most 4096 bytes).
const recordSize = 4096

// maxPlaintext is the largest payload one record carries: the record size,
// minus the header (16 salt + 4 rs + 1 idlen + 65 key), the padding
// delimiter and the 16-byte tag.
const maxPlaintext = recordSize - 86 - 1 - 16

// Keys is the install's VAPID key pair (P-256).
type Keys struct {
	private *ecdsa.PrivateKey
	// Public is the uncompressed public key, base64url without padding: the
	// applicationServerKey a browser subscribes with.
	Public string
}

// GenerateKeys makes a new VAPID key pair.
func GenerateKeys() (Keys, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Keys{}, fmt.Errorf("web push: generate the VAPID key: %w", err)
	}
	return keysOf(key)
}

// ParsePrivateKey reads a stored private key (EncodePrivateKey).
func ParsePrivateKey(encoded string) (Keys, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(encoded, "="))
	if err != nil {
		return Keys{}, errors.New("web push: the stored VAPID key is not base64url")
	}
	key, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), raw)
	if err != nil {
		return Keys{}, errors.New("web push: the stored VAPID key is not a P-256 private key")
	}
	return keysOf(key)
}

func keysOf(key *ecdsa.PrivateKey) (Keys, error) {
	public, err := key.PublicKey.Bytes()
	if err != nil {
		return Keys{}, fmt.Errorf("web push: encode the VAPID public key: %w", err)
	}
	return Keys{private: key, Public: base64.RawURLEncoding.EncodeToString(public)}, nil
}

// EncodePrivateKey is the private key as the vault stores it: the 32-byte
// scalar, base64url without padding.
func (k Keys) EncodePrivateKey() (string, error) {
	if k.private == nil {
		return "", errors.New("web push: no VAPID key")
	}
	raw, err := k.private.Bytes()
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// Valid reports a usable key pair.
func (k Keys) Valid() bool { return k.private != nil && k.Public != "" }

// vapidLifetime is the lifetime of one VAPID token (RFC 8292: at most 24
// hours).
const vapidLifetime = 12 * time.Hour

// Authorization is the VAPID Authorization header for an endpoint (RFC 8292
// section 3): vapid t=<ES256 JWT>, k=<public key>.
func (k Keys) Authorization(endpoint, subject string, now time.Time) (string, error) {
	if !k.Valid() {
		return "", errors.New("web push: no VAPID key")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("web push: the endpoint is not an absolute URL")
	}
	token := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"aud": parsed.Scheme + "://" + parsed.Host,
		"exp": now.Add(vapidLifetime).Unix(),
		"sub": subject,
	})
	signed, err := token.SignedString(k.private)
	if err != nil {
		return "", fmt.Errorf("web push: sign the VAPID token: %w", err)
	}
	return "vapid t=" + signed + ", k=" + k.Public, nil
}

// decodeKey reads a browser key: base64url (or base64), padded or not.
func decodeKey(value string) ([]byte, error) {
	trimmed := strings.TrimRight(value, "=")
	if raw, err := base64.RawURLEncoding.DecodeString(trimmed); err == nil {
		return raw, nil
	}
	return base64.RawStdEncoding.DecodeString(trimmed)
}

// ValidSubscriptionKeys reports keys a browser could have given: p256dh an
// uncompressed P-256 point, auth 16 bytes.
func ValidSubscriptionKeys(p256dh, auth string) bool {
	public, err := decodeKey(p256dh)
	if err != nil {
		return false
	}
	if _, err := ecdh.P256().NewPublicKey(public); err != nil {
		return false
	}
	secret, err := decodeKey(auth)
	return err == nil && len(secret) == 16
}

// encryption is the per-message randomness; tests replace it.
type encryption struct {
	salt      []byte
	ephemeral *ecdh.PrivateKey
}

func newEncryption(random io.Reader) (encryption, error) {
	salt := make([]byte, 16)
	if _, err := io.ReadFull(random, salt); err != nil {
		return encryption{}, err
	}
	ephemeral, err := ecdh.P256().GenerateKey(random)
	if err != nil {
		return encryption{}, err
	}
	return encryption{salt: salt, ephemeral: ephemeral}, nil
}

// Encrypt encrypts a payload for a subscription (RFC 8291 section 3.4): one
// aes128gcm record with the header salt, rs, idlen and keyid (the
// ephemeral public key).
func Encrypt(plaintext []byte, p256dh, auth string) ([]byte, error) {
	params, err := newEncryption(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("web push: %w", err)
	}
	return encrypt(plaintext, p256dh, auth, params)
}

func encrypt(plaintext []byte, p256dh, auth string, params encryption) ([]byte, error) {
	if len(plaintext) > maxPlaintext {
		return nil, errors.New("web push: the payload is too large")
	}
	uaPublicRaw, err := decodeKey(p256dh)
	if err != nil {
		return nil, errors.New("web push: p256dh is not base64url")
	}
	uaPublic, err := ecdh.P256().NewPublicKey(uaPublicRaw)
	if err != nil {
		return nil, errors.New("web push: p256dh is not a P-256 point")
	}
	authSecret, err := decodeKey(auth)
	if err != nil || len(authSecret) != 16 {
		return nil, errors.New("web push: auth is not a 16-byte secret")
	}
	shared, err := params.ephemeral.ECDH(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("web push: ECDH: %w", err)
	}
	asPublic := params.ephemeral.PublicKey().Bytes()
	key, nonce, err := contentKeys(shared, authSecret, uaPublic.Bytes(), asPublic, params.salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	var body bytes.Buffer
	body.Write(params.salt)
	_ = binary.Write(&body, binary.BigEndian, uint32(recordSize))
	body.WriteByte(byte(len(asPublic)))
	body.Write(asPublic)
	// The last (and only) record ends with the delimiter 0x02, no padding.
	record := append(append([]byte(nil), plaintext...), 0x02)
	body.Write(gcm.Seal(nil, nonce, record, nil))
	return body.Bytes(), nil
}

// contentKeys derives the content encryption key and the nonce (RFC 8291
// section 3.3 and 3.4).
func contentKeys(shared, authSecret, uaPublic, asPublic, salt []byte) (key, nonce []byte, err error) {
	info := append(append([]byte("WebPush: info\x00"), uaPublic...), asPublic...)
	ikm, err := hkdf.Key(sha256.New, shared, authSecret, string(info), 32)
	if err != nil {
		return nil, nil, err
	}
	key, err = hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, nil, err
	}
	nonce, err = hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, nil, err
	}
	return key, nonce, nil
}
