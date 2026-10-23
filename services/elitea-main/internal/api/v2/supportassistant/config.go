package supportassistant

// `GET /api/v2/support_assistant/config` — the widget's first call on every page
// load, and the only ungated route in the package.
//
// It is the port of `api/v2/config.py`'s `get`, with ONE deliberate behaviour
// change. The reference returns a hardcoded `'enabled': True` and relies on its
// `@add_support_project_id` decorator to 503 first when the plugin is off, so the
// field never actually reports the switch — a client that reached the body could
// only ever read `true`. Here the field IS the switch, resolved from
// `SupportAssistant.Ready()`, because this route answers on a disabled
// deployment too and "enabled" has to mean something when it does.
//
// `Ready()` and not `Enabled` on purpose: a deployment whose operator flipped the
// switch but never chose an agent cannot answer a question, and reporting it as
// enabled would render a widget whose first message fails. The widget's contract
// is "render me only if I can work".

import (
	"context"
	"net/http"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// ConfigResponse is the widget's `TAssistantConfig`
// (`elitea_assistant/src/lib/types/assistant.types.ts`).
type ConfigResponse struct {
	Enabled bool `json:"enabled"`
	// Title, WelcomeMessage and Placeholder are the operator's strings from the
	// Features page, already defaulted — the client never has to know what the
	// platform's default greeting is.
	Title          string `json:"title"`
	WelcomeMessage string `json:"welcome_message"`
	Placeholder    string `json:"placeholder"`
	// SupportProjectID is echoed so the widget can subscribe to the execution
	// event stream, whose URL is project-scoped. It is NOT an input anywhere:
	// every route resolves the project itself and ignores what a client sends.
	SupportProjectID int64 `json:"support_project_id"`
	// User is the CALLER's own identity, for the avatar beside their messages.
	User ConfigUser `json:"user"`
}

// ConfigUser is the caller's identity as the widget renders it.
type ConfigUser struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Avatar string `json:"avatar"`
}

// disabledConfig is the answer for every deployment that cannot serve the
// assistant, whatever the reason.
//
// It carries the switch and NOTHING ELSE — no project id, no operator strings,
// no identity. A disabled feature should not be a channel that tells an
// unauthenticated-adjacent caller which project the platform reserved for
// support or what the deployment renamed the assistant to.
var disabledConfig = ConfigResponse{Enabled: false, User: ConfigUser{}}

// Config serves the widget's configuration.
func (h *Handler) Config(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		// 200 with `enabled:false`, not 401.
		//
		// The widget asks this question from the app shell, which mounts before
		// the session is necessarily established, and a 401 here would be
		// indistinguishable from the peripheral-401 logout loop this app has
		// already been bitten by once. There is nothing to leak: the disabled
		// body is one `false`.
		writeJSON(w, http.StatusOK, disabledConfig)
		return
	}

	settings, err := h.store.settings(r.Context())
	if err != nil {
		h.logger.Error("support assistant: config read", "err", err)
		writeJSON(w, http.StatusOK, disabledConfig)
		return
	}
	if !settings.Ready() {
		writeJSON(w, http.StatusOK, disabledConfig)
		return
	}

	userID, ok := user.OwningUserID()
	if !ok {
		writeJSON(w, http.StatusOK, disabledConfig)
		return
	}

	writeJSON(w, http.StatusOK, ConfigResponse{
		Enabled:          true,
		Title:            settings.Name,
		WelcomeMessage:   settings.WelcomeMessage,
		Placeholder:      settings.Placeholder,
		SupportProjectID: settings.ProjectID,
		User: ConfigUser{
			ID:     userID,
			Name:   user.Name,
			Avatar: h.store.avatar(r.Context(), userID),
		},
	})
}

// avatar reads the caller's avatar URL, replacing the reference's
// `social_get_user` RPC (which it already wrapped in a 2-second timeout and a
// swallowed exception).
//
// A missing row, a NULL and a failed query are all the empty string: an avatar
// is decoration, and no failure to find one should cost the caller their support
// widget. The reference makes the same judgement.
func (s *store) avatar(ctx context.Context, userID int64) string {
	if s.pool == nil {
		return ""
	}
	var avatar *string
	if err := s.pool.QueryRow(ctx,
		`SELECT avatar FROM centry.social_users WHERE user_id = $1`, userID).Scan(&avatar); err != nil {
		return ""
	}
	if avatar == nil {
		return ""
	}
	return *avatar
}
