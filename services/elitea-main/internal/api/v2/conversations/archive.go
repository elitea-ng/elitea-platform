package conversations

import "net/http"

// Archive serves archiveConversation (client contract 1.10). The contract
// lands first; the per-user archive store follows in the next change.
func (h *Handler) Archive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "error": "not_implemented"})
}

// Unarchive serves unarchiveConversation (client contract 1.10).
func (h *Handler) Unarchive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "error": "not_implemented"})
}
