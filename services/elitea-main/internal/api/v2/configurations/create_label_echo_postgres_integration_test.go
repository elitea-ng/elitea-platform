package configurations_test

// The CREATE route's echo, against a real PostgreSQL.
//
// The defect: the create statement stored `label` and the response struct
// never carried it. The field is `omitempty`, so the key simply vanished from
// the 201 while both read routes served it — a client that rendered its list
// from the create's own answer showed a credential with no label until
// something else reloaded the row, and there was no error anywhere to say why.
//
// This is the same class as the `elitea_title`/`name` gap one field over in
// the same struct, and it is why the assertions below compare the ECHO with
// the STORED COLUMN rather than with the request body: an echo that agreed
// with the request while the column held something else is the other half of
// the same failure, and only the read-back can tell them apart.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

// TestCreateEchoesTheStoredLabel is the acceptance test.
func TestCreateEchoesTheStoredLabel(t *testing.T) {
	pool := newCreateEchoPool(t)
	router := createEchoRouter(pool)

	const title = "autotest_label_echo_cfg"
	const label = "autotest label echo"
	created := createEchoDo(t, router, map[string]any{
		"elitea_title": title,
		"label":        label,
		"type":         "pgvector",
		"section":      "vectorstorage",
		"data":         map[string]any{"host": "127.0.0.1"},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}

	var echo map[string]any
	if err := json.Unmarshal(created.Body.Bytes(), &echo); err != nil {
		t.Fatalf("decode create response %q: %v", created.Body.String(), err)
	}
	// The KEY must be present, not merely equal. `omitempty` is what removed
	// it, so a test that read the value with a zero default would have passed
	// against the defect.
	raw, present := echo["label"]
	if !present {
		t.Fatalf("the create answered no label: %s", created.Body.String())
	}
	if raw != label {
		t.Errorf("echoed label = %#v, want %q", raw, label)
	}

	// …and the echo agrees with the column. The response is assembled in Go
	// from values the handler chose, so only the row says what the next reader
	// will find.
	id, ok := echo["id"].(float64)
	if !ok {
		t.Fatalf("the create answered no id: %s", created.Body.String())
	}
	var stored string
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(
		`SELECT COALESCE(label, '') FROM %q.configuration WHERE id = $1`,
		fmt.Sprintf("p_%d", globalScopeProject)), int(id)).Scan(&stored); err != nil {
		t.Fatalf("read back the created row: %v", err)
	}
	if stored != label {
		t.Errorf("stored label = %q, want %q", stored, label)
	}

	// The detail route already served the field. It is read here so the two
	// answers about one row are compared in one test: a client reads the
	// create's answer and then reloads through this route, and the pair
	// disagreeing is the bug the user actually saw.
	detail := createEchoRead(t, router, int(id))
	if detail["label"] != label {
		t.Errorf("the detail route says label = %#v and the create said %q", detail["label"], label)
	}
}

// TestCreateWithoutALabelOmitsTheKey pins the other side of `omitempty`.
//
// A row with no label must not answer `"label": ""`. The column is nullable
// and the read routes distinguish the two, so the create's echo has to as
// well — otherwise a client cannot tell "not set" from "set to nothing".
func TestCreateWithoutALabelOmitsTheKey(t *testing.T) {
	pool := newCreateEchoPool(t)
	router := createEchoRouter(pool)

	created := createEchoDo(t, router, map[string]any{
		"elitea_title": "autotest_no_label_cfg",
		"type":         "pgvector",
		"section":      "vectorstorage",
		"data":         map[string]any{"host": "127.0.0.1"},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}
	var echo map[string]any
	if err := json.Unmarshal(created.Body.Bytes(), &echo); err != nil {
		t.Fatalf("decode create response %q: %v", created.Body.String(), err)
	}
	if raw, present := echo["label"]; present {
		t.Errorf("a row with no label answered label = %#v", raw)
	}
}

/* ── helpers ───────────────────────────────────────────────────────────── */

// createEchoRouter mounts the two compatibility routes this file exercises.
//
// No permission middleware: the gate is applied at the mount and covered
// there. What is under test is the answer a caller gets once admitted.
func createEchoRouter(pool *pgxpool.Pool) chi.Router {
	handler := configurations.NewHandler(pool,
		configurations.WithPublicProjectID(globalScopeProject))
	r := chi.NewRouter()
	r.Post("/configurations/configurations/{projectID}", handler.Create)
	r.Get("/configurations/configuration/{projectID}/{configID}", handler.Get)
	return r
}

func createEchoDo(t *testing.T, router chi.Router, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	target := fmt.Sprintf("/configurations/configurations/%d", globalScopeProject)
	request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func createEchoRead(t *testing.T, router chi.Router, id int) map[string]any {
	t.Helper()
	target := fmt.Sprintf("/configurations/configuration/%d/%d", globalScopeProject, id)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("detail status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode detail %q: %v", recorder.Body.String(), err)
	}
	return body
}

// newCreateEchoPool opens an isolated database and applies the PRODUCTION
// migration chain to tenant 1.
//
// Not this package's `newGlobalScopePool`, which builds the table from
// internal/db/schema/configuration_baseline.sql — the sqlc projection. That
// projection declares `uuid uuid NOT NULL` with no default, while the shipped
// table declares `DEFAULT gen_random_uuid()`, and the CREATE statement names
// no uuid. A create therefore cannot run at all against the projection, so a
// harness built on it can only ever test the routes that read.
func newCreateEchoPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL configuration create-echo test", environment)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
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

	databaseName := fmt.Sprintf("elitea_cfglabel_%d_%d", os.Getpid(), time.Now().UnixNano())
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
		if _, dropErr := adminPool.Exec(context.Background(),
			"DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); dropErr != nil {
			t.Errorf("drop database after pool open failure: %v", dropErr)
		}
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// 120 s, for the reason the sibling harnesses give: this DROP queues
		// behind the CREATE DATABASE of every package `go test ./...` runs at
		// once, so the wait is server load rather than a hang.
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})

	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("apply bootstrap migrations: %v", err)
	}
	runner := migrate.New(pool, platformmigrations.Files)
	if err := runner.ApplyShared(ctx); err != nil {
		t.Fatalf("apply embedded shared migrations: %v", err)
	}
	if err := runner.ApplyTenant(ctx, globalScopeProject); err != nil {
		t.Fatalf("apply embedded tenant migrations: %v", err)
	}
	return pool
}
