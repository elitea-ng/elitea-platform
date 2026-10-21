package conversations

// S20a: chat attachment byte path. Legacy's chunked-upload contract
// (legacy/plugins/elitea_core/api/v2/attachments.py, utils/attachments.py) —
// form fields file_id/chunk_index (0-based)/total_chunks/file_name, one
// chunk per POST, disk-buffered under a per-file_id temp directory, "all
// chunks received" detected by counting files on disk, merged in
// chunk_index order once complete — is ported here with Postgres standing
// in for local disk (see migrations/shared/0059_attachment_chunks.sql):
// this file's own ADR-0016 standing constraint rules out any-replica local
// buffering, since chunk N and chunk N+1 of the same upload can land on two
// different pods with no sticky session.
//
// No claim/lock guards the merge step deliberately: if two concurrent
// requests both observe "all chunks present" (the last two chunks of one
// upload arriving on two different replicas within the same race window),
// both independently read the same committed chunk rows, both merge, and
// each reserves its own key (#1059: the second gets `name (1).ext`), Puts
// the identical bytes there, and calls DeleteChunks (idempotent — the second
// delete just removes zero rows). Each answer names a complete copy; the copy
// the client does not send is an unreferenced object the retention sweeper
// expires. Redundant work on that rare race, never corrupted output — the
// same trade-off S14's sweeper already accepts for its own duplicate-tick
// idempotency.
//
// Bytes are written through the S6/S9 metadata tables (a reserved
// project-scoped "system" bucket, elitea_storage.buckets/objects), not a
// bare storage.ObjectStore.Put with no metadata row — this is a deliberate
// choice over the simpler bypass: it makes S14's retention sweeper apply to
// attachments (legacy's chat_bucket_retention_days, 365 days by default,
// otherwise has no Go-side equivalent at all) and folds attachment bytes
// into the same SumProjectBytes aggregate every other artifact write
// already contributes to.
//
// The legacy chat_messages_attachment row this file used to leave unwritten
// IS now written, so the open question recorded here through part 1 of #606 is
// answered: Go took migration ownership of the table
// (migrations/tenant/0127_chat_message_attachment_items.sql, which records why
// the collision risk with pylon's own Alembic history is addressable), and the
// row itself is written by the ADMISSION transaction, not by this upload
// handler — internal/infra/db/repos/agent_execution_jobs.go's
// insertCurrentTurnAttachments, from `payload.attachments` on the start route.
// It is written there and not here because an attachment must hang off the
// message group it was SENT with, and this handler runs while the user is
// still composing: at upload time there is no message group to attach to yet.
//
// What that leaves this file responsible for is unchanged — the bytes, the
// elitea_storage.buckets/objects metadata, and the
// `{conversationUUID}/{sanitizedFilename}` object key (numbered when the name
// is taken, #1059). That key is load
// bearing on the other side: the client sends the filepath this endpoint
// returned, the admission splits it on the first slash after the bucket, and
// the remainder becomes the item's `name` — which is what
// ConversationsRepo.DeleteMessage feeds back to the object store when a
// message is deleted with ?delete_attachment.
//
// STILL NOT PORTED, and not by this change: the extracted document text pylon
// appends to the item's `content` from a separate processing step, and image
// thumbnailing. Those read the object through the SDK and belong to the
// worker/SDK port; the admission writes only the creation-time scaffold chunk
// (see agentexecutionapp.attachmentContentScaffold).

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// Legacy defaults (vault secrets chat_max_upload_size_mb /
// chat_max_file_upload_size_mb / chat_max_image_upload_size_mb /
// chat_bucket_retention_days) — this service has no per-project vault-secret
// policy mechanism anywhere else, so these are env-overridable constants
// instead, matching objects.go's ARTIFACT_MAX_OBJECT_BYTES precedent exactly
// rather than inventing a new per-project override path for this one stage.
const (
	defaultAttachmentMaxTotalMB = 150
	defaultAttachmentMaxFileMB  = 150
	defaultAttachmentMaxImageMB = 3
	defaultAttachmentRetention  = 365
	attachmentMaxChunkBytes     = 5 << 20 // wire contract: 5 MiB per chunk
	attachmentMultipartMemLimit = 8 << 20 // in-memory part threshold before ParseMultipartForm spills to temp files
)

// fileIDPattern matches legacy's ChunkUploadPayload.file_id validation — it
// is joined into a storage key, so the same traversal-guard shape legacy
// itself enforces before ever touching a filesystem path.
var fileIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// AttachmentChunk is one previously-received chunk, read back in
// chunk_index order for the merge step.
type AttachmentChunk struct {
	ChunkIndex int32
	Bytes      []byte
}

// AttachmentStore is the S6/S9/S20a Postgres metadata this handler needs,
// expressed without importing internal/infra/db/repos directly: that
// package's own conversations.go already imports this package (to satisfy
// the Repository interface above with wire-shape DTOs like Conversation/
// ListResponse), so importing it back here would be an import cycle.
// router.go, which imports both packages freely, supplies the concrete
// adapter wrapping the real repos.Artifact{Buckets,Objects}Repository and
// repos.AttachmentChunksRepository.
type AttachmentStore interface {
	// AttachmentPolicy resolves the elitea_storage.project_storage_policy
	// fields this stage's limits can legitimately be sourced from for
	// projectID: attachment_bucket (bucketName, "" when unset — the caller
	// falls back to storage.DefaultAttachmentBucket), max_object_bytes
	// (maxFileBytes, nil when unset — matches objects.go's own
	// policy-first-env-fallback-second precedent for the identical concept,
	// "cap on one uploaded object"), and retention_default_days
	// (retentionDays, nil when unset). There is no policy field for
	// "max bytes across every file in one upload call" or "per-image cap"
	// specifically — those two stay env/const-only; see
	// attachmentMaxTotalBytes/attachmentMaxImageBytes's own doc comments.
	AttachmentPolicy(ctx context.Context, projectID int64) (bucketName string, maxFileBytes *int64, retentionDays *int32, err error)
	// RequireAttachmentBucket returns the reserved system bucket's database
	// ID, creating the bucket row on first use. It returns no deadline: the
	// bucket's expires_at is one frozen instant, so an attachment that
	// copied it was born expired after the project's first attachment aged
	// past retention_days. Each attachment carries its own deadline
	// instead — see the RecordAttachmentObject call below.
	RequireAttachmentBucket(ctx context.Context, projectID int64, bucketName string, retentionDays int32) (bucketID int64, err error)
	// ReserveAttachmentObject claims key in the bucket for an upload whose
	// bytes are not written yet: it inserts a placeholder metadata row unless
	// a row already holds the key, and reports whether this caller got it.
	// The unique (bucket_id, key) index is the arbiter, so two concurrent
	// uploads never both get one key (#1059). RecordAttachmentObject then
	// overwrites the placeholder with the stored object's size and type.
	ReserveAttachmentObject(ctx context.Context, bucketID int64, key string, mediaType string, expiresAt *time.Time) (bool, error)
	RecordAttachmentObject(ctx context.Context, bucketID int64, key string, byteLength int64, mediaType string, expiresAt *time.Time) error

	// LookupAttachmentBucket resolves the same reserved system bucket
	// RequireAttachmentBucket returns, but LOOKUP ONLY: it returns
	// storage.ErrNotFound rather than creating the row. DeleteAttachments is
	// its only caller, and a delete that creates a bucket as a side effect
	// would leave a project that never uploaded anything owning a fresh,
	// empty, retention-stamped bucket row — the create belongs to the upload
	// path alone. Not found simply means nothing was ever stored, which is
	// not an error for a delete.
	LookupAttachmentBucket(ctx context.Context, projectID int64, bucketName string) (bucketID int64, err error)
	// ListAttachmentObjectKeys returns the keys of the elitea_storage.objects
	// metadata rows under keyPrefix in one bucket. DeleteAttachments derives
	// its delete set from these ROWS, never from an ObjectStore.List of the
	// bucket: the bucket is shared by every conversation in the project, and
	// an object in it with no metadata row was not written by
	// finalizeAttachment, so it is not this route's to delete.
	ListAttachmentObjectKeys(ctx context.Context, bucketID int64, keyPrefix string) ([]string, error)
	// DeleteAttachmentObjects removes the metadata rows naming keys. Bytes
	// are the caller's job and must go FIRST — see DeleteAttachments.
	DeleteAttachmentObjects(ctx context.Context, bucketID int64, keys []string) error
	// AttachmentObject reads the metadata row of one stored attachment:
	// the bucket is the project's reserved SYSTEM bucket of that name, and
	// a row exists for exactly key. Anything else is storage.ErrNotFound —
	// the same three gates the runtime's attachment reader applies
	// (repos.CurrentAttachmentObjectRepository), so a download and a turn
	// agree about which object a name addresses. DownloadAttachment is its
	// only caller.
	AttachmentObject(ctx context.Context, projectID int64, bucketName, key string) (AttachmentObjectInfo, error)

	UpsertAttachmentChunk(ctx context.Context, projectID int64, conversationID, fileID string, chunkIndex, totalChunks int32, fileName, contentType string, body []byte) error
	CountAttachmentChunks(ctx context.Context, projectID int64, conversationID, fileID string) (int64, error)
	ListAttachmentChunksOrdered(ctx context.Context, projectID int64, conversationID, fileID string) ([]AttachmentChunk, error)
	DeleteAttachmentChunks(ctx context.Context, projectID int64, conversationID, fileID string) error
}

func attachmentMaxTotalBytes() int64 {
	return attachmentEnvMB("ARTIFACT_ATTACHMENT_MAX_TOTAL_MB", defaultAttachmentMaxTotalMB)
}
func attachmentMaxFileBytes() int64 {
	return attachmentEnvMB("ARTIFACT_ATTACHMENT_MAX_FILE_MB", defaultAttachmentMaxFileMB)
}
func attachmentMaxImageBytes() int64 {
	return attachmentEnvMB("ARTIFACT_ATTACHMENT_MAX_IMAGE_MB", defaultAttachmentMaxImageMB)
}

func attachmentRetentionDays() int32 {
	if raw := os.Getenv("ARTIFACT_ATTACHMENT_RETENTION_DAYS"); raw != "" {
		// v > math.MaxInt32 rejected explicitly, not left to int32(v)'s own
		// silent wraparound — strconv.Atoi's int is architecture-dependent
		// (64-bit on every platform this service actually runs on), so an
		// operator-misconfigured value here would otherwise wrap to an
		// unrelated, possibly negative int32 (CodeQL go/incorrect-integer-conversion).
		if v, err := strconv.Atoi(raw); err == nil && v > 0 && v <= math.MaxInt32 {
			return int32(v)
		}
	}
	return defaultAttachmentRetention
}

func attachmentEnvMB(envVar string, defaultMB int64) int64 {
	if raw := os.Getenv(envVar); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil && v > 0 {
			return v << 20
		}
	}
	return defaultMB << 20
}

// maxAttachmentChunks bounds total_chunks — see its call site's own comment.
// Derived from the total-upload limit divided by the per-chunk limit
// (rounded up), doubled for slack: a caller splitting into smaller chunks
// than the maximum is normal, not abuse, so a hard "exactly ceil(total/chunk)"
// ceiling would reject legitimate smaller-chunked uploads under the same
// total-byte budget.
func maxAttachmentChunks() int {
	n := attachmentMaxTotalBytes() / attachmentMaxChunkBytes
	if attachmentMaxTotalBytes()%attachmentMaxChunkBytes != 0 {
		n++
	}
	return int(n) * 2
}

// attachmentCreated is legacy's AttachmentMessageItemCreated wire shape —
// preserved for whatever caller still expects it, even though the
// underlying metadata store below is now elitea_storage.*, not
// chat_messages_attachment.
type attachmentCreated struct {
	Filepath string `json:"filepath"`
	FileSize int64  `json:"file_size"`
}

// isImageAttachment mirrors process_uploaded_files' own is_image check:
// image/* by extension, except .svg (kept on the regular file limit,
// matching legacy exactly).
//
// #606 made this rule load bearing in a SECOND place — an attachment item's
// `attachment_type` column is "image" or "document" by exactly this test — so
// it moved to agentexecutionapp.AttachmentKind and this stays a thin
// delegation. Two copies would let the size ceiling this function gates and
// the classification stored on the message disagree about what an image is
// after any future edit to either copy, and nothing would fail until a user
// uploaded the one extension that had drifted.
func isImageAttachment(fileName string) bool {
	return agentexecutionapp.AttachmentKind(fileName) == agentexecutionapp.AttachmentKindImage
}

// sanitizeAttachmentFilename strips any directory component and control
// characters a client-supplied file_name/filename could carry — the actual
// traversal guard is storage.NewObjectRef's own validateKey (S1), this just
// keeps an otherwise-legitimate filename with e.g. a leading path from
// being rejected outright.
func sanitizeAttachmentFilename(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	cleaned := b.String()
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return "attachment"
	}
	return cleaned
}

// writeAttachmentBytes is AddAttachments' multipart/form-data branch (S20a).
func (h *Handler) writeAttachmentBytes(w http.ResponseWriter, r *http.Request) {
	if h.store == nil || h.attachments == nil {
		apierr.Write(w, apierr.Internal("attachment storage is not configured"))
		return
	}

	projectIDStr := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")
	projectID, err := strconv.ParseInt(projectIDStr, 10, 64)
	if err != nil || projectID <= 0 {
		apierr.Write(w, apierr.BadRequest("invalid project id"))
		return
	}

	// THE CONVERSATION UUID, NOT ITS NUMERIC ID — and refused here rather than
	// stored wrong.
	//
	// finalizeAttachment keys every object `{this parameter}/{filename}`, and
	// that key is an AUTHORISATION boundary, not a filing convention: admission
	// refuses any attachment reference whose name is not prefixed by the
	// conversation's UUID (internal/application/agentexecution/attachments.go,
	// currentTurnAttachments — "this file was uploaded to this conversation").
	// So an object keyed by the numeric id can never ride a turn. Before this
	// guard the mismatch was silent and expensive: the upload answered 201 with
	// a filepath, the very next start was refused 400 BEFORE admissions.Submit
	// ran, and the user lost the question while the bytes sat in the bucket
	// until retention expired them (the composer really did this — see
	// resolveUploadConversationId in
	// apps/elitea-web/src/widgets/chat-box/ui/hooks/useChatBoxHandlers.helpers.ts).
	//
	// A DECIMAL INTEGER IS THE WHOLE TEST, deliberately, rather than "must
	// parse as a UUID". This route is the only writer of these keys but it is
	// not the only reader of the parameter — DeleteAttachments derives a key
	// PREFIX from the same value — and a stricter shape check here would refuse
	// identifiers that are wrong in some other way with a message about UUIDs
	// that may not be true. A numeric id is the one wrong value a client
	// plausibly holds and sends, because it is the identifier every sibling
	// conversation route accepts.
	if _, numeric := strconv.ParseUint(conversationID, 10, 64); numeric == nil {
		apierr.Write(w, apierr.BadRequest("conversation id must be the conversation uuid: a chat attachment is stored as {conversationUUID}/{filename}, and an object keyed by the numeric id cannot be attached to a message"))
		return
	}

	// A single request body can carry at most one chunk (bounded at
	// attachmentMaxChunkBytes) or one whole non-chunked file (bounded at
	// attachmentMaxFileBytes) — the larger of the two, plus slack for
	// multipart field/boundary overhead, bounds the request the same way
	// S12 bounds every other upload path (objects.go).
	limit := attachmentMaxFileBytes()
	if attachmentMaxChunkBytes > limit {
		limit = attachmentMaxChunkBytes
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit+1<<20)

	if err := r.ParseMultipartForm(attachmentMultipartMemLimit); err != nil {
		apierr.Write(w, apierr.BadRequest("request is not multipart/form-data or exceeds the upload limit"))
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, header, err := r.FormFile("file")
	if err != nil {
		apierr.Write(w, apierr.BadRequest("No chunk file provided"))
		return
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(file)
	if err != nil {
		apierr.Write(w, apierr.BadRequest("read uploaded file: "+err.Error()))
		return
	}

	fileID := r.FormValue("file_id")
	chunkIndexStr := r.FormValue("chunk_index")
	totalChunksStr := r.FormValue("total_chunks")
	fileName := r.FormValue("file_name")
	if fileName == "" {
		fileName = header.Filename
	}
	contentType := header.Header.Get("Content-Type")

	// Chunked iff all three chunk-identifying fields are present — mirrors
	// handle_chunked_upload's own dispatch (`all([file_id, chunk_index is
	// not None, total_chunks, file_name])`).
	if fileID == "" || chunkIndexStr == "" || totalChunksStr == "" {
		h.finalizeAttachment(w, r.Context(), projectID, projectIDStr, conversationID, fileName, contentType, content)
		return
	}

	if !fileIDPattern.MatchString(fileID) {
		apierr.Write(w, apierr.BadRequest(fmt.Sprintf("Invalid chunk parameters: file_id must match %s", fileIDPattern.String())))
		return
	}
	chunkIndex, err := strconv.Atoi(chunkIndexStr)
	// chunkIndex/totalChunks are int (architecture-dependent, 64-bit here)
	// but stored — and used in the int32(...) call below — as int32;
	// math.MaxInt32 is checked directly here rather than relying on the
	// transitive chunkIndex < totalChunks <= maxAttachmentChunks() bound a
	// few lines down to keep both safe, so this stays correct even if
	// those other checks are ever reordered or changed (CodeQL
	// go/incorrect-integer-conversion).
	if err != nil || chunkIndex < 0 || chunkIndex > math.MaxInt32 {
		apierr.Write(w, apierr.BadRequest("Invalid chunk parameters: chunk_index must be a non-negative integer"))
		return
	}
	totalChunks, err := strconv.Atoi(totalChunksStr)
	if err != nil || totalChunks <= 0 || totalChunks > math.MaxInt32 {
		apierr.Write(w, apierr.BadRequest("Invalid chunk parameters: total_chunks must be a positive integer"))
		return
	}
	// An adversarial-review finding confirmed that without this cap,
	// total_chunks was unbounded: a caller could declare an arbitrarily
	// large total_chunks and drip-feed distinct chunk_index values forever,
	// accumulating unbounded BYTEA rows in elitea_storage.attachment_chunks
	// (never reached by S12's SumProjectBytes quota, which only sums
	// elitea_storage.objects) — and, separately, a real total_chunks near
	// that size would make the merge step's buffer pre-allocation attempt a
	// multi-gigabyte allocation from a modest real payload. Capped at the
	// number of max-size chunks the total-upload limit could ever
	// legitimately require, with a little slack for a caller that
	// (reasonably) split into smaller-than-maximum chunks.
	if totalChunks > maxAttachmentChunks() {
		apierr.Write(w, apierr.BadRequest(fmt.Sprintf("Invalid chunk parameters: total_chunks exceeds the maximum of %d", maxAttachmentChunks())))
		return
	}
	if chunkIndex >= totalChunks {
		apierr.Write(w, apierr.BadRequest("Invalid chunk parameters: chunk_index must be less than total_chunks"))
		return
	}
	if len(content) > attachmentMaxChunkBytes {
		apierr.Write(w, apierr.BadRequest(fmt.Sprintf("Chunk exceeds the %d MB limit", attachmentMaxChunkBytes>>20)))
		return
	}

	if err := h.attachments.UpsertAttachmentChunk(r.Context(), projectID, conversationID, fileID, int32(chunkIndex), int32(totalChunks), fileName, contentType, content); err != nil {
		apierr.Write(w, apierr.Internal(err.Error()))
		return
	}

	received, err := h.attachments.CountAttachmentChunks(r.Context(), projectID, conversationID, fileID)
	if err != nil {
		apierr.Write(w, apierr.Internal(err.Error()))
		return
	}
	if received < int64(totalChunks) {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"status":       "chunk_received",
			"file_id":      fileID,
			"chunk_index":  chunkIndex,
			"total_chunks": totalChunks,
			"message":      fmt.Sprintf("Chunk %d/%d received", chunkIndex+1, totalChunks),
		})
		return
	}

	chunks, err := h.attachments.ListAttachmentChunksOrdered(r.Context(), projectID, conversationID, fileID)
	if err != nil {
		apierr.Write(w, apierr.Internal(err.Error()))
		return
	}
	// Pre-allocate from the chunks' own actual byte lengths, not
	// len(chunks)*attachmentMaxChunkBytes — an adversarial-review finding
	// confirmed the latter could request a multi-gigabyte capacity from a
	// real payload of a few KB (every chunk near-empty, total_chunks large)
	// even with maxAttachmentChunks() capping the chunk *count*, since
	// nothing bounds how small an individual chunk's real byte length is.
	var mergedSize int
	for _, c := range chunks {
		mergedSize += len(c.Bytes)
	}
	merged := make([]byte, 0, mergedSize)
	for _, c := range chunks {
		merged = append(merged, c.Bytes...)
	}

	h.finalizeAttachment(w, r.Context(), projectID, projectIDStr, conversationID, fileName, contentType, merged)

	// Always runs, success or failure — matches legacy's own `finally:
	// cleanup_chunks(...)`. A failed delete just leaves harmless,
	// self-healing rows (DeleteAttachmentChunks' own doc comment).
	_ = h.attachments.DeleteAttachmentChunks(r.Context(), projectID, conversationID, fileID)
}

// attachmentNameRetries bounds the inserts one upload makes after the
// first numbered name it picked was taken by a concurrent upload of the
// same name, first counting up from that number and then with a random
// suffix. A busy name never answers 409.
const attachmentNameRetries = 4

// reserveAttachmentKey claims a free `{conversationID}/{name}` key and returns
// it with its object ref: the name itself when it is free, otherwise the
// next number after the highest `name (N).ext` the conversation holds, read
// in one query rather than probed name by name (a phone names every photo
// image.jpg, and a long conversation collects hundreds). Concurrent uploads
// of one name race on the unique (bucket_id, key) index, so a lost insert
// moves on to the next number and finally to a random suffix.
func (h *Handler) reserveAttachmentKey(ctx context.Context, bucketID int64, projectIDStr, bucketName, conversationID, name, contentType string, expiresAt *time.Time) (string, storage.ObjectRef, error) {
	try := func(candidate string) (string, storage.ObjectRef, bool, error) {
		key := conversationID + "/" + candidate
		ref, err := storage.NewObjectRef(projectIDStr, bucketName, key)
		if err != nil {
			return "", storage.ObjectRef{}, false, apierr.BadRequest("invalid attachment file name: " + err.Error())
		}
		reserved, err := h.attachments.ReserveAttachmentObject(ctx, bucketID, key, contentType, expiresAt)
		if err != nil {
			return "", storage.ObjectRef{}, false, apierr.Internal("reserve attachment name: " + err.Error())
		}
		return key, ref, reserved, nil
	}

	key, ref, reserved, err := try(name)
	if err != nil || reserved {
		return key, ref, err
	}

	stem, ext := splitAttachmentName(name)
	held, err := h.attachments.ListAttachmentObjectKeys(ctx, bucketID, conversationID+"/"+stem+" (")
	if err != nil {
		return "", storage.ObjectRef{}, apierr.Internal("list attachment names: " + err.Error())
	}
	next := 1
	for _, k := range held {
		if n, ok := attachmentNameNumber(strings.TrimPrefix(k, conversationID+"/"), stem, ext); ok && n >= next {
			next = n + 1
		}
	}
	for attempt := 0; attempt < attachmentNameRetries; attempt++ {
		if key, ref, reserved, err = try(numberedAttachmentName(name, next+attempt)); err != nil || reserved {
			return key, ref, err
		}
	}
	for attempt := 0; attempt < attachmentNameRetries; attempt++ {
		suffix := make([]byte, 4)
		if _, err := rand.Read(suffix); err != nil {
			return "", storage.ObjectRef{}, apierr.Internal("reserve attachment name: " + err.Error())
		}
		if key, ref, reserved, err = try(fmt.Sprintf("%s (%s)%s", stem, hex.EncodeToString(suffix), ext)); err != nil || reserved {
			return key, ref, err
		}
	}
	return "", storage.ObjectRef{}, apierr.Internal(fmt.Sprintf("could not reserve a name for %q: every candidate was taken concurrently", name))
}

// splitAttachmentName splits name into the stem a number follows and the
// extension it goes before. A name that is only an extension (`.env`) is
// all stem.
func splitAttachmentName(name string) (stem, ext string) {
	ext = path.Ext(name)
	stem = strings.TrimSuffix(name, ext)
	if stem == "" {
		return name, ""
	}
	return stem, ext
}

// attachmentNameNumber reports N when candidate is exactly `stem (N)ext`
// for a positive decimal N.
func attachmentNameNumber(candidate, stem, ext string) (int, bool) {
	digits, ok := strings.CutPrefix(candidate, stem+" (")
	if !ok {
		return 0, false
	}
	digits, ok = strings.CutSuffix(digits, ")"+ext)
	if !ok || digits == "" || len(digits) > 9 || strings.TrimLeft(digits, "0123456789") != "" || digits[0] == '0' {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return 0, false
	}
	return n, true
}

// numberedAttachmentName is name itself for attempt 0, and otherwise name
// with " (attempt)" before its extension: `photo (1).jpg`, `notes (2)`. A
// name that is only an extension (`.env`) is numbered at its end.
func numberedAttachmentName(name string, attempt int) string {
	if attempt == 0 {
		return name
	}
	stem, ext := splitAttachmentName(name)
	return fmt.Sprintf("%s (%d)%s", stem, attempt, ext)
}

// finalizeAttachment validates size limits, writes the object through the
// S6/S9 metadata tables, and writes the response — the shared tail of both
// the chunked and the plain-multipart paths above, mirroring legacy's own
// process_uploaded_files, which both call identically.
func (h *Handler) finalizeAttachment(w http.ResponseWriter, ctx context.Context, projectID int64, projectIDStr, conversationID, fileName, contentType string, content []byte) {
	bucketName, policyMaxFileBytes, policyRetentionDays, err := h.attachments.AttachmentPolicy(ctx, projectID)
	if err != nil {
		apierr.Write(w, apierr.Internal("get project storage policy: "+err.Error()))
		return
	}
	if bucketName == "" {
		bucketName = storage.DefaultAttachmentBucket
	}

	isImage := isImageAttachment(fileName)
	// Policy-value-first, env-fallback-second — matches objects.go's own
	// precedent for the identical concept (S12's max_object_bytes: a cap on
	// one uploaded object). Image uploads stay on the env/const-only image
	// ceiling: there is no policy field for "per-image cap" specifically,
	// see AttachmentStore's own doc comment.
	maxFileBytes := attachmentMaxFileBytes()
	if policyMaxFileBytes != nil {
		maxFileBytes = *policyMaxFileBytes
	}
	kind := "File"
	if isImage {
		maxFileBytes = attachmentMaxImageBytes()
		kind = "Image"
	}
	if int64(len(content)) > maxFileBytes {
		apierr.Write(w, apierr.BadRequest(fmt.Sprintf("%s %q exceeds the %d MB limit", kind, fileName, maxFileBytes>>20)))
		return
	}
	if int64(len(content)) > attachmentMaxTotalBytes() {
		apierr.Write(w, apierr.BadRequest(fmt.Sprintf("Total upload size exceeds the limit of %d MB", attachmentMaxTotalBytes()>>20)))
		return
	}

	retentionDays := attachmentRetentionDays()
	if policyRetentionDays != nil {
		retentionDays = *policyRetentionDays
	}
	bucketID, err := h.attachments.RequireAttachmentBucket(ctx, projectID, bucketName, retentionDays)
	if errors.Is(err, storage.ErrSystemBucketNameTaken) {
		apierr.Write(w, apierr.Conflict(fmt.Sprintf(
			"the attachment bucket %q of this project is a bucket a person created, not the platform's attachment bucket; rename or delete it (Artifacts page) and upload again", bucketName)))
		return
	}
	if err != nil {
		apierr.Write(w, apierr.Internal("get or create attachment bucket: "+err.Error()))
		return
	}
	// The retention window of this attachment starts now, not when the
	// bucket was created.
	attachmentExpiresAt := time.Now().AddDate(0, 0, int(retentionDays))

	// Legacy prefixes the stored filename with the conversation's UUID
	// (f"{conversation.uuid}/{sanitized_filename}"); conversationID here is
	// already that identifier — chi.URLParam, not a database lookup — because
	// writeAttachmentBytes refuses a numeric conversation id outright before
	// reaching this point. That guard is what makes this sentence true; without
	// it the claim was merely a hope about the caller, and the composer's own
	// upload disproved it.
	//
	// A NAME ALREADY TAKEN IN THIS CONVERSATION GETS A NUMBER (#1059). The key
	// used to be the sanitized name alone, so a second `photo.jpg` overwrote
	// the first — bytes an earlier message still names. The upload now
	// reserves a free key first (`photo.jpg`, else the next free
	// `photo (N).jpg`) with a placeholder metadata row, and the unique (bucket_id, key) index decides
	// between two concurrent uploads of one name. The answer carries the key
	// that was reserved, and every client already sends back the filepath it
	// was given, so stored objects and their paths are untouched.
	key, ref, err := h.reserveAttachmentKey(ctx, bucketID, projectIDStr, bucketName, conversationID, sanitizeAttachmentFilename(fileName), contentType, &attachmentExpiresAt)
	if err != nil {
		apierr.Write(w, err)
		return
	}

	info, err := h.store.Put(ctx, ref, bytes.NewReader(content), storage.PutOptions{
		ContentType:   contentType,
		ContentLength: int64(len(content)),
	})
	if err != nil {
		// Free the key: the placeholder names bytes that were never stored.
		_ = h.attachments.DeleteAttachmentObjects(ctx, bucketID, []string{key})
		apierr.Write(w, apierr.Internal("store attachment: "+err.Error()))
		return
	}
	if info.Key != key {
		// A store that chose its own key: the reservation names nothing.
		_ = h.attachments.DeleteAttachmentObjects(ctx, bucketID, []string{key})
	}

	if err := h.attachments.RecordAttachmentObject(ctx, bucketID, info.Key, info.Size, contentType, &attachmentExpiresAt); err != nil {
		apierr.Write(w, apierr.Internal("record attachment metadata: "+err.Error()))
		return
	}
	// Extract the text now, in the background (issue 1062): the first turn
	// that carries the file then reads the filed result. It never blocks or
	// fails the upload.
	h.warmAttachmentExtraction(projectID, bucketName, info.Key, fileName)

	writeJSON(w, http.StatusCreated, []attachmentCreated{{
		Filepath: fmt.Sprintf("/%s/%s", bucketName, info.Key),
		FileSize: info.Size,
	}})
}
