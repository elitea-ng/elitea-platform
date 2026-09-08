package middleware

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

func TestSessionCookieDoesNotFabricateLegacyRole(t *testing.T) {
	secret := "0123456789abcdef0123456789abcdef"
	payload, err := json.Marshal(map[string]any{
		"uid":   "7",
		"email": "user@example.com",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(encoded))
	token := encoded + "." + hex.EncodeToString(mac.Sum(nil))

	user, refusal, ok := verifySessionCookie(token, secret)
	if !ok {
		t.Fatal("valid test cookie was rejected")
	}
	if refusal != "" {
		t.Fatalf("refusal reason for an accepted cookie = %q, want empty", refusal)
	}
	if len(user.Roles) != 0 {
		t.Fatalf("session roles = %v, want authoritative resolver only", user.Roles)
	}
}
