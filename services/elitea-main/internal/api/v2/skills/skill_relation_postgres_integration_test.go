package skills_test

// End-to-end tests for the skill-to-agent attachment (#38), against a real
// PostgreSQL server.
//
// `p_<project>.entity_skill_mapping` has existed since 001_initial.sql:422-431
// and NO Go code wrote a row into it from a request. The relation form of
// PATCH /skill/{mode}/{projectID}/{skillID} decoded into `createRequest`, which
// names none of the four relation keys, so every attach and every detach
// answered 200 and changed nothing. A test that stops at the status code passes
// against that code unchanged.
//
// So every assertion below reads the STORED ROW, and each one also reads the
// row back out through a real consumer:
//
//   - GET /application_skills/{mode}/{projectID}/{appVersionID}, which the web
//     app's skill-mention dropdowns call today
//     (apps/elitea-web/src/features/agents/lib/hooks/useInstructionsSkillMention.hooks.ts
//     and src/features/chat-input/lib/hooks/useChatSkillMention.ts);
//   - the body-less PATCH on /version/prompt_lib/..., whose `attached_skills`
//     key is the registry the SDK binds `load_skill` against.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	v2applications "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/applications"
	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

const (
	relationDatabaseURLVariable = "ELITEA_TEST_DATABASE_URL"
	relationProjectID           = "1"
	// relationSecretHeader is the value this fixture seals into the project
	// vault under `secrets_header_value`.
	//
	// It used to be the literal "secret", which the version-details route
	// accepted on any project whose vault held no value. That fallback is gone
	// (#408): the route now refuses such a project with 403, so the fixture
	// must give the project the value provisioning gives a real one.
	relationSecretHeader = "relation-fixture-header-value"
)

// relationFixture is one project schema that holds two agent versions, one
// published version and three skills, reachable through the write route and
// through both read routes.
type relationFixture struct {
	pool *pgxpool.Pool
	// router mounts the three real handlers, with the same patterns
	// internal/api/router.go registers.
	router *chi.Mux

	applicationID    int64
	versionID        int64
	otherVersionID   int64
	publishedVersion int64

	// skills maps a skill name to its id and its base version id.
	skills map[string]seededSkill
}

type seededSkill struct {
	id        int64
	versionID int64
}

func newRelationFixture(t *testing.T) *relationFixture {
	t.Helper()
	pool := newRelationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)

	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run baseline migrations: %v", err)
	}
	// The REAL ledgered corpus on top of the bootstrap schema, not a
	// hand-written subset. A fixture that builds entity_skill_mapping itself
	// can make entity_type nullable and stop reproducing production.
	runner := migrate.New(pool, platformmigrations.Files)
	if err := runner.ApplyShared(ctx); err != nil {
		t.Fatalf("apply shared migrations: %v", err)
	}
	if err := runner.ApplyTenant(ctx, 1); err != nil {
		t.Fatalf("apply tenant migrations to p_1: %v", err)
	}

	fixture := &relationFixture{pool: pool, skills: map[string]seededSkill{}}

	// A decoy application, so the real one does not take id 1 and collide with
	// its own version id. entity_version_id is a VERSION id, and a test that
	// cannot tell the two apart cannot catch a writer that confuses them.
	if _, err := pool.Exec(ctx, `
		INSERT INTO p_1.applications (name, description, owner_id) VALUES ('relation-decoy', '', 1)`); err != nil {
		t.Fatalf("insert decoy application: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.applications (name, description, owner_id)
		VALUES ('relation-fixture', '', 1) RETURNING id`).Scan(&fixture.applicationID); err != nil {
		t.Fatalf("insert fixture application: %v", err)
	}

	insertVersion := func(name, status string) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO p_1.application_versions
				(application_id, name, status, author_id, agent_type, instructions)
			VALUES ($1, $2, $3, 1, 'openai', 'Use the attached skills.')
			RETURNING id`, fixture.applicationID, name, status).Scan(&id); err != nil {
			t.Fatalf("insert version %q: %v", name, err)
		}
		return id
	}
	fixture.versionID = insertVersion("base", "draft")
	fixture.otherVersionID = insertVersion("other", "draft")
	fixture.publishedVersion = insertVersion("released", "published")

	for _, name := range []string{"Reviewer", "Summarizer", "Translator"} {
		fixture.seedSkill(t, name, name+" instructions")
	}

	// The X-SECRET value the version-details read below compares against.
	if err := v2secrets.NewHandler(pool).StoreSecret(
		ctx, nil, relationProjectID, "secrets_header_value", relationSecretHeader,
	); err != nil {
		t.Fatalf("store the project secrets_header_value: %v", err)
	}

	appHandler := v2applications.NewHandler(nil, pool)
	skillHandler := handler.NewHandler(repos.NewSkillsRepo(pool))

	router := chi.NewRouter()
	router.Patch("/elitea_core/skill/{mode}/{projectID}/{skillID}", skillHandler.Update)
	router.Get("/elitea_core/application_skills/{mode}/{projectID}/{appVersionID}", skillHandler.ListForApplication)
	router.Patch("/elitea_core/version/prompt_lib/{projectID}/{applicationID}/{versionID}", appHandler.GetVersionExpanded)
	fixture.router = router

	return fixture
}

func (f *relationFixture) seedSkill(t *testing.T, name, instructions string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var seeded seededSkill
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO p_1.skills (name, description, owner_id, author_id)
		VALUES ($1, $2, 1, 1) RETURNING id`, name, name+" description").Scan(&seeded.id); err != nil {
		t.Fatalf("insert skill %q: %v", name, err)
	}
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO p_1.skill_versions (skill_id, name, instructions, author_id)
		VALUES ($1, 'base', $2, 1) RETURNING id`, seeded.id, instructions).Scan(&seeded.versionID); err != nil {
		t.Fatalf("insert skill version for %q: %v", name, err)
	}
	f.skills[name] = seeded
}

// patch issues the relation request on the real route.
func (f *relationFixture) patch(t *testing.T, skillID int64, body string) *httptest.ResponseRecorder {
	t.Helper()
	path := fmt.Sprintf("/elitea_core/skill/prompt_lib/%s/%d", relationProjectID, skillID)
	request := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, request)
	return recorder
}

func (f *relationFixture) attach(t *testing.T, skillName string, versionID int64) *httptest.ResponseRecorder {
	t.Helper()
	skill := f.skills[skillName]
	return f.patch(t, skill.id, fmt.Sprintf(
		`{"has_relation": true, "entity_version_id": %d, "skill_version_id": %d, "entity_type": "agent"}`,
		versionID, skill.versionID))
}

func (f *relationFixture) detach(t *testing.T, skillName string, versionID int64) *httptest.ResponseRecorder {
	t.Helper()
	return f.patch(t, f.skills[skillName].id, fmt.Sprintf(
		`{"has_relation": false, "entity_version_id": %d}`, versionID))
}

// storedRows reads entity_skill_mapping back through the predicate BOTH readers
// use: the version id AND entity_type = 'agent'. A row written with any other
// entity_type is invisible to them, so selecting on the version alone would
// call a broken write correct.
func (f *relationFixture) storedRows(t *testing.T, versionID int64) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := f.pool.Query(ctx, `
		SELECT skill.name, COALESCE(version.instructions, '')
		FROM p_1.entity_skill_mapping AS mapping
		JOIN p_1.skills AS skill ON skill.id = mapping.skill_id
		LEFT JOIN p_1.skill_versions AS version ON version.id = mapping.skill_version_id
		WHERE mapping.entity_version_id = $1 AND mapping.entity_type = 'agent'
		ORDER BY skill.name`, versionID)
	if err != nil {
		t.Fatalf("read the stored mappings: %v", err)
	}
	defer rows.Close()

	stored := []string{}
	for rows.Next() {
		var name, instructions string
		if err := rows.Scan(&name, &instructions); err != nil {
			t.Fatalf("scan a stored mapping: %v", err)
		}
		stored = append(stored, name+"="+instructions)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the stored mappings: %v", err)
	}
	return stored
}

// countAllRows counts every mapping row for the version, WITHOUT the
// entity_type predicate, so a duplicate written under a second entity_type
// still shows up.
func (f *relationFixture) countAllRows(t *testing.T, versionID int64) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var total int
	if err := f.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM p_1.entity_skill_mapping WHERE entity_version_id = $1`,
		versionID).Scan(&total); err != nil {
		t.Fatalf("count the stored mappings: %v", err)
	}
	return total
}

// listedSkills reads the attachment back through GET /application_skills, the
// route the web app's skill-mention dropdowns call.
func (f *relationFixture) listedSkills(t *testing.T, versionID int64) []string {
	t.Helper()
	path := fmt.Sprintf("/elitea_core/application_skills/prompt_lib/%s/%d", relationProjectID, versionID)
	request := httptest.NewRequest(http.MethodGet, path, nil)
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("application_skills answered %d: %s", recorder.Code, recorder.Body.String())
	}

	var listed handler.ListResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode the application_skills body %q: %v", recorder.Body.String(), err)
	}
	names := []string{}
	for _, item := range listed.Items {
		names = append(names, item.Name)
	}
	sort.Strings(names)
	return names
}

// runtimeSkills reads the attachment back through the `attached_skills` key of
// the body-less version PATCH — the registry the SDK binds `load_skill`
// against. Each entry is "name=instructions", because a copy that loses
// skill_version_id still returns the name and an EMPTY body.
func (f *relationFixture) runtimeSkills(t *testing.T, versionID int64) []string {
	t.Helper()
	path := fmt.Sprintf("/elitea_core/version/prompt_lib/%s/%d/%d",
		relationProjectID, f.applicationID, versionID)
	request := httptest.NewRequest(http.MethodPatch, path, nil)
	request.Header.Set("X-SECRET", relationSecretHeader)
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("the version PATCH answered %d: %s", recorder.Code, recorder.Body.String())
	}

	var body struct {
		AttachedSkills []struct {
			Name         string `json:"name"`
			Instructions string `json:"instructions"`
		} `json:"attached_skills"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the version body %q: %v", recorder.Body.String(), err)
	}
	entries := []string{}
	for _, skill := range body.AttachedSkills {
		entries = append(entries, skill.Name+"="+skill.Instructions)
	}
	sort.Strings(entries)
	return entries
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// ── Attach ───────────────────────────────────────────────────────────────────

// TestAttachWritesTheRowAndBothReadersSeeIt is the acceptance test for #38.
//
// It proves the whole chain, and no assertion is a status code alone:
// the request writes the row, the row carries the correct entity_type and
// skill_version_id, and both consumers of that row then answer with the skill.
// Revert the handler and the row count is 0 here.
func TestAttachWritesTheRowAndBothReadersSeeIt(t *testing.T) {
	fixture := newRelationFixture(t)

	recorder := fixture.attach(t, "Reviewer", fixture.versionID)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("attach answered %d, want 201: %s", recorder.Code, recorder.Body.String())
	}

	// The response is pylon's four-key attachment, and its values come from
	// the rows, not from an echo of the request.
	var attachment handler.SkillAttachment
	if err := json.Unmarshal(recorder.Body.Bytes(), &attachment); err != nil {
		t.Fatalf("decode the attach body %q: %v", recorder.Body.String(), err)
	}
	want := handler.SkillAttachment{
		SkillID:        int(fixture.skills["Reviewer"].id),
		SkillVersionID: int(fixture.skills["Reviewer"].versionID),
		SkillName:      "Reviewer",
		VersionName:    "base",
	}
	if attachment != want {
		t.Errorf("attach body = %+v, want %+v", attachment, want)
	}

	// The stored row. `entity_type` and `skill_version_id` are read through
	// the reader's own predicate and join, so one select proves all three.
	if stored := fixture.storedRows(t, fixture.versionID); !equalStrings(stored, []string{"Reviewer=Reviewer instructions"}) {
		t.Fatalf("stored rows = %v, want [Reviewer=Reviewer instructions]", stored)
	}

	// Reader 1: the route the web app's skill-mention dropdowns call.
	if listed := fixture.listedSkills(t, fixture.versionID); !equalStrings(listed, []string{"Reviewer"}) {
		t.Errorf("application_skills = %v, want [Reviewer]", listed)
	}
	// Reader 2: the registry the SDK binds load_skill against. A row with no
	// skill_version_id returns "Reviewer=" here, and the registry then drops
	// it, so this assertion discriminates on the column.
	if runtime := fixture.runtimeSkills(t, fixture.versionID); !equalStrings(runtime, []string{"Reviewer=Reviewer instructions"}) {
		t.Errorf("attached_skills = %v, want [Reviewer=Reviewer instructions]", runtime)
	}

	// The attachment belongs to ONE version. A writer that ignored
	// entity_version_id would put it on both.
	if stored := fixture.storedRows(t, fixture.otherVersionID); len(stored) != 0 {
		t.Errorf("the other version carries %v, want nothing", stored)
	}
	if listed := fixture.listedSkills(t, fixture.otherVersionID); len(listed) != 0 {
		t.Errorf("the other version lists %v, want nothing", listed)
	}
}

// ── Detach ───────────────────────────────────────────────────────────────────

// TestDetachRemovesOnlyTheNamedRow proves the other direction, and proves it
// removes one row rather than the version's whole set.
func TestDetachRemovesOnlyTheNamedRow(t *testing.T) {
	fixture := newRelationFixture(t)

	for _, name := range []string{"Reviewer", "Summarizer"} {
		if recorder := fixture.attach(t, name, fixture.versionID); recorder.Code != http.StatusCreated {
			t.Fatalf("attach %s answered %d: %s", name, recorder.Code, recorder.Body.String())
		}
	}
	if stored := fixture.storedRows(t, fixture.versionID); len(stored) != 2 {
		t.Fatalf("stored rows before the detach = %v, want 2", stored)
	}

	recorder := fixture.detach(t, "Reviewer", fixture.versionID)
	if recorder.Code != http.StatusOK {
		t.Fatalf("detach answered %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if strings.TrimSpace(recorder.Body.String()) != `{"ok":true}` {
		t.Errorf("detach body = %s, want {\"ok\":true}", recorder.Body.String())
	}

	if stored := fixture.storedRows(t, fixture.versionID); !equalStrings(stored, []string{"Summarizer=Summarizer instructions"}) {
		t.Errorf("stored rows after the detach = %v, want [Summarizer=Summarizer instructions]", stored)
	}
	if listed := fixture.listedSkills(t, fixture.versionID); !equalStrings(listed, []string{"Summarizer"}) {
		t.Errorf("application_skills after the detach = %v, want [Summarizer]", listed)
	}
	if runtime := fixture.runtimeSkills(t, fixture.versionID); !equalStrings(runtime, []string{"Summarizer=Summarizer instructions"}) {
		t.Errorf("attached_skills after the detach = %v, want [Summarizer=Summarizer instructions]", runtime)
	}
}

// TestDetachOfAnUnattachedSkillIsRefused holds pylon's SkillNotAttachedError.
//
// The old app's version selector changes a version by detaching and then
// re-attaching, and it re-attaches ONLY when the detach reports success
// (apps/elitea-ui/src/[fsd]/features/skill/ui/SkillVersionSelector.jsx:54-65).
// A false success there would drop the skill.
func TestDetachOfAnUnattachedSkillIsRefused(t *testing.T) {
	fixture := newRelationFixture(t)

	recorder := fixture.detach(t, "Reviewer", fixture.versionID)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("detach of nothing answered %d, want 404: %s", recorder.Code, recorder.Body.String())
	}

	if recorder := fixture.attach(t, "Reviewer", fixture.versionID); recorder.Code != http.StatusCreated {
		t.Fatalf("attach answered %d: %s", recorder.Code, recorder.Body.String())
	}
	if recorder := fixture.detach(t, "Reviewer", fixture.versionID); recorder.Code != http.StatusOK {
		t.Fatalf("first detach answered %d: %s", recorder.Code, recorder.Body.String())
	}
	if recorder := fixture.detach(t, "Reviewer", fixture.versionID); recorder.Code != http.StatusNotFound {
		t.Fatalf("second detach answered %d, want 404: %s", recorder.Code, recorder.Body.String())
	}
	if total := fixture.countAllRows(t, fixture.versionID); total != 0 {
		t.Errorf("the version still carries %d mapping rows", total)
	}
}

// ── Refusals, each one asserted against the stored rows ──────────────────────

// TestDuplicateAttachIsRefusedAndWritesNoSecondRow holds pylon's
// SkillAlreadyAttachedError, which the old app depends on.
func TestDuplicateAttachIsRefusedAndWritesNoSecondRow(t *testing.T) {
	fixture := newRelationFixture(t)

	if recorder := fixture.attach(t, "Reviewer", fixture.versionID); recorder.Code != http.StatusCreated {
		t.Fatalf("first attach answered %d: %s", recorder.Code, recorder.Body.String())
	}

	recorder := fixture.attach(t, "Reviewer", fixture.versionID)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("duplicate attach answered %d, want 409: %s", recorder.Code, recorder.Body.String())
	}
	// The unique constraint must not reach the caller as a raw 500.
	if strings.Contains(recorder.Body.String(), "_entity_skill_unique") {
		t.Errorf("the constraint violation reached the caller: %s", recorder.Body.String())
	}
	if total := fixture.countAllRows(t, fixture.versionID); total != 1 {
		t.Errorf("the version carries %d mapping rows, want 1", total)
	}
	if listed := fixture.listedSkills(t, fixture.versionID); !equalStrings(listed, []string{"Reviewer"}) {
		t.Errorf("application_skills = %v, want [Reviewer]", listed)
	}
}

// TestAttachRefusesASkillVersionOfAnotherSkill guards the pair, not the parts.
//
// The foreign key says the skill version exists. It does not say whose it is.
// Both readers take the NAME from skill_id and the INSTRUCTIONS from
// skill_version_id, so an unchecked pair serves one skill's body under another
// skill's name.
func TestAttachRefusesASkillVersionOfAnotherSkill(t *testing.T) {
	fixture := newRelationFixture(t)

	recorder := fixture.patch(t, fixture.skills["Reviewer"].id, fmt.Sprintf(
		`{"has_relation": true, "entity_version_id": %d, "skill_version_id": %d}`,
		fixture.versionID, fixture.skills["Translator"].versionID))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("the mismatched pair answered %d, want 404: %s", recorder.Code, recorder.Body.String())
	}
	if total := fixture.countAllRows(t, fixture.versionID); total != 0 {
		t.Errorf("a refused attach still wrote %d rows", total)
	}
}

// TestAttachRefusesAPublishedVersion holds the guard that keeps a published
// version and its copies in agreement. Publish copies the skill rows into the
// public version (#405) and into every embedded sub-agent (#414); a later
// change to the source would make the copies disagree with no way to tell
// which one a consumer holds.
func TestAttachRefusesAPublishedVersion(t *testing.T) {
	fixture := newRelationFixture(t)

	recorder := fixture.attach(t, "Reviewer", fixture.publishedVersion)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("attach to a published version answered %d, want 409: %s", recorder.Code, recorder.Body.String())
	}
	if total := fixture.countAllRows(t, fixture.publishedVersion); total != 0 {
		t.Errorf("a refused attach still wrote %d rows", total)
	}

	// The detach direction takes the same guard. Seed the row directly,
	// because the attach above is refused.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO p_1.entity_skill_mapping (entity_version_id, entity_type, skill_id, skill_version_id)
		VALUES ($1, 'agent', $2, $3)`,
		fixture.publishedVersion, fixture.skills["Reviewer"].id, fixture.skills["Reviewer"].versionID); err != nil {
		t.Fatalf("seed the published mapping: %v", err)
	}
	if recorder := fixture.detach(t, "Reviewer", fixture.publishedVersion); recorder.Code != http.StatusConflict {
		t.Fatalf("detach from a published version answered %d, want 409: %s", recorder.Code, recorder.Body.String())
	}
	if total := fixture.countAllRows(t, fixture.publishedVersion); total != 1 {
		t.Errorf("a refused detach removed the row")
	}
}

// TestAttachRefusesAnUnknownAgentVersion is the one departure from pylon, and
// this test is the record of it.
//
// Pylon writes `if agent_version and ...`, so an unknown version id falls
// through its guard and the attach answers 201. entity_version_id carries no
// foreign key, so that row is an orphan no reader can reach and no cascade
// removes. Attach refuses it here.
func TestAttachRefusesAnUnknownAgentVersion(t *testing.T) {
	fixture := newRelationFixture(t)

	const unknownVersion = 987654
	skill := fixture.skills["Reviewer"]
	recorder := fixture.patch(t, skill.id, fmt.Sprintf(
		`{"has_relation": true, "entity_version_id": %d, "skill_version_id": %d}`,
		unknownVersion, skill.versionID))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("attach to an unknown version answered %d, want 404: %s", recorder.Code, recorder.Body.String())
	}
	if total := fixture.countAllRows(t, unknownVersion); total != 0 {
		t.Errorf("a refused attach still wrote %d orphan rows", total)
	}

	// Detach keeps pylon's fall-through, so an orphan an earlier write left
	// behind can still be removed.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO p_1.entity_skill_mapping (entity_version_id, entity_type, skill_id, skill_version_id)
		VALUES ($1, 'agent', $2, $3)`, unknownVersion, skill.id, skill.versionID); err != nil {
		t.Fatalf("seed the orphan mapping: %v", err)
	}
	if recorder := fixture.patch(t, skill.id, fmt.Sprintf(
		`{"has_relation": false, "entity_version_id": %d}`, unknownVersion)); recorder.Code != http.StatusOK {
		t.Fatalf("detach of an orphan answered %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if total := fixture.countAllRows(t, unknownVersion); total != 0 {
		t.Errorf("the orphan survived the detach")
	}
}

// TestAttachRefusesAnUnknownSkill answers 404 rather than letting the foreign
// key surface as a 500.
func TestAttachRefusesAnUnknownSkill(t *testing.T) {
	fixture := newRelationFixture(t)

	recorder := fixture.patch(t, 987654, fmt.Sprintf(
		`{"has_relation": true, "entity_version_id": %d, "skill_version_id": %d}`,
		fixture.versionID, fixture.skills["Reviewer"].versionID))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("attach of an unknown skill answered %d, want 404: %s", recorder.Code, recorder.Body.String())
	}
	if total := fixture.countAllRows(t, fixture.versionID); total != 0 {
		t.Errorf("a refused attach still wrote %d rows", total)
	}
}

// TestAttachHoldsTheSkillLimit keeps the write side and the read side in
// agreement. The old picker renders "n/max skills added" from the same number
// and disables itself at the cap; without the cap on the write side that
// counter can read 6/5.
func TestAttachHoldsTheSkillLimit(t *testing.T) {
	fixture := newRelationFixture(t)

	for index := 0; index < handler.MaxSkillsPerEntityVersion; index++ {
		name := fmt.Sprintf("Extra %d", index)
		fixture.seedSkill(t, name, name+" instructions")
		if recorder := fixture.attach(t, name, fixture.versionID); recorder.Code != http.StatusCreated {
			t.Fatalf("attach %s answered %d: %s", name, recorder.Code, recorder.Body.String())
		}
	}
	if total := fixture.countAllRows(t, fixture.versionID); total != handler.MaxSkillsPerEntityVersion {
		t.Fatalf("the version carries %d rows, want %d", total, handler.MaxSkillsPerEntityVersion)
	}

	recorder := fixture.attach(t, "Reviewer", fixture.versionID)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("the attach over the limit answered %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
	if total := fixture.countAllRows(t, fixture.versionID); total != handler.MaxSkillsPerEntityVersion {
		t.Errorf("the refused attach still wrote a row: %d rows", total)
	}
}

// TestPlainUpdateStillWorksOnTheSameURL proves the overload did not take the
// URL away from the operation that already used it. It reads the skill back
// out of the database, not off the response.
func TestPlainUpdateStillWorksOnTheSameURL(t *testing.T) {
	fixture := newRelationFixture(t)

	recorder := fixture.patch(t, fixture.skills["Reviewer"].id,
		`{"name": "Renamed", "description": "New description", "instructions": "New body"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("the plain update answered %d, want 200: %s", recorder.Code, recorder.Body.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var name, instructions string
	if err := fixture.pool.QueryRow(ctx, `
		SELECT skill.name, COALESCE(version.instructions, '')
		FROM p_1.skills AS skill
		LEFT JOIN p_1.skill_versions AS version
			ON version.skill_id = skill.id AND version.name = 'base'
		WHERE skill.id = $1`, fixture.skills["Reviewer"].id).Scan(&name, &instructions); err != nil {
		t.Fatalf("read the updated skill: %v", err)
	}
	if name != "Renamed" || instructions != "New body" {
		t.Errorf("stored skill = %q/%q, want \"Renamed\"/\"New body\"", name, instructions)
	}
}

// newRelationPool creates one isolated database per test, on the pattern every
// other *_postgres_integration_test.go in this repository uses.
func newRelationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv(relationDatabaseURLVariable)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", relationDatabaseURLVariable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", relationDatabaseURLVariable, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open the PostgreSQL admin pool: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_skill_rel_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create the isolated integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 6
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("open the isolated integration database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("ping the isolated integration database: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop the isolated integration database: %v", err)
		}
		adminPool.Close()
	})

	return pool
}
