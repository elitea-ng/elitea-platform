// Package agentmediaapi serves an agent's avatar pack media (ADR-0050
// decision 6, issue 1619): upload, the public download, and generation of
// stills and loops through the LLM gateway.
//
// STORAGE. Every file lands in the project's reserved `agent-media` bucket
// through the object store the artifact path uses (ADR-0016), under its
// SHA-256: the URL `/api/v2/agent_media/{project}/{sha256}.{ext}` is a
// content address, so the same bytes are one object, the response is
// immutable, and a pack names a file that cannot change under it.
//
// VALIDATION. Every byte that reaches the bucket has passed
// agentmedia.Inspect, which decides the type from the bytes and refuses
// with `avatar_media_invalid`. Generated stills and loops pass the same
// check. Nothing is stored before it passes.
//
// SERVING. The download route is public, like /icons: an <img> or <video>
// request carries no credential. It sets nosniff, a sandboxing CSP and an
// attachment disposition, as the icon and brand asset routes do, and takes
// the content type from the URL's extension, never from the backend.
//
// GENERATION. A still is `/llm/v1/images/generations` with the soul's
// display name, role and voice in the prompt, `n` candidates. A loop is
// 4–8 `/llm/v1/images/edits` calls over the chosen still, one per pose,
// assembled into an animated WebP. The edits run in parallel, at most
// maxConcurrentFrameEdits at a time, so the request lasts about one call
// rather than the sum of them (#1854). Every call goes through llmproxy.Gateway
// (gRPC over mTLS) under the caller's identity, so the gateway budgets,
// bills and logs it against the project. Generated files are drafts: they
// are stored, and returned with their URL, but nothing joins a pack until
// the author saves the version.
package agentmediaapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"golang.org/x/sync/errgroup"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmedia"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/llmproxy"
)

// Bucket is the reserved project bucket the media live in.
const Bucket = "agent-media"

// The routes. UploadPath and GeneratePath are relative to the
// /api/v2/elitea_core group; DownloadPath is root-mounted.
const (
	UploadPath   = "/agent_media/prompt_lib/{projectID}"
	GeneratePath = "/agent_media_generate/prompt_lib/{projectID}"
	DownloadPath = agentmedia.URLPrefix + "{projectID}/{file}"
)

// Permission gates upload and generation: the agent icon upload permission,
// because a pack's still IS the agent's icon (ADR-0050 decision 6).
const Permission = "models.applications.upload_icon.post"

const (
	// maxUploadBody bounds the multipart request: the file cap plus room for
	// the form framing.
	maxUploadBody  = agentmedia.MaxBytes + 64<<10
	maxPromptRunes = 1_000
	// maxGatewayAnswer bounds a gateway images answer (base64 inflates by a
	// third; four candidates of a 2 MiB image is the worst we accept).
	maxGatewayAnswer  = 48 << 20
	maxCandidates     = 4
	defaultLoopFrames = 6
	loopFrameMS       = 400
	cacheImmutable    = "public, max-age=31536000, immutable"
	// maxConcurrentFrameEdits bounds a loop's edit calls in flight: enough
	// that a default six-frame loop takes about two calls of wall time,
	// few enough that one author cannot open eight provider calls at once.
	maxConcurrentFrameEdits = 4
)

// Handler serves the three routes.
type Handler struct {
	store   storage.ObjectStore
	gateway llmproxy.Gateway
}

// NewHandler builds the handler. A nil store answers 503 on upload and
// generation and 404 on download; a nil gateway answers 503 on generation.
func NewHandler(store storage.ObjectStore, gateway llmproxy.Gateway) *Handler {
	return &Handler{store: store, gateway: gateway}
}

// MediaObject is the answer for one stored file.
type MediaObject struct {
	URL         string `json:"url"`
	MediaType   string `json:"media_type"`
	Format      string `json:"format"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	DurationMS  int    `json:"duration_ms"`
	Bytes       int    `json:"bytes"`
	GeneratedBy string `json:"generated_by,omitempty"`
}

type refusal struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeRefusal(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, refusal{Error: message, Code: code})
}

func validProject(projectID string) bool {
	if projectID == "" || len(projectID) > 18 || projectID[0] == '0' {
		return false
	}
	for _, c := range projectID {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// put stores validated bytes under their digest and answers the object.
func (h *Handler) put(ctx context.Context, projectID string, data []byte, media agentmedia.Media) (MediaObject, error) {
	sum := sha256.Sum256(data)
	file := hex.EncodeToString(sum[:]) + "." + media.Extension
	ref, err := storage.NewObjectRef(projectID, Bucket, file)
	if err != nil {
		return MediaObject{}, err
	}
	if _, err := h.store.Put(ctx, ref, bytes.NewReader(data), storage.PutOptions{
		ContentType: media.MediaType, ContentLength: int64(len(data)), ChecksumSHA256: sum[:],
	}); err != nil {
		return MediaObject{}, err
	}
	return MediaObject{
		URL: agentmedia.URLOf(projectID, file), MediaType: media.MediaType, Format: string(media.Format),
		Width: media.Width, Height: media.Height, DurationMS: media.DurationMS, Bytes: media.Bytes,
	}, nil
}

// Upload stores one file (multipart field `file`). The type comes from the
// bytes; the part's name and Content-Type are not read.
func (h *Handler) Upload(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if !validProject(projectID) {
		writeRefusal(w, http.StatusBadRequest, "invalid_project", "invalid project")
		return
	}
	if h.store == nil {
		writeRefusal(w, http.StatusServiceUnavailable, "storage_unavailable", "agent media storage is not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBody)
	data, ok := readFilePart(w, r)
	if !ok {
		return
	}
	media, err := agentmedia.Inspect(data)
	if err != nil {
		writeRefusal(w, http.StatusBadRequest, agentmedia.Code, err.Error())
		return
	}
	object, err := h.put(r.Context(), projectID, data, media)
	if err != nil {
		slog.ErrorContext(r.Context(), "agent media: store upload", "project_id", projectID, "error", err)
		writeRefusal(w, http.StatusInternalServerError, "storage_failed", "the file could not be stored")
		return
	}
	writeJSON(w, http.StatusCreated, object)
}

// readFilePart reads the `file` part, at most agentmedia.MaxBytes + 1 bytes
// so an oversized file is refused by the validator with its size named.
func readFilePart(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	reader, err := r.MultipartReader()
	if err != nil {
		writeRefusal(w, http.StatusBadRequest, agentmedia.Code, "the request must be multipart/form-data with a `file` part")
		return nil, false
	}
	for {
		part, err := reader.NextPart()
		if err != nil {
			if isTooLarge(err) {
				writeRefusal(w, http.StatusRequestEntityTooLarge, agentmedia.Code, fmt.Sprintf("an avatar file may be at most %d KiB", agentmedia.MaxBytes/1024))
				return nil, false
			}
			writeRefusal(w, http.StatusBadRequest, agentmedia.Code, "the request has no `file` part")
			return nil, false
		}
		if part.FormName() != "file" {
			_ = part.Close()
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part, agentmedia.MaxBytes+1))
		_ = part.Close()
		if err != nil {
			if isTooLarge(err) {
				writeRefusal(w, http.StatusRequestEntityTooLarge, agentmedia.Code, fmt.Sprintf("an avatar file may be at most %d KiB", agentmedia.MaxBytes/1024))
				return nil, false
			}
			writeRefusal(w, http.StatusBadRequest, agentmedia.Code, "the file could not be read")
			return nil, false
		}
		return data, true
	}
}

func isTooLarge(err error) bool {
	var tooLarge *http.MaxBytesError
	return errors.As(err, &tooLarge)
}

// Download serves one stored file. Public: see the package doc.
func Download(store storage.ObjectStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projectID := chi.URLParam(r, "projectID")
		file := chi.URLParam(r, "file")
		owner, name, extension, ok := agentmedia.ParseURL(agentmedia.URLOf(projectID, file))
		if !ok || store == nil || owner != projectID {
			http.NotFound(w, r)
			return
		}
		ref, err := storage.NewObjectRef(projectID, Bucket, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		var rng *storage.ByteRange
		if header := r.Header.Get("Range"); header != "" {
			// A <video> element asks for ranges; a malformed one gets the whole file.
			rng = parseRange(header)
		}
		body, info, err := store.Get(r.Context(), ref, rng)
		if err != nil && rng != nil && !errors.Is(err, storage.ErrNotFound) {
			// A backend may fail a range past the end with its own error;
			// the object's existence decides between 416 and 404.
			if head, statErr := store.Stat(r.Context(), ref); statErr == nil {
				writeUnsatisfiable(w, head.Size)
				return
			}
		}
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer func() { _ = body.Close() }()
		w.Header().Set("Content-Type", agentmedia.MediaTypeOf(extension))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		w.Header().Set("Content-Disposition", "attachment")
		w.Header().Set("Cache-Control", cacheImmutable)
		w.Header().Set("ETag", `"`+strings.TrimSuffix(name, "."+extension)+`"`)
		w.Header().Set("Accept-Ranges", "bytes")
		status := http.StatusOK
		if rng != nil {
			// A ranged read always answers 206 with Content-Range (RFC 7233):
			// a 200 carrying only the range would read as the whole file. A
			// backend may leave TotalSize at 0; the range's own end is then
			// the best known length, as the artifacts download does.
			total := info.TotalSize
			if total <= 0 {
				total = rng.Start + info.Size
			}
			if info.Size <= 0 || rng.Start >= total {
				writeUnsatisfiable(w, total)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rng.Start, rng.Start+info.Size-1, total))
			status = http.StatusPartialContent
		}
		if info.Size > 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
		}
		w.WriteHeader(status)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = io.Copy(w, body)
	}
}

// writeUnsatisfiable answers 416 with the current length.
func writeUnsatisfiable(w http.ResponseWriter, total int64) {
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(total, 10))
	w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
}

// parseRange reads one `bytes=start-[end]` range; nil for anything else.
func parseRange(header string) *storage.ByteRange {
	spec, ok := strings.CutPrefix(header, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return nil
	}
	startText, endText, ok := strings.Cut(spec, "-")
	if !ok || startText == "" {
		return nil
	}
	start, err := strconv.ParseInt(startText, 10, 64)
	if err != nil || start < 0 {
		return nil
	}
	end := int64(-1)
	if endText != "" {
		end, err = strconv.ParseInt(endText, 10, 64)
		if err != nil || end < start {
			return nil
		}
	}
	return &storage.ByteRange{Start: start, End: end}
}

// GenerateRequest is the generation body.
type GenerateRequest struct {
	// Kind is `still` (candidates from a text prompt) or `loop` (pose
	// frames of StillURL assembled into an animated WebP).
	Kind string `json:"kind"`
	// Model is an image_generation_model of the project's catalogue.
	Model string `json:"model"`
	// DisplayName, Role and Voice are the soul's; the server writes the
	// prompt from them.
	DisplayName string   `json:"display_name"`
	Role        string   `json:"role"`
	Voice       []string `json:"voice"`
	// Prompt is an optional extra direction.
	Prompt string `json:"prompt"`
	// Count is the number of still candidates (1–4, default 4).
	Count int `json:"count"`
	// State is the slot a loop is made for (thinking, speaking, ...).
	State string `json:"state"`
	// StillURL is the chosen still a loop animates (this project's media).
	StillURL string `json:"still_url"`
	// Frames is the number of loop frames (4–8, default 6).
	Frames int `json:"frames"`
}

// GenerateResponse lists the stored drafts.
type GenerateResponse struct {
	Media []MediaObject `json:"media"`
}

// Generate makes still candidates or one loop.
func (h *Handler) Generate(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if !validProject(projectID) {
		writeRefusal(w, http.StatusBadRequest, "invalid_project", "invalid project")
		return
	}
	if h.store == nil {
		writeRefusal(w, http.StatusServiceUnavailable, "storage_unavailable", "agent media storage is not configured")
		return
	}
	if h.gateway == nil {
		writeRefusal(w, http.StatusServiceUnavailable, "gateway_unavailable", "image generation needs the LLM gateway (LLM_GATEWAY_URL)")
		return
	}
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		writeRefusal(w, http.StatusUnauthorized, "unauthorized", "an authenticated principal is required")
		return
	}
	userID := ""
	if id, ok := user.OwningUserID(); ok {
		userID = strconv.FormatInt(id, 10)
	}
	var req GenerateRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeRefusal(w, http.StatusBadRequest, "invalid_request", "the body must be a generation request")
		return
	}
	if problem := req.check(projectID); problem != "" {
		writeRefusal(w, http.StatusBadRequest, "invalid_request", problem)
		return
	}
	identity := llmproxy.Identity{ProjectID: projectID, UserID: userID}
	var (
		media []MediaObject
		err   error
	)
	if req.Kind == "still" {
		media, err = h.generateStills(r.Context(), identity, req)
	} else {
		media, err = h.generateLoop(r.Context(), identity, req)
	}
	if err != nil {
		h.writeGenerationError(w, r, projectID, err)
		return
	}
	writeJSON(w, http.StatusCreated, GenerateResponse{Media: media})
}

var loopStates = map[string]string{
	agentmedia.SlotIdle:     "calm, at rest, blinking slowly",
	agentmedia.SlotThinking: "thinking, eyes looking up, a hand at the chin",
	agentmedia.SlotSpeaking: "speaking, mouth open mid-word, engaged",
	agentmedia.SlotWorking:  "busy working, focused, leaning in",
	agentmedia.SlotWaiting:  "waiting patiently, attentive, looking at the viewer",
	agentmedia.SlotSuccess:  "pleased, a small smile, a nod",
	agentmedia.SlotError:    "concerned, a slight frown",
	agentmedia.SlotWaking:   "waking up, stretching, eyes opening",
}

func (req *GenerateRequest) check(projectID string) string {
	req.Model = strings.TrimSpace(req.Model)
	if req.Model == "" || len(req.Model) > 256 {
		return "model must name an image generation model"
	}
	for _, text := range append([]string{req.DisplayName, req.Role, req.Prompt}, req.Voice...) {
		if len([]rune(text)) > maxPromptRunes {
			return fmt.Sprintf("a prompt field is longer than %d characters", maxPromptRunes)
		}
	}
	if len(req.Voice) > 16 {
		return "voice may list at most 16 words"
	}
	switch req.Kind {
	case "still":
		if req.Count == 0 {
			req.Count = maxCandidates
		}
		if req.Count < 1 || req.Count > maxCandidates {
			return fmt.Sprintf("count must be 1 to %d", maxCandidates)
		}
	case "loop":
		if _, ok := loopStates[req.State]; !ok {
			return "state must be one of idle, thinking, speaking, working, waiting, success, error, waking"
		}
		owner, _, extension, ok := agentmedia.ParseURL(req.StillURL)
		if !ok || owner != projectID || extension == "svg" || extension == "mp4" || extension == "webm" {
			return "still_url must be a PNG, JPEG or WebP still uploaded to this project"
		}
		if req.Frames == 0 {
			req.Frames = defaultLoopFrames
		}
		if req.Frames < 4 || req.Frames > agentmedia.MaxLoopFrames {
			return fmt.Sprintf("frames must be 4 to %d", agentmedia.MaxLoopFrames)
		}
	default:
		return "kind must be still or loop"
	}
	return ""
}

// stillPrompt turns the soul's fields into the generation prompt.
func stillPrompt(req GenerateRequest) string {
	var b strings.Builder
	b.WriteString("A square avatar portrait for an AI assistant")
	if name := strings.TrimSpace(req.DisplayName); name != "" {
		b.WriteString(" named ")
		b.WriteString(name)
	}
	if role := strings.TrimSpace(req.Role); role != "" {
		b.WriteString(", whose role is ")
		b.WriteString(role)
	}
	if len(req.Voice) > 0 {
		b.WriteString(". Personality: ")
		b.WriteString(strings.Join(req.Voice, ", "))
	}
	b.WriteString(". Centred head and shoulders, plain background, friendly, no text, no letters.")
	if extra := strings.TrimSpace(req.Prompt); extra != "" {
		b.WriteString(" ")
		b.WriteString(extra)
	}
	return b.String()
}

// errGateway carries the gateway's status for a refused call.
type errGateway struct {
	status int
}

func (e *errGateway) Error() string { return fmt.Sprintf("the gateway answered %d", e.status) }

func (h *Handler) writeGenerationError(w http.ResponseWriter, r *http.Request, projectID string, err error) {
	var gatewayErr *errGateway
	switch {
	case agentmedia.IsRefusal(err):
		writeRefusal(w, http.StatusBadGateway, agentmedia.Code, "the generated image failed validation: "+err.Error())
	case errors.As(err, &gatewayErr):
		status := gatewayErr.status
		// The budget, rate-limit and policy answers pass through as they are;
		// anything else from the provider is a bad gateway.
		if status != http.StatusPaymentRequired && status != http.StatusTooManyRequests && status != http.StatusForbidden && status != http.StatusBadRequest {
			status = http.StatusBadGateway
		}
		writeRefusal(w, status, "generation_refused", fmt.Sprintf("the image model refused the request (%d)", gatewayErr.status))
	default:
		slog.ErrorContext(r.Context(), "agent media: generation failed", "project_id", projectID, "error", err)
		writeRefusal(w, http.StatusBadGateway, "generation_failed", "the image could not be generated")
	}
}

type imagesAnswer struct {
	Data []struct {
		B64JSON string `json:"b64_json"`
	} `json:"data"`
}

// readImages reads a gateway images answer into decoded images.
func readImages(resp *llmproxy.Response) ([][]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, &errGateway{status: resp.StatusCode}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxGatewayAnswer))
	if err != nil {
		return nil, err
	}
	var answer imagesAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, fmt.Errorf("decode images answer: %w", err)
	}
	var images [][]byte
	for _, item := range answer.Data {
		if item.B64JSON == "" {
			continue // a url-only item is never fetched: no egress beyond the gateway
		}
		decoded, err := base64.StdEncoding.DecodeString(item.B64JSON)
		if err != nil {
			return nil, fmt.Errorf("decode image: %w", err)
		}
		images = append(images, decoded)
	}
	if len(images) == 0 {
		return nil, errors.New("the images answer carries no b64_json image")
	}
	return images, nil
}

func (h *Handler) generateStills(ctx context.Context, identity llmproxy.Identity, req GenerateRequest) ([]MediaObject, error) {
	body, _ := json.Marshal(map[string]any{
		"model": req.Model, "prompt": stillPrompt(req), "n": req.Count,
		"size": "1024x1024", "response_format": "b64_json",
	})
	resp, err := h.gateway.ImageGenerate(ctx, &llmproxy.Request{
		Header: http.Header{"Content-Type": {"application/json"}}, Body: bytes.NewReader(body), Identity: identity,
	})
	if err != nil {
		return nil, err
	}
	images, err := readImages(resp)
	if err != nil {
		return nil, err
	}
	var out []MediaObject
	for _, image := range images {
		// A provider answers 1024 px; the validator's edge is 512, so the
		// still is scaled down first (a one-frame fit, not a loop).
		data, media, err := agentmedia.FitStill(image)
		if err != nil {
			return nil, err
		}
		object, err := h.put(ctx, identity.ProjectID, data, media)
		if err != nil {
			return nil, err
		}
		object.GeneratedBy = req.Model
		out = append(out, object)
	}
	return out, nil
}

func (h *Handler) generateLoop(ctx context.Context, identity llmproxy.Identity, req GenerateRequest) ([]MediaObject, error) {
	_, file, _, _ := agentmedia.ParseURL(req.StillURL)
	ref, err := storage.NewObjectRef(identity.ProjectID, Bucket, file)
	if err != nil {
		return nil, err
	}
	reader, _, err := h.store.Get(ctx, ref, nil)
	if err != nil {
		return nil, fmt.Errorf("read the still: %w", err)
	}
	still, err := io.ReadAll(io.LimitReader(reader, agentmedia.MaxBytes+1))
	_ = reader.Close()
	if err != nil {
		return nil, err
	}
	stillMedia, err := agentmedia.Inspect(still)
	if err != nil {
		return nil, err
	}
	frames, err := h.loopFrames(ctx, identity, req, still, stillMedia)
	if err != nil {
		return nil, err
	}
	data, media, err := agentmedia.AssembleLoop(frames, loopFrameMS)
	if err != nil {
		return nil, err
	}
	object, err := h.put(ctx, identity.ProjectID, data, media)
	if err != nil {
		return nil, err
	}
	object.GeneratedBy = req.Model
	return []MediaObject{object}, nil
}

// loopFrames makes the loop's pose frames, in pose order, with at most
// maxConcurrentFrameEdits edit calls in flight (#1854). One edit commonly
// takes 10–30 s, so one after another an eight-frame loop held its request
// for minutes, long enough for an edge or browser timeout to cut it after
// the gateway had already charged the budget. In parallel the wall time is
// about one call. Each call writes only its own slot, so the order is the
// pose order whatever order the answers arrive in. The first failure
// cancels the calls still running, and that failure is the one answered
// (the budget, rate-limit and validation mappings of writeGenerationError
// still apply); the frames already charged are not stored, as before.
func (h *Handler) loopFrames(ctx context.Context, identity llmproxy.Identity, req GenerateRequest, still []byte, stillMedia agentmedia.Media) ([][]byte, error) {
	pose := loopStates[req.State]
	frames := make([][]byte, req.Frames)
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(maxConcurrentFrameEdits)
	for index := 0; index < req.Frames; index++ {
		group.Go(func() error {
			prompt := fmt.Sprintf("The same character, same style, same framing and background. Pose %d of %d of a short seamless loop: %s. Keep the face and identity identical.",
				index+1, req.Frames, pose)
			body, contentType, err := editForm(req.Model, prompt, still, stillMedia)
			if err != nil {
				return err
			}
			resp, err := h.gateway.ImageEdit(groupCtx, &llmproxy.Request{
				Header: http.Header{"Content-Type": {contentType}}, Body: bytes.NewReader(body), Identity: identity,
			})
			if err != nil {
				return err
			}
			images, err := readImages(resp)
			if err != nil {
				return err
			}
			frames[index] = images[0]
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	return frames, nil
}

// editForm builds the /images/edits multipart body. The image part names
// its real type (the gateway's providers read it), never octet-stream.
func editForm(model, prompt string, image []byte, media agentmedia.Media) ([]byte, string, error) {
	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)
	for key, value := range map[string]string{"model": model, "prompt": prompt, "n": "1", "response_format": "b64_json"} {
		if err := form.WriteField(key, value); err != nil {
			return nil, "", err
		}
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "image", "filename": "still." + media.Extension}))
	header.Set("Content-Type", media.MediaType)
	part, err := form.CreatePart(header)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(image); err != nil {
		return nil, "", err
	}
	if err := form.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), form.FormDataContentType(), nil
}
