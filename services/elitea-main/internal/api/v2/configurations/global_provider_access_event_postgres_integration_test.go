package configurations_test

// The global_provider_route kind of admin.access.changed (ADR-0059 R1.5,
// issue 2509): creating, editing the route of, and deleting a global
// provider each append one chained event in the write's transaction. The
// event names the fields, never a value; a failed append rolls the write
// back.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

func providerEventRouter(pool *pgxpool.Pool, options ...configurations.Option) chi.Router {
	handler := configurations.NewHandler(pool,
		append([]configurations.Option{configurations.WithPublicProjectID(globalScopeProject)}, options...)...)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.ContextWithUser(req.Context(),
				auth.User{ID: "7", UserID: "7", CredentialKind: auth.CredentialKindSession})))
		})
	})
	r.Mount("/gateway/providers", handler.GlobalProviderRoutes(openCatalogueGatesExternal))
	return r
}

type providerEvent struct {
	Subject, Action, Principal string
	Fields                     []string
}

func providerEvents(t *testing.T, pool *pgxpool.Pool) []providerEvent {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT event FROM elitea_runtime.security_audit_outbox WHERE event_type = $1 ORDER BY seq`,
		string(securityaudit.TypeAdminAccessChanged))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []providerEvent
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "autotest.invalid") {
			t.Fatalf("an event carries a field value: %s", raw)
		}
		var envelope struct {
			Subject string `json:"subject"`
			Data    struct {
				Action       string `json:"action"`
				PrincipalRef string `json:"principal_ref"`
				Access       struct {
					Fields []string `json:"fields"`
				} `json:"access"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		events = append(events, providerEvent{
			Subject: envelope.Subject, Action: envelope.Data.Action,
			Principal: envelope.Data.PrincipalRef, Fields: envelope.Data.Access.Fields,
		})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func TestGlobalProviderRouteChangesAppendOneAccessChangedEventEach(t *testing.T) {
	pool := newCreateEchoPool(t)
	router := providerEventRouter(pool)

	created := platformSurfacePost(t, router, "/gateway/providers", map[string]any{
		"elitea_title": "autotest_provider_events",
		"type":         "open_ai",
		"data":         map[string]any{"api_base": "http://autotest.invalid/v1"},
	})
	if created.Code != http.StatusCreated && created.Code != http.StatusOK {
		t.Fatalf("POST = %d: %s", created.Code, created.Body.String())
	}
	var row struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &row); err != nil || row.ID == 0 {
		t.Fatalf("decode the created provider %s: %v", created.Body.String(), err)
	}
	subject := "global_provider/" + strconv.Itoa(row.ID)

	events := providerEvents(t, pool)
	if len(events) != 1 || events[0].Action != "admin.access.global_provider_route" ||
		events[0].Subject != subject || events[0].Principal != "user:7" ||
		strings.Join(events[0].Fields, ",") != "created,data.api_base,type" {
		t.Fatalf("events = %+v", events)
	}

	// An endpoint edit is the second event, naming the field only.
	if recorder := adminProviderDo(t, router, http.MethodPut, "/gateway/providers/"+strconv.Itoa(row.ID),
		map[string]any{"data": map[string]any{"api_base": "http://moved.autotest.invalid/v1"}}); recorder.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", recorder.Code, recorder.Body.String())
	}
	// A label edit changes nothing of the route and appends nothing.
	if recorder := adminProviderDo(t, router, http.MethodPut, "/gateway/providers/"+strconv.Itoa(row.ID),
		map[string]any{"label": "Renamed"}); recorder.Code != http.StatusOK {
		t.Fatalf("PUT label = %d: %s", recorder.Code, recorder.Body.String())
	}
	if recorder := adminProviderDo(t, router, http.MethodDelete, "/gateway/providers/"+strconv.Itoa(row.ID),
		nil); recorder.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d: %s", recorder.Code, recorder.Body.String())
	}
	events = providerEvents(t, pool)
	if len(events) != 3 || strings.Join(events[1].Fields, ",") != "data.api_base" ||
		strings.Join(events[2].Fields, ",") != "deleted" || events[2].Subject != subject {
		t.Fatalf("events = %+v, want the create, the endpoint edit and the delete", events)
	}

	report, err := securityaudit.Verify(context.Background(), pool, securityaudit.SourceOutbox, 0, 1)
	if err != nil || report.Break != nil || report.Checked != 3 {
		t.Fatalf("verify = %+v, %v", report, err)
	}
}

func TestAFailedAppendRollsTheGlobalProviderWriteBack(t *testing.T) {
	pool := newCreateEchoPool(t)
	router := providerEventRouter(pool,
		configurations.WithAccessAuditAppender(securityaudit.FailingAppender{Err: errors.New("injected append failure")}))

	created := platformSurfacePost(t, router, "/gateway/providers", map[string]any{
		"elitea_title": "autotest_provider_rollback",
		"type":         "open_ai",
		"data":         map[string]any{"api_base": "http://autotest.invalid/v1"},
	})
	if created.Code == http.StatusCreated || created.Code == http.StatusOK {
		t.Fatalf("POST = %d with a failed append: %s", created.Code, created.Body.String())
	}
	var rows int
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(
		"SELECT count(*) FROM p_%d.configuration WHERE elitea_title = $1", globalScopeProject),
		"autotest_provider_rollback").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatal("the provider was written without its event")
	}
}
