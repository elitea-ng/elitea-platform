package entratest

// The Entra Agent ID half of the mock (issue #1152):
//
//   - the blueprint's client credentials grant, authenticated with a
//     federated assertion: a Microsoft Graph token, or T1 when the request
//     names an `fmi_path` (scope api://AzureADTokenExchange/.default);
//   - the agent identity's client credentials grant, authenticated with T1:
//     an app-only resource token issued to the agent;
//   - Agent ID OBO with a T1 client assertion (server.go, oboGrant);
//   - `accountEnabled` enforcement on every agent token (AADSTS7000112);
//   - Microsoft Graph v1.0 agent identity create, get, list, patch and
//     delete, and /directory/deletedItems for soft-deleted objects.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	// TokenExchangeAudience is the `aud` of T1.
	TokenExchangeAudience = "api://AzureADTokenExchange"
	// GraphResource is the resource of a Microsoft Graph token.
	GraphResource = "https://graph.microsoft.com"
	// AADSTSApplicationDisabled is the code of a token request for a
	// disabled agent identity.
	AADSTSApplicationDisabled = 7000112
	graphAgentCollection      = "v1.0/servicePrincipals/microsoft.graph.agentIdentity"
	maxGraphPage              = 999
)

// AgentIdentity is one agent identity in the mock directory.
type AgentIdentity struct {
	ID             string
	AppID          string
	DisplayName    string
	BlueprintID    string
	AccountEnabled bool
	Tags           []string
	Sponsors       []string
}

// TokenRequest records one client credentials or OBO request, without
// credential values.
type TokenRequest struct {
	Grant           string
	TenantID        string
	ClientID        string
	Scope           string
	FMIPath         string
	ClientRequestID string
}

// GraphRequest records one Graph request.
type GraphRequest struct {
	Method          string
	Path            string
	Query           string
	ClientRequestID string
}

func (s *Server) initAgentID() {
	s.BlueprintAppID = "22222222-2222-2222-2222-222222222222"
	s.blueprintAssertion = "blueprint-federated-token"
	s.agents = map[string]*AgentIdentity{}
	s.deleted = map[string]*AgentIdentity{}
}

// SetBlueprintAssertion sets the federated assertion the blueprint must
// present (a rotated projected token).
func (s *Server) SetBlueprintAssertion(value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blueprintAssertion = value
}

// BlueprintAssertion is the federated assertion the blueprint must present.
func (s *Server) BlueprintAssertion() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blueprintAssertion
}

// AddAgent puts an agent identity into the directory, as an administrator
// creating it outside Elitea would. Empty ID and AppID are generated.
func (s *Server) AddAgent(a AgentIdentity) AgentIdentity {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.ID == "" {
		a.ID = uuid.NewString()
	}
	if a.AppID == "" {
		a.AppID = uuid.NewString()
	}
	if a.BlueprintID == "" {
		a.BlueprintID = s.BlueprintAppID
	}
	stored := a
	stored.Tags, stored.Sponsors = slices.Clone(a.Tags), slices.Clone(a.Sponsors)
	s.agents[a.ID] = &stored
	return a
}

// Agent returns the live agent identity id.
func (s *Server) Agent(id string) (AgentIdentity, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.agents[id]
	if !ok {
		return AgentIdentity{}, false
	}
	return *a, true
}

// Agents returns every live agent identity.
func (s *Server) Agents() []AgentIdentity {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AgentIdentity, 0, len(s.agents))
	for _, a := range s.agents {
		out = append(out, *a)
	}
	return out
}

// SetAccountEnabled changes accountEnabled out of band.
func (s *Server) SetAccountEnabled(id string, enabled bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.agents[id]
	if ok {
		a.AccountEnabled = enabled
	}
	return ok
}

// DeleteAgent soft-deletes id out of band.
func (s *Server) DeleteAgent(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteLocked(id)
}

func (s *Server) deleteLocked(id string) bool {
	a, ok := s.agents[id]
	if ok {
		delete(s.agents, id)
		s.deleted[id] = a
	}
	return ok
}

// RecreateAgent deletes id and creates another object with the same
// display name, blueprint, tags and sponsors: a new object id and app id.
func (s *Server) RecreateAgent(id string) (AgentIdentity, bool) {
	s.mu.Lock()
	old, ok := s.agents[id]
	if ok {
		s.deleteLocked(id)
	}
	s.mu.Unlock()
	if !ok {
		return AgentIdentity{}, false
	}
	return s.AddAgent(AgentIdentity{DisplayName: old.DisplayName, BlueprintID: old.BlueprintID,
		AccountEnabled: old.AccountEnabled, Tags: old.Tags, Sponsors: old.Sponsors}), true
}

// SetIndexed hides id from (false) or shows it to (true) the eventually
// consistent advanced-query index: a tags/any or $count query does not see
// a hidden object, while a GET by id does. It simulates Graph's index lag.
func (s *Server) SetIndexed(id string, indexed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unindexed == nil {
		s.unindexed = map[string]bool{}
	}
	if indexed {
		delete(s.unindexed, id)
	} else {
		s.unindexed[id] = true
	}
}

// ScriptToken queues answers for the next client credentials requests.
func (s *Server) ScriptToken(responses ...Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextToken = append(s.nextToken, responses...)
}

// ScriptGraph queues answers for the next Graph requests.
func (s *Server) ScriptGraph(responses ...Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextGraph = append(s.nextGraph, responses...)
}

// TokenRequests returns every recorded client credentials and OBO request.
func (s *Server) TokenRequests() []TokenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.tokenRequests)
}

// GraphRequests returns every recorded Graph request.
func (s *Server) GraphRequests() []GraphRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.graphRequests)
}

// Calls is the number of token and Graph requests the mock received.
func (s *Server) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tokenRequests) + len(s.graphRequests) + len(s.OBORequests)
}

// IssuedAgentAssertions returns every T1 the mock minted, for a canary scan.
func (s *Server) IssuedAgentAssertions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.issuedT1)
}

func setRequestIDs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("x-ms-request-id", uuid.NewString())
	w.Header().Set("request-id", uuid.NewString())
	if id := r.Header.Get("client-request-id"); id != "" {
		w.Header().Set("client-request-id", id)
	}
}

// clientCredentialsGrant serves the blueprint (Graph token or T1) and the
// agent identity (resource token).
func (s *Server) clientCredentialsGrant(w http.ResponseWriter, r *http.Request, tenant string) {
	form := r.PostForm
	record := TokenRequest{Grant: "client_credentials", TenantID: tenant, ClientID: form.Get("client_id"),
		Scope: form.Get("scope"), FMIPath: form.Get("fmi_path"), ClientRequestID: r.Header.Get("client-request-id")}
	s.mu.Lock()
	s.tokenRequests = append(s.tokenRequests, record)
	var scripted *Response
	if len(s.nextToken) > 0 {
		next := s.nextToken[0]
		s.nextToken = s.nextToken[1:]
		scripted = &next
	}
	blueprintAssertion := s.blueprintAssertion
	s.mu.Unlock()
	if scripted != nil {
		writeJSON(w, scripted.Status, scripted.Body)
		return
	}
	if form.Get("client_secret") != "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_client", "error_codes": []int{7000222}})
		return
	}
	if form.Get("client_assertion_type") != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return
	}
	if record.ClientID == s.BlueprintAppID {
		if form.Get("client_assertion") != blueprintAssertion {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_client", "error_codes": []int{700213}})
			return
		}
		s.blueprintToken(w, tenant, record)
		return
	}
	agent, ok := s.agentByAppID(record.ClientID)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unauthorized_client", "error_codes": []int{700016}})
		return
	}
	if !s.validT1(form.Get("client_assertion"), tenant, agent.AppID) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_client", "error_codes": []int{7000215}})
		return
	}
	if !agent.AccountEnabled {
		writeJSON(w, http.StatusBadRequest, disabledBody())
		return
	}
	resource, ok := strings.CutSuffix(record.Scope, "/.default")
	if !ok || strings.Contains(record.Scope, " ") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_scope", "error_codes": []int{1002012}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token_type": "Bearer", "expires_in": 3599,
		"access_token": s.agentToken(tenant, agent, resource, "app", ""),
	})
}

func disabledBody() map[string]any {
	return map[string]any{"error": "unauthorized_client", "error_codes": []int{AADSTSApplicationDisabled},
		"error_description": "AADSTS7000112: Application is disabled."}
}

func (s *Server) blueprintToken(w http.ResponseWriter, tenant string, record TokenRequest) {
	now := time.Now()
	switch {
	case record.Scope == TokenExchangeAudience+"/.default" && record.FMIPath != "":
		agent, ok := s.agentByAppID(record.FMIPath)
		if !ok || agent.BlueprintID != s.BlueprintAppID {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request", "error_codes": []int{700016}})
			return
		}
		if !agent.AccountEnabled {
			writeJSON(w, http.StatusBadRequest, disabledBody())
			return
		}
		t1 := s.MintToken(jwt.MapClaims{
			"iss": s.Issuer(tenant), "aud": TokenExchangeAudience, "tid": tenant, "azp": s.BlueprintAppID,
			"appid": s.BlueprintAppID, "sub": record.FMIPath, "fmi_path": record.FMIPath, "idtyp": "app",
			"uti": uuid.NewString(), "iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		})
		s.mu.Lock()
		s.issuedT1 = append(s.issuedT1, t1)
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"token_type": "Bearer", "expires_in": 3599, "access_token": t1})
	case record.Scope == GraphResource+"/.default" && record.FMIPath == "":
		writeJSON(w, http.StatusOK, map[string]any{"token_type": "Bearer", "expires_in": 3599,
			"access_token": s.MintToken(jwt.MapClaims{
				"iss": s.Issuer(tenant), "aud": GraphResource, "tid": tenant, "azp": s.BlueprintAppID,
				"appid": s.BlueprintAppID, "idtyp": "app", "roles": []string{"AgentIdentity.CreateAsManager"},
				"uti": uuid.NewString(), "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
			})})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_scope", "error_codes": []int{70011}})
	}
}

// agentToken mints a resource token issued to an agent identity. idtyp is
// "app" for app-only and "user" for OBO (oid is then the user).
func (s *Server) agentToken(tenant string, agent AgentIdentity, resource, idtyp, userOID string) string {
	now := time.Now()
	claims := jwt.MapClaims{
		"iss": s.Issuer(tenant), "aud": resource, "tid": tenant, "azp": agent.AppID, "appid": agent.AppID,
		"xms_par_app_azp": agent.BlueprintID, "idtyp": idtyp, "oid": agent.ID, "uti": uuid.NewString(),
		"ver": "2.0", "iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	if idtyp == "user" {
		claims["oid"], claims["scp"] = userOID, "Sites.Selected"
	}
	return s.MintToken(claims)
}

// validT1 checks a client assertion is a T1 the mock minted for agentAppID.
func (s *Server) validT1(assertion, tenant, agentAppID string) bool {
	var claims jwt.MapClaims
	parsed, err := jwt.ParseWithClaims(assertion, &claims, func(*jwt.Token) (any, error) { return s.key.Public(), nil })
	if err != nil || !parsed.Valid {
		return false
	}
	return claims["aud"] == TokenExchangeAudience && claims["azp"] == s.BlueprintAppID &&
		claims["fmi_path"] == agentAppID && claims["tid"] == tenant
}

func (s *Server) agentByAppID(appID string) (AgentIdentity, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.agents {
		if a.AppID == appID {
			return *a, true
		}
	}
	return AgentIdentity{}, false
}

// graph serves the Graph v1.0 routes. path has no leading slash.
func (s *Server) graph(w http.ResponseWriter, r *http.Request, path string) {
	setRequestIDs(w, r)
	s.mu.Lock()
	s.graphRequests = append(s.graphRequests, GraphRequest{Method: r.Method, Path: path, Query: r.URL.RawQuery,
		ClientRequestID: r.Header.Get("client-request-id")})
	var scripted *Response
	if len(s.nextGraph) > 0 {
		next := s.nextGraph[0]
		s.nextGraph = s.nextGraph[1:]
		scripted = &next
	}
	s.mu.Unlock()
	if scripted != nil {
		writeJSON(w, scripted.Status, scripted.Body)
		return
	}
	if !s.validGraphToken(r.Header.Get("Authorization")) {
		writeJSON(w, http.StatusUnauthorized, graphError("InvalidAuthenticationToken"))
		return
	}
	switch {
	case path == graphAgentCollection && r.Method == http.MethodPost:
		s.graphCreate(w, r)
	case path == graphAgentCollection && r.Method == http.MethodGet:
		s.graphList(w, r)
	case strings.HasPrefix(path, "v1.0/servicePrincipals/") && strings.HasSuffix(path, "/microsoft.graph.agentIdentity"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "v1.0/servicePrincipals/"), "/microsoft.graph.agentIdentity")
		switch r.Method {
		case http.MethodGet:
			s.graphGet(w, id, false)
		case http.MethodPatch:
			s.graphPatch(w, r, id)
		default:
			writeJSON(w, http.StatusMethodNotAllowed, graphError("Request_BadRequest"))
		}
	case strings.HasPrefix(path, "v1.0/servicePrincipals/") && r.Method == http.MethodDelete:
		s.mu.Lock()
		ok := s.deleteLocked(strings.TrimPrefix(path, "v1.0/servicePrincipals/"))
		s.mu.Unlock()
		if !ok {
			writeJSON(w, http.StatusNotFound, graphError("Request_ResourceNotFound"))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(path, "v1.0/directory/deletedItems/") && r.Method == http.MethodGet:
		s.graphGet(w, strings.TrimPrefix(path, "v1.0/directory/deletedItems/"), true)
	default:
		writeJSON(w, http.StatusNotFound, graphError("Request_ResourceNotFound"))
	}
}

func graphError(code string) map[string]any {
	return map[string]any{"error": map[string]any{"code": code, "message": code}}
}

func (s *Server) validGraphToken(header string) bool {
	raw, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return false
	}
	var claims jwt.MapClaims
	parsed, err := jwt.ParseWithClaims(raw, &claims, func(*jwt.Token) (any, error) { return s.key.Public(), nil })
	return err == nil && parsed.Valid && claims["aud"] == GraphResource && claims["appid"] == s.BlueprintAppID
}

func (a AgentIdentity) graphJSON() map[string]any {
	tags := a.Tags
	if tags == nil {
		tags = []string{}
	}
	return map[string]any{
		"@odata.type": "#microsoft.graph.agentIdentity", "id": a.ID, "appId": a.AppID,
		"displayName": a.DisplayName, "agentIdentityBlueprintId": a.BlueprintID,
		"accountEnabled": a.AccountEnabled, "servicePrincipalType": "ServiceIdentity", "tags": tags,
	}
}

func (s *Server) graphCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DisplayName    string   `json:"displayName"`
		BlueprintID    string   `json:"agentIdentityBlueprintId"`
		AccountEnabled *bool    `json:"accountEnabled"`
		Tags           []string `json:"tags"`
		Sponsors       []string `json:"sponsors@odata.bind"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.DisplayName == "" {
		writeJSON(w, http.StatusBadRequest, graphError("Request_BadRequest"))
		return
	}
	if body.BlueprintID != s.BlueprintAppID {
		writeJSON(w, http.StatusForbidden, graphError("Authorization_RequestDenied"))
		return
	}
	if len(body.Sponsors) == 0 {
		writeJSON(w, http.StatusBadRequest, graphError("Request_BadRequest"))
		return
	}
	enabled := body.AccountEnabled == nil || *body.AccountEnabled
	created := s.AddAgent(AgentIdentity{DisplayName: body.DisplayName, BlueprintID: body.BlueprintID,
		AccountEnabled: enabled, Tags: body.Tags, Sponsors: body.Sponsors})
	writeJSON(w, http.StatusCreated, created.graphJSON())
}

func (s *Server) graphGet(w http.ResponseWriter, id string, deleted bool) {
	s.mu.Lock()
	source := s.agents
	if deleted {
		source = s.deleted
	}
	a, ok := source[id]
	var body map[string]any
	if ok {
		body = a.graphJSON()
	}
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, graphError("Request_ResourceNotFound"))
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) graphPatch(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		AccountEnabled *bool     `json:"accountEnabled"`
		Tags           *[]string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || (body.AccountEnabled == nil && body.Tags == nil) {
		writeJSON(w, http.StatusBadRequest, graphError("Request_BadRequest"))
		return
	}
	s.mu.Lock()
	a, ok := s.agents[id]
	if ok {
		if body.AccountEnabled != nil {
			a.AccountEnabled = *body.AccountEnabled
		}
		if body.Tags != nil {
			// Graph replaces the whole collection.
			a.Tags = slices.Clone(*body.Tags)
		}
	}
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, graphError("Request_ResourceNotFound"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var filterClause = regexp.MustCompile(`^(?:(agentIdentityBlueprintId|appId|displayName) eq '([^']*)'|tags/any\(t:t eq '([^']*)'\))$`)

// graphList serves $filter (eq clauses joined by " and "), $top, $count
// and $skiptoken (an offset).
func (s *Server) graphList(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	var match []func(AgentIdentity) bool
	if filter := query.Get("$filter"); filter != "" {
		for _, clause := range strings.Split(filter, " and ") {
			parts := filterClause.FindStringSubmatch(clause)
			if parts == nil {
				writeJSON(w, http.StatusBadRequest, graphError("Request_UnsupportedQuery"))
				return
			}
			field, value, tag := parts[1], parts[2], parts[3]
			match = append(match, func(a AgentIdentity) bool {
				switch field {
				case "agentIdentityBlueprintId":
					return a.BlueprintID == value
				case "appId":
					return a.AppID == value
				case "displayName":
					return a.DisplayName == value
				}
				return slices.Contains(a.Tags, tag)
			})
		}
	}
	if (query.Get("$count") == "true" || strings.Contains(query.Get("$filter"), "tags/any")) &&
		r.Header.Get("ConsistencyLevel") != "eventual" {
		writeJSON(w, http.StatusBadRequest, graphError("Request_UnsupportedQuery"))
		return
	}
	advanced := r.Header.Get("ConsistencyLevel") == "eventual"
	s.mu.Lock()
	var all []AgentIdentity
	for _, a := range s.agents {
		if advanced && s.unindexed[a.ID] {
			continue
		}
		if !slices.ContainsFunc(match, func(m func(AgentIdentity) bool) bool { return !m(*a) }) {
			all = append(all, *a)
		}
	}
	s.mu.Unlock()
	slices.SortFunc(all, func(a, b AgentIdentity) int { return strings.Compare(a.ID, b.ID) })
	top := 100
	if raw := query.Get("$top"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxGraphPage {
			writeJSON(w, http.StatusBadRequest, graphError("Request_BadRequest"))
			return
		}
		top = n
	}
	offset := 0
	if raw := query.Get("$skiptoken"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > len(all) {
			writeJSON(w, http.StatusBadRequest, graphError("Request_BadRequest"))
			return
		}
		offset = n
	}
	end := min(offset+top, len(all))
	values := make([]map[string]any, 0, end-offset)
	for _, a := range all[offset:end] {
		values = append(values, a.graphJSON())
	}
	body := map[string]any{"value": values}
	if query.Get("$count") == "true" {
		body["@odata.count"] = len(all)
	}
	if end < len(all) {
		next := url.Values{}
		for k, v := range query {
			next[k] = v
		}
		next.Set("$skiptoken", strconv.Itoa(end))
		body["@odata.nextLink"] = fmt.Sprintf("%s/%s?%s", s.URL, graphAgentCollection, next.Encode())
	}
	writeJSON(w, http.StatusOK, body)
}
