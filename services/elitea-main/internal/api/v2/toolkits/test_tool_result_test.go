package toolkits

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	"github.com/go-chi/chi/v5"
)

type recoveryToolRuns struct {
	stubToolRuns
	read      toolkitcalltoolapp.ResultRequest
	readCalls int
	readErr   error
}

func (s *recoveryToolRuns) ReadToolRun(_ context.Context, request toolkitcalltoolapp.ResultRequest) (toolkitcalltoolapp.RunOutcome, bool, error) {
	s.read, s.readCalls = request, s.readCalls+1
	return toolkitcalltoolapp.RunOutcome{ExecutionID: "accepted-execution"}, true, s.readErr
}

func TestToolkitResultLookupReturnsActualExecutionWithoutSubmission(t *testing.T) {
	for _, mode := range []string{"request", "execution", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			runs := &recoveryToolRuns{}
			h := &Handler{toolRuns: runs}
			r := newToolRunRequest(t, "/result?lookup="+mode, "1", "19", "")
			r.Method = http.MethodGet
			chi.RouteContext(r.Context()).URLParams.Add("executionID", "receipt-key")
			w := httptest.NewRecorder()
			h.TestToolResult(w, r)
			if runs.calls != 0 {
				t.Fatal("lookup submitted a tool call")
			}
			if mode == "invalid" {
				if w.Code != 400 || runs.readCalls != 0 {
					t.Fatal("invalid lookup reached the reader")
				}
				return
			}
			if w.Code != 202 || !strings.Contains(w.Body.String(), `"task_id":"accepted-execution"`) || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("response: %d %s", w.Code, w.Body.String())
			}
			if (mode == "request") != (runs.read.RequestKey == "receipt-key") || runs.read.ActorUserID != 7 {
				t.Fatalf("incorrect lookup identity: %+v", runs.read)
			}
			runs.readErr = toolkitcalltoolapp.ErrToolRunNotFound
			w = httptest.NewRecorder()
			h.TestToolResult(w, r)
			if w.Code != 404 {
				t.Fatalf("missing execution: %d", w.Code)
			}
		})
	}
}
