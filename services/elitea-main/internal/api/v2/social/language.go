package social

// The caller's interface-language preference: a BCP-47 tag such as `de` or
// `pt-BR`.
//
// WHY IT IS SERVER-STORED. The web app, the desktop app and (later) the mobile
// app do not share browser storage, so the browser-local `el.lang` choice
// never followed a person from one client to another. The server value is what
// the clients agree on; `el.lang` stays the first-paint / pre-auth cache.
//
// WHERE IT LIVES. `centry.social_users.personalization->'language'`, beside
// `theme_mode` (theme.go explains why no migration is needed). The same
// ownership rule applies: only these routes write the key, and
// `PUT /social/author` carries the stored value forward (UpdateAuthor).
//
// WHAT IS ACCEPTED. A strict tag shape, not the list of languages the web
// build ships: the clients ship different sets and add languages over time,
// and a client falls back to its own default for a tag it has no catalogue
// for. Validating the shape keeps junk out of the blob.

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// languageKey is the personalization key holding the preference.
const languageKey = "language"

// languageTag is a language subtag, an optional script and an optional region
// (`de`, `zh-TW`, `zh-Hant-TW`, `es-419`): the subset of BCP-47 the clients'
// locale lists use.
var languageTag = regexp.MustCompile(`^[a-z]{2,3}(-[A-Z][a-z]{3})?(-([A-Z]{2}|[0-9]{3}))?$`)

// ValidLanguageTag reports whether tag has the shape of a supported language tag.
func ValidLanguageTag(tag string) bool {
	return len(tag) <= 16 && languageTag.MatchString(tag)
}

// LanguagePreference is the body of both language routes. A nil Language on
// GET means the user has never chosen one.
type LanguagePreference struct {
	Language *string `json:"language"`
}

// GetLanguagePreference answers the caller's stored language, or null.
func (h *Handler) GetLanguagePreference(w http.ResponseWriter, r *http.Request) {
	userID, ok := themeOwner(w, r)
	if !ok {
		return
	}
	if h.pool == nil {
		writeJSON(w, http.StatusOK, LanguagePreference{})
		return
	}

	ctx := r.Context()
	var stored *string
	err := h.pool.QueryRow(ctx, `
		SELECT personalization->>'language'
		FROM centry.social_users
		WHERE user_id = $1 AND jsonb_typeof(personalization) = 'object'
	`, userID).Scan(&stored)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.ErrorContext(ctx, "social: read language preference", "err", err, "user_id", userID)
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to read the language preference")
		return
	}
	if stored != nil && !ValidLanguageTag(*stored) {
		stored = nil
	}
	writeJSON(w, http.StatusOK, LanguagePreference{Language: stored})
}

// UpdateLanguagePreference stores the caller's language, leaving every other
// personalization key as it is.
func (h *Handler) UpdateLanguagePreference(w http.ResponseWriter, r *http.Request) {
	userID, ok := themeOwner(w, r)
	if !ok {
		return
	}

	var body LanguagePreference
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Language == nil || !ValidLanguageTag(*body.Language) {
		writeJSON(w, http.StatusBadRequest, struct {
			Error string `json:"error"`
			Field string `json:"field"`
		}{Error: "language must be a language tag such as en, de or pt-BR", Field: languageKey})
		return
	}
	language := *body.Language
	if h.pool == nil {
		writeJSON(w, http.StatusOK, LanguagePreference{Language: &language})
		return
	}

	ctx := r.Context()
	tag, err := h.pool.Exec(ctx, `
		INSERT INTO centry.social_users (user_id, personalization)
		SELECT au.id, jsonb_build_object('language', $2::text)
		FROM auth_core__user au WHERE au.id = $1
		ON CONFLICT (user_id) DO UPDATE SET personalization =
			CASE WHEN jsonb_typeof(social_users.personalization) = 'object'
				THEN social_users.personalization || jsonb_build_object('language', $2::text)
				ELSE jsonb_build_object('language', $2::text)
			END
	`, userID, language)
	if err != nil {
		slog.ErrorContext(ctx, "social: write language preference", "err", err, "user_id", userID)
		apierr.WriteStatus(w, http.StatusInternalServerError, "failed to save the language preference")
		return
	}
	if tag.RowsAffected() == 0 {
		apierr.WriteStatus(w, http.StatusNotFound, "no account for the authenticated user")
		return
	}
	writeJSON(w, http.StatusOK, LanguagePreference{Language: &language})
}
