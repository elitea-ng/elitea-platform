package configurations

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/go-chi/chi/v5"
)

type validationAuthorizerStub struct {
	identity executionapp.AdmissionIdentity
	err      error
	calls    int
}

func (s *validationAuthorizerStub) AuthorizeValidation(_ context.Context, _, _ string) (executionapp.AdmissionIdentity, error) {
	s.calls++
	return s.identity, s.err
}

type validationSubmitterStub struct {
	request configurationapp.SubmitValidationRequest
	err     error
	calls   int
}

func (s *validationSubmitterStub) Submit(_ context.Context, request configurationapp.SubmitValidationRequest) (executionapp.AdmissionOutcome, error) {
	s.calls++
	s.request = request
	return executionapp.AdmissionOutcome{ExecutionID: "execution-1", CommandID: "command-1", Created: true}, s.err
}

func TestValidationHandlerRejectsDuplicateWrapperAndRequiresJSONMediaType(t *testing.T) {
	authorizer := &validationAuthorizerStub{identity: validAdmissionIdentity()}
	submitter := &validationSubmitterStub{}
	handler, err := NewValidationHandler(authorizer, submitter)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		contentType string
		body        string
		status      int
	}{
		{name: "valid", contentType: "application/json; charset=utf-8", body: `{"settings":{}}`, status: http.StatusAccepted},
		{name: "duplicate settings", contentType: "application/json", body: `{"settings":{},"settings":{"type":"github"}}`, status: http.StatusBadRequest},
		{name: "unknown wrapper", contentType: "application/json", body: `{"settings":{},"configuration_type":"github"}`, status: http.StatusBadRequest},
		{name: "missing media type", body: `{"settings":{}}`, status: http.StatusUnsupportedMediaType},
		{name: "wrong media type", contentType: "text/plain", body: `{"settings":{}}`, status: http.StatusUnsupportedMediaType},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			beforeSubmit := submitter.calls
			request := validationRequest(test.body, test.contentType)
			response := httptest.NewRecorder()
			handler.Submit(response, request)
			if response.Code != test.status {
				t.Fatalf("got status %d body=%s", response.Code, response.Body.String())
			}
			if test.status != http.StatusAccepted && submitter.calls != beforeSubmit {
				t.Fatal("invalid wrapper reached admission service")
			}
		})
	}
}

func TestValidationHandlerAuthorizesBeforeReadingBodyAndMapsInvalidAdmission(t *testing.T) {
	authorizer := &validationAuthorizerStub{identity: validAdmissionIdentity(), err: ErrValidationForbidden}
	submitter := &validationSubmitterStub{}
	handler, err := NewValidationHandler(authorizer, submitter)
	if err != nil {
		t.Fatal(err)
	}
	reader := &readTrackingBody{Reader: strings.NewReader(`{"settings":{}}`)}
	request := validationRequest("", "application/json")
	request.Body = reader
	response := httptest.NewRecorder()
	handler.Submit(response, request)
	if response.Code != http.StatusForbidden || reader.read {
		t.Fatalf("forbidden request read protected body: status=%d read=%v", response.Code, reader.read)
	}

	authorizer.err = nil
	submitter.err = configurationapp.ErrInvalidValidationAdmission
	response = httptest.NewRecorder()
	handler.Submit(response, validationRequest(`{"settings":{"type":"github"}}`, "application/json"))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid admission mapped to %d", response.Code)
	}
}

func TestValidationHandlerMapsAdmissionCapacityToRetryableServiceUnavailable(t *testing.T) {
	authorizer := &validationAuthorizerStub{identity: validAdmissionIdentity()}
	submitter := &validationSubmitterStub{err: &executionapp.AdmissionCapacityError{
		CapabilityID:   "configuration.validate.v1",
		MaxOutstanding: 3,
	}}
	handler, err := NewValidationHandler(authorizer, submitter)
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	handler.Submit(response, validationRequest(`{"settings":{}}`, "application/json"))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("capacity rejection mapped to status %d body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("capacity rejection Retry-After=%q, want 1", got)
	}
	if strings.Contains(response.Body.String(), "configuration.validate.v1") || strings.Contains(response.Body.String(), "3") {
		t.Fatalf("capacity response exposed internal policy state: %s", response.Body.String())
	}
}

func TestDecodeValidationAdmissionBodyRejectsTrailingJSON(t *testing.T) {
	_, err := decodeValidationAdmissionBody(strings.NewReader(`{"settings":{}} {"settings":{}}`))
	if err == nil {
		t.Fatal("trailing JSON accepted")
	}
}

func TestDecodeValidationAdmissionBodyPreservesExactSettingsBytes(t *testing.T) {
	want := []byte("{}\n")
	settings, err := decodeValidationAdmissionBody(bytes.NewReader(append(append([]byte(`{"settings":`), want...), '}')))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(settings, want) {
		t.Fatalf("settings bytes changed: got length %d, want %d", len(settings), len(want))
	}
}

type readTrackingBody struct {
	io.Reader
	read bool
}

func (r *readTrackingBody) Read(buffer []byte) (int, error) {
	r.read = true
	return r.Reader.Read(buffer)
}

func (r *readTrackingBody) Close() error { return nil }

func validAdmissionIdentity() executionapp.AdmissionIdentity {
	return executionapp.AdmissionIdentity{
		TenantID:            "tenant-1",
		ResourceProjectID:   "project-1",
		ProjectionProjectID: "project-1",
		ActorID:             "actor-1",
	}
}

func validationRequest(body, contentType string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	request.Header.Set("Idempotency-Key", "key-1")
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", "project-1")
	routeContext.URLParams.Add("configurationRevisionID", "revision-1")
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
}
