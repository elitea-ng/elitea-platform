package llmproxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// TestImageVariationEndpointMissingModel exercises the ImageVariation handler's
// build-error branch: a missing required "model" field must yield a 400 before
// the request reaches the router.
func TestImageVariationEndpointMissingModel(t *testing.T) {
	fake := &fakeRouter{}
	h := newTestHandler(fake, nil, nil)

	body, ct := multipartBody(t, nil, map[string][]byte{"image[]": []byte("DATA")})
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/images/variations", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	h.route().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model") {
		t.Errorf("body should name the missing field; got %s", rec.Body.String())
	}
}

// TestImageVariationBadMultipartBody covers the parseMultipart 400 path for the
// variations endpoint (the edit endpoint's equivalent is already covered).
func TestImageVariationBadMultipartBody(t *testing.T) {
	h := newTestHandler(&fakeRouter{}, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/llm/v1/images/variations", strings.NewReader("not multipart"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=xxx")
	rec := httptest.NewRecorder()
	h.route().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestBuildImageVariationRequest_Fallbacks covers the fallbacks branch, which is
// only taken when the form carries one or more "fallbacks" values.
func TestBuildImageVariationRequest_Fallbacks(t *testing.T) {
	body, ct := multipartBody(t,
		map[string]string{"model": "m", "fallbacks": "backup-model"},
		map[string][]byte{"image[]": []byte("DATA")},
	)
	form := parseForm(t, body, ct)

	req, err := buildImageVariationRequest(form)
	if err != nil {
		t.Fatalf("buildImageVariationRequest: %v", err)
	}
	if len(req.Fallbacks) != 1 || req.Fallbacks[0] != "backup-model" {
		t.Errorf("fallbacks = %v, want [backup-model]", req.Fallbacks)
	}
}

// TestBuildImageVariationRequest_BadN covers the setIntValue error branch for a
// non-integer "n" field.
func TestBuildImageVariationRequest_BadN(t *testing.T) {
	body, ct := multipartBody(t,
		map[string]string{"model": "m", "n": "not-an-int"},
		map[string][]byte{"image[]": []byte("DATA")},
	)
	form := parseForm(t, body, ct)

	if _, err := buildImageVariationRequest(form); err == nil {
		t.Fatal("err = nil, want a parse error for non-integer n")
	}
}

// TestImageVariationEndToEnd_RouterError covers the router-error path through
// writeUnary (the endpoint returns the upstream status, e.g. 401).
func TestImageVariationEndToEnd_RouterError(t *testing.T) {
	fake := &fakeRouter{imgErr: &schemas.BifrostError{
		StatusCode: intPtr(http.StatusUnauthorized),
		Error:      &schemas.ErrorField{Message: "bad key"},
	}}
	h := newTestHandler(fake, nil, nil)

	body, ct := multipartBody(t,
		map[string]string{"model": "m"},
		map[string][]byte{"image[]": []byte("DATA")},
	)
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/images/variations", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	h.route().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
