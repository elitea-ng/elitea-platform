package deepwiki

// Server-side history for the wiki chat drawer.
//
// Until this feature the drawer's conversation lived in the BROWSER — a
// namespaced localStorage key per (project, toolkit),
// apps/elitea-web/src/widgets/deepwiki/lib/chatStorage.ts. That is history in
// the sense that a sticky note is a filing cabinet: it is gone on another
// device, in another browser, and on a cleared profile, and nobody but that
// one browser can ever see it.
//
// A wiki chat is now an ORDINARY TENANT CONVERSATION — `p_{id}.chat_conversations`
// with `source = 'deepwiki'` and a participant of `entity_name = 'toolkit'`.
// No migration: migrations/tenant/0123 already declares `source` on the
// conversation, `entity_name` on the participant and `task_id` on the message
// group, which are the three columns this feature needs.
//
// # BOTH TURNS ARE WRITTEN HERE, NEVER BY THE BROWSER
//
// A client-authorable assistant turn is forgery — it would let any caller put
// words in the model's mouth in a transcript that is then read back as a
// record of what the model said. There is also no message-create route to do
// it with. So:
//
//   - The QUESTION is written from the invoke, through material.Observer:
//     after the hop, from the body the CLIENT sent (never the rewritten one,
//     which carries credentials) and the invocation id the provider answered
//     with.
//   - The ANSWER is written from the terminal poll, through providerhost/run's
//     tee. The browser drains its answer THROUGH this facade — the invocation
//     GET is a proxy route — so the facade does see the terminal payload, and
//     that is the only moment it does.
//
// `chat_message_group.task_id` carries the invocation id on the question, and
// the answer is inserted as the reply to that group. That is what makes the
// tee idempotent: a poll loop that reaches the terminal payload twice — and
// it does, because nothing stops the browser polling once more after the
// answer arrives — finds the reply already there and writes nothing.
//
// # WHAT IS IN THIS FILE, AND WHAT IS NOT
//
// Only what NAMES DEEPWIKI. Reading the SPI's poll envelope, deciding whether
// a poll was terminal, turning a `result` into text, and teeing a hop are
// provider-neutral and live in internal/providerhost/run — the frozen SPI
// contract "belongs to no provider" in as many words, and the browser made
// the same extraction for the same reason (ADR-0023 decision 4).
//
// What could not move is here: the two headers this drawer sends, the map
// from a tool name to which agent ran, the parameter DeepWiki's tools carry a
// question in, and the wiring to a store only this feature has.
//
// # THE ACCEPTED GAP (v1)
//
// If the tab closes between the invoke and the terminal poll, the QUESTION is
// stored and the ANSWER is not. The invocation still runs and still finishes;
// nothing re-polls it, because a poll only happens when a browser asks. The
// conversation then shows a question with no reply.
//
// This is accepted rather than half-built. Reconciling would mean elitea-main
// polling the provider on its own — a background worker holding a client the
// facade does not have (it owns a ReverseProxy, not an API client), a
// schedule, and a rule for when to give up. That is a feature, not a detail
// of this one. What this file does instead is make the gap VISIBLE: every
// recorded question logs at info with its invocation id, so an unanswered
// turn can be traced to the invocation that was never drained.
//
// # THE STORE SEAM
//
// The store interface is `internal/domain/wikichat.Store`, declared in its own
// package rather than here. The support assistant's ChatStore could sit in its
// handler package because nothing under internal/infra/db/repos imports that
// handler; this facade is not in that position — its credential resolver
// reaches providerhost/material, which imports repos — so a repository naming
// this package's types would close the loop. See that package's doc comment.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/wikichat"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/material"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/run"
)

// The two request headers the drawer sends with an invoke.
//
// HEADERS AND NOT BODY FIELDS. The body is an SPI envelope that travels to
// the provider verbatim apart from the rewrite; a key put there for
// elitea-main's own bookkeeping would be forwarded to a service that has no
// idea what it means, and stripping it would mean teaching the shared
// envelope about one facade's feature. Both headers are read and DELETED
// before the hop.
const (
	// ChatKeyHeader carries the conversation this question belongs to. It is
	// a client-chosen opaque key, and choosing it is not a privilege: every
	// statement that resolves it also requires `author_id = the caller`, so
	// the worst a caller can do with somebody else's key is fail to match it
	// and open a conversation of their own.
	ChatKeyHeader = "X-Elitea-Wiki-Chat"
	// ToolkitHeader carries the toolkit row the drawer is open on, which is
	// what the conversation is filed under and what the drawer lists by. The
	// invoke body cannot supply it: `Wikis` names a code toolkit,
	// `wikis_query` names a Wikis toolkit and `wiki_query` names nothing at
	// all (wikis.go), so there is no one field that means "the wiki I am
	// looking at".
	ToolkitHeader = "X-Elitea-Wiki-Toolkit"
)

// maxChatKeyLength bounds the opaque key. A UUID is 36 characters; the bound
// exists so a caller cannot make the store compare megabyte strings.
const maxChatKeyLength = 64

// History records wiki conversations. A nil History records nothing and
// serves everything, which is what a deployment with no database pool gets.
type History struct {
	store  wikichat.Store
	logger *slog.Logger
}

// NewHistory builds the recorder. A nil store yields a nil History rather
// than one that fails on every turn: the facade must serve a wiki chat that
// is not being recorded, exactly as it did before this feature existed.
func NewHistory(store wikichat.Store, logger *slog.Logger) *History {
	if store == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &History{store: store, logger: logger}
}

// chatContextKey carries the two headers from the route wrapper to the
// observer, which runs inside material.Invocation.Serve and never sees the
// request.
type chatContextKey struct{}

type chatContext struct {
	key       string
	toolkitID int64
}

// WrapInvoke reads the drawer's two headers, strips them, and remembers them
// for the observer. It is a no-op wrapper on a nil History.
func (h *History) WrapInvoke(next http.HandlerFunc) http.HandlerFunc {
	if h == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		chat := chatContext{key: strings.TrimSpace(r.Header.Get(ChatKeyHeader))}
		if id, err := strconv.ParseInt(strings.TrimSpace(r.Header.Get(ToolkitHeader)), 10, 64); err == nil {
			chat.toolkitID = id
		}
		// Deleted whether or not they were readable: a header this platform
		// gives a meaning to must not also reach the provider, where it means
		// something else or nothing.
		r.Header.Del(ChatKeyHeader)
		r.Header.Del(ToolkitHeader)
		if chat.key == "" {
			// NOT a warning, and this is the case that matters: every wiki
			// GENERATION is an invoke on this same route, and a generation
			// has no conversation. Logging here would put a line per
			// generation in the log for a request that is behaving correctly.
			next(w, r)
			return
		}
		if len(chat.key) > maxChatKeyLength || chat.toolkitID <= 0 {
			// A key with no usable toolkit IS worth a line: the drawer meant
			// to record this turn and something about the request stopped it.
			// The question is still asked and still answered — it is simply
			// not filed, because filing it under a made-up toolkit would put
			// it in a drawer that never lists it.
			h.logger.Warn("deepwiki: wiki chat turn is not recorded",
				"reason", "the request named a conversation key but no usable toolkit")
			next(w, r)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), chatContextKey{}, chat)))
	}
}

// Observer is the invoke hook, or nil when nothing is being recorded.
//
// A METHOD VALUE WOULD NOT DO. `h.observe` on a nil *History is a non-nil
// func value, so handing it straight to material.Invocation would switch on
// body capture for a facade that records nothing — the shape of "absence
// reads as presence" this codebase has been bitten by before. Returning a
// typed nil is what makes the caller's `!= nil` mean what it says.
func (h *History) Observer() material.Observer {
	if h == nil {
		return nil
	}
	return h.observe
}

// observe writes the question of one served invoke.
func (h *History) observe(ctx context.Context, served material.Served) {
	chat, ok := ctx.Value(chatContextKey{}).(chatContext)
	if !ok || chat.key == "" {
		return
	}
	if served.Status < 200 || served.Status >= 300 {
		// A refused invoke started nothing. Recording the question would put
		// a turn in the transcript that no answer can ever join.
		return
	}
	invocationID := run.InvocationIDOf(served.Response)
	if invocationID == "" {
		h.logger.Warn("deepwiki: accepted invoke carried no invocation id; the question is not recorded",
			"project", served.ProjectID)
		return
	}
	question := questionOf(served.Request)
	if question == "" {
		return
	}

	record := wikichat.Question{
		ProjectID:    served.ProjectID,
		UserID:       served.UserID,
		ChatKey:      chat.key,
		ToolkitID:    chat.toolkitID,
		ToolkitName:  served.ToolkitName,
		Capability:   CapabilityOf(served.ToolName),
		Question:     question,
		InvocationID: invocationID,
	}
	if err := h.store.RecordQuestion(ctx, record); err != nil {
		// Not surfaced to the caller: the invocation is already running and
		// the answer is already on its way. Losing the transcript is worse
		// than nothing and better than failing a question that succeeded.
		h.logger.Error("deepwiki: record wiki question",
			"project", record.ProjectID, "invocation", invocationID, "err", err)
		return
	}
	// The gap this line exists for is in the file header: nothing re-polls an
	// invocation the browser abandons, so a question logged here with no
	// matching "record wiki answer" is a turn whose answer was never drained.
	h.logger.Info("deepwiki: wiki question recorded",
		"project", record.ProjectID, "toolkit", record.ToolkitID,
		"invocation", invocationID, "capability", record.Capability)
}

// Poll is the GET-invocation hop with the terminal payload teed off it. The
// tee is providerhost/run's; what is DeepWiki's is only where the answer goes.
func (h *History) Poll(inner run.Forwarder) run.Forwarder {
	if h == nil {
		return inner
	}
	return run.Tee(inner, 0, h.logger, h.record)
}

// record writes the answer one terminal poll carried.
func (h *History) record(ctx context.Context, observed run.Observed) {
	project, projectErr := strconv.ParseInt(observed.ProjectID, 10, 64)
	user, userErr := strconv.ParseInt(observed.UserID, 10, 64)
	if projectErr != nil || userErr != nil || project <= 0 || user <= 0 {
		return
	}
	answer := wikichat.Answer{
		ProjectID:    project,
		UserID:       user,
		InvocationID: observed.InvocationID,
		Content:      observed.Content,
		IsError:      observed.Failed,
	}
	written, err := h.store.RecordAnswer(ctx, answer)
	if err != nil {
		h.logger.Error("deepwiki: record wiki answer",
			"project", project, "invocation", answer.InvocationID, "err", err)
		return
	}
	if written {
		h.logger.Info("deepwiki: wiki answer recorded",
			"project", project, "invocation", answer.InvocationID, "is_error", answer.IsError)
	}
}

// CapabilityOf names which of the two agents a tool is. It is the label the
// drawer renders on the answer, and the reason it is stored rather than
// derived on read: the toggle can move, and a transcript must say what
// actually ran.
func CapabilityOf(toolName string) string {
	if toolName == "deep_research" {
		return "research"
	}
	return "ask"
}

// questionOf reads the question out of the client's invoke body.
//
// The CLIENT's body, which matters more since #806: the attachment feature
// prepends the resolved page text to `question` on the Go host, downstream of
// this facade. So what is stored is what the user typed, and the expanded
// context block — tens of kilobytes of wiki pages — stays out of the
// transcript without anything here having to strip it.
func questionOf(body []byte) string {
	var payload struct {
		Parameters struct {
			Question string `json:"question"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.Parameters.Question)
}
