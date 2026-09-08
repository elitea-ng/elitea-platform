package eliteacore_test

// Real-PostgreSQL coverage for what a PUBLISH says about itself: which draft
// the published row came from, and who published it.
//
// TWO DEFECTS, one request, and both invisible to a status-code test.
//
//  1. The response's `source_version_id` repeated `public_version_id` — the id
//     of the row the request had just created. The published row's own stored
//     meta carried the DRAFT's id under the same key, so one publish answered
//     the question "where did this come from?" two ways, and a client that
//     followed the response back to the author's editable version arrived at
//     the published copy instead.
//
//  2. The published row carried no `published_by`. The operator's
//     published-agents dashboard reads exactly that key
//     (admin_published_agents.go), so its "published by" column was null for
//     every agent on every deployment — permanently, and indistinguishably
//     from "this platform does not record it". The SKILL publish path has
//     written the key since it was built (internal/api/v2/skillpublish).
//
// Both assertions therefore read the DATABASE and the dashboard route back,
// not the publish response alone. The publish answering 200 is what both
// defects already did.
//
// The harness is the catalogue mirror's: the production migration chain over
// two tenants, p_1 (the public project) and p_2 (an ordinary user project).
// Publishing from p_2 is what makes the catalogue TWIN observable, and the
// twin is where the dashboard reads — a fix that stamped the author's own
// clone and not the twin would leave the dashboard exactly as it was.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// publishAttributionActor is the principal the publish requests below carry.
// It is deliberately NOT the seeded author id (1), so an implementation that
// stamped the row's author instead of the caller cannot pass by coincidence.
const publishAttributionActor = 4242

// TestPublishResponseNamesTheSourceDraft pins defect 1.
func TestPublishResponseNamesTheSourceDraft(t *testing.T) {
	pool := newCatalogMirrorPool(t)
	router := catalogMirrorRouter(eliteacore.NewHandler(pool))
	fixture := seedCatalogMirrorFixture(t, pool, 2, "attribution source agent")

	recorder := catalogMirrorPublish(t, router, fixture, map[string]any{
		"version_name":     "v-source",
		"validation_token": catalogMirrorToken(t, pool, fixture),
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("publish status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var response struct {
		PublicVersionID string `json:"public_version_id"`
		SourceVersionID string `json:"source_version_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode publish response %q: %v", recorder.Body.String(), err)
	}

	want := fmt.Sprintf("%d", fixture.versionID)
	if response.SourceVersionID != want {
		t.Errorf("source_version_id = %q, want the draft %q", response.SourceVersionID, want)
	}
	// The two keys must differ. A response that named the clone under both is
	// exactly the shape this test exists to refuse, and the equality is the
	// cheapest statement of it.
	if response.SourceVersionID == response.PublicVersionID {
		t.Errorf("source_version_id repeats public_version_id (%q)", response.PublicVersionID)
	}

	// …and it agrees with the row. The response is built in Go from values the
	// handler chose; only the stored meta says what a later reader will find.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var storedSource string
	if err := pool.QueryRow(ctx, `
SELECT meta->>'source_version_id' FROM p_2.application_versions WHERE id = $1`,
		response.PublicVersionID).Scan(&storedSource); err != nil {
		t.Fatalf("read the published row's meta: %v", err)
	}
	if storedSource != want {
		t.Errorf("stored meta.source_version_id = %q, want the draft %q", storedSource, want)
	}
	if storedSource != response.SourceVersionID {
		t.Errorf("the response says %q and the row says %q about the same publish",
			response.SourceVersionID, storedSource)
	}
}

// TestPublishStampsThePublisher pins defect 2, all the way to the dashboard.
func TestPublishStampsThePublisher(t *testing.T) {
	pool := newCatalogMirrorPool(t)
	handler := eliteacore.NewHandler(pool)
	router := publishAttributionRouter(handler)
	fixture := seedCatalogMirrorFixture(t, pool, 2, "attribution published agent")

	recorder := publishAttributionPublish(t, router, fixture, publishAttributionActor, map[string]any{
		"version_name":     "v-attributed",
		"validation_token": catalogMirrorToken(t, pool, fixture),
		"category":         "Development",
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("publish status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		PublicVersionID  string `json:"public_version_id"`
		CatalogVersionID string `json:"catalog_version_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode publish response %q: %v", recorder.Body.String(), err)
	}
	if response.CatalogVersionID == "" {
		t.Fatalf("publish from a private project named no catalogue row: %s", recorder.Body.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The author's own clone…
	if got := publishedByOf(t, ctx, pool, "p_2", response.PublicVersionID); got != publishAttributionActor {
		t.Errorf("the author-side clone says published_by = %d, want %d", got, publishAttributionActor)
	}
	// …and the catalogue twin, which is the row the dashboard reads. The twin
	// copies the clone's meta, so a stamp on the clone alone would still leave
	// the dashboard blank; this is the assertion that says it does not.
	if got := publishedByOf(t, ctx, pool, "p_1", response.CatalogVersionID); got != publishAttributionActor {
		t.Errorf("the catalogue twin says published_by = %d, want %d", got, publishAttributionActor)
	}
	// The category still rides along. The overlay grew a key; the two it
	// already carried must survive the change.
	var category string
	if err := pool.QueryRow(ctx, `
SELECT meta->>'category' FROM p_1.application_versions WHERE id = $1`,
		response.CatalogVersionID).Scan(&category); err != nil {
		t.Fatalf("read the twin's category: %v", err)
	}
	if category != "Development" {
		t.Errorf("twin category = %q, want %q", category, "Development")
	}

	// The dashboard, through its own route. This is the surface the defect was
	// reported on, and the JSON aggregation that builds it reads the key by
	// name — a stamp the aggregation cannot see is not a fix.
	items := publishAttributionDashboard(t, router)
	if len(items) != 1 {
		t.Fatalf("dashboard lists %d agents, want 1: %v", len(items), items)
	}
	versions, _ := items[0]["published_versions"].([]any)
	if len(versions) != 1 {
		t.Fatalf("dashboard lists %d published versions, want 1: %v", len(versions), items[0])
	}
	version, _ := versions[0].(map[string]any)
	publishedBy, ok := version["published_by"].(float64)
	if !ok {
		t.Fatalf("dashboard published_by = %#v, want the publisher's id", version["published_by"])
	}
	if int(publishedBy) != publishAttributionActor {
		t.Errorf("dashboard published_by = %d, want %d", int(publishedBy), publishAttributionActor)
	}
}

// TestPublishWithoutAPrincipalAttributesTheDraftAuthor covers the fallback.
//
// A request that carries no resolvable owner — a token principal with no owner
// behind it — must not leave the row unattributed while the platform still
// holds an answer. The draft's author is that answer, and it is the same rule
// the skill publish path applies.
func TestPublishWithoutAPrincipalAttributesTheDraftAuthor(t *testing.T) {
	pool := newCatalogMirrorPool(t)
	router := publishAttributionRouter(eliteacore.NewHandler(pool))
	fixture := seedCatalogMirrorFixture(t, pool, 2, "attribution fallback agent")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var authorID int
	if err := pool.QueryRow(ctx, `
SELECT author_id FROM p_2.application_versions WHERE id = $1`, fixture.versionID).Scan(&authorID); err != nil {
		t.Fatalf("read the seeded author: %v", err)
	}

	// No principal at all: the request carries no authenticated user.
	recorder := catalogMirrorPublish(t, router, fixture, map[string]any{
		"version_name":     "v-fallback",
		"validation_token": catalogMirrorToken(t, pool, fixture),
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("publish status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		PublicVersionID string `json:"public_version_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode publish response %q: %v", recorder.Body.String(), err)
	}
	if got := publishedByOf(t, ctx, pool, "p_2", response.PublicVersionID); got != authorID {
		t.Errorf("published_by = %d, want the draft's author %d", got, authorID)
	}
}

/* ── helpers ───────────────────────────────────────────────────────────── */

// publishAttributionRouter adds the operator dashboard to the mirror router.
//
// No permission middleware: the gate is applied at the mount in router.go and
// covered there. What is under test is what the route answers once a caller
// has been admitted.
func publishAttributionRouter(handler *eliteacore.Handler) chi.Router {
	router := catalogMirrorRouter(handler)
	router.Get("/elitea_core/admin_published_agents/administration", handler.AdminPublishedAgents)
	return router
}

// publishAttributionPublish issues a publish AS a user, the way the
// authentication middleware presents one to a handler.
func publishAttributionPublish(
	t *testing.T, router chi.Router, fixture catalogMirrorFixture, userID int, body any,
) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	target := fmt.Sprintf("/elitea_core/publish/prompt_lib/%d/%d", fixture.projectID, fixture.versionID)
	request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	principal := auth.User{ID: fmt.Sprintf("%d", userID), UserID: fmt.Sprintf("%d", userID)}
	request = request.WithContext(auth.ContextWithUser(request.Context(), principal))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// publishedByOf reads the attribution one published row carries.
//
// The pointer is the point: a row with no key scans as NULL, and reporting
// that as 0 would let "nobody published this" pass for user 0.
func publishedByOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema, versionID string) int {
	t.Helper()
	var stamped *int
	query := fmt.Sprintf(`SELECT (meta->>'published_by')::int FROM %s.application_versions WHERE id = $1`, schema)
	if err := pool.QueryRow(ctx, query, versionID).Scan(&stamped); err != nil {
		t.Fatalf("read %s published_by: %v", schema, err)
	}
	if stamped == nil {
		t.Fatalf("%s version %s carries no published_by", schema, versionID)
	}
	return *stamped
}

// publishAttributionDashboard reads the operator listing through its route.
func publishAttributionDashboard(t *testing.T, router chi.Router) []map[string]any {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet,
		"/elitea_core/admin_published_agents/administration", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("admin_published_agents status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode admin_published_agents %q: %v", recorder.Body.String(), err)
	}
	return body.Items
}
