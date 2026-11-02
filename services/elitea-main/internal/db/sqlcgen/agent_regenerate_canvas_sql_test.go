package sqlcgen

import (
	"strings"
	"testing"
)

// TestRegenerationAdmitsAndKeepsCanvasItems pins #1098 in the statements
// themselves. A regeneration of an answer that holds a canvas is admitted,
// and the reset deletes the answer's text but keeps the canvas (the user's
// edits live in it). The PostgreSQL test
// TestPostgresRegeneratingACanvasAnswerKeepsTheEditedCanvas runs the same
// rule against a real database; this one fails without a database.
func TestRegenerationAdmitsAndKeepsCanvasItems(t *testing.T) {
	const responseGate = "unsupported_response_item.item_type NOT IN ('text_message', 'canvas_message')"
	for name, statement := range map[string]string{
		"ResolveCurrentRegeneration": resolveCurrentRegeneration,
		"ResetCurrentAgentResponse":  resetCurrentAgentResponse,
	} {
		normalized := strings.Join(strings.Fields(statement), " ")
		if !strings.Contains(normalized, responseGate) {
			t.Errorf("%s does not admit a canvas on the answer (want %q)", name, responseGate)
		}
		if strings.Contains(normalized, "unsupported_response_item.item_type <> 'text_message'") {
			t.Errorf("%s still refuses every non-text item on the answer", name)
		}
	}

	normalized := strings.Join(strings.Fields(resetCurrentAgentResponse), " ")
	for _, fragment := range []string{
		// The text goes and the canvas stays.
		"WHERE item.message_group_id = resolved.id AND item.item_type <> 'canvas_message' RETURNING item.id",
		// The kept canvases come first, so the new text follows them.
		"kept_canvases AS (",
		"WHERE canvas_item.item_type = 'canvas_message'",
		"AND (SELECT count(*) FROM kept_canvases) >= 0",
	} {
		if !strings.Contains(normalized, fragment) {
			t.Errorf("ResetCurrentAgentResponse is missing %q", fragment)
		}
	}
	// Nothing in the reset may touch a canvas's payload or its versions.
	for _, forbidden := range []string{
		"DELETE FROM chat_canvas_versions", "UPDATE chat_canvas_versions",
		"DELETE FROM chat_messages_canvas", "UPDATE chat_messages_canvas",
	} {
		if strings.Contains(normalized, forbidden) {
			t.Errorf("ResetCurrentAgentResponse touches %s", forbidden)
		}
	}
}
