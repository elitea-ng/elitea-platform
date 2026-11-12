package social_test

// The shared language preference through the real handler and a real
// database: persisted, returned on both the language route and the author
// read, and untouched by a profile save that carries a stale value.

import (
	"encoding/json"
	"net/http"
	"testing"
)

func (f themeFixture) language() any {
	f.t.Helper()
	recorder := f.request(http.MethodGet, "/author/language", nil)
	if recorder.Code != http.StatusOK {
		f.t.Fatalf("GET /author/language status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		f.t.Fatalf("decode language response %s: %v", recorder.Body.String(), err)
	}
	return decoded["language"]
}

func (f themeFixture) putLanguage(tag string) int {
	f.t.Helper()
	encoded, err := json.Marshal(map[string]any{"language": tag})
	if err != nil {
		f.t.Fatalf("encode language body: %v", err)
	}
	return f.request(http.MethodPut, "/author/language", encoded).Code
}

func TestLanguagePreferenceRoundTrip(t *testing.T) {
	fixture := newThemeFixture(t, "language-round-trip@autotest.local")

	if got := fixture.language(); got != nil {
		t.Fatalf("initial language = %v, want null", got)
	}
	if code := fixture.putLanguage("de"); code != http.StatusOK {
		t.Fatalf("PUT de status = %d, want 200", code)
	}
	if got := fixture.language(); got != "de" {
		t.Fatalf("language = %v, want de", got)
	}
	// Returned on the author read the clients already do.
	personalization, _ := fixture.get()["personalization"].(map[string]any)
	if personalization["language"] != "de" {
		t.Fatalf("author read personalization.language = %v, want de", personalization["language"])
	}
	if code := fixture.putLanguage("not a tag"); code != http.StatusBadRequest {
		t.Fatalf("PUT invalid status = %d, want 400", code)
	}
	if got := fixture.language(); got != "de" {
		t.Fatalf("language after a refused PUT = %v, want de", got)
	}
}

func TestProfileSaveKeepsTheStoredLanguageAndTheme(t *testing.T) {
	fixture := newThemeFixture(t, "language-profile-save@autotest.local")

	fixture.putOK(map[string]any{"name": "T", "personalization": map[string]any{"persona": "qa"}})
	if code := fixture.putLanguage("fr-CA"); code != http.StatusOK {
		t.Fatalf("PUT fr-CA status = %d, want 200", code)
	}
	if code := fixture.putTheme("light"); code != http.StatusOK {
		t.Fatalf("PUT light status = %d, want 200", code)
	}
	if got := fixture.persona(); got != "qa" {
		t.Fatalf("persona after language and theme saves = %v, want qa", got)
	}

	fixture.putOK(map[string]any{"name": "T", "personalization": map[string]any{
		"persona": "nerdy", "language": "ja", "theme_mode": "dark",
	}})
	if got := fixture.language(); got != "fr-CA" {
		t.Fatalf("language after a profile save carrying a stale one = %v, want fr-CA", got)
	}
	if got := fixture.themeMode(); got != "light" {
		t.Fatalf("theme_mode after a profile save = %v, want light", got)
	}
	if got := fixture.persona(); got != "nerdy" {
		t.Fatalf("persona = %v, want nerdy", got)
	}

	fixture.putOK(map[string]any{"name": "T"})
	if got := fixture.language(); got != "fr-CA" {
		t.Fatalf("language after a profile save without personalization = %v, want fr-CA", got)
	}
}

func TestProfileSaveCannotSeedALanguage(t *testing.T) {
	fixture := newThemeFixture(t, "language-profile-seed@autotest.local")
	fixture.putOK(map[string]any{"name": "T", "personalization": map[string]any{"persona": "qa", "language": "de"}})
	if got := fixture.language(); got != nil {
		t.Fatalf("language after a profile save = %v, want null", got)
	}
}
