package eliteacore_test

// Real-PostgreSQL coverage for the export and import path — issue #505.
//
// The first case is the one the issue asks for first, because it is DATA LOSS
// and not only a hidden fault: an export followed by an import used to lose the
// tool selection with no message anywhere. The rest of the file proves the
// other faults of the same family, in both directions, by breaking the input
// or the database on purpose.
//
// No case asserts on a status code alone. Each reads the rows back.
//
// The pool, the migration corpus and the one recorded schema adjustment come
// from import_tool_link_postgres_integration_test.go — see its header.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// roundTripSelection is the value under test. It is a JSON ARRAY of tool names,
// which is what the route the agent editor's tool menu drives writes
// (internal/api/v2/toolkits/handler.go, selectedToolsPayload), what the column
// defaults to (internal/infra/db/migrations/001_initial.sql) and the only shape
// the chat turn's tool resolution counts (internal/db/queries/agent_chat.sql).
var roundTripSelection = []string{"get_issue", "create_issue"}

// TestExportImportRoundTripKeepsTheToolSelection is the acceptance test.
//
// It exports an agent that has a toolkit with a tool selection, imports the
// exported document into the same project the way the wizard does, and reads
// the new mapping's `selected_tools` column back out of the database.
//
// Before the repair the import read the key as `map[string]any`. An array fails
// that assertion, so the import wrote `{}` and answered 201. The agent came
// back with its toolkit attached and every tool unchecked, and nothing in the
// response, the log or the wizard said so.
func TestExportImportRoundTripKeepsTheToolSelection(t *testing.T) {
	pool := newImportLinkPool(t)
	handler := eliteacore.NewHandler(pool)

	seeded := seedRoundTripAgent(t, pool)
	document := exportRoundTrip(t, handler, seeded.applicationID, http.StatusOK)

	// The document must carry the selection it was seeded with. If the export
	// lost it, the import cannot be blamed for the loss.
	exportedSelection := roundTripExportedSelection(t, document)
	if !equalStrings(exportedSelection, roundTripSelection) {
		t.Fatalf("the export carried selected_tools %v, want %v", exportedSelection, roundTripSelection)
	}

	recorder := importLinkDo(t, importLinkRouter(handler), roundTripImportBody(t, document))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	answer := decodeImportLink(t, recorder)
	if len(answer.Errors.Agents) != 0 || len(answer.Errors.Toolkits) != 0 {
		t.Fatalf("the round trip reported errors: %s", recorder.Body.String())
	}

	// The row the import wrote, not the row the export read from.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stored string
	if err := pool.QueryRow(ctx, `
SELECT COALESCE(selected_tools::text, '')
FROM p_1.entity_tool_mapping
WHERE entity_version_id <> $1`, seeded.versionID).Scan(&stored); err != nil {
		t.Fatalf("the import wrote no tool link: %v", err)
	}
	var imported []string
	if err := json.Unmarshal([]byte(stored), &imported); err != nil {
		t.Fatalf("the import stored selected_tools = %s, which is not a list of tool names: %v", stored, err)
	}
	if !equalStrings(imported, roundTripSelection) {
		t.Errorf("the imported selection is %v, want the exported %v — the round trip lost it", imported, roundTripSelection)
	}
}

// TestImportKeepsAnObjectToolSelection guards the repair from the other side. A
// file written by an older installation may carry an object here, and the
// import must reproduce the document it is given rather than refuse it or
// replace it.
func TestImportKeepsAnObjectToolSelection(t *testing.T) {
	pool := newImportLinkPool(t)
	router := importLinkRouter(eliteacore.NewHandler(pool))

	recorder := importLinkDo(t, router, importLinkBody([]map[string]any{importLinkToolRef()}, "latest"))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	if stored := roundTripSelectedTools(t, pool); stored != `{"get_issue": true}` {
		t.Errorf("selected_tools = %q, want the object the file carried", stored)
	}
}

// TestImportGivesAnAbsentToolSelectionTheColumnDefault records the third case.
// An absent key means the file says nothing about the selection, and the value
// stored is the column's own default, `[]`. It used to be `{}`, which no reader
// of the column understands.
func TestImportGivesAnAbsentToolSelectionTheColumnDefault(t *testing.T) {
	pool := newImportLinkPool(t)
	router := importLinkRouter(eliteacore.NewHandler(pool))

	recorder := importLinkDo(t, router, importLinkBody(
		[]map[string]any{{"import_uuid": "tk-1"}}, "latest"))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	if stored := roundTripSelectedTools(t, pool); stored != `[]` {
		t.Errorf("selected_tools = %q, want the column default []", stored)
	}
}

/* ── the export refuses a lost read (#381, #439, #505) ──────────────────────
 *
 * Five reads served this route under `if err == nil`, dropped any row they
 * could not scan, and never read rows.Err(). Each case below breaks exactly one
 * of them and requires the export to refuse. Before the repair every one of
 * them answered 200 with a document that was missing that part, which the
 * export button then saved as the agent's backup file.
 */

func TestExportRefusesALostRead(t *testing.T) {
	pool := newImportLinkPool(t)
	handler := eliteacore.NewHandler(pool)
	seeded := seedRoundTripAgent(t, pool)

	// One database and one agent for all five cases. Each case renames the one
	// column its read names and puts it back, and the case before it proves the
	// export answers 200 on the same data. The refusal is therefore the effect
	// of the break and of nothing else.
	for _, testCase := range []struct {
		name    string
		table   string
		column  string
		renamed string
	}{
		{"toolkits", "elitea_tools", "settings", "settings_moved"},
		{"versions", "application_versions", "welcome_message", "welcome_message_moved"},
		{"version tools", "entity_tool_mapping", "selected_tools", "selected_tools_moved"},
		{"version variables", "application_variables", "value", "value_moved"},
		{"version tags", "tags", "data", "data_moved"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
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
			if message, _ := document["error"].(string); message == "" {
				t.Errorf("a refused export must name the reason, got %v", document)
			}
		})
	}
}

// TestExportRefusesAnUnreadableRow breaks the SCAN and not the query. The
// variables view below hands the read a `text[]` where it expects text, so the
// query runs, the rows arrive, and the scan is the statement that fails. The
// old code answered that with `continue`, so the version came back with one
// variable fewer and the export still answered 200.
func TestExportRefusesAnUnreadableRow(t *testing.T) {
	pool := newImportLinkPool(t)
	handler := eliteacore.NewHandler(pool)
	seeded := seedRoundTripAgent(t, pool)

	roundTripExec(t, pool, `ALTER TABLE p_1.application_variables RENAME TO application_variables_store`)
	roundTripExec(t, pool, `
CREATE VIEW p_1.application_variables AS
SELECT id, application_version_id, ARRAY[name] AS name, value
FROM p_1.application_variables_store`)

	document := exportRoundTrip(t, handler, seeded.applicationID, http.StatusInternalServerError)
	if _, hasApplications := document["applications"]; hasApplications {
		t.Errorf("a refused export must carry no applications key, got %v", document)
	}
}

// TestExportRefusesAReadThatStopsPartWayThrough is the third of the three. The
// function below raises on the SECOND variable only, so the result set starts
// and then fails. `rows.Next` reports that by returning false, exactly as it
// does at the end of the rows, and the fault is only visible through
// `rows.Err`, which nothing read.
//
// The export must refuse. Which of the two statements reports the fault —
// `Query` or `Err` — depends on how much of the result set the server had
// already sent, and the test does not depend on that: before the repair
// NEITHER was read, and the answer was 200 with one variable.
func TestExportRefusesAReadThatStopsPartWayThrough(t *testing.T) {
	pool := newImportLinkPool(t)
	handler := eliteacore.NewHandler(pool)
	seeded := seedRoundTripAgent(t, pool)

	roundTripExec(t, pool, `
INSERT INTO p_1.application_variables (application_version_id, name, value)
VALUES ($1, 'second', 'trip')`, seeded.versionID)
	roundTripExec(t, pool, `
CREATE FUNCTION p_1.trip_on(value text) RETURNS text LANGUAGE plpgsql AS $$
BEGIN
	IF value = 'trip' THEN
		RAISE EXCEPTION 'the variable read stopped part way through';
	END IF;
	RETURN value;
END
$$`)
	roundTripExec(t, pool, `ALTER TABLE p_1.application_variables RENAME TO application_variables_store`)
	roundTripExec(t, pool, `
CREATE VIEW p_1.application_variables AS
SELECT id, application_version_id, name, p_1.trip_on(value) AS value
FROM p_1.application_variables_store`)

	document := exportRoundTrip(t, handler, seeded.applicationID, http.StatusInternalServerError)
	if _, hasApplications := document["applications"]; hasApplications {
		t.Errorf("a refused export must carry no applications key, got %v", document)
	}
}

/* ── the import refuses a body it cannot read (#505) ────────────────────── */

// TestImportRefusesABodyItCannotRead covers the io.ReadAll error. The request
// body reports a fault half way through, which is what a connection lost during
// an upload looks like. The error was discarded, so the import answered 201
// with an empty result and the wizard marked every entity green.
func TestImportRefusesABodyItCannotRead(t *testing.T) {
	pool := newImportLinkPool(t)
	router := importLinkRouter(eliteacore.NewHandler(pool))

	request := httptest.NewRequest(http.MethodPost, "/elitea_core/import_wizard/prompt_lib/1",
		io.MultiReader(bytes.NewReader([]byte(`[{"entity":"agents",`)), failingReader{}))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	roundTripRequireEmpty(t, pool)
}

// TestImportRefusesABodyItCannotParse covers both discarded json.Unmarshal
// calls and the whitespace fault beside them.
func TestImportRefusesABodyItCannotParse(t *testing.T) {
	// One database for all four cases: a refused body writes nothing, so no
	// case can leave a row that changes what the next one measures.
	pool := newImportLinkPool(t)
	router := importLinkRouter(eliteacore.NewHandler(pool))

	for _, testCase := range []struct {
		name string
		body string
	}{
		{"a truncated array", `[{"entity":"agents","name":"a"`},
		{"a truncated object", `{"applications":[{"name":"a"`},
		{"a body that is neither", `"just a string"`},
		{"an object with no applications key", `{"agents":[{"name":"a"}]}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := importLinkDoRaw(t, router, testCase.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("import status = %d, want %d, body = %s",
					recorder.Code, http.StatusBadRequest, recorder.Body.String())
			}
			roundTripRequireEmpty(t, pool)
		})
	}
}

// TestImportReadsAnArrayThatStartsWithWhitespace is the other half of the same
// repair. The array branch tested bodyBytes[0], so a leading newline sent a
// valid export file to the object branch, where it failed to decode and took
// the silent 201. It must now import.
func TestImportReadsAnArrayThatStartsWithWhitespace(t *testing.T) {
	pool := newImportLinkPool(t)
	router := importLinkRouter(eliteacore.NewHandler(pool))

	encoded, err := json.Marshal(importLinkBody([]map[string]any{importLinkToolRef()}, "latest"))
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	recorder := importLinkDoRaw(t, router, "\n  "+string(encoded))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.applications`); count != 1 {
		t.Errorf("applications rows = %d, want the imported agent", count)
	}
}

// TestImportReportsAnEntryItCannotRead covers the two `if !ok { continue }`
// drops. Neither the result nor the errors named the dropped entry, so the
// wizard showed the entity as neither imported nor failed and the import
// answered 201.
func TestImportReportsAnEntryItCannotRead(t *testing.T) {
	pool := newImportLinkPool(t)
	router := importLinkRouter(eliteacore.NewHandler(pool))

	recorder := importLinkDo(t, router, []any{
		"not an entity",
		map[string]any{
			"entity": "agents", "name": "fixture agent",
			"versions": []any{"not a version"},
		},
	})
	// 207 and not 400: the agent ROW was written before its version failed, so
	// the handler counts one success. That the row survives with no version at
	// all is a separate defect, reported on #505 and not repaired here — the
	// status rule it feeds is the one pull request #499 set.
	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusMultiStatus, recorder.Body.String())
	}

	answer := decodeImportLink(t, recorder)
	if len(answer.Errors.Agents) != 2 {
		t.Fatalf("errors.agents = %+v, want the dropped entry and the dropped version", answer.Errors.Agents)
	}
	// The wizard marks an entity by this index and by nothing else.
	if answer.Errors.Agents[0].Index != 0 {
		t.Errorf("the entry error index = %d, want 0", answer.Errors.Agents[0].Index)
	}
	if answer.Errors.Agents[1].Index != 1 {
		t.Errorf("the version error index = %d, want 1", answer.Errors.Agents[1].Index)
	}
	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.application_versions`); count != 0 {
		t.Errorf("application_versions rows = %d, want 0", count)
	}
}

// TestImportReportsAColumnOfTheWrongType covers the five silent empty defaults.
// Each of these keys used to be replaced with an empty value when it was not
// the type the handler expected, and the import answered 201: an agent with no
// model settings, or a toolkit that can reach nothing.
func TestImportReportsAColumnOfTheWrongType(t *testing.T) {
	// One database for all four cases. No case may write a version or a
	// toolkit, and the assertion at the end of each one states that, so a case
	// cannot hide behind the case before it.
	pool := newImportLinkPool(t)
	router := importLinkRouter(eliteacore.NewHandler(pool))

	for _, testCase := range []struct {
		name     string
		entities []any
		message  string
		channel  string
	}{
		{
			name: "llm_settings that is not an object",
			entities: []any{map[string]any{
				"entity": "agents", "name": "fixture agent",
				"versions": []any{map[string]any{"name": "latest", "llm_settings": []any{"gpt"}}},
			}},
			message: "llm_settings must be a JSON object",
			channel: "agents",
		},
		{
			name: "meta that is not an object",
			entities: []any{map[string]any{
				"entity": "agents", "name": "fixture agent",
				"versions": []any{map[string]any{"name": "latest", "meta": "icon"}},
			}},
			message: "meta must be a JSON object",
			channel: "agents",
		},
		{
			name: "conversation_starters that is not an array",
			entities: []any{map[string]any{
				"entity": "agents", "name": "fixture agent",
				"versions": []any{map[string]any{"name": "latest", "conversation_starters": "hello"}},
			}},
			message: "conversation_starters must be a JSON array",
			channel: "agents",
		},
		{
			name: "toolkit settings that is not an object",
			entities: []any{map[string]any{
				"entity": "toolkits", "import_uuid": "tk-1",
				"name": "fixture toolkit", "type": "github",
				"settings": "https://example.invalid",
			}},
			message: "settings must be a JSON object",
			channel: "toolkits",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := importLinkDo(t, router, testCase.entities)
			if recorder.Code == http.StatusCreated {
				t.Fatalf("import reported success while the column was refused: %s", recorder.Body.String())
			}
			answer := decodeImportLink(t, recorder)
			reported := answer.Errors.Agents
			if testCase.channel == "toolkits" {
				reported = answer.Errors.Toolkits
			}
			if len(reported) != 1 {
				t.Fatalf("errors.%s = %+v, want exactly the rejected column", testCase.channel, reported)
			}
			if !bytes.Contains([]byte(reported[0].Msg), []byte(testCase.message)) {
				t.Errorf("reported msg = %q, want it to name %q", reported[0].Msg, testCase.message)
			}
			// The wrong value must not have been written under any name. The
			// `applications` table is not in this list: the agent row is
			// written before its versions are, and it survives a refused
			// version. See TestImportReportsAnEntryItCannotRead.
			for _, table := range []string{"application_versions", "elitea_tools"} {
				if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.`+table); count != 0 {
					t.Errorf("%s rows = %d, want 0", table, count)
				}
			}
		})
	}
}

/* ── helpers ───────────────────────────────────────────────────────────── */

type roundTripSeed struct {
	applicationID int
	versionID     int
	toolID        int
}

// seedRoundTripAgent writes one agent with one version, one toolkit, one tool
// link that carries a real selection, one variable and one tag.
func seedRoundTripAgent(t *testing.T, pool *pgxpool.Pool) roundTripSeed {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// `applications.owner_id` is the owning PROJECT, and this row lives in p_1
	// (#533). The seed wrote the caller here, which tenant/0131 now refuses
	// with a foreign key to centry.project. The caller is the version author,
	// which the statement below writes.
	var seed roundTripSeed
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.applications (name, description, owner_id)
VALUES ('round trip agent', 'seeded', 1) RETURNING id`).Scan(&seed.applicationID); err != nil {
		t.Fatalf("seed application: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.application_versions
	(application_id, name, status, agent_type, instructions, welcome_message,
	 llm_settings, conversation_starters, author_id, meta, pipeline_settings)
VALUES ($1, 'latest', 'draft', 'openai', 'do the thing', 'hello',
	'{"model_name": "gpt-4o"}'::jsonb, '["start here"]'::jsonb, $2, '{}'::jsonb, '{}'::jsonb)
RETURNING id`, seed.applicationID, importLinkPrincipal).Scan(&seed.versionID); err != nil {
		t.Fatalf("seed application version: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.elitea_tools (name, type, settings, owner_id, author_id, description, meta)
VALUES ('round trip toolkit', 'github', '{"url": "https://example.invalid"}'::jsonb, 1, $1, 'seeded', '{}'::jsonb)
RETURNING id`, importLinkPrincipal).Scan(&seed.toolID); err != nil {
		t.Fatalf("seed toolkit: %v", err)
	}
	selection, err := json.Marshal(roundTripSelection)
	if err != nil {
		t.Fatalf("marshal the seeded selection: %v", err)
	}
	roundTripExec(t, pool, `
INSERT INTO p_1.entity_tool_mapping (entity_version_id, entity_id, entity_type, tool_id, selected_tools)
VALUES ($1, $2, 'application', $3, $4::jsonb)`,
		seed.versionID, seed.applicationID, seed.toolID, string(selection))
	roundTripExec(t, pool, `
INSERT INTO p_1.application_variables (application_version_id, name, value)
VALUES ($1, 'first', 'value')`, seed.versionID)
	var tagID int
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.tags (name, data) VALUES ('seeded tag', '{}'::jsonb) RETURNING id`).Scan(&tagID); err != nil {
		t.Fatalf("seed tag: %v", err)
	}
	roundTripExec(t, pool, `
INSERT INTO p_1.application_version_tag_association (version_id, tag_id) VALUES ($1, $2)`,
		seed.versionID, tagID)
	return seed
}

func roundTripExec(t *testing.T, pool *pgxpool.Pool, statement string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, statement, args...); err != nil {
		t.Fatalf("run %q: %v", statement, err)
	}
}

// exportRoundTrip serves the export route and requires the status the case
// expects. It returns the decoded document either way, because a refusal has to
// be read as well.
func exportRoundTrip(t *testing.T, handler *eliteacore.Handler, applicationID, wantStatus int) map[string]any {
	t.Helper()
	router := chi.NewRouter()
	router.Get("/elitea_core/export_import/prompt_lib/{projectID}/{entityID}", handler.ExportImportGet)

	target := fmt.Sprintf("/elitea_core/export_import/prompt_lib/1/%d", applicationID)
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request = request.WithContext(auth.ContextWithUser(request.Context(),
		auth.User{ID: strconv.Itoa(importLinkPrincipal)}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != wantStatus {
		t.Fatalf("export status = %d, want %d, body = %s", recorder.Code, wantStatus, recorder.Body.String())
	}
	var document map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatalf("decode export %q: %v", recorder.Body.String(), err)
	}
	return document
}

// roundTripImportBody flattens the export document the way the import wizard
// does: `applications` entries become `entity: "agents"` and every other array
// keeps its own key as the entity name
// (apps/elitea-ui .../importWizardParser.helpers.js, prepareImportWizardData).
func roundTripImportBody(t *testing.T, document map[string]any) []any {
	t.Helper()
	entities := make([]any, 0)
	for _, key := range []string{"applications", "toolkits"} {
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

// roundTripExportedSelection reads applications[0].versions[0].tools[0].selected_tools.
func roundTripExportedSelection(t *testing.T, document map[string]any) []string {
	t.Helper()
	applications, _ := document["applications"].([]any)
	if len(applications) != 1 {
		t.Fatalf("the export carries %d applications, want 1: %v", len(applications), document)
	}
	application, _ := applications[0].(map[string]any)
	versions, _ := application["versions"].([]any)
	if len(versions) != 1 {
		t.Fatalf("the export carries %d versions, want 1: %v", len(versions), application)
	}
	version, _ := versions[0].(map[string]any)
	tools, _ := version["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("the export carries %d tool references, want 1: %v", len(tools), version)
	}
	tool, _ := tools[0].(map[string]any)
	raw, _ := tool["selected_tools"].([]any)
	selection := make([]string, 0, len(raw))
	for _, entry := range raw {
		name, ok := entry.(string)
		if !ok {
			t.Fatalf("the export carries a tool name that is not a string: %v", entry)
		}
		selection = append(selection, name)
	}
	return selection
}

func roundTripSelectedTools(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stored string
	if err := pool.QueryRow(ctx, `
SELECT COALESCE(selected_tools::text, '') FROM p_1.entity_tool_mapping`).Scan(&stored); err != nil {
		t.Fatalf("the import wrote no tool link: %v", err)
	}
	return stored
}

// roundTripRequireEmpty states what a refused import must leave behind.
func roundTripRequireEmpty(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, table := range []string{"applications", "application_versions", "elitea_tools", "entity_tool_mapping"} {
		if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.`+table); count != 0 {
			t.Errorf("%s rows = %d, want 0 — a refused import must write nothing", table, count)
		}
	}
}

func importLinkDoRaw(t *testing.T, router chi.Router, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/elitea_core/import_wizard/prompt_lib/1",
		bytes.NewReader([]byte(body)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// failingReader reports a fault instead of the rest of the body, which is what
// a connection lost during an upload looks like to io.ReadAll.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("the connection was lost")
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
