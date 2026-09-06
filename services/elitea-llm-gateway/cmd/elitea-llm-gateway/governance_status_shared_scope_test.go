package main

// The shared-scope report on GET /governance/status.
//
// It exists to close a silent, total failure: the admin panel publishes a
// platform provider into the public project's schema, and whether that
// credential ever resolves depends on THIS gateway having the same project
// configured. The two are separate environment variables in separate services —
// elitea-main reads AI_PROJECT_ID (defaulting to 1), this gateway reads
// ELITEA_AI_PROJECT_ID (defaulting to OFF) — so a mismatch produces a credential
// that resolves for nobody while every other signal on the admin screen looks
// correct.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func statusBody(t *testing.T, sharedProjectID string) governanceStatusBody {
	t.Helper()
	recorder := httptest.NewRecorder()
	makeGovernanceStatusHandler(nil, nil, sharedProjectID, nil)(
		recorder, httptest.NewRequest(http.MethodGet, "/governance/status", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var body governanceStatusBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

// TestTheStatusRouteNamesTheSharedProject — an armed scope reports WHICH
// project, not merely that one exists. "Armed" alone cannot catch the mismatch,
// which is the case this field is for.
func TestTheStatusRouteNamesTheSharedProject(t *testing.T) {
	if got := statusBody(t, "7").SharedProjectID; got != "7" {
		t.Errorf("shared_project_id = %q, want %q", got, "7")
	}
}

// TestAnUnarmedSharedScopeReportsEmpty — and empty must survive the round trip
// rather than being omitted, so a client can tell "the scope is off" from "this
// gateway is too old to say".
func TestAnUnarmedSharedScopeReportsEmpty(t *testing.T) {
	recorder := httptest.NewRecorder()
	makeGovernanceStatusHandler(nil, nil, "", nil)(
		recorder, httptest.NewRequest(http.MethodGet, "/governance/status", nil))

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	encoded, present := raw["shared_project_id"]
	if !present {
		t.Fatal("shared_project_id was omitted; a client cannot tell 'off' from 'not reported'")
	}
	if string(encoded) != `""` {
		t.Errorf("shared_project_id = %s, want an empty string", encoded)
	}
}
