package conversations

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// NameRequest is one naming call: the first exchange of a conversation and
// who pays for it. The model's answer is a title only.
type NameRequest struct {
	ProjectID string
	UserID    string
	System    string
	User      string
}

// NameCompleter asks a model for a conversation title. The composition root
// adapts the shared predict completer (the project's default model, through
// elitea-llm-gateway); this package does not import it.
type NameCompleter interface {
	Complete(ctx context.Context, request NameRequest) (string, error)
}

// WithNamer wires POST /conversation_name. Left nil, the route answers 503:
// no model plane is composed.
func (h *Handler) WithNamer(namer NameCompleter) *Handler {
	h.namer = namer
	return h
}

const (
	// MaxGeneratedNameLength is the longest title the namer stores, in runes.
	MaxGeneratedNameLength = 80
	// nameExcerptRunes bounds each message of the exchange the model reads.
	nameExcerptRunes = 2000
	// nameTimeout bounds one naming call.
	nameTimeout = 30 * time.Second
	// nameMessages is how many message groups the namer reads (the first
	// question and the first answer, with room for an empty group).
	nameMessages = 4
)

// namingInstructions is the system message of a naming call.
const namingInstructions = "You name chat conversations. Reply with a title of 2 to 6 words " +
	"that says what the conversation is about, in the language of the conversation. " +
	"Do not use quotes, a final full stop or Markdown. Reply with the title only."

type generateNameBody struct {
	ExpectedName *string `json:"expected_name"`
}

type generatedName struct {
	Name    string `json:"name"`
	Renamed bool   `json:"renamed"`
}

// GenerateName names a conversation from its first exchange:
// POST /conversation_name/prompt_lib/{projectID}/{conversationID} with
// `{expected_name}`, the name the client gave the conversation itself (a
// cut of the first message). The conversation is renamed only while its name
// is still that one, so a person's own rename is never overwritten; the
// answer is the name it has then, and whether this call set it. Other devices
// get the new name through `changes_since`, as with any rename.
//
// The name is read again just before the write, after the model call: the
// window in which a rename can be lost is the update itself, not the call.
func (h *Handler) GenerateName(w http.ResponseWriter, r *http.Request) {
	if h.namer == nil {
		slog.ErrorContext(r.Context(), "conversation_name: no LLM plane composed")
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the LLM gateway is not configured"})
		return
	}
	if !h.authorizeConversation(w, r) {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")
	var body generateNameBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil || body.ExpectedName == nil {
		apierr.Write(w, apierr.BadRequest("expected_name is required"))
		return
	}
	expected := *body.ExpectedName
	current, err := h.repo.Get(r.Context(), projectID, conversationID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if current.Name != expected {
		writeJSON(w, http.StatusOK, generatedName{Name: current.Name})
		return
	}
	messages, err := h.repo.ListMessages(r.Context(), projectID, conversationID, MessagesQuery{Limit: nameMessages, SortBy: "created_at", SortOrder: "asc"})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	exchange := firstExchange(messages.Items)
	if exchange == "" {
		writeJSON(w, http.StatusOK, generatedName{Name: current.Name})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), nameTimeout)
	defer cancel()
	answer, err := h.namer.Complete(ctx, NameRequest{
		ProjectID: projectID,
		UserID:    callerID(r.Context()),
		System:    namingInstructions,
		User:      exchange,
	})
	name := cleanGeneratedName(answer)
	if err != nil || name == "" {
		slog.ErrorContext(r.Context(), "conversation_name: the model gave no title",
			"project_id", projectID, "conversation_id", conversationID, "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "the LLM gateway could not name this conversation"})
		return
	}
	again, err := h.repo.Get(r.Context(), projectID, conversationID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if again.Name != expected {
		writeJSON(w, http.StatusOK, generatedName{Name: again.Name})
		return
	}
	updated, err := h.repo.Update(r.Context(), projectID, conversationID, Conversation{Name: name})
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, generatedName{Name: updated.Name, Renamed: true})
}

func callerID(ctx context.Context) string {
	user, ok := auth.UserFromContext(ctx)
	if !ok {
		return ""
	}
	if user.UserID != "" {
		return user.UserID
	}
	return user.ID
}

// firstExchange is the first question and the first answer, each cut to
// nameExcerptRunes, framed for the model; "" without a question.
func firstExchange(messages []Message) string {
	var question, answer string
	for _, message := range messages {
		text := strings.TrimSpace(message.Content)
		if text == "" {
			continue
		}
		if message.Role == "user" {
			if question == "" {
				question = excerpt(text)
			}
		} else if question != "" && answer == "" {
			answer = excerpt(text)
		}
	}
	if question == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("Name this conversation.\n\n<question>\n")
	b.WriteString(question)
	b.WriteString("\n</question>")
	if answer != "" {
		b.WriteString("\n\n<answer>\n")
		b.WriteString(answer)
		b.WriteString("\n</answer>")
	}
	return b.String()
}

func excerpt(text string) string {
	if utf8.RuneCountInString(text) <= nameExcerptRunes {
		return text
	}
	return string([]rune(text)[:nameExcerptRunes]) + "…"
}

// cleanGeneratedName is the model's answer as a title: its first non-empty
// line, without quotes, Markdown emphasis, a "Title:" label or a final full
// stop, with no control characters, at most MaxGeneratedNameLength runes.
// "" when nothing is left.
func cleanGeneratedName(answer string) string {
	line := ""
	for _, candidate := range strings.Split(answer, "\n") {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			line = candidate
			break
		}
	}
	const marks = "\"'`*_“”‘’«» "
	line = strings.Trim(strings.TrimLeft(line, "# "), marks)
	if lower := strings.ToLower(line); strings.HasPrefix(lower, "title:") {
		line = line[len("title:"):]
	}
	line = strings.TrimRight(strings.Trim(line, marks), ".。")
	line = strings.Trim(line, marks)
	line = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, line)
	line = strings.Join(strings.Fields(line), " ")
	if utf8.RuneCountInString(line) > MaxGeneratedNameLength {
		line = strings.TrimSpace(string([]rune(line)[:MaxGeneratedNameLength]))
	}
	return line
}
