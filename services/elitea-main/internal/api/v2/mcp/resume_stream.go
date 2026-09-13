package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func (h *Handler) streamAgentCall(w http.ResponseWriter, r *http.Request, schema string, s scope, message rpcMessage) {
	if len(message.ID) > 1024 {
		writeRPC(w, http.StatusOK, newError(message.ID, codeInvalidRequest, "request id is too large for streaming"))
		return
	}
	var cursor resumeCursor
	started := false
	response := h.callToolWithObserver(r, schema, s, message, func(ctx context.Context, schema string, projectID, actorID int64, outcome agentexecutionapp.CurrentApplicationStartOutcome, tool Tool) map[string]any {
		cursor = resumeCursor{StreamID: uuid.NewString(), ProjectID: projectID, ActorID: actorID, Scope: chi.URLParam(r, "*"), RequestID: message.ID, ExecutionID: outcome.ExecutionID, ResponseMessageID: outcome.ResponseMessageID, ApplicationID: tool.applicationID, ApplicationVersionID: tool.applicationVersionID, ToolName: tool.Name, ExpiresAt: time.Now().Add(resumeCursorLifetime).Unix()}
		encoded, err := h.resumeCodec.seal(cursor, time.Now())
		if err != nil {
			return errorResult("the admitted run cannot be observed through this connection")
		}
		started = true
		if startResumeStream(w, encoded) != nil {
			return nil
		}
		return h.awaitRunResult(ctx, schema, outcome, tool)
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

func (h *Handler) resumeAgentStream(w http.ResponseWriter, r *http.Request, schema string) {
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
		if tool.runnableAgent() && tool.applicationID == cursor.ApplicationID && tool.applicationVersionID == cursor.ApplicationVersionID && tool.Name == cursor.ToolName {
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
	if startResumeStream(w, r.Header.Get("Last-Event-ID")) != nil {
		return
	}
	result := h.awaitRunResult(r.Context(), schema, agentexecutionapp.CurrentApplicationStartOutcome{ExecutionID: cursor.ExecutionID, ResponseMessageID: cursor.ResponseMessageID}, target)
	if r.Context().Err() != nil {
		return
	}
	h.finishResumeStream(w, cursor, newResult(cursor.RequestID, result))
}
