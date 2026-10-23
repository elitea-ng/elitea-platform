package agentexecution

// #606 part 3, gap 1: the CURRENT turn's attachment chunks reach the worker.
//
// Part 2 made an upload a row; nothing sent it. Both start paths hardcoded
// `InputAttachments: []byte("[]")`, so a file the user attached to the message
// they were sending was the one message the model could not see: an earlier
// turn's file at least had a row the history projection could pick up, this
// one had nothing between the admission and the worker.
//
// These assert on what the ADMISSION was handed, not on the helper alone,
// because the helper being right and the field still being `[]` is exactly the
// defect that shipped.

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
)

func TestCurrentApplicationStartSendsTheTurnsAttachmentChunksConcatenatedInOrder(t *testing.T) {
	resolver := &currentApplicationResolverStub{target: CurrentApplicationTarget{
		ApplicationID: 31, ApplicationVersionID: 41,
		Variables:      json.RawMessage(`[]`),
		VersionDetails: json.RawMessage(`{"id":41,"application_id":31,"agent_type":"agent","instructions":"Be concise","llm_settings":{"model_name":"test"},"meta":{},"tools":[]}`),
		ChatHistory:    json.RawMessage(`[]`),
	}}
	admissions := newCurrentAttachmentAdmissionStub()
	service, err := NewCurrentApplicationStartService(
		resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{}, &currentApplicationVersionFreezerStub{}, admissions,
	)
	if err != nil {
		t.Fatal(err)
	}
	request := validCurrentApplicationStartRequest()
	request.Attachments = currentAttachmentInputRefs()

	if _, err := service.StartCurrentApplication(context.Background(), request); err != nil {
		t.Fatalf("StartCurrentApplication() error = %v", err)
	}
	submitted := admissions.requests[0]
	assertCurrentTurnAttachmentsWereSent(
		t,
		submitted.Input.GetInputAttachments(),
		submitted.CurrentTurn.Attachments,
	)
}

func TestCurrentAdhocStartSendsTheTurnsAttachmentChunksConcatenatedInOrder(t *testing.T) {
	resolver := &currentApplicationResolverStub{adhocTarget: CurrentAdhocTarget{
		TargetParticipantID: 21,
		LLMSettings:         json.RawMessage(`{"model_name":"saved"}`),
		Instructions:        "Project chat instructions",
		Tools:               json.RawMessage(`[]`),
		ChatHistory:         json.RawMessage(`[]`),
		ConversationMeta:    json.RawMessage(`{}`),
	}}
	admissions := newCurrentAttachmentAdmissionStub()
	service, err := NewCurrentApplicationStartService(
		resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{}, &currentApplicationVersionFreezerStub{}, admissions,
	)
	if err != nil {
		t.Fatal(err)
	}
	request := validCurrentAdhocStartRequest()
	request.Attachments = currentAttachmentInputRefs()

	if _, err := service.StartCurrentAdhoc(context.Background(), request); err != nil {
		t.Fatalf("StartCurrentAdhoc() error = %v", err)
	}
	submitted := admissions.requests[0]
	assertCurrentTurnAttachmentsWereSent(
		t,
		submitted.Input.GetInputAttachments(),
		submitted.CurrentAdhocTurn.Attachments,
	)
}

// assertCurrentTurnAttachmentsWereSent pins the two properties that make the
// field usable by the worker: it is the FLATTENED concatenation of every
// attachment's chunks (the legacy chat_history.py:67-73 extends, it does not
// nest), and its order is the item order the same admission is writing, so
// chunk N in the prompt belongs to the file the transcript shows Nth.
func assertCurrentTurnAttachmentsWereSent(
	t *testing.T,
	sent []byte,
	written []CurrentTurnAttachment,
) {
	t.Helper()
	if len(written) != 2 {
		t.Fatalf("turn attachments=%+v", written)
	}
	var chunks []map[string]any
	if err := json.Unmarshal(sent, &chunks); err != nil {
		t.Fatalf("input_attachments=%s err=%v", sent, err)
	}
	if len(chunks) != 2 {
		t.Fatalf("input_attachments is not one flat chunk per file: %s", sent)
	}
	for index, attachment := range written {
		var stored []map[string]any
		if err := json.Unmarshal(attachment.Content, &stored); err != nil || len(stored) != 1 {
			t.Fatalf("stored content %d = %s err=%v", index, attachment.Content, err)
		}
		if chunks[index]["type"] != "text" ||
			chunks[index]["text"] != stored[0]["text"] {
			t.Fatalf("chunk %d = %+v, stored = %+v", index, chunks[index], stored[0])
		}
	}
	// The report.pdf came first in the request, so its chunk comes first here.
	first, _ := chunks[0]["text"].(string)
	if !bytes.Contains([]byte(first), []byte("report.pdf")) {
		t.Fatalf("chunk order does not follow item order: %s", sent)
	}
}

func TestCurrentApplicationStartSendsAnEmptyListWhenTheTurnAttachedNothing(t *testing.T) {
	resolver := &currentApplicationResolverStub{target: CurrentApplicationTarget{
		ApplicationID: 31, ApplicationVersionID: 41,
		Variables:      json.RawMessage(`[]`),
		VersionDetails: json.RawMessage(`{"id":41,"application_id":31,"agent_type":"agent","instructions":"Be concise","llm_settings":{"model_name":"test"},"meta":{},"tools":[]}`),
		ChatHistory:    json.RawMessage(`[]`),
	}}
	admissions := newCurrentAttachmentAdmissionStub()
	service, err := NewCurrentApplicationStartService(
		resolver, resolver, resolver, resolver, &currentAgentGuardrailStub{}, &currentApplicationVersionFreezerStub{}, admissions,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.StartCurrentApplication(
		context.Background(),
		validCurrentApplicationStartRequest(),
	); err != nil {
		t.Fatalf("StartCurrentApplication() error = %v", err)
	}
	// `[]`, not `null` and not an empty byte slice: the field is a JSON
	// document the worker parses, and "attached nothing" has to stay
	// byte-identical to what every turn sent before #606.
	if got := admissions.requests[0].Input.GetInputAttachments(); string(got) != "[]" {
		t.Fatalf("input_attachments=%q", got)
	}
}

// TestCurrentTurnInputAttachmentsFlattensMultiChunkContent covers the state the
// items are in AFTER the worker's extraction step has appended a second chunk
// (rpc/chat_all.py:366-374) — the shape a resumed or retried turn can be
// carrying — plus the degenerate contents the helper must not choke on.
func TestCurrentTurnInputAttachmentsFlattensMultiChunkContent(t *testing.T) {
	got := currentTurnInputAttachments([]CurrentTurnAttachment{
		{Content: json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)},
		{Content: json.RawMessage(`[]`)},
		{Content: json.RawMessage(`[{"type":"text","text":"c"}]`)},
	})
	want := `[{"type":"text","text":"a"},{"type":"text","text":"b"},{"type":"text","text":"c"}]`
	if string(got) != want {
		t.Fatalf("input_attachments=%s want %s", got, want)
	}
	// A payload that is not an array at all contributes nothing rather than
	// refusing the turn: legacy-era data has objects here (chat_history.py's
	// non-list fallback), and a malformed attachment must not stop a message
	// whose TEXT is fine from being sent.
	if got := currentTurnInputAttachments([]CurrentTurnAttachment{
		{Content: json.RawMessage(`{}`)},
		{Content: nil},
		{Content: json.RawMessage(`[{"type":"text","text":"c"}]`)},
	}); string(got) != `[{"type":"text","text":"c"}]` {
		t.Fatalf("input_attachments=%s", got)
	}
	if got := currentTurnInputAttachments(nil); string(got) != "[]" {
		t.Fatalf("input_attachments=%q", got)
	}
}

// TestAttachmentScaffoldMarksOnlyDocumentsForContentExtraction pins the
// convention the worker reads. The legacy runtime does not have to persist its
// equivalent flag (utils/attachments.py:320 `needs_content_extraction`) because
// producer and consumer are one process; here they are two services, so the
// marker is part of the stored content and part of this repository's contract.
func TestAttachmentScaffoldMarksOnlyDocumentsForContentExtraction(t *testing.T) {
	attachments, err := currentTurnAttachments(
		"ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
		testConversationUUID,
		[]CurrentTurnAttachmentRef{
			{Bucket: "chat-attachments", Name: testConversationUUID + "/report.pdf"},
			{Bucket: "chat-attachments", Name: testConversationUUID + "/shot.png"},
			{Bucket: "chat-attachments", Name: testConversationUUID + "/diagram.svg"},
		},
	)
	if err != nil || len(attachments) != 3 {
		t.Fatalf("attachments=%+v err=%v", attachments, err)
	}
	marker := currentAttachmentMarker(t, attachments[0].Content)
	if marker == nil {
		t.Fatalf("document carries no extraction marker: %s", attachments[0].Content)
	}
	if marker["needs_content_extraction"] != true ||
		marker["bucket"] != "chat-attachments" ||
		marker["name"] != testConversationUUID+"/report.pdf" ||
		marker["filepath"] != "/chat-attachments/"+testConversationUUID+"/report.pdf" {
		t.Fatalf("marker=%+v", marker)
	}
	// The item id is what lets the worker name EXACTLY the row it enriched
	// when it reports the extracted text back (#607). Matching on
	// (bucket, name) instead is ambiguous the moment the same file is
	// attached twice in one conversation, and would write one file's text
	// onto another's row.
	if marker["item_id"] != attachments[0].ItemID {
		t.Fatalf("marker item_id=%v want the attachment's own id %q", marker["item_id"], attachments[0].ItemID)
	}
	// An image has no text to extract (the legacy ImageToModelProcessor sets no
	// flag), so the key is ABSENT rather than present-and-false: the worker's
	// default reading of a missing key must be "nothing to do".
	if got := currentAttachmentMarker(t, attachments[1].Content); got != nil {
		t.Fatalf("image carries an extraction marker: %+v", got)
	}
	// .svg is an image by MIME and a document by the legacy rule, so it is
	// marked — the marker follows attachment_type, not the media type.
	if got := currentAttachmentMarker(t, attachments[2].Content); got == nil {
		t.Fatalf("svg is a document but carries no marker: %s", attachments[2].Content)
	}
	// The marker rides ALONG the text chunk rather than replacing or
	// displacing it: a chunk that leaks to a model is still a valid text
	// chunk.
	var chunks []map[string]any
	if err := json.Unmarshal(attachments[0].Content, &chunks); err != nil ||
		len(chunks) != 1 || chunks[0]["type"] != "text" || chunks[0]["text"] == "" {
		t.Fatalf("chunks=%+v err=%v", chunks, err)
	}
}

func currentAttachmentMarker(t *testing.T, content json.RawMessage) map[string]any {
	t.Helper()
	var chunks []map[string]any
	if err := json.Unmarshal(content, &chunks); err != nil || len(chunks) != 1 {
		t.Fatalf("content=%s err=%v", content, err)
	}
	marker, ok := chunks[0][attachmentExtractionMarkerKey].(map[string]any)
	if !ok {
		return nil
	}
	return marker
}

func currentAttachmentInputRefs() []CurrentTurnAttachmentRef {
	return []CurrentTurnAttachmentRef{
		{Bucket: "chat-attachments", Name: "8bc66e50-46c4-4e2c-94ec-daec6c596ac0/report.pdf"},
		{Bucket: "chat-attachments", Name: "8bc66e50-46c4-4e2c-94ec-daec6c596ac0/shot.png"},
	}
}

func newCurrentAttachmentAdmissionStub() *currentApplicationAdmissionStub {
	admittedAt := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	return &currentApplicationAdmissionStub{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "execution-attachments", CommandID: "command-attachments",
		Created: true, AdmittedAt: admittedAt, Deadline: admittedAt.Add(time.Minute),
	}}
}

// A file that belongs to a DIFFERENT conversation is refused.
//
// `filepath` comes from the browser, so without this the name is whatever the
// caller typed: another user's conversation key is just
// `{other-conversation-uuid}/file.pdf`, and admitting it would have the worker
// read that file through the artifact toolkit and splice its text into a
// conversation the caller was never granted. The upload path keys every object
// `{conversationID}/{sanitised name}`, so the prefix IS "this was uploaded
// here".
func TestCurrentTurnAttachmentsRefusesAnotherConversationsFile(t *testing.T) {
	const otherConversation = "1f2e3d4c-5b6a-4978-8695-a4b3c2d1e0f9"
	for _, name := range []string{
		otherConversation + "/report.pdf",
		"report.pdf",
		"chat-attachments/report.pdf",
	} {
		if _, err := currentTurnAttachments(
			"ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
			testConversationUUID,
			[]CurrentTurnAttachmentRef{{Bucket: "chat-attachments", Name: name}},
		); err == nil {
			t.Errorf("name %q outside the conversation was admitted", name)
		}
	}
}

// A name the storage layer could never address is refused at admission, so no
// row is stored whose bytes the delete path cannot remove.
func TestCurrentTurnAttachmentsRefusesAnUnaddressableKey(t *testing.T) {
	for _, name := range []string{
		testConversationUUID + "/../escape.pdf",
		testConversationUUID + "/./report.pdf",
		testConversationUUID + "//report.pdf",
		testConversationUUID + "/report.pdf/",
	} {
		if _, err := currentTurnAttachments(
			"ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
			testConversationUUID,
			[]CurrentTurnAttachmentRef{{Bucket: "chat-attachments", Name: name}},
		); err == nil {
			t.Errorf("un-addressable name %q was admitted", name)
		}
	}
}

// The classification must not depend on the host's MIME database.
func TestAttachmentKindClassifiesWithoutTheHostMIMEDatabase(t *testing.T) {
	for name, want := range map[string]string{
		"a.bmp":  AttachmentKindImage,
		"a.tiff": AttachmentKindImage,
		"a.heic": AttachmentKindImage,
		"a.png":  AttachmentKindImage,
		"a.SVG":  AttachmentKindDocument,
		"a.pdf":  AttachmentKindDocument,
	} {
		if got := AttachmentKind(name); got != want {
			t.Errorf("AttachmentKind(%q)=%q want %q", name, got, want)
		}
	}
}
