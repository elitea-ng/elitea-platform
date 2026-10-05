package middleware_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

func clientVersionRequest(path, version string, user *auth.User, bearer string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if version != "" {
		req.Header.Set(apimw.ClientVersionHeader, version)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if user != nil {
		req = req.WithContext(auth.ContextWithUser(req.Context(), *user))
	}
	return req
}

func TestClientVersionMatrix(t *testing.T) {
	minimums := map[string]string{"ai.elitea.ios": "2.0.0", "ai.elitea.android": ""}
	gate := apimw.ClientVersion(apimw.ClientVersionConfig{
		MinimumFor: func(_ context.Context, clientID string) (string, error) {
			if clientID == "broken" {
				return "", errors.New("store down")
			}
			if v, ok := minimums[clientID]; ok {
				return v, nil
			}
			return "1.0.0", nil
		},
		NativeClientForToken: func(_ context.Context, token string) (string, bool, error) {
			switch token {
			case "elnat_forwarded":
				return "ai.elitea.ios", true, nil
			case "elnat_lookup_fails":
				return "", false, errors.New("store down")
			}
			return "", false, nil
		},
	})
	reached := false
	handler := gate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	}))

	native := &auth.User{UserID: "7", TokenID: "70", AuthType: "token", NativeClientID: "ai.elitea.ios"}
	pat := &auth.User{UserID: "7", TokenID: "71", AuthType: "token"}
	cookie := &auth.User{UserID: "7", AuthType: "session"}
	broken := &auth.User{UserID: "7", TokenID: "72", AuthType: "token", NativeClientID: "broken"}

	cases := []struct {
		name   string
		req    *http.Request
		status int
	}{
		{"native without header passes", clientVersionRequest("/api/v2/projects", "", native, ""), http.StatusNoContent},
		{"native at minimum passes", clientVersionRequest("/api/v2/projects", "2.0.0", native, ""), http.StatusNoContent},
		{"native above minimum passes", clientVersionRequest("/api/v2/projects", "2.1.0+77", native, ""), http.StatusNoContent},
		{"native below minimum is 426", clientVersionRequest("/api/v2/projects", "1.9.9", native, ""), http.StatusUpgradeRequired},
		{"pre-release of the minimum is below it", clientVersionRequest("/api/v2/projects", "2.0.0-rc.1", native, ""), http.StatusUpgradeRequired},
		{"native malformed header is 400", clientVersionRequest("/api/v2/projects", "two", native, ""), http.StatusBadRequest},
		{"PAT caller is exempt (decision 13)", clientVersionRequest("/api/v2/projects", "0.0.1", pat, "elitea_pat"), http.StatusNoContent},
		{"PAT caller with junk header is exempt", clientVersionRequest("/api/v2/projects", "junk", pat, ""), http.StatusNoContent},
		{"cookie caller is exempt", clientVersionRequest("/api/v2/projects", "0.0.1", cookie, ""), http.StatusNoContent},
		{"anonymous request is not judged here", clientVersionRequest("/api/v2/projects", "0.0.1", nil, ""), http.StatusNoContent},
		{"revoke is exempt", clientVersionRequest("/api/v2/auth/native/revoke", "0.0.1", native, ""), http.StatusNoContent},
		{"forwarded principal falls back to the bearer's client", clientVersionRequest("/api/v2/projects", "1.0.0", pat, "elnat_forwarded"), http.StatusUpgradeRequired},
		{"failed bearer lookup passes", clientVersionRequest("/api/v2/projects", "0.0.1", pat, "elnat_lookup_fails"), http.StatusNoContent},
		{"unreadable policy fails open", clientVersionRequest("/api/v2/projects", "0.0.1", broken, ""), http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached = false
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, tc.req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.status, rec.Body.String())
			}
			if reached != (tc.status == http.StatusNoContent) {
				t.Fatalf("handler reached = %v for status %d", reached, rec.Code)
			}
		})
	}
}

func TestClientVersion426Body(t *testing.T) {
	gate := apimw.ClientVersion(apimw.ClientVersionConfig{
		MinimumFor: func(context.Context, string) (string, error) { return "3.1.0", nil },
	})
	handler := gate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("must not be reached") }))
	user := &auth.User{UserID: "7", TokenID: "70", AuthType: "token", NativeClientID: "ai.elitea.ios"}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, clientVersionRequest("/api/v2/projects", "3.0.9", user, ""))
	if rec.Code != http.StatusUpgradeRequired {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get(apimw.MinClientVersionHeader); got != "3.1.0" {
		t.Fatalf("X-Min-Client-Version = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "client_upgrade_required" || body["min_client_version"] != "3.1.0" {
		t.Fatalf("body = %v", body)
	}
}

func TestClientVersionWithoutMinimumSourceIsPassThrough(t *testing.T) {
	handler := apimw.ClientVersion(apimw.ClientVersionConfig{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	user := &auth.User{UserID: "7", NativeClientID: "ai.elitea.ios"}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, clientVersionRequest("/api/v2/projects", "junk", user, ""))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestNativeClientIDIsNeverSerialised(t *testing.T) {
	body, err := json.Marshal(auth.User{ID: "1", NativeClientID: "ai.elitea.ios"})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	for key := range decoded {
		if key == "native_client_id" || key == "NativeClientID" {
			t.Fatalf("NativeClientID leaked into JSON: %s", body)
		}
	}
	var forged auth.User
	if err := json.Unmarshal([]byte(`{"id":"1","NativeClientID":"x","native_client_id":"y"}`), &forged); err != nil {
		t.Fatal(err)
	}
	if forged.NativeClientID != "" {
		t.Fatal("a decoded principal must not be able to claim a native client")
	}
}
