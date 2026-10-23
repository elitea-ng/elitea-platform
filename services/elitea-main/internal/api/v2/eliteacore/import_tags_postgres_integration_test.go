package eliteacore_test

// Real-PostgreSQL coverage for AGENT TAGS across the import path.
//
// This is the sibling of fork_import_variables_postgres_integration_test.go,
// one key over in the same export file, and the store is simpler than the
// variables' two: a tag has exactly ONE store, and it is not a column.
//
//   - `p_<id>.tags` (id, name UNIQUE per schema, data jsonb) holds the tag,
//     and `p_<id>.application_version_tag_association (version_id, tag_id)`
//     joins it to the version (001_initial.sql:302-306, :362-366).
//
// Everything that writes a tag writes that pair — the interactive editor
// (applications/handler.go, replaceVersionTags, from UpdateVersion), the skill
// import (import_skills.go, importSkillVersionTags, into the sibling
// association table) and the FORK (handler.go, the `tags` block of its version
// loop) — and everything that reads one reads it back: the version-detail GET
// the editor reloads through (applications/handler.go, versionTagsOrEmpty), the
// publish validation that counts them (runPublishValidation) and the EXPORT
// that puts them in the document at all (export_import.go,
// exportedVersionTags).
//
// The IMPORT wrote NEITHER table. It read the `tags` key not at all, so an
// agent exported with its tags came back with none of them, and the document
// could not survive a second round trip. `meta` cannot stand in: the legacy
// `update_version` dumps with `exclude={'tags', 'variables', 'tools'}`, so no
// tag ever reaches that column.
//
// # Why two readers and not three
//
// The variables file asserts a THIRD surface, the runtime projection
// `application_version_details_json`. Tags do not reach it: that projection
// builds the key as the literal `'tags', '[]'::jsonb`
// (internal/db/queries/agent_chat.sql), so it answers an empty list for every
// agent on the platform however the agent was made — typed in by hand, forked
// or imported. A projection assertion here would therefore pin that hardcoded
// literal rather than anything the import does, and it would pass just as well
// against the unfixed import. The two readers below are the two a tag actually
// reaches.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	applicationsapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/applications"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
)

// TestImportedAgentTagsReachEveryReader posts the `tags` array an export
// writes and asserts the tags are visible to both readers a tag has.
func TestImportedAgentTagsReachEveryReader(t *testing.T) {
	pool := newImportLinkPool(t)
	seedVariableStoreProject(t, pool)

	recorder := importLinkDo(t, importLinkRouter(eliteacore.NewHandler(pool)), importTagsBody())
	if recorder.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}

	applicationID, versionID := onlyCopiedVersion(t, pool)
	want := map[string]string{
		"release":  `{"colour":"blue"}`,
		"internal": `null`,
	}

	assertStoredTags(t, pool, versionID, want)
	assertGetVersionTags(t, pool, applicationID, versionID, want)
}

// TestImportEchoReportsWhatItPersisted reads the answer's own
// `version_details`, which the wizard shows the user after the import.
//
// The map carried neither `variables` nor `tags` nor `meta`, while the sibling
// fork echo and the version-detail GET both carry all three. So the import
// UNDER-REPORTED its own write: a caller comparing the echo with the file it
// had just sent would conclude the import had dropped them, and after the
// repair above that conclusion would have been wrong.
func TestImportEchoReportsWhatItPersisted(t *testing.T) {
	pool := newImportLinkPool(t)
	seedVariableStoreProject(t, pool)

	recorder := importLinkDo(t, importLinkRouter(eliteacore.NewHandler(pool)), importTagsBody())
	if recorder.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}

	var answer struct {
		Result struct {
			Agents []struct {
				VersionDetails struct {
					Tags []struct {
						ID   int             `json:"id"`
						Name string          `json:"name"`
						Data json.RawMessage `json:"data"`
					} `json:"tags"`
					Variables []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"variables"`
					Meta map[string]any `json:"meta"`
				} `json:"version_details"`
			} `json:"agents"`
		} `json:"result"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the import answer %q: %v", recorder.Body.String(), err)
	}
	if len(answer.Result.Agents) != 1 {
		t.Fatalf("import returned %d agents, want 1: %s", len(answer.Result.Agents), recorder.Body.String())
	}
	details := answer.Result.Agents[0].VersionDetails

	gotTags := map[string]string{}
	for _, tag := range details.Tags {
		if tag.ID == 0 {
			t.Errorf("version_details.tags[%q] carries no id, so the echo cannot name the row it wrote", tag.Name)
		}
		gotTags[tag.Name] = canonicalJSON(t, string(tag.Data))
	}
	compareVariables(t, "version_details.tags", gotTags, map[string]string{
		"release":  `{"colour":"blue"}`,
		"internal": `null`,
	})

	gotVariables := map[string]string{}
	for _, variable := range details.Variables {
		gotVariables[variable.Name] = variable.Value
	}
	compareVariables(t, "version_details.variables", gotVariables,
		map[string]string{"topic": "release notes"})

	if details.Meta == nil {
		t.Fatalf("version_details carries no meta, so the echo omits the column it wrote: %s", recorder.Body.String())
	}
	if got, _ := details.Meta["category"].(string); got != "reporting" {
		t.Errorf("version_details.meta[\"category\"] = %q, want %q (all: %v)", got, "reporting", details.Meta)
	}
}

// TestImportKeepsTheDataOfATagTheProjectAlreadyHas pins the conflict rule.
//
// `tags.name` is UNIQUE per tenant schema, so ONE row is shared by every
// version that carries the name. Writing the file's `data` over an existing row
// would repaint that tag for every other agent in the project — a side effect
// of importing one file that no caller asks for. So the row keeps the data it
// has, exactly as replaceVersionTags and importSkillVersionTags leave it, and
// only the association is added.
func TestImportKeepsTheDataOfATagTheProjectAlreadyHas(t *testing.T) {
	pool := newImportLinkPool(t)
	seedVariableStoreProject(t, pool)
	roundTripExec(t, pool,
		`INSERT INTO p_1.tags (name, data) VALUES ('release', '{"colour":"green"}'::jsonb)`)

	recorder := importLinkDo(t, importLinkRouter(eliteacore.NewHandler(pool)), importTagsBody())
	if recorder.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}

	_, versionID := onlyCopiedVersion(t, pool)
	assertStoredTags(t, pool, versionID, map[string]string{
		"release":  `{"colour":"green"}`,
		"internal": `null`,
	})
	if count := importLinkCount(t, pool, `SELECT COUNT(*) FROM p_1.tags WHERE name = 'release'`); count != 1 {
		t.Errorf("p_1.tags holds %d rows named 'release', want the one shared row", count)
	}
}

/* ── helpers ───────────────────────────────────────────────────────────── */

// importTagsBody is one agent with one version that carries two tags, one
// variable and a meta object — the shape `exportedVersions` writes.
//
// The second tag carries NO `data` key, which is the shape a tag saved through
// the editor's control has, and it must import as readily as the first.
func importTagsBody() []map[string]any {
	return []map[string]any{
		{
			"entity":      "agents",
			"import_uuid": "ag-tags",
			"name":        "imported agent",
			"description": "seeded agent",
			"versions": []map[string]any{
				{
					"name":                "latest",
					"agent_type":          "openai",
					"instructions":        "summarise {{topic}}",
					"import_version_uuid": "ver-tags",
					"meta":                map[string]any{"category": "reporting"},
					"variables": []map[string]any{
						{"name": "topic", "value": "release notes"},
					},
					"tags": []map[string]any{
						{"name": "release", "data": map[string]any{"colour": "blue"}},
						{"name": "internal"},
					},
				},
			},
		},
	}
}

// assertStoredTags reads the association and the tag rows — the store the
// editor writes, the export reads and the publish validation counts.
func assertStoredTags(t *testing.T, pool *pgxpool.Pool, versionID int, want map[string]string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := pool.Query(ctx, `
SELECT tag.name, COALESCE(tag.data::text, 'null')
FROM p_1.application_version_tag_association AS association
JOIN p_1.tags AS tag ON tag.id = association.tag_id
WHERE association.version_id = $1
ORDER BY tag.name`, versionID)
	if err != nil {
		t.Fatalf("read p_1.application_version_tag_association: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var name, data string
		if err := rows.Scan(&name, &data); err != nil {
			t.Fatalf("scan a stored tag: %v", err)
		}
		got[name] = canonicalJSON(t, data)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the stored tags: %v", err)
	}
	compareVariables(t, "application_version_tag_association rows", got, want)
}

// assertGetVersionTags reads the version-detail GET the agent editor reloads
// through — the read #345 repaired, and the one a user sees the tag control
// filled from.
func assertGetVersionTags(t *testing.T, pool *pgxpool.Pool, applicationID, versionID int, want map[string]string) {
	t.Helper()
	router := chi.NewRouter()
	router.Get("/version/prompt_lib/{projectID}/{applicationID}/{versionID}",
		applicationsapi.NewHandler(nil, pool).GetVersion)

	request := httptest.NewRequest(http.MethodGet,
		versionPath("/version/prompt_lib/1", applicationID, versionID), nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET version status = %d, want %d, body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var answer struct {
		Tags []struct {
			ID   int             `json:"id"`
			Name string          `json:"name"`
			Data json.RawMessage `json:"data"`
		} `json:"tags"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the version detail %q: %v", recorder.Body.String(), err)
	}
	got := map[string]string{}
	for _, tag := range answer.Tags {
		if tag.ID == 0 {
			t.Errorf("GET version_details.tags[%q] carries no id", tag.Name)
		}
		got[tag.Name] = canonicalJSON(t, string(tag.Data))
	}
	compareVariables(t, "GET version_details.tags", got, want)
}

// canonicalJSON re-encodes a JSON text compactly, so an assertion compares the
// VALUE and not PostgreSQL's jsonb spacing or Go's key order.
func canonicalJSON(t *testing.T, text string) string {
	t.Helper()
	if text == "" {
		return "null"
	}
	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		t.Fatalf("decode the JSON %q: %v", text, err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("re-encode the JSON %q: %v", text, err)
	}
	return string(encoded)
}
