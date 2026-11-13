package llmproxy

import (
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"

	"github.com/maximhq/bifrost/core/providers/openai"
	"github.com/maximhq/bifrost/core/schemas"
)

// maxMultipartMemory bounds the in-memory portion of a parsed multipart form;
// larger file parts spill to temp files. Image payloads (a handful of images +
// a mask) fit comfortably here.
const maxMultipartMemory = 32 << 20 // 32 MiB

// ImageEdit handles POST /llm/v1/images/edits. The gateway parses the
// multipart body itself (the fasthttp integrations parser cannot run under
// net/http) and builds the core ImageEdit request struct, mirroring the field
// contract of parseOpenAIImageEditMultipartRequest (design §6.3).
func (h *Handler) ImageEdit(w http.ResponseWriter, r *http.Request) {
	form, ok := parseMultipart(w, r)
	if !ok {
		return
	}

	req, err := buildImageEditRequest(form)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}
	ctx, ok := h.newContext(w, r)
	if !ok {
		return
	}
	bifReq := req.ToBifrostImageEditRequest(ctx)

	// Issue #317: map the caller's model id before the gate and the provider.
	if !h.mapModel(w, ctx, &bifReq.Provider, &bifReq.Model) {
		return
	}
	// FIX #26: enforce the budget gate before calling the image provider.
	provider, model := providerModelFromImageEditReq(bifReq)
	spec := imageSpecOfEdit(bifReq)
	quote, ok := h.checkImageBudget(w, ctx, provider, model, spec)
	if !ok {
		return
	}

	resp, bErr := h.router.ImageEditRequest(ctx, bifReq)
	h.writeUnary(w, resp, bErr)
	if bErr == nil && resp != nil {
		h.billImageResponse(ctx, provider, model, spec, quote, resp)
	}
}

// ImageVariation handles POST /llm/v1/images/variations, mirroring
// parseOpenAIImageVariationMultipartRequest.
func (h *Handler) ImageVariation(w http.ResponseWriter, r *http.Request) {
	form, ok := parseMultipart(w, r)
	if !ok {
		return
	}

	req, err := buildImageVariationRequest(form)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}
	ctx, ok := h.newContext(w, r)
	if !ok {
		return
	}
	bifReq := req.ToBifrostImageVariationRequest(ctx)

	// Issue #317: map the caller's model id before the gate and the provider.
	if !h.mapModel(w, ctx, &bifReq.Provider, &bifReq.Model) {
		return
	}
	// FIX #26: enforce the budget gate before calling the image provider.
	provider, model := providerModelFromImageVariationReq(bifReq)
	spec := imageSpecOfVariation(bifReq)
	quote, ok := h.checkImageBudget(w, ctx, provider, model, spec)
	if !ok {
		return
	}

	resp, bErr := h.router.ImageVariationRequest(ctx, bifReq)
	h.writeUnary(w, resp, bErr)
	if bErr == nil && resp != nil {
		h.billImageResponse(ctx, provider, model, spec, quote, resp)
	}
}

// parseMultipart parses the request's multipart body, writing a 400 on failure.
func parseMultipart(w http.ResponseWriter, r *http.Request) (*multipart.Form, bool) {
	if err := r.ParseMultipartForm(maxMultipartMemory); err != nil {
		// A body stopped by http.MaxBytesReader arrives here as a parse
		// failure, and "invalid multipart body" would send the caller looking
		// for a malformed request they do not have. Name the real cause.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error",
				"the uploaded body is larger than this route accepts", "payload_too_large")
			return nil, false
		}
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid multipart body: "+err.Error(), "")
		return nil, false
	}
	return r.MultipartForm, true
}

// buildImageEditRequest constructs an OpenAIImageEditRequest from a parsed
// multipart form. Required fields (model, prompt, at least one image) are
// validated; every optional field is copied only when present, mirroring the
// upstream fasthttp parser.
func buildImageEditRequest(form *multipart.Form) (*openai.OpenAIImageEditRequest, error) {
	req := &openai.OpenAIImageEditRequest{}

	model, err := requiredValue(form, "model")
	if err != nil {
		return nil, err
	}
	req.Model = model

	prompt, err := requiredValue(form, "prompt")
	if err != nil {
		return nil, err
	}

	images, err := readImageFiles(form)
	if err != nil {
		return nil, err
	}
	req.Input = &schemas.ImageEditInput{Images: images, Prompt: prompt}

	if v, ok := stringValue(form, "size"); ok {
		req.Size = &v
	}
	if v, ok := stringValue(form, "quality"); ok {
		req.Quality = &v
	}
	if v, ok := stringValue(form, "response_format"); ok {
		req.ResponseFormat = &v
	}
	if v, ok := stringValue(form, "background"); ok {
		req.Background = &v
	}
	if v, ok := stringValue(form, "input_fidelity"); ok {
		req.InputFidelity = &v
	}
	if v, ok := stringValue(form, "output_format"); ok {
		req.OutputFormat = &v
	}
	if v, ok := stringValue(form, "negative_prompt"); ok {
		req.NegativePrompt = &v
	}
	if v, ok := stringValue(form, "user"); ok {
		req.User = &v
	}
	if v, ok := stringValue(form, "type"); ok {
		req.Type = &v
	}

	if err := setIntValue(form, "n", &req.N); err != nil {
		return nil, err
	}
	if err := setIntValue(form, "partial_images", &req.PartialImages); err != nil {
		return nil, err
	}
	if err := setIntValue(form, "num_inference_steps", &req.NumInferenceSteps); err != nil {
		return nil, err
	}
	if err := setIntValue(form, "seed", &req.Seed); err != nil {
		return nil, err
	}
	if err := setIntValue(form, "output_compression", &req.OutputCompression); err != nil {
		return nil, err
	}

	mask, err := readFile(form, "mask")
	if err != nil {
		return nil, err
	}
	if mask != nil {
		req.Mask = mask
	}

	if v, ok := stringValue(form, "stream"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, wrapInvalid("stream")
		}
		req.Stream = &b
	}
	if fb := form.Value["fallbacks"]; len(fb) > 0 {
		req.Fallbacks = fb
	}
	return req, nil
}

// buildImageVariationRequest constructs an OpenAIImageVariationRequest from a
// parsed multipart form. Variation uses only the first image.
func buildImageVariationRequest(form *multipart.Form) (*openai.OpenAIImageVariationRequest, error) {
	req := &openai.OpenAIImageVariationRequest{}

	model, err := requiredValue(form, "model")
	if err != nil {
		return nil, err
	}
	req.Model = model

	images, err := readImageFiles(form)
	if err != nil {
		return nil, err
	}
	req.Input = &schemas.ImageVariationInput{Image: images[0]}

	if v, ok := stringValue(form, "size"); ok {
		req.Size = &v
	}
	if v, ok := stringValue(form, "response_format"); ok {
		req.ResponseFormat = &v
	}
	if v, ok := stringValue(form, "user"); ok {
		req.User = &v
	}
	if err := setIntValue(form, "n", &req.N); err != nil {
		return nil, err
	}
	if fb := form.Value["fallbacks"]; len(fb) > 0 {
		req.Fallbacks = fb
	}
	return req, nil
}

// readImageFiles reads every uploaded image part (accepting both "image[]" and
// "image" field names) into ImageInput values. At least one image is required.
func readImageFiles(form *multipart.Form) ([]schemas.ImageInput, error) {
	headers := form.File["image[]"]
	if len(headers) == 0 {
		headers = form.File["image"]
	}
	if len(headers) == 0 {
		return nil, errRequired("image")
	}
	images := make([]schemas.ImageInput, 0, len(headers))
	for _, fh := range headers {
		data, err := readFileHeader(fh)
		if err != nil {
			return nil, err
		}
		images = append(images, schemas.ImageInput{Image: data})
	}
	return images, nil
}

// readFile reads the first uploaded file under field, or (nil, nil) if absent.
func readFile(form *multipart.Form, field string) ([]byte, error) {
	headers := form.File[field]
	if len(headers) == 0 {
		return nil, nil
	}
	return readFileHeader(headers[0])
}

// readFileHeader opens and fully reads a multipart file part.
func readFileHeader(fh *multipart.FileHeader) ([]byte, error) {
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

// requiredValue returns the first value for field, or an error if missing/empty.
func requiredValue(form *multipart.Form, field string) (string, error) {
	if v, ok := stringValue(form, field); ok {
		return v, nil
	}
	return "", errRequired(field)
}

// stringValue returns the first non-empty value for field.
func stringValue(form *multipart.Form, field string) (string, bool) {
	vals := form.Value[field]
	if len(vals) == 0 || vals[0] == "" {
		return "", false
	}
	return vals[0], true
}

// setIntValue parses field as an int into dst when present; a malformed value
// is an error.
func setIntValue(form *multipart.Form, field string, dst **int) error {
	v, ok := stringValue(form, field)
	if !ok {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return wrapInvalid(field)
	}
	*dst = &n
	return nil
}
