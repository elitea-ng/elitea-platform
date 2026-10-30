package runcaps

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workerruncaps"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

type granterStub struct {
	requests []workerruncaps.OverrideRequest
	err      error
}

func (g *granterStub) Grant(_ context.Context, request workerruncaps.OverrideRequest) (workerruncaps.OverrideGrant, error) {
	g.requests = append(g.requests, request)
	if g.err != nil {
		return workerruncaps.OverrideGrant{}, g.err
	}
	return workerruncaps.OverrideGrant{ID: 9, Cap: "max_tool_calls", SourceExecutionID: "exec-1"}, nil
}

const message = "0190f2a6-7d1c-7cc1-9a35-3f4b4d2a1b11"

func do(t *testing.T, granter Granter, body string) (*httptest.ResponseRecorder, audit.Annotation) {
	t.Helper()
	r := chi.NewRouter()
	r.Post(ProjectPath, NewHandler(granter).Grant)
	request := httptest.NewRequest(http.MethodPost, "/run_cap_overrides/prompt_lib/7", strings.NewReader(body))
	ctx, slot := audit.ContextWithAnnotationSlot(request.Context())
	ctx = auth.ContextWithUser(ctx, auth.User{ID: "42", UserID: "42"})
	recorder := httptest.NewRecorder()
	r.ServeHTTP(recorder, request.WithContext(ctx))
	annotation, _ := slot.Read()
	return recorder, annotation
}

func TestGrantRecordsAndAnnotates(t *testing.T) {
	t.Parallel()
	granter := &granterStub{}
	recorder, annotation := do(t, granter, `{"message_id":"`+message+`"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := granter.requests[0]; got.ProjectID != 7 || got.ActorUserID != 42 || got.MessageID != message {
		t.Fatalf("request = %+v", got)
	}
	var body map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	if body["cap"] != "max_tool_calls" || body["id"] != float64(9) {
		t.Fatalf("body = %v", body)
	}
	if annotation.Action != AuditAction || annotation.EntityName != "max_tool_calls" || *annotation.ProjectID != 7 {
		t.Fatalf("annotation = %+v", annotation)
	}
}

func TestGrantRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{workerruncaps.ErrOverrideInvalid, http.StatusBadRequest, "invalid_request"},
		{workerruncaps.ErrOverrideNotCapped, http.StatusNotFound, "not_capped"},
		{workerruncaps.ErrOverrideAlreadyGranted, http.StatusConflict, "already_granted"},
		{workerruncaps.ErrOverrideOpenGrant, http.StatusConflict, "open_grant"},
		{workerruncaps.ErrOverrideNeedsTierCaps, http.StatusUnprocessableEntity, "tier_caps_not_saved"},
		{fmt.Errorf("wrapped: %w", workerruncaps.ErrOverrideOpenGrant), http.StatusConflict, "open_grant"},
	} {
		recorder, _ := do(t, &granterStub{err: tc.err}, `{"message_id":"`+message+`"}`)
		var body map[string]string
		_ = json.Unmarshal(recorder.Body.Bytes(), &body)
		if recorder.Code != tc.status || body["code"] != tc.code {
			t.Errorf("%v: %d %v", tc.err, recorder.Code, body)
		}
	}
	if recorder, _ := do(t, &granterStub{}, `{"message_id":"x","extra":1}`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("an unknown field = %d", recorder.Code)
	}
	if recorder, _ := do(t, nil, `{}`); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("no service = %d", recorder.Code)
	}
}
