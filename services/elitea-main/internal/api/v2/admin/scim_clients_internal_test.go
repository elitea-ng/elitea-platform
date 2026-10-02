package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/scimclient"
)

// stubSCIMClientStore is an in-memory store with the same secret rule as the
// real one: a secret is minted on create and on rotate, and is never kept.
type stubSCIMClientStore struct {
	clients   []scimclient.Client
	createdBy *int
	err       error
	minted    int
}

func (s *stubSCIMClientStore) List(context.Context) ([]scimclient.Client, error) {
	return s.clients, s.err
}

func (s *stubSCIMClientStore) mint(prefix string) string {
	s.minted++
	return prefix + strings.Repeat("x", 40) + string(rune('A'+s.minted))
}

func (s *stubSCIMClientStore) Create(_ context.Context, name, method string, createdBy *int) (scimclient.Issued, error) {
	if s.err != nil {
		return scimclient.Issued{}, s.err
	}
	s.createdBy = createdBy
	client := scimclient.Client{
		ID: int64(len(s.clients) + 1), Name: name, AuthMethod: method, CreatedBy: createdBy, CreatedAt: time.Now(),
	}
	secret := s.mint(scimclient.PrefixBearerSecret)
	if method == scimclient.MethodClientCredentials {
		client.ClientID = "scimc_public"
		secret = s.mint(scimclient.PrefixClientSecret)
	}
	client.SecretHint = secret[len(secret)-4:]
	s.clients = append(s.clients, client)
	return scimclient.Issued{Client: client, Secret: secret}, nil
}

func (s *stubSCIMClientStore) Rotate(_ context.Context, id int64) (scimclient.Issued, error) {
	if s.err != nil {
		return scimclient.Issued{}, s.err
	}
	for i := range s.clients {
		if s.clients[i].ID == id {
			secret := s.mint(scimclient.PrefixBearerSecret)
			s.clients[i].SecretHint = secret[len(secret)-4:]
			return scimclient.Issued{Client: s.clients[i], Secret: secret}, nil
		}
	}
	return scimclient.Issued{}, scimclient.ErrNotFound
}

func (s *stubSCIMClientStore) Revoke(_ context.Context, id int64) (scimclient.Client, error) {
	for i := range s.clients {
		if s.clients[i].ID == id {
			now := time.Now()
			s.clients[i].RevokedAt = &now
			return s.clients[i], nil
		}
	}
	return scimclient.Client{}, scimclient.ErrNotFound
}

func (s *stubSCIMClientStore) Delete(_ context.Context, id int64) error {
	for i := range s.clients {
		if s.clients[i].ID == id {
			s.clients = append(s.clients[:i], s.clients[i+1:]...)
			return nil
		}
	}
	return scimclient.ErrNotFound
}

func (s *stubSCIMClientStore) AccessTokenTTL() time.Duration { return time.Hour }

func scimClientRouter(handler *Handler) http.Handler {
	router := chi.NewRouter()
	router.Get("/scim_clients/administration", handler.SCIMClientList)
	router.Post("/scim_clients/administration", handler.SCIMClientCreate)
	router.Post("/scim_clients/administration/{id}/rotate", handler.SCIMClientRotate)
	router.Post("/scim_clients/administration/{id}/revoke", handler.SCIMClientRevoke)
	router.Delete("/scim_clients/administration/{id}", handler.SCIMClientDelete)
	return router
}

func callSCIMClients(router http.Handler, method, target, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{
		ID: "42", UserID: "42", Email: "admin@test.local", AuthType: "session",
	}))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestSCIMClientSecretIsReturnedOnceAndNeverListed(t *testing.T) {
	store := &stubSCIMClientStore{}
	router := scimClientRouter(NewHandler(nil, WithSCIMClients(store)))

	response := callSCIMClients(router, http.MethodPost, "/scim_clients/administration",
		`{"name":"Entra ID","auth_method":"client_credentials"}`)
	require.Equal(t, http.StatusCreated, response.Code, response.Body.String())
	require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	var created struct {
		Client            map[string]any `json:"client"`
		Secret            string         `json:"secret"`
		ClientID          string         `json:"client_id"`
		SCIMBasePath      string         `json:"scim_base_path"`
		TokenEndpointPath string         `json:"token_endpoint_path"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &created))
	require.True(t, strings.HasPrefix(created.Secret, scimclient.PrefixClientSecret))
	require.Equal(t, "scimc_public", created.ClientID)
	require.Equal(t, "/api/v2/scim/v2", created.SCIMBasePath)
	require.Equal(t, "/api/v2/scim/oauth/token", created.TokenEndpointPath)
	require.Equal(t, "active", created.Client["status"])
	require.NotContains(t, created.Client, "secret")
	require.Equal(t, 42, *store.createdBy, "the creator is the signed-in administrator")

	list := callSCIMClients(router, http.MethodGet, "/scim_clients/administration", "")
	require.Equal(t, http.StatusOK, list.Code)
	require.NotContains(t, list.Body.String(), created.Secret, "a read must never return a secret")
	var listed struct {
		Clients []map[string]any `json:"clients"`
		Total   int              `json:"total"`
		TTL     int              `json:"access_token_ttl_seconds"`
	}
	require.NoError(t, json.Unmarshal(list.Body.Bytes(), &listed))
	require.Equal(t, 1, listed.Total)
	require.Equal(t, 3600, listed.TTL)
	require.Equal(t, created.Secret[len(created.Secret)-4:], listed.Clients[0]["secret_hint"])
	require.Equal(t, "client_credentials", listed.Clients[0]["auth_method"])
}

func TestSCIMClientRotateReturnsANewSecretAndRevokeAndDeleteWork(t *testing.T) {
	store := &stubSCIMClientStore{}
	router := scimClientRouter(NewHandler(nil, WithSCIMClients(store)))
	created := callSCIMClients(router, http.MethodPost, "/scim_clients/administration", `{"name":"okta","auth_method":"bearer"}`)
	require.Equal(t, http.StatusCreated, created.Code)
	var first struct {
		Secret string `json:"secret"`
	}
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &first))

	rotated := callSCIMClients(router, http.MethodPost, "/scim_clients/administration/1/rotate", "")
	require.Equal(t, http.StatusOK, rotated.Code)
	require.Equal(t, "no-store", rotated.Header().Get("Cache-Control"))
	var second struct {
		Secret string `json:"secret"`
	}
	require.NoError(t, json.Unmarshal(rotated.Body.Bytes(), &second))
	require.NotEmpty(t, second.Secret)
	require.NotEqual(t, first.Secret, second.Secret)

	revoked := callSCIMClients(router, http.MethodPost, "/scim_clients/administration/1/revoke", "")
	require.Equal(t, http.StatusOK, revoked.Code)
	require.Contains(t, revoked.Body.String(), `"status":"revoked"`)
	require.NotContains(t, revoked.Body.String(), `"secret":`)

	require.Equal(t, http.StatusNoContent, callSCIMClients(router, http.MethodDelete, "/scim_clients/administration/1", "").Code)
	require.Equal(t, http.StatusNotFound, callSCIMClients(router, http.MethodDelete, "/scim_clients/administration/1", "").Code)
	require.Equal(t, http.StatusNotFound, callSCIMClients(router, http.MethodPost, "/scim_clients/administration/x/rotate", "").Code)
}

func TestSCIMClientRefusalsAreTyped(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		err    error
		body   string
		status int
	}{
		{"bad name", scimclient.ErrInvalidName, `{"name":"","auth_method":"bearer"}`, http.StatusBadRequest},
		{"bad method", scimclient.ErrInvalidMethod, `{"name":"x","auth_method":"password"}`, http.StatusBadRequest},
		{"duplicate", scimclient.ErrDuplicateName, `{"name":"x","auth_method":"bearer"}`, http.StatusConflict},
		{"store down", errors.New("dial tcp: refused"), `{"name":"x","auth_method":"bearer"}`, http.StatusServiceUnavailable},
		{"unknown field", nil, `{"name":"x","auth_method":"bearer","secret":"mine"}`, http.StatusBadRequest},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			router := scimClientRouter(NewHandler(nil, WithSCIMClients(&stubSCIMClientStore{err: testCase.err})))
			response := callSCIMClients(router, http.MethodPost, "/scim_clients/administration", testCase.body)
			require.Equal(t, testCase.status, response.Code)
			require.NotContains(t, response.Body.String(), "dial tcp")
		})
	}

	// Without a store every route answers 503, never an empty list.
	router := scimClientRouter(NewHandler(nil))
	require.Equal(t, http.StatusServiceUnavailable,
		callSCIMClients(router, http.MethodGet, "/scim_clients/administration", "").Code)
}
