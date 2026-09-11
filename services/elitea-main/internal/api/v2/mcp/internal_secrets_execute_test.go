package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type recordingInternalSecretHandler struct {
	request   *http.Request
	body      map[string]any
	operation string
	calls     int
}

func (handler *recordingInternalSecretHandler) List(writer http.ResponseWriter, request *http.Request) {
	handler.record(writer, request, "list")
}

func (handler *recordingInternalSecretHandler) Create(writer http.ResponseWriter, request *http.Request) {
	handler.decodeAndRecord(writer, request, "create")
}

func (handler *recordingInternalSecretHandler) Update(writer http.ResponseWriter, request *http.Request) {
	handler.decodeAndRecord(writer, request, "update")
}

func (handler *recordingInternalSecretHandler) decodeAndRecord(
	writer http.ResponseWriter,
	request *http.Request,
	operation string,
) {
	if err := json.NewDecoder(request.Body).Decode(&handler.body); err != nil {
		http.Error(writer, "invalid body", http.StatusBadRequest)
		return
	}
	handler.record(writer, request, operation)
}

func (handler *recordingInternalSecretHandler) record(
	writer http.ResponseWriter,
	request *http.Request,
	operation string,
) {
	handler.calls++
	handler.request = request
	handler.operation = operation
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write([]byte(`{"ok":true}`))
}

func TestInternalSecretOperationsUsePathProjectAndAuthenticatedActor(t *testing.T) {
	for _, test := range []struct {
		operation internalSecretOperation
		method    string
		name      string
		arguments map[string]any
	}{
		{internalListSecrets, http.MethodGet, "list", nil},
		{internalCreateSecret, http.MethodPost, "create", map[string]any{
			"name": "API_TOKEN", "value": "private-marker", "unexpected": "blocked",
		}},
		{internalUpdateSecret, http.MethodPut, "update", map[string]any{
			"secret": "API_TOKEN", "name": "RENAMED", "value": "new-private-marker", "unexpected": "blocked",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := &recordingInternalSecretHandler{}
			executor := newHandlerInternalSecretExecutor(handler)
			result, err := executor.Execute(context.Background(), 7, 41, test.operation, test.arguments)
			if err != nil || result.status != http.StatusOK || handler.calls != 1 ||
				handler.operation != test.name || handler.request.Method != test.method {
				t.Fatalf("status=%d error=%v calls=%d operation=%q method=%q",
					result.status, err, handler.calls, handler.operation, handler.request.Method)
			}
			if got := chi.URLParam(handler.request, "projectID"); got != "7" {
				t.Fatalf("project path ID = %q, want 7", got)
			}
			user, ok := auth.UserFromContext(handler.request.Context())
			if !ok || user.UserID != "41" {
				t.Fatalf("request actor = %+v/%v, want user 41", user, ok)
			}
			if test.operation == internalUpdateSecret {
				if got := chi.URLParam(handler.request, "name"); got != "API_TOKEN" {
					t.Fatalf("secret path name = %q, want API_TOKEN", got)
				}
				if handler.body["name"] != "API_TOKEN" || handler.body["value"] != "new-private-marker" {
					t.Fatalf("update body = %#v", handler.body)
				}
			}
			if test.operation == internalCreateSecret &&
				(handler.body["name"] != "API_TOKEN" || handler.body["value"] != "private-marker") {
				t.Fatalf("create body = %#v", handler.body)
			}
			if _, crossed := handler.body["unexpected"]; crossed {
				t.Fatalf("unexpected field crossed into request: %#v", handler.body)
			}
		})
	}
}

func TestInternalSecretValidationStopsBeforeHandlerInvocation(t *testing.T) {
	for _, test := range []struct {
		name      string
		operation internalSecretOperation
		arguments map[string]any
	}{
		{name: "create missing name", operation: internalCreateSecret, arguments: map[string]any{}},
		{name: "create invalid name", operation: internalCreateSecret, arguments: map[string]any{"name": "bad-name"}},
		{name: "update missing secret", operation: internalUpdateSecret, arguments: map[string]any{}},
		{name: "update invalid secret", operation: internalUpdateSecret, arguments: map[string]any{"secret": "../TOKEN"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := &recordingInternalSecretHandler{}
			executor := newHandlerInternalSecretExecutor(handler)
			result, err := executor.Execute(context.Background(), 7, 41, test.operation, test.arguments)
			if err != nil || result.status != http.StatusBadRequest || handler.calls != 0 {
				t.Fatalf("status=%d error=%v calls=%d body=%s", result.status, err, handler.calls, result.body)
			}
		})
	}
}

func TestInternalSecretExecutorRejectsInvalidRequestsBeforeInvocation(t *testing.T) {
	for _, test := range []struct {
		name      string
		ctx       context.Context
		projectID int64
		actorID   int64
		operation internalSecretOperation
	}{
		{name: "nil context", projectID: 7, actorID: 41, operation: internalListSecrets},
		{name: "invalid project", ctx: context.Background(), actorID: 41, operation: internalListSecrets},
		{name: "invalid actor", ctx: context.Background(), projectID: 7, operation: internalListSecrets},
		{name: "unknown operation", ctx: context.Background(), projectID: 7, actorID: 41, operation: "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := &recordingInternalSecretHandler{}
			executor := newHandlerInternalSecretExecutor(handler)
			if _, err := executor.Execute(test.ctx, test.projectID, test.actorID, test.operation, nil); err == nil {
				t.Fatal("invalid request unexpectedly succeeded")
			}
			if handler.calls != 0 {
				t.Fatalf("handler calls = %d", handler.calls)
			}
		})
	}
}

func FuzzInternalSecretNameMatchesPublishedSchema(f *testing.F) {
	for _, seed := range []string{"A", "API_TOKEN", "abc123", "bad-name", "../TOKEN", "", "é"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		_, refusal := internalSecretName(map[string]any{"name": name}, "name")
		accepted := refusal == nil
		want := len(name) >= 1 && len(name) <= 128
		if want {
			for _, char := range name {
				if (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') &&
					(char < '0' || char > '9') && char != '_' {
					want = false
					break
				}
			}
		}
		if accepted != want {
			t.Fatalf("accepted=%v want=%v name=%q", accepted, want, name)
		}
	})
}
