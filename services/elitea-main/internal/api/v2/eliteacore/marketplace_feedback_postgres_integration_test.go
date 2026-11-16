package eliteacore_test

// Real-PostgreSQL coverage for the feedback of a Marketplace listing (ADR-0051
// decision 5, issue 1631): reviews only from verified hires, one per person
// and listing; the workforce report author paths (an agent and the approving
// person, and the auto_feedback policy); the publisher's reply and flag; the
// administrator's hide with an audit event; the daily signals; and the rule
// that no response names a project other than the caller's.
//
// The fixture is the one of marketplace_postgres_integration_test.go: project 1
// is the public project, 2 the publisher's project. Projects 3 (the hirer
// whose name must never be returned), 4 and 5 are added here.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/marketplace"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const (
	secretProjectName = "Skunkworks Secret Lab"
	otherProjectName  = "Zeta Other Project"
	moderatorUser     = 99
)

// moderatorResolver grants the admin published agents permission to one user.
type moderatorResolver struct{}

func (moderatorResolver) ResolvePermissions(_ context.Context, principal auth.User, _ string, _ string) (auth.PermissionResolution, error) {
	if principal.ID == strconv.Itoa(moderatorUser) {
		return auth.PermissionResolution{UserID: moderatorUser, Permissions: []string{eliteacore.PublishedAgentsListPermission}}, nil
	}
	return auth.PermissionResolution{UserID: 1, Permissions: nil}, nil
}

type feedbackFixture struct {
	*marketplaceHarness
	listing  string
	listingN int64
	v1, v2   string
	handler  *eliteacore.Handler
}

// newFeedbackFixture publishes Flake Finder in two versions from project 2 and
// adds the projects and people the tests review from.
func newFeedbackFixture(t *testing.T) *feedbackFixture {
	t.Helper()
	h := newMarketplaceHarness(t)
	ctx := context.Background()
	agent := h.seedAgent(2, "Flake Finder", nil)
	h.publish(agent, "v1", "First release.")
	if _, err := h.pool.Exec(ctx, `UPDATE p_2.application_versions SET instructions = 'v2 text' WHERE id = $1`, agent.versionID); err != nil {
		t.Fatal(err)
	}
	h.publish(agent, "v2", "Changed the text.")

	runner := migrate.New(h.pool, platformmigrations.Files)
	for project, name := range map[int]string{3: secretProjectName, 4: otherProjectName, 5: "Fifth Project"} {
		if _, err := h.pool.Exec(ctx, `INSERT INTO centry.project (id, name, owner_id, create_success, suspended)
VALUES ($1, $2, 1, true, false) ON CONFLICT (id) DO NOTHING`, project, name); err != nil {
			t.Fatalf("seed project %d: %v", project, err)
		}
		if _, err := h.pool.Exec(ctx, fmt.Sprintf(`SELECT create_tenant_schema('p_%d')`, project)); err != nil {
			t.Fatalf("build tenant %d: %v", project, err)
		}
		if err := runner.ApplyTenant(ctx, int64(project)); err != nil {
			t.Fatalf("apply tenant %d: %v", project, err)
		}
	}
	for id, name := range map[int]string{11: "Reviewer Eleven", 12: "Reviewer Twelve", 13: "Reader Thirteen", 7: "Publisher Seven"} {
		if _, err := h.pool.Exec(ctx, `INSERT INTO public.auth_core__user (id, email, name) VALUES ($1, $2, $3)
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name`, id, fmt.Sprintf("u%d@example.test", id), name); err != nil {
			t.Fatalf("seed user %d: %v", id, err)
		}
	}
	f := &feedbackFixture{marketplaceHarness: h}
	f.listing = h.listingID("Flake Finder")
	f.listingN, _ = strconv.ParseInt(f.listing, 10, 64)
	for _, v := range versionsOf(h.detail(f.listing)) {
		switch v["name"] {
		case "v1":
			f.v1 = v["id"].(string)
		case "v2":
			f.v2 = v["id"].(string)
		}
	}
	if f.v1 == "" || f.v2 == "" {
		t.Fatalf("versions = %v", versionsOf(h.detail(f.listing)))
	}
	f.handler = eliteacore.NewHandler(h.pool, eliteacore.WithPermissionResolver(moderatorResolver{}))
	return f
}

// router mounts the feedback routes as the user.
func (f *feedbackFixture) router(userID int) chi.Router {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user := auth.User{ID: strconv.Itoa(userID)}
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), user)))
		})
	})
	router.Get("/elitea_core/marketplace_feedback/prompt_lib/{projectID}/{listingID}", f.handler.MarketplaceFeedback)
	router.Post("/elitea_core/marketplace_feedback/prompt_lib/{projectID}/{listingID}", f.handler.MarketplaceFeedbackReview)
	router.Post("/elitea_core/marketplace_feedback_report/prompt_lib/{projectID}/{listingID}", f.handler.MarketplaceFeedbackReport)
	router.Post("/elitea_core/marketplace_feedback_action/prompt_lib/{projectID}/{listingID}/{feedbackID}", f.handler.MarketplaceFeedbackAction)
	router.Get("/elitea_core/admin_published_agents/administration", f.handler.AdminPublishedAgents)
	return router
}

func (f *feedbackFixture) hire(project int, entity int64, version string) {
	f.t.Helper()
	listingID, _ := strconv.ParseInt(f.listing, 10, 64)
	versionID, _ := strconv.ParseInt(version, 10, 64)
	stored, err := marketplace.LoadVersion(context.Background(), f.pool, listingID, versionID)
	if err != nil {
		f.t.Fatalf("load the version: %v", err)
	}
	if err := marketplace.RecordInstall(context.Background(), f.pool, marketplace.Install{
		ProjectID: int64(project), EntityKind: marketplace.KindAgent, EntityID: entity, ListingID: listingID,
		VersionID: versionID, Digest: stored.Digest, Mode: marketplace.ModeInstall, ToolsOn: true, InstalledBy: 1,
	}); err != nil {
		f.t.Fatalf("record the hire: %v", err)
	}
}

// workforceManager seeds a published agent that may review agents in the project.
func (f *feedbackFixture) workforceManager(project int, name string, manager bool) int64 {
	f.t.Helper()
	ctx := context.Background()
	var id int64
	if err := f.pool.QueryRow(ctx, fmt.Sprintf(`INSERT INTO p_%d.applications (name, description, owner_id)
VALUES ($1, 'a seeded agent', $2) RETURNING id`, project), name, project).Scan(&id); err != nil {
		f.t.Fatalf("seed the agent: %v", err)
	}
	meta := `{}`
	if manager {
		meta = `{"capabilities":{"can_review_agents":true}}`
	}
	if _, err := f.pool.Exec(ctx, fmt.Sprintf(`INSERT INTO p_%d.application_versions (application_id, name, status, author_id, meta)
VALUES ($1, 'v1', 'published', 1, $2::jsonb)`, project), id, meta); err != nil {
		f.t.Fatalf("seed the agent version: %v", err)
	}
	return id
}

func (f *feedbackFixture) feedback(userID, project int, query string) (int, map[string]any, string) {
	f.t.Helper()
	status, body, raw := f.doRaw(f.router(userID), http.MethodGet,
		fmt.Sprintf("/elitea_core/marketplace_feedback/prompt_lib/%d/%s%s", project, f.listing, query), nil)
	return status, body, raw
}

// doRaw is do with the raw body, for the checks that read the whole response.
func (h *marketplaceHarness) doRaw(router chi.Router, method, target string, body any) (int, map[string]any, string) {
	h.t.Helper()
	status, decoded := h.do(router, method, target, body)
	raw, _ := json.Marshal(decoded)
	return status, decoded, string(raw)
}

func (f *feedbackFixture) post(userID, project int, path string, body any) (int, map[string]any) {
	f.t.Helper()
	return f.do(f.router(userID), http.MethodPost, fmt.Sprintf("/elitea_core/%s/prompt_lib/%d/%s", path, project, f.listing), body)
}

func itemsOf(body map[string]any) []map[string]any {
	out := []map[string]any{}
	items, _ := body["items"].([]any)
	for _, item := range items {
		out = append(out, item.(map[string]any))
	}
	return out
}

func (f *feedbackFixture) countRows(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		f.t.Fatalf("count: %v", err)
	}
	return n
}

// TestReviewsComeOnlyFromVerifiedHiresOneEach: a project that never hired the
// listing cannot review it, nor can its publisher; a person reviews once and a
// second call edits; the rating, the distribution per version and the version
// filter follow.
func TestReviewsComeOnlyFromVerifiedHiresOneEach(t *testing.T) {
	f := newFeedbackFixture(t)
	f.hire(3, 501, f.v1)
	f.hire(4, 601, f.v2)

	status, body := f.post(11, 5, "marketplace_feedback", map[string]any{"rating": 4})
	if status != http.StatusForbidden || body["code"] != "not_verified_hire" {
		t.Fatalf("a project that did not hire it: %d %v, want 403 not_verified_hire", status, body)
	}
	status, body = f.post(7, 2, "marketplace_feedback", map[string]any{"rating": 5})
	if status != http.StatusForbidden || body["code"] != "own_listing" {
		t.Fatalf("the publisher: %d %v, want 403 own_listing", status, body)
	}
	for _, bad := range []int{0, 6, -1} {
		if status, _ = f.post(11, 3, "marketplace_feedback", map[string]any{"rating": bad}); status != http.StatusBadRequest {
			t.Errorf("rating %d answered %d, want 400", bad, status)
		}
	}
	// A version the project did not hire cannot be reviewed from it.
	if status, body = f.post(11, 3, "marketplace_feedback", map[string]any{"rating": 4, "version_id": f.v2}); status != http.StatusForbidden {
		t.Errorf("review of an unhired version: %d %v, want 403", status, body)
	}

	if status, body = f.post(11, 3, "marketplace_feedback", map[string]any{"rating": 4, "body": "Works, a little slow."}); status != http.StatusCreated {
		t.Fatalf("first review = %d %v", status, body)
	}
	status, body = f.post(11, 3, "marketplace_feedback", map[string]any{"rating": 5, "body": "Faster after the update."})
	if status != http.StatusOK || body["created"] != false {
		t.Fatalf("second review = %d %v, want 200 and an edit", status, body)
	}
	if n := f.countRows(`SELECT count(*) FROM elitea_marketplace.listing_feedback WHERE kind = 'review'`); n != 1 {
		t.Fatalf("a person's two reviews left %d rows, want 1", n)
	}
	if status, body = f.post(12, 3, "marketplace_feedback", map[string]any{"rating": 2}); status != http.StatusCreated {
		t.Fatalf("another person of the same project = %d %v", status, body)
	}
	if status, body = f.post(13, 4, "marketplace_feedback", map[string]any{"rating": 3, "body": "Fine."}); status != http.StatusCreated {
		t.Fatalf("project 4 review = %d %v", status, body)
	}

	status, page, _ := f.feedback(13, 4, "")
	if status != http.StatusOK {
		t.Fatalf("feedback = %d %v", status, page)
	}
	overall := page["overall"].(map[string]any)
	if overall["rating_count"].(float64) != 3 || overall["rating_avg"].(float64) != 3.33 {
		t.Errorf("overall = %v, want 3 reviews averaging 3.33 (5, 2 and 3)", overall)
	}
	for _, raw := range page["versions"].([]any) {
		bucket := raw.(map[string]any)
		dist := bucket["distribution"].([]any)
		switch bucket["version_name"] {
		case "v1":
			if dist[1].(float64) != 1 || dist[4].(float64) != 1 || bucket["rating_count"].(float64) != 2 {
				t.Errorf("v1 distribution = %v, want one 2-star and one 5-star", dist)
			}
		case "v2":
			if dist[2].(float64) != 1 || bucket["rating_count"].(float64) != 1 {
				t.Errorf("v2 distribution = %v, want one 3-star", dist)
			}
		}
	}
	if status, page, _ = f.feedback(13, 4, "?version_id="+f.v2); status != http.StatusOK || len(itemsOf(page)) != 1 {
		t.Errorf("version filter v2 = %d with %d items, want 1", status, len(itemsOf(page)))
	}
	if status, _, _ = f.feedback(13, 4, "?kind=nonsense"); status != http.StatusBadRequest {
		t.Errorf("an unknown kind answered %d, want 400", status)
	}
	if page["can_review"] != true {
		t.Errorf("project 4 hired the listing, can_review = %v", page["can_review"])
	}
	// The caller's own review comes back whatever the page, with the version it is on.
	if mine, _ := page["my_review"].(map[string]any); mine == nil || mine["rating"] != float64(3) || mine["version_name"] != "v2" {
		t.Errorf("my_review = %v, want the 3-star review of v2", page["my_review"])
	}
	if _, other, _ := f.feedback(7, 2, ""); other["my_review"] != nil {
		t.Errorf("the publisher has no review, my_review = %v", other["my_review"])
	}
	if _, page, _ = f.feedback(13, 5, ""); page["can_review"] != false {
		t.Errorf("project 5 did not hire it, can_review = %v", page["can_review"])
	}

	// The listing card carries the average.
	for _, row := range rowsOf(f.search("?query=flake")) {
		if row["rating_count"].(float64) != 3 {
			t.Errorf("the listing card rating_count = %v, want 3", row["rating_count"])
		}
	}
}

// TestNoOtherProjectNameIsReturned is the done-when of issue 1631: a review, a
// workforce report and the whole feedback response of a project never name
// another project or carry a project id.
func TestNoOtherProjectNameIsReturned(t *testing.T) {
	f := newFeedbackFixture(t)
	f.hire(3, 501, f.v1)
	manager := f.workforceManager(3, "Warden", true)
	if status, body := f.post(11, 3, "marketplace_feedback", map[string]any{"rating": 4, "body": "Good."}); status != http.StatusCreated {
		t.Fatalf("review = %d %v", status, body)
	}
	if status, body := f.post(11, 3, "marketplace_feedback_report", map[string]any{
		"hired_agent_id": 501, "agent_id": manager, "outcome": "dismissed", "stage": "dismissal",
		"reason_code": "too_many_failures", "text": "It kept failing on large repositories.",
	}); status != http.StatusCreated {
		t.Fatalf("report = %d %v", status, body)
	}

	// Project 4 never hired it; the reader may still read the listing's feedback.
	for _, reader := range []struct {
		user, project int
	}{{13, 4}, {13, 5}, {7, 2}, {moderatorUser, 4}} {
		status, page, raw := f.feedback(reader.user, reader.project, "")
		if status != http.StatusOK {
			t.Fatalf("reader %v: feedback = %d %v", reader, status, page)
		}
		if len(itemsOf(page)) != 2 {
			t.Errorf("reader %v sees %d items, want the review and the report", reader, len(itemsOf(page)))
		}
		for _, forbidden := range []string{secretProjectName, "Skunkworks", `"project_id"`, `"project_name"`, `"project"`} {
			if strings.Contains(raw, forbidden) {
				t.Errorf("reader %v: the response carries %q: %s", reader, forbidden, raw)
			}
		}
		if !strings.Contains(raw, "Reviewer Eleven") || !strings.Contains(raw, marketplace.WorkforceReportLabel) {
			t.Errorf("reader %v: the review author and the report label are missing: %s", reader, raw)
		}
	}
	// The listing page itself and the search do not name it either.
	for _, target := range []string{
		"/elitea_core/marketplace_listing/prompt_lib/4/" + f.listing,
		"/elitea_core/marketplace_listings/prompt_lib/4",
	} {
		_, _, raw := f.doRaw(f.router(13), http.MethodGet, target, nil)
		if strings.Contains(raw, secretProjectName) {
			t.Errorf("%s names another project: %s", target, raw)
		}
	}
	// The row keeps the id, for the project's own management and the guards.
	if n := f.countRows(`SELECT count(*) FROM elitea_marketplace.listing_feedback WHERE project_id = 3`); n != 2 {
		t.Errorf("project_id stored on %d rows, want 2", n)
	}
	// A hidden item is shown to its author's side only.
	if _, page, _ := f.feedback(11, 3, ""); !anyMine(itemsOf(page)) {
		t.Errorf("project 3 does not see its own items as mine: %v", page)
	}
}

func anyMine(items []map[string]any) bool {
	for _, item := range items {
		if item["mine"] == true {
			return true
		}
	}
	return false
}

// TestWorkforceReportAuthorPaths walks the three authors of ADR-0051 decision
// 5: an agent with the approving person, the auto_feedback policy with the
// person who set it, and the refusals of decision 6.
func TestWorkforceReportAuthorPaths(t *testing.T) {
	f := newFeedbackFixture(t)
	ctx := context.Background()
	f.hire(3, 501, f.v1)
	f.hire(3, 502, f.v1)
	manager := f.workforceManager(3, "Warden", true)
	plain := f.workforceManager(3, "Plain helper", false)
	report := func(user int, agent int64, hired int, stage string) (int, map[string]any) {
		return f.post(user, 3, "marketplace_feedback_report", map[string]any{
			"hired_agent_id": hired, "agent_id": agent, "outcome": "dismissed", "stage": stage, "text": "It failed.",
			"scorecard": map[string]any{"failure_rate": 0.9},
		})
	}

	// An agent and the approving person.
	status, body := report(11, manager, 501, "dismissal")
	if status != http.StatusCreated {
		t.Fatalf("agent report = %d %v", status, body)
	}
	var kind, label string
	var author, agentID int64
	var version int64
	if err := f.pool.QueryRow(ctx, `SELECT author_kind, author_label, author_user_id, author_agent_id, version_id
FROM elitea_marketplace.listing_feedback WHERE id = $1`, body["id"]).Scan(&kind, &label, &author, &agentID, &version); err != nil {
		t.Fatal(err)
	}
	if kind != "agent" || label != "" || author != 11 || agentID != manager || strconv.FormatInt(version, 10) != f.v1 {
		t.Errorf("agent report row = %s %q %d %d %d, want agent, the approver 11, the manager and the hired version", kind, label, author, agentID, version)
	}
	// The scorecard is the platform's, never the request's.
	var scorecard string
	if err := f.pool.QueryRow(ctx, `SELECT scorecard::text FROM elitea_marketplace.listing_feedback WHERE id = $1`, body["id"]).Scan(&scorecard); err != nil || scorecard != "{}" {
		t.Errorf("scorecard = %q (%v), want the empty summary the request cannot fill", scorecard, err)
	}

	// One report per hired agent and stage.
	if status, body = report(11, manager, 501, "dismissal"); status != http.StatusConflict || body["code"] != "already_done" {
		t.Errorf("a second report for the stage = %d %v, want 409", status, body)
	}
	if status, _ = report(11, manager, 501, "demotion"); status != http.StatusCreated {
		t.Errorf("another stage = %d, want 201", status)
	}
	// An agent that is not a workforce manager, a hired agent the project does not have.
	if status, body = report(11, plain, 502, "dismissal"); status != http.StatusForbidden || body["code"] != "not_workforce_manager" {
		t.Errorf("a plain agent = %d %v, want 403 not_workforce_manager", status, body)
	}
	if status, body = report(11, manager, 999, "dismissal"); status != http.StatusForbidden || body["code"] != "not_verified_hire" {
		t.Errorf("an agent that was not hired = %d %v, want 403 not_verified_hire", status, body)
	}
	if status, _ = f.post(11, 3, "marketplace_feedback_report", map[string]any{"hired_agent_id": 502, "agent_id": manager, "outcome": "fired", "stage": "x"}); status != http.StatusBadRequest {
		t.Errorf("an unknown outcome = %d, want 400", status)
	}

	// The policy path is the platform's: the store takes the person who set it.
	policyID, err := marketplace.SaveWorkforceReport(ctx, f.pool, marketplace.ReportInput{
		ListingID: f.listingN, ProjectID: 3, HiredEntityID: 502, AgentID: manager, ApprovedBy: 12, Policy: true,
		Outcome: marketplace.OutcomeDismissed, Stage: "dismissal", ReasonCode: "cost", Body: "Cost twice the median.",
		Scorecard: json.RawMessage(`{"failure_rate":0.2,"cost_vs_median":2.1,"active_days":14,"transcript":"never stored","nested":{"a":1}}`),
	})
	if err != nil {
		t.Fatalf("policy report: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT author_kind, author_label, author_user_id, author_agent_id, scorecard::text
FROM elitea_marketplace.listing_feedback WHERE id = $1`, policyID).Scan(&kind, &label, &author, &agentID, &scorecard); err != nil {
		t.Fatal(err)
	}
	if kind != "policy" || label != "policy:auto_feedback" || author != 12 || agentID != manager {
		t.Errorf("policy report row = %s %q %d %d, want policy:auto_feedback, the policy setter 12 and the manager", kind, label, author, agentID)
	}
	if strings.Contains(scorecard, "transcript") || strings.Contains(scorecard, "nested") ||
		!strings.Contains(scorecard, "failure_rate") || !strings.Contains(scorecard, "cost_vs_median") {
		t.Errorf("scorecard = %s, want only the four numeric summary keys", scorecard)
	}

	// The project that published the listing cannot report on it.
	f.hire(2, 900, f.v1)
	ownManager := f.workforceManager(2, "Own warden", true)
	if _, err := marketplace.SaveWorkforceReport(ctx, f.pool, marketplace.ReportInput{
		ListingID: f.listingN, ProjectID: 2, HiredEntityID: 900, AgentID: ownManager, ApprovedBy: 7,
		Outcome: marketplace.OutcomeKept, Stage: "review",
	}); err != marketplace.ErrOwnListing {
		t.Errorf("a report from the publisher's project = %v, want ErrOwnListing", err)
	}

	// A reader sees the reports as such, with the platform's scorecard.
	_, page, raw := f.feedback(13, 5, "?kind=workforce_report")
	if len(itemsOf(page)) != 3 {
		t.Fatalf("reports = %d, want 3: %s", len(itemsOf(page)), raw)
	}
	for _, item := range itemsOf(page) {
		author := item["author"].(map[string]any)
		if author["name"] != marketplace.WorkforceReportLabel || item["rating"] != nil {
			t.Errorf("a report item = %v, want the label and no rating", item)
		}
	}
	// A report is not a review: the rating stays empty.
	if n := f.countRows(`SELECT rating_count FROM elitea_marketplace.listings WHERE id = $1`, f.listingN); n != 0 {
		t.Errorf("rating_count = %d after reports only, want 0", n)
	}
}

// TestPublisherReplyFlagAndAdministratorHide: the publisher answers an item
// once and flags it; an administrator hides it, which removes it from other
// readers and the rating and leaves an audit event; nobody else can.
func TestPublisherReplyFlagAndAdministratorHide(t *testing.T) {
	f := newFeedbackFixture(t)
	f.hire(3, 501, f.v1)
	status, body := f.post(11, 3, "marketplace_feedback", map[string]any{"rating": 1, "body": "Rude text about a person."})
	if status != http.StatusCreated {
		t.Fatalf("review = %d %v", status, body)
	}
	id := body["id"].(string)
	action := func(user, project int, act string, extra map[string]any) (int, map[string]any) {
		payload := map[string]any{"action": act}
		for k, v := range extra {
			payload[k] = v
		}
		return f.do(f.router(user), http.MethodPost,
			fmt.Sprintf("/elitea_core/marketplace_feedback_action/prompt_lib/%d/%s/%s", project, f.listing, id), payload)
	}

	// Reply: the publisher only, once.
	if status, _ = action(13, 4, "reply", map[string]any{"text": "I am not the publisher."}); status != http.StatusForbidden {
		t.Errorf("a reply from a reader = %d, want 403", status)
	}
	if status, _ = action(7, 2, "reply", map[string]any{"text": ""}); status != http.StatusBadRequest {
		t.Errorf("an empty reply = %d, want 400", status)
	}
	if status, body = action(7, 2, "reply", map[string]any{"text": "Thanks, we fixed it in v2."}); status != http.StatusOK {
		t.Fatalf("reply = %d %v", status, body)
	}
	if status, body = action(7, 2, "reply", map[string]any{"text": "Another."}); status != http.StatusConflict || body["code"] != "already_done" {
		t.Errorf("a second reply = %d %v, want 409", status, body)
	}
	_, page, _ := f.feedback(13, 4, "")
	if reply, _ := itemsOf(page)[0]["reply"].(map[string]any); reply["body"] != "Thanks, we fixed it in v2." {
		t.Errorf("the reply = %v", itemsOf(page)[0]["reply"])
	}

	// Flag: the publisher; a reader cannot. The reason shows to the publisher only.
	if status, _ = action(13, 4, "flag", nil); status != http.StatusForbidden {
		t.Errorf("a flag from a reader = %d, want 403", status)
	}
	if status, _ = action(7, 2, "flag", map[string]any{"reason": "abusive"}); status != http.StatusOK {
		t.Fatalf("flag = %d", status)
	}
	if status, _ = action(7, 2, "flag", map[string]any{"reason": "abusive"}); status != http.StatusOK {
		t.Errorf("a repeated flag = %d, want 200 and no change", status)
	}
	if n := f.countRows(`SELECT count(*) FROM elitea_marketplace.listing_feedback_events WHERE action = 'flag'`); n != 1 {
		t.Errorf("flag events = %d, want 1: a repeat changes nothing", n)
	}
	_, page, _ = f.feedback(7, 2, "")
	if item := itemsOf(page)[0]; item["flagged"] != true || item["flag_reason"] != "abusive" {
		t.Errorf("the publisher sees %v, want the flag and its reason", item)
	}
	_, page, _ = f.feedback(13, 4, "")
	if item := itemsOf(page)[0]; item["flagged"] != true || item["flag_reason"] != nil {
		t.Errorf("a reader sees %v, want the flag without its reason", item)
	}

	// Hide: an administrator only, with an audit event.
	for _, user := range []struct{ id, project int }{{7, 2}, {13, 4}, {11, 3}} {
		if status, _ = action(user.id, user.project, "hide", map[string]any{"reason": "abuse"}); status != http.StatusForbidden {
			t.Errorf("hide by %v = %d, want 403", user, status)
		}
	}
	if status, body = action(moderatorUser, 4, "hide", map[string]any{"reason": "personal abuse"}); status != http.StatusOK {
		t.Fatalf("hide = %d %v", status, body)
	}
	var actor int64
	var reason string
	if err := f.pool.QueryRow(context.Background(), `SELECT actor_user_id, reason FROM elitea_marketplace.listing_feedback_events
WHERE action = 'hide' AND feedback_id = $1`, id).Scan(&actor, &reason); err != nil || actor != moderatorUser || reason != "personal abuse" {
		t.Errorf("the hide event = %d %q (%v), want the administrator and the reason", actor, reason, err)
	}
	if _, page, _ = f.feedback(13, 4, ""); len(itemsOf(page)) != 0 {
		t.Errorf("a reader still sees the hidden item: %v", page["items"])
	}
	if _, page, _ = f.feedback(11, 3, ""); len(itemsOf(page)) != 1 || itemsOf(page)[0]["state"] != "hidden" {
		t.Errorf("the author sees %v, want their hidden item", page["items"])
	}
	if _, page, _ = f.feedback(moderatorUser, 4, ""); len(itemsOf(page)) != 1 || itemsOf(page)[0]["hide_reason"] != "personal abuse" {
		t.Errorf("the administrator sees %v, want the item and the reason", page["items"])
	}
	if n := f.countRows(`SELECT rating_count FROM elitea_marketplace.listings WHERE id = $1`, f.listingN); n != 0 {
		t.Errorf("a hidden review still counts in the rating: %d", n)
	}
	// A reply cannot be added to a hidden item.
	if status, _ = action(7, 2, "reply", map[string]any{"text": "x"}); status == http.StatusOK {
		t.Errorf("a reply to a hidden item was accepted")
	}
	if status, _ = action(moderatorUser, 4, "hide", nil); status != http.StatusOK {
		t.Errorf("hiding twice = %d, want 200", status)
	}
	if n := f.countRows(`SELECT count(*) FROM elitea_marketplace.listing_feedback_events WHERE action = 'hide'`); n != 1 {
		t.Errorf("hide events = %d, want 1", n)
	}
	if status, _ = action(moderatorUser, 4, "unhide", nil); status != http.StatusOK {
		t.Fatalf("unhide = %d", status)
	}
	if n := f.countRows(`SELECT rating_count FROM elitea_marketplace.listings WHERE id = $1`, f.listingN); n != 1 {
		t.Errorf("rating_count after unhide = %d, want 1", n)
	}
	if status, _ = action(moderatorUser, 4, "explode", nil); status != http.StatusBadRequest {
		t.Errorf("an unknown action = %d, want 400", status)
	}
	if status, _ = f.do(f.router(moderatorUser), http.MethodPost,
		fmt.Sprintf("/elitea_core/marketplace_feedback_action/prompt_lib/4/%s/999999", f.listing), map[string]any{"action": "hide"}); status != http.StatusNotFound {
		t.Errorf("hiding an item that does not exist = %d, want 404", status)
	}
}

// TestSignalsAndAdoptionCounters: the daily pass writes active hires, 30-day
// retention, dismissal rate and upgrade adoption per listing and version, the
// read falls back to live figures, and the admin published agents counter reads
// the projects with an active hire.
func TestSignalsAndAdoptionCounters(t *testing.T) {
	f := newFeedbackFixture(t)
	ctx := context.Background()

	// Before any hire: nothing measured, and the admin counter is a measured 0.
	set, err := marketplace.ReadSignals(ctx, f.pool, f.listingN)
	if err != nil || set.Listing.ActiveHires != 0 || set.Listing.DismissalRate != nil || !set.Listing.Live {
		t.Fatalf("signals with no hire = %+v (%v), want live zeroes and no rates", set.Listing, err)
	}
	adoption := func() map[string]any {
		status, body := f.do(f.router(moderatorUser), http.MethodGet, "/elitea_core/admin_published_agents/administration", nil)
		if status != http.StatusOK {
			t.Fatalf("admin published agents = %d %v", status, body)
		}
		for _, raw := range body["items"].([]any) {
			item := raw.(map[string]any)
			if item["name"] == "Flake Finder" {
				return item["adoption"].(map[string]any)
			}
		}
		t.Fatalf("Flake Finder is not in the admin list: %v", body)
		return nil
	}
	if a := adoption(); a["project_count"] != float64(0) || a["conversation_count"] != nil {
		t.Errorf("adoption with no hire = %v, want project_count 0 and conversation_count null", a)
	}

	f.hire(3, 501, f.v1)
	f.hire(4, 601, f.v1)
	f.hire(5, 801, f.v1)
	// 501: hired 40 days ago and kept. 601: hired 40 days ago, dismissed on day 5.
	// 801: hired now.
	if _, err := f.pool.Exec(ctx, `
UPDATE elitea_marketplace.installs SET created_at = clock_timestamp() - interval '40 days' WHERE entity_id IN (501, 601)`); err != nil {
		t.Fatal(err)
	}
	if err := marketplace.MarkDismissed(ctx, f.pool, 4, marketplace.KindAgent, 601, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE elitea_marketplace.installs SET dismissed_at = created_at + interval '5 days' WHERE entity_id = 601`); err != nil {
		t.Fatal(err)
	}
	// Make the current version (v2) newer than the 40-day-old hires.
	written, err := marketplace.ComputeSignals(ctx, f.pool)
	if err != nil || written == 0 {
		t.Fatalf("compute = %d (%v)", written, err)
	}
	set, err = marketplace.ReadSignals(ctx, f.pool, f.listingN)
	if err != nil {
		t.Fatal(err)
	}
	l := set.Listing
	if l.Live || l.ActiveHires != 2 || l.HiresTotal != 3 || l.Dismissed != 1 {
		t.Errorf("listing signals = %+v, want 2 active of 3 hires with 1 dismissed", l)
	}
	if l.DismissalRate == nil || *l.DismissalRate != 0.333 {
		t.Errorf("dismissal rate = %v, want 0.333", l.DismissalRate)
	}
	if l.RetentionCohort != 2 || l.Retention30d == nil || *l.Retention30d != 0.5 {
		t.Errorf("retention = %v of %d, want 0.5 of 2 (501 kept, 601 left on day 5)", l.Retention30d, l.RetentionCohort)
	}
	// 501 was hired before v2 was published and is still on v1; 801 was hired after.
	if l.UpgradeEligible != 1 || l.UpgradeAdoption == nil || *l.UpgradeAdoption != 0 {
		t.Errorf("upgrade = %v of %d, want 0 of 1", l.UpgradeAdoption, l.UpgradeEligible)
	}
	if v1 := set.Versions[f.v1]; v1.ActiveHires != 2 || v1.HiresTotal != 3 {
		t.Errorf("v1 signals = %+v, want 2 active of 3", v1)
	}

	// 501 upgrades: the adoption follows the next pass, and a second pass the
	// same day replaces the row instead of adding one.
	if _, err := f.pool.Exec(ctx, `UPDATE elitea_marketplace.installs SET version_id = $1 WHERE entity_id = 501`, f.v2); err != nil {
		t.Fatal(err)
	}
	if _, err := marketplace.ComputeSignals(ctx, f.pool); err != nil {
		t.Fatal(err)
	}
	if n := f.countRows(`SELECT count(*) FROM elitea_marketplace.listing_signals_daily WHERE listing_id = $1 AND version_id = 0`, f.listingN); n != 1 {
		t.Errorf("version 0 rows = %d after two passes in a day, want 1", n)
	}
	set, _ = marketplace.ReadSignals(ctx, f.pool, f.listingN)
	if set.Listing.UpgradeAdoption == nil || *set.Listing.UpgradeAdoption != 1 {
		t.Errorf("upgrade adoption = %v after 501 moved to v2, want 1", set.Listing.UpgradeAdoption)
	}

	// The signals ride in the feedback response, beside the rating.
	_, page, _ := f.feedback(13, 4, "")
	if signals, _ := page["signals"].(map[string]any); signals == nil || signals["listing"].(map[string]any)["active_hires"] != float64(2) {
		t.Errorf("the feedback response signals = %v", page["signals"])
	}
	// The listing's hires and the admin counter count active projects, not the dismissed one.
	for _, row := range rowsOf(f.search("?query=flake")) {
		if row["hires"] != float64(2) {
			t.Errorf("hires = %v, want 2 active projects", row["hires"])
		}
	}
	if a := adoption(); a["project_count"] != float64(2) || a["conversation_count"] != nil {
		t.Errorf("adoption = %v, want project_count 2 and conversation_count null", a)
	}
	// A dismissed agent can still be reviewed by its project: it hired the listing.
	if status, body := f.post(13, 4, "marketplace_feedback", map[string]any{"rating": 2, "body": "Dropped it."}); status != http.StatusCreated {
		t.Errorf("a review from a project whose hire was dismissed = %d %v, want 201", status, body)
	}
	if err := marketplace.RefreshAllRatings(ctx, f.pool); err != nil {
		t.Fatal(err)
	}
	if n := f.countRows(`SELECT rating_count FROM elitea_marketplace.listings WHERE id = $1`, f.listingN); n != 1 {
		t.Errorf("rating_count after the refresh = %d, want 1", n)
	}
	// Rows older than the retention are pruned by the next pass.
	if _, err := f.pool.Exec(ctx, `UPDATE elitea_marketplace.listing_signals_daily SET day = day - $1::int`, marketplace.SignalsRetentionDays+5); err != nil {
		t.Fatal(err)
	}
	if _, err := marketplace.ComputeSignals(ctx, f.pool); err != nil {
		t.Fatal(err)
	}
	if n := f.countRows(`SELECT count(*) FROM elitea_marketplace.listing_signals_daily WHERE day < current_date - 1`); n != 0 {
		t.Errorf("%d signal rows older than the retention remain", n)
	}
}
