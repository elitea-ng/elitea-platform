package conversations

// The three pure decisions the export route makes before it touches a
// database: which format was asked for, what the downloaded file is called,
// and what the readable document looks like.
//
// These have no integration test of their own — the Postgres one next door
// (internal/infra/db/repos/conversation_export_postgres_integration_test.go)
// covers "the document holds the conversation that was stored" and would be a
// wasteful place to enumerate filename edge cases.

import (
	"strings"
	"testing"
	"time"
)

func TestExportFormatResolvesTheOfferedFormats(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{"", "md"},
		{"md", "md"},
		{"MD", "md"},
		{" markdown ", "md"},
		{"json", "json"},
		{"JSON", "json"},
	} {
		got, err := exportFormat(tc.raw)
		if err != nil {
			t.Errorf("exportFormat(%q) refused: %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("exportFormat(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// An unknown format is refused rather than defaulted — a client that asked for
// something it did not get would save the wrong extension and never notice.
func TestExportFormatRefusesAnythingElse(t *testing.T) {
	for _, raw := range []string{"pdf", "html", "csv", "txt"} {
		if _, err := exportFormat(raw); err == nil {
			t.Errorf("exportFormat(%q) was accepted, want a refusal", raw)
		}
	}
}

// The filename comes from a conversation NAME, which a user may set to
// anything at all — including a path separator, a quote, or nothing usable.
func TestExportFilenameSanitisesTheConversationName(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
	}{
		{"autotest_plain", "autotest_plain.md"},
		{"Quarterly review 2026", "Quarterly_review_2026.md"},
		{"../../etc/passwd", "etc_passwd.md"},
		{`he said "hi"`, "he_said_hi.md"},
		{"  spaced  out  ", "spaced_out.md"},
		{"日本語", "conversation_42.md"},
		{"!!!", "conversation_42.md"},
		{"", "conversation_42.md"},
	} {
		if got := exportFilename(tc.name, "42", "md"); got != tc.want {
			t.Errorf("exportFilename(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A very long name is truncated rather than rejected: a filename no
// filesystem will accept is a failed download, and the name is decoration.
func TestExportFilenameIsBounded(t *testing.T) {
	got := exportFilename(strings.Repeat("a", 500), "42", "json")
	if len(got) > exportFilenameLimit+len(".json") {
		t.Errorf("exportFilename produced %d characters, want at most %d", len(got), exportFilenameLimit+len(".json"))
	}
	if !strings.HasSuffix(got, ".json") {
		t.Errorf("exportFilename dropped the extension: %q", got)
	}
}

func TestRenderExportMarkdownWritesTheTranscript(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	doc := exportDocument{
		ID:           "7",
		ProjectID:    "1",
		Name:         "autotest_render",
		ExportedAt:   at,
		MessageCount: 2,
		Messages: []exportMessage{
			{ID: "1", Role: "user", Author: "autotest_author", Content: "the question", CreatedAt: at},
			{ID: "2", Role: "assistant", Content: "the answer", CreatedAt: at.Add(time.Minute)},
		},
	}

	body := renderExportMarkdown(doc)
	for _, want := range []string{
		"# autotest_render",
		"- Conversation: 7",
		"- Messages: 2",
		"## autotest_author — 2026-01-02T03:04:05Z",
		"the question",
		// No author on the second message, so it falls back to its role.
		"## Assistant — 2026-01-02T03:05:05Z",
		"the answer",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the document is missing %q:\n%s", want, body)
		}
	}
	if strings.Index(body, "the question") > strings.Index(body, "the answer") {
		t.Error("the document put the answer before the question")
	}
}

// An empty conversation still exports. A zero-byte file would be
// indistinguishable from a failed download.
func TestRenderExportMarkdownSaysSoWhenThereIsNothingToSay(t *testing.T) {
	body := renderExportMarkdown(exportDocument{ID: "7", ProjectID: "1", Name: "autotest_empty", ExportedAt: time.Now()})
	if !strings.Contains(body, "# autotest_empty") || !strings.Contains(body, "no messages") {
		t.Errorf("an empty conversation exported as:\n%s", body)
	}
}

// A message with no text is a real state (an attachment-only turn), and it
// must not silently collapse two headings into one another.
func TestRenderExportMarkdownMarksAnEmptyMessage(t *testing.T) {
	at := time.Now().UTC()
	body := renderExportMarkdown(exportDocument{
		ID: "7", ProjectID: "1", Name: "autotest_blank", ExportedAt: at, MessageCount: 1,
		Messages: []exportMessage{{ID: "1", Role: "user", CreatedAt: at}},
	})
	if !strings.Contains(body, "(empty message)") {
		t.Errorf("a message with no content vanished from the document:\n%s", body)
	}
}

func TestParticipantNamesPrefersTheResolvedUserName(t *testing.T) {
	names := participantNames([]Participant{
		// A user participant: the resolved display name lives on `meta`,
		// which is where the transcript's own author captions read it.
		{ID: 1, EntityName: "user", Meta: map[string]any{"user_name": "Ada"}, EntityMeta: map[string]any{"id": 42}},
		// An agent participant names itself through `entity_meta`.
		{ID: 2, EntityName: "application", EntityMeta: map[string]any{"name": "Reviewer"}},
		// Neither: the message falls back to its role label.
		{ID: 3, EntityName: "application", EntityMeta: map[string]any{"id": 9}},
		// Present but blank is the same as absent.
		{ID: 4, EntityName: "user", Meta: map[string]any{"user_name": "   "}},
	})
	if names[1] != "Ada" || names[2] != "Reviewer" {
		t.Errorf("participantNames resolved %v, want 1=Ada and 2=Reviewer", names)
	}
	if _, ok := names[3]; ok {
		t.Errorf("participant 3 was named %q, want no name at all", names[3])
	}
	if _, ok := names[4]; ok {
		t.Errorf("a blank name was accepted as %q", names[4])
	}
}
