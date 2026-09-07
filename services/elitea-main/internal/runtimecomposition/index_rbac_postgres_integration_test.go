package runtimecomposition

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	publicapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api"
	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	executionapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/executions"
	indexingapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/indexing"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	indexmetaapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexmeta"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authsvc"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	indexRBACPeerAddress = "10.0.0.8:43120"
	indexRBACSessionKey  = "index-rbac-session-secret-for-tests"
	indexRBACStartBody   = `{"toolkit_config":{"toolkit_id":9},"tool_name":"index_data","tool_params":{"index_name":"docs"}}`
)

type indexRBACPeerVerifier struct{}

func (indexRBACPeerVerifier) VerifyForwardedIdentityPeer(request *http.Request) error {
	if request.RemoteAddr != indexRBACPeerAddress {
		return errors.New("untrusted forwarded identity peer")
	}
	return nil
}

type indexRBACStartSpy struct {
	calls int
}

func (spy *indexRBACStartSpy) StartIndexData(
	context.Context,
	indexingapp.StartRequest,
) (indexingapp.StartOutcome, error) {
	spy.calls++
	return indexingapp.StartOutcome{TaskID: "index-rbac-task"}, nil
}

type indexRBACCancelSpy struct {
	calls int
}

func (spy *indexRBACCancelSpy) Cancel(
	context.Context,
	indexingapp.CurrentIndexCancelRequest,
) (bool, error) {
	spy.calls++
	return true, nil
}

type indexRBACMetaSpy struct {
	calls int
}

func (spy *indexRBACMetaSpy) List(
	context.Context,
	indexmetaapp.Request,
) ([]indexmetaapp.Item, error) {
	spy.calls++
	return []indexmetaapp.Item{}, nil
}

type indexRBACMetaDeleteSpy struct {
	calls   int
	request indexmetaapp.DeleteRequest
}

func (spy *indexRBACMetaDeleteSpy) Delete(
	_ context.Context,
	request indexmetaapp.DeleteRequest,
) error {
	spy.calls++
	spy.request = request
	return nil
}

type indexRBACReplaySpy struct {
	calls int
}

func (spy *indexRBACReplaySpy) Replay(
	context.Context,
	string,
	string,
	uint64,
	int,
) ([]executionapi.DurableEvent, error) {
	spy.calls++
	return nil, nil
}

type indexRBACWaiter struct{}

func (indexRBACWaiter) Wait(context.Context, string, string, uint64) (bool, error) {
	return false, context.Canceled
}

type indexRBACTrackingBody struct {
	io.Reader
	read bool
}

func (body *indexRBACTrackingBody) Read(target []byte) (int, error) {
	body.read = true
	return body.Reader.Read(target)
}

func (*indexRBACTrackingBody) Close() error { return nil }

type indexRBACStreamingRecorder struct {
	*httptest.ResponseRecorder
}

func newIndexRBACStreamingRecorder() *indexRBACStreamingRecorder {
	return &indexRBACStreamingRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (*indexRBACStreamingRecorder) SetWriteDeadline(time.Time) error { return nil }
func (recorder *indexRBACStreamingRecorder) Flush()                  { recorder.ResponseRecorder.Flush() }

type indexRBACSpies struct {
	start      *indexRBACStartSpy
	cancel     indexingapi.CurrentIndexCanceller
	meta       *indexRBACMetaSpy
	metaDelete *indexRBACMetaDeleteSpy
	replay     *indexRBACReplaySpy
}

func TestIndexRoutesPostgresRBACAndTenantMatrix(t *testing.T) {
	pool := newIndexRBACPostgresPool(t)
	prepareIndexRBACFixtures(t, pool)

	type expectation struct {
		name       string
		authType   string
		userID     string
		tokenID    string
		projectID  string
		start      int
		cancel     int
		meta       int
		metaDelete int
		events     int
	}
	for _, test := range []expectation{
		{name: "global super-admin", userID: "1", projectID: "1", start: 403, cancel: 403, meta: 403, metaDelete: 403, events: 403},
		{name: "platform admin", userID: "2", projectID: "1", start: 403, cancel: 403, meta: 403, metaDelete: 403, events: 403},
		{name: "project admin forwarded user", userID: "3", projectID: "1", start: 200, cancel: 204, meta: 200, metaDelete: 200, events: 200},
		// Browser sessions terminate at the auth gateway for production-runtime
		// SSE; Main receives the gateway's trusted forwarded user identity.
		{name: "project admin session", authType: "session", userID: "3", projectID: "1", start: 200, cancel: 204, meta: 200, metaDelete: 200, events: 401},
		{name: "project editor forwarded user", userID: "4", projectID: "1", start: 200, cancel: 204, meta: 200, metaDelete: 200, events: 200},
		{name: "project editor PAT", authType: "token", userID: "4", tokenID: "104", projectID: "1", start: 200, cancel: 204, meta: 200, metaDelete: 200, events: 200},
		// The current permission-consolidation migration grants task.delete to
		// the default viewer role. Preserve that effective baseline even though
		// the older route decorator recommends viewer=false.
		{name: "project viewer", userID: "5", projectID: "1", start: 403, cancel: 204, meta: 200, metaDelete: 403, events: 403},
		{name: "custom patch only", userID: "6", projectID: "1", start: 200, cancel: 403, meta: 403, metaDelete: 403, events: 200},
		{name: "custom delete only", userID: "11", projectID: "1", start: 403, cancel: 403, meta: 403, metaDelete: 200, events: 403},
		{name: "custom metadata reader only", userID: "12", projectID: "1", start: 403, cancel: 403, meta: 200, metaDelete: 403, events: 403},
		{name: "custom cancel only", userID: "13", projectID: "1", start: 403, cancel: 204, meta: 403, metaDelete: 403, events: 403},
		{name: "suspended user", userID: "7", projectID: "1", start: 401, cancel: 401, meta: 401, metaDelete: 401, events: 401},
		{name: "wrong project", userID: "8", projectID: "1", start: 403, cancel: 403, meta: 403, metaDelete: 403, events: 403},
		{name: "suspended project", userID: "9", projectID: "3", start: 403, cancel: 403, meta: 403, metaDelete: 403, events: 403},
		{name: "nonmember", userID: "10", projectID: "1", start: 403, cancel: 403, meta: 403, metaDelete: 403, events: 403},
		{name: "dual-project member project one", userID: "14", projectID: "1", start: 200, cancel: 204, meta: 200, metaDelete: 200, events: 200},
		{name: "dual-project member project two", userID: "14", projectID: "2", start: 200, cancel: 204, meta: 200, metaDelete: 200, events: 200},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Run("start", func(t *testing.T) {
				spies, router := newIndexRBACRouter(t, pool, nil)
				bodyText := indexRBACStartBody
				if test.start != http.StatusOK {
					bodyText = "{"
				}
				body := &indexRBACTrackingBody{Reader: strings.NewReader(bodyText)}
				request := newIndexRBACPrincipalRequest(
					t,
					http.MethodPost,
					"/api/v2/elitea_core/test_toolkit_tool/prompt_lib/"+test.projectID+"?await_response=false",
					test.authType,
					test.userID,
					test.tokenID,
					body,
				)
				request.Header.Set("Content-Type", "application/json")
				response := newIndexRBACStreamingRecorder()
				router.ServeHTTP(response, request)
				if response.Code != test.start {
					t.Fatalf("status=%d want=%d body=%s", response.Code, test.start, response.Body.String())
				}
				wantCalls := 0
				if test.start == http.StatusOK {
					wantCalls = 1
				}
				if spies.start.calls != wantCalls || body.read != (wantCalls == 1) {
					t.Fatalf("start calls=%d body_read=%v want_calls=%d", spies.start.calls, body.read, wantCalls)
				}
			})

			t.Run("cancel", func(t *testing.T) {
				cancel := &indexRBACCancelSpy{}
				spies, router := newIndexRBACRouter(t, pool, cancel)
				response := newIndexRBACStreamingRecorder()
				router.ServeHTTP(response, newIndexRBACPrincipalRequest(
					t,
					http.MethodDelete,
					"/api/v2/elitea_core/index_cancel/prompt_lib/"+test.projectID+
						"/9/docs/11111111111111111111111111111111",
					test.authType,
					test.userID,
					test.tokenID,
					nil,
				))
				if response.Code != test.cancel {
					t.Fatalf("status=%d want=%d body=%s", response.Code, test.cancel, response.Body.String())
				}
				wantCalls := 0
				if test.cancel == http.StatusNoContent {
					wantCalls = 1
				}
				if cancel.calls != wantCalls || spies.cancel != cancel {
					t.Fatalf("cancel calls=%d want=%d", cancel.calls, wantCalls)
				}
			})

			t.Run("metadata", func(t *testing.T) {
				spies, router := newIndexRBACRouter(t, pool, nil)
				response := newIndexRBACStreamingRecorder()
				router.ServeHTTP(response, newIndexRBACPrincipalRequest(
					t,
					http.MethodGet,
					"/api/v2/elitea_core/index_meta/prompt_lib/"+test.projectID+"/9",
					test.authType,
					test.userID,
					test.tokenID,
					nil,
				))
				if response.Code != test.meta {
					t.Fatalf("status=%d want=%d body=%s", response.Code, test.meta, response.Body.String())
				}
				wantCalls := 0
				if test.meta == http.StatusOK {
					wantCalls = 1
				}
				if spies.meta.calls != wantCalls {
					t.Fatalf("metadata calls=%d want=%d", spies.meta.calls, wantCalls)
				}
			})

			t.Run("metadata delete", func(t *testing.T) {
				spies, router := newIndexRBACRouter(t, pool, nil)
				response := newIndexRBACStreamingRecorder()
				router.ServeHTTP(response, newIndexRBACPrincipalRequest(
					t,
					http.MethodDelete,
					"/api/v2/elitea_core/index_meta/prompt_lib/"+test.projectID+"/9/meta-1",
					test.authType,
					test.userID,
					test.tokenID,
					nil,
				))
				if response.Code != test.metaDelete {
					t.Fatalf("status=%d want=%d body=%s", response.Code, test.metaDelete, response.Body.String())
				}
				wantCalls := 0
				if test.metaDelete == http.StatusOK {
					wantCalls = 1
				}
				if spies.metaDelete.calls != wantCalls {
					t.Fatalf("metadata delete calls=%d want=%d", spies.metaDelete.calls, wantCalls)
				}
				if wantCalls == 1 &&
					(fmt.Sprint(spies.metaDelete.request.ProjectID) != test.projectID ||
						fmt.Sprint(spies.metaDelete.request.ActorUserID) != test.userID) {
					t.Fatalf(
						"metadata delete identity project=%d actor=%d want project=%s actor=%s",
						spies.metaDelete.request.ProjectID,
						spies.metaDelete.request.ActorUserID,
						test.projectID,
						test.userID,
					)
				}
			})

			t.Run("events", func(t *testing.T) {
				spies, router := newIndexRBACRouter(t, pool, nil)
				executionID := "execution-project-" + test.projectID
				response := newIndexRBACStreamingRecorder()
				router.ServeHTTP(response, newIndexRBACPrincipalRequest(
					t,
					http.MethodGet,
					"/api/v2/executions/"+test.projectID+"/"+executionID+"/events",
					test.authType,
					test.userID,
					test.tokenID,
					nil,
				))
				if response.Code != test.events {
					t.Fatalf("status=%d want=%d body=%s", response.Code, test.events, response.Body.String())
				}
				wantCalls := 0
				if test.events == http.StatusOK {
					wantCalls = 1
				}
				if spies.replay.calls != wantCalls {
					t.Fatalf("event replay calls=%d want=%d", spies.replay.calls, wantCalls)
				}
			})
		})
	}
}

func TestIndexEventsPostgresAuthorizeBeforeCursorAndBindEveryTenantDimension(t *testing.T) {
	pool := newIndexRBACPostgresPool(t)
	prepareIndexRBACFixtures(t, pool)

	t.Run("permission denial wins over malformed cursor", func(t *testing.T) {
		spies, router := newIndexRBACRouter(t, pool, nil)
		response := newIndexRBACStreamingRecorder()
		router.ServeHTTP(response, newIndexRBACRequest(
			http.MethodGet,
			"/api/v2/executions/1/execution-project-1/events?cursor=not-a-number",
			"5",
			nil,
		))
		if response.Code != http.StatusForbidden || spies.replay.calls != 0 {
			t.Fatalf("status=%d replay_calls=%d body=%s", response.Code, spies.replay.calls, response.Body.String())
		}
	})

	t.Run("authorized malformed cursor is a request error", func(t *testing.T) {
		spies, router := newIndexRBACRouter(t, pool, nil)
		response := newIndexRBACStreamingRecorder()
		router.ServeHTTP(response, newIndexRBACRequest(
			http.MethodGet,
			"/api/v2/executions/1/execution-project-1/events?cursor=not-a-number",
			"3",
			nil,
		))
		if response.Code != http.StatusBadRequest || spies.replay.calls != 0 {
			t.Fatalf("status=%d replay_calls=%d body=%s", response.Code, spies.replay.calls, response.Body.String())
		}
	})

	t.Run("membership in both projects cannot cross execution tenant", func(t *testing.T) {
		spies, router := newIndexRBACRouter(t, pool, nil)
		response := newIndexRBACStreamingRecorder()
		router.ServeHTTP(response, newIndexRBACRequest(
			http.MethodGet,
			"/api/v2/executions/2/execution-project-1/events",
			"14",
			nil,
		))
		if response.Code != http.StatusForbidden || spies.replay.calls != 0 {
			t.Fatalf("status=%d replay_calls=%d body=%s", response.Code, spies.replay.calls, response.Body.String())
		}
	})
}

func TestIndexStopPostgresBindsDurableTransitionToExactTenantAndTarget(t *testing.T) {
	pool := newIndexRBACPostgresPool(t)
	prepareIndexRBACFixtures(t, pool)

	repository, err := repos.NewCurrentIndexCancellationRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	canceller, err := indexingapp.NewCurrentIndexCancellationService(repository)
	if err != nil {
		t.Fatal(err)
	}
	_, router := newIndexRBACRouter(t, pool, canceller)

	for _, test := range []struct {
		name        string
		userID      string
		projectID   string
		executionID string
		wantStatus  int
		wantState   string
	}{
		{
			name:        "project admin transitions exact active execution",
			userID:      "3",
			projectID:   "1",
			executionID: "11111111111111111111111111111111",
			wantStatus:  http.StatusNoContent,
			wantState:   "CANCELLED",
		},
		{
			name:        "authorized project cannot cancel another tenant",
			userID:      "3",
			projectID:   "1",
			executionID: "22222222222222222222222222222222",
			wantStatus:  http.StatusNoContent,
			wantState:   "RUNNING",
		},
		{
			name:        "inconsistent tenant binding fails closed",
			userID:      "3",
			projectID:   "1",
			executionID: "33333333333333333333333333333333",
			wantStatus:  http.StatusNoContent,
			wantState:   "RUNNING",
		},
		{
			name:        "viewer preserves effective baseline cancel grant",
			userID:      "5",
			projectID:   "1",
			executionID: "44444444444444444444444444444444",
			wantStatus:  http.StatusNoContent,
			wantState:   "CANCELLED",
		},
		{
			name:        "wrong-project member cannot transition durable state",
			userID:      "8",
			projectID:   "1",
			executionID: "55555555555555555555555555555555",
			wantStatus:  http.StatusForbidden,
			wantState:   "RUNNING",
		},
		{
			name:        "suspended user cannot transition durable state",
			userID:      "7",
			projectID:   "1",
			executionID: "66666666666666666666666666666666",
			wantStatus:  http.StatusUnauthorized,
			wantState:   "RUNNING",
		},
		{
			name:        "platform admin without project role cannot transition durable state",
			userID:      "2",
			projectID:   "1",
			executionID: "77777777777777777777777777777777",
			wantStatus:  http.StatusForbidden,
			wantState:   "RUNNING",
		},
		{
			name:        "nonmember cannot transition durable state",
			userID:      "10",
			projectID:   "1",
			executionID: "88888888888888888888888888888888",
			wantStatus:  http.StatusForbidden,
			wantState:   "RUNNING",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			response := newIndexRBACStreamingRecorder()
			router.ServeHTTP(response, newIndexRBACRequest(
				http.MethodDelete,
				"/api/v2/elitea_core/index_cancel/prompt_lib/"+test.projectID+
					"/9/docs/"+test.executionID,
				test.userID,
				nil,
			))
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.wantStatus, response.Body.String())
			}
			var desiredState string
			if err := pool.QueryRow(
				context.Background(),
				`SELECT desired_state FROM elitea_runtime.execution_jobs WHERE execution_id = $1`,
				test.executionID,
			).Scan(&desiredState); err != nil {
				t.Fatal(err)
			}
			if desiredState != test.wantState {
				t.Fatalf("desired_state=%q want=%q", desiredState, test.wantState)
			}
		})
	}
}

func TestIndexAdditionalContractsResolveLegacyRolesAndRejectUnauthenticatedScheduleSearch(t *testing.T) {
	pool := newIndexRBACPostgresPool(t)
	prepareIndexRBACFixtures(t, pool)
	resolver := legacyrbac.NewPostgresResolver(pool)

	for _, test := range []struct {
		userID   string
		delete   bool
		schedule bool
		search   bool
	}{
		{userID: "1"},
		{userID: "2"},
		{userID: "3", delete: true, schedule: true, search: true},
		{userID: "4", delete: true, schedule: true, search: true},
		{userID: "5", search: true},
		{userID: "6"},
	} {
		resolution, err := resolver.ResolvePermissions(
			context.Background(),
			auth.User{ID: test.userID, UserID: test.userID, AuthType: "user"},
			auth.PermissionModeDefault,
			"1",
		)
		if err != nil {
			t.Fatalf("user=%s resolve permissions: %v", test.userID, err)
		}
		for _, permission := range []struct {
			value string
			want  bool
		}{
			{value: indexingapi.SourceOnlyIndexDeletePermission, want: test.delete},
			{value: indexingapi.SourceOnlyIndexSchedulePermission, want: test.schedule},
			{value: indexingapi.SourceOnlyIndexSearchPermission, want: test.search},
		} {
			granted := containsPermission(resolution.Permissions, permission.value)
			if granted != permission.want {
				t.Fatalf(
					"user=%s permission=%q granted=%v want=%v all=%v",
					test.userID,
					permission.value,
					granted,
					permission.want,
					resolution.Permissions,
				)
			}
		}
	}

	// PATCH .../index_meta/... and GET .../search_options/... are NOT
	// unmounted: internal/api/router.go registers toolkitHandler.IndexMetaUpdate
	// and coreHandler.SearchOptions for these exact paths unconditionally
	// (not gated on any RouterConfig.*Repo field), inside the router's own
	// auth-wrapped /api/v2 group. newIndexRBACRouter doesn't wire
	// RouterConfig.Auth's ForwardedIdentityVerifier/PrincipalValidator (only
	// the per-route ones passed into the CurrentIndex* constructors below),
	// so the outer Auth middleware can't validate the X-Auth-Type/X-Auth-ID
	// headers newIndexRBACRequest sends and 401s before reaching either
	// handler — same as production would for a caller with no valid
	// credentials, not evidence the routes are absent.
	_, router := newIndexRBACRouter(t, pool, nil)
	for _, test := range []struct {
		request *http.Request
		want    int
	}{
		{
			request: newIndexRBACRequest(indexingapi.SourceOnlyIndexScheduleMethod, "/api/v2/elitea_core/index_meta/prompt_lib/1/9/meta-1", "3", nil),
			want:    http.StatusUnauthorized,
		},
		{
			request: newIndexRBACRequest(indexingapi.SourceOnlyIndexSearchMethod, "/api/v2/elitea_core/search_options/prompt_lib/1", "5", nil),
			want:    http.StatusUnauthorized,
		},
	} {
		response := newIndexRBACStreamingRecorder()
		router.ServeHTTP(response, test.request)
		if response.Code != test.want {
			t.Fatalf(
				"%s %s status=%d want=%d body=%s",
				test.request.Method,
				test.request.URL.Path,
				response.Code,
				test.want,
				response.Body.String(),
			)
		}
	}
}

func newIndexRBACRouter(
	t *testing.T,
	pool *pgxpool.Pool,
	canceller indexingapi.CurrentIndexCanceller,
) (*indexRBACSpies, http.Handler) {
	t.Helper()
	resolver := legacyrbac.NewPostgresResolver(pool)
	authConfig := apimw.AuthConfig{
		PrincipalValidator:        authsvc.NewPrincipalValidator(pool),
		ForwardedIdentityVerifier: indexRBACPeerVerifier{},
		SessionSecret:             indexRBACSessionKey,
	}
	start := &indexRBACStartSpy{}
	if canceller == nil {
		canceller = &indexRBACCancelSpy{}
	}
	meta := &indexRBACMetaSpy{}
	metaDelete := &indexRBACMetaDeleteSpy{}
	replay := &indexRBACReplaySpy{}

	startRoute, err := indexingapi.NewCurrentIndexStartRoute(start, authConfig, resolver)
	if err != nil {
		t.Fatal(err)
	}
	cancelRoute, err := indexingapi.NewCurrentIndexCancelRoute(canceller, authConfig, resolver)
	if err != nil {
		t.Fatal(err)
	}
	metaRoute, err := indexingapi.NewCurrentIndexMetaRoute(meta, authConfig, resolver)
	if err != nil {
		t.Fatal(err)
	}
	metaDeleteRoute, err := indexingapi.NewCurrentIndexMetaDeleteRoute(
		metaDelete,
		authConfig,
		resolver,
	)
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := newPostgresPublicAuthorizer(sqlcgen.New(pool), sqlcgen.New(pool), resolver)
	if err != nil {
		t.Fatal(err)
	}
	eventHandler, err := executionapi.NewEventHandler(authorizer, replay, indexRBACWaiter{})
	if err != nil {
		t.Fatal(err)
	}
	runtimeRoutes, err := publicapi.NewProductionRuntimeRoutes(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		http.HandlerFunc(eventHandler.Stream),
		authsvc.NewPrincipalValidator(pool),
		indexRBACPeerVerifier{},
		apimw.AuthConfig{},
	)
	if err != nil {
		t.Fatal(err)
	}

	return &indexRBACSpies{
			start:      start,
			cancel:     canceller,
			meta:       meta,
			metaDelete: metaDelete,
			replay:     replay,
		}, publicapi.NewRouter(publicapi.RouterConfig{
			ProductionRuntime:      runtimeRoutes,
			CurrentIndexStart:      startRoute,
			CurrentIndexCancel:     cancelRoute,
			CurrentIndexMeta:       metaRoute,
			CurrentIndexMetaDelete: metaDeleteRoute,
		})
}

func newIndexRBACRequest(
	method,
	target,
	userID string,
	body io.ReadCloser,
) *http.Request {
	request := httptest.NewRequest(method, target, nil)
	request.Body = body
	request.RemoteAddr = indexRBACPeerAddress
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", userID)
	return request
}

func newIndexRBACPrincipalRequest(
	t *testing.T,
	method,
	target,
	authType,
	userID,
	tokenID string,
	body io.ReadCloser,
) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	request.Body = body
	request.RemoteAddr = indexRBACPeerAddress
	switch authType {
	case "", "user":
		request.Header.Set("X-Auth-Type", "user")
		request.Header.Set("X-Auth-ID", userID)
	case "token":
		request.Header.Set("X-Auth-Type", "token")
		request.Header.Set("X-Auth-ID", tokenID)
		request.Header.Set("X-Auth-User-ID", userID)
	case "session":
		request.AddCookie(&http.Cookie{
			Name:  "elitea_session",
			Value: indexRBACTestSessionCookie(t, userID),
		})
	default:
		t.Fatalf("unsupported test auth type %q", authType)
	}
	return request
}

func indexRBACTestSessionCookie(t *testing.T, userID string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"uid":   userID,
		"email": "ignored-session-email@example.test",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(indexRBACSessionKey))
	if _, err := mac.Write([]byte(encoded)); err != nil {
		t.Fatal(err)
	}
	return encoded + "." + hex.EncodeToString(mac.Sum(nil))
}

func newIndexRBACPostgresPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL index-RBAC integration tests", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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

	databaseName := fmt.Sprintf("elitea_index_rbac_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 6
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("ping isolated PostgreSQL integration database: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(
			dropCtx,
			"DROP DATABASE "+quotedDatabase+" WITH (FORCE)",
		); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})
	return pool
}

func prepareIndexRBACFixtures(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx, `
CREATE SCHEMA centry;
CREATE SCHEMA elitea_runtime;

CREATE TABLE centry.project (
    id BIGINT PRIMARY KEY,
    suspended BOOLEAN NOT NULL DEFAULT FALSE
);
INSERT INTO centry.project (id, suspended) VALUES
    (1, FALSE),
    (2, FALSE),
    (3, TRUE);

CREATE TABLE public.auth_core__user (
    id BIGINT PRIMARY KEY,
    email TEXT,
    suspended BOOLEAN NOT NULL DEFAULT FALSE,
    last_login TIMESTAMP WITHOUT TIME ZONE
);
INSERT INTO public.auth_core__user (id, email, suspended) VALUES
    (1, 'global-super-admin@example.test', FALSE),
    (2, 'platform-admin@example.test', FALSE),
    (3, 'project-admin@example.test', FALSE),
    (4, 'editor@example.test', FALSE),
    (5, 'viewer@example.test', FALSE),
    (6, 'custom@example.test', FALSE),
    (7, 'suspended@example.test', TRUE),
    (8, 'wrong-project@example.test', FALSE),
    (9, 'suspended-project@example.test', FALSE),
    (10, 'nonmember@example.test', FALSE),
    (11, 'custom-delete@example.test', FALSE),
    (12, 'custom-meta-reader@example.test', FALSE),
    (13, 'custom-cancel@example.test', FALSE),
    (14, 'cross-tenant@example.test', FALSE);

CREATE TABLE public.auth_core__token (
    id BIGINT PRIMARY KEY,
    user_id BIGINT NOT NULL,
    expires TIMESTAMP WITHOUT TIME ZONE
);
INSERT INTO public.auth_core__token (id, user_id, expires) VALUES
    (104, 4, (clock_timestamp() AT TIME ZONE 'UTC') + interval '1 hour');

CREATE TABLE public.auth_core__role (
    id BIGINT PRIMARY KEY,
    name TEXT NOT NULL,
    mode TEXT NOT NULL
);
CREATE TABLE public.auth_core__role_permission (
    id BIGINT PRIMARY KEY,
    role_id BIGINT NOT NULL,
    permission TEXT NOT NULL
);
CREATE TABLE public.auth_core__user_role (
    id BIGINT PRIMARY KEY,
    user_id BIGINT NOT NULL,
    role_id BIGINT NOT NULL
);
CREATE TABLE public.auth_core__project_role (
    id BIGINT PRIMARY KEY,
    project_id BIGINT NOT NULL,
    name TEXT NOT NULL
);
CREATE TABLE public.auth_core__project_role_permission (
    id BIGINT PRIMARY KEY,
    project_id BIGINT NOT NULL,
    role_id BIGINT NOT NULL,
    permission TEXT NOT NULL
);
CREATE TABLE public.auth_core__project_user_role (
    id BIGINT PRIMARY KEY,
    project_id BIGINT NOT NULL,
    user_id BIGINT NOT NULL,
    role_id BIGINT NOT NULL
);

INSERT INTO public.auth_core__role (id, name, mode) VALUES
    (10, 'admin', 'default'),
    (11, 'editor', 'default'),
    (12, 'viewer', 'default'),
    (13, 'super_admin', 'administration'),
    (14, 'admin', 'administration');
INSERT INTO public.auth_core__role_permission (id, role_id, permission) VALUES
    (100, 10, 'models.applications.tool.patch'),
    (101, 10, 'models.applications.task.delete'),
    (102, 10, 'models.applications.index_meta.details'),
    (103, 10, 'models.applications.index_meta.delete'),
    (104, 10, 'models.applications.index_meta.edit'),
    (105, 10, 'models.promptlib_shared.search'),
    (110, 11, 'models.applications.tool.patch'),
    (111, 11, 'models.applications.task.delete'),
    (112, 11, 'models.applications.index_meta.details'),
    (113, 11, 'models.applications.index_meta.delete'),
    (114, 11, 'models.applications.index_meta.edit'),
    (115, 11, 'models.promptlib_shared.search'),
    (120, 12, 'models.applications.index_meta.details'),
    (121, 12, 'models.promptlib_shared.search'),
    -- Current baseline: 202602261000_permission_consolidation grants this to viewer.
    (122, 12, 'models.applications.task.delete'),
    (130, 13, 'models.applications.tool.patch'),
    (140, 14, 'models.applications.tool.patch');
INSERT INTO public.auth_core__user_role (id, user_id, role_id) VALUES
    (200, 1, 13),
    (201, 2, 14);

INSERT INTO public.auth_core__project_role (id, project_id, name) VALUES
    (30, 1, 'admin'),
    (31, 1, 'editor'),
    (32, 1, 'viewer'),
    (33, 1, 'custom_index_starter'),
    (34, 2, 'editor'),
    (35, 3, 'editor'),
    (36, 1, 'custom_index_deleter'),
    (37, 1, 'custom_index_metadata_reader'),
    (38, 1, 'custom_index_canceller');
INSERT INTO public.auth_core__project_role_permission (id, project_id, role_id, permission) VALUES
    (300, 1, 33, 'models.applications.tool.patch'),
    (301, 1, 36, 'models.applications.index_meta.delete'),
    (302, 1, 37, 'models.applications.index_meta.details'),
    (303, 1, 38, 'models.applications.task.delete');
INSERT INTO public.auth_core__project_user_role (id, project_id, user_id, role_id) VALUES
    (400, 1, 3, 30),
    (401, 1, 4, 31),
    (402, 1, 5, 32),
    (403, 1, 6, 33),
    (404, 1, 7, 31),
    (405, 2, 8, 34),
    (406, 3, 9, 35),
    (407, 1, 14, 31),
    (408, 2, 14, 34),
    (409, 1, 11, 36),
    (410, 1, 12, 37),
    (411, 1, 13, 38);

CREATE TABLE elitea_runtime.execution_jobs (
    execution_id TEXT PRIMARY KEY,
    generation BIGINT NOT NULL,
    tenant_id TEXT NOT NULL,
    resource_project_id BIGINT NOT NULL,
    projection_project_id BIGINT NOT NULL,
    capability_id TEXT NOT NULL,
    desired_state TEXT NOT NULL,
    state TEXT NOT NULL
);
CREATE TABLE elitea_runtime.index_ingest_jobs (
    execution_id TEXT NOT NULL,
    generation BIGINT NOT NULL,
    capability_id TEXT NOT NULL,
    toolkit_id BIGINT NOT NULL,
    index_name TEXT NOT NULL,
    index_meta_initialized_at TIMESTAMPTZ,
    index_manual_stop_requested_at TIMESTAMPTZ,
    index_manual_cleanup_status TEXT,
    index_manual_cleanup_attempt_count INTEGER,
    index_manual_cleanup_next_attempt_at TIMESTAMPTZ,
    index_manual_cleanup_last_error_code TEXT
);

INSERT INTO elitea_runtime.execution_jobs (
    execution_id,
    generation,
    tenant_id,
    resource_project_id,
    projection_project_id,
    capability_id,
    desired_state,
    state
) VALUES
    ('execution-project-1', 1, '1', 1, 1, 'index.ingest.v1', 'RUNNING', 'RUNNING'),
    ('execution-project-2', 1, '2', 2, 2, 'index.ingest.v1', 'RUNNING', 'RUNNING'),
    ('execution-project-3', 1, '3', 3, 3, 'index.ingest.v1', 'RUNNING', 'RUNNING'),
    ('11111111111111111111111111111111', 1, '1', 1, 1, 'index.ingest.v1', 'RUNNING', 'RUNNING'),
    ('22222222222222222222222222222222', 1, '2', 2, 2, 'index.ingest.v1', 'RUNNING', 'RUNNING'),
    ('33333333333333333333333333333333', 1, '2', 1, 1, 'index.ingest.v1', 'RUNNING', 'RUNNING'),
    ('44444444444444444444444444444444', 1, '1', 1, 1, 'index.ingest.v1', 'RUNNING', 'RUNNING'),
    ('55555555555555555555555555555555', 1, '1', 1, 1, 'index.ingest.v1', 'RUNNING', 'RUNNING'),
    ('66666666666666666666666666666666', 1, '1', 1, 1, 'index.ingest.v1', 'RUNNING', 'RUNNING'),
    ('77777777777777777777777777777777', 1, '1', 1, 1, 'index.ingest.v1', 'RUNNING', 'RUNNING'),
    ('88888888888888888888888888888888', 1, '1', 1, 1, 'index.ingest.v1', 'RUNNING', 'RUNNING');
INSERT INTO elitea_runtime.index_ingest_jobs (
    execution_id,
    generation,
    capability_id,
    toolkit_id,
    index_name
) VALUES
    ('11111111111111111111111111111111', 1, 'index.ingest.v1', 9, 'docs'),
    ('22222222222222222222222222222222', 1, 'index.ingest.v1', 9, 'docs'),
    ('33333333333333333333333333333333', 1, 'index.ingest.v1', 9, 'docs'),
    ('44444444444444444444444444444444', 1, 'index.ingest.v1', 9, 'docs'),
    ('55555555555555555555555555555555', 1, 'index.ingest.v1', 9, 'docs'),
    ('66666666666666666666666666666666', 1, 'index.ingest.v1', 9, 'docs'),
    ('77777777777777777777777777777777', 1, 'index.ingest.v1', 9, 'docs'),
    ('88888888888888888888888888888888', 1, 'index.ingest.v1', 9, 'docs');
`); err != nil {
		t.Fatal(err)
	}
}
