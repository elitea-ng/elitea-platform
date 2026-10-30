package llmproxy

// guardrail_toolargs.go — the output stage on tool-call arguments (ADR-0043
// decision 1, issue #1500).
//
// A model can put content in a tool call as well as in text: Chat
// tool_calls[].function.arguments, a Responses function_call (arguments) or
// custom_tool_call (input), and an Anthropic tool_use block (the Messages
// route converts it to a Responses function_call). The output stage inspects
// all of them. A block refuses the answer. A flag records a verdict.
//
// # Masking keeps the arguments valid JSON
//
// Function arguments are a JSON document. The stage decodes every string
// VALUE of the document (object keys stay as they are), runs the detectors on
// the decoded values together with the text of the answer, and writes each
// masked value back as a JSON string. So a mask never breaks the JSON, and a
// secret written with JSON escapes (a PEM block with \n) is found in its
// decoded form. Arguments that are not valid JSON (a custom tool's free text,
// or a truncated document) are inspected and masked as plain text.
//
// # Streamed arguments are held until the call completes
//
// A JSON value cannot be masked while it is incomplete. So, when a block or
// mask detector applies, the stream holds the argument deltas of each call
// and sends no argument byte until the call completes:
//
//   - Chat: the finish chunk of the choice (or the end of the stream) carries
//     the whole masked arguments of each call of that choice. The deltas
//     before it carry the call id and name with empty arguments.
//   - Responses and Messages: the argument deltas are dropped. Before the
//     function_call_arguments.done (custom_tool_call_input.done) event, one
//     delta event carries the whole masked arguments; the done event, the
//     output_item.done item and the response.completed output carry the same
//     masked value.
//
// A flag-only pipeline holds nothing: it reads a copy of the arguments when
// the call completes.

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/guardrail"
)

// guardTargets is what one stage inspects: plain texts, and tool-call
// arguments (JSON documents, or plain text when they are not valid JSON).
type guardTargets struct {
	texts []*string
	args  []*string
}

// jsonLit is one string value of a JSON document: its byte range in the
// document and its decoded value.
type jsonLit struct {
	start, end int
	value      string
}

type argPlan struct {
	target *string
	lits   []jsonLit
	whole  bool
	first  int
}

// checkTargets runs pipe once over every text and every argument string value
// (one detector run, so an http adapter costs one round trip), and writes the
// masked values back. It returns the refusing verdict, or nil.
func checkTargets(ctx context.Context, pipe *guardrail.Pipeline, t guardTargets) *guardrail.Verdict {
	parts := make([]string, 0, len(t.texts)+len(t.args))
	for _, p := range t.texts {
		parts = append(parts, *p)
	}
	plans := make([]argPlan, 0, len(t.args))
	for _, a := range t.args {
		plan := argPlan{target: a, first: len(parts)}
		if lits, ok := jsonStringValues(*a); ok {
			plan.lits = lits
			for _, l := range lits {
				parts = append(parts, l.value)
			}
		} else {
			plan.whole = true
			parts = append(parts, *a)
		}
		plans = append(plans, plan)
	}
	if len(parts) == 0 {
		return nil
	}
	masked, blocked := pipe.CheckAll(ctx, parts)
	if blocked != nil {
		return blocked
	}
	for i, p := range t.texts {
		*p = masked[i]
	}
	for _, plan := range plans {
		if plan.whole {
			*plan.target = masked[plan.first]
			continue
		}
		*plan.target = replaceJSONValues(*plan.target, plan.lits, masked[plan.first:plan.first+len(plan.lits)])
	}
	return nil
}

// jsonStringValues returns the string values (not the object keys) of a
// valid JSON document, in document order. ok is false when raw is not valid
// JSON.
func jsonStringValues(raw string) ([]jsonLit, bool) {
	if !json.Valid([]byte(raw)) {
		return nil, false
	}
	var out []jsonLit
	for i := 0; i < len(raw); i++ {
		if raw[i] != '"' {
			continue
		}
		// In a valid document a quote outside a string opens a string.
		j := i + 1
		for j < len(raw) && raw[j] != '"' {
			if raw[j] == '\\' {
				j++
			}
			j++
		}
		k := j + 1
		for k < len(raw) && (raw[k] == ' ' || raw[k] == '\t' || raw[k] == '\n' || raw[k] == '\r') {
			k++
		}
		if k >= len(raw) || raw[k] != ':' {
			var value string
			if err := json.Unmarshal([]byte(raw[i:j+1]), &value); err != nil {
				return nil, false
			}
			out = append(out, jsonLit{start: i, end: j + 1, value: value})
		}
		i = j
	}
	return out, true
}

// replaceJSONValues writes each changed value back into raw as a JSON string.
// Everything else (keys, order, numbers, spacing) stays as it was.
func replaceJSONValues(raw string, lits []jsonLit, values []string) string {
	var b strings.Builder
	pos := 0
	for i, l := range lits {
		if values[i] == l.value {
			continue
		}
		b.WriteString(raw[pos:l.start])
		b.WriteString(jsonString(values[i]))
		pos = l.end
	}
	if pos == 0 {
		return raw
	}
	b.WriteString(raw[pos:])
	return b.String()
}

// jsonString encodes s as a JSON string without HTML escaping.
func jsonString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(buf.String(), "\n")
}

// ---------------------------------------------------------------------------
// Streamed arguments

// argsBuffer holds the arguments of one streamed tool call.
type argsBuffer struct {
	// json is false for a custom tool's free-text input.
	json bool
	buf  strings.Builder
	// held is set when the stream kept an argument byte from the client:
	// the masked arguments must then be sent when the call completes.
	held bool
	// resolved is the checked (masked) arguments, once the call completed.
	resolved *string
	chat     *chatCall
	resp     *respCall
}

type chatCall struct {
	choice int
	index  uint16
}

type respCall struct {
	outputIndex *int
	itemID      *string
}

func (g *guardedStream) argsBuf(key string, isJSON bool) *argsBuffer {
	b, ok := g.args[key]
	if !ok {
		b = &argsBuffer{json: isJSON}
		g.args[key] = b
		g.argsOrder = append(g.argsOrder, key)
	}
	return b
}

// resolveArgs checks the arguments of a completed call. full is the whole
// value from a done event; nil uses the held deltas.
func (g *guardedStream) resolveArgs(ctx context.Context, b *argsBuffer, full *string) (string, *guardrail.Verdict) {
	if b.resolved != nil {
		return *b.resolved, nil
	}
	text := b.buf.String()
	if full != nil {
		text = *full
	}
	t := guardTargets{texts: []*string{&text}}
	if b.json {
		t = guardTargets{args: []*string{&text}}
	}
	if blocked := checkTargets(ctx, g.pipe, t); blocked != nil {
		return "", blocked
	}
	b.resolved = &text
	return text, nil
}

func chatToolKey(choice int, index uint16) string {
	return "chat:" + strconv.Itoa(choice) + ":tool:" + strconv.Itoa(int(index))
}

// chatToolDeltas takes the argument deltas of a chat chunk. When the stream
// holds, it empties them and drops an entry that carried only arguments.
func (g *guardedStream) chatToolDeltas(choice int, calls []schemas.ChatAssistantMessageToolCall) []schemas.ChatAssistantMessageToolCall {
	holds := g.pipe.Holds()
	kept := calls[:0]
	for _, tc := range calls {
		b := g.argsBuf(chatToolKey(choice, tc.Index), true)
		b.chat = &chatCall{choice: choice, index: tc.Index}
		b.buf.WriteString(tc.Function.Arguments)
		if holds {
			if tc.Function.Arguments != "" {
				b.held = true
			}
			tc.Function.Arguments = ""
			if tc.ID == nil && tc.Type == nil && tc.Function.Name == nil && len(tc.ExtraContent) == 0 && tc.CacheControl == nil {
				continue
			}
		}
		kept = append(kept, tc)
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// finishChatTools completes the calls of one choice. It returns the deltas
// that carry the masked arguments the stream held, or the refusing verdict.
func (g *guardedStream) finishChatTools(ctx context.Context, choice int) ([]schemas.ChatAssistantMessageToolCall, *guardrail.Verdict) {
	var out []schemas.ChatAssistantMessageToolCall
	for _, key := range g.argsOrder {
		b := g.args[key]
		if b.chat == nil || b.chat.choice != choice || b.resolved != nil {
			continue
		}
		masked, blocked := g.resolveArgs(ctx, b, nil)
		if blocked != nil {
			return nil, blocked
		}
		if b.held && masked != "" {
			out = append(out, schemas.ChatAssistantMessageToolCall{
				Index:    b.chat.index,
				Function: schemas.ChatAssistantMessageToolCallFunction{Arguments: masked},
			})
		}
	}
	return out, nil
}

func responsesArgsKey(outputIndex *int) string {
	o := 0
	if outputIndex != nil {
		o = *outputIndex
	}
	return "args:" + strconv.Itoa(o)
}

func (b *argsBuffer) at(outputIndex *int, itemID *string) {
	if b.resp == nil {
		b.resp = &respCall{outputIndex: outputIndex, itemID: itemID}
	}
	if b.resp.itemID == nil {
		b.resp.itemID = itemID
	}
}

// responsesItemAdded takes the arguments an output_item.added item already
// carries (most providers send them empty).
func (g *guardedStream) responsesItemAdded(sr *schemas.BifrostResponsesStreamResponse) {
	if sr.Item == nil || sr.Item.ResponsesToolMessage == nil {
		return
	}
	tm := sr.Item.ResponsesToolMessage
	holds := g.pipe.Holds()
	key := responsesArgsKey(sr.OutputIndex)
	if tm.Arguments != nil {
		b := g.argsBuf(key, true)
		b.at(sr.OutputIndex, sr.Item.ID)
		if *tm.Arguments != "" {
			b.buf.WriteString(*tm.Arguments)
			if holds {
				b.held = true
				empty := ""
				tm.Arguments = &empty
			}
		}
	}
	if tm.ResponsesCustomToolCall != nil {
		b := g.argsBuf(key, false)
		b.at(sr.OutputIndex, sr.Item.ID)
		if tm.ResponsesCustomToolCall.Input != "" {
			b.buf.WriteString(tm.ResponsesCustomToolCall.Input)
			if holds {
				b.held = true
				tm.ResponsesCustomToolCall.Input = ""
			}
		}
	}
}

// responsesArgsDelta takes one argument (or custom input) delta. It reports
// whether the stream holds it, so the event must not be sent.
func (g *guardedStream) responsesArgsDelta(sr *schemas.BifrostResponsesStreamResponse) bool {
	b := g.argsBuf(responsesArgsKey(sr.OutputIndex), sr.Type == schemas.ResponsesStreamResponseTypeFunctionCallArgumentsDelta)
	b.at(sr.OutputIndex, sr.ItemID)
	switch {
	case sr.Delta != nil:
		b.buf.WriteString(*sr.Delta)
	case sr.Arguments != nil:
		b.buf.WriteString(*sr.Arguments)
	}
	if !g.pipe.Holds() {
		return false
	}
	b.held = true
	return true
}

// resolveResponsesArgs completes the call at outputIndex. field is the full
// value of a done event or item (nil, or a nil value, uses the held deltas);
// it receives the masked value. It returns one delta event with the masked
// arguments when the stream held deltas of the call, or the refusing verdict.
func (g *guardedStream) resolveResponsesArgs(ctx context.Context, outputIndex *int, itemID *string, isJSON bool, field **string) ([]*schemas.BifrostResponsesStreamResponse, *guardrail.Verdict) {
	b := g.argsBuf(responsesArgsKey(outputIndex), isJSON)
	b.at(outputIndex, itemID)
	sendDelta := b.resolved == nil && b.held
	var full *string
	if field != nil && *field != nil {
		full = *field
	}
	masked, blocked := g.resolveArgs(ctx, b, full)
	if blocked != nil {
		return nil, blocked
	}
	if field != nil && *field != nil {
		value := masked
		*field = &value
	}
	if !sendDelta || masked == "" {
		return nil, nil
	}
	typ := schemas.ResponsesStreamResponseTypeFunctionCallArgumentsDelta
	if !b.json {
		typ = schemas.ResponsesStreamResponseTypeCustomToolCallInputDelta
	}
	delta := masked
	return []*schemas.BifrostResponsesStreamResponse{{
		Type: typ, OutputIndex: b.resp.outputIndex, ItemID: b.resp.itemID, Delta: &delta,
	}}, nil
}

// responsesItemArgs puts the masked arguments into a done item (an
// output_item.done item, or an item of response.completed).
func (g *guardedStream) responsesItemArgs(ctx context.Context, outputIndex *int, item *schemas.ResponsesMessage) ([]*schemas.BifrostResponsesStreamResponse, *guardrail.Verdict) {
	tm := item.ResponsesToolMessage
	if tm == nil {
		return nil, nil
	}
	var before []*schemas.BifrostResponsesStreamResponse
	if tm.Arguments != nil {
		ev, blocked := g.resolveResponsesArgs(ctx, outputIndex, item.ID, true, &tm.Arguments)
		if blocked != nil {
			return nil, blocked
		}
		before = append(before, ev...)
	}
	if tm.ResponsesCustomToolCall != nil {
		input := &tm.ResponsesCustomToolCall.Input
		ev, blocked := g.resolveResponsesArgs(ctx, outputIndex, item.ID, false, &input)
		if blocked != nil {
			return nil, blocked
		}
		tm.ResponsesCustomToolCall.Input = *input
		before = append(before, ev...)
	}
	return before, nil
}
