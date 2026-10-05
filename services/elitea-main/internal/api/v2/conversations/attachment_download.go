package conversations

// Chat attachment download (client contract 1.1,
// downloadConversationAttachment): GET and HEAD
// /attachments/prompt_lib/{projectID}/{conversationUUID}/{name}.
//
// WHY A CONVERSATION-SCOPED READ. A transcript's attachment item names its
// file as `/{bucket}/{conversationUUID}/{name}`, and until now the only way
// to fetch those bytes was the artifacts object route — which is gated by
// `configuration.artifacts.artifacts.view` on the WHOLE bucket. The chat
// attachment bucket is shared by every conversation in the project, so that
// permission reads every conversation's files, private ones included, and a
// participant without it reads none of their own. This route is gated by the
// conversation instead: the conversation-read permission plus the chat
// authority's visibility check, and the object key is derived from the
// conversation the caller was authorized for — never from a client-supplied
// bucket or path.
//
// WHY 404 AND NOT 403. A caller who cannot see the conversation gets the same
// answer as for a conversation that does not exist (AuthorizeChatResource),
// so this route cannot be used to learn which conversations exist.
//
// WHY THE HEADERS. The bytes are user uploads. They are served as a download
// (`Content-Disposition: attachment`), never sniffed (`nosniff`), sandboxed if
// a browser renders them anyway (`Content-Security-Policy: sandbox`), never
// cached by a shared cache, and an ACTIVE type (HTML, SVG, XML, script) is
// served as application/octet-stream whatever was recorded at upload: the
// recorded type is whatever the uploading client said.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// AttachmentObjectInfo is the metadata row of one stored chat attachment.
type AttachmentObjectInfo struct {
	MediaType  string
	ByteLength int64
}

// attachmentSniffBytes is what http.DetectContentType reads.
const attachmentSniffBytes = 512

// DownloadAttachment answers GET and HEAD for one chat attachment.
func (h *Handler) DownloadAttachment(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	if h.store == nil || h.attachments == nil {
		apierr.Write(w, apierr.NotImplemented("chat attachments are not stored on this deployment"))
		return
	}
	projectIDText := chi.URLParam(r, "projectID")
	projectID, err := strconv.ParseInt(projectIDText, 10, 64)
	if err != nil || projectID <= 0 {
		apierr.Write(w, apierr.BadRequest("invalid project id"))
		return
	}
	conversationID := chi.URLParam(r, "conversationID")
	if _, numeric := strconv.ParseUint(conversationID, 10, 64); numeric == nil {
		apierr.Write(w, apierr.BadRequest("conversation id must be the conversation uuid"))
		return
	}
	name, ok := attachmentDownloadName(chi.URLParam(r, "name"), r.URL.RawPath != "")
	if !ok {
		apierr.Write(w, apierr.BadRequest("invalid attachment name"))
		return
	}

	bucketName, _, _, err := h.attachments.AttachmentPolicy(r.Context(), projectID)
	if err != nil {
		apierr.Write(w, apierr.Internal("get project storage policy: "+err.Error()))
		return
	}
	if bucketName == "" {
		bucketName = defaultAttachmentBucketName
	}
	key := conversationID + "/" + name
	info, err := h.attachments.AttachmentObject(r.Context(), projectID, bucketName, key)
	if errors.Is(err, storage.ErrNotFound) {
		apierr.Write(w, apierr.NotFound("attachment not found"))
		return
	}
	if err != nil {
		apierr.Write(w, apierr.Internal("read attachment metadata: "+err.Error()))
		return
	}
	ref, err := storage.NewObjectRef(projectIDText, bucketName, key)
	if err != nil {
		apierr.Write(w, apierr.NotFound("attachment not found"))
		return
	}

	var rng *storage.ByteRange
	if header := r.Header.Get("Range"); header != "" {
		parsed, ok := parseAttachmentRange(header, info.ByteLength)
		if !ok {
			w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(info.ByteLength, 10))
			apierr.Write(w, &apierr.APIError{Status: http.StatusRequestedRangeNotSatisfiable, Code: "range_not_satisfiable", Message: "requested range is not satisfiable"})
			return
		}
		rng = &parsed
	}

	body, objectInfo, err := h.store.Get(r.Context(), ref, rng)
	if errors.Is(err, storage.ErrNotFound) {
		apierr.Write(w, apierr.NotFound("attachment not found"))
		return
	}
	if err != nil {
		apierr.Write(w, apierr.Internal("read attachment: "+err.Error()))
		return
	}
	defer func() { _ = body.Close() }()
	if rng == nil && objectInfo.Size > 0 && objectInfo.Size != info.ByteLength {
		// The metadata row and the object drifted (a re-upload raced this
		// read). Serving either length would truncate or pad the file.
		apierr.Write(w, apierr.Internal("attachment length disagrees with its metadata row"))
		return
	}

	reader := bufio.NewReaderSize(body, attachmentSniffBytes)
	var prefix []byte
	if rng == nil || rng.Start == 0 {
		prefix, _ = reader.Peek(attachmentSniffBytes)
	} else {
		prefix = h.attachmentPrefix(r.Context(), ref)
	}

	header := w.Header()
	header.Set("Content-Type", safeAttachmentContentType(info.MediaType, name, prefix))
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Disposition", attachmentContentDisposition(name))
	header.Set("Content-Security-Policy", "sandbox")
	header.Set("Cache-Control", "private, no-store")
	header.Set("Accept-Ranges", "bytes")
	if objectInfo.ETag != "" {
		etag := objectInfo.ETag
		if !strings.HasPrefix(etag, `"`) && !strings.HasPrefix(etag, `W/"`) {
			etag = `"` + etag + `"`
		}
		header.Set("ETag", etag)
		if rng == nil && r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	status := http.StatusOK
	length := info.ByteLength
	if rng != nil {
		end := rng.End
		if end < 0 || end >= info.ByteLength {
			end = info.ByteLength - 1
		}
		length = end - rng.Start + 1
		header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rng.Start, end, info.ByteLength))
		status = http.StatusPartialContent
	}
	header.Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.CopyN(w, reader, length)
}

// attachmentPrefix reads the object's first bytes for a ranged request that
// does not start at 0, so the content type is judged on the file's real
// header rather than on a slice of its middle.
func (h *Handler) attachmentPrefix(ctx context.Context, ref storage.ObjectRef) []byte {
	body, _, err := h.store.Get(ctx, ref, &storage.ByteRange{Start: 0, End: attachmentSniffBytes - 1})
	if err != nil {
		return nil
	}
	defer func() { _ = body.Close() }()
	prefix, _ := io.ReadAll(io.LimitReader(body, attachmentSniffBytes))
	return prefix
}

// attachmentDownloadName validates the {name} path segment: one file name, as
// the upload stored it (sanitizeAttachmentFilename's output). chi routes on
// r.URL.RawPath when the request carried non-default escapes (an escaped
// slash, say), and then hands the segment over STILL escaped; otherwise it
// routes on the decoded path. So the segment is unescaped exactly when it
// came from RawPath — unescaping a decoded name twice would turn a literal
// "%41" into "A" — and then refused if it names a directory, climbs out of
// one, or carries a control character.
func attachmentDownloadName(raw string, escaped bool) (string, bool) {
	name := raw
	if escaped {
		unescaped, err := url.PathUnescape(raw)
		if err != nil {
			return "", false
		}
		name = unescaped
	}
	if name == "" || len(name) > 1024 || name == "." || name == ".." {
		return "", false
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return "", false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	if sanitizeAttachmentFilename(name) != name {
		return "", false
	}
	return name, true
}

// parseAttachmentRange accepts one `bytes=start-end` or `bytes=start-` range
// inside an object of total bytes. A suffix range (`bytes=-N`) is answered
// as the last N bytes. Anything else, or a range that starts past the end,
// is unsatisfiable.
func parseAttachmentRange(header string, total int64) (storage.ByteRange, bool) {
	spec, ok := strings.CutPrefix(header, "bytes=")
	if !ok || strings.Contains(spec, ",") || total <= 0 {
		return storage.ByteRange{}, false
	}
	first, last, ok := strings.Cut(strings.TrimSpace(spec), "-")
	if !ok {
		return storage.ByteRange{}, false
	}
	if first == "" {
		suffix, err := strconv.ParseInt(last, 10, 64)
		if err != nil || suffix <= 0 {
			return storage.ByteRange{}, false
		}
		start := total - suffix
		if start < 0 {
			start = 0
		}
		return storage.ByteRange{Start: start, End: total - 1}, true
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 || start >= total {
		return storage.ByteRange{}, false
	}
	end := total - 1
	if last != "" {
		end, err = strconv.ParseInt(last, 10, 64)
		if err != nil || end < start {
			return storage.ByteRange{}, false
		}
		if end >= total {
			end = total - 1
		}
	}
	return storage.ByteRange{Start: start, End: end}, true
}

// activeAttachmentTypes are media types a browser executes or renders as a
// document. They are never served as themselves.
var activeAttachmentTypes = []string{
	"text/html", "application/xhtml", "image/svg", "text/xml", "application/xml",
	"text/javascript", "application/javascript", "application/ecmascript", "text/ecmascript",
	"application/x-javascript", "text/css", "multipart/",
}

func activeAttachmentType(mediaType string) bool {
	lowered := strings.ToLower(mediaType)
	for _, active := range activeAttachmentTypes {
		if strings.HasPrefix(lowered, active) {
			return true
		}
	}
	return strings.HasSuffix(lowered, "+xml")
}

// safeAttachmentContentType decides the served Content-Type. The recorded
// type (or the extension's, when nothing was recorded) is kept only when the
// file's first bytes agree with it; an active type, a disagreement or an
// unparseable value is application/octet-stream.
func safeAttachmentContentType(recorded, name string, prefix []byte) string {
	const fallback = "application/octet-stream"
	declared := recorded
	if declared == "" || declared == fallback {
		declared = mime.TypeByExtension(strings.ToLower(path.Ext(name)))
	}
	mediaType, _, err := mime.ParseMediaType(declared)
	if err != nil || mediaType == "" || activeAttachmentType(mediaType) {
		return fallback
	}
	if len(prefix) == 0 {
		return fallback
	}
	sniffed, _, _ := mime.ParseMediaType(http.DetectContentType(prefix))
	if activeAttachmentType(sniffed) {
		return fallback
	}
	switch {
	case strings.HasPrefix(mediaType, "image/"), strings.HasPrefix(mediaType, "audio/"),
		strings.HasPrefix(mediaType, "video/"), mediaType == "application/pdf":
		// A binary type the client may render natively must be what the
		// bytes say it is.
		if sniffed != mediaType {
			return fallback
		}
	case strings.HasPrefix(mediaType, "text/"):
		if !strings.HasPrefix(sniffed, "text/plain") {
			return fallback
		}
		return mediaType + "; charset=utf-8"
	}
	return mediaType
}

// attachmentContentDisposition is `attachment` with an ASCII fallback name
// and the exact name in RFC 5987 form.
func attachmentContentDisposition(name string) string {
	var fallback strings.Builder
	for _, r := range name {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' || r == '%' || r == ';' {
			fallback.WriteByte('_')
			continue
		}
		fallback.WriteRune(r)
	}
	var encoded strings.Builder
	for _, b := range []byte(name) {
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') ||
			strings.IndexByte("!#$&+-.^_`|~", b) >= 0 {
			encoded.WriteByte(b)
			continue
		}
		fmt.Fprintf(&encoded, "%%%02X", b)
	}
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, fallback.String(), encoded.String())
}
