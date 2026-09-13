package mcp_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/mcp"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
)

// The database is real; admission is a double. No worker restart is claimed here.
func TestMCPAgentResponseResumesAfterHandlerReplacementPostgres(t *testing.T) {
	pool := newMCPPool(t)
	seedUser(t, pool, callerUserID)
	seedAgent(t, pool, homeSchema, "Recovery Agent", "recovery test", "mcp")
	const responseID = "fa3799b2-08a3-4cbe-8038-c3beac807381"
	start := &fakeStart{outcome: agentexecutionapp.CurrentApplicationStartOutcome{ExecutionID: "resume-execution", ResponseMessageID: responseID, Created: true}}
	start.onStart = func(request agentexecutionapp.CurrentApplicationStartRequest) {
		projectAnswer(t, pool, homeSchema, request.ConversationUUID, responseID, completedMeta, "EXACT_RECOVERED_RESULT")
	}
	key := bytes.Repeat([]byte{71}, 32)
	codec, err := mcp.NewResumeCursorCodec(key)
	if err != nil {
		t.Fatal(err)
	}
	handler := mcp.NewHandler(pool, nil, start, allowRuns{userID: callerUserID}, mcp.WithResumeCursorCodec(codec))
	target := "/app/" + homeProject + "/mcp"
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(`{"jsonrpc":"2.0","id":"original-call","method":"tools/call","params":{"name":"Recovery_Agent","arguments":{"task":"test recovery"}}}`))
	request.Header.Set("Accept", "application/json, text/event-stream")
	response := httptest.NewRecorder()
	newRouter(handler, callerUserID).ServeHTTP(response, request)
	if response.Code != 200 || response.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("POST: %d %s", response.Code, response.Body.String())
	}
	var cursor string
	var initialReply string
	for _, line := range strings.Split(response.Body.String(), "\n") {
		if cursor == "" && strings.HasPrefix(line, "id: ") {
			cursor = strings.TrimPrefix(line, "id: ")
		}
		if strings.HasPrefix(line, "data: {") {
			initialReply = strings.TrimPrefix(line, "data: ")
		}
	}
	if cursor == "" || initialReply == "" {
		t.Fatal("POST omitted priming cursor or final response")
	}
	// Drop the initial final response. Replace the entire handler and codec.
	codec, err = mcp.NewResumeCursorCodec(key)
	if err != nil {
		t.Fatal(err)
	}
	handler = mcp.NewHandler(pool, nil, nil, allowRuns{userID: callerUserID}, mcp.WithResumeCursorCodec(codec))
	request = httptest.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Last-Event-ID", cursor)
	response = httptest.NewRecorder()
	newRouter(handler, callerUserID).ServeHTTP(response, request)
	var resumedReply string
	for _, line := range strings.Split(response.Body.String(), "\n") {
		if strings.HasPrefix(line, "data: {") {
			resumedReply = strings.TrimPrefix(line, "data: ")
		}
	}
	if response.Code != 200 || resumedReply != initialReply || len(start.requests) != 1 {
		t.Fatalf("GET: status=%d reply=%s admissions=%d", response.Code, resumedReply, len(start.requests))
	}
	var rpc struct {
		ID     string         `json:"id"`
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal([]byte(resumedReply), &rpc); err != nil {
		t.Fatal(err)
	}
	if rpc.ID != "original-call" || resultText(t, rpc.Result) != "EXACT_RECOVERED_RESULT" {
		t.Fatalf("wrong recovered response: %s", resumedReply)
	}
}
