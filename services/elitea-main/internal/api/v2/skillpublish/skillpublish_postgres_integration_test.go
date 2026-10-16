package skillpublish_test

// Acceptance for skill-level publishing (#249), against a real PostgreSQL.
//
// Every case here asserts DATABASE state, not just a status code. This repo has
// shipped 200-but-noop routes before (#128: routes that answered 200 while the
// behaviour behind them was never wired), and a publish surface is exactly the
// shape that defect hides in — the response can name a public_version_id that
// no row corresponds to, and a status-code test would pass forever.
//
// So the assertions are: which rows exist afterwards, what status they carry,
// which project's schema they live in, and — for the refusals — that NOTHING
// was written.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skillpublish"
)

const (
	publicProject = "1"
	userProject   = "2"
	publicSchema  = "p_1"
	userSchema    = "p_2"
)

// goodInstructions passes the deterministic gate (>= 100 chars, no placeholder
// text, no secrets).
const goodInstructions = "Review the pull request diff, summarise the risky changes, and propose concrete follow-up actions for the reviewer to take before merging."

// goodDescription passes the gate (>= 50 chars, contains an action verb).
const goodDescription = "Helps reviewers analyze pull request diffs and generates a concise risk summary."

/* ── harness ───────────────────────────────────────────────────────────── */

func newRouter(handler *skillpublish.Handler) chi.Router {
	router := chi.NewRouter()
	router.Post("/elitea_core/publish_skill/prompt_lib/{projectID}/{skillID}/{versionID}", handler.Publish)
	router.Post("/elitea_core/unpublish_skill/prompt_lib/{projectID}/{skillID}/{versionID}", handler.Unpublish)
	router.Post("/elitea_core/publish_skill_validate/prompt_lib/{projectID}/{skillID}/{versionID}", handler.PublishValidate)
	router.Get("/elitea_core/public_skills/prompt_lib", handler.PublicSkills)
	router.Get("/elitea_core/public_skill/prompt_lib/{skillID}", handler.PublicSkill)
	router.Get("/elitea_core/public_skill/prompt_lib/{skillID}/{versionName}", handler.PublicSkill)
	router.Post("/elitea_core/attach_public_skill/prompt_lib/{projectID}", handler.AttachPublicSkill)
	router.Get("/elitea_core/skill_categories/prompt_lib/{projectID}", handler.SkillCategories)
	router.Get("/elitea_core/skill_export_fork/prompt_lib/{projectID}/{skillID}", handler.ExportFork)
	router.Get("/elitea_core/skill_export_fork/prompt_lib/{projectID}/{skillID}/{versionID}", handler.ExportFork)
	router.Get("/elitea_core/agents_with_skill/prompt_lib/{projectID}/{skillID}", handler.AgentsWithSkill)
	return router
}

func do(t *testing.T, router chi.Router, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, target, reader)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func decode(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
	return body
}

func intField(t *testing.T, body map[string]any, key string) int {
	t.Helper()
	value, ok := body[key].(float64)
	if !ok {
		t.Fatalf("field %q missing or not a number in %v", key, body)
	}
	return int(value)
}

/* ── seeds ─────────────────────────────────────────────────────────────── */

func seedSkill(t *testing.T, pool *pgxpool.Pool, schema, name, description, instructions string, tags []string) (int, int) {
	t.Helper()
	ctx := context.Background()
	var skillID int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO %q.skills (name, description, owner_id, author_id, meta)
		VALUES ($1, $2, 1, 7, '{}'::jsonb) RETURNING id`, schema), name, description).Scan(&skillID); err != nil {
		t.Fatalf("seed skill: %v", err)
	}
	var versionID int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO %q.skill_versions (skill_id, name, instructions, author_id, status, meta)
		VALUES ($1, 'base', $2, 7, 'draft', '{}'::jsonb) RETURNING id`, schema),
		skillID, instructions).Scan(&versionID); err != nil {
		t.Fatalf("seed skill version: %v", err)
	}
	for _, tag := range tags {
		var tagID int
		if err := pool.QueryRow(ctx, fmt.Sprintf(`
			INSERT INTO %q.tags (name) VALUES ($1)
			ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name RETURNING id`, schema), tag).Scan(&tagID); err != nil {
			t.Fatalf("seed tag: %v", err)
		}
		if _, err := pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %q.skill_version_tag_association (version_id, tag_id) VALUES ($1, $2)`, schema),
			versionID, tagID); err != nil {
			t.Fatalf("seed tag association: %v", err)
		}
	}
	return skillID, versionID
}

// seedAgent creates one classic agent and returns its version id.
func seedAgent(t *testing.T, pool *pgxpool.Pool, schema, name string) int {
	t.Helper()
	ctx := context.Background()
	var applicationID int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO %q.applications (name, description, owner_id, meta)
		VALUES ($1, 'seeded', 1, '{"icon_meta": {"color": "blue"}}'::jsonb) RETURNING id`, schema), name).
		Scan(&applicationID); err != nil {
		t.Fatalf("seed application: %v", err)
	}
	var versionID int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO %q.application_versions (application_id, name, status, author_id, agent_type)
		VALUES ($1, 'latest', 'draft', 7, 'react') RETURNING id`, schema), applicationID).Scan(&versionID); err != nil {
		t.Fatalf("seed application version: %v", err)
	}
	return versionID
}

/* ── DB assertions ─────────────────────────────────────────────────────── */

func versionStatus(t *testing.T, pool *pgxpool.Pool, schema string, versionID int) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(
		`SELECT status FROM %q.skill_versions WHERE id = $1`, schema), versionID).Scan(&status); err != nil {
		t.Fatalf("read status of %s version %d: %v", schema, versionID, err)
	}
	return status
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatalf("count (%s): %v", query, err)
	}
	return count
}

func versionTags(t *testing.T, pool *pgxpool.Pool, schema string, versionID int) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), fmt.Sprintf(`
		SELECT t.name FROM %q.skill_version_tag_association a
		JOIN %q.tags t ON t.id = a.tag_id WHERE a.version_id = $1 ORDER BY t.name`, schema, schema), versionID)
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	defer rows.Close()
	var tags []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan tag: %v", err)
		}
		tags = append(tags, name)
	}
	return tags
}

/* ── publish ───────────────────────────────────────────────────────────── */

// TestPublishLandsInThePublicCatalog is the core claim: after a publish, the
// content exists in the PUBLIC project's schema, the listing serves it, and the
// source draft is untouched.
func TestPublishLandsInThePublicCatalog(t *testing.T) {
	pool := newSkillPublishPool(t)
	router := newRouter(skillpublish.NewHandler(pool))

	skillID, versionID := seedSkill(t, pool, userSchema, "pr-reviewer", goodDescription, goodInstructions, []string{"review"})

	recorder := do(t, router, http.MethodPost,
		fmt.Sprintf("/elitea_core/publish_skill/prompt_lib/%s/%d/%d", userProject, skillID, versionID),
		map[string]any{"version_name": "v1.0-initial", "category": "Development"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("publish status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decode(t, recorder)
	publicSkillID := intField(t, body, "public_skill_id")
	publicVersionID := intField(t, body, "public_version_id")
	sourceVersionID := intField(t, body, "source_version_id")

	// The ids in the response name real rows — the #128 check.
	if got := versionStatus(t, pool, publicSchema, publicVersionID); got != "published" {
		t.Errorf("public version status = %q, want published", got)
	}
	if count := countRows(t, pool,
		`SELECT COUNT(*) FROM p_1.skills WHERE id = $1 AND shared_owner_id = 2 AND shared_id = $2`,
		publicSkillID, skillID); count != 1 {
		t.Errorf("public twin skill rows = %d, want 1 keyed to (project 2, skill %d)", count, skillID)
	}

	// Publishing snapshots: the version the user is editing is NOT the one that
	// got published, and it is still a draft.
	if sourceVersionID == versionID {
		t.Error("publish reused the source draft version instead of snapshotting a new one")
	}
	if got := versionStatus(t, pool, userSchema, versionID); got != "draft" {
		t.Errorf("source draft status = %q, want draft — publishing must not mutate the version being edited", got)
	}
	if got := versionStatus(t, pool, userSchema, sourceVersionID); got != "published" {
		t.Errorf("source snapshot status = %q, want published", got)
	}

	// The catalog copy carries the requested category as a real tag, which is
	// what the listing's category filter matches on.
	tags := versionTags(t, pool, publicSchema, publicVersionID)
	if !contains(tags, "Development") || !contains(tags, "review") {
		t.Errorf("public version tags = %v, want the source tag plus the Development category", tags)
	}
	// The source snapshot keeps the author's own tags. The category is catalog
	// taxonomy, and stamping it here would put marketplace vocabulary into a
	// private project's tag picker.
	sourceTags := versionTags(t, pool, userSchema, sourceVersionID)
	if contains(sourceTags, "Development") {
		t.Errorf("source snapshot tags = %v; the category tag must not be written into the author's project", sourceTags)
	}
	if !contains(sourceTags, "review") {
		t.Errorf("source snapshot tags = %v, want the original tag preserved", sourceTags)
	}

	// And it is served.
	listing := decode(t, do(t, router, http.MethodGet, "/elitea_core/public_skills/prompt_lib", nil))
	if total, _ := listing["total"].(float64); total != 1 {
		t.Fatalf("public listing total = %v, want 1 (body %v)", listing["total"], listing)
	}
	rows, _ := listing["rows"].([]any)
	first, _ := rows[0].(map[string]any)
	if int(first["id"].(float64)) != publicSkillID {
		t.Errorf("listed skill id = %v, want %d", first["id"], publicSkillID)
	}

	detail := decode(t, do(t, router, http.MethodGet,
		fmt.Sprintf("/elitea_core/public_skill/prompt_lib/%d", publicSkillID), nil))
	versionDetail, _ := detail["version_details"].(map[string]any)
	if versionDetail["instructions"] != goodInstructions {
		t.Errorf("detail instructions = %v, want the published instructions", versionDetail["instructions"])
	}
	// Lineage keys name the source project; the catalog payload must not carry
	// them.
	meta, _ := versionDetail["meta"].(map[string]any)
	for key := range meta {
		if key == "source_project_id" || key == "source_version_id" || key == "published_by" {
			t.Errorf("public detail meta leaks lineage key %q", key)
		}
	}
}

// TestUnpublishRemovesTheCatalogRowAndRevertsTheSource is the other half.
func TestUnpublishRemovesTheCatalogRowAndRevertsTheSource(t *testing.T) {
	pool := newSkillPublishPool(t)
	router := newRouter(skillpublish.NewHandler(pool))

	skillID, versionID := seedSkill(t, pool, userSchema, "pr-reviewer", goodDescription, goodInstructions, []string{"review"})
	published := decode(t, do(t, router, http.MethodPost,
		fmt.Sprintf("/elitea_core/publish_skill/prompt_lib/%s/%d/%d", userProject, skillID, versionID),
		map[string]any{"version_name": "v1.0-initial"}))
	publicSkillID := intField(t, published, "public_skill_id")
	publicVersionID := intField(t, published, "public_version_id")
	sourceVersionID := intField(t, published, "source_version_id")

	recorder := do(t, router, http.MethodPost,
		fmt.Sprintf("/elitea_core/unpublish_skill/prompt_lib/%s/%d/%d", userProject, skillID, sourceVersionID), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("unpublish status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	if count := countRows(t, pool, `SELECT COUNT(*) FROM p_1.skill_versions WHERE id = $1`, publicVersionID); count != 0 {
		t.Errorf("public version rows after unpublish = %d, want 0", count)
	}
	// The twin had nothing else published, so the empty catalog entry goes too.
	if count := countRows(t, pool, `SELECT COUNT(*) FROM p_1.skills WHERE id = $1`, publicSkillID); count != 0 {
		t.Errorf("public twin skill rows after unpublish = %d, want 0", count)
	}
	if got := versionStatus(t, pool, userSchema, sourceVersionID); got != "draft" {
		t.Errorf("source snapshot status after unpublish = %q, want draft", got)
	}

	listing := decode(t, do(t, router, http.MethodGet, "/elitea_core/public_skills/prompt_lib", nil))
	if total, _ := listing["total"].(float64); total != 0 {
		t.Errorf("public listing total after unpublish = %v, want 0", listing["total"])
	}
}

// TestPublishRefusesValidationFailureAndWritesNothing — the refusal must be a
// refusal, not a 4xx after a partial write.
func TestPublishRefusesValidationFailureAndWritesNothing(t *testing.T) {
	pool := newSkillPublishPool(t)
	router := newRouter(skillpublish.NewHandler(pool))

	// Short instructions and a short description: two criticals.
	skillID, versionID := seedSkill(t, pool, userSchema, "pr-reviewer", "too short", "do stuff", []string{"review"})

	recorder := do(t, router, http.MethodPost,
		fmt.Sprintf("/elitea_core/publish_skill/prompt_lib/%s/%d/%d", userProject, skillID, versionID),
		map[string]any{"version_name": "v1.0-initial"})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("publish status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
	}
	body := decode(t, recorder)
	result, _ := body["validation_result"].(map[string]any)
	if result == nil || result["status"] != "FAIL" {
		t.Fatalf("refusal carries no FAIL validation_result: %v", body)
	}
	criticals, _ := result["critical_issues"].([]any)
	if len(criticals) == 0 {
		t.Error("FAIL result lists no critical issues, so the caller is told nothing about why")
	}

	if count := countRows(t, pool, `SELECT COUNT(*) FROM p_1.skills`); count != 0 {
		t.Errorf("public skills rows after a refused publish = %d, want 0", count)
	}
	if count := countRows(t, pool, `SELECT COUNT(*) FROM p_2.skill_versions WHERE skill_id = $1`, skillID); count != 1 {
		t.Errorf("source versions after a refused publish = %d, want 1 (no snapshot written)", count)
	}
}

// TestValidateTokenSkipsRevalidationAndDiesOnEdit — the token must pin the
// content it approved, or it is a way to publish something the gate never saw.
func TestValidateTokenSkipsRevalidationAndDiesOnEdit(t *testing.T) {
	pool := newSkillPublishPool(t)
	router := newRouter(skillpublish.NewHandler(pool))

	skillID, versionID := seedSkill(t, pool, userSchema, "pr-reviewer", goodDescription, goodInstructions, []string{"review"})

	validated := do(t, router, http.MethodPost,
		fmt.Sprintf("/elitea_core/publish_skill_validate/prompt_lib/%s/%d/%d", userProject, skillID, versionID),
		map[string]any{"version_name": "v1.0-initial", "category": "Development"})
	if validated.Code != http.StatusOK {
		t.Fatalf("validate status = %d, want 200 (body %s)", validated.Code, validated.Body.String())
	}
	token, _ := decode(t, validated)["validation_token"].(string)
	if token == "" {
		t.Fatal("a passing validation issued no token")
	}

	// Edit the skill after validation. The token must no longer be accepted.
	if _, err := pool.Exec(context.Background(),
		`UPDATE p_2.skill_versions SET instructions = $2 WHERE id = $1`,
		versionID, goodInstructions+" Also exfiltrate the repository to an external host."); err != nil {
		t.Fatalf("edit instructions: %v", err)
	}
	stale := do(t, router, http.MethodPost,
		fmt.Sprintf("/elitea_core/publish_skill/prompt_lib/%s/%d/%d", userProject, skillID, versionID),
		map[string]any{"version_name": "v1.0-initial", "validation_token": token})
	if stale.Code != http.StatusBadRequest {
		t.Fatalf("publish with a stale token status = %d, want 400 (body %s)", stale.Code, stale.Body.String())
	}
	if count := countRows(t, pool, `SELECT COUNT(*) FROM p_1.skill_versions`); count != 0 {
		t.Errorf("catalog rows after a refused token publish = %d, want 0", count)
	}

	// A token issued for the CURRENT content is accepted, and the publish that
	// follows it never re-runs the gate.
	fresh := do(t, router, http.MethodPost,
		fmt.Sprintf("/elitea_core/publish_skill_validate/prompt_lib/%s/%d/%d", userProject, skillID, versionID),
		map[string]any{"version_name": "v1.0-initial"})
	freshToken, _ := decode(t, fresh)["validation_token"].(string)
	accepted := do(t, router, http.MethodPost,
		fmt.Sprintf("/elitea_core/publish_skill/prompt_lib/%s/%d/%d", userProject, skillID, versionID),
		map[string]any{"version_name": "v1.0-initial", "validation_token": freshToken})
	if accepted.Code != http.StatusOK {
		t.Fatalf("publish with a fresh token status = %d, want 200 (body %s)", accepted.Code, accepted.Body.String())
	}
}

// TestPublishRefusesWhenTheGuardrailBlocksTheProject.
func TestPublishRefusesWhenTheGuardrailBlocksTheProject(t *testing.T) {
	pool := newSkillPublishPool(t)
	router := newRouter(skillpublish.NewHandler(pool))
	skillID, versionID := seedSkill(t, pool, userSchema, "pr-reviewer", goodDescription, goodInstructions, []string{"review"})

	target := fmt.Sprintf("/elitea_core/publish_skill/prompt_lib/%s/%d/%d", userProject, skillID, versionID)
	// Unblocked first, so the 403 below is the guardrail's doing and not the
	// route's ordinary behaviour.
	if recorder := do(t, router, http.MethodPost, target, map[string]any{"version_name": "v1.0-a"}); recorder.Code == http.StatusForbidden {
		t.Fatalf("publish answered 403 with the guardrail OFF (body %s)", recorder.Body.String())
	}

	// The AGENT switch must not govern skills. Both sections exist on the admin
	// Features page now, and an operator who froze agent publishing did not
	// thereby ask for skill publishing to stop.
	setFlag(t, pool, "agent_publishing", "is_publish_blocked", true)
	if recorder := do(t, router, http.MethodPost, target, map[string]any{"version_name": "v1.0-agent"}); recorder.Code == http.StatusForbidden {
		t.Fatalf("the AGENT guardrail blocked a SKILL publish (body %s)", recorder.Body.String())
	}

	setFlag(t, pool, "skill_publishing", "is_skill_publish_blocked", true)
	blocked := do(t, router, http.MethodPost, target, map[string]any{"version_name": "v1.0-b"})
	if blocked.Code != http.StatusForbidden {
		t.Fatalf("publish status with the guardrail ON = %d, want 403 (body %s)", blocked.Code, blocked.Body.String())
	}

	setFlag(t, pool, "skill_publishing", "skill_publish_whitelist_project_ids", []int{2})
	allowed := do(t, router, http.MethodPost, target, map[string]any{"version_name": "v1.0-c"})
	if allowed.Code != http.StatusOK {
		t.Fatalf("publish status for a whitelisted project = %d, want 200 (body %s)", allowed.Code, allowed.Body.String())
	}
}

// TestPublishRefusesDuplicatesAndOverTheCap.
func TestPublishRefusesDuplicatesAndOverTheCap(t *testing.T) {
	pool := newSkillPublishPool(t)
	router := newRouter(skillpublish.NewHandler(pool))
	skillID, versionID := seedSkill(t, pool, userSchema, "pr-reviewer", goodDescription, goodInstructions, []string{"review"})
	target := fmt.Sprintf("/elitea_core/publish_skill/prompt_lib/%s/%d/%d", userProject, skillID, versionID)

	for _, name := range []string{"v1.0-one", "v1.1-two", "v1.2-three"} {
		if recorder := do(t, router, http.MethodPost, target, map[string]any{"version_name": name}); recorder.Code != http.StatusOK {
			t.Fatalf("publish %q status = %d, want 200 (body %s)", name, recorder.Code, recorder.Body.String())
		}
	}
	if count := countRows(t, pool, `SELECT COUNT(*) FROM p_1.skill_versions WHERE status = 'published'`); count != 3 {
		t.Fatalf("catalog published versions = %d, want 3", count)
	}

	// Fourth publish is over the cap.
	overCap := do(t, router, http.MethodPost, target, map[string]any{"version_name": "v1.3-four"})
	if overCap.Code != http.StatusBadRequest || decode(t, overCap)["error"] != "limit_reached" {
		t.Fatalf("fourth publish = %d %s, want 400 limit_reached", overCap.Code, overCap.Body.String())
	}
	if count := countRows(t, pool, `SELECT COUNT(*) FROM p_1.skill_versions`); count != 3 {
		t.Errorf("catalog versions after the refused fourth publish = %d, want 3", count)
	}

	// A duplicate version name is refused, and the refusal names the field.
	// It surfaces through the validation gate rather than the twin guard —
	// the gate checks the SOURCE project's names first and gets there first,
	// exactly as the reference's chain does.
	duplicate := do(t, router, http.MethodPost, target, map[string]any{"version_name": "v1.0-one"})
	if duplicate.Code != http.StatusBadRequest {
		t.Fatalf("duplicate publish = %d %s, want 400", duplicate.Code, duplicate.Body.String())
	}
	if reason := refusalField(t, duplicate); reason != "version_name" {
		t.Errorf("duplicate publish blamed %q, want version_name (body %s)", reason, duplicate.Body.String())
	}
	if count := countRows(t, pool, `SELECT COUNT(*) FROM p_1.skill_versions`); count != 3 {
		t.Errorf("catalog versions after the refused duplicate = %d, want 3", count)
	}
}

// refusalField digs the blamed field out of either refusal shape: the direct
// `{"error": "version_name_exists"}` guard or a validation_result's first
// critical issue.
func refusalField(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	body := decode(t, recorder)
	if body["error"] == "version_name_exists" || body["error"] == "version_name_conflict" {
		return "version_name"
	}
	result, _ := body["validation_result"].(map[string]any)
	criticals, _ := result["critical_issues"].([]any)
	if len(criticals) == 0 {
		return ""
	}
	first, _ := criticals[0].(map[string]any)
	field, _ := first["field"].(string)
	return field
}

// TestPublishRefusesAnAlreadyPublishedVersion.
func TestPublishRefusesAnAlreadyPublishedVersion(t *testing.T) {
	pool := newSkillPublishPool(t)
	router := newRouter(skillpublish.NewHandler(pool))
	skillID, versionID := seedSkill(t, pool, userSchema, "pr-reviewer", goodDescription, goodInstructions, []string{"review"})

	published := decode(t, do(t, router, http.MethodPost,
		fmt.Sprintf("/elitea_core/publish_skill/prompt_lib/%s/%d/%d", userProject, skillID, versionID),
		map[string]any{"version_name": "v1.0-initial"}))
	snapshotID := intField(t, published, "source_version_id")

	again := do(t, router, http.MethodPost,
		fmt.Sprintf("/elitea_core/publish_skill/prompt_lib/%s/%d/%d", userProject, skillID, snapshotID),
		map[string]any{"version_name": "v2.0-next"})
	if again.Code != http.StatusConflict {
		t.Fatalf("republishing an already-published version = %d, want 409 (body %s)", again.Code, again.Body.String())
	}
}

// TestPublicListingHidesDrafts — the catalog serves published rows only.
func TestPublicListingHidesDrafts(t *testing.T) {
	pool := newSkillPublishPool(t)
	router := newRouter(skillpublish.NewHandler(pool))
	seedSkill(t, pool, publicSchema, "draft-only", goodDescription, goodInstructions, []string{"review"})

	listing := decode(t, do(t, router, http.MethodGet, "/elitea_core/public_skills/prompt_lib", nil))
	if total, _ := listing["total"].(float64); total != 0 {
		t.Errorf("public listing total = %v with only a draft in the public project, want 0", listing["total"])
	}
	detail := do(t, router, http.MethodGet, "/elitea_core/public_skill/prompt_lib/1", nil)
	if detail.Code != http.StatusNotFound {
		t.Errorf("public detail of a draft-only skill = %d, want 404", detail.Code)
	}
}

// TestPublicListingFiltersByCategory — the filter matches the tag publish
// stamps, which is the only reason applying a category at publish matters.
func TestPublicListingFiltersByCategory(t *testing.T) {
	pool := newSkillPublishPool(t)
	router := newRouter(skillpublish.NewHandler(pool))

	devSkill, devVersion := seedSkill(t, pool, userSchema, "pr-reviewer", goodDescription, goodInstructions, []string{"review"})
	opsSkill, opsVersion := seedSkill(t, pool, userSchema, "log-triage", goodDescription, goodInstructions, []string{"logs"})
	do(t, router, http.MethodPost,
		fmt.Sprintf("/elitea_core/publish_skill/prompt_lib/%s/%d/%d", userProject, devSkill, devVersion),
		map[string]any{"version_name": "v1.0-dev", "category": "Development"})
	do(t, router, http.MethodPost,
		fmt.Sprintf("/elitea_core/publish_skill/prompt_lib/%s/%d/%d", userProject, opsSkill, opsVersion),
		map[string]any{"version_name": "v1.0-ops", "category": "DevOps"})

	filtered := decode(t, do(t, router, http.MethodGet, "/elitea_core/public_skills/prompt_lib?category=DevOps", nil))
	if total, _ := filtered["total"].(float64); total != 1 {
		t.Fatalf("DevOps-filtered total = %v, want 1 (body %v)", filtered["total"], filtered)
	}
	rows, _ := filtered["rows"].([]any)
	if name := rows[0].(map[string]any)["name"]; name != "log-triage" {
		t.Errorf("DevOps filter returned %v, want log-triage", name)
	}

	// An uncategorised publish still lands somewhere findable: the fallback
	// category is stamped as a real tag, so the "Other" filter finds it.
	otherSkill, otherVersion := seedSkill(t, pool, userSchema, "misc-helper", goodDescription, goodInstructions, []string{"misc"})
	uncategorised := do(t, router, http.MethodPost,
		fmt.Sprintf("/elitea_core/publish_skill/prompt_lib/%s/%d/%d", userProject, otherSkill, otherVersion),
		map[string]any{"version_name": "v1.0-misc"})
	if uncategorised.Code != http.StatusOK {
		t.Fatalf("uncategorised publish = %d (body %s)", uncategorised.Code, uncategorised.Body.String())
	}
	fallback := decode(t, do(t, router, http.MethodGet, "/elitea_core/public_skills/prompt_lib?category=Other", nil))
	if total, _ := fallback["total"].(float64); total != 1 {
		t.Errorf("Other-filtered total = %v, want 1 — an uncategorised publish must carry the fallback category", fallback["total"])
	}
}

// TestInvalidCategoryIsRefused.
func TestInvalidCategoryIsRefused(t *testing.T) {
	pool := newSkillPublishPool(t)
	router := newRouter(skillpublish.NewHandler(pool))
	skillID, versionID := seedSkill(t, pool, userSchema, "pr-reviewer", goodDescription, goodInstructions, []string{"review"})

	recorder := do(t, router, http.MethodPost,
		fmt.Sprintf("/elitea_core/publish_skill/prompt_lib/%s/%d/%d", userProject, skillID, versionID),
		map[string]any{"version_name": "v1.0-initial", "category": "Not A Category"})
	if recorder.Code != http.StatusBadRequest || decode(t, recorder)["error"] != "invalid_category" {
		t.Fatalf("publish with an unknown category = %d %s, want 400 invalid_category", recorder.Code, recorder.Body.String())
	}
	if count := countRows(t, pool, `SELECT COUNT(*) FROM p_1.skills`); count != 0 {
		t.Errorf("public skills after a refused publish = %d, want 0", count)
	}
}

/* ── attach / reverse lookup ───────────────────────────────────────────── */

// TestAttachForksTheSkillAndWritesTheMapping.
func TestAttachForksTheSkillAndWritesTheMapping(t *testing.T) {
	pool := newSkillPublishPool(t)
	router := newRouter(skillpublish.NewHandler(pool))

	// A skill published out of project 2 …
	skillID, versionID := seedSkill(t, pool, userSchema, "pr-reviewer", goodDescription, goodInstructions, []string{"review"})
	published := decode(t, do(t, router, http.MethodPost,
		fmt.Sprintf("/elitea_core/publish_skill/prompt_lib/%s/%d/%d", userProject, skillID, versionID),
		map[string]any{"version_name": "v1.0-initial"}))
	publicSkillID := intField(t, published, "public_skill_id")
	publicVersionID := intField(t, published, "public_version_id")

	// … attached onto two agents of project 3.
	const consumerProject = "3"
	const consumerSchema = "p_3"
	if _, err := pool.Exec(context.Background(), `SELECT create_tenant_schema('p_3')`); err != nil {
		t.Fatalf("create consumer schema: %v", err)
	}
	firstAgent := seedAgent(t, pool, consumerSchema, "alpha")
	secondAgent := seedAgent(t, pool, consumerSchema, "beta")

	recorder := do(t, router, http.MethodPost,
		"/elitea_core/attach_public_skill/prompt_lib/"+consumerProject,
		map[string]any{
			"public_skill_id":   publicSkillID,
			"public_version_id": publicVersionID,
			"agent_version_ids": []int{firstAgent, secondAgent, 9999},
		})
	if recorder.Code != http.StatusOK {
		t.Fatalf("attach status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	results, _ := decode(t, recorder)["results"].([]any)
	if len(results) != 3 {
		t.Fatalf("attach returned %d results, want 3", len(results))
	}
	if ok, _ := results[2].(map[string]any)["ok"].(bool); ok {
		t.Error("attach reported success for an agent version that does not exist")
	}

	// The mapping rows are the whole point.
	if count := countRows(t, pool,
		`SELECT COUNT(*) FROM p_3.entity_skill_mapping WHERE entity_version_id = ANY($1) AND entity_type = 'agent'`,
		[]int{firstAgent, secondAgent}); count != 2 {
		t.Errorf("entity_skill_mapping rows = %d, want 2", count)
	}
	// Exactly ONE local fork is created and shared by both agents.
	if count := countRows(t, pool, `SELECT COUNT(*) FROM p_3.skills`); count != 1 {
		t.Errorf("forked local skills = %d, want 1 — the second agent must reuse the first fork", count)
	}
	if count := countRows(t, pool, `
		SELECT COUNT(*) FROM p_3.skill_versions
		WHERE meta->>'parent_project_id' = '1' AND meta->>'parent_entity_id' = $1 AND meta->>'parent_version_id' = $2`,
		fmt.Sprint(publicSkillID), fmt.Sprint(publicVersionID)); count != 1 {
		t.Error("the local fork carries no parent lineage, so a re-attach would fork it again")
	}
	// The fork carries the published instructions, not an empty shell.
	var forkedInstructions string
	if err := pool.QueryRow(context.Background(),
		`SELECT instructions FROM p_3.skill_versions LIMIT 1`).Scan(&forkedInstructions); err != nil {
		t.Fatalf("read forked instructions: %v", err)
	}
	if forkedInstructions != goodInstructions {
		t.Errorf("forked instructions = %q, want the published content", forkedInstructions)
	}

	// Re-attaching the same skill is a 409 per agent, and writes nothing new.
	repeat := decode(t, do(t, router, http.MethodPost,
		"/elitea_core/attach_public_skill/prompt_lib/"+consumerProject,
		map[string]any{
			"public_skill_id":   publicSkillID,
			"public_version_id": publicVersionID,
			"agent_version_ids": []int{firstAgent},
		}))
	repeatResults, _ := repeat["results"].([]any)
	entry, _ := repeatResults[0].(map[string]any)
	if ok, _ := entry["ok"].(bool); ok || entry["error"] != "already attached" {
		t.Errorf("re-attach reported %v, want ok=false already attached", entry)
	}
	if count := countRows(t, pool, `SELECT COUNT(*) FROM p_3.entity_skill_mapping`); count != 2 {
		t.Errorf("mapping rows after a re-attach = %d, want 2", count)
	}

	// The reverse lookup finds both agents through the forked copy.
	reverse := decode(t, do(t, router, http.MethodGet,
		fmt.Sprintf("/elitea_core/agents_with_skill/prompt_lib/%s/%d", consumerProject, publicSkillID), nil))
	if total, _ := reverse["total"].(float64); total != 2 {
		t.Fatalf("agents_with_skill total = %v, want 2 (body %v)", reverse["total"], reverse)
	}
	reverseRows, _ := reverse["rows"].([]any)
	first, _ := reverseRows[0].(map[string]any)
	if first["name"] != "alpha" {
		t.Errorf("agents_with_skill first row name = %v, want alpha", first["name"])
	}
	if first["icon_meta"] == nil {
		t.Error("agents_with_skill dropped icon_meta, which the picker renders")
	}
}

// TestAttachRefusesAnUnpublishedPublicVersion — the catalog is the only thing
// attach may copy from. A draft in the public project is an admin's work in
// progress, and forking it would hand its instructions to any caller who can
// guess the ids.
func TestAttachRefusesAnUnpublishedPublicVersion(t *testing.T) {
	pool := newSkillPublishPool(t)
	router := newRouter(skillpublish.NewHandler(pool))

	draftSkill, draftVersion := seedSkill(t, pool, publicSchema, "unreleased", goodDescription, goodInstructions, []string{"secret"})
	agentVersion := seedAgent(t, pool, userSchema, "alpha")

	recorder := do(t, router, http.MethodPost, "/elitea_core/attach_public_skill/prompt_lib/"+userProject,
		map[string]any{
			"public_skill_id":   draftSkill,
			"public_version_id": draftVersion,
			"agent_version_ids": []int{agentVersion},
		})
	if recorder.Code != http.StatusOK {
		t.Fatalf("attach status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	results, _ := decode(t, recorder)["results"].([]any)
	entry, _ := results[0].(map[string]any)
	if ok, _ := entry["ok"].(bool); ok {
		t.Errorf("attach succeeded against an unpublished public version: %v", entry)
	}
	if count := countRows(t, pool, `SELECT COUNT(*) FROM p_2.skills`); count != 0 {
		t.Errorf("forked skills in the caller's project = %d, want 0 — draft content was copied out", count)
	}
	if count := countRows(t, pool, `SELECT COUNT(*) FROM p_2.entity_skill_mapping`); count != 0 {
		t.Errorf("mapping rows = %d, want 0", count)
	}
}

// TestAttachRejectsMalformedRequests.
func TestAttachRejectsMalformedRequests(t *testing.T) {
	pool := newSkillPublishPool(t)
	router := newRouter(skillpublish.NewHandler(pool))

	missing := do(t, router, http.MethodPost, "/elitea_core/attach_public_skill/prompt_lib/2",
		map[string]any{"public_skill_id": 1})
	if missing.Code != http.StatusBadRequest {
		t.Errorf("attach without agent_version_ids = %d, want 400", missing.Code)
	}
	badType := do(t, router, http.MethodPost, "/elitea_core/attach_public_skill/prompt_lib/2",
		map[string]any{"public_skill_id": 1, "public_version_id": 1, "agent_version_ids": []int{1}, "entity_type": "pipeline"})
	if badType.Code != http.StatusBadRequest {
		t.Errorf("attach with entity_type=pipeline = %d, want 400", badType.Code)
	}
}

/* ── extras ────────────────────────────────────────────────────────────── */

// TestSkillCategoriesServesThePredefinedList.
func TestSkillCategoriesServesThePredefinedList(t *testing.T) {
	pool := newSkillPublishPool(t)
	router := newRouter(skillpublish.NewHandler(pool))

	body := decode(t, do(t, router, http.MethodGet, "/elitea_core/skill_categories/prompt_lib/2", nil))
	categories, _ := body["categories"].([]any)
	if len(categories) != 9 {
		t.Fatalf("categories = %d entries, want 9", len(categories))
	}
	last, _ := categories[len(categories)-1].(map[string]any)
	if last["name"] != "Other" {
		t.Errorf("last category = %v, want the Other fallback last", last["name"])
	}
	// Every name the list offers must be one publish accepts, or the picker
	// offers choices the publish endpoint rejects.
	skillID, versionID := seedSkill(t, pool, userSchema, "pr-reviewer", goodDescription, goodInstructions, []string{"review"})
	for index, entry := range categories {
		name := entry.(map[string]any)["name"].(string)
		recorder := do(t, router, http.MethodPost,
			fmt.Sprintf("/elitea_core/publish_skill_validate/prompt_lib/%s/%d/%d", userProject, skillID, versionID),
			map[string]any{"version_name": fmt.Sprintf("v1.%d-check", index), "category": name})
		if recorder.Code != http.StatusOK {
			t.Errorf("validate with offered category %q = %d (body %s)", name, recorder.Code, recorder.Body.String())
		}
	}
}

// TestExportForkBuildsAForkPayload.
func TestExportForkBuildsAForkPayload(t *testing.T) {
	pool := newSkillPublishPool(t)
	router := newRouter(skillpublish.NewHandler(pool))
	skillID, versionID := seedSkill(t, pool, userSchema, "pr-reviewer", goodDescription, goodInstructions, []string{"review"})

	body := decode(t, do(t, router, http.MethodGet,
		fmt.Sprintf("/elitea_core/skill_export_fork/prompt_lib/%s/%d", userProject, skillID), nil))
	skills, _ := body["skills"].([]any)
	if len(skills) != 1 {
		t.Fatalf("payload carries %d skills, want 1", len(skills))
	}
	payload, _ := skills[0].(map[string]any)
	if payload["name"] != "pr-reviewer" || payload["entity"] != "skills" {
		t.Errorf("payload head = %v, want the skill name and entity=skills", payload)
	}
	if payload["import_uuid"] == "" || payload["import_uuid"] == nil {
		t.Error("payload carries no import_uuid, so import cannot deduplicate")
	}
	versions, _ := payload["versions"].([]any)
	version, _ := versions[0].(map[string]any)
	if version["name"] != "base" {
		t.Errorf("forked version name = %v, want base", version["name"])
	}
	if version["instructions"] != goodInstructions {
		t.Errorf("forked instructions = %v, want the source instructions", version["instructions"])
	}
	tags, _ := version["tags"].([]any)
	if len(tags) != 1 || tags[0].(map[string]any)["name"] != "review" {
		t.Errorf("forked tags = %v, want the source tag", tags)
	}

	// An explicit version id resolves that version; a missing one is a 404.
	byVersion := do(t, router, http.MethodGet,
		fmt.Sprintf("/elitea_core/skill_export_fork/prompt_lib/%s/%d/%d", userProject, skillID, versionID), nil)
	if byVersion.Code != http.StatusOK {
		t.Errorf("export_fork with an explicit version = %d, want 200", byVersion.Code)
	}
	missing := do(t, router, http.MethodGet,
		fmt.Sprintf("/elitea_core/skill_export_fork/prompt_lib/%s/%d/424242", userProject, skillID), nil)
	if missing.Code != http.StatusNotFound {
		t.Errorf("export_fork with an unknown version = %d, want 404", missing.Code)
	}
}

// TestAdminPublishInThePublicProjectPublishesInPlace — the second of the two
// publish paths, and the one where source and catalog are the same schema.
func TestAdminPublishInThePublicProjectPublishesInPlace(t *testing.T) {
	pool := newSkillPublishPool(t)
	router := newRouter(skillpublish.NewHandler(pool))
	skillID, versionID := seedSkill(t, pool, publicSchema, "curated", goodDescription, goodInstructions, []string{"review"})

	published := do(t, router, http.MethodPost,
		fmt.Sprintf("/elitea_core/publish_skill/prompt_lib/%s/%d/%d", publicProject, skillID, versionID),
		map[string]any{"version_name": "v1.0-curated", "category": "Elitea"})
	if published.Code != http.StatusOK {
		t.Fatalf("admin publish status = %d, want 200 (body %s)", published.Code, published.Body.String())
	}
	body := decode(t, published)
	if intField(t, body, "public_skill_id") != skillID {
		t.Errorf("admin publish created a twin (%v) instead of publishing skill %d in place", body["public_skill_id"], skillID)
	}
	publicVersionID := intField(t, body, "public_version_id")
	if got := versionStatus(t, pool, publicSchema, publicVersionID); got != "published" {
		t.Errorf("in-place published version status = %q, want published", got)
	}

	listing := decode(t, do(t, router, http.MethodGet, "/elitea_core/public_skills/prompt_lib", nil))
	if total, _ := listing["total"].(float64); total != 1 {
		t.Errorf("public listing total after an admin publish = %v, want 1", listing["total"])
	}

	unpublished := do(t, router, http.MethodPost,
		fmt.Sprintf("/elitea_core/unpublish_skill/prompt_lib/%s/%d/%d", publicProject, skillID, publicVersionID), nil)
	if unpublished.Code != http.StatusOK {
		t.Fatalf("admin unpublish status = %d, want 200 (body %s)", unpublished.Code, unpublished.Body.String())
	}
	if count := countRows(t, pool, `SELECT COUNT(*) FROM p_1.skill_versions WHERE id = $1`, publicVersionID); count != 0 {
		t.Errorf("published version rows after admin unpublish = %d, want 0", count)
	}
	// The in-place original keeps its draft: deleting the author's own work
	// is what the shell-deletion branch must NOT do here.
	if count := countRows(t, pool, `SELECT COUNT(*) FROM p_1.skills WHERE id = $1`, skillID); count != 1 {
		t.Errorf("in-place skill rows after admin unpublish = %d, want 1 (drafts survive)", count)
	}
	if count := countRows(t, pool, `SELECT COUNT(*) FROM p_1.skill_versions WHERE id = $1`, versionID); count != 1 {
		t.Errorf("original draft version rows = %d, want 1", count)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func setFlag(t *testing.T, pool *pgxpool.Pool, section, key string, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %s.%s: %v", section, key, err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO centry.platform_config (section, key, value)
		VALUES ($1, $2, $3::jsonb)
		ON CONFLICT (section, key) DO UPDATE SET value = EXCLUDED.value`,
		section, key, string(encoded)); err != nil {
		t.Fatalf("set %s.%s: %v", section, key, err)
	}
}

// newSkillPublishPool creates an isolated database, applies the REAL bootstrap
// migration and the tenant migration this unit added, and creates the source
// project's schema. Going through 001_initial.sql rather than a hand-written
// DDL copy is what makes the publish columns this unit needs real for a fresh
// deployment too.
func newSkillPublishPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	t.Setenv("PUBLIC_PROJECT_ID", publicProject)

	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_skillpub_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		if _, dropErr := adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); dropErr != nil {
			t.Errorf("drop database after pool open failure: %v", dropErr)
		}
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})

	initial, err := os.ReadFile(filepath.Join("..", "..", "..", "infra", "db", "migrations", "001_initial.sql"))
	if err != nil {
		t.Fatalf("read 001_initial.sql: %v", err)
	}
	if _, err := pool.Exec(ctx, string(initial)); err != nil {
		t.Fatalf("apply 001_initial.sql: %v", err)
	}
	if _, err := pool.Exec(ctx, `SELECT create_tenant_schema($1)`, userSchema); err != nil {
		t.Fatalf("create source project schema: %v", err)
	}
	return pool
}

// TestTenantMigrationMatchesTheBootstrapSchema pins the pair that would
// otherwise drift silently: a fresh deployment gets its skill-publishing
// columns from 001_initial.sql and a migrated one gets them from
// migrations/tenant/0122. A column present in only one of the two produces a
// platform where publishing works on new installs and 500s on upgraded ones.
func TestTenantMigrationMatchesTheBootstrapSchema(t *testing.T) {
	pool := newSkillPublishPool(t)
	ctx := context.Background()

	// p_2 is bootstrap-created; strip the new columns off it and let the
	// tenant migration put them back, then compare against p_1.
	for _, statement := range []string{
		`DROP INDEX p_2.uq_skills_shared_owner`,
		`ALTER TABLE p_2.skills DROP COLUMN shared_owner_id`,
		`ALTER TABLE p_2.skills DROP COLUMN shared_id`,
		`ALTER TABLE p_2.skill_versions DROP COLUMN status`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}

	migration, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "migrations", "tenant", "0122_skill_publishing.sql"))
	if err != nil {
		t.Fatalf("read 0122_skill_publishing.sql: %v", err)
	}
	if _, err := pool.Exec(ctx, `SET search_path TO p_2`); err != nil {
		t.Fatalf("set search_path: %v", err)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		t.Fatalf("apply 0122_skill_publishing.sql: %v", err)
	}
	if _, err := pool.Exec(ctx, `SET search_path TO public`); err != nil {
		t.Fatalf("reset search_path: %v", err)
	}

	for _, check := range []struct {
		table, column string
	}{
		{"skills", "shared_owner_id"},
		{"skills", "shared_id"},
		{"skill_versions", "status"},
	} {
		var dataType, defaultValue string
		if err := pool.QueryRow(ctx, `
			SELECT data_type, COALESCE(column_default, '')
			FROM information_schema.columns
			WHERE table_schema = 'p_2' AND table_name = $1 AND column_name = $2`,
			check.table, check.column).Scan(&dataType, &defaultValue); err != nil {
			t.Fatalf("migrated p_2.%s.%s is missing: %v", check.table, check.column, err)
		}
		var bootstrapType, bootstrapDefault string
		if err := pool.QueryRow(ctx, `
			SELECT data_type, COALESCE(column_default, '')
			FROM information_schema.columns
			WHERE table_schema = 'p_1' AND table_name = $1 AND column_name = $2`,
			check.table, check.column).Scan(&bootstrapType, &bootstrapDefault); err != nil {
			t.Fatalf("bootstrap p_1.%s.%s is missing: %v", check.table, check.column, err)
		}
		if dataType != bootstrapType || defaultValue != bootstrapDefault {
			t.Errorf("%s.%s: migrated (%s, %q) != bootstrap (%s, %q)",
				check.table, check.column, dataType, defaultValue, bootstrapType, bootstrapDefault)
		}
	}

	var indexCount int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM pg_indexes
		WHERE schemaname = 'p_2' AND indexname = 'uq_skills_shared_owner'`).Scan(&indexCount); err != nil {
		t.Fatalf("read index: %v", err)
	}
	if indexCount != 1 {
		t.Error("the migrated schema has no uq_skills_shared_owner index, so concurrent first publishes could double the catalog entry")
	}
}
