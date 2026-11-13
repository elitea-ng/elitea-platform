package social_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	handler "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/social"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

func languageRequest(t *testing.T, method, body string, user *auth.User) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "/author/language", strings.NewReader(body))
	if user != nil {
		request = request.WithContext(auth.ContextWithUser(request.Context(), *user))
	}
	recorder := httptest.NewRecorder()
	handler.NewHandler(nil).Routes().ServeHTTP(recorder, request)
	return recorder
}

func TestValidLanguageTag(t *testing.T) {
	for _, tag := range []string{"en", "de", "pt-BR", "fr-CA", "zh-TW", "zh-Hant-TW", "es-419", "fil"} {
		if !handler.ValidLanguageTag(tag) {
			t.Errorf("ValidLanguageTag(%q) = false, want true", tag)
		}
	}
	for _, tag := range []string{"", "EN", "e", "pt_BR", "pt-br", "en-US-x-private", " de", "<script>", "de-", strings.Repeat("a", 40)} {
		if handler.ValidLanguageTag(tag) {
			t.Errorf("ValidLanguageTag(%q) = true, want false", tag)
		}
	}
}

func TestLanguagePreferenceRequiresAnOwningUser(t *testing.T) {
	tokenOnly := &auth.User{ID: "tok-1", TokenID: "tok-1", AuthType: "token"}
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		for name, user := range map[string]*auth.User{"anonymous": nil, "token without owner": tokenOnly} {
			if recorder := languageRequest(t, method, `{"language":"de"}`, user); recorder.Code != http.StatusUnauthorized {
				t.Errorf("%s %s: status = %d, want 401", method, name, recorder.Code)
			}
		}
	}
}

func TestUpdateLanguagePreferenceValidatesTheTag(t *testing.T) {
	for _, body := range []string{
		`{"language":"klingon-x"}`, `{"language":"DE"}`, `{"language":""}`,
		`{"language":null}`, `{"language":1}`, `{}`, `not json`,
	} {
		if recorder := languageRequest(t, http.MethodPut, body, themeUser); recorder.Code != http.StatusBadRequest {
			t.Errorf("PUT %s: status = %d, want 400 (body %s)", body, recorder.Code, recorder.Body.String())
		}
	}
}

func TestLanguagePreferenceWithoutADatabase(t *testing.T) {
	get := languageRequest(t, http.MethodGet, "", themeUser)
	if get.Code != http.StatusOK || strings.TrimSpace(get.Body.String()) != `{"language":null}` {
		t.Errorf("GET = %d %s, want 200 {\"language\":null}", get.Code, get.Body.String())
	}
	put := languageRequest(t, http.MethodPut, `{"language":"pt-BR"}`, themeUser)
	if put.Code != http.StatusOK || strings.TrimSpace(put.Body.String()) != `{"language":"pt-BR"}` {
		t.Errorf("PUT = %d %s, want 200 {\"language\":\"pt-BR\"}", put.Code, put.Body.String())
	}
}
