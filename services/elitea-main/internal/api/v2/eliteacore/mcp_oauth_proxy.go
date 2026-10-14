package eliteacore

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpregistry"
)

const (
	mcpOAuthProxyMaxRequestBytes  = 64 * 1024
	mcpOAuthProxyMaxResponseBytes = 512 * 1024
	mcpOAuthProxyTimeout          = 30 * time.Second
	mcpOAuthProxyMaxListItems     = 32
)

var errMCPProxyInvalidRequest = errors.New("invalid MCP proxy request")

type mcpOAuthProxyRequest struct {
	AuthorizationReferenceOnly bool            `json:"authorization_reference_only,omitempty"`
	Resource                   string          `json:"resource,omitempty"`
	TokenEndpoint              string          `json:"token_endpoint"`
	Code                       string          `json:"code,omitempty"`
	RedirectURI                string          `json:"redirect_uri,omitempty"`
	ClientID                   string          `json:"client_id,omitempty"`
	ClientSecret               string          `json:"client_secret,omitempty"`
	ClientReference            string          `json:"client_reference,omitempty"`
	CodeVerifier               string          `json:"code_verifier,omitempty"`
	GrantType                  string          `json:"grant_type,omitempty"`
	RefreshToken               string          `json:"refresh_token,omitempty"`
	Scope                      string          `json:"scope,omitempty"`
	ToolkitID                  json.RawMessage `json:"toolkit_id,omitempty"`
	ToolkitType                string          `json:"toolkit_type,omitempty"`
	ConfigurationID            string          `json:"configuration_uuid,omitempty"`
	// ResourceMetadataURL is the protected-resource metadata URL the challenge
	// advertised. It is a hint: Main uses it only when it is same-origin as the
	// toolkit's own URL, and otherwise falls back to the well-known locations.
	ResourceMetadataURL string `json:"resource_metadata_url,omitempty"`
	UsedDCR             bool   `json:"used_dcr,omitempty"`
}

type mcpDCRProxyRequest struct {
	TokenEndpoint           string   `json:"token_endpoint,omitempty"`
	Resource                string   `json:"resource,omitempty"`
	RegistrationEndpoint    string   `json:"registration_endpoint"`
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name,omitempty"`
	GrantTypes              []string `json:"grant_types,omitempty"`
	ResponseTypes           []string `json:"response_types,omitempty"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method,omitempty"`
	ApplicationType         string   `json:"application_type,omitempty"`
	Scope                   string   `json:"scope,omitempty"`
	SoftwareID              string   `json:"software_id,omitempty"`
	SoftwareVersion         string   `json:"software_version,omitempty"`
}

type mcpOAuthCredentials struct {
	clientID     string
	clientSecret string
	scope        string
}

type mcpOAuthResolutionError struct {
	status int
	code   string
}

func (e *mcpOAuthResolutionError) Error() string { return e.code }

func (h *Handler) mcpOAuthProxy(w http.ResponseWriter, r *http.Request) {
	preventMCPCredentialCaching(w)
	if !h.requireMCPEnabled(w, r) {
		return
	}

	var body mcpOAuthProxyRequest
	if !decodeMCPProxyRequest(w, r, &body) {
		return
	}
	tokenEndpoint, err := validateMCPProxyURL(body.TokenEndpoint)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_token_endpoint"})
		return
	}
	if !h.mcpAuthorizationDestinationAllowed(w, r, tokenEndpoint, "invalid_token_endpoint") {
		return
	}
	grantType := body.GrantType
	if grantType == "" {
		grantType = "authorization_code"
	}
	if !validateMCPGrantRequest(w, body, grantType) {
		return
	}

	// One budget covers discovery and the exchange: the exchange gets whatever
	// the credential resolution left.
	budget := mcpOAuthProxyTimeout
	if h.mcpProxyBudget > 0 {
		budget = h.mcpProxyBudget
	}
	ctx, cancel := context.WithTimeout(r.Context(), budget)
	defer cancel()
	credentials, err := h.resolveMCPOAuthCredentials(ctx, chi.URLParam(r, "projectID"), tokenEndpoint, body)
	if err != nil {
		var resolutionError *mcpOAuthResolutionError
		if errors.As(err, &resolutionError) {
			writeJSON(w, resolutionError.status, map[string]any{"error": resolutionError.code})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "oauth_credentials_unavailable"})
		return
	}

	form := url.Values{"grant_type": {grantType}}
	if body.Resource != "" {
		form.Set("resource", body.Resource)
	}
	if credentials.clientID != "" {
		form.Set("client_id", credentials.clientID)
	}
	if credentials.clientSecret != "" {
		form.Set("client_secret", credentials.clientSecret)
	}
	if credentials.scope != "" {
		form.Set("scope", credentials.scope)
	}
	if grantType == "refresh_token" {
		form.Set("refresh_token", body.RefreshToken)
	} else {
		form.Set("code", body.Code)
		form.Set("redirect_uri", body.RedirectURI)
		if body.CodeVerifier != "" {
			form.Set("code_verifier", body.CodeVerifier)
		}
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx, http.MethodPost, tokenEndpoint.String(), strings.NewReader(form.Encode()),
	)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_token_endpoint"})
		return
	}
	httpRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpRequest.Header.Set("Accept", "application/json")

	response, err := h.doMCPAuthorizationRequest(httpRequest)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "token_exchange_failed"})
		return
	}
	defer func() { _ = response.Body.Close() }()

	providerBody, err := decodeOAuthProviderResponse(response.Body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "invalid_token_response"})
		return
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		writeJSON(w, http.StatusBadRequest, safeOAuthProviderError(
			"token_exchange_failed", providerBody,
			credentials.clientSecret, body.Code, body.RefreshToken, body.CodeVerifier,
		))
		return
	}
	if !validMCPCredentialResponse(providerBody, "access_token", "token_type", "refresh_token", "id_token", "scope") {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "invalid_token_response"})
		return
	}
	h.writeMCPTokenResponse(w, r, body, providerBody)
}

func (h *Handler) mcpDCRProxy(w http.ResponseWriter, r *http.Request) {
	preventMCPCredentialCaching(w)
	if !h.requireMCPEnabled(w, r) {
		return
	}

	var body mcpDCRProxyRequest
	if !decodeMCPProxyRequest(w, r, &body) {
		return
	}
	registrationEndpoint, err := validateMCPProxyURL(body.RegistrationEndpoint)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_registration_endpoint"})
		return
	}
	if !h.mcpAuthorizationDestinationAllowed(w, r, registrationEndpoint, "invalid_registration_endpoint") {
		return
	}
	if !validMCPProxyStringList(body.RedirectURIs, true) ||
		!validMCPProxyStringList(body.GrantTypes, false) ||
		!validMCPProxyStringList(body.ResponseTypes, false) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_dcr_request"})
		return
	}
	if body.TokenEndpoint != "" {
		if _, err := validateMCPProxyURL(body.TokenEndpoint); err != nil || len(body.TokenEndpoint) > 4096 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_token_endpoint"})
			return
		}
	}
	if body.Resource != "" {
		if _, err := validateMCPProxyURL(body.Resource); err != nil || len(body.Resource) > 4096 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_resource"})
			return
		}
	}

	grantTypes := body.GrantTypes
	if len(grantTypes) == 0 {
		grantTypes = []string{"authorization_code", "refresh_token"}
	}
	responseTypes := body.ResponseTypes
	if len(responseTypes) == 0 {
		responseTypes = []string{"code"}
	}
	authMethod := body.TokenEndpointAuthMethod
	if authMethod == "" {
		authMethod = "none"
	}
	applicationType := body.ApplicationType
	if applicationType == "" {
		applicationType = "web"
	}

	registration := map[string]any{
		"redirect_uris":              body.RedirectURIs,
		"grant_types":                grantTypes,
		"response_types":             responseTypes,
		"token_endpoint_auth_method": authMethod,
		"application_type":           applicationType,
	}
	setOptionalMCPDCRField(registration, "client_name", body.ClientName)
	setOptionalMCPDCRField(registration, "scope", body.Scope)
	setOptionalMCPDCRField(registration, "software_id", body.SoftwareID)
	setOptionalMCPDCRField(registration, "software_version", body.SoftwareVersion)
	requestBody, err := json.Marshal(registration)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_dcr_request"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), mcpOAuthProxyTimeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(
		ctx, http.MethodPost, registrationEndpoint.String(), bytes.NewReader(requestBody),
	)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_registration_endpoint"})
		return
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")

	response, err := h.doMCPAuthorizationRequest(httpRequest)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "dcr_failed"})
		return
	}
	defer func() { _ = response.Body.Close() }()

	providerBody, err := decodeOAuthJSONObject(response.Body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "invalid_dcr_response"})
		return
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		writeJSON(w, http.StatusBadRequest, safeOAuthProviderError("registration_failed", providerBody))
		return
	}
	if !validMCPCredentialResponse(providerBody, "client_id", "client_secret") {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "invalid_dcr_response"})
		return
	}
	h.writeMCPDCRClient(w, r, body, providerBody)
}

func preventMCPCredentialCaching(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}

// Keep optional legacy fields, including omitted token_type and string expiry.
// Reject a malformed credential before it crosses the browser boundary.
func validMCPCredentialResponse(provider map[string]any, required string, optional ...string) bool {
	if _, failed := provider["error"]; failed {
		return false
	}
	value, ok := provider[required].(string)
	if !ok || strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	for _, field := range optional {
		if value, present := provider[field]; present {
			text, ok := value.(string)
			if !ok || strings.ContainsAny(text, "\x00\r\n") {
				return false
			}
		}
	}
	return true
}

func decodeMCPProxyRequest(w http.ResponseWriter, r *http.Request, destination any) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, mcpOAuthProxyMaxRequestBytes+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return false
	}
	if len(raw) > mcpOAuthProxyMaxRequestBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "request_too_large"})
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(destination); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return false
	}
	return true
}

func validateMCPGrantRequest(w http.ResponseWriter, body mcpOAuthProxyRequest, grantType string) bool {
	if body.Resource != "" {
		if _, err := validateMCPProxyURL(body.Resource); err != nil || len(body.Resource) > 4096 || strings.Contains(body.Resource, "#") {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_resource"})
			return false
		}
	}
	switch grantType {
	case "authorization_code":
		if body.Code == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing_code"})
			return false
		}
		if body.RedirectURI == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing_redirect_uri"})
			return false
		}
	case "refresh_token":
		if body.RefreshToken == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing_refresh_token"})
			return false
		}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported_grant_type"})
		return false
	}
	return true
}

func (h *Handler) resolveMCPOAuthCredentials(
	ctx context.Context,
	projectIDText string,
	tokenEndpoint *url.URL,
	body mcpOAuthProxyRequest,
) (mcpOAuthCredentials, error) {
	if body.ClientReference != "" {
		return h.loadMCPDCRCredentials(ctx, projectIDText, tokenEndpoint, body)
	}
	credentials := mcpOAuthCredentials{
		clientID: body.ClientID,
		scope:    body.Scope,
	}
	secret, placeholder, err := normalizeMCPClientSecret(body.ClientSecret)
	if err != nil {
		return mcpOAuthCredentials{}, &mcpOAuthResolutionError{http.StatusBadRequest, "invalid_client_secret"}
	}
	credentials.clientSecret = secret

	toolkitID, present, err := decodeOptionalPositiveInt32(body.ToolkitID)
	if err != nil {
		return mcpOAuthCredentials{}, &mcpOAuthResolutionError{http.StatusBadRequest, "invalid_toolkit_id"}
	}
	if placeholder && !present {
		return mcpOAuthCredentials{}, &mcpOAuthResolutionError{http.StatusBadRequest, "toolkit_required"}
	}
	needsStoredCredentials := !body.UsedDCR &&
		(credentials.clientID == "" || credentials.clientSecret == "" || placeholder)
	// RFC 6749 section 6: an omitted refresh scope means the original grant.
	// Toolkit defaults may be broader than what the user actually authorized.
	needsStoredScope := body.GrantType != "refresh_token" && credentials.scope == "" && h.delegatedAuth != nil
	if !present || (!needsStoredCredentials && !needsStoredScope) {
		return credentials, nil
	}
	projectID, actorID, err := delegatedAuthIdentity(ctx, projectIDText)
	if err != nil {
		return mcpOAuthCredentials{}, err
	}
	if h.delegatedAuth == nil {
		return mcpOAuthCredentials{}, &mcpOAuthResolutionError{http.StatusServiceUnavailable, "oauth_credentials_unavailable"}
	}
	resolved, found, err := h.delegatedAuth.ResolveDelegatedAuthToolkitSettings(
		ctx, projectID, actorID, toolkitID,
	)
	if err != nil {
		return mcpOAuthCredentials{}, err
	}
	if !found {
		return mcpOAuthCredentials{}, &mcpOAuthResolutionError{http.StatusNotFound, "toolkit_not_found"}
	}

	effectiveType, err := effectiveOAuthToolkitType(resolved.ToolkitType, body.ToolkitType, resolved.Settings)
	if err != nil {
		return mcpOAuthCredentials{}, &mcpOAuthResolutionError{http.StatusBadRequest, "toolkit_type_mismatch"}
	}
	settings, prebuilt, err := h.resolvePrebuiltSettingsReport(ctx, resolved.Settings, effectiveType)
	if err != nil {
		return mcpOAuthCredentials{}, &mcpOAuthResolutionError{http.StatusServiceUnavailable, "prebuilt_catalogue_unavailable"}
	}
	layers := oauthSettingsLayers(settings)
	storedSecret := ""
	if !body.UsedDCR {
		if credentials.clientID == "" {
			credentials.clientID = firstOAuthText(layers, "client_id")
		}
		if credentials.clientSecret == "" {
			if prebuilt.filled.ClientSecret {
				// The catalogue's own value, exactly: not whichever layer of
				// the toolkit's settings happens to hold a secret first.
				storedSecret, _ = settings["client_secret"].(string)
			} else {
				storedSecret = firstOAuthText(layers, "client_secret")
			}
			credentials.clientSecret = storedSecret
		}
	}
	if needsStoredScope {
		credentials.scope = firstOAuthScope(layers)
	}
	if storedSecret == "" {
		return credentials, nil
	}
	// THE RELEASE RULE. A client secret is released only to a token endpoint
	// its OWNER controls. Ownership is what mcpregistry.ResolveReport says: a
	// secret the catalogue filled belongs to the operator, and nothing else
	// decides it. The operator's secret goes only to (1) the token_url pinned
	// on the catalogue entry, or (2) the token endpoint discovered from the
	// entry's EXACT url when that url has no placeholder. A url template, or an
	// entry with neither, refuses the release and tells the operator to pin a
	// token_url. Nothing a toolkit editor can write (its url, token_endpoint,
	// token_url, oauth_discovery_endpoint, a metadata hint) is consulted for it.
	// A secret the toolkit's own settings supplied may follow those settings:
	// its saved token settings, or discovery from its saved url.
	if prebuilt.filled.ClientSecret {
		if failure := h.releaseCatalogueSecret(ctx, prebuilt.entry, projectID, toolkitID, tokenEndpoint); failure != nil {
			return mcpOAuthCredentials{}, failure
		}
		return credentials, nil
	}
	if oauthTokenEndpointIsBound(tokenEndpoint, layers) {
		return credentials, nil
	}
	saved, _ := settings["url"].(string)
	if effectiveType == "mcp" || mcpregistry.IsPrebuiltToolkitType(effectiveType) {
		failure := h.bindingFailure(h.mcpDiscoveryBindsTokenEndpoint(ctx, mcpBindingRequest{
			projectID: projectID, toolkitID: toolkitID, startURL: saved,
			tokenEndpoint: tokenEndpoint, advertised: body.ResourceMetadataURL,
		}))
		if failure != nil {
			return mcpOAuthCredentials{}, failure
		}
		return credentials, nil
	}
	return mcpOAuthCredentials{}, &mcpOAuthResolutionError{http.StatusBadRequest, "untrusted_token_endpoint"}
}

func delegatedAuthIdentity(ctx context.Context, projectIDText string) (int32, int32, error) {
	projectID64, err := strconv.ParseInt(projectIDText, 10, 32)
	if err != nil || projectID64 <= 0 {
		return 0, 0, &mcpOAuthResolutionError{http.StatusBadRequest, "invalid_project_id"}
	}
	principal, ok := auth.UserFromContext(ctx)
	if !ok {
		return 0, 0, &mcpOAuthResolutionError{http.StatusUnauthorized, "authentication_required"}
	}
	actorID, ok := principal.OwningUserID()
	if !ok || actorID <= 0 || actorID > math.MaxInt32 {
		return 0, 0, &mcpOAuthResolutionError{http.StatusUnauthorized, "authentication_required"}
	}
	return int32(projectID64), int32(actorID), nil
}

func decodeOptionalPositiveInt32(raw json.RawMessage) (int32, bool, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, false, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return 0, false, errMCPProxyInvalidRequest
	}
	var text string
	switch typed := value.(type) {
	case string:
		text = typed
	case json.Number:
		text = typed.String()
	default:
		return 0, false, errMCPProxyInvalidRequest
	}
	id, err := strconv.ParseInt(text, 10, 32)
	if err != nil || id <= 0 {
		return 0, false, errMCPProxyInvalidRequest
	}
	return int32(id), true, nil
}

func normalizeMCPClientSecret(value string) (string, bool, error) {
	if value == "" {
		return "", false, nil
	}
	if strings.HasPrefix(value, "*") {
		return "", true, nil
	}
	if strings.Contains(value, "{{") || strings.Contains(value, "}}") {
		if strings.HasPrefix(value, "{{secret.") && strings.HasSuffix(value, "}}") &&
			len(strings.TrimSuffix(strings.TrimPrefix(value, "{{secret."), "}}")) > 0 {
			return "", true, nil
		}
		return "", false, errMCPProxyInvalidRequest
	}
	return value, false, nil
}

func effectiveOAuthToolkitType(storedType, requestedType string, settings map[string]any) (string, error) {
	if storedType == "" || settings == nil {
		return "", errMCPProxyInvalidRequest
	}
	if requestedType == "" || requestedType == storedType {
		if storedType == "mcp_config" {
			if serverName, _ := settings["server_name"].(string); serverName != "" {
				return mcpregistry.PrebuiltToolkitTypePrefix + mcpregistry.NormalizeCatalogueKey(serverName), nil
			}
		}
		return storedType, nil
	}
	if storedType == "mcp_config" {
		serverName, _ := settings["server_name"].(string)
		if mcpregistry.IsPrebuiltToolkitType(requestedType) &&
			mcpregistry.NormalizeCatalogueKey(requestedType) == mcpregistry.NormalizeCatalogueKey(serverName) {
			return requestedType, nil
		}
	}
	return "", errMCPProxyInvalidRequest
}

func oauthSettingsLayers(settings map[string]any) []map[string]any {
	layers := []map[string]any{settings}
	for _, key := range []string{"sharepoint_configuration", "openapi_configuration"} {
		if nested, ok := settings[key].(map[string]any); ok && nested != nil {
			layers = append(layers, nested)
		}
	}
	return layers
}

func firstOAuthText(layers []map[string]any, keys ...string) string {
	for _, layer := range layers {
		for _, key := range keys {
			if value, ok := layer[key].(string); ok && value != "" {
				return value
			}
		}
	}
	return ""
}

func firstOAuthScope(layers []map[string]any) string {
	for _, layer := range layers {
		for _, key := range []string{"scopes", "scope"} {
			switch value := layer[key].(type) {
			case string:
				if value != "" {
					return value
				}
			case []string:
				if len(value) > 0 {
					return strings.Join(value, " ")
				}
			case []any:
				parts := make([]string, 0, len(value))
				for _, item := range value {
					text, ok := item.(string)
					if !ok || text == "" {
						parts = nil
						break
					}
					parts = append(parts, text)
				}
				if len(parts) > 0 {
					return strings.Join(parts, " ")
				}
			}
		}
	}
	return ""
}

func oauthTokenEndpointIsBound(endpoint *url.URL, layers []map[string]any) bool {
	for _, layer := range layers {
		for _, key := range []string{"token_endpoint", "token_url", "oauth_token_endpoint"} {
			candidate, _ := layer[key].(string)
			trusted, err := validateMCPProxyURL(candidate)
			if err == nil && sameMCPProxyEndpoint(endpoint, trusted) {
				return true
			}
		}
		candidate, _ := layer["oauth_discovery_endpoint"].(string)
		trusted, err := validateMCPProxyURL(candidate)
		if err == nil && sameMCPProxyOrigin(endpoint, trusted) && urlPathContains(trusted.Path, endpoint.Path) {
			return true
		}
	}
	return false
}

func sameMCPProxyEndpoint(first, second *url.URL) bool {
	return sameMCPProxyOrigin(first, second) &&
		strings.TrimSuffix(first.EscapedPath(), "/") == strings.TrimSuffix(second.EscapedPath(), "/") &&
		first.RawQuery == second.RawQuery
}

func urlPathContains(basePath, endpointPath string) bool {
	basePath = strings.TrimSuffix(basePath, "/")
	endpointPath = strings.TrimSuffix(endpointPath, "/")
	if basePath == "" {
		return true
	}
	return endpointPath == basePath || strings.HasPrefix(endpointPath, basePath+"/")
}

func validMCPProxyStringList(values []string, required bool) bool {
	if required && len(values) == 0 || len(values) > mcpOAuthProxyMaxListItems {
		return false
	}
	for _, value := range values {
		if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
			return false
		}
	}
	return true
}

func setOptionalMCPDCRField(target map[string]any, name, value string) {
	if value != "" {
		target[name] = value
	}
}

func decodeOAuthProviderResponse(body io.Reader) (map[string]any, error) {
	raw, err := readBoundedMCPProxyResponse(body)
	if err != nil {
		return nil, err
	}
	if object, err := decodeJSONBytesObject(raw); err == nil {
		return object, nil
	}
	if !bytes.Contains(raw, []byte("=")) {
		return nil, errMCPProxyInvalidRequest
	}
	values, err := url.ParseQuery(string(raw))
	if err != nil || len(values) == 0 {
		return nil, errMCPProxyInvalidRequest
	}
	object := make(map[string]any, len(values))
	for key, items := range values {
		if len(items) == 1 {
			object[key] = items[0]
		} else {
			object[key] = items
		}
	}
	return object, nil
}

func decodeOAuthJSONObject(body io.Reader) (map[string]any, error) {
	raw, err := readBoundedMCPProxyResponse(body)
	if err != nil {
		return nil, err
	}
	return decodeJSONBytesObject(raw)
}

func readBoundedMCPProxyResponse(body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, mcpOAuthProxyMaxResponseBytes+1))
	if err != nil || len(raw) == 0 || len(raw) > mcpOAuthProxyMaxResponseBytes {
		return nil, errMCPProxyInvalidRequest
	}
	return raw, nil
}

func decodeJSONBytesObject(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, errMCPProxyInvalidRequest
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errMCPProxyInvalidRequest
	}
	return object, nil
}

func oauthTokenResponse(provider map[string]any) map[string]any {
	response := make(map[string]any, 9)
	for _, key := range []string{
		"access_token", "token_type", "expires_in", "refresh_token", "id_token",
		"session_id", "scope", "issued_token_type", "expires_at",
	} {
		if value, ok := provider[key]; ok {
			response[key] = value
		}
	}
	return response
}

func safeOAuthProviderError(code string, provider map[string]any, sensitive ...string) map[string]any {
	response := map[string]any{"error": code}
	description := ""
	for _, key := range []string{"error_description", "error"} {
		if value, ok := provider[key].(string); ok && value != "" {
			description = value
			break
		}
	}
	// Match longer credentials first. Do not scan replacement text again.
	slices.SortFunc(sensitive, func(first, second string) int { return len(second) - len(first) })
	replacements := make([]string, 0, 2*len(sensitive))
	for _, value := range sensitive {
		if value != "" {
			replacements = append(replacements, value, "[redacted]")
		}
	}
	description = strings.NewReplacer(replacements...).Replace(description)
	if len(description) > 1024 {
		description = description[:1024]
	}
	if description != "" {
		response["error_description"] = description
	}
	return response
}

const (
	mcpBindingCacheTTL  = time.Hour
	mcpBindingCacheSize = 1024
)

type bindingOutcome int

const (
	bindingUntrusted bindingOutcome = iota
	bindingBound
	bindingTimedOut
)

func (h *Handler) bindingFailure(outcome bindingOutcome) *mcpOAuthResolutionError {
	switch outcome {
	case bindingBound:
		return nil
	case bindingTimedOut:
		return &mcpOAuthResolutionError{http.StatusGatewayTimeout, "oauth_discovery_timeout"}
	default:
		return &mcpOAuthResolutionError{http.StatusBadRequest, "untrusted_token_endpoint"}
	}
}

// releaseCatalogueSecret applies the release rule to an operator-owned secret.
func (h *Handler) releaseCatalogueSecret(
	ctx context.Context, entry mcpregistry.PrebuiltServer, projectID, toolkitID int32, tokenEndpoint *url.URL,
) *mcpOAuthResolutionError {
	if pin := strings.TrimSpace(entry.TokenURL); pin != "" {
		trusted, err := validateMCPProxyURL(pin)
		if err == nil && sameMCPProxyEndpoint(tokenEndpoint, trusted) {
			return nil
		}
		return &mcpOAuthResolutionError{http.StatusBadRequest, "untrusted_token_endpoint"}
	}
	server := strings.TrimSpace(entry.ServerURL)
	if server == "" || strings.ContainsAny(server, "{}") {
		return &mcpOAuthResolutionError{http.StatusBadRequest, "catalogue_token_endpoint_not_pinned"}
	}
	return h.bindingFailure(h.mcpDiscoveryBindsTokenEndpoint(ctx, mcpBindingRequest{
		projectID: projectID, toolkitID: toolkitID, startURL: server, catalogue: true,
		tokenEndpoint: tokenEndpoint,
	}))
}

// mcpBindingRequest is everything the binding decision depends on. For a
// catalogue secret startURL is the catalogue entry's own URL and advertised is
// always empty; only an own secret may carry a hint, and only a same-origin one.
type mcpBindingRequest struct {
	projectID     int32
	toolkitID     int32
	startURL      string
	catalogue     bool
	tokenEndpoint *url.URL
	advertised    string
}

type mcpBindingKey struct {
	projectID, toolkitID int32
	startURL             string
	catalogue            bool
}

type mcpBindingEntry struct {
	key       mcpBindingKey
	endpoints []string
	expires   time.Time
}

// mcpBindingCache remembers the token endpoints a server advertised so a
// refresh does not re-run discovery and a brief metadata outage does not strand
// a signed-in user. It is a bounded LRU: a full cache evicts its least recently
// used entry, never everything, and each toolkit holds at most one entry, so
// editing its URL replaces the old one.
type mcpBindingCache struct {
	mu        sync.Mutex
	capacity  int
	order     *list.List
	entries   map[mcpBindingKey]*list.Element
	byToolkit map[[2]int32]mcpBindingKey
}

func (c *mcpBindingCache) init() {
	if c.order == nil {
		c.order = list.New()
		c.entries = make(map[mcpBindingKey]*list.Element)
		c.byToolkit = make(map[[2]int32]mcpBindingKey)
	}
	if c.capacity <= 0 {
		c.capacity = mcpBindingCacheSize
	}
}

func (c *mcpBindingCache) remove(element *list.Element) {
	entry := element.Value.(*mcpBindingEntry)
	c.order.Remove(element)
	delete(c.entries, entry.key)
	if c.byToolkit[[2]int32{entry.key.projectID, entry.key.toolkitID}] == entry.key {
		delete(c.byToolkit, [2]int32{entry.key.projectID, entry.key.toolkitID})
	}
}

func (c *mcpBindingCache) lookup(key mcpBindingKey, endpoint *url.URL, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.init()
	element, ok := c.entries[key]
	if !ok {
		return false
	}
	entry := element.Value.(*mcpBindingEntry)
	if !now.Before(entry.expires) {
		c.remove(element)
		return false
	}
	c.order.MoveToFront(element)
	for _, candidate := range entry.endpoints {
		if trusted, err := validateMCPProxyURL(candidate); err == nil && sameMCPProxyEndpoint(endpoint, trusted) {
			return true
		}
	}
	return false
}

func (c *mcpBindingCache) store(key mcpBindingKey, endpoints []string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.init()
	if element, ok := c.entries[key]; ok {
		c.remove(element)
	}
	owner := [2]int32{key.projectID, key.toolkitID}
	if previous, ok := c.byToolkit[owner]; ok {
		if element, ok := c.entries[previous]; ok {
			c.remove(element)
		}
	}
	c.entries[key] = c.order.PushFront(&mcpBindingEntry{key: key, endpoints: endpoints, expires: now.Add(mcpBindingCacheTTL)})
	c.byToolkit[owner] = key
	for c.order.Len() > c.capacity {
		c.remove(c.order.Back())
	}
}

// mcpDiscoveryBindsTokenEndpoint repeats the public discovery the worker ran,
// starting from req.startURL, and reports whether the server itself advertises
// the endpoint. Nothing the caller or model supplies can nominate the
// destination; a hint is honoured only when same-origin, and a failed hint
// falls back to the well-known locations.
func (h *Handler) mcpDiscoveryBindsTokenEndpoint(ctx context.Context, req mcpBindingRequest) bindingOutcome {
	resourceURL, err := validateMCPMetadataURL(req.startURL)
	if err != nil {
		return bindingUntrusted
	}
	key := mcpBindingKey{projectID: req.projectID, toolkitID: req.toolkitID, startURL: resourceURL.String(), catalogue: req.catalogue}
	now := time.Now
	if h.mcpBindingClock != nil {
		now = h.mcpBindingClock
	}
	if h.mcpBindings.lookup(key, req.tokenEndpoint, now()) {
		return bindingBound
	}
	source := h.authorizationMetadataSource()
	if h.mcpDiscoveryFetchTimeout > 0 {
		source.perFetch = h.mcpDiscoveryFetchTimeout
	}
	matches := func(issuer map[string]any) bool {
		advertised, _ := issuer["token_endpoint"].(string)
		trusted, err := validateMCPProxyURL(advertised)
		return err == nil && sameMCPProxyEndpoint(req.tokenEndpoint, trusted)
	}
	attempt := func(advertised string) (mcpDiscovered, error) {
		return source.discover(ctx, resourceURL, advertised, mcpBindingMaxServers, true, matches)
	}
	var found mcpDiscovered
	if hint, err := validateMCPMetadataURL(req.advertised); !req.catalogue && req.advertised != "" &&
		err == nil && sameMCPProxyOrigin(hint, resourceURL) {
		found, err = attempt(req.advertised)
		if err != nil {
			found, err = attempt("")
		}
	} else {
		found, _ = attempt("")
	}
	var endpoints []string
	bound := false
	for _, issuer := range found.issuers {
		if advertised, _ := issuer["token_endpoint"].(string); advertised != "" {
			endpoints = append(endpoints, advertised)
		}
		if matches(issuer) {
			bound = true
		}
	}
	switch {
	case bound:
		h.mcpBindings.store(key, endpoints, now())
		return bindingBound
	case found.timedOut:
		return bindingTimedOut
	default:
		return bindingUntrusted
	}
}
