package configurations

// The refusals and the fail-safe behaviour of POST /revalidate (revalidate.go).
//
// The flip itself — status_ok true -> false after the referenced credential is
// deleted, read back with SQL — is in
// stored_check_postgres_integration_test.go, because a status column that the
// handler believes it wrote is not the same fact as a status column the
// database holds. This file measures what happens when the decision is NOT
// available, which is the direction that fails silently: a route that answers
// 200 with the unchanged row looks exactly like a route that revalidated and
// found nothing to change.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

/* ── fakes ─────────────────────────────────────────────────────────────── */

type recordingAdmission struct {
	calls    []configurationapp.CurrentConfigurationLifecycleSnapshot
	decision configurationapp.CurrentProviderAdmissionDecision
	err      error
}

func (a *recordingAdmission) AdmitCurrentProviderConfiguration(
	_ context.Context,
	snapshot configurationapp.CurrentConfigurationLifecycleSnapshot,
) (configurationapp.CurrentProviderAdmissionDecision, error) {
	if a == nil {
		// The typed-nil shape: a nil pointer boxed into the interface makes the
		// handler's nil test false, so the call lands here. It must answer,
		// not dereference.
		return configurationapp.CurrentProviderAdmissionDecision{}, errors.New("admission is not composed")
	}
	a.calls = append(a.calls, snapshot)
	return a.decision, a.err
}

// revalidateClosedPool is a pool whose every statement fails. It lets a request
// reach the handler body — a nil pool is refused earlier — without a database.
func revalidateClosedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://unused:unused@127.0.0.1:1/unused")
	if err != nil {
		t.Fatalf("build the closed pool: %v", err)
	}
	pool.Close()
	return pool
}

func revalidateRequest(t *testing.T, handler *Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Post("/revalidate/{projectID}/{configID}", handler.RevalidateConfiguration)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, target, nil))
	return recorder
}

/* ── the refusals ──────────────────────────────────────────────────────── */

// A build with no admission composed REFUSES. It does not answer 200 with the
// row it did not revalidate.
//
// Both answers are HTTP-shaped and neither carries an error the client can
// see, so the difference is invisible unless it is asserted here. The row's
// status_ok is the platform's own claim that a credential is usable; a control
// that silently declines to re-derive it leaves the user re-saving credentials
// to fix a status nothing was ever going to change.
func TestRevalidateRefusesWhenNoAdmissionIsComposed(t *testing.T) {
	handler := NewHandler(revalidateClosedPool(t))

	recorder := revalidateRequest(t, handler, "/revalidate/7/11")

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d. A build that cannot revalidate must say so.",
			recorder.Code, http.StatusServiceUnavailable)
	}
}

// The store itself missing is also a refusal, and a different one.
func TestRevalidateRefusesWithoutADatabase(t *testing.T) {
	handler := NewHandler(nil, WithProviderAdmission(&recordingAdmission{}))

	recorder := revalidateRequest(t, handler, "/revalidate/7/11")

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
}

// A project id that is not a plain decimal number never reaches SQL: the
// schema name goes into the statement TEXT, so it is quoted with SQL rules or
// the request is refused (#543).
func TestRevalidateRefusesAProjectIDThatIsNotAProjectID(t *testing.T) {
	handler := NewHandler(revalidateClosedPool(t), WithProviderAdmission(&recordingAdmission{}))

	recorder := revalidateRequest(t, handler, `/revalidate/7%22;%20DROP%20SCHEMA%20public/11`)

	if recorder.Code != http.StatusBadRequest && recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want a refusal", recorder.Code)
	}
}

// A read that FAILS is not a row that is absent. Reporting the first as the
// second would tell a user their credential has been deleted whenever the pool
// is saturated.
func TestRevalidateDoesNotReportAFailedReadAsAMissingRow(t *testing.T) {
	handler := NewHandler(revalidateClosedPool(t), WithProviderAdmission(&recordingAdmission{}))

	recorder := revalidateRequest(t, handler, "/revalidate/7/11")

	if recorder.Code == http.StatusNotFound {
		t.Fatal("a failed statement answered 404; absence and failure must not be the same answer")
	}
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
}

/* ── the fail-safe decision ────────────────────────────────────────────── */

// admitConfiguration is the ONE writer this route uses — the same one Update
// uses. These cases pin the two directions in which it must leave the stored
// value alone, because both are reached by rows this route will be pointed at.
func TestRevalidationLeavesTheStoredStatusAloneWhenTheDecisionDoesNotOwnIt(t *testing.T) {
	for name, testCase := range map[string]struct {
		admission *recordingAdmission
		stored    bool
		want      bool
	}{
		// An unmanaged row — a generic SDK configuration, or an imported model
		// with no ai_credentials — declares no references and holds no
		// secrets. Its status belongs to whoever wrote it.
		"unmanaged row keeps its stored status": {
			admission: &recordingAdmission{decision: configurationapp.CurrentProviderAdmissionDecision{
				Managed: false, StatusOK: true,
			}},
			stored: false,
			want:   false,
		},
		"unmanaged row keeps a true status": {
			admission: &recordingAdmission{decision: configurationapp.CurrentProviderAdmissionDecision{
				Managed: false,
			}},
			stored: true,
			want:   true,
		},
		// A decision that could not be reached is not a verdict. Turning it
		// into `false` would withdraw a working credential from the gateway
		// every time the vault was briefly unreachable.
		"a failed decision keeps the stored status": {
			admission: &recordingAdmission{err: errors.New("the decision did not complete")},
			stored:    true,
			want:      true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			handler := NewHandler(revalidateClosedPool(t), WithProviderAdmission(testCase.admission))
			snapshot, ok := configurationAdmissionSnapshot(
				11, "11111111-1111-4111-8111-111111111111", 7, "OpenAI", "open_ai", "ai_credentials",
				map[string]any{}, nil,
			)
			if !ok {
				t.Fatal("the snapshot could not be built")
			}

			got := handler.admitConfiguration(context.Background(), `"p_7"`, testCase.stored, snapshot)

			if got != testCase.want {
				t.Fatalf("status_ok = %v, want %v", got, testCase.want)
			}
		})
	}
}

// A typed-nil admission — a nil pointer boxed into the interface at a
// composition root — costs the caller the decision, not the process.
func TestRevalidationSurvivesATypedNilAdmission(t *testing.T) {
	handler := NewHandler(revalidateClosedPool(t), WithProviderAdmission((*recordingAdmission)(nil)))
	snapshot, ok := configurationAdmissionSnapshot(
		11, "11111111-1111-4111-8111-111111111111", 7, "OpenAI", "open_ai", "ai_credentials",
		map[string]any{}, nil,
	)
	if !ok {
		t.Fatal("the snapshot could not be built")
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("admission panicked with %v on a typed-nil dependency", recovered)
		}
	}()
	if got := handler.admitConfiguration(context.Background(), `"p_7"`, true, snapshot); !got {
		t.Fatal("a typed-nil admission changed the stored status")
	}
}

/* ── the response shape ────────────────────────────────────────────────── */

// The route answers the same object the detail route answers, so the browser
// can replace the row it holds without a second read. Asserted on the DTO
// rather than on a live response, so a renamed JSON field is caught here as
// well as in the integration file.
func TestRevalidationAnswersTheConfigurationShape(t *testing.T) {
	encoded, err := json.Marshal(Configuration{ID: 11, Name: "OpenAI", StatusOK: true})
	if err != nil {
		t.Fatalf("encode the configuration: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode the configuration: %v", err)
	}
	for _, field := range []string{"id", "name", "status_ok"} {
		if _, present := decoded[field]; !present {
			t.Fatalf("the revalidation response carries no %q field", field)
		}
	}
}
