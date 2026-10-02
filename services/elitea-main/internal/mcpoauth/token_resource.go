package mcpoauth

import (
	"encoding/json"
	"net"
	"net/url"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ToolkitResource follows the native worker's endpoint precedence.
// Callers must resolve configuration references and prebuilt templates first.
func ToolkitResource(toolkitType string, settings map[string]any) (string, error) {
	var resource string
	switch toolkitType {
	case "mcp":
		resource, _ = settings["url"].(string)
	case "openapi":
		var err error
		resource, err = openAPIResource(settings)
		if err != nil {
			return "", err
		}
	default:
		return "", ErrTokenUnavailable
	}
	return CanonicalResource(toolkitType, resource)
}

// CanonicalResource matches MCP URL serialization and OpenAPI's slash trimming.
func CanonicalResource(toolkitType, resource string) (string, error) {
	if len(resource) == 0 || len(resource) > 4096 || strings.ContainsAny(resource, "\\%\x00\r\n\t ") {
		return "", ErrTokenUnavailable
	}
	u, err := url.Parse(resource)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(resource, "#") {
		return "", ErrTokenUnavailable
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", ErrTokenUnavailable
		}
	}
	for _, ch := range host {
		if ch > 127 {
			return "", ErrTokenUnavailable
		}
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return "", ErrTokenUnavailable
		}
	}
	if port != "" && port != "443" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	u.Host = host
	if u.Path == "" {
		u.Path = "/"
	}
	result := u.String()
	if toolkitType == "openapi" {
		result = strings.TrimRight(result, "/")
	}
	return result, nil
}

func openAPIResource(settings map[string]any) (string, error) {
	for _, key := range []string{"base_url", "base_url_override"} {
		value, present := settings[key]
		if !present || value == nil {
			continue
		}
		text, ok := value.(string)
		if !ok {
			return "", ErrTokenUnavailable
		}
		if text != "" {
			return text, nil
		}
	}
	var spec any
	for _, key := range []string{"spec", "schema_settings", "openapi_spec"} {
		if value, present := settings[key]; present {
			spec = value
			break
		}
	}
	if text, ok := spec.(string); ok {
		if len(text) > 2*1024*1024 || strings.HasPrefix(strings.TrimSpace(text), "https://") || strings.HasPrefix(strings.TrimSpace(text), "http://") {
			return "", ErrTokenUnavailable
		}
		if json.Unmarshal([]byte(text), &spec) != nil {
			var root map[string]any
			if yaml.Unmarshal([]byte(text), &root) != nil {
				return "", ErrTokenUnavailable
			}
			spec = root
		}
	}
	root, ok := spec.(map[string]any)
	if !ok {
		return "", ErrTokenUnavailable
	}
	servers, ok := root["servers"].([]any)
	if !ok || len(servers) == 0 {
		return "", ErrTokenUnavailable
	}
	server, ok := servers[0].(map[string]any)
	if !ok {
		return "", ErrTokenUnavailable
	}
	expanded, ok := server["url"].(string)
	if !ok || len(expanded) > 4096 {
		return "", ErrTokenUnavailable
	}
	variables, _ := server["variables"].(map[string]any)
	for count := 0; strings.Contains(expanded, "{"); count++ {
		if count >= 32 {
			return "", ErrTokenUnavailable
		}
		start := strings.Index(expanded, "{")
		end := strings.Index(expanded[start+1:], "}")
		if end < 0 {
			return "", ErrTokenUnavailable
		}
		end += start + 1
		variable, _ := variables[expanded[start+1:end]].(map[string]any)
		replacement, _ := variable["default"].(string)
		if replacement == "" || strings.ContainsAny(replacement, "{}") || len(replacement) > 4096 {
			return "", ErrTokenUnavailable
		}
		expanded = expanded[:start] + replacement + expanded[end+1:]
		if len(expanded) > 4096 {
			return "", ErrTokenUnavailable
		}
	}
	if strings.Contains(expanded, "}") {
		return "", ErrTokenUnavailable
	}
	return expanded, nil
}
