package conversations

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// MaxCanvasContentBytes is the largest canvas text a create or an edit may
// store: 64 KiB, measured in bytes of UTF-8.
//
// WHY A CAP AT ALL. A canvas is no longer only something the user looks at:
// the turn resolvers project its newest version into the chat history the
// model is shown (agent_chat.sql, canvas_budget), and the Python worker
// refuses an input bundle above 256 KiB (elitea-worker-python config.py,
// content_max_body_bytes). Uncapped, one pasted log or generated file made
// every later turn of its conversation fail — unrecoverably, because history
// only grows.
//
// WHY 64 KiB. The history carries the newest canvases whose combined size fits
// a budget of the same 64 KiB (the literal 65536 in agent_chat.sql), so any
// canvas that can be written can also be shown in full while it is the newest
// one. Together with the attachment bound (four newest x 32 KiB = 128 KiB)
// that is 192 KiB worst case, leaving a quarter of the worker's ceiling for
// the conversation's own words and the JSON framing. A larger write cap would
// let a single canvas exceed the history budget on its own and be withheld
// from the model on every turn, which reads to the user as the model ignoring
// the document they just saved.
const MaxCanvasContentBytes = 64 << 10

// maxCanvasRequestBytes bounds the JSON body of a canvas write before it is
// decoded. JSON escaping can grow text (a newline is two bytes, a control
// character six), so the envelope is several times the content cap; anything
// past it cannot hold an acceptable canvas and is refused unread.
const maxCanvasRequestBytes = 8 * MaxCanvasContentBytes

// CanvasTooLargeError refuses canvas text above MaxCanvasContentBytes. The
// repository returns it for both the create (whose text is a slice of a stored
// message) and the edit, and the handler answers it as a 413 whose
// safe_message the editor shows beside its save — the edit stays in the
// editor; nothing is silently dropped.
type CanvasTooLargeError struct {
	// Size is the refused text's size in bytes, or 0 when the request body
	// was refused before it was read (its size is then only known to exceed
	// maxCanvasRequestBytes).
	Size  int
	Limit int
}

func (e *CanvasTooLargeError) Error() string {
	return fmt.Sprintf("canvas content is %d bytes; the limit is %d bytes", e.Size, e.Limit)
}

// SafeMessage is the user-facing sentence: what happened and what to do.
func (e *CanvasTooLargeError) SafeMessage() string {
	if e.Size <= 0 {
		return fmt.Sprintf(
			"This document is too large to save (the limit is %s). Shorten it, or save it to artifacts instead.",
			humanBytes(e.Limit),
		)
	}
	return fmt.Sprintf(
		"This document is too large to save (%s; the limit is %s). Shorten it, or save it to artifacts instead.",
		humanBytes(e.Size), humanBytes(e.Limit),
	)
}

// CheckCanvasContent answers a CanvasTooLargeError for text above the cap.
func CheckCanvasContent(content string) error {
	if len(content) > MaxCanvasContentBytes {
		return &CanvasTooLargeError{Size: len(content), Limit: MaxCanvasContentBytes}
	}
	return nil
}

func humanBytes(n int) string {
	if n >= 1<<10 {
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// writeCanvasTooLarge answers 413 with the shape every other refusal here has
// (`error`) plus `code` and `safe_message`, the pair the web client reads for
// a message it may show verbatim.
func writeCanvasTooLarge(w http.ResponseWriter, tooLarge *CanvasTooLargeError) {
	writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
		"error":        tooLarge.SafeMessage(),
		"code":         "canvas_too_large",
		"safe_message": tooLarge.SafeMessage(),
		"size_bytes":   tooLarge.Size,
		"limit_bytes":  tooLarge.Limit,
	})
}

// decodeCanvasBody decodes a canvas write's JSON body under
// maxCanvasRequestBytes. It answers the refusal itself and reports false when
// the body was too large or malformed.
func decodeCanvasBody(w http.ResponseWriter, r *http.Request, body *map[string]any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxCanvasRequestBytes)
	err := json.NewDecoder(r.Body).Decode(body)
	if err == nil {
		return true
	}
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeCanvasTooLarge(w, &CanvasTooLargeError{Limit: MaxCanvasContentBytes})
		return false
	}
	apierr.Write(w, apierr.BadRequest("invalid request body"))
	return false
}
