package repos

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CHATTING WITH A PUBLISHED AGENT FROM ANOTHER PROJECT.
//
// A conversation lives in ONE project and its participants may name an agent in
// another. `application_versions` is a per-project table, so the turn resolver —
// which runs inside the conversation's own tenant schema — used to demand that
// the participant's `entity_meta.project_id` be the conversation's project: an
// agent published into the catalogue could be ADDED to a conversation in another
// project and then answered every send with 422. The legacy product allowed that
// chat, using the published version, billed and authorised under the caller's
// project.
//
// The rule these cases pin is deliberately two-part, because either half alone
// is wrong:
//
//   - the participant's project must be the ONE public (catalogue) project. Any
//     other foreign project is the tenancy boundary and stays refused, and the
//     third case proves that a schema that exists and holds a perfectly good
//     PUBLISHED version is still refused when it is not the catalogue.
//   - the version must be `published`. A draft sitting in the catalogue project
//     is the moderator's working copy and was never offered to anyone, so the
//     second case refuses it.
//
// The fourth case is the reason the version is read from the CATALOGUE schema
// rather than trusted: every id inside a version's `tools` — the toolkit row,
// the credential reference in its settings, a nested agent's id pair — names a
// row in the project that version came from, while the freeze that runs next
// resolves them in the CALLER's project. The catalogue twin carries no tool
// mappings at all (internal/api/v2/eliteacore/catalog_mirror.go says why), so
// this refusal never fires for a real twin; it fires if that ever changes.

// catalogueTurnProject is the project the conversation lives in. It is the one
// the base fixture already seeds.
const catalogueTurnProject int64 = 1

// catalogueTurnCatalogueProject is the PUBLIC project in these cases — the
// schema that holds the published catalogue. It is a parameter of the
// repository rather than an environment read, so it can be a project the
// fixture makes rather than the deployment default.
const catalogueTurnCatalogueProject int32 = 2

// catalogueTurnStrangerProject is a third project that is NOT the catalogue and
// whose schema is fully built and holds a published version. It exists so the
// refusal below cannot be explained by a missing schema or a missing row.
const catalogueTurnStrangerProject int64 = 3

func TestPostgresCurrentApplicationTurnAdmitsAPublishedCatalogueAgent(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	seedCurrentAgentCatalogueSchemas(t, pool)

	repository, err := NewCurrentAgentStartRepository(pool, catalogueTurnCatalogueProject)
	if err != nil {
		t.Fatal(err)
	}

	target, err := repository.ResolveCurrentApplication(
		t.Context(),
		catalogueTurnRequest(catalogueTurnPublishedParticipant),
	)
	if err != nil {
		t.Fatalf(
			"a conversation in project %d could not address the PUBLISHED agent in the catalogue project: %v",
			catalogueTurnProject, err,
		)
	}
	if target.ApplicationID != 131 || target.ApplicationVersionID != 141 {
		t.Fatalf(
			"the resolved target is not the catalogue pair: application %d version %d",
			target.ApplicationID, target.ApplicationVersionID,
		)
	}

	// The version details must come from the CATALOGUE project's schema. The
	// conversation's own schema holds an application_versions row with the same
	// id 141 and different instructions, seeded exactly so a resolver that read
	// the wrong schema would pass this test with the wrong document.
	var version struct {
		ID           int64  `json:"id"`
		Status       string `json:"status"`
		Instructions string `json:"instructions"`
		LLMSettings  struct {
			ModelProjectID int64 `json:"model_project_id"`
		} `json:"llm_settings"`
	}
	if err := json.Unmarshal(target.VersionDetails, &version); err != nil {
		t.Fatalf("the resolved version details are not a version document: %v", err)
	}
	if version.ID != 141 || version.Status != "published" {
		t.Fatalf("the resolved version is not the published catalogue row: %+v", version)
	}
	if version.Instructions != "catalogue instructions" {
		t.Fatalf(
			"the version details were read from the conversation's own schema, not the catalogue's: instructions = %q",
			version.Instructions,
		)
	}
	// A published agent's model is the CATALOGUE's. Publish refuses a version
	// whose model is not shared, and a shared model's `model_project_id` IS the
	// catalogue project, so both the clone and the twin carry that id
	// (internal/api/v2/eliteacore/handler.go copies `llm_settings` verbatim;
	// catalog_mirror.go does the same for the twin). The value has to survive
	// this read, because it is what makes the published agent answer with the
	// catalogue's model rather than whatever the caller's project happens to
	// have under the same name.
	if version.LLMSettings.ModelProjectID != int64(catalogueTurnCatalogueProject) {
		t.Fatalf(
			"the published agent's model project was not carried through: %d",
			version.LLMSettings.ModelProjectID,
		)
	}
}

func TestPostgresCurrentApplicationTurnRefusesADraftInTheCatalogueProject(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	seedCurrentAgentCatalogueSchemas(t, pool)

	repository, err := NewCurrentAgentStartRepository(pool, catalogueTurnCatalogueProject)
	if err != nil {
		t.Fatal(err)
	}

	_, err = repository.ResolveCurrentApplication(
		t.Context(),
		catalogueTurnRequest(catalogueTurnDraftParticipant),
	)
	if !errors.Is(err, agentexecutionapp.ErrUnsupportedCurrentAgentStart) {
		t.Fatalf(
			"a DRAFT version in the catalogue project was admitted; only a published version crosses a project line: %v",
			err,
		)
	}
}

func TestPostgresCurrentApplicationTurnRefusesAnotherProjectsPrivateAgent(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	seedCurrentAgentCatalogueSchemas(t, pool)

	repository, err := NewCurrentAgentStartRepository(pool, catalogueTurnCatalogueProject)
	if err != nil {
		t.Fatal(err)
	}

	_, err = repository.ResolveCurrentApplication(
		t.Context(),
		catalogueTurnRequest(catalogueTurnStrangerParticipant),
	)
	if !errors.Is(err, agentexecutionapp.ErrUnsupportedCurrentAgentStart) {
		t.Fatalf(
			"an agent in project %d — not the catalogue — was admitted into a project %d conversation; "+
				"that is the tenancy boundary: %v",
			catalogueTurnStrangerProject, catalogueTurnProject, err,
		)
	}
}

func TestPostgresCurrentApplicationTurnRefusesACatalogueVersionCarryingTools(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	seedCurrentAgentCatalogueSchemas(t, pool)

	repository, err := NewCurrentAgentStartRepository(pool, catalogueTurnCatalogueProject)
	if err != nil {
		t.Fatal(err)
	}

	_, err = repository.ResolveCurrentApplication(
		t.Context(),
		catalogueTurnRequest(catalogueTurnToolingParticipant),
	)
	if !errors.Is(err, agentexecutionapp.ErrUnsupportedCurrentAgentStart) {
		t.Fatalf(
			"a catalogue version carrying a toolkit reference was admitted; its toolkit id names a row "+
				"in the catalogue schema and the freeze would resolve it in the caller's: %v",
			err,
		)
	}
}

// The four participants the fixture adds to the SAME conversation. Each one is
// an `application` participant; they differ only in the project and version
// their `entity_meta` / `entity_settings` name, which is exactly the axis the
// admission rule reads.
const (
	catalogueTurnPublishedParticipant int64 = 25
	catalogueTurnDraftParticipant     int64 = 26
	catalogueTurnStrangerParticipant  int64 = 27
	catalogueTurnToolingParticipant   int64 = 28
)

func catalogueTurnRequest(participantID int64) agentexecutionapp.CurrentApplicationStartRequest {
	return agentexecutionapp.CurrentApplicationStartRequest{
		ProjectID:           catalogueTurnProject,
		ActorUserID:         11,
		ConversationUUID:    "10000000-0000-4000-8000-000000000031",
		TargetParticipantID: participantID,
		QuestionID:          "50000000-0000-4000-8000-000000000041",
		UserInput:           "which of you answered?",
	}
}

// seedCurrentAgentCatalogueSchemas builds the two foreign projects and hangs
// four cross-project participants on the conversation the continuation seed
// already made.
//
// Both foreign schemas are built the SAME way and hold a `published` version:
// the only difference between them is which id the repository is told is the
// catalogue. That is what makes the stranger's refusal a statement about the
// rule rather than about a missing fixture.
func seedCurrentAgentCatalogueSchemas(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	// A decoy in the CONVERSATION's own schema, carrying the same version id as
	// the catalogue's published row and different instructions. A resolver that
	// read the version out of the wrong schema would find this and look correct.
	if _, err := pool.Exec(t.Context(), `
INSERT INTO p_1.application_versions (
    id, application_id, name, status, author_id, uuid, llm_settings, instructions,
    conversation_starters, welcome_message, agent_type, meta, pipeline_settings
) VALUES (
    141, 131, 'decoy', 'published', 11, '80000000-0000-4000-8000-000000000141',
    '{"model_name":"decoy","model_project_id":1}'::jsonb,
    'the conversation project''s own row', '[]'::json, '', 'agent', '{}'::jsonb, '{}'::jsonb
);
INSERT INTO p_1.chat_participants (id, uuid, entity_name, entity_meta) VALUES
    (25, 'a0000000-0000-4000-8000-000000000041', 'application', '{"id":131,"project_id":2}'::jsonb),
    (26, 'a0000000-0000-4000-8000-000000000042', 'application', '{"id":131,"project_id":2}'::jsonb),
    (27, 'a0000000-0000-4000-8000-000000000043', 'application', '{"id":231,"project_id":3}'::jsonb),
    (28, 'a0000000-0000-4000-8000-000000000044', 'application', '{"id":131,"project_id":2}'::jsonb);
INSERT INTO p_1.chat_participant_mapping (
    conversation_id, participant_id, entity_settings
) VALUES
    (1, 25, '{"version_id":141,"variables":[]}'::jsonb),
    (1, 26, '{"version_id":142,"variables":[]}'::jsonb),
    (1, 27, '{"version_id":241,"variables":[]}'::jsonb),
    (1, 28, '{"version_id":143,"variables":[]}'::jsonb);`); err != nil {
		t.Fatal(err)
	}

	seedCurrentAgentForeignProjectSchema(t, pool, 2)
	seedCurrentAgentForeignProjectSchema(t, pool, 3)

	if _, err := pool.Exec(t.Context(), `
INSERT INTO p_2.application_versions (
    id, application_id, name, status, author_id, uuid, llm_settings, instructions,
    conversation_starters, welcome_message, agent_type, meta, pipeline_settings
) VALUES
    (141, 131, 'rel-1', 'published', 11, '80000000-0000-4000-8000-000000000241',
     '{"model_name":"shared","model_project_id":2}'::jsonb, 'catalogue instructions',
     '[]'::json, '', 'agent', '{}'::jsonb, '{}'::jsonb),
    (142, 131, 'latest', 'draft', 11, '80000000-0000-4000-8000-000000000242',
     '{"model_name":"shared","model_project_id":2}'::jsonb, 'the moderator''s working copy',
     '[]'::json, '', 'agent', '{}'::jsonb, '{}'::jsonb),
    (143, 131, 'rel-2', 'published', 11, '80000000-0000-4000-8000-000000000243',
     '{"model_name":"shared","model_project_id":2}'::jsonb, 'published, but wired to a toolkit',
     '[]'::json, '', 'agent', '{}'::jsonb, '{}'::jsonb);
INSERT INTO p_2.elitea_tools (id, type, name, description, settings, author_id, meta)
VALUES (151, 'aha', 'product', 'Aha product access',
        '{"selected_tools":["list_products"]}'::jsonb, 11, '{}'::jsonb);
INSERT INTO p_2.entity_tool_mapping (id, tool_id, entity_id, entity_version_id, entity_type, selected_tools)
VALUES (161, 151, 131, 143, 'agent', '["list_products"]'::jsonb);
INSERT INTO p_3.application_versions (
    id, application_id, name, status, author_id, uuid, llm_settings, instructions,
    conversation_starters, welcome_message, agent_type, meta, pipeline_settings
) VALUES (
    241, 231, 'rel-1', 'published', 11, '80000000-0000-4000-8000-000000000341',
    '{"model_name":"shared","model_project_id":3}'::jsonb, 'a stranger''s published agent',
    '[]'::json, '', 'agent', '{}'::jsonb, '{}'::jsonb
);`); err != nil {
		t.Fatal(err)
	}
}

// seedCurrentAgentForeignProjectSchema builds one more tenant schema holding the
// tables the version-details projection reads.
//
// It is written directly rather than run through the tenant migrations because
// the point of these cases is the version READ, not the schema history: what
// matters is that `p_<id>` exists, that `centry.project` lists the project (both
// are what tenant.BindProject checks before it will install a search path), and
// that the tables the projection touches are there with the shapes
// internal/infra/db/migrations/001_initial.sql gives them.
func seedCurrentAgentForeignProjectSchema(t *testing.T, pool *pgxpool.Pool, projectID int64) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), fmt.Sprintf(`
INSERT INTO centry.project (id, create_success, suspended) VALUES (%[1]d, TRUE, FALSE);
CREATE SCHEMA p_%[1]d;
CREATE TABLE p_%[1]d.applications (
    id SERIAL PRIMARY KEY,
    name VARCHAR(128) NOT NULL
);
CREATE TABLE p_%[1]d.application_versions (
    id SERIAL PRIMARY KEY,
    application_id INTEGER NOT NULL,
    name VARCHAR(128) NOT NULL,
    status VARCHAR NOT NULL,
    author_id INTEGER NOT NULL,
    uuid UUID NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL DEFAULT now(),
    llm_settings JSONB NOT NULL DEFAULT '{}'::jsonb,
    instructions VARCHAR,
    conversation_starters JSON NOT NULL DEFAULT '[]'::json,
    welcome_message VARCHAR NOT NULL DEFAULT '',
    agent_type VARCHAR NOT NULL,
    meta JSONB NOT NULL DEFAULT '{}'::jsonb,
    pipeline_settings JSONB NOT NULL DEFAULT '{}'::jsonb
);
CREATE TABLE p_%[1]d.application_variables (
    id SERIAL PRIMARY KEY,
    application_version_id INTEGER NOT NULL
        REFERENCES p_%[1]d.application_versions(id) ON DELETE CASCADE,
    name VARCHAR NOT NULL,
    value VARCHAR,
    created_at TIMESTAMP NOT NULL DEFAULT now(),
    updated_at TIMESTAMP
);
CREATE TABLE p_%[1]d.elitea_tools (
    id SERIAL PRIMARY KEY, created_at TIMESTAMP NOT NULL DEFAULT now(), updated_at TIMESTAMP,
    type VARCHAR NOT NULL, name VARCHAR(128), description VARCHAR(1024),
    settings JSONB NOT NULL, author_id INTEGER NOT NULL, meta JSONB NOT NULL
);
CREATE TABLE p_%[1]d.entity_tool_mapping (
    id SERIAL PRIMARY KEY, tool_id INTEGER NOT NULL, entity_id INTEGER NOT NULL,
    entity_version_id INTEGER NOT NULL, entity_type VARCHAR NOT NULL,
    selected_tools JSONB
);
CREATE TABLE p_%[1]d.skills (
    id SERIAL PRIMARY KEY,
    name VARCHAR(128) NOT NULL,
    description VARCHAR(2304) NOT NULL,
    owner_id INTEGER NOT NULL DEFAULT 1,
    author_id INTEGER NOT NULL DEFAULT 1,
    meta JSONB DEFAULT '{}'::jsonb
);
CREATE TABLE p_%[1]d.skill_versions (
    id SERIAL PRIMARY KEY,
    skill_id INTEGER NOT NULL REFERENCES p_%[1]d.skills(id) ON DELETE CASCADE,
    name VARCHAR(128) NOT NULL DEFAULT 'base',
    instructions TEXT NOT NULL,
    author_id INTEGER NOT NULL DEFAULT 1,
    meta JSONB DEFAULT '{}'::jsonb
);
CREATE TABLE p_%[1]d.entity_skill_mapping (
    id SERIAL PRIMARY KEY, entity_version_id INTEGER NOT NULL,
    entity_type VARCHAR(50) NOT NULL, skill_id INTEGER NOT NULL REFERENCES p_%[1]d.skills(id),
    skill_version_id INTEGER REFERENCES p_%[1]d.skill_versions(id)
);`, projectID)); err != nil {
		t.Fatal(err)
	}
}

// THE WRITE HALF. A resolve that admits a turn the INSERT then refuses answers
// the same 422 the resolve was fixed to stop answering, one statement later and
// in a different transaction — so the admission rule has to be the same on both
// sides, and these two cases hold it there.
//
// `InsertCurrentApplicationTurn` joins `application_versions` in the
// conversation's schema exactly as the resolve did, so the catalogue twin's row
// is invisible to it too. What replaces the join on that branch is a pair of
// comparisons against the participant's OWN stored ids, and the second case is
// what proves they are still made: a caller that named a different version than
// the participant's mapping does must still write nothing.

func TestPostgresCurrentApplicationTurnInsertAdmitsTheCatalogueParticipant(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	seedCurrentAgentCatalogueSchemas(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)

	turn := agentexecutionapp.CurrentApplicationTurn{
		ProjectID: catalogueTurnProject, ActorUserID: 11,
		TargetParticipantID: catalogueTurnPublishedParticipant,
		ApplicationID:       131, ApplicationVersionID: 141,
		ConversationUUID:  "10000000-0000-4000-8000-000000000031",
		QuestionID:        "20000000-0000-4000-8000-000000000051",
		QuestionItemID:    "30000000-0000-4000-8000-000000000051",
		ResponseMessageID: "40000000-0000-4000-8000-000000000051",
		QuestionMeta:      json.RawMessage(`{}`), UserInput: "which of you answered?",
	}
	if err := insertCurrentApplicationTurn(
		t.Context(), queries, "execution-catalogue-1", turn, catalogueTurnCatalogueProject,
	); err != nil {
		t.Fatalf("the turn addressed at the published catalogue twin was not written: %v", err)
	}
}

func TestPostgresCurrentApplicationTurnInsertRefusesAMismatchedCatalogueVersion(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	seedCurrentAgentCatalogueSchemas(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)

	// The participant's mapping names version 141. This names 142 — the draft —
	// which is the shape a caller could send if the two ids were taken on trust
	// once the version join could no longer make the comparison.
	turn := agentexecutionapp.CurrentApplicationTurn{
		ProjectID: catalogueTurnProject, ActorUserID: 11,
		TargetParticipantID: catalogueTurnPublishedParticipant,
		ApplicationID:       131, ApplicationVersionID: 142,
		ConversationUUID:  "10000000-0000-4000-8000-000000000031",
		QuestionID:        "20000000-0000-4000-8000-000000000052",
		QuestionItemID:    "30000000-0000-4000-8000-000000000052",
		ResponseMessageID: "40000000-0000-4000-8000-000000000052",
		QuestionMeta:      json.RawMessage(`{}`), UserInput: "which of you answered?",
	}
	err := insertCurrentApplicationTurn(
		t.Context(), queries, "execution-catalogue-2", turn, catalogueTurnCatalogueProject,
	)
	if !errors.Is(err, agentexecutionapp.ErrUnsupportedCurrentAgentStart) {
		t.Fatalf(
			"a turn naming a version the participant's mapping does not was written: %v",
			err,
		)
	}
}

func TestPostgresCurrentApplicationTurnInsertStillRefusesAStrangersProject(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	seedCurrentAgentCatalogueSchemas(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)

	turn := agentexecutionapp.CurrentApplicationTurn{
		ProjectID: catalogueTurnProject, ActorUserID: 11,
		TargetParticipantID: catalogueTurnStrangerParticipant,
		ApplicationID:       231, ApplicationVersionID: 241,
		ConversationUUID:  "10000000-0000-4000-8000-000000000031",
		QuestionID:        "20000000-0000-4000-8000-000000000053",
		QuestionItemID:    "30000000-0000-4000-8000-000000000053",
		ResponseMessageID: "40000000-0000-4000-8000-000000000053",
		QuestionMeta:      json.RawMessage(`{}`), UserInput: "which of you answered?",
	}
	err := insertCurrentApplicationTurn(
		t.Context(), queries, "execution-catalogue-3", turn, catalogueTurnCatalogueProject,
	)
	if !errors.Is(err, agentexecutionapp.ErrUnsupportedCurrentAgentStart) {
		t.Fatalf(
			"a turn addressed at an agent private to another project was written: %v",
			err,
		)
	}
}
