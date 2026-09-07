package eliteacore_test

// Real-PostgreSQL coverage for the skill attachments of the EXPORT, the IMPORT
// and the FORK — issue #611.
//
// Five paths copy an agent version. Publish and the embed under it carried the
// rows of `entity_skill_mapping`, and each has its own test
// (publish_tool_copy_postgres_integration_test.go,
// embed_skill_copy_postgres_integration_test.go). These three carried nothing,
// and no test could see it: the round-trip file
// (export_import_roundtrip_postgres_integration_test.go) has no
// case-insensitive match for "skill" at all, so it asserted the tool selection
// survives and said nothing about the skills.
//
// No case here asserts on a status code alone. A 201 from the import is exactly
// what the defect produced. Every case reads `entity_skill_mapping` back out of
// the destination, through the joins and the predicate the chat read uses
// (internal/db/queries/agent_chat.sql:126-132), so a row that carries the wrong
// `entity_type` or no `skill_version_id` fails the assertion rather than
// passing as "one row, close enough".
//
// The pool, the migration corpus and the one recorded schema adjustment come
// from import_tool_link_postgres_integration_test.go — see its header.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// skillRoundTripSeed is the agent the export reads. It has TWO versions, and
// that is the whole point of it.
//
// A skill's version is named, not numbered, and the export ships only the
// versions the attachments name. One agent version can hold at most one
// attachment per skill (`_entity_skill_unique`), so a document can only carry
// TWO versions of one skill when TWO agent versions pin different ones. With a
// single-version agent every skill in the document has exactly one version, and
// then every branch of `importedSkill.versionID` — the named version, `base`,
// the first version — lands on the same row and no assertion can tell a correct
// resolver from a broken one. The seed below pins `earlier` to the skill's
// `base` and `latest` to its `reviewed`, with different instructions in each, so
// resolving to the wrong version changes an assertion.
//
// The two versions also pin the alignment of `info.skills[vIdx]` against
// `createdVersions[vIdx]` in the import's phase 3. An index shift there attaches
// every skill to the wrong agent version, and no single-version agent can see it.
type skillRoundTripSeed struct {
	applicationID int
	// latestVersionID pins skill one at `reviewed` and skill two at NULL.
	latestVersionID int
	// earlierVersionID pins skill one at `base`.
	earlierVersionID int
	pinnedSkillID    int
	looseSkillID     int
}

const (
	skillRoundTripPinnedName = "round trip skill one"
	skillRoundTripLooseName  = "round trip skill two"
	skillRoundTripPinnedBody = "follow the steps"
	skillRoundTripBaseBody   = "the base body of skill one"
	skillRoundTripLooseBody  = "read the manual"
	// A version of skill one that NO attachment names. It must never leave the
	// source project: the export ships only the versions the attachments name,
	// so a regression that shipped a skill's whole history would carry it.
	skillRoundTripUnusedVName = "unused"
	skillRoundTripUnusedBody  = "the body no attachment names"
	skillRoundTripPinnedVName = "reviewed"
	skillRoundTripTagName     = "round trip skill tag"
)

// attachedSkill is one row of the destination's `entity_skill_mapping`, read
// through the chat read's own projection.
type attachedSkill struct {
	skillName    string
	versionName  string
	instructions string
	entityType   string
}

/* ── the acceptance test ─────────────────────────────────────────────────── */

// TestExportImportRoundTripKeepsTheSkillAttachments is the case the issue asks
// for. It exports an agent that HAS skills attached, imports the document the
// way the wizard does, and reads the attachment ROWS back out of the database.
//
// Before the repair the export wrote no skills key at all, the import read
// none, and the imported agent came back with its instructions, its toolkits,
// its variables, its tags — and no skills, at 201, with nothing said anywhere.
func TestExportImportRoundTripKeepsTheSkillAttachments(t *testing.T) {
	pool := newImportLinkPool(t)
	handler := eliteacore.NewHandler(pool)

	seeded := seedSkillRoundTripAgent(t, pool)
	document := exportRoundTrip(t, handler, seeded.applicationID, http.StatusOK)

	// The document must carry the attachments before the import can be blamed
	// for losing them.
	assertExportedSkillDocument(t, document)

	recorder := importLinkDo(t, importLinkRouter(handler), skillRoundTripImportBody(t, document))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	if errors := decodeSkillErrors(t, recorder); len(errors) != 0 {
		t.Fatalf("the round trip reported skill errors: %s", recorder.Body.String())
	}

	// The rows the IMPORT wrote, not the rows the export read from. The version
	// filter excludes the seeded versions, so a query that found only the source
	// would report no rows rather than pass.
	imported := readAttachedSkillsExcept(t, pool, seeded.earlierVersionID, seeded.latestVersionID)
	// Ordered by mapping id, which is the order the import writes them: the
	// document's versions in order, and each version's references in order.
	want := []attachedSkill{
		{
			// `earlier` pinned `base`. Resolving this one to `reviewed` would
			// carry "follow the steps" here.
			skillName: skillRoundTripPinnedName, versionName: "base",
			instructions: skillRoundTripBaseBody, entityType: "agent",
		},
		{
			// `latest` pinned `reviewed`, and the skill's `base` is in the
			// document beside it with a different body. A resolver that reached
			// for `base` first — pylon's fallback order reversed — carries
			// "the base body of skill one" here and fails.
			skillName: skillRoundTripPinnedName, versionName: skillRoundTripPinnedVName,
			instructions: skillRoundTripPinnedBody, entityType: "agent",
		},
		{
			// The source row named no version. The reference then names none
			// either, and the import resolves it to the skill's base version —
			// the rule `_resolve_and_attach_skill` applies
			// (rpc/import_wizzard.py:245-252). An attachment with no version at
			// all renders as a named skill with an empty body.
			skillName: skillRoundTripLooseName, versionName: "base",
			instructions: skillRoundTripLooseBody, entityType: "agent",
		},
	}
	assertAttachedSkills(t, imported, want)

	// Each attachment landed on the RIGHT agent version. The assertion above
	// reads every imported row at once, so on its own it would pass if phase 3
	// attached the whole set to one version, or swapped the two.
	assertAttachedSkillsOfImportedVersion(t, pool, seeded, "earlier", []attachedSkill{want[0]})
	assertAttachedSkillsOfImportedVersion(t, pool, seeded, "latest", []attachedSkill{want[1], want[2]})

	// The imported skills are NEW rows in this project, not the source rows.
	for _, sourceID := range []int{seeded.pinnedSkillID, seeded.looseSkillID} {
		if importLinkCount(t, pool, fmt.Sprintf(
			`SELECT count(*) FROM p_1.entity_skill_mapping
			 WHERE skill_id = %d AND entity_version_id NOT IN (%d, %d)`,
			sourceID, seeded.earlierVersionID, seeded.latestVersionID)) != 0 {
			t.Errorf("the import attached the SOURCE skill %d rather than its own copy", sourceID)
		}
	}

	// The source agent keeps its own attachments. A move that satisfied every
	// assertion above would still break the agent the file was exported from.
	if source := readAttachedSkills(t, pool, seeded.latestVersionID); len(source) != 2 {
		t.Errorf("the source `latest` carries %d skill attachments after the round trip, want 2", len(source))
	}
	if source := readAttachedSkills(t, pool, seeded.earlierVersionID); len(source) != 1 {
		t.Errorf("the source `earlier` carries %d skill attachments after the round trip, want 1", len(source))
	}

	// The skill's own content travelled with it, not only its name.
	assertImportedSkillTags(t, pool, skillRoundTripPinnedName, seeded.pinnedSkillID)

	// Every imported version is a DRAFT, and the seed made every source version
	// `published`, so a copy that carried the status through fails here.
	// `published` says this project holds a twin of the version in the public
	// project; an import creates no twin, and a skill with a published version
	// can neither be deleted nor unpublished
	// (internal/infra/db/repos/skills.go:585-604).
	if count := importLinkCount(t, pool, fmt.Sprintf(`
SELECT count(*) FROM p_1.skill_versions
WHERE status <> 'draft' AND skill_id NOT IN (%d, %d)`,
		seeded.pinnedSkillID, seeded.looseSkillID)); count != 0 {
		t.Errorf("%d imported skill versions are not drafts", count)
	}

	// Both versions of skill one travelled, because both are named by an
	// attachment. This is the premise every resolution assertion above rests on:
	// if the document carried one version, every branch of the resolver would
	// land on the same row and none of them could fail.
	if count := importLinkCount(t, pool, fmt.Sprintf(`
SELECT count(*) FROM p_1.skill_versions AS version
JOIN p_1.skills AS skill ON skill.id = version.skill_id
WHERE skill.name = '%s' AND skill.id <> %d`,
		skillRoundTripPinnedName, seeded.pinnedSkillID)); count != 2 {
		t.Errorf("the imported copy of %q carries %d versions, want the two the document names",
			skillRoundTripPinnedName, count)
	}
}

// TestForkKeepsTheSkillAttachments is the same statement for the fork.
//
// The fork copies `applications`, `application_versions`,
// `application_variables` and the tag association, and it copied no row of
// `entity_skill_mapping`. It reads the export the fork button fetches, which
// carries the same `skills` array, and the legacy fork hands exactly that key
// to the same import (legacy/plugins/elitea_core/api/v2/fork.py).
func TestForkKeepsTheSkillAttachments(t *testing.T) {
	pool := newImportLinkPool(t)
	handler := eliteacore.NewHandler(pool)

	seeded := seedSkillRoundTripAgent(t, pool)
	document := exportRoundTrip(t, handler, seeded.applicationID, http.StatusOK)
	assertExportedSkillDocument(t, document)

	recorder := forkSkillDo(t, handler, map[string]any{
		"applications": document["applications"],
		"skills":       document["skills"],
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("fork status = %d, want %d, body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	if errors := decodeSkillErrors(t, recorder); len(errors) != 0 {
		t.Fatalf("the fork reported skill errors: %s", recorder.Body.String())
	}

	assertAttachedSkills(t,
		readAttachedSkillsExcept(t, pool, seeded.earlierVersionID, seeded.latestVersionID),
		[]attachedSkill{
			{
				skillName: skillRoundTripPinnedName, versionName: "base",
				instructions: skillRoundTripBaseBody, entityType: "agent",
			},
			{
				skillName: skillRoundTripPinnedName, versionName: skillRoundTripPinnedVName,
				instructions: skillRoundTripPinnedBody, entityType: "agent",
			},
			{
				skillName: skillRoundTripLooseName, versionName: "base",
				instructions: skillRoundTripLooseBody, entityType: "agent",
			},
		})
}

// TestForkReportsASkillItCannotFork covers the fork's own error channel, which
// nothing exercised. The import path has three cases for its channel and the
// fork had none, so a regression that dropped the `errorSkills` append, or that
// stopped counting it in the status rule, would leave the suite green.
//
// Three branches in one case, because they share a fixture: an entry that is not
// an object, an entry that cannot be imported, and the index every fork skill
// error carries.
func TestForkReportsASkillItCannotFork(t *testing.T) {
	pool := newImportLinkPool(t)
	handler := eliteacore.NewHandler(pool)

	recorder := forkSkillDo(t, handler, map[string]any{
		"applications": []any{map[string]any{
			"id": "1", "name": "fixture agent", "owner_id": "9",
			"versions": []any{map[string]any{"name": "latest", "agent_type": "openai"}},
		}},
		"skills": []any{
			"not an object",
			// An entry with no version cannot be imported, and the fork must say
			// so rather than write a skill with no instructions.
			map[string]any{"import_uuid": "sk-1", "name": "empty skill", "versions": []any{}},
		},
	})
	// 207 and not 400: the agent itself forked. Only the skills failed.
	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("fork status = %d, want %d, body = %s", recorder.Code, http.StatusMultiStatus, recorder.Body.String())
	}

	reported := decodeSkillErrors(t, recorder)
	if len(reported) != 2 {
		t.Fatalf("errors.skills = %+v, want both refused entries", reported)
	}
	// The index space is ONE list: the applications, then the skills — the
	// legacy's own numbering (fork.py concatenates the two arrays and numbers
	// every entry by its position in the result). This body sends one
	// application, so the two skills are at 1 and 2. An entry that reported 0
	// would name the AGENT, which forked correctly.
	for position, entry := range reported {
		if want := 1 + position; entry.Index != want {
			t.Errorf("fork skill error %d has index = %d, want %d — the index names the skill in the "+
				"concatenation of applications then skills", position, entry.Index, want)
		}
	}
	// The message has to name the entry, because the index cannot.
	if !bytes.Contains([]byte(reported[0].Msg), []byte("skills entry 0")) {
		t.Errorf("the first message %q names no position, so the user cannot tell which entry failed", reported[0].Msg)
	}
	if !bytes.Contains([]byte(reported[1].Msg), []byte("empty skill")) {
		t.Errorf("the second message %q does not name the skill", reported[1].Msg)
	}
	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.skills`); count != 0 {
		t.Errorf("skill rows = %d, want 0 — neither entry could be forked", count)
	}
}

// TestForkAttributesTheFailureToTheSkillThatFailed is what the index is FOR.
//
// One application and TWO skills, of which only the second is unusable. A single
// skill in the body cannot discriminate: index 0 and "the first skill" are the
// same entry, so every wrong implementation passes. With two, an index that
// names the agent, or the wrong skill, changes the assertion.
func TestForkAttributesTheFailureToTheSkillThatFailed(t *testing.T) {
	pool := newImportLinkPool(t)
	handler := eliteacore.NewHandler(pool)

	recorder := forkSkillDo(t, handler, map[string]any{
		"applications": []any{map[string]any{
			"id": "1", "name": "fixture agent", "owner_id": "9",
			"versions": []any{map[string]any{"name": "latest", "agent_type": "openai"}},
		}},
		"skills": []any{
			map[string]any{
				"import_uuid": "sk-good", "name": "a skill that forks", "description": "seeded",
				"versions": []any{map[string]any{"name": "base", "instructions": "read this"}},
			},
			// No version, so this one cannot be forked.
			map[string]any{"import_uuid": "sk-bad", "name": "a skill that cannot", "versions": []any{}},
		},
	})
	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("fork status = %d, want %d, body = %s", recorder.Code, http.StatusMultiStatus, recorder.Body.String())
	}

	reported := decodeSkillErrors(t, recorder)
	if len(reported) != 1 {
		t.Fatalf("errors.skills = %+v, want exactly the skill that could not be forked", reported)
	}
	// One application, then two skills: the SECOND skill is at index 2.
	if reported[0].Index != 2 {
		t.Errorf("the failure is reported at index %d, want 2 — index 0 names the agent, 1 the skill that worked",
			reported[0].Index)
	}
	if reported[0].Name != "a skill that cannot" {
		t.Errorf("the failure names %q, want the skill that failed", reported[0].Name)
	}
	// The good skill still forked. A report that blamed the wrong entry would
	// send the user to look at a skill that is in their project and correct.
	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.skills`); count != 1 {
		t.Errorf("skill rows = %d, want the one that forked", count)
	}
}

// TestForkSaysWhenTheRequestCarriesNoSkills separates "you sent no skills" from
// "the skill you sent could not be linked".
//
// The route used to answer 201 for an applications-only body, and a current
// export's version entries now carry skill references. A client that forwards
// the applications and drops the document's top-level `skills` array asks for
// skills it did not send. That fork IS incomplete, so it still answers 207 —
// but the response must not describe the file as broken.
func TestForkSaysWhenTheRequestCarriesNoSkills(t *testing.T) {
	pool := newImportLinkPool(t)
	handler := eliteacore.NewHandler(pool)
	seeded := seedSkillRoundTripAgent(t, pool)
	document := exportRoundTrip(t, handler, seeded.applicationID, http.StatusOK)

	// The applications alone, exactly as a non-wizard client would send them.
	recorder := forkSkillDo(t, handler, map[string]any{"applications": document["applications"]})
	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("fork status = %d, want %d, body = %s", recorder.Code, http.StatusMultiStatus, recorder.Body.String())
	}
	reported := decodeSkillErrors(t, recorder)
	if len(reported) != 2 {
		t.Fatalf("errors.skills = %+v, want one message for each version that references a skill", reported)
	}
	for _, entry := range reported {
		if !bytes.Contains([]byte(entry.Msg), []byte("carries no skills array")) {
			t.Errorf("reported msg = %q, want it to name the missing array rather than blame the file", entry.Msg)
		}
	}
	// The agent forked. Only its skills did not.
	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.applications`); count != 2 {
		t.Errorf("applications rows = %d, want the seeded agent and its fork", count)
	}
	if count := importLinkCount(t, pool, fmt.Sprintf(
		`SELECT count(*) FROM p_1.entity_skill_mapping WHERE entity_version_id NOT IN (%d, %d)`,
		seeded.earlierVersionID, seeded.latestVersionID)); count != 0 {
		t.Errorf("the fork wrote %d skill attachments from a body that named no skills", count)
	}
}

/* ── the report the issue asks for ───────────────────────────────────────── */

// TestImportReportsASkillItCannotLink is task 4 of the issue. A version names a
// skill the document does not carry, so there is nothing in the destination
// project to attach. The reference must be REPORTED, the way a failed toolkit
// link is (#420), and never dropped in silence.
func TestImportReportsASkillItCannotLink(t *testing.T) {
	pool := newImportLinkPool(t)
	router := importLinkRouter(eliteacore.NewHandler(pool))

	recorder := importLinkDo(t, router, []any{
		map[string]any{
			"entity": "agents", "import_uuid": "ag-1", "name": "fixture agent",
			"versions": []any{map[string]any{
				"name":         "latest",
				"agent_type":   "openai",
				"instructions": "do the thing",
				"skills": []any{map[string]any{
					"import_uuid": "sk-missing", "version_name": "base", "entity_type": "agent",
				}},
			}},
		},
	})
	// 207 and not 400: the agent itself imported. Only one of the rows that
	// make it complete is missing, which is partial success.
	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusMultiStatus, recorder.Body.String())
	}
	reported := decodeSkillErrors(t, recorder)
	if len(reported) != 1 {
		t.Fatalf("errors.skills = %+v, want exactly the reference that could not be linked", reported)
	}
	// The wizard marks the entity by this index and by nothing else
	// (apps/elitea-ui .../importWizardForkImport.helpers.js, getErrorImportUUID).
	if reported[0].Index != 0 {
		t.Errorf("reported index = %d, want the agent's position 0", reported[0].Index)
	}
	if !bytes.Contains([]byte(reported[0].Msg), []byte("sk-missing")) {
		t.Errorf("reported msg = %q, want it to name the skill it could not link", reported[0].Msg)
	}
	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.entity_skill_mapping`); count != 0 {
		t.Errorf("skill attachment rows = %d, want 0", count)
	}
}

// TestImportReportsAFailedSkillAttachment breaks the INSERT itself, so the
// skill imports and the row that joins it to the agent cannot be written. This
// is the other half of the same rule: a failed write is a different fault from
// an unresolved reference, and the response must not stay quiet about either.
func TestImportReportsAFailedSkillAttachment(t *testing.T) {
	pool := newImportLinkPool(t)
	router := importLinkRouter(eliteacore.NewHandler(pool))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Break the attachment statement and nothing before it. Every row the
	// import can write carries a positive entity_version_id and cannot satisfy
	// this.
	if _, err := pool.Exec(ctx, `
ALTER TABLE p_1.entity_skill_mapping ADD CONSTRAINT attachment_must_fail CHECK (entity_version_id < 0)`); err != nil {
		t.Fatalf("install failing constraint: %v", err)
	}

	recorder := importLinkDo(t, router, skillImportBody("sk-1", "base"))
	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusMultiStatus, recorder.Body.String())
	}
	reported := decodeSkillErrors(t, recorder)
	if len(reported) != 1 {
		t.Fatalf("errors.skills = %+v, want exactly the failed attachment", reported)
	}
	if !bytes.Contains([]byte(reported[0].Msg), []byte("unable to link skill")) {
		t.Errorf("reported msg = %q, want the failed-attachment message", reported[0].Msg)
	}
	// The skill itself imported. The report must be about the attachment only.
	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.skills`); count != 1 {
		t.Errorf("skill rows = %d, want the imported skill", count)
	}
}

// TestImportTreatsASkillAsItsOwnEntity is the second fault this repair closes.
// The wizard already sent `entity: "skills"` entries — it builds them out of
// any markdown file that carries a skills block
// (apps/elitea-ui .../importWizardParser.helpers.js,
// buildSkillsFromFrontmatter) — and this service had no branch for them. They
// fell to the branch that treats an entry as an agent, so importing such a file
// created a phantom AGENT named after each skill.
func TestImportTreatsASkillAsItsOwnEntity(t *testing.T) {
	pool := newImportLinkPool(t)
	router := importLinkRouter(eliteacore.NewHandler(pool))

	recorder := importLinkDo(t, router, []any{map[string]any{
		"entity": "skills", "import_uuid": "sk-1", "name": "lonely skill",
		"description": "seeded",
		"versions":    []any{map[string]any{"name": "base", "instructions": "read this"}},
	}})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.applications`); count != 0 {
		t.Errorf("applications rows = %d, want 0 — a skill is not an agent", count)
	}
	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.skills`); count != 1 {
		t.Errorf("skills rows = %d, want the imported skill", count)
	}
	// owner_id on this table is the DESTINATION PROJECT and not a user (#533).
	var ownerID, versionCount int
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pool.QueryRow(ctx, `
SELECT skill.owner_id, count(version.id)
FROM p_1.skills AS skill
LEFT JOIN p_1.skill_versions AS version ON version.skill_id = skill.id
GROUP BY skill.owner_id`).Scan(&ownerID, &versionCount); err != nil {
		t.Fatalf("read the imported skill: %v", err)
	}
	if ownerID != 1 {
		t.Errorf("skills.owner_id = %d, want the destination project 1", ownerID)
	}
	if versionCount != 1 {
		t.Errorf("skill_versions rows = %d, want the imported version", versionCount)
	}
}

// TestImportRefusesASkillWithNoVersion states what an unusable entry does. A
// skill with no version has no instructions, so nothing attached to it can run.
// It is refused and named rather than written as an empty shell.
func TestImportRefusesASkillWithNoVersion(t *testing.T) {
	pool := newImportLinkPool(t)
	router := importLinkRouter(eliteacore.NewHandler(pool))

	recorder := importLinkDo(t, router, []any{map[string]any{
		"entity": "skills", "import_uuid": "sk-1", "name": "empty skill",
		"description": "seeded", "versions": []any{},
	}})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	reported := decodeSkillErrors(t, recorder)
	if len(reported) != 1 {
		t.Fatalf("errors.skills = %+v, want exactly the refused skill", reported)
	}
	if !bytes.Contains([]byte(reported[0].Msg), []byte("carries no version")) {
		t.Errorf("reported msg = %q, want it to name the reason", reported[0].Msg)
	}
}

// TestImportKeepsTheSkillCapOfAVersion pins the cap the attach route enforces
// (v2skills.MaxSkillsPerEntityVersion, pylon's MAX_SKILLS_PER_AGENT). The read
// side renders it as "n/5 skills added" and disables the menu at the limit, so
// an import that walked past it would leave a counter that says 6/5.
func TestImportKeepsTheSkillCapOfAVersion(t *testing.T) {
	pool := newImportLinkPool(t)
	router := importLinkRouter(eliteacore.NewHandler(pool))

	entities := make([]any, 0, 7)
	references := make([]any, 0, 6)
	for index := 0; index < 6; index++ {
		importUUID := fmt.Sprintf("sk-%d", index)
		entities = append(entities, map[string]any{
			"entity": "skills", "import_uuid": importUUID,
			"name": fmt.Sprintf("capped skill %d", index), "description": "seeded",
			"versions": []any{map[string]any{"name": "base", "instructions": "read this"}},
		})
		references = append(references, map[string]any{
			"import_uuid": importUUID, "version_name": "base", "entity_type": "agent",
		})
	}
	entities = append(entities, map[string]any{
		"entity": "agents", "import_uuid": "ag-1", "name": "fixture agent",
		"versions": []any{map[string]any{
			"name": "latest", "agent_type": "openai", "instructions": "do the thing",
			"skills": references,
		}},
	})

	recorder := importLinkDo(t, router, entities)
	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusMultiStatus, recorder.Body.String())
	}
	reported := decodeSkillErrors(t, recorder)
	if len(reported) != 1 {
		t.Fatalf("errors.skills = %+v, want exactly the reference over the cap", reported)
	}
	if !bytes.Contains([]byte(reported[0].Msg), []byte("at most 5 skills")) {
		t.Errorf("reported msg = %q, want it to name the cap", reported[0].Msg)
	}
	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.entity_skill_mapping`); count != 5 {
		t.Errorf("skill attachment rows = %d, want the capped 5", count)
	}
}

// TestImportedSkillIsReadableThroughTheSkillsRoute is the invariant the whole Go
// skills surface rests on, and the one this change came close to breaking.
//
// The export ships only the versions the attachments name, so an agent pinned
// to a version called `reviewed` exports a skill whose version list is
// `[reviewed]` and nothing else. Written to the destination as it stands, that
// skill has no `base` row — and `skillsFromJoin`
// (internal/infra/db/repos/skills.go:34-39) LEFT JOINs `sv.name = 'base'` for
// every skills read this service has. The skill would come back with a name, a
// description, and no instructions, no versions and no version details: a blank
// entry on the agent's Skills panel, on the skill list and in the skill editor,
// while chat ran correctly because it reads `skill_version_id` and not the name.
//
// The route is reached from the product with no pylon anywhere: `skillBlocks`
// writes `version: <name>` into the markdown this service hands the user, and
// `buildSkillsFromFrontmatter`
// (apps/elitea-ui .../importWizardParser.helpers.js:142) reads it back as
// `name: block.version || 'base'`.
//
// The assertion goes through `repos.SkillsRepo.Get` — the read
// GET /elitea_core/skill/{mode}/{projectID}/{skillID} makes, and the read the
// skill editor and the skills list both render — and not through a copy of its
// SQL, so it cannot drift away from what the product runs.
//
// NOTE(#395): it used to go through `repos.SkillsRepo.ListForApplicationVersion`.
// That method served the PROTOTYPE attached-skills route, which #395 deleted
// with the method. The route that answers now,
// internal/api/v2/applicationskills, deliberately does NOT project
// instructions, tags or versions, so it cannot see this defect at all. Get
// projects all three through the same `skillsFromJoin` LEFT JOIN, which is
// where the defect lives.
func TestImportedSkillIsReadableThroughTheSkillsRoute(t *testing.T) {
	pool := newImportLinkPool(t)
	router := importLinkRouter(eliteacore.NewHandler(pool))

	const versionName = "reviewed"
	const instructions = "follow the steps"
	recorder := importLinkDo(t, router, []any{
		map[string]any{
			"entity": "skills", "import_uuid": "sk-1",
			"name": "pinned skill", "description": "seeded skill",
			// No `base`. This is exactly what the export of an agent pinned to
			// a named version produces.
			"versions": []any{map[string]any{"name": versionName, "instructions": instructions}},
		},
		map[string]any{
			"entity": "agents", "import_uuid": "ag-1", "name": "fixture agent",
			"versions": []any{map[string]any{
				"name": "latest", "agent_type": "openai", "instructions": "do the thing",
				"skills": []any{map[string]any{
					"import_uuid": "sk-1", "version_name": versionName, "entity_type": "agent",
				}},
			}},
		},
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var agentVersionID int
	if err := pool.QueryRow(ctx, `SELECT id FROM p_1.application_versions`).Scan(&agentVersionID); err != nil {
		t.Fatalf("the import wrote no agent version: %v", err)
	}

	var attachedSkillID int
	if err := pool.QueryRow(ctx, `
SELECT mapping.skill_id
FROM p_1.entity_skill_mapping AS mapping
WHERE mapping.entity_version_id = $1 AND mapping.entity_type = 'agent'`,
		agentVersionID).Scan(&attachedSkillID); err != nil {
		t.Fatalf("the import attached no skill to the agent version: %v", err)
	}

	skill, err := repos.NewSkillsRepo(pool).Get(ctx, "1", strconv.Itoa(attachedSkillID))
	if err != nil {
		t.Fatalf("read the agent's skill: %v", err)
	}
	// Each of these is empty when the skill has no `base` version, and each is
	// what the user actually reads.
	if skill.Instructions != instructions {
		t.Errorf("the imported skill reads as instructions %q, want %q", skill.Instructions, instructions)
	}
	if len(skill.Versions) == 0 {
		t.Error("the imported skill reads with no versions, so the editor renders it blank")
	}
	if skill.VersionDetails == nil {
		t.Error("the imported skill reads with no version_details, which is the key the frontend prefers")
	}

	// ADDITIVE, not a rename: the named version has to survive, or the
	// attachment that pins it would resolve to something else.
	names := map[string]string{}
	rows, err := pool.Query(ctx, `
SELECT version.name, version.instructions
FROM p_1.skill_versions AS version
JOIN p_1.skills AS skill ON skill.id = version.skill_id
WHERE skill.name = 'pinned skill'`)
	if err != nil {
		t.Fatalf("read the imported skill versions: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, body string
		if err := rows.Scan(&name, &body); err != nil {
			t.Fatalf("scan an imported skill version: %v", err)
		}
		names[name] = body
	}
	if len(names) != 2 || names["base"] != instructions || names[versionName] != instructions {
		t.Errorf("the imported skill carries versions %v, want a `base` clone beside the named %q", names, versionName)
	}

	// The attachment still pins the NAMED version, not the clone.
	assertAttachedSkills(t, readAttachedSkills(t, pool, agentVersionID), []attachedSkill{{
		skillName: "pinned skill", versionName: versionName,
		instructions: instructions, entityType: "agent",
	}})
}

// TestImportOfTheSameDocumentIsRepeatable is the case the wizard invites most.
//
// A partly failed import answers 207 and the wizard paints the failed entity red
// and invites a retry. The skills that succeeded were written by a plain INSERT,
// so the retry wrote them AGAIN: the project collected one copy of every skill
// per attempt, each orphaned from the agent whose import kept failing.
func TestImportOfTheSameDocumentIsRepeatable(t *testing.T) {
	pool := newImportLinkPool(t)
	router := importLinkRouter(eliteacore.NewHandler(pool))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Make the AGENT fail and leave the skill import untouched, which is the
	// shape that produces the retry.
	if _, err := pool.Exec(ctx, `
ALTER TABLE p_1.application_versions ADD CONSTRAINT version_must_fail CHECK (name <> 'latest')`); err != nil {
		t.Fatalf("install failing constraint: %v", err)
	}

	body := skillImportBody("sk-1", "base")
	for attempt := 1; attempt <= 3; attempt++ {
		recorder := importLinkDo(t, router, body)
		if recorder.Code != http.StatusMultiStatus {
			t.Fatalf("attempt %d: import status = %d, want %d, body = %s",
				attempt, recorder.Code, http.StatusMultiStatus, recorder.Body.String())
		}
		if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.skills`); count != 1 {
			t.Fatalf("after attempt %d the project holds %d skills, want the one the file names", attempt, count)
		}
		if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.skill_versions`); count != 1 {
			t.Errorf("after attempt %d the skill holds %d versions, want 1", attempt, count)
		}
	}

	// The retry converges: with the agent repaired, the same document attaches
	// to the one skill it has been writing all along.
	if _, err := pool.Exec(ctx, `ALTER TABLE p_1.application_versions DROP CONSTRAINT version_must_fail`); err != nil {
		t.Fatalf("drop the failing constraint: %v", err)
	}
	recorder := importLinkDo(t, router, body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("the repaired import status = %d, want %d, body = %s",
			recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.skills`); count != 1 {
		t.Errorf("the project holds %d skills after four runs of one document, want 1", count)
	}
	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.entity_skill_mapping`); count != 1 {
		t.Errorf("skill attachment rows = %d, want the one the repaired run wrote", count)
	}
}

// TestImportRefusesASkillWithNoImportUUID states what an entity nothing can
// reference does. It used to be written and reported in `result.skills` as a
// success, while every reference to it answered "it is not among the imported
// skills" — the response claiming both at once, over a row the caller could not
// find.
func TestImportRefusesASkillWithNoImportUUID(t *testing.T) {
	pool := newImportLinkPool(t)
	router := importLinkRouter(eliteacore.NewHandler(pool))

	recorder := importLinkDo(t, router, []any{map[string]any{
		"entity": "skills", "name": "anonymous skill", "description": "seeded",
		"versions": []any{map[string]any{"name": "base", "instructions": "read this"}},
	}})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	reported := decodeSkillErrors(t, recorder)
	if len(reported) != 1 || !bytes.Contains([]byte(reported[0].Msg), []byte("import_uuid")) {
		t.Fatalf("errors.skills = %+v, want it to name the missing import_uuid", reported)
	}
	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.skills`); count != 0 {
		t.Errorf("skill rows = %d, want 0 — a refused entity must write nothing", count)
	}
}

// TestImportRefusesAnEntityTypeNothingReads closes the one path that could write
// a row no reader ever matches. Both skills reads filter on the literal `agent`
// (internal/db/queries/agent_chat.sql:132 and the attached-skill registry), and
// the sibling write route answers 400 for any other value, so the import must
// not accept one from a file.
func TestImportRefusesAnEntityTypeNothingReads(t *testing.T) {
	pool := newImportLinkPool(t)
	router := importLinkRouter(eliteacore.NewHandler(pool))

	entities := skillImportBody("sk-1", "base")
	agent, _ := entities[1].(map[string]any)
	versions, _ := agent["versions"].([]any)
	version, _ := versions[0].(map[string]any)
	references, _ := version["skills"].([]any)
	reference, _ := references[0].(map[string]any)
	reference["entity_type"] = "pipeline"

	recorder := importLinkDo(t, router, entities)
	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusMultiStatus, recorder.Body.String())
	}
	reported := decodeSkillErrors(t, recorder)
	if len(reported) != 1 || !bytes.Contains([]byte(reported[0].Msg), []byte("entity_type must be")) {
		t.Fatalf("errors.skills = %+v, want it to refuse the entity_type", reported)
	}
	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.entity_skill_mapping`); count != 0 {
		t.Errorf("skill attachment rows = %d, want 0 — the row would be one nothing reads", count)
	}
}

// TestImportReportsASkillItWroteAndCouldNotFinish covers the half-written skill.
// The `skills` row is inserted before its versions, so a version that cannot be
// written leaves a row behind. It used to appear in neither `result.skills` nor
// the errors' account of what exists, so the caller owned a row it could not
// find and every reference to it said the skill was never imported.
func TestImportReportsASkillItWroteAndCouldNotFinish(t *testing.T) {
	pool := newImportLinkPool(t)
	router := importLinkRouter(eliteacore.NewHandler(pool))

	recorder := importLinkDo(t, router, []any{map[string]any{
		"entity": "skills", "import_uuid": "sk-1", "name": "half a skill",
		"description": "seeded",
		"versions": []any{
			map[string]any{"name": "base", "instructions": "read this"},
			// Not an object, so importSkill stops after the row and the first
			// version are already written.
			"not a version",
		},
	}})
	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusMultiStatus, recorder.Body.String())
	}
	// The row exists, so the response must name it on BOTH channels: the result
	// so the caller can find it, the errors so the caller knows it is partial.
	answer := decodeImportLink(t, recorder)
	if len(answer.Result.Agents) != 0 {
		t.Errorf("result.agents = %+v, want none — the document carries only a skill", answer.Result.Agents)
	}
	reported := decodeSkillErrors(t, recorder)
	if len(reported) != 1 || !bytes.Contains([]byte(reported[0].Msg), []byte("was written and is incomplete")) {
		t.Fatalf("errors.skills = %+v, want it to say the row exists and is partial", reported)
	}
	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.skills`); count != 1 {
		t.Fatalf("skill rows = %d, want the row the import wrote", count)
	}
	var reportedID int
	if err := pool.QueryRow(context.Background(), `SELECT id FROM p_1.skills`).Scan(&reportedID); err != nil {
		t.Fatalf("read the half-written skill: %v", err)
	}
	if !bytes.Contains([]byte(reported[0].Msg), []byte(strconv.Itoa(reportedID))) {
		t.Errorf("reported msg = %q, want it to name skill %d so the caller can find it", reported[0].Msg, reportedID)
	}
}

// TestExportGivesAVersionlessSkillAnEmptyArray states the document's one shape.
// `skills` needs no `skill_versions` row and `skill_version_id` is nullable, so
// an agent can be attached to a skill that has no version. The export used to
// emit `"versions": null` for it, the only null array in a document whose every
// other array is `[]`.
func TestExportGivesAVersionlessSkillAnEmptyArray(t *testing.T) {
	pool := newImportLinkPool(t)
	handler := eliteacore.NewHandler(pool)
	seeded := seedRoundTripAgent(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var skillID int
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.skills (name, description, owner_id, author_id)
VALUES ('a skill with no version', 'seeded', 1, $1) RETURNING id`, importLinkPrincipal).Scan(&skillID); err != nil {
		t.Fatalf("seed the version-less skill: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.entity_skill_mapping (entity_version_id, entity_type, skill_id, skill_version_id)
VALUES ($1, 'agent', $2, NULL)`, seeded.versionID, skillID); err != nil {
		t.Fatalf("attach the version-less skill: %v", err)
	}

	document := exportRoundTrip(t, handler, seeded.applicationID, http.StatusOK)
	exported, _ := document["skills"].([]any)
	if len(exported) != 1 {
		t.Fatalf("the export carries %v as its skills array, want the attached skill", document["skills"])
	}
	skill, _ := exported[0].(map[string]any)
	versions, present := skill["versions"]
	if !present {
		t.Fatalf("the exported skill carries no versions key: %v", skill)
	}
	if versions == nil {
		t.Fatalf("the exported skill carries versions: null, and every other array in the document is []")
	}
	if list, ok := versions.([]any); !ok || len(list) != 0 {
		t.Errorf("the exported skill carries versions %v, want an empty array", versions)
	}
}

/* ── the export refuses a lost read ──────────────────────────────────────── */

// TestExportRefusesALostSkillRead extends the rule of `exportReadFailed` to the
// three reads this repair adds. An export is a BACKUP: a document that is
// missing a skill is worse than no document, because the operator keeps it and
// finds out later.
func TestExportRefusesALostSkillRead(t *testing.T) {
	pool := newImportLinkPool(t)
	handler := eliteacore.NewHandler(pool)
	seeded := seedSkillRoundTripAgent(t, pool)

	for _, testCase := range []struct {
		name    string
		table   string
		column  string
		renamed string
	}{
		{"the attachments", "entity_skill_mapping", "skill_id", "skill_id_moved"},
		{"the skills", "skills", "description", "description_moved"},
		{"the skill versions", "skill_versions", "instructions", "instructions_moved"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// The case before the break proves the export answers 200 on this
			// data, so the refusal is the effect of the break and nothing else.
			exportRoundTrip(t, handler, seeded.applicationID, http.StatusOK)

			roundTripExec(t, pool, fmt.Sprintf(`ALTER TABLE p_1.%s RENAME COLUMN %s TO %s`,
				testCase.table, testCase.column, testCase.renamed))
			t.Cleanup(func() {
				roundTripExec(t, pool, fmt.Sprintf(`ALTER TABLE p_1.%s RENAME COLUMN %s TO %s`,
					testCase.table, testCase.renamed, testCase.column))
			})

			document := exportRoundTrip(t, handler, seeded.applicationID, http.StatusInternalServerError)
			if _, hasApplications := document["applications"]; hasApplications {
				t.Errorf("a refused export must carry no applications key, got %v", document)
			}
		})
	}
}

// TestExportOmitsTheSkillsArrayForASkilllessAgent states the shape of a
// document for the agent that has no skills, which is nearly every agent. The
// key is left off, the way the legacy leaves it off (export_import.py:210-211),
// because the import wizard turns every top-level array into a row the user
// selects and an empty one would show a group with nothing in it.
func TestExportOmitsTheSkillsArrayForASkilllessAgent(t *testing.T) {
	pool := newImportLinkPool(t)
	handler := eliteacore.NewHandler(pool)
	seeded := seedRoundTripAgent(t, pool)

	document := exportRoundTrip(t, handler, seeded.applicationID, http.StatusOK)
	if _, present := document["skills"]; present {
		t.Errorf("the export of a skill-less agent carries a skills key: %v", document["skills"])
	}
	// The VERSION key is written either way, beside `tools`, `variables` and
	// `tags`, so a reader can tell "no skills" from "this export is older than
	// the field".
	version := skillRoundTripVersionNamed(t, document, "latest")
	references, present := version["skills"].([]any)
	if !present {
		t.Fatalf("the exported version carries no skills key: %v", version)
	}
	if len(references) != 0 {
		t.Errorf("the exported version carries %d skill references, want none", len(references))
	}
}

// TestMarkdownExportCarriesTheSkillBlocks closes the third gap the markdown
// export used to disclose. `_extract_skills_for_md` reads the document's
// `skills` array, `ExportImportGet` built no such array, and export_markdown.go
// recorded that as a key it has no producer for. The array exists now, so the
// key is written — and the wizard rebuilds both halves of the JSON shape out of
// it (apps/elitea-ui .../importWizardParser.helpers.js,
// buildSkillsFromFrontmatter), which is what makes a markdown file that carries
// a skill importable at all.
func TestMarkdownExportCarriesTheSkillBlocks(t *testing.T) {
	pool := newImportLinkPool(t)
	handler := eliteacore.NewHandler(pool)
	seeded := seedSkillRoundTripAgent(t, pool)

	recorder := exportWithQuery(t, handler, seeded.applicationID, "?format=md")
	if recorder.Code != http.StatusOK {
		t.Fatalf("markdown export status = %d, want %d", recorder.Code, http.StatusOK)
	}
	// The agent has two versions, so the markdown export is one file per
	// version in a ZIP. Reading the archive is also the only way to state the
	// claim that matters: each file must carry the version of the skill THAT
	// file's agent version is attached to.
	files := readMarkdownArchive(t, recorder)
	if len(files) != 2 {
		t.Fatalf("the markdown export carries %d files, want one per agent version", len(files))
	}

	earlier := markdownFileFor(t, files, "earlier")
	latest := markdownFileFor(t, files, "latest")

	// `latest` pins `reviewed`. A block that reached for the skill's first
	// version instead would render `version: reviewed` above the BASE body —
	// a file whose two halves disagree, which the reader then edits and
	// imports again.
	assertMarkdownSkillBlock(t, latest, "latest",
		[]string{"skills:", skillRoundTripPinnedName, "version: " + skillRoundTripPinnedVName,
			skillRoundTripPinnedBody, skillRoundTripLooseName, skillRoundTripLooseBody},
		[]string{skillRoundTripBaseBody, skillRoundTripUnusedBody})

	// `earlier` pins `base`, and carries only that one skill.
	assertMarkdownSkillBlock(t, earlier, "earlier",
		[]string{"skills:", skillRoundTripPinnedName, skillRoundTripBaseBody},
		[]string{skillRoundTripPinnedBody, skillRoundTripLooseName, skillRoundTripUnusedBody})
}

// readMarkdownArchive reads the ZIP the markdown export answers with when the
// agent has more than one version.
func readMarkdownArchive(t *testing.T, recorder *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	body := recorder.Body.Bytes()
	archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("the markdown export is not a ZIP (content type %q): %v",
			recorder.Header().Get("Content-Type"), err)
	}
	files := map[string]string{}
	for _, entry := range archive.File {
		handle, err := entry.Open()
		if err != nil {
			t.Fatalf("open %q in the markdown archive: %v", entry.Name, err)
		}
		content, err := io.ReadAll(handle)
		_ = handle.Close()
		if err != nil {
			t.Fatalf("read %q in the markdown archive: %v", entry.Name, err)
		}
		files[entry.Name] = string(content)
	}
	return files
}

// markdownFileFor finds the file one agent version rendered to. The name comes
// from `markdownFilename`, which slugs the version into it.
func markdownFileFor(t *testing.T, files map[string]string, versionName string) string {
	t.Helper()
	for name, content := range files {
		if strings.Contains(name, "."+versionName+".") {
			return content
		}
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	t.Fatalf("the markdown archive carries no file for version %q, only %v", versionName, names)
	return ""
}

func assertMarkdownSkillBlock(t *testing.T, content, versionName string, want, absent []string) {
	t.Helper()
	for _, text := range want {
		if !strings.Contains(content, text) {
			t.Errorf("the markdown of version %q does not carry %q; file was:\n%s", versionName, text, content)
		}
	}
	for _, text := range absent {
		if strings.Contains(content, text) {
			t.Errorf("the markdown of version %q carries %q, which belongs to another version; file was:\n%s",
				versionName, text, content)
		}
	}
}

/* ── the channel set every answer carries ────────────────────────────────── */

// answerChannels is the set of entity channels that every import and every fork
// answer must carry, on `result` and on `errors` both.
//
// It is the legacy set. `rpc/import_wizzard.py` seeds `result[key] = []` and
// `errors[key] = []` for every key of ENTITY_IMPORT_MAPPER, which is exactly
// agents, toolkits and skills
// (legacy/plugins/elitea_core/utils/export_import_utils.py:21-25).
var answerChannels = []string{"agents", "toolkits", "skills"}

// TestEveryImportAndForkAnswerCarriesEveryChannel states the key set of the two
// envelopes on all FOUR answer paths: the fork with entities, the fork with an
// empty request, the import with entities and the import with an empty request.
//
// The fork answer left the toolkits channel out, and the wizard reads
// `result[item.entity]` for every row it sent with no guard
// (apps/elitea-ui .../ImportWizardModal/IWModalForkButton.jsx). An agent fork
// that carries toolkit rows therefore raised a TypeError inside an async map
// callback, the `Promise.all` rejected, and neither the error toast nor the
// success branch ran: the dialog stopped with no message. The empty fork answer
// left out toolkits AND skills, and carried `datasources` and `prompts`, which
// no path of this service can fill.
//
// Each path is asserted, because each path builds its own answer. A case that
// covered only the main paths would have stayed green while the two empty ones
// answered with a different set.
func TestEveryImportAndForkAnswerCarriesEveryChannel(t *testing.T) {
	pool := newImportLinkPool(t)
	handler := eliteacore.NewHandler(pool)
	router := importLinkRouter(handler)

	seeded := seedSkillRoundTripAgent(t, pool)
	document := exportRoundTrip(t, handler, seeded.applicationID, http.StatusOK)

	// Every case answers 201, so the assertion below reads the envelopes of a
	// SUCCESSFUL answer. That is the answer the defect came back with: the
	// wizard breaks on the channel that has no entries, never on the one that
	// carries the work, so a fork that did everything right still stopped.
	for _, testCase := range []struct {
		name   string
		answer func(*testing.T) *httptest.ResponseRecorder
	}{
		{
			name: "the fork of an agent and its skills",
			answer: func(t *testing.T) *httptest.ResponseRecorder {
				return forkSkillDo(t, handler, map[string]any{
					"applications": document["applications"],
					"skills":       document["skills"],
				})
			},
		},
		{
			name: "the fork of an empty applications array",
			answer: func(t *testing.T) *httptest.ResponseRecorder {
				return forkSkillDo(t, handler, map[string]any{"applications": []any{}})
			},
		},
		{
			name: "the import of a skill and an agent",
			answer: func(t *testing.T) *httptest.ResponseRecorder {
				return importLinkDo(t, router, skillImportBody("sk-channel-set", "base"))
			},
		},
		{
			name: "the import of an empty entity array",
			answer: func(t *testing.T) *httptest.ResponseRecorder {
				return importLinkDo(t, router, []any{})
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := testCase.answer(t)
			if recorder.Code != http.StatusCreated {
				t.Fatalf("status = %d, want %d, body = %s",
					recorder.Code, http.StatusCreated, recorder.Body.String())
			}
			assertAnswerChannels(t, recorder)
		})
	}
}

// assertAnswerChannels reads `result` and `errors` as MAPS and states the key
// set of each.
//
// A struct decode cannot see this defect. A missing key leaves the zero value
// of its field, which reads back as an empty channel — exactly what a correct
// answer gives — so the decode reports success on the answer that stops the
// wizard. The keys must be read as data.
//
// A channel that is present but `null` breaks the wizard in the same way an
// absent one does, because `.find` is not a method of `null` either. Each
// channel is therefore also asserted to be a JSON array.
func assertAnswerChannels(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	var answer map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %q: %v", recorder.Body.String(), err)
	}
	for _, envelope := range []string{"result", "errors"} {
		raw, present := answer[envelope]
		if !present {
			t.Errorf("the answer carries no %q envelope: %s", envelope, recorder.Body.String())
			continue
		}
		var channels map[string]json.RawMessage
		if err := json.Unmarshal(raw, &channels); err != nil {
			t.Errorf("the %q envelope is not an object: %s", envelope, raw)
			continue
		}
		for _, name := range answerChannels {
			if _, ok := channels[name]; !ok {
				t.Errorf("the %q envelope carries no %q channel: %s", envelope, name, raw)
			}
		}
		for name, value := range channels {
			if !slices.Contains(answerChannels, name) {
				t.Errorf("the %q envelope carries the channel %q, which is in no entity mapper: %s",
					envelope, name, raw)
				continue
			}
			var list []any
			if err := json.Unmarshal(value, &list); err != nil || list == nil {
				t.Errorf("%s.%s = %s, want a JSON array", envelope, name, value)
			}
		}
	}
}

/* ── helpers ─────────────────────────────────────────────────────────────── */

// skillErrorEntry is one entry of the import and fork answer's `errors.skills`.
type skillErrorEntry struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	Msg   string `json:"msg"`
}

func decodeSkillErrors(t *testing.T, recorder *httptest.ResponseRecorder) []skillErrorEntry {
	t.Helper()
	var answer struct {
		Errors struct {
			Skills []skillErrorEntry `json:"skills"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer %q: %v", recorder.Body.String(), err)
	}
	return answer.Errors.Skills
}

// skillImportBody is one skill and one agent whose single version references
// it. It is the smallest document that exercises the attachment.
func skillImportBody(importUUID, versionName string) []any {
	return []any{
		map[string]any{
			"entity": "skills", "import_uuid": importUUID,
			"name": "fixture skill", "description": "seeded skill",
			"versions": []any{map[string]any{"name": "base", "instructions": "read this"}},
		},
		map[string]any{
			"entity": "agents", "import_uuid": "ag-1", "name": "fixture agent",
			"versions": []any{map[string]any{
				"name": "latest", "agent_type": "openai", "instructions": "do the thing",
				"skills": []any{map[string]any{
					"import_uuid": importUUID, "version_name": versionName, "entity_type": "agent",
				}},
			}},
		},
	}
}

// skillRoundTripImportBody flattens the export document the way the import
// wizard does: every top-level array becomes entities that carry
// `entity: "<the array's key>"`, with `applications` renamed to `agents`
// (apps/elitea-ui .../importWizardParser.helpers.js, prepareImportWizardData).
func skillRoundTripImportBody(t *testing.T, document map[string]any) []any {
	t.Helper()
	entities := make([]any, 0)
	for _, key := range []string{"applications", "toolkits", "skills"} {
		list, ok := document[key].([]any)
		if !ok {
			t.Fatalf("the export carries no %s array: %v", key, document)
		}
		name := key
		if key == "applications" {
			name = "agents"
		}
		for _, raw := range list {
			entry, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("the export carries a %s entry that is not an object: %v", key, raw)
			}
			entry["entity"] = name
			entities = append(entities, entry)
		}
	}
	return entities
}

// forkSkillDo serves the fork route with a principal, the way the router does.
func forkSkillDo(t *testing.T, handler *eliteacore.Handler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			user := auth.User{ID: strconv.Itoa(importLinkPrincipal)}
			next.ServeHTTP(w, request.WithContext(auth.ContextWithUser(request.Context(), user)))
		})
	})
	router.Post("/elitea_core/fork/prompt_lib/{projectID}", handler.Fork)

	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal the fork body: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/elitea_core/fork/prompt_lib/1", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// assertExportedSkillDocument states what the export must carry before an
// import can be blamed for anything.
func assertExportedSkillDocument(t *testing.T, document map[string]any) {
	t.Helper()
	exported, ok := document["skills"].([]any)
	if !ok || len(exported) != 2 {
		t.Fatalf("the export carries %v as its skills array, want the two attached skills", document["skills"])
	}
	// BOTH versions of the pinned skill must travel, because both are named by
	// an attachment. Everything the resolution assertions claim rests on this:
	// with one version in the document, every branch of the resolver lands on
	// the same row.
	// Two of the skill's THREE versions: the two an attachment names. The third
	// must not travel.
	assertExportedSkillVersions(t, exported, skillRoundTripPinnedName,
		map[string]string{"base": skillRoundTripBaseBody, skillRoundTripPinnedVName: skillRoundTripPinnedBody})
	assertExportedSkillVersions(t, exported, skillRoundTripLooseName,
		map[string]string{"base": skillRoundTripLooseBody})

	earlier := skillRoundTripVersionNamed(t, document, "earlier")
	latest := skillRoundTripVersionNamed(t, document, "latest")

	earlierRefs := assertExportedSkillReferences(t, earlier, 1)
	if name, _ := earlierRefs[0]["version_name"].(string); name != "base" {
		t.Errorf("the `earlier` reference names version %q, want %q", name, "base")
	}

	latestRefs := assertExportedSkillReferences(t, latest, 2)
	if name, _ := latestRefs[0]["version_name"].(string); name != skillRoundTripPinnedVName {
		t.Errorf("the first `latest` reference names version %q, want %q", name, skillRoundTripPinnedVName)
	}
	// The row named no version, so the reference must name none either.
	if _, named := latestRefs[1]["version_name"]; named {
		t.Errorf("the second `latest` reference names a version, and its row names none: %v", latestRefs[1])
	}
}

// assertExportedSkillReferences reads and checks one version entry's `skills`.
func assertExportedSkillReferences(t *testing.T, version map[string]any, want int) []map[string]any {
	t.Helper()
	raw, ok := version["skills"].([]any)
	if !ok || len(raw) != want {
		t.Fatalf("the exported version carries %v as its skill references, want %d", version["skills"], want)
	}
	references := make([]map[string]any, 0, len(raw))
	for index, entry := range raw {
		reference, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("skill reference %d is not an object: %v", index, entry)
		}
		// The reference must name the skill by `import_uuid` and never by the
		// source `skill_id`: that id belongs to the project the file came from.
		if importUUID, _ := reference["import_uuid"].(string); importUUID == "" {
			t.Errorf("skill reference %d names no import_uuid: %v", index, reference)
		}
		if _, leaked := reference["skill_id"]; leaked {
			t.Errorf("skill reference %d carries the source skill_id, which names no row in any other project: %v",
				index, reference)
		}
		// The column is part of the key the table is unique on and part of the
		// predicate the chat read matches on.
		if entityType, _ := reference["entity_type"].(string); entityType != "agent" {
			t.Errorf("skill reference %d carries entity_type %q, want %q", index, entityType, "agent")
		}
		references = append(references, reference)
	}
	return references
}

// assertExportedSkillVersions checks which versions of one exported skill
// travelled, and their bodies.
func assertExportedSkillVersions(t *testing.T, exported []any, skillName string, want map[string]string) {
	t.Helper()
	for _, entry := range exported {
		skill, _ := entry.(map[string]any)
		if name, _ := skill["name"].(string); name != skillName {
			continue
		}
		versions, _ := skill["versions"].([]any)
		got := map[string]string{}
		for _, raw := range versions {
			version, _ := raw.(map[string]any)
			versionName, _ := version["name"].(string)
			instructions, _ := version["instructions"].(string)
			got[versionName] = instructions
		}
		if len(got) != len(want) {
			t.Fatalf("the export carries %v for skill %q, want %v", got, skillName, want)
		}
		for versionName, instructions := range want {
			if got[versionName] != instructions {
				t.Errorf("the export carries version %q of skill %q as %q, want %q",
					versionName, skillName, got[versionName], instructions)
			}
		}
		return
	}
	t.Fatalf("the export carries no skill named %q", skillName)
}

// skillRoundTripVersionNamed reads one version entry of applications[0].
func skillRoundTripVersionNamed(t *testing.T, document map[string]any, name string) map[string]any {
	t.Helper()
	applications, _ := document["applications"].([]any)
	if len(applications) != 1 {
		t.Fatalf("the export carries %d applications, want 1: %v", len(applications), document)
	}
	application, _ := applications[0].(map[string]any)
	versions, _ := application["versions"].([]any)
	for _, raw := range versions {
		version, _ := raw.(map[string]any)
		if versionName, _ := version["name"].(string); versionName == name {
			return version
		}
	}
	t.Fatalf("the export carries no version named %q: %v", name, application)
	return nil
}

func assertAttachedSkills(t *testing.T, got, want []attachedSkill) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("the destination carries %d skill attachments, want %d: %+v", len(got), len(want), got)
	}
	for index, expected := range want {
		if got[index] != expected {
			t.Errorf("attachment %d = %+v, want %+v", index, got[index], expected)
		}
	}
}

// readAttachedSkills selects the attachments of one version through the joins,
// the predicate and the ordering of the chat read
// (internal/db/queries/agent_chat.sql:126-132), including its
// COALESCE(skill_version.instructions, ”) projection. A row that carries the
// wrong entity_type is invisible to that read, so selecting on it proves the
// column and the presence of the row in one statement.
func readAttachedSkills(t *testing.T, pool *pgxpool.Pool, versionID int) []attachedSkill {
	t.Helper()
	return readAttachedSkillRows(t, pool, `
WHERE mapping.entity_version_id = $1
  AND mapping.entity_type = 'agent'`, versionID)
}

// assertAttachedSkillsOfImportedVersion reads the attachments of ONE imported
// agent version, found by its name among the versions that are not the seeded
// source. It exists because reading every imported row at once cannot tell a
// correct phase-3 index from one that attached the whole set to a single
// version, or swapped two versions' sets.
func assertAttachedSkillsOfImportedVersion(
	t *testing.T, pool *pgxpool.Pool, seeded skillRoundTripSeed, versionName string, want []attachedSkill,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// ORDER BY, and a count beside it: exactly one imported version carries this
	// name today, and a QueryRow with neither would silently take whichever row
	// the planner returned first the moment a case imports the document twice.
	var versionID, matches int
	if err := pool.QueryRow(ctx, `
SELECT id, count(*) OVER () FROM p_1.application_versions
WHERE name = $1 AND id <> ALL($2)
ORDER BY id`,
		versionName, []int{seeded.earlierVersionID, seeded.latestVersionID}).Scan(&versionID, &matches); err != nil {
		t.Fatalf("the import wrote no version named %q: %v", versionName, err)
	}
	if matches != 1 {
		t.Fatalf("%d imported versions are named %q, so this assertion reads an arbitrary one", matches, versionName)
	}
	got := readAttachedSkills(t, pool, versionID)
	if len(got) != len(want) {
		t.Fatalf("the imported %q carries %d skill attachments, want %d: %+v",
			versionName, len(got), len(want), got)
	}
	for index, expected := range want {
		if got[index] != expected {
			t.Errorf("the imported %q attachment %d = %+v, want %+v", versionName, index, got[index], expected)
		}
	}
}

// readAttachedSkillsExcept reads every attachment that is NOT on one of the
// seeded source versions, which is the set a round trip or a fork wrote.
func readAttachedSkillsExcept(t *testing.T, pool *pgxpool.Pool, sourceVersionIDs ...int) []attachedSkill {
	t.Helper()
	return readAttachedSkillRows(t, pool, `
WHERE mapping.entity_version_id <> ALL($1)
  AND mapping.entity_type = 'agent'`, sourceVersionIDs)
}

func readAttachedSkillRows(t *testing.T, pool *pgxpool.Pool, predicate string, argument any) []attachedSkill {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := pool.Query(ctx, `
SELECT skill.name,
       COALESCE(version.name, ''),
       COALESCE(version.instructions, ''),
       mapping.entity_type
FROM p_1.entity_skill_mapping AS mapping
JOIN p_1.skills AS skill ON skill.id = mapping.skill_id
LEFT JOIN p_1.skill_versions AS version ON version.id = mapping.skill_version_id`+predicate+`
ORDER BY mapping.id`, argument)
	if err != nil {
		t.Fatalf("read the skill attachments: %v", err)
	}
	defer rows.Close()
	attachments := make([]attachedSkill, 0)
	for rows.Next() {
		var attachment attachedSkill
		if err := rows.Scan(&attachment.skillName, &attachment.versionName,
			&attachment.instructions, &attachment.entityType); err != nil {
			t.Fatalf("scan a skill attachment: %v", err)
		}
		attachments = append(attachments, attachment)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the skill attachments: %v", err)
	}
	return attachments
}

// assertImportedSkillTags proves the skill's own content travelled and not only
// its name. A copy that carried the name and dropped everything under it is the
// same class of silent loss this issue is about.
//
// It also pins the version SELECTION. The source skill has THREE versions and
// the attachments name two of them, so the document carries those two alone — a
// skill's unused version history says nothing about the agent in the file
// (export_import.py:266-274). The copy must therefore hold exactly two tagged
// versions, and not the three the source holds.
func assertImportedSkillTags(t *testing.T, pool *pgxpool.Pool, skillName string, sourceSkillID int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var tagged int
	if err := pool.QueryRow(ctx, `
SELECT count(*)
FROM p_1.skills AS skill
JOIN p_1.skill_versions AS version ON version.skill_id = skill.id
JOIN p_1.skill_version_tag_association AS association ON association.version_id = version.id
JOIN p_1.tags AS tag ON tag.id = association.tag_id
WHERE skill.name = $1 AND skill.id <> $2 AND tag.name = $3`,
		skillName, sourceSkillID, skillRoundTripTagName).Scan(&tagged); err != nil {
		t.Fatalf("read the tags of the imported skill: %v", err)
	}
	if tagged != 2 {
		t.Errorf("the imported copy of %q carries %d tagged versions, want the two the file names", skillName, tagged)
	}
	// Stated as its own assertion, because a count alone cannot say WHICH two.
	var unused int
	if err := pool.QueryRow(ctx, `
SELECT count(*)
FROM p_1.skills AS skill
JOIN p_1.skill_versions AS version ON version.skill_id = skill.id
WHERE skill.name = $1 AND skill.id <> $2 AND version.name = $3`,
		skillName, sourceSkillID, skillRoundTripUnusedVName).Scan(&unused); err != nil {
		t.Fatalf("look for the unused version of the imported skill: %v", err)
	}
	if unused != 0 {
		t.Errorf("the imported copy of %q carries the %q version, which no attachment names",
			skillName, skillRoundTripUnusedVName)
	}
}

// seedSkillRoundTripAgent writes one agent with TWO versions and two skills.
//
//	earlier -> skill one @ base       ("the base body of skill one")
//	latest  -> skill one @ reviewed   ("follow the steps")
//	           skill two @ NULL       (the column permits it; the import resolves
//	                                   it to that skill's own base)
//
// Both versions of skill one are therefore NAMED by an attachment, so the export
// carries both and a resolver that picks the wrong one changes an assertion. See
// the comment on skillRoundTripSeed for why one agent version cannot do this.
//
// `created_at` is set explicitly. The export orders versions by that column
// (`exportedVersionRows`), and two rows written in the same statement batch can
// take the same default timestamp — which would leave the document's version
// order, and every index-based assertion over it, up to the planner.
func seedSkillRoundTripAgent(t *testing.T, pool *pgxpool.Pool) skillRoundTripSeed {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// `applications.owner_id` is the owning PROJECT, and this row lives in p_1
	// (#533). The seed wrote the caller here, which is the shape the writers
	// had before the meaning was settled; tenant/0131 now refuses it with a
	// foreign key. The caller is the version author, which
	// seedSkillRoundTripVersion writes.
	var seed skillRoundTripSeed
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.applications (name, description, owner_id)
VALUES ('skill round trip agent', 'seeded', 1) RETURNING id`).Scan(&seed.applicationID); err != nil {
		t.Fatalf("seed application: %v", err)
	}
	seed.earlierVersionID = seedSkillRoundTripVersion(t, ctx, pool, seed.applicationID, "earlier", "2 minutes")
	seed.latestVersionID = seedSkillRoundTripVersion(t, ctx, pool, seed.applicationID, "latest", "1 minute")

	var tagID int
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.tags (name, data) VALUES ($1, '{}'::jsonb) RETURNING id`,
		skillRoundTripTagName).Scan(&tagID); err != nil {
		t.Fatalf("seed skill tag: %v", err)
	}

	seed.pinnedSkillID = seedRoundTripSkill(t, ctx, pool, skillRoundTripPinnedName, map[string]string{
		"base":                    skillRoundTripBaseBody,
		skillRoundTripPinnedVName: skillRoundTripPinnedBody,
		skillRoundTripUnusedVName: skillRoundTripUnusedBody,
	}, tagID)
	seed.looseSkillID = seedRoundTripSkill(t, ctx, pool, skillRoundTripLooseName, map[string]string{
		"base": skillRoundTripLooseBody,
	}, 0)

	seedSkillRoundTripAttachment(t, ctx, pool, seed.latestVersionID, seed.pinnedSkillID, skillRoundTripPinnedVName)
	seedSkillRoundTripAttachment(t, ctx, pool, seed.earlierVersionID, seed.pinnedSkillID, "base")
	// `skill_version_id` is nullable, and this is the row that proves it.
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.entity_skill_mapping (entity_version_id, entity_type, skill_id, skill_version_id)
VALUES ($1, 'agent', $2, NULL)`, seed.latestVersionID, seed.looseSkillID); err != nil {
		t.Fatalf("seed the unpinned skill attachment: %v", err)
	}
	return seed
}

func seedSkillRoundTripVersion(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	applicationID int, name, age string,
) int {
	t.Helper()
	var versionID int
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.application_versions
	(application_id, name, status, agent_type, instructions, welcome_message,
	 llm_settings, conversation_starters, author_id, meta, pipeline_settings, created_at)
VALUES ($1, $2, 'draft', 'openai', 'do the thing', 'hello',
	'{"model_name": "gpt-4o"}'::jsonb, '[]'::jsonb, $3, '{}'::jsonb, '{}'::jsonb,
	now() - $4::interval)
RETURNING id`, applicationID, name, importLinkPrincipal, age).Scan(&versionID); err != nil {
		t.Fatalf("seed application version %q: %v", name, err)
	}
	return versionID
}

// seedSkillRoundTripAttachment attaches one NAMED version of a skill to one
// agent version.
func seedSkillRoundTripAttachment(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	entityVersionID, skillID int, versionName string,
) {
	t.Helper()
	tag, err := pool.Exec(ctx, `
INSERT INTO p_1.entity_skill_mapping (entity_version_id, entity_type, skill_id, skill_version_id)
SELECT $1, 'agent', $2, id FROM p_1.skill_versions WHERE skill_id = $2 AND name = $3`,
		entityVersionID, skillID, versionName)
	if err != nil {
		t.Fatalf("seed the %q attachment of skill %d: %v", versionName, skillID, err)
	}
	// The SELECT form writes nothing when the version name does not exist, and
	// a seed that silently attached nothing would make every assertion below
	// measure an empty database.
	if tag.RowsAffected() != 1 {
		t.Fatalf("the %q attachment of skill %d wrote %d rows, want 1",
			versionName, skillID, tag.RowsAffected())
	}
}

// seedRoundTripSkill writes one skill with the named versions. A non-zero
// tagID tags every version of it.
func seedRoundTripSkill(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	name string, versions map[string]string, tagID int,
) int {
	t.Helper()
	var skillID int
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.skills (name, description, owner_id, author_id)
VALUES ($1, 'seeded', 1, $2) RETURNING id`, name, importLinkPrincipal).Scan(&skillID); err != nil {
		t.Fatalf("seed skill %q: %v", name, err)
	}
	// Sorted, so the ids the seed writes do not depend on map iteration order.
	for _, versionName := range sortedKeys(versions) {
		var versionID int
		if err := pool.QueryRow(ctx, `
INSERT INTO p_1.skill_versions (skill_id, name, instructions, author_id, status)
VALUES ($1, $2, $3, $4, 'published') RETURNING id`,
			skillID, versionName, versions[versionName], importLinkPrincipal).Scan(&versionID); err != nil {
			t.Fatalf("seed version %q of skill %q: %v", versionName, name, err)
		}
		if tagID != 0 {
			if _, err := pool.Exec(ctx, `
INSERT INTO p_1.skill_version_tag_association (version_id, tag_id) VALUES ($1, $2)`,
				versionID, tagID); err != nil {
				t.Fatalf("tag version %q of skill %q: %v", versionName, name, err)
			}
		}
	}
	return skillID
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
