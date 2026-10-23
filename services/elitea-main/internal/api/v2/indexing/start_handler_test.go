package indexing_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	handler "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/indexing"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	indexingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

type startUseCaseStub struct {
	request indexingapp.StartRequest
	outcome indexingapp.StartOutcome
	err     error
	calls   int
}

func (s *startUseCaseStub) StartIndexData(_ context.Context, request indexingapp.StartRequest) (indexingapp.StartOutcome, error) {
	s.calls++
	s.request = request.Clone()
	return s.outcome, s.err
}

func TestStartMapsCurrentAsyncShapeWithoutTrustingClientToolkitSettings(t *testing.T) {
	const canary = "CLIENT_TOOLKIT_SECRET_MUST_NOT_REACH_USE_CASE"
	useCase := &startUseCaseStub{outcome: indexingapp.StartOutcome{TaskID: "task-123"}}
	start, err := handler.NewStartHandler(useCase)
	if err != nil {
		t.Fatal(err)
	}
	body := `{
		"toolkit_config":{"toolkit_id":"42","type":"confluence","settings":{"password":"` + canary + `"}},
		"tool_name":"index_data",
		"tool_params":{"index_name":"docs","clean_index":false},
		"llm_model":"gpt-test",
		"llm_settings":{"temperature":0.2,"max_tokens":512},
		"stream_id":"stream-1",
		"message_id":"message-1",
		"project_id":999,
		"user_id":999,
		"project_auth_token":"` + canary + `",
		"unknown_current_extension":true
	}`
	request := currentRequest(body)
	response := httptest.NewRecorder()

	start.Start(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got := strings.TrimSpace(response.Body.String()); got != `{"task_id":"task-123"}` {
		t.Fatalf("response=%s, want exact current async task shape", got)
	}
	if useCase.calls != 1 {
		t.Fatalf("use case calls=%d, want 1", useCase.calls)
	}
	want := indexingapp.StartRequest{
		ProjectID:            7,
		ActorUserID:          11,
		ToolkitID:            42,
		ToolParameters:       json.RawMessage(`{"index_name":"docs","clean_index":false}`),
		RequestedLLMModel:    stringPointer("gpt-test"),
		RequestedLLMSettings: json.RawMessage(`{"temperature":0.2,"max_tokens":512}`),
		StreamID:             "stream-1",
		MessageID:            "message-1",
		SIOEvent:             indexingapp.CurrentIndexSIOEvent,
	}
	if !reflect.DeepEqual(useCase.request, want) {
		t.Fatalf("request=%+v\nwant=%+v", useCase.request, want)
	}
	encoded, err := json.Marshal(useCase.request)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), canary) || strings.Contains(string(encoded), "project_auth_token") {
		t.Fatalf("client credential material crossed the use-case boundary: %s", encoded)
	}
}

func TestStartDefersModelDefaultsToConfigurationsAndAcceptsResolvedIDAlias(t *testing.T) {
	useCase := &startUseCaseStub{outcome: indexingapp.StartOutcome{TaskID: "task-defaults"}}
	start, err := handler.NewStartHandler(useCase)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	start.Start(response, currentRequest(`{"toolkit_config":{"id":9},"tool_name":"index_data","tool_params":{"index_name":"docs"}}`))

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if useCase.request.ToolkitID != 9 || string(useCase.request.ToolParameters) != `{"index_name":"docs"}` ||
		useCase.request.RequestedLLMModel != nil ||
		string(useCase.request.RequestedLLMSettings) != `{"max_tokens":1024,"temperature":0.1}` {
		t.Fatalf("model defaults leaked into the HTTP boundary: %+v", useCase.request)
	}
}

func TestStartRejectsUnsupportedOrMalformedCurrentRequestsBeforeUseCase(t *testing.T) {
	valid := `{"toolkit_config":{"toolkit_id":9},"tool_name":"index_data","tool_params":{"index_name":"docs"}}`
	tests := []struct {
		name        string
		body        string
		contentType string
		query       string
		status      int
	}{
		{name: "synchronous", body: valid, contentType: "application/json", query: "await_response=true", status: http.StatusBadRequest},
		{name: "other tool", body: strings.Replace(valid, "index_data", "search_index", 1), contentType: "application/json", query: "await_response=false", status: http.StatusBadRequest},
		{name: "missing toolkit id", body: `{"toolkit_config":{"settings":{}},"tool_name":"index_data"}`, contentType: "application/json", query: "await_response=false", status: http.StatusBadRequest},
		{name: "missing index name", body: `{"toolkit_config":{"toolkit_id":9},"tool_name":"index_data","tool_params":{}}`, contentType: "application/json", query: "await_response=false", status: http.StatusBadRequest},
		{name: "tool params array", body: `{"toolkit_config":{"toolkit_id":9},"tool_name":"index_data","tool_params":[]}`, contentType: "application/json", query: "await_response=false", status: http.StatusBadRequest},
		{name: "malformed json", body: `{`, contentType: "application/json", query: "await_response=false", status: http.StatusBadRequest},
		{name: "trailing json", body: valid + `{}`, contentType: "application/json", query: "await_response=false", status: http.StatusBadRequest},
		{name: "wrong media type", body: valid, contentType: "text/plain", query: "await_response=false", status: http.StatusUnsupportedMediaType},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			useCase := &startUseCaseStub{outcome: indexingapp.StartOutcome{TaskID: "must-not-run"}}
			start, err := handler.NewStartHandler(useCase)
			if err != nil {
				t.Fatal(err)
			}
			request := currentRequestWith(test.body, test.contentType, test.query)
			response := httptest.NewRecorder()
			start.Start(response, request)
			if response.Code != test.status {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.status, response.Body.String())
			}
			if useCase.calls != 0 {
				t.Fatalf("invalid request reached use case %d times", useCase.calls)
			}
			if test.status == http.StatusBadRequest {
				var shape struct {
					Error []struct {
						Type string   `json:"type"`
						Loc  []string `json:"loc"`
						Msg  string   `json:"msg"`
					} `json:"error"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &shape); err != nil || len(shape.Error) != 1 ||
					shape.Error[0].Type != "value_error" || len(shape.Error[0].Loc) != 1 || shape.Error[0].Msg == "" {
					t.Fatalf("validation response lost current error-list shape: %s", response.Body.String())
				}
			}
		})
	}
}

func TestStartBoundsBodyAndMapsUseCaseErrorsWithoutLeakingCauses(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		outcome    indexingapp.StartOutcome
		status     int
		body       string
		retryAfter string
		noStore    bool
	}{
		{name: "toolkit invisible", err: indexingapp.ErrToolkitNotVisible, status: http.StatusNotFound, body: `{"error":"Toolkit not found"}`},
		{name: "invalid admission", err: indexingapp.ErrInvalidIndexStart, status: http.StatusBadRequest, body: `{"error":"Invalid index_data request"}`},
		{name: "same index active", err: indexingapp.NewActiveIndexConflictError("task-active"), status: http.StatusConflict, body: `{"error":"Indexing is already in progress for this index","task_id":"task-active"}`, noStore: true},
		{name: "an index-meta conflict is not an active-task disclosure", err: indexingapp.ErrCurrentIndexMetaConflict, status: http.StatusInternalServerError, body: `{"error":"Failed to start index_data"}`},
		{name: "embedding unavailable", err: fmt.Errorf("unsafe upstream: %w", indexingapp.ErrCurrentEmbeddingBindingUnavailable), status: http.StatusServiceUnavailable, body: `{"error":"Embedding model is unavailable"}`},
		{name: "capacity", err: &executionapp.AdmissionCapacityError{CapabilityID: "index.ingest.v1", MaxOutstanding: 3}, status: http.StatusServiceUnavailable, body: `{"error":"temporarily_unavailable","message":"The service is busy processing other requests. Please try again in a few seconds.","retry_after":1}`, retryAfter: "1"},
		{name: "internal", err: errors.New("database password is secret-value"), status: http.StatusInternalServerError, body: `{"error":"Failed to start index_data"}`},
		{name: "empty outcome", outcome: indexingapp.StartOutcome{}, status: http.StatusInternalServerError, body: `{"error":"No response from toolkit tool test"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			useCase := &startUseCaseStub{outcome: test.outcome, err: test.err}
			start, err := handler.NewStartHandler(useCase)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			start.Start(response, currentRequest(`{"toolkit_config":{"toolkit_id":9},"tool_name":"index_data","tool_params":{"index_name":"docs"}}`))
			if response.Code != test.status || strings.TrimSpace(response.Body.String()) != test.body {
				t.Fatalf("status/body=%d %s, want %d %s", response.Code, response.Body.String(), test.status, test.body)
			}
			if response.Header().Get("Retry-After") != test.retryAfter {
				t.Fatalf("Retry-After=%q want=%q", response.Header().Get("Retry-After"), test.retryAfter)
			}
			if got := response.Header().Get("Cache-Control"); (got == "no-store") != test.noStore {
				t.Fatalf("Cache-Control=%q no_store=%v", got, test.noStore)
			}
			if strings.Contains(response.Body.String(), "secret-value") {
				t.Fatal("internal cause leaked to caller")
			}
		})
	}

	useCase := &startUseCaseStub{outcome: indexingapp.StartOutcome{TaskID: "must-not-run"}}
	start, err := handler.NewStartHandler(useCase)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	oversized := `{"padding":"` + strings.Repeat("x", int(handler.MaxCurrentIndexStartBodyBytes)) + `","toolkit_config":{"toolkit_id":9},"tool_name":"index_data"}`
	start.Start(response, currentRequest(oversized))
	if response.Code != http.StatusRequestEntityTooLarge || useCase.calls != 0 {
		t.Fatalf("oversized status=%d calls=%d body=%s", response.Code, useCase.calls, response.Body.String())
	}
}

func currentRequest(body string) *http.Request {
	return currentRequestWith(body, "application/json", "await_response=false")
}

func currentRequestWith(body, contentType, query string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/?"+query, strings.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: "11", UserID: "11", AuthType: "user"}))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", "7")
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
}

func stringPointer(value string) *string { return &value }

type trackingBody struct {
	io.Reader
	read bool
}

func (body *trackingBody) Read(buffer []byte) (int, error) {
	body.read = true
	return body.Reader.Read(buffer)
}

func (body *trackingBody) Close() error { return nil }
