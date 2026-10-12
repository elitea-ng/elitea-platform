package social_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/social"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// themeRequest serves one request through the real router with a nil pool, so
// everything up to the database is exercised: routing, the principal check
// and the enum validation.
func themeRequest(t *testing.T, method, body string, user *auth.User) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "/author/theme", strings.NewReader(body))
	if user != nil {
		request = request.WithContext(auth.ContextWithUser(request.Context(), *user))
	}
	recorder := httptest.NewRecorder()
	handler.NewHandler(nil).Routes().ServeHTTP(recorder, request)
	return recorder
}

var themeUser = &auth.User{ID: "7", UserID: "7", Email: "theme@autotest.local"}

func TestValidThemeMode(t *testing.T) {
	for _, mode := range []string{"system", "light", "dark"} {
		if !handler.ValidThemeMode(mode) {
			t.Errorf("ValidThemeMode(%q) = false, want true", mode)
		}
	}
	for _, mode := range []string{"", "Dark", "auto", "high-contrast", " light"} {
		if handler.ValidThemeMode(mode) {
			t.Errorf("ValidThemeMode(%q) = true, want false", mode)
		}
	}
}

func TestThemePreferenceRequiresAnOwningUser(t *testing.T) {
	tokenOnly := &auth.User{ID: "tok-1", TokenID: "tok-1", AuthType: "token"}
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		for name, user := range map[string]*auth.User{"anonymous": nil, "token without owner": tokenOnly} {
			recorder := themeRequest(t, method, `{"theme_mode":"dark"}`, user)
			if recorder.Code != http.StatusUnauthorized {
				t.Errorf("%s %s: status = %d, want 401", method, name, recorder.Code)
			}
		}
	}
}

func TestUpdateThemePreferenceValidatesTheMode(t *testing.T) {
	for _, body := range []string{
		`{"theme_mode":"auto"}`,
		`{"theme_mode":"DARK"}`,
		`{"theme_mode":""}`,
		`{"theme_mode":null}`,
		`{"theme_mode":1}`,
		`{}`,
		`not json`,
	} {
		recorder := themeRequest(t, http.MethodPut, body, themeUser)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("PUT %s: status = %d, want 400 (body %s)", body, recorder.Code, recorder.Body.String())
		}
	}
}

func TestThemePreferenceWithoutADatabase(t *testing.T) {
	get := themeRequest(t, http.MethodGet, "", themeUser)
	if get.Code != http.StatusOK || strings.TrimSpace(get.Body.String()) != `{"theme_mode":null}` {
		t.Errorf("GET = %d %s, want 200 {\"theme_mode\":null}", get.Code, get.Body.String())
	}
	put := themeRequest(t, http.MethodPut, `{"theme_mode":"light"}`, themeUser)
	if put.Code != http.StatusOK || strings.TrimSpace(put.Body.String()) != `{"theme_mode":"light"}` {
		t.Errorf("PUT = %d %s, want 200 {\"theme_mode\":\"light\"}", put.Code, put.Body.String())
	}
}
