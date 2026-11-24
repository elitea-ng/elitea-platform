package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// consoleTestSessionSecret signs the elitea_session cookie asConsoleSession
// sends. A router it wraps must carry it as RouterConfig.SessionSecret.
const consoleTestSessionSecret = "console-test-session-secret"

// asConsoleSession serves router as a browser session of user: it drops the
// request's Authorization header and sends user's signed elitea_session
// cookie instead.
//
// Since ADR-0059 R1.2a the credential decides the administration set: a
// principal a token validator returns over the Bearer path has no browser
// kind and resolves no administration permission, whatever its AuthType
// says. A test that drives an administration route as an administrator's
// console therefore has to arrive as one.
func asConsoleSession(router http.Handler, user auth.User) http.Handler {
	payload, err := json.Marshal(map[string]any{
		"uid": user.UserID, "email": user.Email, "exp": time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		panic(err)
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(consoleTestSessionSecret))
	mac.Write([]byte(body))
	cookie := &http.Cookie{Name: "elitea_session", Value: body + "." + hex.EncodeToString(mac.Sum(nil))}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del("Authorization")
		r.AddCookie(cookie)
		router.ServeHTTP(w, r)
	})
}
