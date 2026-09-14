package execution

import (
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const ToolkitAuthorizationMessage = "Authorization is required to run this tool."

// ToolkitAuthorizationRequired contains public consent metadata, never credentials.
type ToolkitAuthorizationRequired struct {
	ToolkitName         string          `json:"toolkit_name"`
	ToolkitType         string          `json:"toolkit_type"`
	ToolkitID           string          `json:"toolkit_id"`
	ServerURL           string          `json:"server_url"`
	ResourceMetadataURL string          `json:"resource_metadata_url,omitempty"`
	ResourceMetadata    json.RawMessage `json:"resource_metadata,omitempty"`
}

func (a ToolkitAuthorizationRequired) Validate() error {
	invalid := errors.New("invalid toolkit authorization challenge")
	for _, value := range []string{a.ToolkitName, a.ToolkitType, a.ToolkitID} {
		if !authorizationText(value, 1024) {
			return invalid
		}
	}
	if id, err := strconv.ParseInt(a.ToolkitID, 10, 64); err != nil || id <= 0 || strconv.FormatInt(id, 10) != a.ToolkitID {
		return invalid
	}
	if !authorizationURL(a.ServerURL) || a.ResourceMetadataURL != "" && !authorizationURL(a.ResourceMetadataURL) {
		return invalid
	}
	if len(a.ResourceMetadata) == 0 {
		return nil
	}
	if len(a.ResourceMetadata) > 16*1024 {
		return invalid
	}
	var metadata map[string]json.RawMessage
	if json.Unmarshal(a.ResourceMetadata, &metadata) != nil || metadata == nil {
		return invalid
	}
	for key, value := range metadata {
		switch key {
		case "authorization_servers":
			if !authorizationList(value, true) {
				return invalid
			}
		case "scopes_supported":
			if !authorizationList(value, false) {
				return invalid
			}
		case "resource_name", "configuration_uuid", "toolkit_id":
			var text string
			if json.Unmarshal(value, &text) != nil || !authorizationText(text, 1024) || key == "toolkit_id" && text != a.ToolkitID {
				return invalid
			}
		case "provided_settings":
			if !authorizationProvided(value) {
				return invalid
			}
		case "oauth_authorization_server":
			if !authorizationServer(value) {
				return invalid
			}
		default:
			return invalid
		}
	}
	var servers []string
	if json.Unmarshal(metadata["authorization_servers"], &servers) != nil || len(servers) == 0 {
		return invalid
	}
	if metadata["oauth_authorization_server"] == nil && metadata["provided_settings"] == nil {
		return invalid
	}
	return nil
}

func authorizationText(value string, max int) bool {
	return value != "" && len(value) <= max && utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}
func authorizationURL(value string) bool {
	if !authorizationText(value, 4096) {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Hostname() != "" && parsed.User == nil && parsed.Fragment == ""
}
func authorizationList(raw json.RawMessage, urls bool) bool {
	var values []string
	if json.Unmarshal(raw, &values) != nil || values == nil || len(values) > 64 {
		return false
	}
	for _, value := range values {
		if !authorizationText(value, 4096) || urls && !authorizationURL(value) {
			return false
		}
	}
	return true
}
func authorizationProvided(raw json.RawMessage) bool {
	var values map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil || values == nil {
		return false
	}
	var client string
	if json.Unmarshal(values["mcp_client_id"], &client) != nil || !authorizationText(client, 1024) {
		return false
	}
	for key, value := range values {
		switch key {
		case "mcp_client_id":
		case "mcp_client_secret":
			var secret string
			if json.Unmarshal(value, &secret) != nil || secret != "********" {
				return false
			}
		case "scopes":
			if !authorizationList(value, false) {
				return false
			}
		default:
			return false
		}
	}
	return true
}
func authorizationServer(raw json.RawMessage) bool {
	var values map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil || values == nil {
		return false
	}
	for _, key := range []string{"authorization_endpoint", "token_endpoint"} {
		var value string
		if json.Unmarshal(values[key], &value) != nil || !authorizationURL(value) {
			return false
		}
	}
	for key, raw := range values {
		switch key {
		case "authorization_endpoint", "token_endpoint", "registration_endpoint", "issuer", "jwks_uri", "revocation_endpoint", "userinfo_endpoint":
			var value string
			if json.Unmarshal(raw, &value) != nil || !authorizationURL(value) {
				return false
			}
		case "scopes_supported", "response_types_supported", "grant_types_supported", "token_endpoint_auth_methods_supported", "code_challenge_methods_supported":
			if !authorizationList(raw, false) {
				return false
			}
		default:
			return false
		}
	}
	return true
}
