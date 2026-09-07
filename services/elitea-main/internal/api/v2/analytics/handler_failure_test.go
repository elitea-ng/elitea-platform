package analytics

// The test that fails against the shipped behaviour (issue #303).
//
// Before this change every route here answered 200 whatever the repository did,
// because the error was discarded (`summary, _ := h.repo.GetUsageSummary(...)`)
// and the KPI block was a literal of zeros. So the obvious test — "GET
// /analytics returns 200" — passed against a handler whose every query raised
// undefined_table, and would have gone on passing forever. Status alone cannot
// discriminate here; these cases assert what the response SAYS.
//
// Two properties are pinned:
//   1. A repository error must not produce a 2xx. Restore the `_` and every
//      case in TestUsageSurfacesRepositoryFailure goes red.
//   2. A successful read must not carry counts nobody computed. Put the
//      hardcoded `"unique_users": 0, "tool_runs": 0, ...` block back and
//      TestSuccessfulResponseCarriesNoUncomputedCounts goes red.
//   3. An ABSENT SOURCE and a FAILED QUERY must not carry the same status.
//      Collapse them back into one 500 and TestNoSourceIsFinalNotTransient
//      goes red. That distinction is what stops every 5xx-retrying client
//      asking twice for an answer the server has already decided it cannot
//      give — measured as 8 requests for one Analytics page load.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/analytics"
)

// stubRepo answers every method with the same outcome, so one case covers the
// route under test without hiding which method it exercised.
type stubRepo struct {
	err     error
	summary domain.UsageSummary
}

func (s stubRepo) GetUsageSummary(_ context.Context, _ domain.QueryParams) (domain.UsageSummary, error) {
	return s.summary, s.err
}

func (s stubRepo) GetAgentAnalytics(_ context.Context, _ domain.QueryParams) (domain.AgentBreakdown, error) {
	return domain.AgentBreakdown{}, s.err
}

func (s stubRepo) GetToolAnalytics(_ context.Context, _ domain.QueryParams) ([]domain.ToolAnalytics, error) {
	return nil, s.err
}

func (s stubRepo) GetUserActivity(_ context.Context, _ domain.QueryParams) ([]domain.UserActivity, bool, error) {
	return nil, false, s.err
}

func do(t *testing.T, repo Repository, target string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	NewHandler(repo).Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON (%q): %v", rec.Body.String(), err)
	}
	return rec, body
}

func TestUsageSurfacesRepositoryFailure(t *testing.T) {
	t.Parallel()

	routes := []string{
		"/",
		"/agents",
		"/agents?application_id=7", // the detail branch, which used to answer
		"/tools",                   // before consulting the repository at all
		"/tools?toolkit_id=3",
		"/users",
		"/users?user_id=9",
	}

	for _, route := range routes {
		t.Run(route, func(t *testing.T) {
			t.Parallel()
			repo := stubRepo{err: fmt.Errorf("connection refused")}
			rec, body := do(t, repo, route)

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status %d for a failed repository read, want 500", rec.Code)
			}
			// Not merely a non-200: the body must not present the failure as
			// data. A zero KPI or an empty item list here is the original
			// defect wearing a different status code.
			if _, ok := body["kpis"]; ok {
				t.Fatalf("failure response carries a kpis block: %v", body)
			}
			if _, ok := body["items"]; ok {
				t.Fatalf("failure response carries an items list: %v", body)
			}
			if body["error"] == nil || body["error"] == "" {
				t.Fatalf("failure response has no error message: %v", body)
			}
		})
	}
}

// A figure with no producer must be reported as such, not as a transient
// failure an operator — or a retrying client — will ask for again.
func TestNoSourceFailureSaysSo(t *testing.T) {
	t.Parallel()

	repo := stubRepo{err: domain.NoSourceError("usage summary", "total_tokens has no producer")}
	rec, body := do(t, repo, "/")

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501", rec.Code)
	}
	detail, _ := body["detail"].(string)
	if detail == "" {
		t.Fatalf("no-source failure carries no detail: %v", body)
	}
	// The reason has to reach the response, because the browser is the only
	// place this endpoint's failure has ever been observable from.
	if !errors.Is(domain.NoSourceError("x", "y"), domain.ErrNoSource) {
		t.Fatal("NoSourceError does not wrap ErrNoSource")
	}
	if got := body["error"]; got == "failed to query analytics" {
		t.Fatalf("a permanent absence is reported as a transient query failure: %v", got)
	}
	// A client should not have to parse prose to decide whether to retry.
	if body["code"] != "no_data_source" {
		t.Fatalf("no-source failure carries code %v, want no_data_source", body["code"])
	}
}

// The two failure kinds must be DISTINGUISHABLE, not merely both non-2xx.
//
// This is the property that fixes the doubled requests: a client classifying
// 5xx as transient retries every one of them, so an absent producer answered
// with 500 is asked for twice. 501 is final; 500 is not.
func TestNoSourceIsFinalNotTransient(t *testing.T) {
	t.Parallel()

	noSource, _ := do(t, stubRepo{err: domain.NoSourceError("usage summary", "no producer")}, "/")
	transient, _ := do(t, stubRepo{err: fmt.Errorf("connection refused")}, "/")

	if noSource.Code == transient.Code {
		t.Fatalf("an absent source and a failed query both answer %d — a client cannot tell a permanent refusal from a blip", noSource.Code)
	}
	if noSource.Code != http.StatusNotImplemented {
		t.Errorf("absent source answers %d, want 501", noSource.Code)
	}
	if transient.Code != http.StatusInternalServerError {
		t.Errorf("failed query answers %d, want 500", transient.Code)
	}
}

// The other half: when the repository DOES answer, the response must contain
// only figures that came from it.
func TestSuccessfulResponseCarriesNoUncomputedCounts(t *testing.T) {
	t.Parallel()

	repo := stubRepo{summary: domain.UsageSummary{TotalRuns: 12, TotalTokens: 3400, ActiveUsers: 4}}
	rec, body := do(t, repo, "/")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	kpis, ok := body["kpis"].(map[string]any)
	if !ok {
		t.Fatalf("no kpis block: %v", body)
	}

	// The figures that still have no producer anywhere in the platform. None of
	// them had a query behind it, and "absent" is the only honest report of
	// something nothing measured — present-and-zero is a count.
	//
	// `agent_runs` is on this list for a subtler reason than the others: it was
	// not zero, it was set to the same value as llm_calls. That is a LARGER
	// claim than zero — it asserts that every LLM call is an agent run.
	//
	// `total_cost` is absent because it has a producer that is not this
	// endpoint: /analytics_costs owns the money, with the scope rules that stop
	// it double-counting. Two views of the same dollars that can disagree is
	// worse than one view.
	for _, fabricated := range []string{
		"unique_users", "tool_runs", "chat_msgs", "agent_runs", "total_cost",
	} {
		if v, present := kpis[fabricated]; present {
			t.Errorf("kpis.%s is present as %v, but nothing computes it — an absent key is detectable, a zero is a claim", fabricated, v)
		}
	}

	// And what IS present must be the repository's own numbers, so the block
	// cannot drift back into literals.
	if kpis["llm_calls"] != float64(12) || kpis["total_tokens"] != float64(3400) || kpis["ai_active_users"] != float64(4) {
		t.Fatalf("kpis do not carry the repository's values: %v", kpis)
	}
	// The denominator was not measured, so neither it nor the rate derived from
	// it may appear. An adoption rate over an invented denominator is a
	// percentage of nothing.
	for _, derived := range []string{"total_project_users", "adoption_rate"} {
		if v, present := kpis[derived]; present {
			t.Errorf("kpis.%s is %v with no membership source", derived, v)
		}
	}
}

// The membership denominator, when it IS measured.
func TestAdoptionRateIsReportedOnlyWithItsDenominator(t *testing.T) {
	t.Parallel()

	total := int64(19)
	activeMembers := int64(9)
	repo := stubRepo{summary: domain.UsageSummary{
		TotalRuns: 5, ActiveUsers: 12, TotalProjectUsers: &total, ActiveMembers: &activeMembers,
	}}
	_, body := do(t, repo, "/")
	kpis, _ := body["kpis"].(map[string]any)

	if kpis["total_project_users"] != float64(19) {
		t.Fatalf("total_project_users %v, want 19", kpis["total_project_users"])
	}
	// 9/19 = 47.368…%, reported to one decimal: the extra digits are not a more
	// accurate statement about 9 of 19 people.
	//
	// 9, NOT the 12 in ActiveUsers. Three of those callers are not members of
	// this project — a removed member, an administrator, a service token — and
	// dividing all twelve by nineteen would report a rate over two different
	// populations. The same arithmetic with a smaller membership is how the
	// unintersected version produced "300% adoption".
	if kpis["adoption_rate"] != 47.4 {
		t.Fatalf("adoption_rate %v, want 47.4", kpis["adoption_rate"])
	}
	if kpis["ai_active_users"] != float64(12) {
		t.Fatalf("ai_active_users %v, want the unintersected 12", kpis["ai_active_users"])
	}
	if kpis["active_project_members"] != float64(9) {
		t.Fatalf("active_project_members %v, want 9", kpis["active_project_members"])
	}
}

// The rate can never exceed 100%, because its numerator is a subset of its
// denominator by construction. This is the property the intersection buys.
func TestAdoptionRateCannotExceedOneHundredPercent(t *testing.T) {
	t.Parallel()

	total, activeMembers := int64(1), int64(1)
	repo := stubRepo{summary: domain.UsageSummary{
		// Three callers, one member: the shape that used to report 300%.
		ActiveUsers: 3, TotalProjectUsers: &total, ActiveMembers: &activeMembers,
	}}
	_, body := do(t, repo, "/")
	kpis, _ := body["kpis"].(map[string]any)

	if kpis["adoption_rate"] != float64(100) {
		t.Fatalf("adoption_rate %v, want 100", kpis["adoption_rate"])
	}
	// The unintersected caller count is still reported — it is a true figure,
	// just not the numerator of a rate.
	if kpis["ai_active_users"] != float64(3) {
		t.Fatalf("ai_active_users %v, want 3", kpis["ai_active_users"])
	}
}

// A project whose membership table exists and holds nobody publishes NEITHER
// figure. 0/0 is not 0%, and "of 0" is not a denominator.
//
// The tile prints the caller count over total_project_users: with a zero there
// and one caller it read "AI ACTIVE 1 of 0 members" — one caller cannot be one
// of none. A zero denominator says the membership source did not describe this
// project rather than describing it, so the pair goes and the tile reports the
// caller count alone.
func TestAdoptionPairIsAbsentForAProjectWithNoMembers(t *testing.T) {
	t.Parallel()

	total, activeMembers := int64(0), int64(0)
	repo := stubRepo{summary: domain.UsageSummary{
		ActiveUsers:       1,
		TotalProjectUsers: &total,
		ActiveMembers:     &activeMembers,
	}}
	_, body := do(t, repo, "/")
	kpis, _ := body["kpis"].(map[string]any)

	for _, absent := range []string{"total_project_users", "active_project_members", "adoption_rate"} {
		if v, present := kpis[absent]; present {
			t.Fatalf("kpis.%s is %v for a project with no members", absent, v)
		}
	}
	// The caller count survives: it is a true figure about real usage.
	if kpis["ai_active_users"] != float64(1) {
		t.Fatalf("ai_active_users %v, want 1", kpis["ai_active_users"])
	}
}

// An empty result must render as `[]`, never `null`: a client that maps over
// the value sees a different thing, and only one of them means "no rows".
func TestEmptyCollectionsAreArraysNotNull(t *testing.T) {
	t.Parallel()

	_, body := do(t, stubRepo{}, "/")
	for _, key := range []string{"models", "daily_activity", "top_ai_users"} {
		if _, ok := body[key].([]any); !ok {
			t.Errorf("%s is %#v, want an empty array", key, body[key])
		}
	}

	_, users := do(t, stubRepo{}, "/users")
	if _, ok := users["items"].([]any); !ok {
		t.Errorf("items is %#v, want an empty array", users["items"])
	}
	// The completeness signal must be present even on the happy path: the
	// client paginates over `items` and needs to know whether it has all of
	// them. An absent flag would read as false and be indistinguishable from a
	// server that does not send one.
	if _, ok := users["truncated"].(bool); !ok {
		t.Errorf("truncated is %#v, want a boolean", users["truncated"])
	}
}

// The detail branches used to answer with an eight-field zero KPI block before
// touching the repository. Their success path must not resurrect it.
func TestDetailBranchesCarryNoZeroKpiBlock(t *testing.T) {
	t.Parallel()

	for _, route := range []string{"/agents?application_id=7", "/tools?toolkit_id=3", "/users?user_id=9"} {
		t.Run(route, func(t *testing.T) {
			t.Parallel()
			rec, body := do(t, stubRepo{}, route)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d, want 200", rec.Code)
			}
			if kpis, present := body["kpis"]; present {
				t.Fatalf("detail response still carries an uncomputed kpis block: %v", kpis)
			}
		})
	}
}

// The date window, and the two spellings these routes accept.
//
// `start_date`/`end_date` and `date_from`/`date_to` both reach the same window.
// The second pair is what the web client sends; the first is what parseParams
// has always read first, and dateWindow — /analytics_costs' transcription of
// the pylon reference — knows only the second. A caller using the first pair
// getting the DEFAULT window instead of theirs is a plausible wrong answer over
// a range nobody asked for, which no status code would reveal.
func TestBothDateSpellingsReachTheSameWindow(t *testing.T) {
	t.Parallel()

	var seen []domain.QueryParams
	repo := recordingRepo{seen: &seen}
	handler := NewHandler(repo).WithClock(func() time.Time {
		return time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	})

	for _, target := range []string{
		"/?date_from=2026-08-01T00:00:00Z&date_to=2026-08-10T00:00:00Z",
		"/?start_date=2026-08-01T00:00:00Z&end_date=2026-08-10T00:00:00Z",
	} {
		rec := httptest.NewRecorder()
		handler.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	}

	if len(seen) != 2 {
		t.Fatalf("repository saw %d reads, want 2", len(seen))
	}
	if !seen[0].From.Equal(seen[1].From) || !seen[0].To.Equal(seen[1].To) {
		t.Fatalf("the two spellings resolved different windows: %v..%v vs %v..%v",
			seen[0].From, seen[0].To, seen[1].From, seen[1].To)
	}
	want := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if !seen[0].From.Equal(want) {
		t.Fatalf("window starts at %v, want the requested %v", seen[0].From, want)
	}
}

// A request with no bounds at all falls back to the default window, measured
// back from the handler's clock rather than from whatever the repository
// happened to see last.
func TestAbsentBoundsUseTheDefaultWindow(t *testing.T) {
	t.Parallel()

	var seen []domain.QueryParams
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	handler := NewHandler(recordingRepo{seen: &seen}).WithClock(func() time.Time { return now })

	rec := httptest.NewRecorder()
	handler.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if len(seen) != 1 {
		t.Fatalf("repository saw %d reads, want 1", len(seen))
	}
	if !seen[0].To.Equal(now) {
		t.Fatalf("window ends at %v, want the clock's %v", seen[0].To, now)
	}
	if got := seen[0].To.Sub(seen[0].From); got != defaultDateRangeDays*24*time.Hour {
		t.Fatalf("default window spans %v, want %d days", got, defaultDateRangeDays)
	}
}

// recordingRepo answers successfully and keeps what it was asked, so a test can
// assert on the RESOLVED scope rather than on the response the handler built
// from it.
type recordingRepo struct {
	seen *[]domain.QueryParams
}

func (r recordingRepo) GetUsageSummary(_ context.Context, params domain.QueryParams) (domain.UsageSummary, error) {
	*r.seen = append(*r.seen, params)
	return domain.UsageSummary{}, nil
}

func (r recordingRepo) GetAgentAnalytics(_ context.Context, params domain.QueryParams) (domain.AgentBreakdown, error) {
	*r.seen = append(*r.seen, params)
	return domain.AgentBreakdown{}, nil
}

func (r recordingRepo) GetToolAnalytics(_ context.Context, params domain.QueryParams) ([]domain.ToolAnalytics, error) {
	*r.seen = append(*r.seen, params)
	return nil, nil
}

func (r recordingRepo) GetUserActivity(_ context.Context, params domain.QueryParams) ([]domain.UserActivity, bool, error) {
	*r.seen = append(*r.seen, params)
	return nil, false, nil
}
