package eliteacore_test

// Real-PostgreSQL coverage for the public catalogue list
// (public_applications.go).
//
// Every assertion here goes through the HTTP route the catalogue page calls —
// GET /elitea_core/public_applications/prompt_lib — because that is the surface
// that failed: the handler answered 200 with a plausible body while ignoring
// every parameter but ?category. A test that called a query builder directly
// would have passed against the broken handler too.
//
// The fixture reuses newCatalogMirrorPool (catalog_mirror_postgres_integration
// _test.go): the production migration chain applied to p_1, the public project,
// and p_2, an ordinary user project. p_2 exists here for one assertion — that a
// published row in a private project stays invisible to this route.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type publicCatalogueResponse struct {
	Rows  []map[string]any `json:"rows"`
	Total int              `json:"total"`
}

// seedPublicCatalogueAgent inserts one application with one published version
// into the given schema, and returns the application id.
func seedPublicCatalogueAgent(
	t *testing.T, pool *pgxpool.Pool, schema, name, description, category, agentType string,
) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var appID int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.applications (name, description, owner_id) VALUES ($1, $2, 1) RETURNING id`, schema),
		name, description).Scan(&appID); err != nil {
		t.Fatalf("seed application %q: %v", name, err)
	}
	meta := map[string]any{}
	if category != "" {
		meta["category"] = category
	}
	encodedMeta, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.application_versions (application_id, name, status, author_id, agent_type, meta)
VALUES ($1, 'latest', 'published', 1, $2, $3::jsonb)`, schema), appID, agentType, string(encodedMeta)); err != nil {
		t.Fatalf("seed published version for %q: %v", name, err)
	}
	return appID
}

func tagPublicCatalogueAgent(t *testing.T, pool *pgxpool.Pool, schema string, appID int, tagName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var tagID int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.tags (name) VALUES ($1)
ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name RETURNING id`, schema), tagName).Scan(&tagID); err != nil {
		t.Fatalf("seed tag %q: %v", tagName, err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.application_version_tag_association (version_id, tag_id)
SELECT id, $2 FROM %s.application_versions WHERE application_id = $1`, schema, schema), appID, tagID); err != nil {
		t.Fatalf("attach tag %q: %v", tagName, err)
	}
}

func likePublicCatalogueAgent(t *testing.T, pool *pgxpool.Pool, schema string, appID, userID int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.social_likes (entity_name, entity_id, user_id) VALUES ('application', $1, $2)
ON CONFLICT DO NOTHING`, schema), appID, userID); err != nil {
		t.Fatalf("seed like: %v", err)
	}
}

// readPublicCatalogue calls the route and decodes the envelope. `userID` is the
// caller's id, or "" for an anonymous request.
func readPublicCatalogue(t *testing.T, router chi.Router, query, userID string) (publicCatalogueResponse, int) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/elitea_core/public_applications/prompt_lib?"+query, nil)
	if userID != "" {
		request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: userID}))
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	var body publicCatalogueResponse
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %q: %v", recorder.Body.String(), err)
		}
	}
	return body, recorder.Code
}

func rowNames(response publicCatalogueResponse) []string {
	names := make([]string, 0, len(response.Rows))
	for _, row := range response.Rows {
		names = append(names, fmt.Sprint(row["name"]))
	}
	return names
}

// newPublicCatalogueFixture seeds a small, deterministic catalogue in p_1 and
// one published agent in p_2 that must never appear.
func newPublicCatalogueFixture(t *testing.T) (chi.Router, *pgxpool.Pool, map[string]int) {
	t.Helper()
	pool := newCatalogMirrorPool(t)
	handler := eliteacore.NewHandler(pool)
	router := chi.NewRouter()
	router.Get("/elitea_core/public_applications/prompt_lib", handler.PublicApplications)

	ids := map[string]int{}
	// Seeded in a fixed order so the id ordering is known.
	ids["alpha"] = seedPublicCatalogueAgent(t, pool, "p_1", "Alpha release notes", "writes release notes", "Development", "openai")
	ids["bravo"] = seedPublicCatalogueAgent(t, pool, "p_1", "Bravo triage", "sorts the release backlog", "Development", "pipeline")
	ids["charlie"] = seedPublicCatalogueAgent(t, pool, "p_1", "Charlie helper", "answers questions", "", "openai")
	ids["private"] = seedPublicCatalogueAgent(t, pool, "p_2", "Private project agent", "not in the catalogue", "Development", "openai")
	return router, pool, ids
}

// TestPublicCatalogueSearchesNameAndDescription is the search box.
func TestPublicCatalogueSearchesNameAndDescription(t *testing.T) {
	router, _, _ := newPublicCatalogueFixture(t)

	byName, status := readPublicCatalogue(t, router, "query=alpha", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if got := rowNames(byName); len(got) != 1 || got[0] != "Alpha release notes" {
		t.Errorf("query=alpha returned %v", got)
	}
	if byName.Total != 1 {
		t.Errorf("query=alpha total = %d, want 1", byName.Total)
	}

	// "release" is in Alpha's NAME and in Bravo's DESCRIPTION. Both must match.
	byDescription, _ := readPublicCatalogue(t, router, "query=release", "")
	if len(byDescription.Rows) != 2 {
		t.Errorf("query=release returned %v, want both the name and the description match", rowNames(byDescription))
	}

	none, _ := readPublicCatalogue(t, router, "query=zzzz", "")
	if len(none.Rows) != 0 || none.Total != 0 {
		t.Errorf("a search that matches nothing returned %v (total %d)", rowNames(none), none.Total)
	}
}

// TestPublicCatalogueSortKeys walks every accepted key. A key that quietly fell
// back to the old hardcoded `ORDER BY a.id DESC` would pass a smoke test.
func TestPublicCatalogueSortKeys(t *testing.T) {
	router, pool, ids := newPublicCatalogueFixture(t)
	// Charlie is the most liked, Alpha next, Bravo has none.
	likePublicCatalogueAgent(t, pool, "p_1", ids["charlie"], 7)
	likePublicCatalogueAgent(t, pool, "p_1", ids["charlie"], 8)
	likePublicCatalogueAgent(t, pool, "p_1", ids["alpha"], 7)

	cases := map[string][]string{
		"sort_by=name&sort_order=asc":   {"Alpha release notes", "Bravo triage", "Charlie helper"},
		"sort_by=name&sort_order=desc":  {"Charlie helper", "Bravo triage", "Alpha release notes"},
		"sort_by=id&sort_order=asc":     {"Alpha release notes", "Bravo triage", "Charlie helper"},
		"sort_by=id&sort_order=desc":    {"Charlie helper", "Bravo triage", "Alpha release notes"},
		"sort_by=likes&sort_order=desc": {"Charlie helper", "Alpha release notes", "Bravo triage"},
		"sort_by=likes&sort_order=asc":  {"Bravo triage", "Alpha release notes", "Charlie helper"},
	}
	for query, want := range cases {
		t.Run(query, func(t *testing.T) {
			response, status := readPublicCatalogue(t, router, query, "")
			if status != http.StatusOK {
				t.Fatalf("status = %d", status)
			}
			got := rowNames(response)
			if len(got) != len(want) {
				t.Fatalf("returned %v, want %v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("order = %v, want %v", got, want)
				}
			}
		})
	}

	// created_at is the default key. The three rows are seeded inside one
	// transaction-free burst and can share a timestamp, so this only pins that
	// the default key is accepted and answers the whole catalogue.
	byDefault, _ := readPublicCatalogue(t, router, "sort_by=created_at", "")
	if len(byDefault.Rows) != 3 {
		t.Errorf("sort_by=created_at returned %v", rowNames(byDefault))
	}
}

// TestPublicCatalogueRefusesAnUnknownSortKey — the parameter must not be
// ignored. A silent fallback is how a catalogue answers the wrong order.
func TestPublicCatalogueRefusesBadParameters(t *testing.T) {
	router, _, _ := newPublicCatalogueFixture(t)
	cases := map[string]string{
		"sort_by=instructions":    "sort_by",
		"sort_order=sideways":     "sort_order",
		"agents_type=swarm":       "agents_type",
		"statuses=published,gone": "statuses",
		"limit=0":                 "limit",
		"offset=-3":               "offset",
	}
	for query, param := range cases {
		t.Run(query, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/elitea_core/public_applications/prompt_lib?"+query, nil)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %q: %v", recorder.Body.String(), err)
			}
			if body["param"] != param {
				t.Errorf("refusal named %v, want %q", body["param"], param)
			}
		})
	}
}

// TestPublicCataloguePaginates: the page is a window on the filtered set and
// `total` describes that whole set, not the page.
func TestPublicCataloguePaginates(t *testing.T) {
	router, _, _ := newPublicCatalogueFixture(t)

	first, _ := readPublicCatalogue(t, router, "limit=2&offset=0&sort_by=id&sort_order=asc", "")
	if len(first.Rows) != 2 {
		t.Fatalf("page one holds %v, want two rows", rowNames(first))
	}
	if first.Total != 3 {
		t.Errorf("total = %d, want 3 — the whole filtered set", first.Total)
	}

	second, _ := readPublicCatalogue(t, router, "limit=2&offset=2&sort_by=id&sort_order=asc", "")
	if got := rowNames(second); len(got) != 1 || got[0] != "Charlie helper" {
		t.Errorf("page two holds %v, want the last row alone", got)
	}
	if second.Total != 3 {
		t.Errorf("page two total = %d, want 3", second.Total)
	}

	// A filtered total must describe the filter, not the catalogue.
	filtered, _ := readPublicCatalogue(t, router, "query=release&limit=1", "")
	if filtered.Total != 2 || len(filtered.Rows) != 1 {
		t.Errorf("filtered page = %v (total %d), want 1 row of 2", rowNames(filtered), filtered.Total)
	}
}

// TestPublicCatalogueCategoryStillWorks — the one parameter that already
// worked must keep working, including the Other catch-all.
func TestPublicCatalogueCategoryStillWorks(t *testing.T) {
	router, _, _ := newPublicCatalogueFixture(t)

	development, _ := readPublicCatalogue(t, router, "category=Development", "")
	if len(development.Rows) != 2 || development.Total != 2 {
		t.Errorf("category=Development returned %v (total %d)", rowNames(development), development.Total)
	}

	other, _ := readPublicCatalogue(t, router, "category=Other", "")
	if got := rowNames(other); len(got) != 1 || got[0] != "Charlie helper" {
		t.Errorf("category=Other returned %v, want the uncategorised agent", got)
	}

	// Category and search compose.
	combined, _ := readPublicCatalogue(t, router, "category=Development&query=triage", "")
	if got := rowNames(combined); len(got) != 1 || got[0] != "Bravo triage" {
		t.Errorf("category + query returned %v", got)
	}
}

func TestPublicCatalogueAgentsTypeAndStatuses(t *testing.T) {
	router, _, _ := newPublicCatalogueFixture(t)

	pipelines, _ := readPublicCatalogue(t, router, "agents_type=pipeline", "")
	if got := rowNames(pipelines); len(got) != 1 || got[0] != "Bravo triage" {
		t.Errorf("agents_type=pipeline returned %v", got)
	}
	classic, _ := readPublicCatalogue(t, router, "agents_type=classic", "")
	if len(classic.Rows) != 2 {
		t.Errorf("agents_type=classic returned %v", rowNames(classic))
	}
	all, _ := readPublicCatalogue(t, router, "agents_type=all", "")
	if len(all.Rows) != 3 {
		t.Errorf("agents_type=all returned %v", rowNames(all))
	}

	published, _ := readPublicCatalogue(t, router, "statuses=published", "")
	if len(published.Rows) != 3 {
		t.Errorf("statuses=published returned %v", rowNames(published))
	}
	// The catalogue holds published versions only, so a status list without
	// `published` narrows to nothing rather than widening.
	drafts, _ := readPublicCatalogue(t, router, "statuses=draft", "")
	if len(drafts.Rows) != 0 || drafts.Total != 0 {
		t.Errorf("statuses=draft returned %v (total %d), want nothing", rowNames(drafts), drafts.Total)
	}
}

// TestPublicCatalogueRowsCarryTagsAndLikes — the row fields the Agent Hub card
// needs and the old handler never sent.
func TestPublicCatalogueRowsCarryTagsAndLikes(t *testing.T) {
	router, pool, ids := newPublicCatalogueFixture(t)
	tagPublicCatalogueAgent(t, pool, "p_1", ids["alpha"], "documentation")
	tagPublicCatalogueAgent(t, pool, "p_1", ids["alpha"], "writing")
	likePublicCatalogueAgent(t, pool, "p_1", ids["alpha"], 7)
	likePublicCatalogueAgent(t, pool, "p_1", ids["alpha"], 8)

	response, _ := readPublicCatalogue(t, router, "query=alpha", "7")
	if len(response.Rows) != 1 {
		t.Fatalf("returned %v", rowNames(response))
	}
	row := response.Rows[0]

	tags, ok := row["tags"].([]any)
	if !ok || len(tags) != 2 {
		t.Fatalf("tags = %v, want the two attached tags", row["tags"])
	}
	first, _ := tags[0].(map[string]any)
	if first["name"] != "documentation" {
		t.Errorf("tags are not ordered by name: %v", tags)
	}
	if first["id"] == nil {
		t.Errorf("tag carries no id: %v", first)
	}

	if likes, _ := row["likes"].(float64); likes != 2 {
		t.Errorf("likes = %v, want 2", row["likes"])
	}
	if liked, _ := row["is_liked"].(bool); !liked {
		t.Errorf("is_liked = %v for the user who liked it", row["is_liked"])
	}

	// A row nobody tagged still carries an empty array, never null.
	untagged, _ := readPublicCatalogue(t, router, "query=charlie", "")
	if tags, ok := untagged.Rows[0]["tags"].([]any); !ok || len(tags) != 0 {
		t.Errorf("an untagged row's tags = %v, want []", untagged.Rows[0]["tags"])
	}
	if liked, _ := untagged.Rows[0]["is_liked"].(bool); liked {
		t.Errorf("is_liked = true for an anonymous caller")
	}
}

// TestPublicCatalogueMyLiked — the bucket that used to answer the generic list.
func TestPublicCatalogueMyLiked(t *testing.T) {
	router, pool, ids := newPublicCatalogueFixture(t)
	likePublicCatalogueAgent(t, pool, "p_1", ids["bravo"], 7)
	likePublicCatalogueAgent(t, pool, "p_1", ids["charlie"], 8)

	mine, _ := readPublicCatalogue(t, router, "my_liked=true", "7")
	if got := rowNames(mine); len(got) != 1 || got[0] != "Bravo triage" {
		t.Errorf("my_liked for user 7 returned %v", got)
	}
	if mine.Total != 1 {
		t.Errorf("my_liked total = %d, want 1", mine.Total)
	}

	other, _ := readPublicCatalogue(t, router, "my_liked=true", "8")
	if got := rowNames(other); len(got) != 1 || got[0] != "Charlie helper" {
		t.Errorf("my_liked for user 8 returned %v", got)
	}

	anonymous, _ := readPublicCatalogue(t, router, "my_liked=true", "")
	if len(anonymous.Rows) != 0 || anonymous.Total != 0 {
		t.Errorf("my_liked with no identity returned %v (total %d), want nothing",
			rowNames(anonymous), anonymous.Total)
	}
}

// TestPublicCatalogueStaysInThePublicProject — project scoping is unchanged:
// the p_2 agent is published in its own project and must not appear, whatever
// the new parameters say.
func TestPublicCatalogueStaysInThePublicProject(t *testing.T) {
	router, _, _ := newPublicCatalogueFixture(t)
	for _, query := range []string{
		"",
		"query=private",
		"limit=1000",
		"sort_by=name&sort_order=asc",
		"category=Development",
		"agents_type=classic",
	} {
		response, status := readPublicCatalogue(t, router, query, "")
		if status != http.StatusOK {
			t.Fatalf("%q: status = %d", query, status)
		}
		for _, name := range rowNames(response) {
			if name == "Private project agent" {
				t.Fatalf("%q leaked a private project's published agent", query)
			}
		}
		for _, row := range response.Rows {
			if row["project_id"] != "1" {
				t.Errorf("%q: row project_id = %v, want the public project", query, row["project_id"])
			}
		}
	}
}
