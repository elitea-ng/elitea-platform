package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	toolkitexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
	"net/http"
	"strings"
	"time"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func (h *Handler) streamToolCall(w http.ResponseWriter, r *http.Request, schema string, s scope, message rpcMessage) {
	if len(message.ID) > 1024 {
		writeRPC(w, http.StatusOK, newError(message.ID, codeInvalidRequest, "request id is too large for streaming"))
		return
	}
	var cursor resumeCursor
	started := false
	begin := func(projectID, actorID int64, tool Tool) {
		cursor = resumeCursor{StreamID: uuid.NewString(), ProjectID: projectID, ActorID: actorID, Scope: chi.URLParam(r, "*"), RequestID: message.ID, ToolName: tool.Name, ExpiresAt: time.Now().Add(resumeCursorLifetime).Unix()}
	}
	prime := func() bool {
		encoded, err := h.resumeCodec.seal(cursor, time.Now())
		if err != nil {
			return false
		}
		started = true
		return startResumeStream(w, encoded) == nil
	}
	response := h.callToolWithObservers(r, schema, s, message,
		func(ctx context.Context, schema string, projectID, actorID int64, outcome agentexecutionapp.CurrentApplicationStartOutcome, tool Tool) map[string]any {
			begin(projectID, actorID, tool)
			cursor.ExecutionID = outcome.ExecutionID
			cursor.ResponseMessageID = outcome.ResponseMessageID
			cursor.ApplicationID = tool.applicationID
			cursor.ApplicationVersionID = tool.applicationVersionID
			if !prime() {
				return errorResult("the admitted run cannot be observed through this connection")
			}
			return h.awaitRunResultWithMode(ctx, schema, outcome, tool, true)
		},
		func(ctx context.Context, projectID, actorID int64, tool Tool, reference toolkitexecutionapp.ReadToolResultReference, service ToolkitResumeUseCase) map[string]any {
			begin(projectID, actorID, tool)
			cursor.ExecutionID = reference.ExecutionID
			cursor.ToolkitID = tool.toolkitID
			cursor.ToolkitResult = &reference
			if !prime() {
				return errorResult("the admitted run cannot be observed through this connection")
			}
			return observeToolkitResult(ctx, service, reference, tool.Name)
		})
	if !started {
		writeRPC(w, http.StatusOK, response)
		return
	}
	if r.Context().Err() != nil {
		return
	}
	h.finishResumeStream(w, cursor, response)
}

func observeToolkitResult(ctx context.Context, service ToolkitResumeUseCase, reference toolkitexecutionapp.ReadToolResultReference, name string) map[string]any {
	deadline, cancel := context.WithTimeout(ctx, mcpRunDeadline)
	defer cancel()
	outcome, err := service.WaitForResult(deadline, reference)
	if err != nil {
		if errors.Is(err, toolkitexecutionapp.ErrToolkitExecuteReadResultMismatch) {
			return errorResult("the durable toolkit result does not match this invocation")
		}
		if errors.Is(err, toolkitexecutionapp.ErrToolkitExecuteReadFailed) {
			return errorResult("the durable toolkit invocation failed")
		}
		return nil
	}
	return toolkitResult(name, outcome.Completion.ResultJSON)
}

func startResumeStream(w http.ResponseWriter, cursor string) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	if _, err := fmt.Fprintf(w, "id: %s\ndata:\nretry: 1000\n\n", cursor); err != nil {
		return err
	}
	return http.NewResponseController(w).Flush()
}

func (h *Handler) finishResumeStream(w http.ResponseWriter, cursor resumeCursor, response rpcResponse) {
	result, isToolResult := response.Result.(map[string]any)
	if response.Error == nil && (response.Result == nil || (isToolResult && result == nil)) {
		return
	}
	cursor.Complete = true
	encoded, err := h.resumeCodec.seal(cursor, time.Now())
	if err != nil {
		return
	}
	data, err := json.Marshal(response)
	if err != nil {
		return
	}
	if _, err = fmt.Fprintf(w, "id: %s\nevent: message\ndata: %s\n\n", encoded, data); err != nil {
		return
	}
	_ = http.NewResponseController(w).Flush()
}

func (h *Handler) resumeToolStream(w http.ResponseWriter, r *http.Request, schema string) {
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		http.Error(w, "SSE response required", http.StatusNotAcceptable)
		return
	}
	cursor, err := h.resumeCodec.open(r.Header.Get("Last-Event-ID"), time.Now())
	projectID, ok := runProjectID(r)
	if err != nil || !ok || cursor.ProjectID != projectID || cursor.Scope != chi.URLParam(r, "*") {
		http.Error(w, "resume cursor unavailable", http.StatusNotFound)
		return
	}
	actorID, refusal := h.authorizeRun(r, projectID)
	if refusal != nil || actorID != cursor.ActorID {
		http.Error(w, "resume cursor unavailable", http.StatusForbidden)
		return
	}
	s, err := parseScope(cursor.Scope)
	if err != nil {
		http.Error(w, "resume cursor unavailable", http.StatusNotFound)
		return
	}
	tools, err := h.source.tools(r.Context(), schema, s)
	if err != nil {
		http.Error(w, "resume lookup unavailable", http.StatusServiceUnavailable)
		return
	}
	var target Tool
	found := false
	for _, tool := range tools {
		agentMatch := cursor.ToolkitResult == nil && tool.runnableAgent() && tool.applicationID == cursor.ApplicationID && tool.applicationVersionID == cursor.ApplicationVersionID
		toolkitMatch := cursor.ToolkitResult != nil && tool.runnableToolkitTool() && tool.toolkitID == cursor.ToolkitID && tool.toolkitToolName == cursor.ToolkitResult.ToolName
		if (agentMatch || toolkitMatch) && tool.Name == cursor.ToolName {
			target = tool
			found = true
			break
		}
	}
	if !found {
		http.Error(w, "resume cursor unavailable", http.StatusForbidden)
		return
	}
	if cursor.Complete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	service, toolkitReady := h.toolkitExecute.(ToolkitResumeUseCase)
	if cursor.ToolkitResult != nil && !toolkitReady {
		http.Error(w, "toolkit observation unavailable", http.StatusServiceUnavailable)
		return
	}
	if startResumeStream(w, r.Header.Get("Last-Event-ID")) != nil {
		return
	}
	var result map[string]any
	if cursor.ToolkitResult != nil {
		result = observeToolkitResult(r.Context(), service, *cursor.ToolkitResult, target.Name)
	} else {
		result = h.awaitRunResultWithMode(r.Context(), schema, agentexecutionapp.CurrentApplicationStartOutcome{ExecutionID: cursor.ExecutionID, ResponseMessageID: cursor.ResponseMessageID}, target, true)
	}
	if r.Context().Err() != nil {
		return
	}
	h.finishResumeStream(w, cursor, newResult(cursor.RequestID, result))
}
