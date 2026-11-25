package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

type tokenRepositoryStub struct {
	listFunc   func(context.Context, int64) ([]tokenRecord, error)
	getFunc    func(context.Context, int64, string) (tokenRecord, error)
	createFunc func(context.Context, tokenCreateInput) (tokenRecord, error)
	deleteFunc func(context.Context, int64, string) error
	setFunc    func(context.Context, tokenAdminAccessUpdate) (tokenRecord, error)
}

func (s tokenRepositoryStub) SetAdminAccess(ctx context.Context, update tokenAdminAccessUpdate) (tokenRecord, error) {
	return s.setFunc(ctx, update)
}

func (s tokenRepositoryStub) List(ctx context.Context, userID int64) ([]tokenRecord, error) {
	return s.listFunc(ctx, userID)
}

func (s tokenRepositoryStub) GetOwned(ctx context.Context, userID int64, tokenUUID string) (tokenRecord, error) {
	return s.getFunc(ctx, userID, tokenUUID)
}

func (s tokenRepositoryStub) Create(ctx context.Context, input tokenCreateInput) (tokenRecord, error) {
	return s.createFunc(ctx, input)
}

func (s tokenRepositoryStub) DeleteOwned(ctx context.Context, userID int64, tokenUUID string) error {
	return s.deleteFunc(ctx, userID, tokenUUID)
}

func TestTokenCreatePersistsCurrentBaselineContractForOwningUser(t *testing.T) {
	const tokenUUID = "8ce4be49-0d10-4f05-a63f-d6d46f99a3f0"
	secret := []byte("test-secret")
	var gotOwnerID int64
	var gotName *string
	var gotExpires *time.Time
	var gotProjectID *int64
	handler := &Handler{
		tokenSigningKey: secret,
		tokens: tokenRepositoryStub{
			createFunc: func(_ context.Context, input tokenCreateInput) (tokenRecord, error) {
				gotOwnerID, gotName, gotExpires = input.OwnerID, input.Name, input.Expires
				gotProjectID = input.ProjectID
				return tokenRecord{
					ID:      11,
					UUID:    stringAddress(tokenUUID),
					Expires: input.Expires,
					UserID:  input.OwnerID,
					Name:    input.Name,
				}, nil
			},
		},
	}

	req := browserSessionTokenRequest("/token/", `{"name":"ci-token","expires":{"measure":"hours","value":24}}`)
	rec := httptest.NewRecorder()
	handler.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if gotOwnerID != 7 {
		t.Fatalf("persisted owner = %d, want 7, the browser session's user", gotOwnerID)
	}
	if gotName == nil || *gotName != "ci-token" || gotExpires == nil {
		t.Fatalf("persisted token name=%v expires=%v", gotName, gotExpires)
	}
	if gotProjectID != nil {
		t.Fatalf("persisted project = %v, want nil; a request without project_id creates an unbound token", *gotProjectID)
	}
	remaining := time.Until(*gotExpires)
	if remaining < 23*time.Hour+59*time.Minute || remaining > 24*time.Hour+time.Minute {
		t.Fatalf("expiry remaining = %s, want approximately 24h", remaining)
	}

	var response map[string]json.RawMessage
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"id", "uuid", "expires", "user_id", "name", "project_id", "token"} {
		if _, ok := response[required]; !ok {
			t.Fatalf("response is missing current-baseline field %q: %v", required, response)
		}
	}
	for _, obsolete := range []string{"created_at", "prefix"} {
		if _, ok := response[obsolete]; ok {
			t.Fatalf("response contains non-baseline field %q", obsolete)
		}
	}
	if string(response["project_id"]) != "null" {
		t.Fatalf("project_id = %s, want null for an unbound token", response["project_id"])
	}
	var encoded string
	if err := json.Unmarshal(response["token"], &encoded); err != nil {
		t.Fatal(err)
	}
	parsed, err := jwt.ParseWithClaims(encoded, &baselineTokenClaims{}, func(token *jwt.Token) (any, error) {
		if token.Method.Alg() != jwt.SigningMethodHS512.Alg() {
			t.Fatalf("signing algorithm = %s, want HS512", token.Method.Alg())
		}
		return secret, nil
	})
	if err != nil || !parsed.Valid {
		t.Fatalf("created token is not a valid current-baseline HS512 JWT: valid=%v err=%v", parsed.Valid, err)
	}
	claims := parsed.Claims.(*baselineTokenClaims)
	if claims.UUID == nil || *claims.UUID != tokenUUID || claims.Expires == nil {
		t.Fatalf("claims = %+v", claims)
	}
}

func TestTokenCreateRejectsTrailingJSONDocument(t *testing.T) {
	createCalls := 0
	handler := &Handler{
		tokenSigningKey: []byte("test-secret"),
		tokens: tokenRepositoryStub{
			createFunc: func(context.Context, tokenCreateInput) (tokenRecord, error) {
				createCalls++
				return tokenRecord{}, nil
			},
		},
	}

	req := browserSessionTokenRequest("/token/", `{"name":"first"}{"name":"second"}`)
	rec := httptest.NewRecorder()
	handler.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if createCalls != 0 {
		t.Fatalf("repository create calls = %d, want 0", createCalls)
	}
}

func TestTokenCreateDistinguishesMissingAndExplicitNullName(t *testing.T) {
	const tokenUUID = "8ce4be49-0d10-4f05-a63f-d6d46f99a3f0"

	t.Run("missing", func(t *testing.T) {
		createCalls := 0
		handler := &Handler{
			tokenSigningKey: []byte("test-secret"),
			tokens: tokenRepositoryStub{
				createFunc: func(context.Context, tokenCreateInput) (tokenRecord, error) {
					createCalls++
					return tokenRecord{}, nil
				},
			},
		}
		rec := httptest.NewRecorder()
		handler.Routes().ServeHTTP(rec, browserSessionTokenRequest("/token/", `{}`))
		if rec.Code != http.StatusBadRequest || createCalls != 0 {
			t.Fatalf("status = %d, create calls = %d, body = %s", rec.Code, createCalls, rec.Body.String())
		}
	})

	t.Run("explicit null", func(t *testing.T) {
		createCalls := 0
		handler := &Handler{
			tokenSigningKey: []byte("test-secret"),
			tokens: tokenRepositoryStub{
				createFunc: func(_ context.Context, input tokenCreateInput) (tokenRecord, error) {
					createCalls++
					if input.Name != nil || input.Expires != nil {
						t.Fatalf("name = %v, expires = %v; want explicit nulls", input.Name, input.Expires)
					}
					return tokenRecord{ID: 11, UUID: stringAddress(tokenUUID), UserID: input.OwnerID}, nil
				},
			},
		}
		rec := httptest.NewRecorder()
		handler.Routes().ServeHTTP(rec, browserSessionTokenRequest("/token/", `{"name":null}`))
		if rec.Code != http.StatusOK || createCalls != 1 {
			t.Fatalf("status = %d, create calls = %d, body = %s", rec.Code, createCalls, rec.Body.String())
		}
		var response map[string]json.RawMessage
		if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
			t.Fatal(err)
		}
		if string(response["name"]) != "null" {
			t.Fatalf("name = %s, want null", response["name"])
		}
	})
}

// TestTokenCreateBindsRequestedProject covers the create half of ADR-0018: a
// present project_id reaches the repository unchanged, and the response reports
// the project the key bills.
func TestTokenCreateBindsRequestedProject(t *testing.T) {
	const tokenUUID = "8ce4be49-0d10-4f05-a63f-d6d46f99a3f0"
	var gotProjectID *int64
	handler := &Handler{
		tokenSigningKey: []byte("test-secret"),
		tokens: tokenRepositoryStub{
			createFunc: func(_ context.Context, input tokenCreateInput) (tokenRecord, error) {
				gotProjectID = input.ProjectID
				return tokenRecord{
					ID:        11,
					UUID:      stringAddress(tokenUUID),
					UserID:    input.OwnerID,
					Name:      input.Name,
					ProjectID: input.ProjectID,
				}, nil
			},
		},
	}

	rec := httptest.NewRecorder()
	handler.Routes().ServeHTTP(rec, browserSessionTokenRequest(
		"/token/", `{"name":"team-key","project_id":42}`,
	))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotProjectID == nil || *gotProjectID != 42 {
		t.Fatalf("repository project = %v, want 42", gotProjectID)
	}
	var response Token
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.ProjectID == nil || *response.ProjectID != 42 {
		t.Fatalf("response project = %v, want 42", response.ProjectID)
	}
}

// TestTokenCreateTreatsAbsentAndNullProjectAsUnbound pins the default. Unbound
// is the current behaviour and every existing client depends on it.
func TestTokenCreateTreatsAbsentAndNullProjectAsUnbound(t *testing.T) {
	const tokenUUID = "8ce4be49-0d10-4f05-a63f-d6d46f99a3f0"
	for name, body := range map[string]string{
		"absent":       `{"name":"personal-key"}`,
		"explicitNull": `{"name":"personal-key","project_id":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			var seen bool
			var gotProjectID *int64
			handler := &Handler{
				tokenSigningKey: []byte("test-secret"),
				tokens: tokenRepositoryStub{
					createFunc: func(_ context.Context, input tokenCreateInput) (tokenRecord, error) {
						seen, gotProjectID = true, input.ProjectID
						return tokenRecord{ID: 11, UUID: stringAddress(tokenUUID), UserID: input.OwnerID}, nil
					},
				},
			}
			rec := httptest.NewRecorder()
			handler.Routes().ServeHTTP(rec, browserSessionTokenRequest("/token/", body))
			if rec.Code != http.StatusOK || !seen {
				t.Fatalf("status = %d, create called = %v, body = %s", rec.Code, seen, rec.Body.String())
			}
			if gotProjectID != nil {
				t.Fatalf("repository project = %d, want unbound", *gotProjectID)
			}
			var response map[string]json.RawMessage
			if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
				t.Fatal(err)
			}
			if string(response["project_id"]) != "null" {
				t.Fatalf("project_id = %s, want null", response["project_id"])
			}
		})
	}
}

// TestTokenCreateRefusesNonMemberProject is the §8 error contract for a project
// the owner cannot reach: 403, permission_error, project_forbidden.
func TestTokenCreateRefusesNonMemberProject(t *testing.T) {
	handler := &Handler{
		tokenSigningKey: []byte("test-secret"),
		tokens: tokenRepositoryStub{
			createFunc: func(context.Context, tokenCreateInput) (tokenRecord, error) {
				return tokenRecord{}, errTokenProjectForbidden
			},
		},
	}

	rec := httptest.NewRecorder()
	handler.Routes().ServeHTTP(rec, browserSessionTokenRequest(
		"/token/", `{"name":"team-key","project_id":42}`,
	))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}
	var response tokenJSONError
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Error.Type != "permission_error" || response.Error.Code != "project_forbidden" {
		t.Fatalf("error envelope = %+v", response.Error)
	}
	if !strings.Contains(response.Error.Message, "42") {
		t.Fatalf("message = %q, want the requested project named", response.Error.Message)
	}
}

// TestTokenCreateRefusesMalformedProjectID is the §8 error contract for a
// project_id no membership check can be run against. Every case must produce
// invalid_project_id and must NOT reach the repository, because a token created
// and then refused is a token that exists.
func TestTokenCreateRefusesMalformedProjectID(t *testing.T) {
	for name, body := range map[string]string{
		"zero":         `{"name":"k","project_id":0}`,
		"negative":     `{"name":"k","project_id":-1}`,
		"aboveInt32":   `{"name":"k","project_id":2147483648}`,
		"string":       `{"name":"k","project_id":"42"}`,
		"float":        `{"name":"k","project_id":42.5}`,
		"boolean":      `{"name":"k","project_id":true}`,
		"object":       `{"name":"k","project_id":{"id":42}}`,
		"emptyString":  `{"name":"k","project_id":""}`,
		"numericArray": `{"name":"k","project_id":[42]}`,
	} {
		t.Run(name, func(t *testing.T) {
			createCalls := 0
			handler := &Handler{
				tokenSigningKey: []byte("test-secret"),
				tokens: tokenRepositoryStub{
					createFunc: func(context.Context, tokenCreateInput) (tokenRecord, error) {
						createCalls++
						return tokenRecord{}, nil
					},
				},
			}
			rec := httptest.NewRecorder()
			handler.Routes().ServeHTTP(rec, browserSessionTokenRequest("/token/", body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
			}
			if createCalls != 0 {
				t.Fatalf("repository create calls = %d, want 0", createCalls)
			}
			var response tokenJSONError
			if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
				t.Fatal(err)
			}
			if response.Error.Type != "invalid_request_error" || response.Error.Code != "invalid_project_id" {
				t.Fatalf("error envelope = %+v", response.Error)
			}
		})
	}
}

// TestTokenReadsReportProjectBinding proves a user can see what a key bills.
// Without this, a bound key is indistinguishable from an unbound one.
func TestTokenReadsReportProjectBinding(t *testing.T) {
	const tokenUUID = "8ce4be49-0d10-4f05-a63f-d6d46f99a3f0"
	bound := int64(42)
	handler := &Handler{
		tokenSigningKey: []byte("test-secret"),
		tokens: tokenRepositoryStub{
			listFunc: func(_ context.Context, ownerID int64) ([]tokenRecord, error) {
				return []tokenRecord{
					{ID: 11, UUID: stringAddress(tokenUUID), UserID: ownerID, ProjectID: &bound},
					{ID: 12, UUID: stringAddress(tokenUUID), UserID: ownerID},
				}, nil
			},
			getFunc: func(_ context.Context, ownerID int64, _ string) (tokenRecord, error) {
				return tokenRecord{ID: 11, UUID: stringAddress(tokenUUID), UserID: ownerID, ProjectID: &bound}, nil
			},
		},
	}

	rec := httptest.NewRecorder()
	handler.Routes().ServeHTTP(rec, authenticatedTokenRequest(http.MethodGet, "/token/", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var listed []Token
	if err := json.NewDecoder(rec.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].ProjectID == nil || *listed[0].ProjectID != 42 || listed[1].ProjectID != nil {
		t.Fatalf("listed bindings = %+v", listed)
	}

	rec = httptest.NewRecorder()
	handler.Routes().ServeHTTP(rec, authenticatedTokenRequest(http.MethodGet, "/token/"+tokenUUID, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var single Token
	if err := json.NewDecoder(rec.Body).Decode(&single); err != nil {
		t.Fatal(err)
	}
	if single.ProjectID == nil || *single.ProjectID != 42 {
		t.Fatalf("get binding = %v, want 42", single.ProjectID)
	}
}

// TestTokenRoutesExposeNoBindingUpdatePath keeps "what does this key bill" a
// fact about a key. A binding is set once, at creation. To change scope, a user
// creates a new token and deletes the old one (spec-llm-project-scope §4).
func TestTokenRoutesExposeNoBindingUpdatePath(t *testing.T) {
	const tokenUUID = "8ce4be49-0d10-4f05-a63f-d6d46f99a3f0"
	handler := &Handler{
		tokenSigningKey: []byte("test-secret"),
		tokens:          tokenRepositoryStub{},
	}
	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		for _, target := range []string{"/token/", "/token/" + tokenUUID} {
			rec := httptest.NewRecorder()
			handler.Routes().ServeHTTP(rec, authenticatedTokenRequest(method, target, `{"project_id":42}`))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s status = %d, want 405", method, target, rec.Code)
			}
		}
	}
}

func TestTokenListMasksReconstructedTokensAndScopesByOwner(t *testing.T) {
	const tokenUUID = "8ce4be49-0d10-4f05-a63f-d6d46f99a3f0"
	var gotOwnerID int64
	handler := &Handler{
		tokenSigningKey: []byte("test-secret"),
		tokens: tokenRepositoryStub{
			listFunc: func(_ context.Context, ownerID int64) ([]tokenRecord, error) {
				gotOwnerID = ownerID
				return []tokenRecord{{ID: 11, UUID: stringAddress(tokenUUID), UserID: ownerID, Name: stringAddress("ci-token")}}, nil
			},
		},
	}

	req := authenticatedTokenRequest(http.MethodGet, "/token/", "")
	rec := httptest.NewRecorder()
	handler.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotOwnerID != 7 {
		t.Fatalf("listed owner = %d, want 7", gotOwnerID)
	}
	var response []Token
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if len(response) != 1 || len(response[0].Token) != 10 || !strings.HasPrefix(response[0].Token, "...") {
		t.Fatalf("masked response = %+v", response)
	}
}

func TestTokenListPreservesNullableCurrentBaselineMetadata(t *testing.T) {
	handler := &Handler{
		tokenSigningKey: []byte("test-secret"),
		tokens: tokenRepositoryStub{
			listFunc: func(_ context.Context, ownerID int64) ([]tokenRecord, error) {
				return []tokenRecord{{ID: 11, UserID: ownerID}}, nil
			},
		},
	}

	req := authenticatedTokenRequest(http.MethodGet, "/token/", "")
	rec := httptest.NewRecorder()
	handler.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response []map[string]json.RawMessage
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if len(response) != 1 || string(response[0]["uuid"]) != "null" || string(response[0]["name"]) != "null" {
		t.Fatalf("nullable metadata response = %s", rec.Body.String())
	}
}

func TestTokenDeletePreservesNotFoundAndOwnershipFailures(t *testing.T) {
	const tokenUUID = "8ce4be49-0d10-4f05-a63f-d6d46f99a3f0"
	for _, test := range []struct {
		name   string
		err    error
		status int
	}{
		{name: "missing", err: errTokenNotFound, status: http.StatusBadRequest},
		{name: "different owner", err: errTokenForbidden, status: http.StatusForbidden},
		{name: "deleted", status: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := &Handler{
				tokenSigningKey: []byte("test-secret"),
				tokens: tokenRepositoryStub{
					deleteFunc: func(_ context.Context, ownerID int64, gotUUID string) error {
						if ownerID != 7 || gotUUID != tokenUUID {
							t.Fatalf("delete owner=%d uuid=%q", ownerID, gotUUID)
						}
						return test.err
					},
				},
			}
			req := authenticatedTokenRequest(http.MethodDelete, "/token/"+tokenUUID, "")
			rec := httptest.NewRecorder()
			handler.Routes().ServeHTTP(rec, req)
			if rec.Code != test.status {
				t.Fatalf("status = %d, want %d, body=%s", rec.Code, test.status, rec.Body.String())
			}
		})
	}
}

func TestTokenServiceFailsClosedWithoutDatabaseOrSigningKey(t *testing.T) {
	for _, handler := range []*Handler{
		{tokenSigningKey: []byte("configured")},
		{tokens: tokenRepositoryStub{listFunc: func(context.Context, int64) ([]tokenRecord, error) { return nil, errors.New("unused") }}},
	} {
		req := authenticatedTokenRequest(http.MethodGet, "/token/", "")
		rec := httptest.NewRecorder()
		handler.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}
	}
}

func authenticatedTokenRequest(method, target, body string) *http.Request {
	req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	principal := identity.User{
		ID:       "42",
		TokenID:  "42",
		UserID:   "7",
		AuthType: "token",
	}
	return req.WithContext(identity.ContextWithUser(req.Context(), principal))
}

// browserSessionTokenRequest authenticates as user 7 through a browser
// session, the credential that may mint a personal access token (issue 2559).
func browserSessionTokenRequest(target, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewBufferString(body))
	principal := identity.User{ID: "7", UserID: "7", BrowserSessionRef: 3}
	return req.WithContext(identity.ContextWithAuthenticatedUser(
		req.Context(), principal, identity.AuthenticationSourceSession))
}

func stringAddress(value string) *string {
	return &value
}

// ---- Administration scopes (ADR-0059 R1.3a, issue 2507) --------------------

// scopeClock is the fixed "now" of the scope tests; the limit is 15 minutes.
var scopeClock = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

const scopeTokenUUID = "8ce4be49-0d10-4f05-a63f-d6d46f99a3f0"

// scopeResolver answers the caller's administration set, and an elevation
// when elevated is set (the resolver's indicator, R2.1's).
type scopeResolver struct {
	set      []string
	elevated bool
	err      error
}

func (s scopeResolver) ResolvePermissions(_ context.Context, _ identity.User, mode, _ string) (identity.PermissionResolution, error) {
	if mode != identity.PermissionModeAdministration {
		return identity.PermissionResolution{}, errors.New("unexpected mode " + mode)
	}
	if s.err != nil {
		return identity.PermissionResolution{}, s.err
	}
	resolution := identity.PermissionResolution{UserID: 7, Permissions: s.set}
	if s.elevated {
		resolution.ElevationID = "0b9f7e8a-3c2d-4e1f-9a8b-7c6d5e4f3a2b"
	}
	return resolution, nil
}

// adminCallerSet is what the caller of most scope tests holds: two token
// scopes and identity trust, which is never one.
var adminCallerSet = []string{"admin.auth.users", "access.roles.view", "identity.sso.trust.manage"}

// scopeCaller is a request principal of the scope tests.
type scopeCaller struct {
	user   identity.User
	source identity.AuthenticationSource
	bearer string
}

// browserScopeCaller is an OIDC browser session that signed in signedInAgo
// before scopeClock.
func browserScopeCaller(kind identity.CredentialKind, provider string, signedInAgo time.Duration) scopeCaller {
	return scopeCaller{
		user: identity.User{
			ID: "7", UserID: "7", AuthType: "session", CredentialKind: kind, BrowserSessionRef: 3,
			BrowserSessionProvider: provider, BrowserSessionAuthenticationProven: true,
			BrowserSessionAuthenticatedAt: scopeClock.Add(-signedInAgo),
		},
		source: identity.AuthenticationSourceSession,
	}
}

var (
	freshSession = browserScopeCaller(identity.CredentialKindSession, "oidc", time.Minute)
	staleSession = browserScopeCaller(identity.CredentialKindSession, "saml", time.Hour)
	sealedCaller = browserScopeCaller(identity.CredentialKindSealed, "sealed", time.Minute)
	noneToken    = scopeCaller{
		user: identity.User{ID: "7", UserID: "7", TokenID: "42", AuthType: "token",
			CredentialKind: identity.CredentialKindToken},
		source: identity.AuthenticationSourceToken, bearer: "eyJhbGciOiJIUzUxMiJ9.e30.sig",
	}
	nativeSession = scopeCaller{
		user: identity.User{ID: "7", UserID: "7", TokenID: "44", AuthType: "token",
			CredentialKind: identity.CredentialKindNative, NativeClientID: "elitea-desktop"},
		source: identity.AuthenticationSourceToken, bearer: "elnat_abc",
	}
	runToken = scopeCaller{
		user: identity.User{ID: "7", UserID: "7", TokenID: "43", AuthType: "token",
			CredentialKind: identity.CredentialKindRun},
		source: identity.AuthenticationSourceToken, bearer: "elrun_abc",
	}
)

type scopeOutcome struct {
	status  int
	code    string
	scopes  []string
	created *tokenCreateInput
	set     *tokenAdminAccessUpdate
	header  http.Header
	body    string
	audit   string
}

// scopeHandler is a handler whose repository records what it is asked to
// write, over resolver, with the scope clock.
func scopeHandler(resolver identity.PermissionResolver, out *scopeOutcome) *Handler {
	return &Handler{
		tokenSigningKey:      []byte("test-secret"),
		permissions:          resolver,
		recentAuthentication: apimw.RecentAuthentication{MaxAge: 15 * time.Minute, Now: func() time.Time { return scopeClock }},
		tokens: tokenRepositoryStub{
			createFunc: func(_ context.Context, input tokenCreateInput) (tokenRecord, error) {
				copied := input
				out.created = &copied
				return tokenRecord{ID: 11, UUID: stringAddress(scopeTokenUUID), UserID: input.OwnerID,
					Name: input.Name, Admin: input.Admin}, nil
			},
			setFunc: func(_ context.Context, update tokenAdminAccessUpdate) (tokenRecord, error) {
				copied := update
				out.set = &copied
				return tokenRecord{ID: 11, UUID: stringAddress(update.UUID), UserID: update.OwnerID, Admin: update.Admin}, nil
			},
		},
	}
}

func scopeRequest(t *testing.T, caller scopeCaller, resolver identity.PermissionResolver, method, target, body string) scopeOutcome {
	t.Helper()
	var out scopeOutcome
	handler := scopeHandler(resolver, &out)
	req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	if caller.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+caller.bearer)
	}
	ctx := identity.ContextWithAuthenticatedUser(req.Context(), caller.user, caller.source)
	ctx, slot := audit.ContextWithAnnotationSlot(ctx)
	rec := httptest.NewRecorder()
	handler.Routes().ServeHTTP(rec, req.WithContext(ctx))
	out.status, out.header, out.body = rec.Code, rec.Header(), rec.Body.String()
	if annotation, ok := slot.Read(); ok {
		out.audit = annotation.Action + ":" + annotation.EntityName
	}
	var envelope struct {
		Error struct {
			Code   string   `json:"code"`
			Scopes []string `json:"scopes"`
		} `json:"error"`
	}
	if rec.Code >= 400 && json.Unmarshal(rec.Body.Bytes(), &envelope) == nil {
		out.code, out.scopes = envelope.Error.Code, envelope.Error.Scopes
	}
	return out
}

func createScoped(scopes ...string) string {
	encoded, _ := json.Marshal(scopes)
	return `{"name":"ci","admin_access":"scoped","admin_scopes":` + string(encoded) + `}`
}

func requireScopeRefusal(t *testing.T, got scopeOutcome, code string) {
	t.Helper()
	if got.status != http.StatusForbidden || got.code != code {
		t.Fatalf("status %d code %q, want 403 %s: %s", got.status, got.code, code, got.body)
	}
	if got.created != nil || got.set != nil {
		t.Fatalf("a refused request wrote: created %+v set %+v", got.created, got.set)
	}
	if !strings.HasSuffix(got.audit, ":"+code) {
		t.Fatalf("audit annotation %q, want the code %s", got.audit, code)
	}
}

// Acceptance: a scope outside the calling credential's administration set,
// or a scope the registry marks token: never, answers 403.
func TestTokenCreateRefusesAScopeOutsideTheCallersSetOrMarkedNever(t *testing.T) {
	resolver := scopeResolver{set: adminCallerSet}
	notHeld := scopeRequest(t, freshSession, resolver, http.MethodPost, "/token/", createScoped("admin.auth.users", "llm.policy.manage"))
	requireScopeRefusal(t, notHeld, CodeScopeNotHeld)
	if strings.Join(notHeld.scopes, ",") != "llm.policy.manage" {
		t.Fatalf("refused scopes %v, want the one not held", notHeld.scopes)
	}
	if notHeld.audit != TokenMintRefusedAuditAction+":"+CodeScopeNotHeld {
		t.Fatalf("audit %q", notHeld.audit)
	}
	// Held, and still never a token scope: identity trust, and a name that
	// is no administration permission at all.
	for _, never := range []string{"identity.sso.trust.manage", "admin.auth.users.super_admin", "no.such.permission"} {
		got := scopeRequest(t, freshSession, scopeResolver{set: append(slices.Clone(adminCallerSet), never)},
			http.MethodPost, "/token/", createScoped("admin.auth.users", never))
		requireScopeRefusal(t, got, CodeScopeNotAllowed)
		if strings.Join(got.scopes, ",") != never {
			t.Fatalf("refused scopes %v, want %s", got.scopes, never)
		}
	}
}

// Acceptance (three tests): a none token, a native session and a run token
// each get 403 when they ask for admin_scopes, on the create and on the
// scope-change route.
func TestANoneTokenGets403ForAdminScopes(t *testing.T) {
	requireCredentialRefusedScopes(t, noneToken, CodeBrowserSessionRequired)
}

func TestANativeSessionGets403ForAdminScopes(t *testing.T) {
	// The owner of a native session that may mint holds no administration
	// permission; the scope rule refuses the kind whatever the owner holds.
	requireCredentialRefusedScopes(t, nativeSession, CodeScopesBrowserSessionRequired)
}

func TestARunTokenGets403ForAdminScopes(t *testing.T) {
	requireCredentialRefusedScopes(t, runToken, CodeBrowserSessionRequired)
}

// requireCredentialRefusedScopes asks for scopes on both routes. createCode is
// the create's refusal: the mint rule refuses a token and a run token before
// the scope rule runs; the scope-change route answers the scope rule's code.
func requireCredentialRefusedScopes(t *testing.T, caller scopeCaller, createCode string) {
	t.Helper()
	resolver := scopeResolver{}
	if caller.user.CredentialKind != identity.CredentialKindNative {
		resolver.set = adminCallerSet
	}
	created := scopeRequest(t, caller, resolver, http.MethodPost, "/token/", createScoped("admin.auth.users"))
	requireScopeRefusal(t, created, createCode)
	changed := scopeRequest(t, caller, scopeResolver{set: adminCallerSet}, http.MethodPut,
		"/token/"+scopeTokenUUID+"/admin_scopes", `{"admin_access":"scoped","admin_scopes":["admin.auth.users"]}`)
	requireScopeRefusal(t, changed, CodeScopesBrowserSessionRequired)
	if changed.audit != TokenAdminScopesRefusedAuditAction+":"+CodeScopesBrowserSessionRequired {
		t.Fatalf("audit %q", changed.audit)
	}
}

// Acceptance (two tests): a sealed session and an elevated session get 403.
func TestASealedSessionGets403ForAdminScopes(t *testing.T) {
	got := scopeRequest(t, sealedCaller, scopeResolver{set: adminCallerSet}, http.MethodPost, "/token/", createScoped("admin.auth.users"))
	requireScopeRefusal(t, got, CodeScopesSealedSession)
}

func TestAnElevatedSessionGets403ForAdminScopes(t *testing.T) {
	elevated := scopeResolver{set: adminCallerSet, elevated: true}
	got := scopeRequest(t, freshSession, elevated, http.MethodPost, "/token/", createScoped("admin.auth.users"))
	requireScopeRefusal(t, got, CodeScopesElevated)
	got = scopeRequest(t, freshSession, elevated, http.MethodPut, "/token/"+scopeTokenUUID+"/admin_scopes",
		`{"admin_access":"none"}`)
	requireScopeRefusal(t, got, CodeScopesElevated)
}

// Acceptance: a browser session whose sign-in is older than the configured
// time answers 401 reauthentication_required on a request with admin_scopes,
// with the limit and the sign-in method; the same request passes after a
// fresh sign-in.
func TestAdminScopesNeedARecentSignIn(t *testing.T) {
	resolver := scopeResolver{set: adminCallerSet}
	body := createScoped("admin.auth.users")
	stale := scopeRequest(t, staleSession, resolver, http.MethodPost, "/token/", body)
	if stale.status != http.StatusUnauthorized || stale.code != apimw.CodeReauthenticationRequired {
		t.Fatalf("stale sign-in: status %d code %q, want 401 reauthentication_required: %s", stale.status, stale.code, stale.body)
	}
	if stale.created != nil {
		t.Fatalf("a stale sign-in created a token: %+v", stale.created)
	}
	if got := stale.header.Get(apimw.HeaderReauthenticationMaxAge); got != "900" {
		t.Fatalf("max age header %q, want 900", got)
	}
	if got := stale.header.Get(apimw.HeaderReauthenticationMethod); got != "saml" {
		t.Fatalf("method header %q, want saml", got)
	}

	// The same person after a fresh sign-in.
	fresh := staleSession
	fresh.user.BrowserSessionAuthenticatedAt = scopeClock.Add(-time.Minute)
	got := scopeRequest(t, fresh, resolver, http.MethodPost, "/token/", body)
	if got.status != http.StatusOK || got.created == nil {
		t.Fatalf("fresh sign-in: status %d: %s", got.status, got.body)
	}
	if got.created.Admin.Access != "scoped" || strings.Join(got.created.Admin.Scopes, ",") != "admin.auth.users" ||
		got.created.OnAdminAccess == nil {
		t.Fatalf("created %+v, want scoped [admin.auth.users] with its event hook", got.created.Admin)
	}
	var token Token
	if err := json.Unmarshal([]byte(got.body), &token); err != nil {
		t.Fatal(err)
	}
	if token.AdminAccess != "scoped" || strings.Join(token.AdminScopes, ",") != "admin.auth.users" || token.InheritEndsOn != "" {
		t.Fatalf("response %+v", token)
	}

	// A Form session is a browser session too, and a sign-in of unknown age
	// (a SAML sign-in without ForceAuthn) is refused like a stale one.
	form := browserScopeCaller(identity.CredentialKindForm, "form", time.Minute)
	if got := scopeRequest(t, form, resolver, http.MethodPost, "/token/", body); got.status != http.StatusOK {
		t.Fatalf("fresh Form session: status %d: %s", got.status, got.body)
	}
	unproven := freshSession
	unproven.user.BrowserSessionAuthenticationProven = false
	if got := scopeRequest(t, unproven, resolver, http.MethodPost, "/token/", body); got.status != http.StatusUnauthorized ||
		got.header.Get(apimw.HeaderReauthenticationMethod) != "oidc" {
		t.Fatalf("unproven sign-in: status %d method %q", got.status, got.header.Get(apimw.HeaderReauthenticationMethod))
	}
}

// A create without scopes is the default none token: no scope rule, no
// recent sign-in, and inherit is never accepted.
func TestTokenCreateWithoutScopesIsNoneAndInheritIsRefused(t *testing.T) {
	resolver := scopeResolver{set: adminCallerSet}
	got := scopeRequest(t, staleSession, resolver, http.MethodPost, "/token/", `{"name":"ci"}`)
	if got.status != http.StatusOK || got.created == nil || got.created.Admin.granted() {
		t.Fatalf("status %d created %+v, want a none token from a stale session", got.status, got.created)
	}
	var token Token
	if err := json.Unmarshal([]byte(got.body), &token); err != nil {
		t.Fatal(err)
	}
	if token.AdminAccess != "none" || token.AdminScopes == nil || len(token.AdminScopes) != 0 {
		t.Fatalf("response %+v, want admin_access none and admin_scopes []", token)
	}
	for body, code := range map[string]string{
		`{"name":"ci","admin_access":"inherit"}`:                                   "invalid_admin_access",
		`{"name":"ci","admin_access":"scoped"}`:                                    "invalid_admin_scopes",
		`{"name":"ci","admin_access":"scoped","admin_scopes":[]}`:                  "invalid_admin_scopes",
		`{"name":"ci","admin_access":"none","admin_scopes":["access.roles.view"]}`: "invalid_admin_scopes",
		`{"name":"ci","admin_scopes":["access.roles.view"]}`:                       "invalid_admin_scopes",
		`{"name":"ci","admin_access":"scoped","admin_scopes":[""]}`:                "invalid_admin_scopes",
	} {
		got := scopeRequest(t, freshSession, resolver, http.MethodPost, "/token/", body)
		if got.status != http.StatusBadRequest || got.code != code || got.created != nil {
			t.Errorf("%s: status %d code %q, want 400 %s", body, got.status, got.code, code)
		}
	}
}

// The scope-change route replaces the scopes under the same rules, and
// dropping every scope needs no recent sign-in.
func TestTokenSetAdminScopesReplacesUnderTheSameRules(t *testing.T) {
	resolver := scopeResolver{set: adminCallerSet}
	target := "/token/" + scopeTokenUUID + "/admin_scopes"
	got := scopeRequest(t, freshSession, resolver, http.MethodPut, target,
		`{"admin_access":"scoped","admin_scopes":["access.roles.view","admin.auth.users","access.roles.view"]}`)
	if got.status != http.StatusOK || got.set == nil {
		t.Fatalf("status %d: %s", got.status, got.body)
	}
	if got.set.OwnerID != 7 || got.set.UUID != scopeTokenUUID || got.set.OnChange == nil ||
		strings.Join(got.set.Admin.Scopes, ",") != "access.roles.view,admin.auth.users" {
		t.Fatalf("update %+v, want the sorted, de-duplicated scopes of user 7's token with its event hook", got.set)
	}
	requireScopeRefusal(t, scopeRequest(t, freshSession, resolver, http.MethodPut, target,
		`{"admin_access":"scoped","admin_scopes":["llm.usage.view"]}`), CodeScopeNotHeld)
	if got := scopeRequest(t, staleSession, resolver, http.MethodPut, target,
		`{"admin_access":"scoped","admin_scopes":["admin.auth.users"]}`); got.status != http.StatusUnauthorized || got.set != nil {
		t.Fatalf("stale widening: status %d", got.status)
	}
	if got := scopeRequest(t, staleSession, resolver, http.MethodPut, target, `{"admin_access":"none"}`); got.status != http.StatusOK ||
		got.set == nil || got.set.Admin.Access != "none" {
		t.Fatalf("dropping every scope from a stale session: status %d: %s", got.status, got.body)
	}
	for _, body := range []string{`{}`, `{"admin_access":"inherit"}`} {
		if got := scopeRequest(t, freshSession, resolver, http.MethodPut, target, body); got.status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, got.status)
		}
	}
}

// The offer is the caller's own set intersected with the token scopes, and
// nothing for a credential that may set none.
func TestTokenAdminScopeOfferIsTheCallersTokenScopes(t *testing.T) {
	got := scopeRequest(t, freshSession, scopeResolver{set: adminCallerSet}, http.MethodGet, "/token/admin_scopes", "")
	var offer tokenScopeOffer
	if err := json.Unmarshal([]byte(got.body), &offer); err != nil || got.status != http.StatusOK {
		t.Fatalf("status %d body %s: %v", got.status, got.body, err)
	}
	var names []string
	for _, scope := range offer.Scopes {
		names = append(names, scope.Name)
	}
	if !offer.Settable || strings.Join(names, ",") != "access.roles.view,admin.auth.users" {
		t.Fatalf("offer %+v, want the two token scopes and not identity trust", offer)
	}
	for _, caller := range []scopeCaller{nativeSession, noneToken, sealedCaller} {
		got := scopeRequest(t, caller, scopeResolver{set: adminCallerSet}, http.MethodGet, "/token/admin_scopes", "")
		if err := json.Unmarshal([]byte(got.body), &offer); err != nil || offer.Settable || len(offer.Scopes) != 0 || offer.Code == "" {
			t.Fatalf("%s: offer %s, want nothing settable with a code", caller.user.CredentialKind, got.body)
		}
	}
}

// An inherit token states the sunset date once it is set, and no date while
// it is empty (decision 2).
func TestAnInheritTokenStatesItsEndOnlyOnceTheDateIsSet(t *testing.T) {
	handler := &Handler{tokenSigningKey: []byte("test-secret")}
	token, err := handler.presentToken(tokenRecord{ID: 1, UUID: stringAddress(scopeTokenUUID), UserID: 7,
		Admin: tokenAdminAccess{Access: "inherit"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if token.AdminAccess != "inherit" || token.InheritEndsOn != adminaccess.InheritEndsOn() {
		t.Fatalf("token %+v, want inherit ending on %q", token, adminaccess.InheritEndsOn())
	}
	encoded, _ := json.Marshal(token)
	if adminaccess.InheritEndsOn() == "" && strings.Contains(string(encoded), "inherit_ends_on") {
		t.Fatalf("an empty sunset date is stated: %s", encoded)
	}
}

// A permission store that does not answer is a 500 on both routes, and the
// audit row names the request's own action: the scope change is not a mint.
func TestAScopeFaultIsAnnotatedWithTheRequestsAction(t *testing.T) {
	broken := scopeResolver{err: errors.New("store down")}
	created := scopeRequest(t, freshSession, broken, http.MethodPost, "/token/", createScoped("admin.auth.users"))
	if created.status != http.StatusInternalServerError || created.audit != TokenMintRefusedAuditAction+":"+mintFaultAuditName {
		t.Fatalf("create: %d %q", created.status, created.audit)
	}
	changed := scopeRequest(t, freshSession, broken, http.MethodPut,
		"/token/"+scopeTokenUUID+"/admin_scopes", `{"admin_access":"scoped","admin_scopes":["admin.auth.users"]}`)
	if changed.status != http.StatusInternalServerError || changed.audit != TokenAdminScopesRefusedAuditAction+":"+mintFaultAuditName {
		t.Fatalf("change: %d %q", changed.status, changed.audit)
	}
}

// A scope refusal names the calling credential in the mint rule's words.
func TestAScopeRefusalNamesTheCredentialInTheMintVocabulary(t *testing.T) {
	for kind, want := range map[identity.CredentialKind]mintCredential{
		identity.CredentialKindSession: mintCredentialBrowserSession,
		identity.CredentialKindForm:    mintCredentialBrowserSession,
		identity.CredentialKindSealed:  mintCredentialBrowserSession,
		identity.CredentialKindNative:  mintCredentialNativeSession,
		identity.CredentialKindToken:   mintCredentialAccessToken,
		identity.CredentialKindRun:     mintCredentialAccessToken,
		identity.CredentialKindSystem:  mintCredentialAccessToken,
		identity.CredentialKindNone:    mintCredentialUnresolved,
	} {
		if got := scopeMintCredential(kind); got != want {
			t.Errorf("%q: %q, want %q", kind, got, want)
		}
	}
}

// The reserved break-glass account creates no token (ADR-0059 R2.6b), by
// account: a browser session of it is refused with its own code before the
// repository is called, whatever the case of the address.
func TestTokenCreateRefusesTheReservedAccount(t *testing.T) {
	for _, email := range []string{"break-glass@centry.user", "Break-Glass@CENTRY.user"} {
		created := false
		handler := &Handler{
			tokenSigningKey: []byte("test-secret"),
			tokens: tokenRepositoryStub{createFunc: func(context.Context, tokenCreateInput) (tokenRecord, error) {
				created = true
				return tokenRecord{}, nil
			}},
		}
		req := httptest.NewRequest(http.MethodPost, "/token/", bytes.NewBufferString(`{"name":"x"}`))
		principal := identity.User{ID: "7", UserID: "7", Email: email, BrowserSessionRef: 3}
		req = req.WithContext(identity.ContextWithAuthenticatedUser(
			req.Context(), principal, identity.AuthenticationSourceSession))
		rec := httptest.NewRecorder()
		handler.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || created ||
			!strings.Contains(rec.Body.String(), CodeMintReservedAccount) {
			t.Fatalf("%s: status = %d, created = %v, body = %s; want 403 %s",
				email, rec.Code, created, rec.Body.String(), CodeMintReservedAccount)
		}
	}
}
