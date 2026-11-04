package webpush

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func b64(t *testing.T, value string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		t.Fatalf("decode %q: %v", value, err)
	}
	return raw
}

// TestEncryptMatchesRFC8291Example encrypts the worked example of RFC 8291
// section 5 with its fixed salt and application server key, and expects the
// RFC's message byte for byte.
func TestEncryptMatchesRFC8291Example(t *testing.T) {
	const (
		plaintext = "When I grow up, I want to be a watermelon"
		asPrivate = "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"
		uaPublic  = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
		auth      = "BTBZMqHH6r4Tts7J_aSIgg"
		salt      = "DGv6ra1nlYgDCS1FRnbzlw"
		message   = "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	)
	ephemeral, err := ecdh.P256().NewPrivateKey(b64(t, asPrivate))
	if err != nil {
		t.Fatal(err)
	}
	got, err := encrypt([]byte(plaintext), uaPublic, auth, encryption{salt: b64(t, salt), ephemeral: ephemeral})
	if err != nil {
		t.Fatal(err)
	}
	if want := b64(t, message); !bytes.Equal(got, want) {
		t.Fatalf("encrypt = %s\nwant      %s", base64.RawURLEncoding.EncodeToString(got), message)
	}
}

// decrypt is the user agent's side of RFC 8291, for the tests.
func decrypt(t *testing.T, body []byte, uaPrivate *ecdh.PrivateKey, authSecret []byte) []byte {
	t.Helper()
	if len(body) < 21 {
		t.Fatal("body too short")
	}
	salt, rs, idlen := body[:16], binary.BigEndian.Uint32(body[16:20]), int(body[20])
	if rs != recordSize {
		t.Fatalf("rs = %d", rs)
	}
	asPublicRaw := body[21 : 21+idlen]
	asPublic, err := ecdh.P256().NewPublicKey(asPublicRaw)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := uaPrivate.ECDH(asPublic)
	if err != nil {
		t.Fatal(err)
	}
	key, nonce, err := contentKeys(shared, authSecret, uaPrivate.PublicKey().Bytes(), asPublicRaw, salt)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	record, err := gcm.Open(nil, nonce, body[21+idlen:], nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	end := bytes.LastIndexByte(record, 0x02)
	if end < 0 || len(bytes.Trim(record[end+1:], "\x00")) != 0 {
		t.Fatal("no last-record delimiter")
	}
	return record[:end]
}

func TestEncryptRoundTrip(t *testing.T) {
	ua, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authSecret := make([]byte, 16)
	_, _ = rand.Read(authSecret)
	p256dh := base64.RawURLEncoding.EncodeToString(ua.PublicKey().Bytes())
	auth := base64.RawURLEncoding.EncodeToString(authSecret)
	if !ValidSubscriptionKeys(p256dh, auth) {
		t.Fatal("generated keys are not valid")
	}
	payload := []byte(`{"type":"run_finished","notification_id":1}`)
	body, err := Encrypt(payload, p256dh, auth)
	if err != nil {
		t.Fatal(err)
	}
	if got := decrypt(t, body, ua, authSecret); !bytes.Equal(got, payload) {
		t.Fatalf("round trip = %q", got)
	}
	// Padded standard base64 keys work as well.
	body, err = Encrypt(payload, base64.StdEncoding.EncodeToString(ua.PublicKey().Bytes()),
		base64.URLEncoding.EncodeToString(authSecret))
	if err != nil {
		t.Fatal(err)
	}
	if got := decrypt(t, body, ua, authSecret); !bytes.Equal(got, payload) {
		t.Fatalf("round trip (padded keys) = %q", got)
	}
}

func TestEncryptRefusesBadInput(t *testing.T) {
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	p256dh := base64.RawURLEncoding.EncodeToString(ua.PublicKey().Bytes())
	auth := base64.RawURLEncoding.EncodeToString(make([]byte, 16))
	if _, err := Encrypt(make([]byte, maxPlaintext+1), p256dh, auth); err == nil {
		t.Fatal("an oversized payload is accepted")
	}
	if _, err := Encrypt([]byte("x"), "AAAA", auth); err == nil {
		t.Fatal("a bad p256dh is accepted")
	}
	if _, err := Encrypt([]byte("x"), p256dh, "AAAA"); err == nil {
		t.Fatal("a short auth is accepted")
	}
	if ValidSubscriptionKeys("not base64!", auth) || ValidSubscriptionKeys(p256dh, "AAAA") {
		t.Fatal("bad keys reported valid")
	}
}

func TestKeysSurviveTheVaultEncoding(t *testing.T) {
	keys, err := GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	if raw := b64(t, keys.Public); len(raw) != 65 || raw[0] != 0x04 {
		t.Fatalf("public key is not an uncompressed point: %d bytes", len(raw))
	}
	encoded, err := keys.EncodePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParsePrivateKey(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Public != keys.Public {
		t.Fatal("the public key changed through the vault encoding")
	}
	if _, err := ParsePrivateKey("not a key"); err == nil {
		t.Fatal("garbage parsed")
	}
}

func TestAuthorizationIsAValidVAPIDToken(t *testing.T) {
	keys, err := GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	header, err := keys.Authorization("https://push.example.com/send/abc?x=1", "mailto:ops@example.com", now)
	if err != nil {
		t.Fatal(err)
	}
	token, key, ok := strings.Cut(strings.TrimPrefix(header, "vapid t="), ", k=")
	if !ok || !strings.HasPrefix(header, "vapid t=") || key != keys.Public {
		t.Fatalf("header = %q", header)
	}
	public, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), b64(t, key))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jwt.Parse(token, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodES256 {
			return nil, errors.New("not ES256")
		}
		return public, nil
	})
	if err != nil || !parsed.Valid {
		t.Fatalf("token does not verify: %v", err)
	}
	claims := parsed.Claims.(jwt.MapClaims)
	if claims["aud"] != "https://push.example.com" || claims["sub"] != "mailto:ops@example.com" {
		t.Fatalf("claims = %v", claims)
	}
	exp, _ := claims.GetExpirationTime()
	if exp == nil || exp.Sub(now) > 24*time.Hour || exp.Sub(now) <= 0 {
		t.Fatalf("exp = %v", exp)
	}
	if _, err := keys.Authorization("not a url", "mailto:x@example.com", now); err == nil {
		t.Fatal("a relative endpoint is accepted")
	}
}
