package eliteacore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
	TokenEndpoint   string          `json:"token_endpoint"`
	Code            string          `json:"code,omitempty"`
	RedirectURI     string          `json:"redirect_uri,omitempty"`
	ClientID        string          `json:"client_id,omitempty"`
	ClientSecret    string          `json:"client_secret,omitempty"`
	CodeVerifier    string          `json:"code_verifier,omitempty"`
	GrantType       string          `json:"grant_type,omitempty"`
	RefreshToken    string          `json:"refresh_token,omitempty"`
	Scope           string          `json:"scope,omitempty"`
	ToolkitID       json.RawMessage `json:"toolkit_id,omitempty"`
	ToolkitType     string          `json:"toolkit_type,omitempty"`
	ConfigurationID string          `json:"configuration_uuid,omitempty"`
	UsedDCR         bool            `json:"used_dcr,omitempty"`
}

type mcpDCRProxyRequest struct {
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
	grantType := body.GrantType
	if grantType == "" {
		grantType = "authorization_code"
	}
	if !validateMCPGrantRequest(w, body, grantType) {
		return
	}

	credentials, err := h.resolveMCPOAuthCredentials(r.Context(), chi.URLParam(r, "projectID"), tokenEndpoint, body)
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

	ctx, cancel := context.WithTimeout(r.Context(), mcpOAuthProxyTimeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(
		ctx, http.MethodPost, tokenEndpoint.String(), strings.NewReader(form.Encode()),
	)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_token_endpoint"})
		return
	}
	httpRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpRequest.Header.Set("Accept", "application/json")

	response, err := h.doMCPProxyRequest(httpRequest)
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
			credentials.clientSecret, body.Code, body.RefreshToken,
		))
		return
	}
	writeJSON(w, http.StatusOK, oauthTokenResponse(providerBody))
}

func (h *Handler) mcpDCRProxy(w http.ResponseWriter, r *http.Request) {
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
	if !validMCPProxyStringList(body.RedirectURIs, true) ||
		!validMCPProxyStringList(body.GrantTypes, false) ||
		!validMCPProxyStringList(body.ResponseTypes, false) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_dcr_request"})
		return
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

	response, err := h.doMCPProxyRequest(httpRequest)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "dcr_failed"})
		return
	}
	defer func() { _ = response.Body.Close() }()

	providerBody, err := decodeJSONObject(response.Body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "invalid_dcr_response"})
		return
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		writeJSON(w, http.StatusBadRequest, safeOAuthProviderError("registration_failed", providerBody))
		return
	}
	writeJSON(w, http.StatusOK, providerBody)
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
	needsStoredScope := credentials.scope == "" && h.delegatedAuth != nil
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
	settings, err := h.resolvePrebuiltSettings(ctx, resolved.Settings, effectiveType)
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
			storedSecret = firstOAuthText(layers, "client_secret")
			credentials.clientSecret = storedSecret
		}
	}
	if credentials.scope == "" {
		credentials.scope = firstOAuthScope(layers)
	}
	if storedSecret != "" && !oauthTokenEndpointIsBound(tokenEndpoint, layers) {
		return mcpOAuthCredentials{}, &mcpOAuthResolutionError{http.StatusBadRequest, "untrusted_token_endpoint"}
	}
	return credentials, nil
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

func decodeJSONObject(body io.Reader) (map[string]any, error) {
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
	if len(description) > 1024 {
		description = description[:1024]
	}
	for _, value := range sensitive {
		if len(value) >= 4 {
			description = strings.ReplaceAll(description, value, "[redacted]")
		}
	}
	if description != "" {
		response["error_description"] = description
	}
	return response
}
