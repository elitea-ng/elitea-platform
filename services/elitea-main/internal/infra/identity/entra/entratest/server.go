// Package entratest is an in-process Microsoft Entra ID mock for tests
// (issue #1117). It serves, over TLS:
//
//   - tenant-scoped v2 discovery: /<tid>/v2.0/.well-known/openid-configuration
//     (issuer <url>/<tid>/v2.0) and the multi-tenant alias
//     /organizations/v2.0/... (issuer <url>/{tenantid}/v2.0, as Entra does);
//   - the key set: /discovery/v2.0/keys;
//   - the token endpoint /<tid>/oauth2/v2.0/token with the authorization
//     code grant, classic OBO and Agent ID OBO, plus scripted
//     interaction_required and claims-challenge answers.
//
// Test fixtures only: nothing in a production build imports it.
package entratest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// GraphAudience is the Microsoft Graph resource id.
const GraphAudience = "00000003-0000-0000-c000-000000000000"

// Server is the mock.
type Server struct {
	*httptest.Server
	key *rsa.PrivateKey

	mu          sync.Mutex
	codes       map[string]Authorization
	nextOBO     []Response
	OBORequests []OBORequest
	// AgentAssertion is the T1 value an Agent OBO request must present.
	AgentAssertion string
	// AgentID is the agent identity client id Agent OBO accepts.
	AgentID string
	// MiddleTierClientID is the classic OBO client and the sign-in client.
	MiddleTierClientID string
	// BlueprintAudience is the `aud` an Agent OBO assertion must carry.
	BlueprintAudience string
}

// Authorization is one pending authorization code.
type Authorization struct {
	TenantID string
	ObjectID string
	Subject  string
	Email    string
	Nonce    string
	// AccessTokenAudience overrides the access token `aud`; empty is the
	// middle tier. AccessTokenScope overrides `scp`; "-" omits it.
	AccessTokenAudience string
	AccessTokenScope    string
	// OmitTID and OmitOID drop the claims from the ID token.
	OmitTID bool
	OmitOID bool
	// IssuerTenant overrides the tenant in the ID token `iss`.
	IssuerTenant string
}

// Response is a scripted token endpoint answer.
type Response struct {
	Status int
	Body   map[string]any
}

// OBORequest records one OBO request, without credential values.
type OBORequest struct {
	TenantID        string
	ClientID        string
	Scope           string
	Claims          string
	ClientRequestID string
	HasSecret       bool
	HasAssertion    bool
}

// New starts the mock.
func New(t testing.TB) *Server {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		key:                key,
		codes:              map[string]Authorization{},
		MiddleTierClientID: "11111111-1111-1111-1111-111111111111",
		BlueprintAudience:  "api://22222222-2222-2222-2222-222222222222/access_agent",
		AgentID:            "33333333-3333-3333-3333-333333333333",
		AgentAssertion:     "agent-t1-assertion",
	}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Issuer is the v2 issuer of tid.
func (s *Server) Issuer(tid string) string { return s.URL + "/" + tid + "/v2.0" }

// AddCode registers an authorization code.
func (s *Server) AddCode(code string, authorization Authorization) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codes[code] = authorization
}

// ScriptOBO queues answers for the next OBO requests.
func (s *Server) ScriptOBO(responses ...Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextOBO = append(s.nextOBO, responses...)
}

// Requests returns the recorded OBO requests.
func (s *Server) Requests() []OBORequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]OBORequest(nil), s.OBORequests...)
}

// InteractionRequired is the AADSTS50076 answer.
func InteractionRequired() Response {
	return Response{Status: http.StatusBadRequest, Body: map[string]any{
		"error": "interaction_required", "error_codes": []int{50076}, "suberror": "basic_action",
		"error_description": "AADSTS50076: contains eyJhbGciOi.fake.token text that must never be echoed",
	}}
}

// ClaimsChallenge is a Conditional Access claims challenge answer.
func ClaimsChallenge(claims string) Response {
	return Response{Status: http.StatusBadRequest, Body: map[string]any{
		"error": "interaction_required", "error_codes": []int{50079}, "claims": claims,
	}}
}

// MintToken signs claims with the mock key.
func (s *Server) MintToken(claims jwt.MapClaims) string {
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "entra-mock"
	signed, err := token.SignedString(s.key)
	if err != nil {
		panic(err)
	}
	return signed
}

// AccessToken mints a delegated access token.
func (s *Server) AccessToken(tid, oid, aud, scp string, lifetime time.Duration) string {
	now := time.Now()
	claims := jwt.MapClaims{
		"iss": s.Issuer(tid), "aud": aud, "tid": tid, "oid": oid, "sub": "pairwise-" + oid,
		"ver": "2.0", "iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(lifetime).Unix(),
	}
	if scp != "-" {
		claims["scp"] = scp
	}
	return s.MintToken(claims)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(path, "/")
	switch {
	case path == "discovery/v2.0/keys":
		s.writeKeys(w)
	case len(parts) == 4 && parts[1] == "v2.0" && parts[2] == ".well-known" && parts[3] == "openid-configuration":
		s.writeDiscovery(w, parts[0])
	case len(parts) == 4 && parts[1] == "oauth2" && parts[2] == "v2.0" && parts[3] == "token":
		s.token(w, r, parts[0])
	case len(parts) == 4 && parts[1] == "oauth2" && parts[2] == "v2.0" && parts[3] == "authorize":
		http.Error(w, "the mock has no interactive authorize page", http.StatusNotImplemented)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) writeDiscovery(w http.ResponseWriter, tenant string) {
	issuer := s.Issuer(tenant)
	if tenant == "organizations" || tenant == "common" {
		issuer = s.URL + "/{tenantid}/v2.0"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                s.URL + "/" + tenant + "/oauth2/v2.0/authorize",
		"token_endpoint":                        s.URL + "/" + tenant + "/oauth2/v2.0/token",
		"jwks_uri":                              s.URL + "/discovery/v2.0/keys",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"pairwise"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

func (s *Server) writeKeys(w http.ResponseWriter) {
	public := s.key.Public().(*rsa.PublicKey)
	writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "entra-mock",
		"n": base64.RawURLEncoding.EncodeToString(public.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(public.E)).Bytes()),
	}}})
}

func (s *Server) token(w http.ResponseWriter, r *http.Request, tenant string) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.codeGrant(w, r, tenant)
	case "urn:ietf:params:oauth:grant-type:jwt-bearer":
		s.oboGrant(w, r, tenant)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported_grant_type"})
	}
}

func (s *Server) codeGrant(w http.ResponseWriter, r *http.Request, tenant string) {
	s.mu.Lock()
	authorization, ok := s.codes[r.PostForm.Get("code")]
	delete(s.codes, r.PostForm.Get("code"))
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
		return
	}
	tid := authorization.TenantID
	issuerTenant := tid
	if authorization.IssuerTenant != "" {
		issuerTenant = authorization.IssuerTenant
	}
	now := time.Now()
	idClaims := jwt.MapClaims{
		"iss": s.Issuer(issuerTenant), "aud": s.MiddleTierClientID, "sub": authorization.Subject,
		"email": authorization.Email, "name": authorization.Email, "nonce": authorization.Nonce,
		"tid": tid, "oid": authorization.ObjectID, "ver": "2.0",
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	if authorization.OmitTID {
		delete(idClaims, "tid")
	}
	if authorization.OmitOID {
		delete(idClaims, "oid")
	}
	audience := authorization.AccessTokenAudience
	if audience == "" {
		audience = s.MiddleTierClientID
	}
	scope := authorization.AccessTokenScope
	if scope == "" {
		scope = "access_as_user"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token_type":   "Bearer",
		"expires_in":   3600,
		"scope":        r.PostForm.Get("scope"),
		"access_token": s.AccessToken(tid, authorization.ObjectID, audience, scope, time.Hour),
		"id_token":     s.MintToken(idClaims),
	})
}

func (s *Server) oboGrant(w http.ResponseWriter, r *http.Request, tenant string) {
	form := r.PostForm
	record := OBORequest{
		TenantID: tenant, ClientID: form.Get("client_id"), Scope: form.Get("scope"), Claims: form.Get("claims"),
		ClientRequestID: r.Header.Get("client-request-id"),
		HasSecret:       form.Get("client_secret") != "", HasAssertion: form.Get("client_assertion") != "",
	}
	s.mu.Lock()
	s.OBORequests = append(s.OBORequests, record)
	var scripted *Response
	if len(s.nextOBO) > 0 {
		next := s.nextOBO[0]
		s.nextOBO = s.nextOBO[1:]
		scripted = &next
	}
	s.mu.Unlock()
	if form.Get("requested_token_use") != "on_behalf_of" || form.Get("assertion") == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return
	}
	if scripted != nil {
		writeJSON(w, scripted.Status, scripted.Body)
		return
	}
	var claims jwt.MapClaims
	parsed, err := jwt.ParseWithClaims(form.Get("assertion"), &claims, func(*jwt.Token) (any, error) {
		return s.key.Public(), nil
	})
	if err != nil || !parsed.Valid {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "suberror": "bad_token"})
		return
	}
	expectedAudience := s.MiddleTierClientID
	if record.ClientID == s.AgentID {
		// Agent ID OBO: the agent identity authenticates with T1 and the
		// user assertion was issued to the blueprint.
		if form.Get("client_assertion") != s.AgentAssertion {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_client", "error_codes": []int{7000215}})
			return
		}
		expectedAudience = s.BlueprintAudience
	} else if record.ClientID != s.MiddleTierClientID || (!record.HasSecret && !record.HasAssertion) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_client"})
		return
	}
	if claims["aud"] != expectedAudience {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_codes": []int{50013}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token_type": "Bearer", "expires_in": "3599", "scope": form.Get("scope"),
		"access_token": s.AccessToken(tenant, fmt.Sprint(claims["oid"]), "https://graph.microsoft.com", "Sites.Selected", time.Hour),
	})
}

func writeJSON(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
