package api

import (
	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/sharedchat"
)

// SharedChatViewPath is the anonymous read of a shared conversation.
const SharedChatViewPath = "/api/v2/elitea_core/shared_chat_view/prompt_lib/{token}"

// SharedChatUnlockPath exchanges a link password for a grant cookie.
const SharedChatUnlockPath = "/api/v2/elitea_core/shared_chat_view_unlock/prompt_lib/{token}/unlock"

// mountSharedChatAnonymousRoutes registers the two routes that serve
// conversation content to callers with no session.
//
// SEPARATE FILE, and separate from the owner-facing registrations inside
// /elitea_core, on purpose. Those run behind Auth, project membership and a
// legacy permission; these run behind none of the three. Keeping the two
// registrations visually adjacent is exactly how one of them acquires — or
// loses — a middleware nobody meant to change. The doc comments on
// sharedchat.Handler.View and .Unlock carry the threat model these paths are
// built against.
//
// Gated on the link store, not on the transcript reader: with a store and no
// reader the routes still register and answer 500 for a VALID token, while an
// invalid, revoked or expired one is still refused as it should be. A gate on
// both would make a half-wired deployment answer 404 everywhere, which is the
// "absence reads as correctness" failure this codebase has produced repeatedly
// — an unregistered route's 404 is indistinguishable from a token that does
// not exist.
func mountSharedChatAnonymousRoutes(r chi.Router, cfg RouterConfig) {
	if cfg.SharedChatStore == nil {
		return
	}
	h := sharedchat.NewHandler(cfg.SharedChatStore, cfg.SharedChatTranscript, []byte(cfg.SessionSecret))
	// The unlock route's per-client budget is keyed on the same trusted-proxy
	// interpretation the SCIM and native token endpoints use. The field is
	// checked for nil so an unset pointer never becomes a non-nil interface.
	if cfg.SCIMClientAddresses != nil {
		h.WithClientAddresses(cfg.SCIMClientAddresses)
	}
	r.Get(SharedChatViewPath, h.View)
	r.Post(SharedChatUnlockPath, h.Unlock)
}
