package agentexecution

// #606 part 2: the WRITE side of `chat_messages_attachment`.
//
// Part 1 gave the table an owner (migrations/tenant/0127) and taught
// ConversationsRepo to READ and DELETE its rows. Nothing produced them, so the
// read path had nothing to render: an upload's bytes were durable
// (internal/api/v2/conversations/attachments.go) and listed in
// `chat_conversations.meta.attachments`, but no row associated a file with the
// message it was sent with, so it never appeared inline in the transcript and
// per-message cleanup had nothing to iterate.
//
// The producer is the ADMISSION transaction, not the worker: the worker writes
// no chat rows at all, and message groups are created in exactly two places
// (InsertCurrentApplicationTurn / InsertCurrentAdhocTurn). Attaching the items
// there is what makes "the question group and its attachments exist together
// or not at all" a property of the database rather than of a retry policy.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"path"
	"strconv"
	"strings"
)

const (
	// AttachmentKindImage / AttachmentKindDocument are two of pylon's three
	// `attachment_type` values. The third, "text", is deliberately not
	// reachable here — see AttachmentKind.
	AttachmentKindImage    = "image"
	AttachmentKindDocument = "document"

	// maxCurrentTurnAttachments bounds how many item+payload row pairs one
	// admission may add to its own transaction. It is NOT a pylon rule —
	// pylon iterates `attachments_info` unbounded — it is a bound on this
	// transaction's size: the start body cap is 512 KiB
	// (internal/api/v2/agentexecution/route.go), and a filepath costs ~20
	// bytes, so an unbounded loop would admit a request asking for ~25k
	// inserts inside the same transaction that holds the runtime
	// execution/outbox rows. 64 is far above anything the client produces
	// (one entry per file the user picked in the composer) and far below the
	// point where the transaction becomes a availability problem.
	maxCurrentTurnAttachments = 64

	// varchar(256) in migrations/tenant/0127. Rejecting here rather than
	// letting Postgres raise 22001 keeps a client-supplied value from turning
	// the whole admission into a 500.
	maxAttachmentFieldBytes = 256

	// attachmentExtractionMarkerKey names the "this document still needs its
	// text extracted" marker, carried as a SIBLING KEY of the scaffold text
	// chunk:
	//
	//     {
	//       "type": "text",
	//       "text": "Bucket: ...\nFilename: ...\nfilepath: /b/n\n...",
	//       "elitea_attachment": {
	//         "needs_content_extraction": true,
	//         "bucket":   "chat-attachments",
	//         "name":     "<conversation-uuid>/report.pdf",
	//         "filepath": "/chat-attachments/<conversation-uuid>/report.pdf"
	//       }
	//     }
	//
	// The key is present ONLY on `document` chunks. An image never carries it
	// (pylon's ImageToModelProcessor sets no such flag —
	// utils/attachments.py:183-225), so "key absent" means "nothing to do",
	// which is the reading a worker gets right by default.
	//
	// WHY A MARKER EXISTS AT ALL. Pylon flags the same thing, but it never has
	// to persist the flag: DocumentToModelProcessor returns
	// `needs_content_extraction: True` alongside the content list
	// (utils/attachments.py:313-321) and rpc/chat_all.py:340-374 consumes it in
	// the SAME python process, moments later, from the object it just built.
	// Here the producer and the consumer are different services separated by a
	// database row and a runtime command, so the flag has to travel: either
	// inside `content`, or as a new column. `content` was chosen because it is
	// the only thing that reaches BOTH consumers — the worker gets the current
	// turn's chunks through InputAttachments, and a prior turn's chunks through
	// the chat-history projection, and neither carries the item's other columns.
	//
	// WHY A SIBLING OBJECT RATHER THAN A SENTINEL IN `text`. The worker needs
	// the bucket and the name to fetch the file. They are already printed in
	// `text`, but recovering them from there means parsing prose that exists to
	// be READ BY A MODEL and is expected to change wording; a nested object
	// survives any rewording, survives the `json`->`jsonb` round trip the
	// history projection performs (it is a plain object of strings and one
	// bool — no duplicate keys, no key order to preserve), and is removable
	// with one delete.
	//
	// WHY IT IS SAFE IF IT LEAKS. A chunk that reaches a model with this key
	// still IS a valid `{"type":"text","text":...}` chunk; the extra key is
	// inert metadata under an obviously-namespaced name, not an instruction, so
	// the worst case is an ignored field rather than a malformed message or a
	// prompt the model tries to obey. The worker is expected to strip the key
	// once it has acted on it.
	attachmentExtractionMarkerKey = "elitea_attachment"
)

// CurrentTurnAttachmentRef is one attachment exactly as the client addressed
// it: the (bucket, name) pair its `filepath` parsed into. The route parses;
// this package classifies, generates the item identity and builds the payload,
// so the HTTP layer never decides what goes in a column.
type CurrentTurnAttachmentRef struct {
	Bucket string
	Name   string
}

// CurrentTurnAttachment is one fully-formed `attachment_message` item: a
// chat_message_items row plus its 1:1 chat_messages_attachment payload.
//
// ItemID is caller-generated for the same reason QuestionItemID and
// ResponseMessageID are — the repository must be able to write the row without
// reading anything back to learn what it just wrote, and a retried admission
// must address the same rows.
type CurrentTurnAttachment struct {
	ItemID         string
	Name           string
	Bucket         string
	AttachmentType string
	Content        json.RawMessage
}

// ParseAttachmentFilepath splits `/{bucket}/{name}` the way pylon's
// parse_filepath does (legacy/plugins/elitea_core/utils/attachments.py:33-39):
// strip ONE leading slash, then split on the FIRST remaining slash.
//
// The consequence is load-bearing and is not a bug: the upload endpoint keys
// objects `{conversationUUID}/{sanitizedFilename}` (attachments.go's
// finalizeAttachment), so `/chat-attachments/<uuid>/report.pdf` yields bucket
// `chat-attachments` and name `<uuid>/report.pdf`. The name KEEPS the
// conversation-uuid prefix. That is what makes `name` address the stored
// object, and it is what ConversationsRepo.DeleteMessage feeds back to the
// object store, and what its ListMessageGroups reassembles into `filepath` as
// "/" + bucket + "/" + name. Trimming the prefix to a bare basename here would
// leave every attachment's bytes un-deletable and its filepath wrong.
func ParseAttachmentFilepath(filepath string) (CurrentTurnAttachmentRef, bool) {
	trimmed := strings.TrimPrefix(filepath, "/")
	bucket, name, found := strings.Cut(trimmed, "/")
	if !found || bucket == "" || name == "" ||
		len(bucket) > maxAttachmentFieldBytes || len(name) > maxAttachmentFieldBytes ||
		strings.ContainsAny(bucket, "\x00\r\n") || strings.ContainsAny(name, "\x00\r\n") {
		return CurrentTurnAttachmentRef{}, false
	}
	return CurrentTurnAttachmentRef{Bucket: bucket, Name: name}, true
}

// AttachmentKind is pylon's `attachment_type`, which is NOT a MIME type: the
// column holds one of exactly "text", "image" or "document"
// (utils/attachments.py:140-169 / :223 / :313 — the processor's own "type").
//
// "text" is unreachable from here on purpose, and that is a real difference
// from pylon rather than an omission. Pylon picks TextToModelProcessor only
// after it has already FETCHED the file's text (the processor raises without
// `text` in additional_params), i.e. the classification is a by-product of
// content extraction. This admission path extracts nothing (see
// attachmentContentScaffold), so a text/code file is classified "document" —
// the same value pylon gives it when extraction has not happened yet.
//
// The image/svg carve-out is shared with, not copied from, the upload path:
// internal/api/v2/conversations/attachments.go's isImageAttachment delegates
// here. Duplicating the rule would let the byte path's size ceiling and the
// item's attachment_type disagree about what an image is after any future
// edit to either copy.
func AttachmentKind(fileName string) string {
	if _, image := attachmentImageExtensions[strings.ToLower(path.Ext(fileName))]; image {
		return AttachmentKindImage
	}
	return AttachmentKindDocument
}

// addressableObjectKey restates the rules internal/infra/storage's validateKey
// applies, because this layer may not import infra and the two must agree: a
// name admitted here becomes an object key, and DeleteMessage later hands it to
// storage.NewObjectRef. A name that passes here and fails there is a row whose
// bytes nothing can delete — NewObjectRef errors, the cleanup skips it, and the
// route still answers 200 as though the file were gone.
//
// Only the rules that can differ are restated; length and control characters
// are already checked by ParseAttachmentFilepath.
func addressableObjectKey(key string) bool {
	if strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") ||
		strings.Contains(key, "//") {
		return false
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// attachmentImageExtensions is the explicit table this classification uses
// instead of mime.TypeByExtension.
//
// mime.TypeByExtension consults the HOST: Go seeds a small builtin table and
// then loads /etc/mime.types and friends at init. That made the answer depend
// on the image the service happens to run in — in a minimal container carrying
// no MIME database, `.bmp`, `.tiff` and `.heic` all resolved to "" and were
// classified as documents, so the scaffold asked the worker to extract TEXT
// from binary image data, and the same file classified differently on a
// developer machine that does have the file. A classification that decides
// whether a file is read as prose must not vary by base image.
//
// `.svg` is deliberately ABSENT: it is an image by MIME and a document here,
// which is pylon's own carve-out (utils/attachments.py:140-169) — it is markup,
// so its text is worth extracting and it is not something a vision model
// should be handed.
var attachmentImageExtensions = map[string]struct{}{
	".png": {}, ".jpg": {}, ".jpeg": {}, ".gif": {}, ".webp": {},
	".bmp": {}, ".tif": {}, ".tiff": {}, ".heic": {}, ".heif": {},
	".avif": {}, ".ico": {},
}

// ── #979: an attached IMAGE reaches the model as BYTES, not as a filename ────
//
// Before this, both ends of the image contract were built and each expected the
// other to fill it in. This scaffold wrote ONE text chunk for an image and set
// the extraction marker for documents only — "producing the image_url chunk
// requires reading the object, so an image gets the same single text chunk
// here" — while the worker's own contract says "the key is absent on image
// chunks, so absent means nothing to do" (elitea_worker/agents/attachments.py,
// and the identical sentence in services/elitea-worker-rust/src/agents/
// attachments.rs). Both workers VALIDATE an `image_url` chunk (`text` and
// `image_url` are the two admitted shapes on both legs). Nobody produced one,
// so a user attached a picture, watched it render in the transcript, and got an
// answer about a filename.
//
// The producer is here, because this is the only place that knows the item's
// identity and writes its `content`, and because the alternative — teaching the
// worker to fetch images — would have to be built twice (the SDK worker reads
// through the artifact toolkit, the native one through the claim-bound
// attachment route) and would put the bytes on a second network hop for every
// turn that re-sends the same conversation.
//
// THE READ IS OPTIONAL AND FAILS OPEN. A deployment with no object store
// attaches no reader (composition.go leaves the attachment routes unregistered
// there for the same reason), and a read that errors costs this attachment its
// bytes, never the turn: the file is still announced by name, which is exactly
// the behaviour this code had before.

const (
	// maxInlineAttachmentImageBytes bounds ONE image's RAW bytes.
	//
	// The ceiling that matters is the runtime input as a whole:
	// `MaxAgentExecutionInputBytes` is 1 MiB (internal/domain/execution/
	// model.go) and the input also carries the instructions, the frozen
	// version and the chat history. Base64 costs 4 bytes per 3, so 256 KiB of
	// image is ~341 KiB on the wire — one such picture fits comfortably; a
	// 1 MiB one would not, and would fail the turn rather than the file, which
	// is the failure mode this whole file exists to avoid.
	//
	// It is also well under the 150 MiB an upload accepts: a photo straight
	// from a phone is not something to put in a prompt, and the model provider
	// would refuse it separately.
	maxInlineAttachmentImageBytes = 256 * 1024

	// maxInlineAttachmentImageTurnBytes bounds what ALL of one turn's images
	// contribute, measured in the encoded (base64) bytes they actually add.
	// One attachment at the per-image cap cannot exhaust it; four can, and the
	// fifth is then announced rather than embedded — a bound on the turn, not
	// on the user's file.
	maxInlineAttachmentImageTurnBytes = 512 * 1024
)

// inlineAttachmentImageMediaTypes is the set of image types that are handed to
// a model AS AN IMAGE, and the media type each is sent with.
//
// Narrower than `attachmentImageExtensions` on purpose. That table answers
// "is this file an image" for classification; this one answers "will a provider
// take these bytes", and the answer is the four formats every multimodal
// provider documents — the same four the SDK's own `image_loaders_map` lists as
// directly LLM-supported (elitea-sdk runtime/langchain/document_loaders/
// constants.py). A `.bmp`/`.tiff`/`.heic`/`.avif`/`.ico` is still an image
// here; it simply is not one that can be embedded without a converter this
// service does not have, so it takes the extraction path below instead.
//
// The media type comes from the EXTENSION, never from the upload's recorded
// `media_type`: that value is whatever the browser put in the multipart part
// (a .png arrives as application/octet-stream from some clients), and a data
// URL that lies about its type is worse than one that is absent.
var inlineAttachmentImageMediaTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// CurrentAttachmentImageReader reads one stored chat attachment's bytes at
// ADMISSION time, inside the project the turn was authorized for.
//
// Implemented by internal/infra/db/repos.CurrentAttachmentObjectRepository —
// the same reader the native runtime's attachment route uses, which is what
// makes the two paths agree about what a chat attachment is: the bucket must be
// the reserved system one, there must be a metadata row, and the row's length
// must match the object's. A caller may not pass a project from the request;
// the admission path passes the one it resolved.
type CurrentAttachmentImageReader interface {
	ReadCurrentAttachmentImage(
		ctx context.Context,
		projectID int64,
		bucket string,
		name string,
		maxBytes int64,
	) ([]byte, error)
}

// inlineAttachmentImage is what one image attachment contributes beyond its
// header chunk: either a data URL to embed, or a sentence saying why there is
// none plus a request that the worker read the file instead.
type inlineAttachmentImage struct {
	// dataURL is `data:<media type>;base64,<bytes>` — empty when nothing is
	// embedded.
	dataURL string
	// refusal is appended to the header chunk's own text. A model that is told
	// "a picture is attached" and shown nothing answers as though it had seen
	// it; a model told the picture could not be embedded does not.
	refusal string
	// extract asks for the DOCUMENT extraction marker on the header chunk, so
	// the worker reads the file the way it reads a PDF. For an image that
	// yields the SDK loader's own description on the python leg, and the
	// honest "this runtime cannot serve it as text" on the native one — either
	// is better than a filename alone.
	extract bool
}

// inlineAttachmentImageFor reads one image attachment and decides what it
// contributes. `budget` is the turn's remaining encoded allowance and is spent
// here.
func inlineAttachmentImageFor(
	ctx context.Context,
	reader CurrentAttachmentImageReader,
	projectID int64,
	ref CurrentTurnAttachmentRef,
	budget *int,
) inlineAttachmentImage {
	if reader == nil || projectID <= 0 {
		// No reader attached: exactly the behaviour this path had before
		// #979 — the file is announced and nothing claims it was seen.
		return inlineAttachmentImage{}
	}
	mediaType, inlineable := inlineAttachmentImageMediaTypes[strings.ToLower(path.Ext(ref.Name))]
	if !inlineable {
		return inlineAttachmentImage{
			refusal: "NOTE: this image is in a format that cannot be sent to the model directly " +
				"(only PNG, JPEG, GIF and WebP are). Its contents may be read with a file-reading tool.",
			extract: true,
		}
	}
	content, err := reader.ReadCurrentAttachmentImage(
		ctx, projectID, ref.Bucket, ref.Name, int64(maxInlineAttachmentImageBytes),
	)
	if err != nil || len(content) == 0 {
		// Over the reader's own cap, absent, or a store that is having a bad
		// minute. All three are the same to the turn: no bytes, no claim, no
		// failure. The size case says so out loud, because "the model did not
		// see my screenshot" is otherwise indistinguishable from a bug.
		return inlineAttachmentImage{
			refusal: "NOTE: this image could not be embedded for the model (it is larger than " +
				strconv.Itoa(maxInlineAttachmentImageBytes/1024) +
				" KiB, or its bytes could not be read). Its contents may be read with a file-reading tool.",
			extract: true,
		}
	}
	encoded := base64.StdEncoding.EncodeToString(content)
	cost := len(encoded)
	if budget != nil {
		if cost > *budget {
			return inlineAttachmentImage{
				refusal: "NOTE: this image was not embedded because the turn's other attachments " +
					"already use the space available for images. Its contents may be read with a " +
					"file-reading tool.",
				extract: true,
			}
		}
		*budget -= cost
	}
	return inlineAttachmentImage{dataURL: "data:" + mediaType + ";base64," + encoded}
}

// attachmentImageChunk is the `{"type":"image_url"}` chunk both workers already
// admit (`_ADMITTED_CHUNK_TYPES` / the `image_url` arm of
// `validate_input_attachments`), in pylon's own ImageToModelProcessor shape
// (utils/attachments.py:183-225).
//
// It carries NO marker: the marker means "this file still needs reading", and
// an embedded image has been read.
func attachmentImageChunk(dataURL string) map[string]any {
	return map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": dataURL},
	}
}

// attachmentContentScaffold is the creation-time `content` chunk, in pylon's
// shape (utils/attachments.py:288-320, DocumentToModelProcessor.process): a
// JSON ARRAY of chunks, of which exactly one exists at creation time, naming
// the bucket, the filename and the filepath.
//
// WHAT IS DELIBERATELY MISSING, so that nothing downstream mistakes this for a
// complete payload:
//
//   - The extracted document TEXT. Pylon appends it as a SECOND chunk from a
//     separate processing step that runs after the items are created
//     (rpc/chat_all.py:369-377), reading each file through the SDK. That step
//     runs in the WORKER, not here: an admission transaction must not perform
//     IO against object storage while holding the runtime execution rows.
//     Which chunks it must do that for is not something the worker can infer
//     from the text, so the scaffold says so explicitly — see
//     attachmentExtractionMarkerKey below.
//   - For an image, pylon's ImageToModelProcessor also emits an `image_url`
//     chunk (utils/attachments.py:183-225) carrying base64 bytes. Producing it
//     requires reading the object, so an image gets the same single text chunk
//     here.
//
// The scaffold is NOT an empty array. An empty array is what a caller writes
// when it has computed the content and found none; this is a partial payload
// that names where the file lives, and a later extraction step appending to it
// is exactly what pylon does.
func attachmentContentScaffold(
	ref CurrentTurnAttachmentRef,
	kind, itemID string,
	image inlineAttachmentImage,
) json.RawMessage {
	filepath := "/" + ref.Bucket + "/" + ref.Name
	lines := []string{
		"Bucket: " + ref.Bucket,
		"Filename: " + ref.Name,
		"filepath: " + filepath,
		"",
		"NOTE: File content may be EMBEDDED in the next message chunk.",
		"If embedded content is provided below, please review it first - the full text is already included.",
		"File reading tools are available if needed for specific operations (search, partial access), but prefer embedded content when available.",
	}
	// An image that could NOT be embedded says so here. Without this line the
	// model is told a picture is attached, shown nothing, and answers as
	// though it had seen it (#979).
	if image.refusal != "" {
		lines = append(lines, "", image.refusal)
	}
	chunk := map[string]any{"type": "text", "text": strings.Join(lines, "\n")}
	// The marker rides a document's header chunk, and an IMAGE that could not
	// be embedded takes the same path deliberately: "read this file for me" is
	// exactly what the worker's extraction step does, and for an image that is
	// the SDK loader's own description rather than nothing at all. An EMBEDDED
	// image never carries it — the marker means "still needs reading", and it
	// has been read.
	if kind == AttachmentKindDocument || image.extract {
		chunk[attachmentExtractionMarkerKey] = map[string]any{
			"needs_content_extraction": true,
			"bucket":                   ref.Bucket,
			"name":                     ref.Name,
			"filepath":                 filepath,
			// The item's own id, so the worker can name EXACTLY the row whose
			// content it enriched when it reports the text back (#607). The
			// alternative — matching on (bucket, name) — is ambiguous the
			// moment the same file is attached twice in one conversation,
			// which is an ordinary thing for a user to do and would silently
			// write one file's text onto another's row.
			"item_id": itemID,
		}
	}
	chunks := []map[string]any{chunk}
	if image.dataURL != "" {
		chunks = append(chunks, attachmentImageChunk(image.dataURL))
	}
	encoded, err := json.Marshal(chunks)
	if err != nil {
		// Unreachable: the value is a map of finite strings and bools.
		return json.RawMessage(`[]`)
	}
	return encoded
}

// currentTurnAttachments turns the client's refs into the items the admission
// transaction writes.
//
// Item ids are derived from the question id with the same namespaced-SHA1
// helper the question and response ids already use, rather than being random:
// a retried start for the same question_id must address the same rows, so the
// identity of an attachment item is a function of the turn, not of when the
// retry happened.
// currentTurnAttachments builds this turn's attachment items, refusing any
// reference that does not belong to conversationUUID.
//
// THE CONVERSATION PREFIX IS AN AUTHORISATION CHECK, not tidiness. `filepath`
// arrives from the browser in `payload.attachments`, and until this check the
// only validation was shape: a caller could name ANY object in any bucket it
// could spell — another user's conversation key is just
// `{other-conversation-uuid}/file.pdf` — and the worker would read those bytes
// through the artifact toolkit and splice them into the model's context, in a
// conversation the caller was never granted.
//
// The upload endpoint is the only thing that writes these objects and it keys
// every one of them `{conversationID}/{sanitised name}`
// (api/v2/conversations/attachments.go, finalizeAttachment), so requiring that
// prefix is exactly "this file was uploaded to this conversation". Anything
// else was not put there by this conversation's upload path and has no business
// being read into it.
func currentTurnAttachments(
	questionID string,
	conversationUUID string,
	refs []CurrentTurnAttachmentRef,
) ([]CurrentTurnAttachment, error) {
	return currentTurnAttachmentsWithImages(context.Background(), 0, nil, questionID, conversationUUID, refs)
}

// currentTurnAttachmentsWithImages is the same builder with the image reader
// attached (#979). The three-argument form above is what the callers that have
// no reader — and every test that predates this — keep using, and it produces
// exactly what it always did: a header chunk per file and nothing embedded.
func currentTurnAttachmentsWithImages(
	ctx context.Context,
	projectID int64,
	images CurrentAttachmentImageReader,
	questionID string,
	conversationUUID string,
	refs []CurrentTurnAttachmentRef,
) ([]CurrentTurnAttachment, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	imageBudget := maxInlineAttachmentImageTurnBytes
	if len(refs) > maxCurrentTurnAttachments {
		return nil, ErrInvalidCurrentAgentStart
	}
	attachments := make([]CurrentTurnAttachment, 0, len(refs))
	for index, ref := range refs {
		if ref.Bucket == "" || ref.Name == "" ||
			len(ref.Bucket) > maxAttachmentFieldBytes || len(ref.Name) > maxAttachmentFieldBytes {
			return nil, ErrInvalidCurrentAgentStart
		}
		if !validUUID(conversationUUID) ||
			!strings.HasPrefix(ref.Name, conversationUUID+"/") {
			return nil, ErrInvalidCurrentAgentStart
		}
		// The name is also the object key and the value DeleteMessage hands to
		// storage.NewObjectRef. Admitting one that key validation would reject
		// (`..`, `//`, a trailing slash) stored a row whose bytes the delete
		// path could never address: it fails NewObjectRef, skips, and answers
		// 200 as though the file had been removed.
		if !addressableObjectKey(ref.Name) {
			return nil, ErrInvalidCurrentAgentStart
		}
		kind := AttachmentKind(ref.Name)
		itemID := currentTurnUUID(questionID, "attachment-item-"+strconv.Itoa(index+1))
		var image inlineAttachmentImage
		if kind == AttachmentKindImage {
			image = inlineAttachmentImageFor(ctx, images, projectID, ref, &imageBudget)
		}
		attachments = append(attachments, CurrentTurnAttachment{
			ItemID:         itemID,
			Name:           ref.Name,
			Bucket:         ref.Bucket,
			AttachmentType: kind,
			Content:        attachmentContentScaffold(ref, kind, itemID, image),
		})
	}
	return attachments, nil
}

// currentTurnInputAttachments is the runtime input's `input_attachments`: the
// CONCATENATION of every attachment's `content` chunks, in item order.
//
// WHY CONCATENATED AND FLAT, rather than one entry per file. This value has
// exactly one consumer — the worker splices it into the human message's
// multimodal content list — and one meaning to match: pylon builds the current
// turn's message the same way its chat-history projection builds a prior turn's
// (utils/chat_history.py:67-73 EXTENDS an attachment item's content list into
// the group's content array). Nesting per-file would make the current turn and
// every earlier turn two different shapes for the same thing, and the model
// sees them side by side in one request.
//
// It is built from the CurrentTurnAttachment values the caller already holds —
// the same slice the admission transaction is about to write — rather than
// re-derived from the refs or read back after the insert. Re-deriving would let
// what was sent to the worker and what was stored drift apart silently; reading
// back would be a query for bytes already in memory, inside the start path.
//
// `[]` for no attachments: an empty list is what the worker's protocol has
// always been handed here, and it is what "this turn attached nothing" means.
func currentTurnInputAttachments(attachments []CurrentTurnAttachment) []byte {
	chunks := []json.RawMessage{}
	for _, attachment := range attachments {
		var attachmentChunks []json.RawMessage
		if err := json.Unmarshal(attachment.Content, &attachmentChunks); err != nil {
			// Not reachable through currentTurnAttachments, whose content is
			// always a marshalled array; skipping rather than failing keeps a
			// malformed payload from refusing a turn whose TEXT is fine.
			continue
		}
		chunks = append(chunks, attachmentChunks...)
	}
	encoded, err := json.Marshal(chunks)
	if err != nil {
		// Unreachable: every element is already valid JSON.
		return []byte(`[]`)
	}
	return encoded
}

// validCurrentTurnAttachments is the Validate() half, re-checked on the struct
// the repository is actually handed rather than on the request that produced
// it — every other field of a turn is validated the same way.
func validCurrentTurnAttachments(attachments []CurrentTurnAttachment) bool {
	if len(attachments) > maxCurrentTurnAttachments {
		return false
	}
	for _, attachment := range attachments {
		if !validUUID(attachment.ItemID) ||
			!validCurrentAgentText(attachment.Name, maxAttachmentFieldBytes) ||
			!validCurrentAgentText(attachment.Bucket, maxAttachmentFieldBytes) ||
			(attachment.AttachmentType != AttachmentKindImage &&
				attachment.AttachmentType != AttachmentKindDocument) ||
			!validJSONArray(attachment.Content) {
			return false
		}
	}
	return true
}

func cloneCurrentTurnAttachments(attachments []CurrentTurnAttachment) []CurrentTurnAttachment {
	if attachments == nil {
		return nil
	}
	clone := make([]CurrentTurnAttachment, len(attachments))
	for index, attachment := range attachments {
		clone[index] = attachment
		clone[index].Content = bytes.Clone(attachment.Content)
	}
	return clone
}
