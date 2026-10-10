package social

// The caller's colour-theme preference: `system`, `light` or `dark`.
//
// WHY IT IS SERVER-STORED. The web app and the desktop app (Tauri, the same
// elitea-web build in `desktop` mode) run on different origins, so the
// browser-local `el-mode` key the theme toggle writes is never shared between
// them: one person saw two themes. The server value is what the two clients
// agree on; `el-mode` stays as each client's first-paint cache.
//
// WHERE IT LIVES. `centry.social_users.personalization->'theme_mode'` — the
// author record's existing per-user settings blob, the one Settings › AI
// Personality already writes `persona` and `default_instructions` into. No
// new table or column: social_users is a pylon-baseline table that no
// numbered migration owns (001_initial.sql projects it; claiming it in a new
// migration breaks the repos seeds), and a jsonb key needs no DDL at all.
//
// WHO OWNS THE KEY. These two routes, and only these. `PUT /social/author`
// replaces `personalization` outright with whatever the profile form sends,
// and that form was loaded before the user last flipped the toggle — so it
// strips any `theme_mode` from its body and carries the STORED one forward
// (UpdateAuthor). Without that, every profile save would undo the last theme
// change made on any client.

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// themeModeKey is the personalization key holding the preference.
const themeModeKey = "theme_mode"

// validThemeModes are the values the web app's toggle can set — MUI's
// `useColorScheme().setMode` modes.
var validThemeModes = map[string]struct{}{
	"system": {},
	"light":  {},
	"dark":   {},
}

// ValidThemeMode reports whether mode is one of `system`, `light`, `dark`.
func ValidThemeMode(mode string) bool {
	_, ok := validThemeModes[mode]
	return ok
}

// ThemePreference is the body of both theme routes. A nil ThemeMode on GET
// means the user has never chosen one: the client keeps its local choice
// rather than being reset to `system`.
type ThemePreference struct {
	ThemeMode *string `json:"theme_mode"`
}

// GetThemePreference answers the caller's stored theme mode, or null.
func (h *Handler) GetThemePreference(w http.ResponseWriter, r *http.Request) {
	userID, ok := themeOwner(w, r)
	if !ok {
		return
	}
	if h.pool == nil {
		writeJSON(w, http.StatusOK, ThemePreference{})
		return
	}

	ctx := r.Context()
	var stored *string
	err := h.pool.QueryRow(ctx, `
		SELECT personalization->>'theme_mode'
		FROM centry.social_users
		WHERE user_id = $1 AND jsonb_typeof(personalization) = 'object'
	`, userID).Scan(&stored)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.ErrorContext(ctx, "social: read theme preference", "err", err, "user_id", userID)
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to read the theme preference")
		return
	}
	// A value this API would refuse to store is "never chosen", not an error:
	// the blob is free-form and `PUT /social/author` once took anything.
	if stored != nil && !ValidThemeMode(*stored) {
		stored = nil
	}
	writeJSON(w, http.StatusOK, ThemePreference{ThemeMode: stored})
}

// UpdateThemePreference stores the caller's theme mode, leaving every other
// personalization key as it is.
func (h *Handler) UpdateThemePreference(w http.ResponseWriter, r *http.Request) {
	userID, ok := themeOwner(w, r)
	if !ok {
		return
	}

	var body ThemePreference
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.ThemeMode == nil || !ValidThemeMode(*body.ThemeMode) {
		writeJSON(w, http.StatusBadRequest, struct {
			Error string `json:"error"`
			Field string `json:"field"`
		}{Error: "theme_mode must be one of system, light, dark", Field: themeModeKey})
		return
	}
	mode := *body.ThemeMode
	if h.pool == nil {
		writeJSON(w, http.StatusOK, ThemePreference{ThemeMode: &mode})
		return
	}

	// A merge, never a replace: persona, instructions and anything else in the
	// blob survive. A blob that is not an object (SQL NULL, or the JSON `null`
	// `PUT /social/author` stores when its body has no personalization) is
	// replaced by a fresh object.
	ctx := r.Context()
	tag, err := h.pool.Exec(ctx, `
		INSERT INTO centry.social_users (user_id, personalization)
		SELECT au.id, jsonb_build_object('theme_mode', $2::text)
		FROM auth_core__user au WHERE au.id = $1
		ON CONFLICT (user_id) DO UPDATE SET personalization =
			CASE WHEN jsonb_typeof(social_users.personalization) = 'object'
				THEN social_users.personalization || jsonb_build_object('theme_mode', $2::text)
				ELSE jsonb_build_object('theme_mode', $2::text)
			END
	`, userID, mode)
	if err != nil {
		slog.ErrorContext(ctx, "social: write theme preference", "err", err, "user_id", userID)
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to save the theme preference")
		return
	}
	if tag.RowsAffected() == 0 {
		// The principal names no account row to hang a profile on.
		apierr.WriteStatus(w, http.StatusNotFound, "no account for the authenticated user")
		return
	}
	writeJSON(w, http.StatusOK, ThemePreference{ThemeMode: &mode})
}

// themeOwner resolves the account the preference belongs to: the
// authenticated principal's OWN user, never a token id. It writes the 401
// itself when there is none.
func themeOwner(w http.ResponseWriter, r *http.Request) (int64, bool) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		apierr.WriteStatus(w, http.StatusUnauthorized, "unauthorized")
		return 0, false
	}
	userID, ok := user.OwningUserID()
	if !ok {
		apierr.WriteStatus(w, http.StatusUnauthorized, "unauthorized")
		return 0, false
	}
	return userID, true
}
