package social_test

// The shared theme preference (web and desktop), through the real handler and
// a real database.
//
// THE ROUND TRIP THAT MATTERS is the one across the two writers of
// `centry.social_users.personalization`: the theme route merges one key into
// the blob, and `PUT /social/author` replaces the blob with whatever the
// profile form sends. The form was loaded before the toggle last moved, so a
// profile save must neither drop the stored theme nor overwrite it with the
// stale one it carries — and a theme save must not drop the persona.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL); the pool fixture
// and `seedAuthorUser` are personal_project_postgres_integration_test.go's.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/social"
)

type themeFixture struct {
	authorMemoryFixture
	pool *pgxpool.Pool
}

func newThemeFixture(t *testing.T, email string) themeFixture {
	t.Helper()
	pool := newPersonalProjectSocialPool(t)
	return themeFixture{
		authorMemoryFixture: authorMemoryFixture{
			t:      t,
			routes: handler.NewHandler(pool).Routes(),
			email:  email,
			userID: seedAuthorUser(t, pool, email, "Theme Tester"),
		},
		pool: pool,
	}
}

func (f themeFixture) themeMode() any {
	f.t.Helper()
	recorder := f.request(http.MethodGet, "/author/theme", nil)
	if recorder.Code != http.StatusOK {
		f.t.Fatalf("GET /author/theme status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		f.t.Fatalf("decode theme response %s: %v", recorder.Body.String(), err)
	}
	value, present := decoded["theme_mode"]
	if !present {
		f.t.Fatalf("theme_mode is absent from %s", recorder.Body.String())
	}
	return value
}

func (f themeFixture) putTheme(mode string) int {
	f.t.Helper()
	encoded, err := json.Marshal(map[string]any{"theme_mode": mode})
	if err != nil {
		f.t.Fatalf("encode theme body: %v", err)
	}
	return f.request(http.MethodPut, "/author/theme", encoded).Code
}

func (f themeFixture) persona() any {
	f.t.Helper()
	personalization, _ := f.get()["personalization"].(map[string]any)
	return personalization["persona"]
}

func (f themeFixture) setRawPersonalization(raw string) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO centry.social_users (user_id, personalization) VALUES ($1, $2::jsonb)
		ON CONFLICT (user_id) DO UPDATE SET personalization = EXCLUDED.personalization
	`, f.userID, raw); err != nil {
		f.t.Fatalf("seed personalization %s: %v", raw, err)
	}
}

func TestThemePreferenceRoundTrip(t *testing.T) {
	fixture := newThemeFixture(t, "theme-round-trip@autotest.local")

	// Never chosen: null, so the client keeps its own choice.
	if got := fixture.themeMode(); got != nil {
		t.Fatalf("initial theme_mode = %v, want null", got)
	}
	// No social_users row yet: the PUT creates one.
	if code := fixture.putTheme("dark"); code != http.StatusOK {
		t.Fatalf("PUT dark status = %d, want 200", code)
	}
	if got := fixture.themeMode(); got != "dark" {
		t.Fatalf("theme_mode = %v, want dark", got)
	}
	if code := fixture.putTheme("system"); code != http.StatusOK {
		t.Fatalf("PUT system status = %d, want 200", code)
	}
	if got := fixture.themeMode(); got != "system" {
		t.Fatalf("theme_mode = %v, want system", got)
	}
	// Refused values change nothing.
	if code := fixture.putTheme("sepia"); code != http.StatusBadRequest {
		t.Fatalf("PUT sepia status = %d, want 400", code)
	}
	if got := fixture.themeMode(); got != "system" {
		t.Fatalf("theme_mode after a refused PUT = %v, want system", got)
	}
}

func TestProfileSaveKeepsTheStoredTheme(t *testing.T) {
	fixture := newThemeFixture(t, "theme-profile-save@autotest.local")

	fixture.putOK(map[string]any{"name": "T", "personalization": map[string]any{"persona": "qa"}})
	if code := fixture.putTheme("light"); code != http.StatusOK {
		t.Fatalf("PUT light status = %d, want 200", code)
	}
	// The theme save merged: the persona is still there.
	if got := fixture.persona(); got != "qa" {
		t.Fatalf("persona after a theme save = %v, want qa", got)
	}

	// A profile form loaded earlier carries a stale theme: the stored one wins.
	fixture.putOK(map[string]any{"name": "T", "personalization": map[string]any{
		"persona": "nerdy", "theme_mode": "dark",
	}})
	if got := fixture.themeMode(); got != "light" {
		t.Fatalf("theme_mode after a profile save carrying a stale one = %v, want light", got)
	}
	if got := fixture.persona(); got != "nerdy" {
		t.Fatalf("persona = %v, want nerdy", got)
	}

	// A profile save that sends no personalization at all keeps it too.
	fixture.putOK(map[string]any{"name": "T"})
	if got := fixture.themeMode(); got != "light" {
		t.Fatalf("theme_mode after a profile save without personalization = %v, want light", got)
	}
}

func TestProfileSaveCannotSeedATheme(t *testing.T) {
	fixture := newThemeFixture(t, "theme-profile-seed@autotest.local")

	// The theme route owns the key: a profile body cannot introduce one.
	fixture.putOK(map[string]any{"name": "T", "personalization": map[string]any{
		"persona": "qa", "theme_mode": "dark",
	}})
	if got := fixture.themeMode(); got != nil {
		t.Fatalf("theme_mode after a profile save = %v, want null", got)
	}
	if got := fixture.persona(); got != "qa" {
		t.Fatalf("persona = %v, want qa", got)
	}
}

func TestThemePreferenceOverOddStoredBlobs(t *testing.T) {
	fixture := newThemeFixture(t, "theme-odd-blobs@autotest.local")

	// A value this API would refuse reads as never chosen.
	fixture.setRawPersonalization(`{"persona":"qa","theme_mode":"sepia"}`)
	if got := fixture.themeMode(); got != nil {
		t.Fatalf("stored sepia reads as %v, want null", got)
	}
	// A non-object blob (the JSON null `PUT /social/author` stores for a body
	// without personalization) reads as unset and is replaced by a save.
	fixture.setRawPersonalization(`null`)
	if got := fixture.themeMode(); got != nil {
		t.Fatalf("JSON-null blob reads as %v, want null", got)
	}
	if code := fixture.putTheme("dark"); code != http.StatusOK {
		t.Fatalf("PUT dark over a JSON-null blob status = %d, want 200", code)
	}
	if got := fixture.themeMode(); got != "dark" {
		t.Fatalf("theme_mode = %v, want dark", got)
	}
}
