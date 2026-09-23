package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type recordingInternalProjectContextHandler struct {
	request   *http.Request
	body      map[string]any
	operation string
	calls     int
}

func (handler *recordingInternalProjectContextHandler) ProjectContext(
	writer http.ResponseWriter,
	request *http.Request,
) {
	handler.record(writer, request, "get")
}

func (handler *recordingInternalProjectContextHandler) UpdateProjectContext(
	writer http.ResponseWriter,
	request *http.Request,
) {
	if err := json.NewDecoder(request.Body).Decode(&handler.body); err != nil {
		http.Error(writer, "invalid body", http.StatusBadRequest)
		return
	}
	handler.record(writer, request, "put")
}

func (handler *recordingInternalProjectContextHandler) DeleteProjectContext(
	writer http.ResponseWriter,
	request *http.Request,
) {
	handler.record(writer, request, "delete")
}

func (handler *recordingInternalProjectContextHandler) record(
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

func TestInternalProjectContextOperationsUsePathProjectAndAuthenticatedActor(t *testing.T) {
	for _, test := range []struct {
		operation internalProjectContextOperation
		method    string
		name      string
	}{
		{internalGetProjectContext, http.MethodGet, "get"},
		{internalUpdateProjectContext, http.MethodPut, "put"},
		{internalDeleteProjectContext, http.MethodDelete, "delete"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := &recordingInternalProjectContextHandler{}
			executor := newHandlerInternalProjectContextExecutor(handler)
			result, err := executor.Execute(context.Background(), 7, 41, test.operation, map[string]any{
				"content": "rules", "enabled": false,
				"activation_description": "when billing is discussed",
				"project_id":             json.Number("999"),
				"author_id":              json.Number("999"),
				"unexpected":             "must-not-cross",
			})
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
			if test.operation == internalUpdateProjectContext {
				if handler.body["content"] != "rules" || handler.body["enabled"] != false ||
					handler.body["activation_description"] != "when billing is discussed" {
					t.Fatalf("update body = %#v", handler.body)
				}
				for _, forbidden := range []string{"project_id", "author_id", "unexpected"} {
					if _, crossed := handler.body[forbidden]; crossed {
						t.Fatalf("control field %q crossed into update: %#v", forbidden, handler.body)
					}
				}
			}
		})
	}
}

func TestInternalProjectContextExecutorRejectsInvalidRequestsBeforeInvocation(t *testing.T) {
	for _, test := range []struct {
		name      string
		ctx       context.Context
		projectID int64
		actorID   int64
		operation internalProjectContextOperation
	}{
		{name: "nil context", projectID: 7, actorID: 41, operation: internalGetProjectContext},
		{name: "invalid project", ctx: context.Background(), actorID: 41, operation: internalGetProjectContext},
		{name: "invalid actor", ctx: context.Background(), projectID: 7, operation: internalGetProjectContext},
		{name: "unknown operation", ctx: context.Background(), projectID: 7, actorID: 41, operation: "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := &recordingInternalProjectContextHandler{}
			executor := newHandlerInternalProjectContextExecutor(handler)
			if _, err := executor.Execute(test.ctx, test.projectID, test.actorID, test.operation, nil); err == nil {
				t.Fatal("invalid request unexpectedly succeeded")
			}
			if handler.calls != 0 {
				t.Fatalf("handler calls = %d", handler.calls)
			}
		})
	}
}
