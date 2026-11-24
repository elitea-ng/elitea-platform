package llmproxy

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// multipartBody builds a multipart form. fields are name→value text parts;
// files are name→content file parts (used for image/mask uploads).
func multipartBody(t *testing.T, fields map[string]string, files map[string][]byte) (body *bytes.Buffer, contentType string) {
	t.Helper()
	body = &bytes.Buffer{}
	w := multipart.NewWriter(body)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatalf("write field %s: %v", k, err)
		}
	}
	for name, content := range files {
		fw, err := w.CreateFormFile(name, name+".png")
		if err != nil {
			t.Fatalf("create file %s: %v", name, err)
		}
		if _, err := fw.Write(content); err != nil {
			t.Fatalf("write file %s: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return body, w.FormDataContentType()
}

// parseForm runs the multipart body through net/http and returns the parsed form.
func parseForm(t *testing.T, body *bytes.Buffer, contentType string) *multipart.Form {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/images/edits", body)
	req.Header.Set("Content-Type", contentType)
	if err := req.ParseMultipartForm(maxMultipartMemory); err != nil {
		t.Fatalf("ParseMultipartForm: %v", err)
	}
	return req.MultipartForm
}

func TestBuildImageEditRequest_Full(t *testing.T) {
	body, ct := multipartBody(t,
		map[string]string{
			"model": "gpt-image-1", "prompt": "make it blue",
			"size": "1024x1024", "quality": "high", "response_format": "b64_json",
			"background": "transparent", "input_fidelity": "high", "output_format": "png",
			"negative_prompt": "no red", "user": "u1", "type": "edit",
			"n": "2", "partial_images": "1", "num_inference_steps": "30",
			"seed": "7", "output_compression": "80", "stream": "false",
		},
		map[string][]byte{"image[]": []byte("PNGDATA"), "mask": []byte("MASKDATA")},
	)
	form := parseForm(t, body, ct)

	req, err := buildImageEditRequest(form)
	if err != nil {
		t.Fatalf("buildImageEditRequest: %v", err)
	}
	if req.Model != "gpt-image-1" {
		t.Errorf("model = %q", req.Model)
	}
	if req.Input == nil || req.Input.Prompt != "make it blue" {
		t.Errorf("prompt not set: %+v", req.Input)
	}
	if len(req.Input.Images) != 1 || string(req.Input.Images[0].Image) != "PNGDATA" {
		t.Errorf("image not read: %+v", req.Input)
	}
	if len(req.Mask) == 0 || string(req.Mask) != "MASKDATA" {
		t.Errorf("mask not read: %v", req.Mask)
	}
	if req.N == nil || *req.N != 2 {
		t.Errorf("n = %v, want 2", req.N)
	}
	if req.Seed == nil || *req.Seed != 7 {
		t.Errorf("seed = %v, want 7", req.Seed)
	}
	if req.Size == nil || *req.Size != "1024x1024" {
		t.Errorf("size = %v", req.Size)
	}
	if req.Stream == nil || *req.Stream != false {
		t.Errorf("stream = %v, want false", req.Stream)
	}
}

func TestBuildImageEditRequest_AcceptsBareImageField(t *testing.T) {
	body, ct := multipartBody(t,
		map[string]string{"model": "m", "prompt": "p"},
		map[string][]byte{"image": []byte("DATA")},
	)
	form := parseForm(t, body, ct)

	req, err := buildImageEditRequest(form)
	if err != nil {
		t.Fatalf("buildImageEditRequest: %v", err)
	}
	if len(req.Input.Images) != 1 {
		t.Errorf("bare 'image' field not accepted: %+v", req.Input)
	}
}

func TestBuildImageEditRequest_MissingModel(t *testing.T) {
	body, ct := multipartBody(t,
		map[string]string{"prompt": "p"},
		map[string][]byte{"image": []byte("DATA")},
	)
	form := parseForm(t, body, ct)

	_, err := buildImageEditRequest(form)
	if err == nil || !strings.Contains(err.Error(), "model") {
		t.Errorf("err = %v, want model-required", err)
	}
}

func TestBuildImageEditRequest_MissingPrompt(t *testing.T) {
	body, ct := multipartBody(t,
		map[string]string{"model": "m"},
		map[string][]byte{"image": []byte("DATA")},
	)
	form := parseForm(t, body, ct)

	_, err := buildImageEditRequest(form)
	if err == nil || !strings.Contains(err.Error(), "prompt") {
		t.Errorf("err = %v, want prompt-required", err)
	}
}

func TestBuildImageEditRequest_MissingImage(t *testing.T) {
	body, ct := multipartBody(t, map[string]string{"model": "m", "prompt": "p"}, nil)
	form := parseForm(t, body, ct)

	_, err := buildImageEditRequest(form)
	if err == nil || !strings.Contains(err.Error(), "image") {
		t.Errorf("err = %v, want image-required", err)
	}
}

func TestBuildImageEditRequest_BadIntField(t *testing.T) {
	body, ct := multipartBody(t,
		map[string]string{"model": "m", "prompt": "p", "n": "notanint"},
		map[string][]byte{"image": []byte("DATA")},
	)
	form := parseForm(t, body, ct)

	_, err := buildImageEditRequest(form)
	if err == nil || !strings.Contains(err.Error(), "n") {
		t.Errorf("err = %v, want invalid-n", err)
	}
}

func TestBuildImageEditRequest_BadStreamField(t *testing.T) {
	body, ct := multipartBody(t,
		map[string]string{"model": "m", "prompt": "p", "stream": "maybe"},
		map[string][]byte{"image": []byte("DATA")},
	)
	form := parseForm(t, body, ct)

	_, err := buildImageEditRequest(form)
	if err == nil || !strings.Contains(err.Error(), "stream") {
		t.Errorf("err = %v, want invalid-stream", err)
	}
}

func TestBuildImageVariationRequest(t *testing.T) {
	body, ct := multipartBody(t,
		map[string]string{"model": "m", "size": "512x512", "response_format": "url", "user": "u", "n": "3"},
		map[string][]byte{"image[]": []byte("VARDATA")},
	)
	form := parseForm(t, body, ct)

	req, err := buildImageVariationRequest(form)
	if err != nil {
		t.Fatalf("buildImageVariationRequest: %v", err)
	}
	if req.Model != "m" {
		t.Errorf("model = %q", req.Model)
	}
	if req.Input == nil || string(req.Input.Image.Image) != "VARDATA" {
		t.Errorf("image not read: %+v", req.Input)
	}
	if req.N == nil || *req.N != 3 {
		t.Errorf("n = %v, want 3", req.N)
	}
}

func TestBuildImageVariationRequest_MissingImage(t *testing.T) {
	body, ct := multipartBody(t, map[string]string{"model": "m"}, nil)
	form := parseForm(t, body, ct)

	_, err := buildImageVariationRequest(form)
	if err == nil || !strings.Contains(err.Error(), "image") {
		t.Errorf("err = %v, want image-required", err)
	}
}

// End-to-end: a valid multipart edit request reaches the router and returns 200.
func TestImageEditEndToEnd(t *testing.T) {
	fake := &fakeRouter{imgResp: &schemas.BifrostImageGenerationResponse{}}
	h := newTestHandler(fake, nil, nil)

	body, ct := multipartBody(t,
		map[string]string{"model": "m", "prompt": "p"},
		map[string][]byte{"image[]": []byte("DATA")},
	)
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/images/edits", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	h.route().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestImageVariationEndToEnd(t *testing.T) {
	fake := &fakeRouter{imgResp: &schemas.BifrostImageGenerationResponse{}}
	h := newTestHandler(fake, nil, nil)

	body, ct := multipartBody(t,
		map[string]string{"model": "m"},
		map[string][]byte{"image[]": []byte("DATA")},
	)
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/images/variations", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	h.route().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestImageEditBadMultipartBody(t *testing.T) {
	fake := &fakeRouter{}
	h := newTestHandler(fake, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/llm/v1/images/edits", strings.NewReader("not multipart"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=xxx")
	rec := httptest.NewRecorder()
	h.route().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestImageEditMissingFieldReturns400(t *testing.T) {
	fake := &fakeRouter{}
	h := newTestHandler(fake, nil, nil)

	body, ct := multipartBody(t, map[string]string{"prompt": "p"}, map[string][]byte{"image": []byte("D")})
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/images/edits", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	h.route().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
