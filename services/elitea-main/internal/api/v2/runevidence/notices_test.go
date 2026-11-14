package runevidence

import (
	"encoding/json"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// TestGuardrailNoticesMergeTheRunsVerdicts: the chat gets one typed notice per
// guardrail and kind, with the labels merged and the requests counted, in
// first-seen order, and nothing a person would read as content.
func TestGuardrailNoticesMergeTheRunsVerdicts(t *testing.T) {
	store := chain(t, []byte(`{"q":1}`))
	at := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	store.notices = []repos.GuardrailNoticeVerdict{
		{OccurredAt: at, Stage: "input", Row: "pii", Detector: "personal_data", Action: "mask", Outcome: "masked", Labels: []string{"email"}},
		{OccurredAt: at.Add(time.Second), Stage: "input", Row: "pii", Detector: "personal_data", Action: "mask", Outcome: "masked", Labels: []string{"iban", "email"}},
		{OccurredAt: at.Add(2 * time.Second), Stage: "output", Row: "heads-up", Detector: "secrets", Action: "warn", Outcome: "warned", Labels: []string{"jwt"}},
		{OccurredAt: at.Add(3 * time.Second), Stage: "input", Row: "keys", Detector: "secrets", Action: "block", Outcome: "blocked", Labels: []string{"github-token", "slot:prompt_injection"}},
		{OccurredAt: at.Add(4 * time.Second), Stage: "input", Row: "cloud", Detector: "azure", Action: "block", Outcome: "error_closed", Labels: []string{"detector_error"}},
		{OccurredAt: at.Add(5 * time.Second), Stage: "input", Row: "audit", Detector: "regex", Action: "flag", Outcome: "flagged", Labels: []string{"x"}},
	}
	recorder := serve(t, NoticesPath, "/guardrail_notices/prompt_lib/3/run-1", NewHandler(store, nil).GuardrailNotices)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d %s", recorder.Code, recorder.Body)
	}
	got := decode[NoticesResponse](t, recorder)
	if got.RunID != "run-1" || len(got.Notices) != 4 {
		t.Fatalf("notices = %+v", got)
	}
	pii := got.Notices[0]
	if pii.Kind != NoticeRedacted || pii.Guardrail != "pii" || pii.Stage != "input" || pii.Action != "mask" ||
		pii.Requests != 2 || strings.Join(pii.Labels, ",") != "email,iban" || !pii.FirstSeenAt.Equal(at) {
		t.Fatalf("redacted notice = %+v", pii)
	}
	if n := got.Notices[1]; n.Kind != NoticeWarned || n.Stage != "output" || n.Action != "warn" {
		t.Fatalf("warned notice = %+v", n)
	}
	if n := got.Notices[2]; n.Kind != NoticeBlocked || strings.Join(n.Labels, ",") != "github-token" {
		t.Fatalf("blocked notice = %+v (the slot label is a detector detail, not a finding)", n)
	}
	if n := got.Notices[3]; n.Kind != NoticeUnavailable {
		t.Fatalf("unavailable notice = %+v", n)
	}
	for _, n := range got.Notices {
		if n.Kind == "flagged" || n.Guardrail == "audit" {
			t.Fatalf("a flag verdict became a notice: %+v", n)
		}
	}
	var raw struct {
		Notices []map[string]any `json:"notices"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for key := range raw.Notices[0] {
		if strings.Contains(key, "content") || strings.Contains(key, "text") || strings.Contains(key, "match") || strings.Contains(key, "value") {
			t.Fatalf("notice field %q could carry content", key)
		}
	}
}

func TestGuardrailNoticesOfAnUnknownRunAreEmpty(t *testing.T) {
	store := chain(t, []byte(`{"q":1}`))
	recorder := serve(t, NoticesPath, "/guardrail_notices/prompt_lib/3/other-run", NewHandler(store, nil).GuardrailNotices)
	got := decode[map[string]any](t, recorder)
	notices, ok := got["notices"].([]any)
	if recorder.Code != http.StatusOK || !ok || len(notices) != 0 {
		t.Fatalf("%d %s: an empty list must be [], not null", recorder.Code, recorder.Body)
	}
}

func TestGuardrailNoticesWithoutStoreAre503(t *testing.T) {
	recorder := serve(t, NoticesPath, "/guardrail_notices/prompt_lib/3/run-1", NewHandler(nil, nil).GuardrailNotices)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", recorder.Code)
	}
}

// TestGuardrailNoticesAreScopedToTheCaller: another member of the project reads
// nothing of a run that is not theirs, and a caller without a user is refused.
func TestGuardrailNoticesAreScopedToTheCaller(t *testing.T) {
	store := chain(t, []byte(`{"q":1}`))
	store.notices = []repos.GuardrailNoticeVerdict{{Stage: "input", Row: "keys", Detector: "secrets", Action: "block", Outcome: "blocked", Labels: []string{"jwt"}}}
	handler := NewHandler(store, nil)

	router := chi.NewRouter()
	router.Get(NoticesPath, handler.GuardrailNotices)
	as := func(user auth.User) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/guardrail_notices/prompt_lib/3/run-1", nil)
		request = request.WithContext(auth.ContextWithUser(request.Context(), user))
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		return recorder
	}
	owner := decode[NoticesResponse](t, as(auth.User{ID: "8", UserID: "8"}))
	if len(owner.Notices) != 1 {
		t.Fatalf("the owner's notices = %+v", owner)
	}
	other := as(auth.User{ID: "9", UserID: "9"})
	if got := decode[NoticesResponse](t, other); other.Code != http.StatusOK || len(got.Notices) != 0 {
		t.Fatalf("another member read %d %s", other.Code, other.Body)
	}
	if anonymous := as(auth.User{}); anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("no user = %d", anonymous.Code)
	}
}

// TestGuardrailNoticesSumTheGroupsRequests: SQL aggregation hands one row per
// group with its request count; the merge sums them.
func TestGuardrailNoticesSumTheGroupsRequests(t *testing.T) {
	store := chain(t, []byte(`{"q":1}`))
	store.notices = []repos.GuardrailNoticeVerdict{
		{Stage: "input", Row: "pii", Detector: "p", Action: "mask", Outcome: "masked", Labels: []string{"email"}, Requests: 40},
		{Stage: "input", Row: "pii", Detector: "p", Action: "mask", Outcome: "masked", Labels: []string{"iban"}, Requests: 2},
	}
	got := decode[NoticesResponse](t, serve(t, NoticesPath, "/guardrail_notices/prompt_lib/3/run-1", NewHandler(store, nil).GuardrailNotices))
	if len(got.Notices) != 1 || got.Notices[0].Requests != 42 || strings.Join(got.Notices[0].Labels, ",") != "email,iban" {
		t.Fatalf("notices = %+v", got.Notices)
	}
}
