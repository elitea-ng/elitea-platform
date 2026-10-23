package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

type tokenRepositoryStub struct {
	listFunc   func(context.Context, int64) ([]tokenRecord, error)
	getFunc    func(context.Context, int64, string) (tokenRecord, error)
	createFunc func(context.Context, tokenCreateInput) (tokenRecord, error)
	deleteFunc func(context.Context, int64, string) error
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

	req := authenticatedTokenRequest(http.MethodPost, "/token/", `{"name":"ci-token","expires":{"measure":"hours","value":24}}`)
	rec := httptest.NewRecorder()
	handler.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if gotOwnerID != 7 {
		t.Fatalf("persisted owner = %d, want 7; token row ID 42 must not be treated as user ID", gotOwnerID)
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

	req := authenticatedTokenRequest(http.MethodPost, "/token/", `{"name":"first"}{"name":"second"}`)
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
		handler.Routes().ServeHTTP(rec, authenticatedTokenRequest(http.MethodPost, "/token/", `{}`))
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
		handler.Routes().ServeHTTP(rec, authenticatedTokenRequest(http.MethodPost, "/token/", `{"name":null}`))
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
	handler.Routes().ServeHTTP(rec, authenticatedTokenRequest(
		http.MethodPost, "/token/", `{"name":"team-key","project_id":42}`,
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
			handler.Routes().ServeHTTP(rec, authenticatedTokenRequest(http.MethodPost, "/token/", body))
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
	handler.Routes().ServeHTTP(rec, authenticatedTokenRequest(
		http.MethodPost, "/token/", `{"name":"team-key","project_id":42}`,
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
			handler.Routes().ServeHTTP(rec, authenticatedTokenRequest(http.MethodPost, "/token/", body))
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

func stringAddress(value string) *string {
	return &value
}
