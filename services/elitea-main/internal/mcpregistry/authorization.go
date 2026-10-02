package mcpregistry

import (
	"mime"
	"strings"
)

// AuthorizationRequired preserves a bounded challenge, never a provider body.
type AuthorizationRequired struct {
	challenges []string
}

func (*AuthorizationRequired) Error() string { return "MCP authorization required" }

func authorizationRequired(headers []string) *AuthorizationRequired {
	result := &AuthorizationRequired{}
	total := 0
	for _, header := range headers {
		total += len(header)
		if total > 8192 || len(headers) > 16 {
			return result
		}
	}
	result.challenges = headers
	return result
}

// ResourceMetadataURL accepts only a Bearer challenge parameter. Quoted commas
// remain part of their value. Conflicting or malformed parameters fail closed.
func (e *AuthorizationRequired) ResourceMetadataURL() string {
	var found string
	for _, header := range e.challenges {
		parts, valid := splitAuthFields(header)
		if !valid {
			return ""
		}
		for i := 0; i < len(parts); i++ {
			scheme, params, ok := strings.Cut(strings.TrimSpace(parts[i]), " ")
			if !ok || !strings.EqualFold(scheme, "Bearer") {
				continue
			}
			value := "Bearer; " + params
			for i+1 < len(parts) && isAuthParameter(parts[i+1]) {
				i++
				value += "; " + parts[i]
			}
			_, fields, err := mime.ParseMediaType(value)
			if err != nil {
				return ""
			}
			if resource := fields["resource_metadata"]; resource != "" {
				if found != "" {
					return ""
				}
				found = resource
			}
		}
	}
	return found
}

func isAuthParameter(field string) bool {
	key, _, ok := strings.Cut(strings.TrimSpace(field), "=")
	return ok && key != "" && !strings.ContainsAny(strings.TrimSpace(key), " \t")
}

func splitAuthFields(value string) ([]string, bool) {
	var fields []string
	quoted, escaped, start := false, false, 0
	for i, char := range value {
		if escaped {
			escaped = false
			continue
		}
		if quoted && char == '\\' {
			escaped = true
			continue
		}
		if char == '"' {
			quoted = !quoted
		}
		if char == ',' && !quoted {
			fields = append(fields, value[start:i])
			start = i + 1
		}
	}
	return append(fields, value[start:]), !quoted && !escaped
}
