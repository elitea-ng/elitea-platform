package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

func TestBrowserWriteOrigin(t *testing.T) {
	session := func(r *http.Request) *http.Request {
		return r.WithContext(auth.ContextWithAuthenticatedUser(r.Context(),
			auth.User{ID: "1", UserID: "1"}, auth.AuthenticationSourceSession))
	}
	forwardedUser := func(r *http.Request) *http.Request {
		return r.WithContext(auth.ContextWithAuthenticatedUser(r.Context(),
			auth.User{ID: "1", UserID: "1", AuthType: "user"}, auth.AuthenticationSourceForwarded))
	}
	forwardedToken := func(r *http.Request) *http.Request {
		return r.WithContext(auth.ContextWithAuthenticatedUser(r.Context(),
			auth.User{ID: "9", TokenID: "9", UserID: "1", AuthType: "token"}, auth.AuthenticationSourceForwarded))
	}
	bearer := func(r *http.Request) *http.Request {
		return r.WithContext(auth.ContextWithAuthenticatedUser(r.Context(),
			auth.User{ID: "1", UserID: "1"}, auth.AuthenticationSourceToken))
	}

	cases := []struct {
		name    string
		method  string
		as      func(*http.Request) *http.Request
		headers map[string]string
		want    int
	}{
		{"cookie POST from a sibling site", http.MethodPost, session, map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://evil.example.com"}, http.StatusForbidden},
		{"cookie POST cross-site", http.MethodPost, session, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"cookie POST with a foreign Origin and no Sec-Fetch-Site", http.MethodPost, session, map[string]string{"Origin": "https://evil.example.com"}, http.StatusForbidden},
		{"edge-cookie POST from a sibling site", http.MethodPost, forwardedUser, map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"cookie POST from this origin", http.MethodPost, session, map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "https://app.example.com"}, http.StatusOK},
		{"cookie POST, Origin is this host", http.MethodPost, session, map[string]string{"Origin": "https://app.example.com"}, http.StatusOK},
		{"cookie POST with the project selector", http.MethodPost, session, map[string]string{"Sec-Fetch-Site": "same-site", "X-Project-Id": "7"}, http.StatusOK},
		{"cookie POST from a non-browser client", http.MethodPost, session, nil, http.StatusOK},
		{"cookie GET from a sibling site", http.MethodGet, session, map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusOK},
		{"bearer POST cross-site", http.MethodPost, bearer, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusOK},
		{"forwarded token POST cross-site", http.MethodPost, forwardedToken, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached := false
			h := BrowserWriteOrigin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest(tc.method, "https://app.example.com/llm/v1/audio/transcriptions", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, tc.as(req))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if reached != (tc.want == http.StatusOK) {
				t.Fatalf("handler reached = %v for status %d", reached, rec.Code)
			}
		})
	}
}
