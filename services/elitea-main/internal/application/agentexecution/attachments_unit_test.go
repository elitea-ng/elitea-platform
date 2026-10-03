package agentexecution

// #606: the classification, filepath split and content scaffold the admission
// stores in chat_messages_attachment.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestParseAttachmentFilepathKeepsTheConversationPrefixInTheName(t *testing.T) {
	// pylon's parse_filepath (utils/attachments.py:33-39) strips ONE leading
	// slash then splits on the FIRST remaining one. The upload endpoint keys
	// objects `{conversationUUID}/{filename}`, so the uuid is part of the
	// object's name, not part of the bucket and not something to trim.
	ref, ok := ParseAttachmentFilepath(
		"/chat-attachments/8bc66e50-46c4-4e2c-94ec-daec6c596ac0/quarterly report.pdf",
	)
	if !ok || ref.Bucket != "chat-attachments" ||
		ref.Name != "8bc66e50-46c4-4e2c-94ec-daec6c596ac0/quarterly report.pdf" {
		t.Fatalf("ref=%+v ok=%v", ref, ok)
	}
}

func TestParseAttachmentFilepathRejectsUnsplittableAndUnstorableValues(t *testing.T) {
	for _, filepath := range []string{
		"",
		"/",
		"bucket-only",
		"/bucket-only",
		"//name.pdf",
		"/bucket/",
		"/bucket/na\x00me",
		"/bucket/na\nme",
		"/" + strings.Repeat("b", 257) + "/name.pdf",
		"/bucket/" + strings.Repeat("n", 257),
	} {
		if _, ok := ParseAttachmentFilepath(filepath); ok {
			t.Fatalf("accepted %q", filepath)
		}
	}
}

func TestAttachmentKindIsPylonsTypeNotAMIMEType(t *testing.T) {
	// The column holds "text"/"image"/"document", never a MIME type. .svg is
	// carved out of "image" on purpose (utils/attachments.py:325), the same
	// carve-out the upload path's size ceiling uses.
	for name, want := range map[string]string{
		"conv/shot.png":     AttachmentKindImage,
		"conv/photo.JPEG":   AttachmentKindImage,
		"conv/diagram.svg":  AttachmentKindDocument,
		"conv/diagram.SVG":  AttachmentKindDocument,
		"conv/report.pdf":   AttachmentKindDocument,
		"conv/notes.md":     AttachmentKindDocument,
		"conv/no-extension": AttachmentKindDocument,
	} {
		if got := AttachmentKind(name); got != want {
			t.Fatalf("AttachmentKind(%q)=%q want %q", name, got, want)
		}
	}
}

// testConversationUUID is the conversation these fixtures belong to. Every
// attachment name carries it as a prefix because that is what the upload path
// writes and what currentTurnAttachments now REQUIRES: a name outside this
// conversation is refused, so a caller cannot name another conversation's file
// and have the worker read it into this one.
const testConversationUUID = "7c9e1a52-0f3b-4c8d-9b21-5d6e4f8a1c30"

func TestCurrentTurnAttachmentsNumbersItemsFromOneAndScaffoldsContent(t *testing.T) {
	const questionID = "ee92ccbd-3312-4c72-b20b-fddf224e7c0e"
	attachments, err := currentTurnAttachments(questionID, testConversationUUID, []CurrentTurnAttachmentRef{
		{Bucket: "chat-attachments", Name: testConversationUUID + "/report.pdf"},
		{Bucket: "chat-attachments", Name: testConversationUUID + "/shot.png"},
	})
	if err != nil || len(attachments) != 2 {
		t.Fatalf("attachments=%+v err=%v", attachments, err)
	}
	if attachments[0].AttachmentType != AttachmentKindDocument ||
		attachments[1].AttachmentType != AttachmentKindImage {
		t.Fatalf("types=%q,%q", attachments[0].AttachmentType, attachments[1].AttachmentType)
	}
	// Item ids are derived from the question id, so a retried start addresses
	// the same rows; distinct per position, so two files never collide.
	if attachments[0].ItemID == attachments[1].ItemID {
		t.Fatalf("attachment item ids collided: %q", attachments[0].ItemID)
	}
	if attachments[0].ItemID != currentTurnUUID(questionID, "attachment-item-1") ||
		attachments[1].ItemID != currentTurnUUID(questionID, "attachment-item-2") {
		t.Fatalf("item ids are not derived from the question id: %+v", attachments)
	}

	// The scaffold is pylon's document shape: an ARRAY of chunks with exactly
	// one text chunk naming bucket, filename and filepath. Not an empty array
	// — that would claim content was computed and found to be none.
	// map[string]any, not map[string]string: a document chunk also carries the
	// extraction marker, which is a nested object (see
	// attachmentExtractionMarkerKey).
	var chunks []map[string]any
	if err := json.Unmarshal(attachments[0].Content, &chunks); err != nil {
		t.Fatalf("content=%s err=%v", attachments[0].Content, err)
	}
	if len(chunks) != 1 || chunks[0]["type"] != "text" {
		t.Fatalf("chunks=%+v", chunks)
	}
	text, _ := chunks[0]["text"].(string)
	for _, want := range []string{
		"Bucket: chat-attachments",
		"Filename: " + testConversationUUID + "/report.pdf",
		"filepath: /chat-attachments/" + testConversationUUID + "/report.pdf",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("scaffold %q missing %q", text, want)
		}
	}
}

// TestAttachmentHeaderMakesNoPromiseTheRuntimeCannotKeep pins the removal of
// pylon's note. It told the model that the full text was already embedded and
// that file-reading tools were available; on the native runtime neither was
// true, and the model answered about a PDF it had never read.
func TestAttachmentHeaderMakesNoPromiseTheRuntimeCannotKeep(t *testing.T) {
	ref := CurrentTurnAttachmentRef{Bucket: "chat-attachments", Name: testConversationUUID + "/Unknown.pdf"}
	for name, image := range map[string]inlineAttachmentImage{
		"document":         {},
		"unembedded image": {refusal: "NOTE: x", extract: true},
		"embedded image":   {dataURL: "data:image/png;base64,AAAA"},
	} {
		var chunks []map[string]any
		if err := json.Unmarshal(attachmentContentScaffold(ref, AttachmentKindDocument, "item-1", image), &chunks); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		header, _ := chunks[0]["text"].(string)
		for _, falsehood := range []string{"EMBEDDED", "full text is already included", "File reading tools are available"} {
			if strings.Contains(header, falsehood) {
				t.Fatalf("%s: header still claims %q:\n%s", name, falsehood, header)
			}
		}
		if !strings.Contains(header, attachmentHeaderNote) {
			t.Fatalf("%s: header lacks the honest note:\n%s", name, header)
		}
	}
}

func TestAttachmentImageRefusalsDoNotOfferAToolThatDoesNotExist(t *testing.T) {
	failing := &stubAttachmentImageReader{err: errors.New("gone")}
	fitting := &stubAttachmentImageReader{content: []byte("png bytes")}
	budget := 0
	for _, image := range []inlineAttachmentImage{
		inlineAttachmentImageFor(context.Background(), failing, 7,
			CurrentTurnAttachmentRef{Bucket: "chat-attachments", Name: testConversationUUID + "/diagram.bmp"}, nil),
		inlineAttachmentImageFor(context.Background(), failing, 7,
			CurrentTurnAttachmentRef{Bucket: "chat-attachments", Name: testConversationUUID + "/photo.png"}, nil),
		inlineAttachmentImageFor(context.Background(), fitting, 7,
			CurrentTurnAttachmentRef{Bucket: "chat-attachments", Name: testConversationUUID + "/photo.png"}, &budget),
	} {
		if image.refusal == "" {
			t.Fatalf("expected a refusal, got %+v", image)
		}
		if strings.Contains(image.refusal, "file-reading tool") {
			t.Fatalf("refusal offers a tool: %q", image.refusal)
		}
	}
}

func TestCurrentTurnAttachmentsProducesNothingForATurnWithNoFiles(t *testing.T) {
	attachments, err := currentTurnAttachments("ee92ccbd-3312-4c72-b20b-fddf224e7c0e", testConversationUUID, nil)
	if err != nil || attachments != nil {
		t.Fatalf("attachments=%+v err=%v", attachments, err)
	}
}

func TestCurrentApplicationTurnValidateRejectsUnstorableAttachments(t *testing.T) {
	base := CurrentApplicationTurn{
		ProjectID: 7, ActorUserID: 11, TargetParticipantID: 21,
		ApplicationID: 31, ApplicationVersionID: 41,
		ConversationUUID:  "8bc66e50-46c4-4e2c-94ec-daec6c596ac0",
		QuestionID:        "ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
		QuestionItemID:    "31df012a-300d-4722-9be2-521d987c63a8",
		ResponseMessageID: "30e0913e-10d4-43db-b8d0-c7b79480935a",
		QuestionMeta:      json.RawMessage(`{}`), UserInput: "hello",
	}
	good := CurrentTurnAttachment{
		ItemID: currentTurnUUID(base.QuestionID, "attachment-item-1"),
		Name:   testConversationUUID + "/report.pdf", Bucket: "chat-attachments",
		AttachmentType: AttachmentKindDocument,
		Content:        json.RawMessage(`[{"type":"text","text":"x"}]`),
	}
	valid := base
	valid.Attachments = []CurrentTurnAttachment{good}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid turn rejected: %v", err)
	}

	mutate := map[string]func(CurrentTurnAttachment) CurrentTurnAttachment{
		"non-uuid item id":  func(a CurrentTurnAttachment) CurrentTurnAttachment { a.ItemID = "not-a-uuid"; return a },
		"empty name":        func(a CurrentTurnAttachment) CurrentTurnAttachment { a.Name = ""; return a },
		"empty bucket":      func(a CurrentTurnAttachment) CurrentTurnAttachment { a.Bucket = ""; return a },
		"oversized name":    func(a CurrentTurnAttachment) CurrentTurnAttachment { a.Name = strings.Repeat("n", 257); return a },
		"mime as type":      func(a CurrentTurnAttachment) CurrentTurnAttachment { a.AttachmentType = "application/pdf"; return a },
		"object as content": func(a CurrentTurnAttachment) CurrentTurnAttachment { a.Content = json.RawMessage(`{}`); return a },
	}
	for name, apply := range mutate {
		turn := base
		turn.Attachments = []CurrentTurnAttachment{apply(good)}
		if err := turn.Validate(); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}

	tooMany := base
	tooMany.Attachments = make([]CurrentTurnAttachment, maxCurrentTurnAttachments+1)
	for index := range tooMany.Attachments {
		tooMany.Attachments[index] = good
	}
	if err := tooMany.Validate(); err == nil {
		t.Fatal("an unbounded attachment list was accepted")
	}
}

func TestCurrentTurnAttachmentsCloneDoesNotShareContentBacking(t *testing.T) {
	turn := &CurrentApplicationTurn{
		Attachments: []CurrentTurnAttachment{{Content: json.RawMessage(`[{"type":"text"}]`)}},
	}
	clone := turn.Clone()
	clone.Attachments[0].Content[1] = 'X'
	if turn.Attachments[0].Content[1] == 'X' {
		t.Fatal("clone shares the original's content backing array")
	}
}
