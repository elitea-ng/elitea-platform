package agentexecution

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

// #979: an attached IMAGE must reach the model as an `image_url` chunk, not as
// a filename in prose. These tests are on the SCAFFOLD, because that is where
// the decision is made and where the pre-#979 behaviour (announce the file and
// hope) was indistinguishable from success on every layer above it.

type stubAttachmentImageReader struct {
	content []byte
	err     error
	calls   []string
	maxSeen int64
}

func (reader *stubAttachmentImageReader) ReadCurrentAttachmentImage(
	_ context.Context,
	projectID int64,
	bucket string,
	name string,
	maxBytes int64,
) ([]byte, error) {
	reader.calls = append(reader.calls, bucket+"/"+name)
	reader.maxSeen = maxBytes
	_ = projectID
	if reader.err != nil {
		return nil, reader.err
	}
	return reader.content, nil
}

func imageChunksOf(t *testing.T, content json.RawMessage) []map[string]any {
	t.Helper()
	var chunks []map[string]any
	if err := json.Unmarshal(content, &chunks); err != nil {
		t.Fatalf("content=%s err=%v", content, err)
	}
	return chunks
}

func TestAttachedImageIsEmbeddedAsAnImageChunk(t *testing.T) {
	reader := &stubAttachmentImageReader{content: []byte("\x89PNG\r\n\x1a\nautotest")}
	attachments, err := currentTurnAttachmentsWithImages(
		context.Background(), 42, reader,
		"ee92ccbd-3312-4c72-b20b-fddf224e7c0e", testConversationUUID,
		[]CurrentTurnAttachmentRef{{Bucket: "chat-attachments", Name: testConversationUUID + "/shot.png"}},
	)
	if err != nil || len(attachments) != 1 {
		t.Fatalf("attachments=%+v err=%v", attachments, err)
	}
	chunks := imageChunksOf(t, attachments[0].Content)
	if len(chunks) != 2 {
		t.Fatalf("an embedded image must add a SECOND chunk beside its header: %+v", chunks)
	}
	if chunks[0]["type"] != "text" {
		t.Fatalf("the header chunk must still come first: %+v", chunks[0])
	}
	// The header of an EMBEDDED image carries no extraction marker: the marker
	// means "still needs reading", and it has been read.
	if _, marked := chunks[0][attachmentExtractionMarkerKey]; marked {
		t.Fatalf("an embedded image must not ask the worker to read it again: %+v", chunks[0])
	}
	if chunks[1]["type"] != "image_url" {
		t.Fatalf("chunk=%+v", chunks[1])
	}
	image, _ := chunks[1]["image_url"].(map[string]any)
	url, _ := image["url"].(string)
	const want = "data:image/png;base64,"
	if !strings.HasPrefix(url, want) {
		t.Fatalf("url=%q must be a data URL of the extension's OWN media type", url)
	}
	decoded, decodeErr := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, want))
	if decodeErr != nil || string(decoded) != string(reader.content) {
		t.Fatalf("the data URL must carry the object's bytes unchanged: err=%v", decodeErr)
	}
	// The read is scoped to the object the ref names, under the per-image cap.
	if len(reader.calls) != 1 || reader.calls[0] != "chat-attachments/"+testConversationUUID+"/shot.png" {
		t.Fatalf("calls=%v", reader.calls)
	}
	if reader.maxSeen != int64(maxInlineAttachmentImageBytes) {
		t.Fatalf("maxBytes=%d", reader.maxSeen)
	}
}

func TestAttachedImageMediaTypeComesFromTheExtension(t *testing.T) {
	for name, want := range map[string]string{
		"a.png": "data:image/png;base64,", "b.JPG": "data:image/jpeg;base64,",
		"c.jpeg": "data:image/jpeg;base64,", "d.gif": "data:image/gif;base64,",
		"e.webp": "data:image/webp;base64,",
	} {
		reader := &stubAttachmentImageReader{content: []byte("bytes")}
		attachments, err := currentTurnAttachmentsWithImages(
			context.Background(), 42, reader,
			"ee92ccbd-3312-4c72-b20b-fddf224e7c0e", testConversationUUID,
			[]CurrentTurnAttachmentRef{{Bucket: "chat-attachments", Name: testConversationUUID + "/" + name}},
		)
		if err != nil {
			t.Fatalf("%s: err=%v", name, err)
		}
		chunks := imageChunksOf(t, attachments[0].Content)
		if len(chunks) != 2 {
			t.Fatalf("%s: chunks=%+v", name, chunks)
		}
		image, _ := chunks[1]["image_url"].(map[string]any)
		url, _ := image["url"].(string)
		if !strings.HasPrefix(url, want) {
			t.Fatalf("%s: url=%q want prefix %q", name, url, want)
		}
	}
}

func TestAnImageFormatNoProviderTakesIsNotEmbeddedButIsRead(t *testing.T) {
	// `.bmp` is an image for classification and not one a provider is handed.
	// It must not be embedded, the model must be TOLD so, and the file must
	// take the same extraction path a document takes.
	reader := &stubAttachmentImageReader{content: []byte("BMbytes")}
	attachments, err := currentTurnAttachmentsWithImages(
		context.Background(), 42, reader,
		"ee92ccbd-3312-4c72-b20b-fddf224e7c0e", testConversationUUID,
		[]CurrentTurnAttachmentRef{{Bucket: "chat-attachments", Name: testConversationUUID + "/scan.bmp"}},
	)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if attachments[0].AttachmentType != AttachmentKindImage {
		t.Fatalf("a .bmp is still classified as an image: %q", attachments[0].AttachmentType)
	}
	chunks := imageChunksOf(t, attachments[0].Content)
	if len(chunks) != 1 {
		t.Fatalf("nothing may be embedded for a format no provider takes: %+v", chunks)
	}
	text, _ := chunks[0]["text"].(string)
	if !strings.Contains(text, "cannot be sent to the model directly") {
		t.Fatalf("the model must be told why it sees no picture: %q", text)
	}
	marker, marked := chunks[0][attachmentExtractionMarkerKey].(map[string]any)
	if !marked || marker["needs_content_extraction"] != true {
		t.Fatalf("an image that could not be embedded must still be READ: %+v", chunks[0])
	}
	if len(reader.calls) != 0 {
		t.Fatalf("a format that cannot be embedded must not be downloaded at all: %v", reader.calls)
	}
}

func TestAnUnreadableImageAnnouncesItselfAndAsksForAText(t *testing.T) {
	reader := &stubAttachmentImageReader{err: errors.New("too large")}
	attachments, err := currentTurnAttachmentsWithImages(
		context.Background(), 42, reader,
		"ee92ccbd-3312-4c72-b20b-fddf224e7c0e", testConversationUUID,
		[]CurrentTurnAttachmentRef{{Bucket: "chat-attachments", Name: testConversationUUID + "/huge.png"}},
	)
	if err != nil {
		t.Fatalf("a failed image read must never fail the turn: %v", err)
	}
	chunks := imageChunksOf(t, attachments[0].Content)
	if len(chunks) != 1 {
		t.Fatalf("chunks=%+v", chunks)
	}
	text, _ := chunks[0]["text"].(string)
	if !strings.Contains(text, "could not be embedded") {
		t.Fatalf("text=%q", text)
	}
	if _, marked := chunks[0][attachmentExtractionMarkerKey]; !marked {
		t.Fatalf("an image whose bytes could not be embedded must fall back to the read path: %+v", chunks[0])
	}
}

func TestTheTurnsImageBudgetIsSpentOnceAndThenRefused(t *testing.T) {
	// Three images at the per-image cap: the first embeds, the rest are
	// announced. The budget bounds the TURN, which is what keeps the bundle
	// under the ceiling the WORKERS fetch it with — see
	// `TestTheImageBudgetsFitTheWorkersFetchCeiling`.
	big := make([]byte, maxInlineAttachmentImageBytes)
	for index := range big {
		big[index] = byte(index % 251)
	}
	reader := &stubAttachmentImageReader{content: big}
	refs := []CurrentTurnAttachmentRef{
		{Bucket: "chat-attachments", Name: testConversationUUID + "/one.png"},
		{Bucket: "chat-attachments", Name: testConversationUUID + "/two.png"},
		{Bucket: "chat-attachments", Name: testConversationUUID + "/three.png"},
	}
	attachments, err := currentTurnAttachmentsWithImages(
		context.Background(), 42, reader,
		"ee92ccbd-3312-4c72-b20b-fddf224e7c0e", testConversationUUID, refs,
	)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	embedded := 0
	for index, attachment := range attachments {
		chunks := imageChunksOf(t, attachment.Content)
		if len(chunks) == 2 {
			embedded++
			continue
		}
		text, _ := chunks[0]["text"].(string)
		if index == 2 && !strings.Contains(text, "already use the space available for images") {
			t.Fatalf("the refused one must say WHY: %q", text)
		}
	}
	if embedded != 1 {
		t.Fatalf("the per-turn budget must bound how many images embed, got %d", embedded)
	}
}

func TestWithNoReaderAnImageBehavesExactlyAsItDidBefore(t *testing.T) {
	attachments, err := currentTurnAttachments(
		"ee92ccbd-3312-4c72-b20b-fddf224e7c0e", testConversationUUID,
		[]CurrentTurnAttachmentRef{{Bucket: "chat-attachments", Name: testConversationUUID + "/shot.png"}},
	)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	chunks := imageChunksOf(t, attachments[0].Content)
	if len(chunks) != 1 || chunks[0]["type"] != "text" {
		t.Fatalf("a deployment with no object store must embed nothing: %+v", chunks)
	}
	if _, marked := chunks[0][attachmentExtractionMarkerKey]; marked {
		t.Fatalf("and must claim nothing about reading it either: %+v", chunks[0])
	}
}

func TestEmbeddedImageChunksTravelToTheWorkerInOrder(t *testing.T) {
	// `input_attachments` is the flat concatenation the worker splices into the
	// human message. An image's two chunks must arrive adjacent and in order,
	// or the model sees a picture with no idea which file it is.
	reader := &stubAttachmentImageReader{content: []byte("bytes")}
	attachments, err := currentTurnAttachmentsWithImages(
		context.Background(), 42, reader,
		"ee92ccbd-3312-4c72-b20b-fddf224e7c0e", testConversationUUID,
		[]CurrentTurnAttachmentRef{
			{Bucket: "chat-attachments", Name: testConversationUUID + "/notes.txt"},
			{Bucket: "chat-attachments", Name: testConversationUUID + "/shot.png"},
		},
	)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	var flat []map[string]any
	if unmarshalErr := json.Unmarshal(currentTurnInputAttachments(attachments), &flat); unmarshalErr != nil {
		t.Fatalf("err=%v", unmarshalErr)
	}
	if len(flat) != 3 {
		t.Fatalf("a document header, an image header and its image: %+v", flat)
	}
	if flat[0]["type"] != "text" || flat[1]["type"] != "text" || flat[2]["type"] != "image_url" {
		t.Fatalf("order=%v/%v/%v", flat[0]["type"], flat[1]["type"], flat[2]["type"])
	}
	header, _ := flat[1]["text"].(string)
	if !strings.Contains(header, "shot.png") {
		t.Fatalf("the image's own header must precede it: %q", header)
	}
}

/* ── #984: the budgets are the WORKER's, not this service's ────────────── */

// The image caps were first sized against `MaxAgentExecutionInputBytes`
// (1 MiB), which is the frame ADMISSION refuses. Both workers fetch the bundle
// under `MaxWorkerInputBundleBytes` (256 KiB) — `RUNTIME_INPUT_CONTENT_BYTES`
// in the native worker's config.rs, `_V1_INPUT_CONTENT_BYTES` in the python
// worker's config.py — and a bundle over THAT is refused at the fetch, so the
// turn failed at the worker although this side had admitted it. One 256 KiB
// PNG is ~341 KiB of base64: over the worker's whole budget by itself.
//
// This test is the arithmetic, stated once. It fails if either cap is raised
// past what the worker will take, which is the drift that produced the defect.
func TestTheImageBudgetsFitTheWorkersFetchCeiling(t *testing.T) {
	t.Parallel()

	// base64 is 4 bytes per 3: one image at the raw cap must not encode past
	// the whole TURN's allowance.
	encodedAtCap := base64.StdEncoding.EncodedLen(maxInlineAttachmentImageBytes)
	if encodedAtCap > maxInlineAttachmentImageTurnBytes {
		t.Fatalf("one image at the raw cap encodes to %d bytes, over the turn's %d",
			encodedAtCap, maxInlineAttachmentImageTurnBytes)
	}
	// And the turn's allowance must leave room for everything else the bundle
	// carries: the four newest attachments' text is 4 x 32 KiB on its own
	// (internal/db/queries/agent_chat.sql), and the instructions, the frozen
	// version, the transcript and the user's input come after that.
	const historyAttachmentBytes = 4 * 32 * 1024
	if maxInlineAttachmentImageTurnBytes+historyAttachmentBytes >=
		executiondomain.MaxWorkerInputBundleBytes {
		t.Fatalf("images (%d) plus history attachments (%d) leave nothing under the worker's %d",
			maxInlineAttachmentImageTurnBytes, historyAttachmentBytes,
			executiondomain.MaxWorkerInputBundleBytes)
	}
}
