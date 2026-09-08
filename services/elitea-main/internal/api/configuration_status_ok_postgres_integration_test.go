package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This is the end-to-end proof for issue #457, and it runs on a database that
// deploy/scripts/standalone-stack.sh never touched.
//
// That matters. The seed script writes status_ok = true in raw SQL at six
// places, so every green run of the standalone stack has used credential rows
// that the product's own write route could never have produced. A proof built
// on that stack cannot fail on this defect.
//
// The test writes through the real HTTP route, and it reads back with the
// gateway's own SQL. The gateway is a separate Go module (Go 1.26, outside
// go.work), so its statement cannot be imported. It is read out of the gateway
// source instead, so the two can never drift: change the gateway predicate and
// this test changes with it, and delete the file and this test fails.
//
// No step of this test writes status_ok with SQL.
const statusOKIntegrationDatabaseURL = "ELITEA_TEST_DATABASE_URL"

const statusOKIntegrationDeadline = 90 * time.Second

func TestConfigurationWriteRouteMakesACredentialVisibleToTheGateway(t *testing.T) {
	pool := newStatusOKIntegrationPool(t)
	applyStatusOKIntegrationSchema(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), statusOKIntegrationDeadline)
	defer cancel()

	// Project 1 is the public project and has a provisioned vault. Project 2
	// has no vault, so a credential stored there cannot have its hidden secrets
	// redeemed. That is the negative control, and it is a real deployment
	// state, not a contrivance: a project whose vault provisioning failed.
	if err := v2secrets.NewHandler(pool).EnsureProjectVault(ctx, "1"); err != nil {
		t.Fatalf("provision the project 1 vault: %v", err)
	}

	configurations, err := runtimecomposition.NewCurrentConfigurationsRuntime(pool, 1, "", nil)
	if err != nil {
		t.Fatalf("compose the Configurations runtime: %v", err)
	}
	defer configurations.Destroy()
	admission, err := runtimecomposition.NewCurrentProviderAdmission(configurations, true)
	if err != nil {
		t.Fatalf("compose the provider admission: %v", err)
	}

	// Both routers carry an ENTITLED caller. The write route is gated since
	// #496 — it applied no permission of any kind before that — and this
	// database holds no auth_core role rows, so the live resolver would refuse
	// every request and the test would measure the gate instead of status_ok.
	// The permission is the one internal/api/v2/configurations gates the write
	// on, spelled out rather than imported, so a rename shows up as a 403 here
	// with the string named.
	entitled := fakePermissionResolver{granted: []string{
		"configurations.configurations.list",
		"configurations.configuration.details",
		"configurations.configuration.create",
		"configurations.configuration.update",
		"configurations.configuration.delete",
	}}

	fixed := NewRouter(RouterConfig{
		Pool:                      pool,
		AuthValidator:             testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:        testPrincipalValidator{},
		ConfigProviderAdmission:   admission,
		ProjectPermissionResolver: entitled,
	})
	// The composition of every shipped deployment before this change: the same
	// route, with no decision behind it.
	unfixed := NewRouter(RouterConfig{
		Pool:                      pool,
		AuthValidator:             testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:        testPrincipalValidator{},
		ProjectPermissionResolver: entitled,
	})

	credentialSQL, modelSQL := gatewayAdmissionStatements(t)

	t.Run("a legacy schema without a status default accepts creates", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `
ALTER TABLE p_1.configuration ALTER COLUMN status_ok DROP DEFAULT;
ALTER TABLE p_2.configuration ALTER COLUMN status_ok DROP DEFAULT;`); err != nil {
			t.Fatalf("remove the status default: %v", err)
		}
		// Complete against the type's own schema. The create route refuses a
		// body missing a schema-required field before it reaches the admission
		// decision this file is about
		// (internal/api/v2/configurations/required_fields.go).
		created := postStatusOKConfiguration(t, unfixed, 1, map[string]any{
			"elitea_title": "legacy-schema-openapi",
			"label":        "legacy-schema-openapi",
			"type":         "openapi",
			"data":         map[string]any{},
		})
		if created["status_ok"] != false {
			t.Fatalf("create response status_ok = %v, want false", created["status_ok"])
		}
	})

	t.Run("the defect", func(t *testing.T) {
		created := postStatusOKConfiguration(t, unfixed, 1, map[string]any{
			"elitea_title": "unfixed-openai",
			"label":        "unfixed-openai",
			"type":         "open_ai",
			"data":         map[string]any{"api_key": "sk-unfixed", "api_base": ""},
		})
		if created["status_ok"] != false {
			t.Fatalf("the route reported status_ok=%v without an admission decision", created["status_ok"])
		}
		if stored := storedStatusOK(t, ctx, pool, 1, "unfixed-openai"); stored {
			t.Fatal("a row reached status_ok = true with no component able to write it")
		}
		if titles := gatewayCredentialTitles(t, ctx, pool, credentialSQL, 1); contains(titles, "unfixed-openai") {
			t.Fatalf("the gateway admitted a status_ok = false row: %v", titles)
		}
	})

	t.Run("a saved credential becomes visible", func(t *testing.T) {
		created := postStatusOKConfiguration(t, fixed, 1, map[string]any{
			"elitea_title": "standalone-openai",
			"label":        "standalone-openai",
			"type":         "open_ai",
			// A literal api_key is the shape this route stores. It does not
			// extract secrets into the vault, and the gateway reads either a
			// literal or a {{secret.NAME}} reference.
			"data": map[string]any{"api_key": "sk-standalone", "api_base": ""},
		})
		if created["status_ok"] != true {
			t.Fatalf("create response status_ok = %v, want true", created["status_ok"])
		}
		if !storedStatusOK(t, ctx, pool, 1, "standalone-openai") {
			t.Fatal("the stored row is not usable")
		}
		titles := gatewayCredentialTitles(t, ctx, pool, credentialSQL, 1)
		if !contains(titles, "standalone-openai") {
			t.Fatalf("the gateway credential read returned %v, want the saved credential", titles)
		}
	})

	t.Run("a credential whose secrets cannot be redeemed stays refused", func(t *testing.T) {
		created := postStatusOKConfiguration(t, fixed, 2, map[string]any{
			"elitea_title": "vaultless-openai",
			"label":        "vaultless-openai",
			"type":         "open_ai",
			"data":         map[string]any{"api_key": "{{secret.openai}}", "api_base": ""},
		})
		if created["status_ok"] != false {
			t.Fatalf("create response status_ok = %v, want false", created["status_ok"])
		}
		if storedStatusOK(t, ctx, pool, 2, "vaultless-openai") {
			t.Fatal("a credential with no readable vault was marked usable")
		}
		if titles := gatewayCredentialTitles(t, ctx, pool, credentialSQL, 2); len(titles) != 0 {
			t.Fatalf("the gateway credential read returned %v, want nothing", titles)
		}
	})

	t.Run("a linked model becomes visible", func(t *testing.T) {
		created := postStatusOKConfiguration(t, fixed, 1, map[string]any{
			"elitea_title": "standalone-model",
			"label":        "standalone-model",
			"type":         "llm_model",
			"section":      "llm",
			"data": map[string]any{
				"name":           "gpt-4o",
				"ai_credentials": map[string]any{"elitea_title": "standalone-openai"},
			},
		})
		if created["status_ok"] != true {
			t.Fatalf("create response status_ok = %v, want true", created["status_ok"])
		}
		titles := gatewayModelTitles(t, ctx, pool, modelSQL, 1)
		if !contains(titles, "standalone-model") {
			t.Fatalf("the gateway model read returned %v, want the saved model", titles)
		}
	})

	t.Run("a model whose credential reference dangles stays refused", func(t *testing.T) {
		created := postStatusOKConfiguration(t, fixed, 1, map[string]any{
			"elitea_title": "dangling-model",
			"label":        "dangling-model",
			"type":         "llm_model",
			"section":      "llm",
			"data": map[string]any{
				"name":           "gpt-4o",
				"ai_credentials": map[string]any{"elitea_title": "no-such-credential"},
			},
		})
		if created["status_ok"] != false {
			t.Fatalf("create response status_ok = %v, want false", created["status_ok"])
		}
		if titles := gatewayModelTitles(t, ctx, pool, modelSQL, 1); contains(titles, "dangling-model") {
			t.Fatalf("the gateway model read returned %v, want the unresolved model excluded", titles)
		}
	})

	// An update must be able to withdraw a row as well as admit one. Without
	// this, a credential that stops resolving stays advertised to the gateway
	// and fails at the user's next request instead.
	t.Run("an update withdraws a row that stops resolving", func(t *testing.T) {
		id := storedConfigurationID(t, ctx, pool, 1, "standalone-model")
		updated := putStatusOKConfiguration(t, fixed, 1, id, map[string]any{
			"elitea_title": "standalone-model",
			"label":        "standalone-model",
			"type":         "llm_model",
			"section":      "llm",
			"data": map[string]any{
				"name":           "gpt-4o",
				"ai_credentials": map[string]any{"elitea_title": "no-such-credential"},
			},
		})
		if updated["status_ok"] != false {
			t.Fatalf("update response status_ok = %v, want false", updated["status_ok"])
		}
		if titles := gatewayModelTitles(t, ctx, pool, modelSQL, 1); contains(titles, "standalone-model") {
			t.Fatalf("the gateway model read returned %v, want the withdrawn model excluded", titles)
		}

		restored := putStatusOKConfiguration(t, fixed, 1, id, map[string]any{
			"elitea_title": "standalone-model",
			"label":        "standalone-model",
			"type":         "llm_model",
			"section":      "llm",
			"data": map[string]any{
				"name":           "gpt-4o",
				"ai_credentials": map[string]any{"elitea_title": "standalone-openai"},
			},
		})
		if restored["status_ok"] != true {
			t.Fatalf("restored update status_ok = %v, want true", restored["status_ok"])
		}
	})
}

// gatewayAdmissionStatements reads the two statements that decide what the LLM
// gateway can use, out of the gateway's own source. A missing file or a renamed
// constant is a failure, not a skip.
func gatewayAdmissionStatements(t *testing.T) (credentialSQL, modelSQL string) {
	t.Helper()
	credentialSQL = gatewayRawConstant(
		t,
		filepath.Join("..", "..", "..", "elitea-llm-gateway", "internal", "account", "credentials.go"),
		"credentialsSQL",
	)
	modelSQL = gatewayRawConstant(
		t,
		filepath.Join("..", "..", "..", "elitea-llm-gateway", "internal", "llmproxy", "models.go"),
		"modelsSQL",
	)
	for name, statement := range map[string]string{"credentialsSQL": credentialSQL, "modelsSQL": modelSQL} {
		if !regexp.MustCompile(`status_ok = true`).MatchString(statement) {
			t.Fatalf("%s no longer filters on status_ok; this test no longer proves anything: %s", name, statement)
		}
	}
	return credentialSQL, modelSQL
}

func gatewayRawConstant(t *testing.T, source, name string) string {
	t.Helper()
	content, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read the gateway source %s: %v", source, err)
	}
	pattern := regexp.MustCompile("(?s)const " + regexp.QuoteMeta(name) + " = `(.*?)`")
	match := pattern.FindSubmatch(content)
	if match == nil {
		t.Fatalf("%s is no longer declared in %s", name, source)
	}
	return string(match[1])
}

func gatewayCredentialTitles(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	credentialSQL string,
	projectID int,
) []string {
	t.Helper()
	// The empty predicate is the caller's own project scope, which is the scope
	// a project's own credential is read in.
	query := fmt.Sprintf(credentialSQL, fmt.Sprintf("p_%d", projectID), "")
	rows, err := pool.Query(ctx, query, []string{"open_ai"})
	if err != nil {
		t.Fatalf("run the gateway credential read: %v", err)
	}
	defer rows.Close()
	titles := make([]string, 0)
	for rows.Next() {
		var id, title string
		var data []byte
		var shared bool
		if err := rows.Scan(&id, &title, &data, &shared); err != nil {
			t.Fatalf("scan the gateway credential read: %v", err)
		}
		titles = append(titles, title)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the gateway credential rows: %v", err)
	}
	return titles
}

func gatewayModelTitles(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	modelSQL string,
	projectID int,
) []string {
	t.Helper()
	query := fmt.Sprintf(modelSQL, fmt.Sprintf("p_%d", projectID), "")
	rows, err := pool.Query(
		ctx,
		query,
		[]string{"llm", "embedding", "image_generation"},
		[]string{"llm_model", "embedding_model", "image_generation_model"},
	)
	if err != nil {
		t.Fatalf("run the gateway model read: %v", err)
	}
	defer rows.Close()
	titles := make([]string, 0)
	for rows.Next() {
		var title string
		var data []byte
		var shared bool
		if err := rows.Scan(&title, &data, &shared); err != nil {
			t.Fatalf("scan the gateway model read: %v", err)
		}
		titles = append(titles, title)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the gateway model rows: %v", err)
	}
	return titles
}

func postStatusOKConfiguration(
	t *testing.T,
	router http.Handler,
	projectID int,
	body map[string]any,
) map[string]any {
	t.Helper()
	return callStatusOKConfiguration(
		t, router, http.MethodPost,
		fmt.Sprintf("/api/v2/configurations/configurations/%d", projectID),
		body, http.StatusCreated,
	)
}

func putStatusOKConfiguration(
	t *testing.T,
	router http.Handler,
	projectID, configurationID int,
	body map[string]any,
) map[string]any {
	t.Helper()
	return callStatusOKConfiguration(
		t, router, http.MethodPut,
		fmt.Sprintf("/api/v2/configurations/configuration/%d/%d", projectID, configurationID),
		body, http.StatusOK,
	)
}

func callStatusOKConfiguration(
	t *testing.T,
	router http.Handler,
	method, path string,
	body map[string]any,
	want int,
) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, testAuthHeader(request))
	if recorder.Code != want {
		t.Fatalf("%s %s status=%d body=%s", method, path, recorder.Code, recorder.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode the response of %s %s: %v", method, path, err)
	}
	return decoded
}

func storedStatusOK(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID int, title string) bool {
	t.Helper()
	var statusOK bool
	query := fmt.Sprintf(`SELECT status_ok FROM p_%d.configuration WHERE elitea_title = $1`, projectID)
	if err := pool.QueryRow(ctx, query, title).Scan(&statusOK); err != nil {
		t.Fatalf("read the stored status of %s: %v", title, err)
	}
	return statusOK
}

func storedConfigurationID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID int, title string) int {
	t.Helper()
	var id int
	query := fmt.Sprintf(`SELECT id FROM p_%d.configuration WHERE elitea_title = $1`, projectID)
	if err := pool.QueryRow(ctx, query, title).Scan(&id); err != nil {
		t.Fatalf("read the stored id of %s: %v", title, err)
	}
	return id
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// applyStatusOKIntegrationSchema builds the two tenant schemas from the
// platform's own initial migration. The real DDL is used on purpose: the
// column default this defect depends on is declared there
// (status_ok BOOLEAN NOT NULL DEFAULT false), and a hand-written copy could
// declare a different default and hide the fault.
func applyStatusOKIntegrationSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), statusOKIntegrationDeadline)
	defer cancel()

	source := filepath.Join("..", "infra", "db", "migrations", "001_initial.sql")
	initial, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read %s: %v", source, err)
	}
	if _, err := pool.Exec(ctx, string(initial)); err != nil {
		t.Fatalf("apply %s: %v", source, err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO centry.project (id, name, owner_id, create_success)
VALUES (1, 'public', 1, true), (2, 'tenant', 1, true)
ON CONFLICT (id) DO NOTHING;
SELECT create_tenant_schema('p_1');
SELECT create_tenant_schema('p_2');`); err != nil {
		t.Fatalf("create the tenant schemas: %v", err)
	}

	var defaultExpression *string
	if err := pool.QueryRow(ctx, `
SELECT column_default
FROM information_schema.columns
WHERE table_schema = 'p_1' AND table_name = 'configuration' AND column_name = 'status_ok'`,
	).Scan(&defaultExpression); err != nil {
		t.Fatalf("read the status_ok column default: %v", err)
	}
	if defaultExpression == nil || *defaultExpression != "false" {
		t.Fatalf("status_ok column default = %v, want false; this test assumes the defect's precondition", defaultExpression)
	}
}

func newStatusOKIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv(statusOKIntegrationDatabaseURL)
	if databaseURL == "" {
		t.Skipf("set %s to run the configuration status_ok integration test", statusOKIntegrationDatabaseURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), statusOKIntegrationDeadline)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", statusOKIntegrationDatabaseURL, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open the PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_status_ok_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		adminPool.Close()
		t.Fatalf("create the isolated integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quoted+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("open the isolated integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop the isolated integration database: %v", err)
		}
		adminPool.Close()
	})
	return pool
}
