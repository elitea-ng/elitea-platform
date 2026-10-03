package scim

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/scimclient"
)

// fakeCredentials accepts the tokens in its map and rejects everything else.
type fakeCredentials struct {
	tokens  map[string]scimclient.Principal
	fail    error
	touched []int64
	issued  map[string]string // client id -> secret
	// issueCalls counts store lookups, so a test can prove a blocked
	// caller reached no database read.
	issueCalls int
}

func (f *fakeCredentials) Authenticate(_ context.Context, token string) (scimclient.Principal, error) {
	if f.fail != nil {
		return scimclient.Principal{}, f.fail
	}
	principal, ok := f.tokens[token]
	if !ok {
		return scimclient.Principal{}, scimclient.ErrRejected
	}
	return principal, nil
}

func (f *fakeCredentials) TouchLastUsed(_ context.Context, id int64) {
	f.touched = append(f.touched, id)
}

func (f *fakeCredentials) IssueAccessToken(_ context.Context, clientID, secret string) (scimclient.AccessToken, scimclient.Principal, error) {
	f.issueCalls++
	if f.fail != nil {
		return scimclient.AccessToken{}, scimclient.Principal{}, f.fail
	}
	if want, ok := f.issued[clientID]; !ok || want != secret {
		return scimclient.AccessToken{}, scimclient.Principal{}, scimclient.ErrRejected
	}
	return scimclient.AccessToken{Token: "scimat_issued", ExpiresIn: time.Hour},
		scimclient.Principal{ID: 9, Name: "entra-oauth", Method: scimclient.MethodClientCredentials}, nil
}

func newAuthFixture() *fakeCredentials {
	return &fakeCredentials{
		tokens: map[string]scimclient.Principal{
			"scim_bearer":   {ID: 1, Name: "Entra ID", Method: scimclient.MethodBearer},
			"scimat_access": {ID: 9, Name: "entra-oauth", Method: scimclient.MethodClientCredentials},
		},
		issued: map[string]string{"scimc_id": "scimcs_secret"},
	}
}

func serveAuthenticated(credentials Credentials, request *http.Request) (*httptest.ResponseRecorder, *scimclient.Principal, audit.Annotation) {
	var seen *scimclient.Principal
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := PrincipalFromContext(r.Context())
		if ok {
			seen = &principal
		}
		w.WriteHeader(http.StatusOK)
	})
	ctx, slot := audit.ContextWithAnnotationSlot(request.Context())
	recorder := httptest.NewRecorder()
	Authenticate(credentials)(AnnotateAuditActor(next)).ServeHTTP(recorder, request.WithContext(ctx))
	annotation, _ := slot.Read()
	return recorder, seen, annotation
}

func TestSCIMAdmitsABearerClientAndAnAccessToken(t *testing.T) {
	credentials := newAuthFixture()
	for token, name := range map[string]string{"scim_bearer": "Entra ID", "scimat_access": "entra-oauth"} {
		request := httptest.NewRequest(http.MethodGet, BasePath+"/Users", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		recorder, principal, annotation := serveAuthenticated(credentials, request)
		require.Equal(t, http.StatusOK, recorder.Code, token)
		require.NotNil(t, principal)
		require.Equal(t, name, principal.Name)
		require.Equal(t, "scim:"+name, annotation.Actor, "the audit row must name the SCIM client")
	}
	require.Equal(t, []int64{1, 9}, sortedIDs(credentials.touched))
}

func sortedIDs(ids []int64) []int64 {
	out := append([]int64(nil), ids...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestSCIMRefusesAPersonalAccessTokenASessionAndARevokedClient(t *testing.T) {
	credentials := newAuthFixture()
	for name, prepare := range map[string]func(*http.Request){
		"personal access token": func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2ln")
		},
		"browser session": func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: "elitea_session", Value: "opaque"})
		},
		"revoked or unknown client": func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer scim_revoked")
		},
		"basic credentials": func(r *http.Request) { r.SetBasicAuth("scimc_id", "scimcs_secret") },
		"empty bearer":      func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") },
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, BasePath+"/Users", nil)
			prepare(request)
			recorder, principal, _ := serveAuthenticated(credentials, request)
			require.Equal(t, http.StatusUnauthorized, recorder.Code)
			require.Nil(t, principal)
			require.Equal(t, `Bearer realm="SCIM"`, recorder.Header().Get("WWW-Authenticate"))
			require.Equal(t, contentType, recorder.Header().Get("Content-Type"))
			var body map[string]any
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
			require.Equal(t, RefusalDetail, body["detail"])
			require.Equal(t, "401", body["status"])
			require.Equal(t, []any{schemaError}, body["schemas"])
		})
	}
	require.Empty(t, credentials.touched)
}

func TestSCIMAnswers503WhenTheCredentialStoreFailsOrIsAbsent(t *testing.T) {
	failing := newAuthFixture()
	failing.fail = errors.New("connection refused")
	request := httptest.NewRequest(http.MethodGet, BasePath+"/Users", nil)
	request.Header.Set("Authorization", "Bearer scim_bearer")
	recorder, _, _ := serveAuthenticated(failing, request)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.NotContains(t, recorder.Body.String(), "connection refused")

	recorder, _, _ = serveAuthenticated(nil, request)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
}

func TestServiceProviderConfigAdvertisesTheOAuthBearerScheme(t *testing.T) {
	recorder := httptest.NewRecorder()
	(&Handler{}).ServiceProviderConfig(recorder, httptest.NewRequest(http.MethodGet, BasePath+"/ServiceProviderConfig", nil))
	var body struct {
		AuthenticationSchemes []map[string]any `json:"authenticationSchemes"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Len(t, body.AuthenticationSchemes, 1)
	scheme := body.AuthenticationSchemes[0]
	require.Equal(t, "oauthbearertoken", scheme["type"])
	require.Contains(t, scheme["description"], TokenPath)
	require.NotContains(t, strings.ToLower(scheme["description"].(string)), "holds admin.auth.users")
}

/* ── token endpoint ─────────────────────────────────────────────────────── */

func postToken(handler http.Handler, form url.Values, prepare func(*http.Request)) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, TokenPath, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if prepare != nil {
		prepare(request)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func oauthError(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
	return body["error"]
}

func TestTokenEndpointIssuesWithClientSecretPostAndBasic(t *testing.T) {
	handler := NewTokenHandler(newAuthFixture(), nil)
	for name, send := range map[string]func() *httptest.ResponseRecorder{
		"client_secret_post": func() *httptest.ResponseRecorder {
			return postToken(handler, url.Values{
				"grant_type": {"client_credentials"}, "client_id": {"scimc_id"},
				"client_secret": {"scimcs_secret"}, "scope": {"anything"},
			}, nil)
		},
		"client_secret_basic": func() *httptest.ResponseRecorder {
			return postToken(handler, url.Values{"grant_type": {"client_credentials"}}, func(r *http.Request) {
				r.SetBasicAuth(url.QueryEscape("scimc_id"), url.QueryEscape("scimcs_secret"))
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := send()
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
			require.Equal(t, "no-cache", recorder.Header().Get("Pragma"))
			require.Equal(t, "application/json;charset=UTF-8", recorder.Header().Get("Content-Type"))
			var body map[string]any
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
			require.Equal(t, "scimat_issued", body["access_token"])
			require.Equal(t, "Bearer", body["token_type"])
			require.EqualValues(t, 3600, body["expires_in"])
		})
	}
}

func TestTokenEndpointRefusals(t *testing.T) {
	handler := NewTokenHandler(newAuthFixture(), nil)

	recorder := postToken(handler, url.Values{
		"grant_type": {"client_credentials"}, "client_id": {"scimc_id"}, "client_secret": {"wrong"},
	}, nil)
	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	require.Equal(t, "invalid_client", oauthError(t, recorder))
	require.Empty(t, recorder.Header().Get("WWW-Authenticate"), "no Basic was used")

	recorder = postToken(handler, url.Values{"grant_type": {"client_credentials"}}, func(r *http.Request) {
		r.SetBasicAuth("scimc_id", "wrong")
	})
	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	require.Equal(t, "invalid_client", oauthError(t, recorder))
	require.Contains(t, recorder.Header().Get("WWW-Authenticate"), "Basic")

	recorder = postToken(handler, url.Values{
		"grant_type": {"password"}, "client_id": {"scimc_id"}, "client_secret": {"scimcs_secret"},
	}, nil)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Equal(t, "unsupported_grant_type", oauthError(t, recorder))

	recorder = postToken(handler, url.Values{"client_id": {"scimc_id"}, "client_secret": {"scimcs_secret"}}, nil)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Equal(t, "invalid_request", oauthError(t, recorder))

	recorder = postToken(handler, url.Values{"grant_type": {"client_credentials"}}, nil)
	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	require.Equal(t, "invalid_client", oauthError(t, recorder))

	// Two client authentication methods in one request (RFC 6749 §2.3).
	recorder = postToken(handler, url.Values{
		"grant_type": {"client_credentials"}, "client_secret": {"scimcs_secret"},
	}, func(r *http.Request) { r.SetBasicAuth("scimc_id", "scimcs_secret") })
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Equal(t, "invalid_request", oauthError(t, recorder))

	// A repeated parameter (RFC 6749 §3.2).
	recorder = postToken(handler, url.Values{
		"grant_type": {"client_credentials", "client_credentials"}, "client_id": {"scimc_id"}, "client_secret": {"scimcs_secret"},
	}, nil)
	require.Equal(t, http.StatusBadRequest, recorder.Code)

	// JSON is not the token request media type.
	request := httptest.NewRequest(http.MethodPost, TokenPath, strings.NewReader(`{"grant_type":"client_credentials"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Equal(t, "invalid_request", oauthError(t, recorder))
}

type fixedAddress string

func (f fixedAddress) ResolveClientKey(*http.Request) (string, error) { return string(f), nil }

func TestTokenEndpointLimitsFailedClientAuthentication(t *testing.T) {
	credentials := newAuthFixture()
	handler := NewTokenHandler(credentials, fixedAddress("203.0.113.9"))
	wrong := url.Values{"grant_type": {"client_credentials"}, "client_id": {"scimc_id"}, "client_secret": {"wrong"}}
	for i := 0; i < tokenFailureLimit-1; i++ {
		require.Equal(t, http.StatusUnauthorized, postToken(handler, wrong, nil).Code)
	}
	recorder := postToken(handler, wrong, nil)
	require.Equal(t, http.StatusTooManyRequests, recorder.Code)
	require.NotEmpty(t, recorder.Header().Get("Retry-After"))
	require.Equal(t, tokenFailureLimit, credentials.issueCalls)

	// Once blocked, the address is refused BEFORE the store is read: no
	// database lookup per request, whatever the secret.
	good := url.Values{"grant_type": {"client_credentials"}, "client_id": {"scimc_id"}, "client_secret": {"scimcs_secret"}}
	require.Equal(t, http.StatusTooManyRequests, postToken(handler, wrong, nil).Code)
	require.Equal(t, http.StatusTooManyRequests, postToken(handler, good, nil).Code)
	require.Equal(t, tokenFailureLimit, credentials.issueCalls, "a blocked caller must not reach the store")

	// The identity provider calls from another address. The attacker's
	// failures do not block it, and its correct secret gets a token.
	handler.resolver = fixedAddress("198.51.100.7")
	require.Equal(t, http.StatusOK, postToken(handler, good, nil).Code)

	// Another client id from the blocked address has its own window.
	handler.resolver = fixedAddress("203.0.113.9")
	other := url.Values{"grant_type": {"client_credentials"}, "client_id": {"scimc_other"}, "client_secret": {"wrong"}}
	require.Equal(t, http.StatusUnauthorized, postToken(handler, other, nil).Code)
}

func TestTokenEndpointCountsFailuresPerCallerAddress(t *testing.T) {
	credentials := newAuthFixture()
	attacker := NewTokenHandler(credentials, fixedAddress("203.0.113.9"))
	wrong := url.Values{"grant_type": {"client_credentials"}, "client_id": {"scimc_id"}, "client_secret": {"wrong"}}
	for i := 0; i < tokenFailureLimit; i++ {
		postToken(attacker, wrong, nil)
	}
	require.Equal(t, http.StatusTooManyRequests, postToken(attacker, wrong, nil).Code)

	// The same handler (one limiter), another caller address: its own window.
	attacker.resolver = fixedAddress("198.51.100.7")
	require.Equal(t, http.StatusUnauthorized, postToken(attacker, wrong, nil).Code)
}

func TestTokenEndpointHidesStoreFailures(t *testing.T) {
	credentials := newAuthFixture()
	credentials.fail = errors.New("pq: connection refused")
	recorder := postToken(NewTokenHandler(credentials, nil), url.Values{
		"grant_type": {"client_credentials"}, "client_id": {"scimc_id"}, "client_secret": {"scimcs_secret"},
	}, nil)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.NotContains(t, recorder.Body.String(), "connection refused")

	recorder = postToken(NewTokenHandler(nil, nil), url.Values{"grant_type": {"client_credentials"}}, nil)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
}
