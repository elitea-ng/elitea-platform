package eliteacore_test

// Real-PostgreSQL coverage for AGENT VARIABLES across the copy paths.
//
// A variable has TWO stores in this repository, and until this file nothing
// asserted that a single agent reaches every reader through both:
//
//   - `p_<id>.application_variables`, a real table (001_initial.sql). The
//     interactive editor writes it (applications/handler.go,
//     replaceVersionVariables), the version-detail GET the editor reloads
//     through READS it (fetchVersionDetails), the runtime-facing
//     `GetVersionExpanded` READS it, and the export READS it
//     (export_import.go, exportedVersionVariables). It is also the ONLY store
//     the legacy product has: `ApplicationVersion.variables` is a relationship
//     to `ApplicationVariable`, and `update_version` dumps the payload with
//     `exclude={'tags', 'variables', 'tools'}` so a variable can never reach
//     `meta` there (legacy/plugins/elitea_core/utils/application_utils.py,
//     utils/create_utils.py).
//
//   - `application_versions.meta -> 'variables'`, a mirror the Go create/update
//     paths fold in so their own 201 echo can report what was saved
//     (versionFromBody, versionDetailsResponse). The RUNTIME projection
//     `application_version_details_json` reads that mirror, and nothing else.
//
// The two copy paths write one store each, so the mirror and the table
// disagree the moment an agent is not typed in by hand:
//
//   - FORK writes the table and copies `meta` verbatim from the body. An export
//     of an agent whose variables live only in the table — every legacy agent,
//     and every agent this platform imported — carries no `meta.variables`, so
//     the forked agent's rows exist, the GET returns them, and the runtime
//     projection answers `[]`: every authored `{{name}}` reaches the model
//     unsubstituted.
//
//   - IMPORT writes NEITHER. It never reads the file's `variables` array at
//     all, so the forked/exported variables are dropped on the floor; only
//     whatever the file's `meta` happened to carry survives, and that is the
//     one store the editor, the GET and the next export do not read.
//
// Every case below reads all three surfaces of one agent: the stored rows, the
// HTTP GET, and the projection a worker is actually served
// (`ResolveCurrentApplicationVersionDetails`, the query behind
// `/runtime-context/applications/{application_id}/versions/{version_id}`).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	applicationsapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/applications"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenant"
)

// TestForkedAgentVariablesReachEveryReader forks an agent whose variables live
// where every other reader keeps them — the table — and asserts the copy is
// visible to all three readers.
//
// `forkBody` carries NO `meta` key, which is the point: it is the shape an
// export of a legacy agent has, and the shape the fork button therefore sends
// most of the time.
func TestForkedAgentVariablesReachEveryReader(t *testing.T) {
	pool := newImportLinkPool(t)
	seedVariableStoreProject(t, pool)

	recorder := forkDo(t, forkRouter(eliteacore.NewHandler(pool), true), forkBody("api_token"))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("fork status = %d, want %d, body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}

	applicationID, versionID := onlyCopiedVersion(t, pool)
	want := map[string]string{"api_token": "secret-token"}

	assertStoredVariables(t, pool, versionID, want)
	assertGetVersionVariables(t, pool, applicationID, versionID, want)
	assertProjectedVariables(t, pool, applicationID, versionID, want)
}

// TestImportedAgentVariablesReachEveryReader is the same assertion for the
// import path, which is separately reachable: the wizard posts an export file
// to `/elitea_core/import_wizard/prompt_lib/{projectID}`, and the export half
// of that round trip writes the `variables` array this body carries
// (export_import.go, exportedVersionVariables).
func TestImportedAgentVariablesReachEveryReader(t *testing.T) {
	pool := newImportLinkPool(t)
	seedVariableStoreProject(t, pool)

	recorder := importLinkDo(t, importLinkRouter(eliteacore.NewHandler(pool)), []map[string]any{
		{
			"entity":      "agents",
			"import_uuid": "ag-vars",
			"name":        "imported agent",
			"description": "seeded agent",
			"versions": []map[string]any{
				{
					"name":                "latest",
					"agent_type":          "openai",
					"instructions":        "summarise {{topic}}",
					"import_version_uuid": "ver-vars",
					"variables": []map[string]any{
						{"name": "topic", "value": "release notes"},
					},
				},
			},
		},
	})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}

	applicationID, versionID := onlyCopiedVersion(t, pool)
	want := map[string]string{"topic": "release notes"}

	assertStoredVariables(t, pool, versionID, want)
	assertGetVersionVariables(t, pool, applicationID, versionID, want)
	assertProjectedVariables(t, pool, applicationID, versionID, want)
}

/* ── helpers ───────────────────────────────────────────────────────────── */

// seedVariableStoreProject writes the `centry.project` row `tenant.BindProject`
// validates before it will install a tenant search path. The migration corpus
// creates the schema but seeds no project, and the projection assertion below
// resolves its tables through that search path exactly as the runtime does.
func seedVariableStoreProject(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	roundTripExec(t, pool, `
INSERT INTO centry.project (id, name, owner_id, create_success)
VALUES (1, 'variable store fixture', $1, TRUE)
ON CONFLICT (id) DO NOTHING`, importLinkPrincipal)
}

// onlyCopiedVersion returns the single application/version the copy wrote. Both
// cases copy exactly one agent with exactly one version, so more than one row
// means the case built something other than what it asserts on.
func onlyCopiedVersion(t *testing.T, pool *pgxpool.Pool) (applicationID, versionID int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := pool.Query(ctx, `
SELECT application_id, id FROM p_1.application_versions ORDER BY id`)
	if err != nil {
		t.Fatalf("read the copied versions: %v", err)
	}
	defer rows.Close()
	found := 0
	for rows.Next() {
		if err := rows.Scan(&applicationID, &versionID); err != nil {
			t.Fatalf("scan a copied version: %v", err)
		}
		found++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the copied versions: %v", err)
	}
	if found != 1 {
		t.Fatalf("p_1.application_versions holds %d rows, want exactly the one copied version", found)
	}
	return applicationID, versionID
}

// assertStoredVariables reads `application_variables`, the store the export and
// both HTTP reads are built on.
func assertStoredVariables(t *testing.T, pool *pgxpool.Pool, versionID int, want map[string]string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := pool.Query(ctx, `
SELECT name, COALESCE(value, '') FROM p_1.application_variables
WHERE application_version_id = $1 ORDER BY id`, versionID)
	if err != nil {
		t.Fatalf("read p_1.application_variables: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			t.Fatalf("scan a stored variable: %v", err)
		}
		got[name] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the stored variables: %v", err)
	}
	compareVariables(t, "application_variables rows", got, want)
}

// assertGetVersionVariables reads the version-detail GET the agent editor
// reloads through, and the API a caller reads an agent back with.
func assertGetVersionVariables(t *testing.T, pool *pgxpool.Pool, applicationID, versionID int, want map[string]string) {
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
		Variables []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"variables"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the version detail %q: %v", recorder.Body.String(), err)
	}
	got := map[string]string{}
	for _, variable := range answer.Variables {
		got[variable.Name] = variable.Value
	}
	compareVariables(t, "GET version_details.variables", got, want)
}

// assertProjectedVariables reads what a WORKER is served.
//
// `ResolveCurrentApplicationVersionDetails` is the query behind
// `/runtime-context/applications/{application_id}/versions/{version_id}`, and
// its `variables` key is the SDK worker's primary variable source
// (`assistant.py` builds `prompt_variables` out of `data['variables']`) and the
// first source the native runtime captures
// (services/elitea-worker-rust/src/agents/variables.rs). Asserting the stored
// rows or the HTTP read alone cannot see this key at all: it is built by a
// separate SQL expression over a separate store.
func assertProjectedVariables(t *testing.T, pool *pgxpool.Pool, applicationID, versionID int, want map[string]string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin the projection transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(ctx, tx, tenant.Project{ID: 1}); err != nil {
		t.Fatalf("bind the tenant schema: %v", err)
	}

	row, err := sqlcgen.New(tx).ResolveCurrentApplicationVersionDetails(ctx,
		sqlcgen.ResolveCurrentApplicationVersionDetailsParams{
			ApplicationVersionID: int32(versionID),
			ApplicationID:        int32(applicationID),
		})
	if err != nil {
		t.Fatalf("resolve the version details projection: %v", err)
	}

	var details struct {
		Variables []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"variables"`
	}
	if err := json.Unmarshal([]byte(row.ApplicationVersionDetailsJson), &details); err != nil {
		t.Fatalf("decode the projection %q: %v", row.ApplicationVersionDetailsJson, err)
	}
	got := map[string]string{}
	for _, variable := range details.Variables {
		got[variable.Name] = variable.Value
	}
	compareVariables(t, "application_version_details_json.variables", got, want)
}

func compareVariables(t *testing.T, surface string, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", surface, got, want)
		return
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s[%q] = %q, want %q (all: %v)", surface, name, got[name], value, got)
		}
	}
}

func versionPath(prefix string, applicationID, versionID int) string {
	return prefix + "/" + strconv.Itoa(applicationID) + "/" + strconv.Itoa(versionID)
}
