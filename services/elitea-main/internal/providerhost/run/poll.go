// Package run reads one provider invocation as the SPI reports it, and lets a
// facade observe the terminal payload passing through its hop.
//
// # WHY THIS IS NOT IN ONE FACADE
//
// The envelope below belongs to NO provider. conformance/provider/spi/contract.json
// says so in as many words — a status, read-once `custom_events`, and on the
// terminal poll a `result` — and every facade proxies the same
// `/tools/{toolkit}/{tool}/invocations/{id}`. DeepWiki and Inventory both poll
// it today.
//
// The browser already made this exact extraction and recorded why:
// `apps/elitea-web/src/entities/provider-run/model/poll.ts` (ADR-0023 decision
// 4) — "Both DeepWiki features used to spell this envelope out for
// themselves; this is the one spelling." This package is that decision's Go
// twin, and it is deliberately the same shape so the two cannot drift: what a
// transcript records and what the user was shown must agree, and they only do
// while the status vocabulary, the fallback order and the result shapes are
// read the same way on both sides.
//
// WHAT IS NOT HERE is anything a particular provider knows: which tool means
// which agent, which parameter carries a question, which header a client
// sends. Those stay with the facade — see internal/api/v2/deepwiki.
package run

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Terminal is a finished invocation, as its last poll reported it.
type Terminal struct {
	InvocationID string
	// Content is the answer, or the failure's message. Never empty for a
	// Terminal a caller is handed.
	Content string
	// Failed marks `Error` and `Stopped`. It is carried rather than dropped:
	// a run that ended badly is still a thing that happened, and a consumer
	// that saw nothing cannot tell it from one that never finished.
	Failed bool
}

// poll is the wire envelope. `custom_events` is deliberately absent: it is
// read-once — progress, and since issue #701 the answer's own fragments —
// and a facade observing a response it is also forwarding has no business
// appearing to consume it. The tee below records the answer from the
// TERMINAL body for the same reason: the fragments belong to the browser
// that is draining them, and a transcript built from them would be a second
// reading of the same answer.
type poll struct {
	InvocationID string `json:"invocation_id"`
	Status       string `json:"status"`
	Result       string `json:"result"`
	Message      string `json:"message"`
}

// TerminalOf reports whether one poll body ended the invocation, and what it
// said.
//
// The status vocabulary and the fallbacks are the browser's `terminalOutcome`.
// `Stopped` is in the list although the generated client's enum lacks it: the
// facade answers it after a cancel, and every consumer branches on it.
//
// A status this build has never heard of is NOT terminal. Reading an unknown
// status as an ending would settle a run that is still going.
func TerminalOf(body []byte) (Terminal, bool) {
	var payload poll
	if err := json.Unmarshal(body, &payload); err != nil {
		return Terminal{}, false
	}
	if payload.InvocationID == "" || payload.Status == "" {
		return Terminal{}, false
	}
	switch payload.Status {
	case "Completed":
		content := AnswerText(payload.Result)
		if content == "" {
			return Terminal{}, false
		}
		return Terminal{InvocationID: payload.InvocationID, Content: content}, true
	case "Error", "Stopped":
		return Terminal{
			InvocationID: payload.InvocationID,
			Content:      firstNonEmpty(payload.Result, payload.Message, "The request failed."),
			Failed:       true,
		}, true
	default:
		return Terminal{}, false
	}
}

// InvocationIDOf reads `invocation_id` out of an accepted invoke. An
// acceptance without one is a run nothing can follow, and the browser refuses
// it too (entities/provider-run/model/invocationId.ts).
func InvocationIDOf(body []byte) string {
	var payload poll
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return payload.InvocationID
}

// AnswerText reads the answer out of a `Completed` result.
//
// A transcription of the browser's `readAnswer`
// (features/wiki-chat/model/frames/terminalFrames.ts), covering the same
// shapes: the platform's result array, a JSON envelope naming one of four
// keys, and a bare string. Where the two disagree the browser wins, because it
// is what the user saw — so the fallbacks are the same and in the same order,
// including the "raw string, key order and all" last resort.
func AnswerText(result string) string {
	trimmed := strings.TrimSpace(result)
	if trimmed == "" {
		return ""
	}
	if !strings.HasPrefix(trimmed, "[") && !strings.HasPrefix(trimmed, "{") {
		return trimmed
	}
	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		// A brace-first sentence must not cost the user their answer.
		return trimmed
	}
	switch value := parsed.(type) {
	case []any:
		return resultArrayText(value)
	case map[string]any:
		if text := firstTruthyText(value, "answer", "result", "message", "data"); text != "" {
			return text
		}
		return trimmed
	default:
		return trimmed
	}
}

// resultArrayText joins every `message` object's data, in order — the shape
// the platform's runners answer with, where an answer and its "Sources:" list
// are two separate entries. Keeping only the first dropped the sources
// (DWIKI-012 caught it on the real provider).
func resultArrayText(entries []any) string {
	var parts []string
	for _, entry := range entries {
		object, ok := entry.(map[string]any)
		if !ok || primitiveText(object["object_type"]) != "message" {
			continue
		}
		if text := primitiveText(object["data"]); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

// firstTruthyText is the browser's `firstTruthy` chain: `||` and not `??`, so
// an empty `answer` falls through to `result` rather than stopping there.
func firstTruthyText(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if text := primitiveText(object[key]); text != "" {
			return text
		}
	}
	return ""
}

// primitiveText renders a JSON scalar the way the browser's `primitiveText`
// does, and refuses a composite: an object rendered as its Go formatting is
// not an answer.
func primitiveText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
