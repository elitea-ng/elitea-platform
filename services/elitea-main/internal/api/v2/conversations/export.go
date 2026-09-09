package conversations

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// Conversation export — the transcript as a file the reader keeps.
//
// ISSUE #851. The rail's row menu has always rendered an "Export" entry. It
// was disabled, its two options were labelled `Option1`/`Option2`, and there
// was no route behind either of them: a control that reads as "coming very
// soon" and was in fact wired to nothing. This is the missing half.
//
// TWO FORMATS, which is what the two menu options become:
//
//   - `format=md` (the default) is the READABLE document — a title, a short
//     header block, then one section per message. It is what a user means by
//     "export this conversation": something to paste into a ticket, a review
//     or a mail.
//   - `format=json` is the FAITHFUL one — every field the transcript read
//     carries, so a conversation can be archived and later read back by a
//     program rather than by a person.
//
// Both are served with `Content-Disposition: attachment`, so a browser saves
// the response instead of rendering it. The name is derived from the
// conversation's own name, which is why it is sanitised here rather than
// trusted: a conversation may be called anything, including something with a
// path separator or a quote in it.
//
// THE WHOLE TRANSCRIPT, NOT A PAGE. An export that silently stopped at the
// newest ten messages would be worse than no export: the file looks complete.
// The handler walks the message list in fixed-size pages until the server
// stops returning rows, and REFUSES — rather than writing a truncated file —
// for a conversation longer than exportMessageCap. Nothing in this product produces
// a conversation that long today; if one ever exists, the reader is told the
// export is incomplete rather than handed a plausible lie.

const (
	// exportMessagePageSize is how many message groups one read pulls. The
	// repository applies its own upper bound on `limit`, so the export asks
	// for a page size that is comfortably inside it and loops.
	exportMessagePageSize = 100
	// exportMessageCap bounds the whole document. See the note above on why
	// exceeding it is an error rather than a truncation.
	exportMessageCap = 10_000
	// exportFilenameLimit keeps a derived filename inside what every common
	// filesystem accepts, before the extension is appended.
	exportFilenameLimit = 80
)

// exportMessage is one message as an exported document carries it.
type exportMessage struct {
	ID          string         `json:"id"`
	UUID        string         `json:"uid,omitempty"`
	Role        string         `json:"role"`
	Author      string         `json:"author,omitempty"`
	Content     string         `json:"content"`
	ContentType string         `json:"content_type,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	// Feedback carries this message's like/dislike aggregate (#880), absent
	// when nobody has rated it — same "absence means unrated" contract
	// GetMessageFeedback's response carries. `Mine` is whichever user holds
	// the API key or session that requested the export, not a fixed identity:
	// two different members exporting the same conversation see their OWN
	// vote highlighted, same as the transcript does live.
	Feedback *MessageFeedbackSummary `json:"feedback,omitempty"`
}

// exportDocument is the `format=json` body, and the model the Markdown
// renderer writes from — one shape, so the two formats cannot drift into
// describing different conversations.
type exportDocument struct {
	ID           string          `json:"id"`
	UUID         string          `json:"uuid,omitempty"`
	ProjectID    string          `json:"project_id"`
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	ExportedAt   time.Time       `json:"exported_at"`
	MessageCount int             `json:"message_count"`
	Messages     []exportMessage `json:"messages"`
}

// exportFormat resolves the `format=` parameter.
//
// An ABSENT format is Markdown, the readable one, because a caller that names
// no format is a person following a link. An UNKNOWN format is refused rather
// than defaulted: a client that asked for `pdf` and silently received Markdown
// would ship a file with the wrong extension and no way to notice.
func exportFormat(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "md", "markdown":
		return "md", nil
	case "json":
		return "json", nil
	default:
		return "", apierr.BadRequest("unsupported export format: use md or json")
	}
}

// exportFilename derives a download name from the conversation's own name.
//
// Anything that is not a letter, digit, dash or underscore becomes an
// underscore, runs collapse, and an empty result falls back to the
// conversation id — so a conversation named entirely in punctuation still
// downloads as a file rather than as `.md`.
func exportFilename(name, conversationID, extension string) string {
	var b strings.Builder
	lastWasSeparator := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
			lastWasSeparator = false
		default:
			if !lastWasSeparator && b.Len() > 0 {
				b.WriteByte('_')
				lastWasSeparator = true
			}
		}
		if b.Len() >= exportFilenameLimit {
			break
		}
	}
	stem := strings.Trim(b.String(), "_")
	if stem == "" {
		stem = "conversation_" + conversationID
	}
	return stem + "." + extension
}

// participantNames maps a participant row id to a display name, for
// attributing each message to whoever wrote it.
//
// TWO PLACES ARE READ, in the order the chat surface itself reads them.
// `meta.user_name` is where the participants read puts the display name it
// resolved for a USER participant (the auth_core__user join in
// ConversationsRepo.ListParticipants) — the transcript's own author captions
// read exactly that key. `entity_meta.name` is where an AGENT, pipeline or
// model participant carries its own name, a document whose shape belongs to
// the participating entity rather than to this route. A participant that
// carries neither simply has no name, and its messages fall back to the role
// label.
func participantNames(participants []Participant) map[int]string {
	names := make(map[int]string, len(participants))
	for _, p := range participants {
		for _, candidate := range []any{p.Meta["user_name"], p.EntityMeta["name"], p.EntityMeta["display_name"]} {
			if value, ok := candidate.(string); ok && strings.TrimSpace(value) != "" {
				names[p.ID] = value
				break
			}
		}
	}
	return names
}

// roleLabel is the heading one message gets in the Markdown document.
func roleLabel(role string) string {
	switch strings.ToLower(role) {
	case "user", "human":
		return "User"
	case "assistant", "ai":
		return "Assistant"
	case "":
		return "Message"
	default:
		return strings.ToUpper(role[:1]) + role[1:]
	}
}

// renderExportMarkdown writes the readable document.
//
// Message bodies are emitted verbatim, NOT escaped or fenced. The content of a
// chat message already is Markdown in this product — the renderer on screen
// treats it as such — so escaping it here would export something that does not
// match what the user was looking at when they asked for the export.
func renderExportMarkdown(doc exportDocument) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", doc.Name)
	if doc.Description != "" {
		fmt.Fprintf(&b, "%s\n\n", doc.Description)
	}
	fmt.Fprintf(&b, "- Conversation: %s\n", doc.ID)
	fmt.Fprintf(&b, "- Project: %s\n", doc.ProjectID)
	fmt.Fprintf(&b, "- Messages: %d\n", doc.MessageCount)
	fmt.Fprintf(&b, "- Exported: %s\n\n", doc.ExportedAt.UTC().Format(time.RFC3339))

	if len(doc.Messages) == 0 {
		b.WriteString("_This conversation has no messages._\n")
		return b.String()
	}

	for _, message := range doc.Messages {
		author := message.Author
		if author == "" {
			author = roleLabel(message.Role)
		}
		fmt.Fprintf(&b, "---\n\n## %s — %s\n\n", author, message.CreatedAt.UTC().Format(time.RFC3339))
		content := strings.TrimRight(message.Content, "\n")
		if content == "" {
			content = "_(empty message)_"
		}
		b.WriteString(content)
		b.WriteString("\n\n")
		if message.Feedback != nil && (message.Feedback.Likes > 0 || message.Feedback.Dislikes > 0) {
			fmt.Fprintf(&b, "_Feedback: %d 👍 · %d 👎_\n\n", message.Feedback.Likes, message.Feedback.Dislikes)
		}
	}
	return b.String()
}

// collectExportMessages reads the whole transcript, oldest message first.
//
// `sort_order=asc` is the document order a reader expects, and it is also what
// makes the paging loop safe: a descending read paged from an offset would
// re-serve rows as new messages arrive. The loop stops on a short page, which
// is the only end-of-list signal that does not depend on `total` being exact.
func (h *Handler) collectExportMessages(r *http.Request, projectID, conversationID string) ([]Message, error) {
	messages := make([]Message, 0)
	for offset := 0; ; offset += exportMessagePageSize {
		page, err := h.repo.ListMessages(r.Context(), projectID, conversationID, MessagesQuery{
			Limit:     exportMessagePageSize,
			Offset:    offset,
			SortBy:    "created_at",
			SortOrder: "asc",
		})
		if err != nil {
			return nil, err
		}
		messages = append(messages, page.Items...)
		if len(page.Items) < exportMessagePageSize {
			return messages, nil
		}
		if len(messages) >= exportMessageCap {
			// A REFUSAL, not a truncation. `apierr.BadRequest` rather than a
			// 413 because this package's error type carries the four named
			// refusals the router already maps, and a partial export that
			// looks complete is the failure this guard exists to prevent —
			// the status code matters less than the file not being written.
			return nil, apierr.BadRequest(
				fmt.Sprintf("this conversation has more than %d messages and cannot be exported in one document", exportMessageCap))
		}
	}
}

// buildExportDocument assembles the document both formats are written from.
func (h *Handler) buildExportDocument(r *http.Request, projectID, conversationID string) (exportDocument, error) {
	conv, err := h.repo.Get(r.Context(), projectID, conversationID)
	if err != nil {
		return exportDocument{}, err
	}

	// Every downstream read keys off `conv.ID`, never the path segment: the
	// path may carry a UUID, and messages and participants join on the
	// numeric id only. Handler.Get already does this; getting it wrong here
	// would export an empty transcript for every UUID deep link.
	messages, err := h.collectExportMessages(r, projectID, conv.ID)
	if err != nil {
		return exportDocument{}, err
	}

	// A participant list is attribution, not content: a conversation whose
	// participants cannot be read still exports, with role labels instead of
	// names.
	participants, _ := h.repo.ListParticipants(r.Context(), projectID, conv.ID)
	names := participantNames(participants)

	// The repository's ordering is trusted for the page boundaries, and the
	// assembled list is sorted once more here so a repository that ignores
	// `sort_by` cannot produce a document whose messages are out of order.
	sort.SliceStable(messages, func(i, j int) bool { return messages[i].CreatedAt.Before(messages[j].CreatedAt) })

	// #880: one batched read for every message's feedback rather than one
	// read per message — see ListMessageFeedbackBatch's own doc comment.
	// Feedback is a nice-to-have on an export, not the transcript itself, so
	// a caller with no resolved identity (defensive: this route sits behind
	// authentication) still gets the aggregate counts, just no highlighted
	// "mine".
	messageUUIDs := make([]string, 0, len(messages))
	for _, message := range messages {
		if message.UUID != "" {
			messageUUIDs = append(messageUUIDs, message.UUID)
		}
	}
	user, _ := auth.UserFromContext(r.Context())
	feedbackByMessage, err := h.repo.ListMessageFeedbackBatch(r.Context(), projectID, messageUUIDs, user.ID)
	if err != nil {
		return exportDocument{}, err
	}

	rows := make([]exportMessage, 0, len(messages))
	for _, message := range messages {
		row := exportMessage{
			ID:          message.ID,
			UUID:        message.UUID,
			Role:        message.Role,
			Content:     message.Content,
			ContentType: message.ContentType,
			CreatedAt:   message.CreatedAt,
			Metadata:    message.Metadata,
		}
		if message.AuthorParticipantID != nil {
			row.Author = names[*message.AuthorParticipantID]
		}
		if summary, ok := feedbackByMessage[message.UUID]; ok {
			row.Feedback = &summary
		}
		rows = append(rows, row)
	}

	return exportDocument{
		ID:           conv.ID,
		UUID:         conv.UUID,
		ProjectID:    conv.ProjectID,
		Name:         conv.Name,
		Description:  conv.Description,
		CreatedAt:    conv.CreatedAt,
		UpdatedAt:    conv.UpdatedAt,
		ExportedAt:   time.Now().UTC(),
		MessageCount: len(rows),
		Messages:     rows,
	}, nil
}

// Export serves one conversation's transcript as a downloadable document.
func (h *Handler) Export(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")

	format, err := exportFormat(r.URL.Query().Get("format"))
	if err != nil {
		apierr.Write(w, err)
		return
	}

	doc, err := h.buildExportDocument(r, projectID, conversationID)
	if err != nil {
		apierr.Write(w, err)
		return
	}

	extension := "md"
	contentType := "text/markdown; charset=utf-8"
	body := []byte(renderExportMarkdown(doc))
	if format == "json" {
		extension = "json"
		contentType = "application/json; charset=utf-8"
		// Indented, because this file is read by people as often as by
		// programs, and a one-line 4 MB transcript is neither.
		encoded, encodeErr := json.MarshalIndent(doc, "", "  ")
		if encodeErr != nil {
			apierr.Write(w, fmt.Errorf("conversations: encode export: %w", encodeErr))
			return
		}
		body = append(encoded, '\n')
	}

	filename := exportFilename(doc.Name, doc.ID, extension)
	// mime.FormatMediaType writes the RFC 2183 form, including the extended
	// `filename*` parameter when the name is not plain ASCII — which the
	// sanitiser above already guarantees, but the encoder is still the right
	// place for the question rather than a hand-built header string.
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	w.Header().Set("Content-Type", contentType)
	// The web client reads the name back off the header to name the saved
	// file; without this the browser hides the header from the fetch and the
	// download lands as "download".
	w.Header().Set("Access-Control-Expose-Headers", "Content-Disposition")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body) // connection already committed; ignore write error
}
