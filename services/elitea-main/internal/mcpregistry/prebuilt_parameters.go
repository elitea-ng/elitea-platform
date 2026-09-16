package mcpregistry

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	maxPrebuiltParameters  = 64
	maxPrebuiltSchemaBytes = 64 * 1024
	maxPrebuiltStringBytes = 16 * 1024
)

var (
	ErrInvalidPrebuiltParameters = errors.New("mcpregistry: invalid pre-built MCP parameters")
	prebuiltParameterName        = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	prebuiltPlaceholder          = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)
)

var reservedPrebuiltParameterNames = map[string]struct{}{
	"base_url": {}, "cache_ttl": {}, "client_id": {}, "client_secret": {},
	"enable_caching": {}, "excluded_tools": {}, "headers": {}, "selected_tools": {},
	"server_name": {}, "ssl_verify": {}, "timeout": {}, "url": {},
	// These values belong to the authenticated execution. An operator can use
	// them in a trusted prebuilt template, but a toolkit form must never ask a
	// user to provide or persist either value.
	"personal_token": {}, "project_id": {},
}

var runtimePrebuiltParameterNames = map[string]struct{}{
	"personal_token": {},
	"project_id":     {},
}

// ValidatePrebuiltServer validates operator-owned templates and parameter metadata.
func ValidatePrebuiltServer(entry PrebuiltServer) error {
	properties, err := PrebuiltConfigProperties(entry.ConfigSchema)
	if err != nil {
		return err
	}
	declared := make(map[string]struct{}, len(properties))
	for name, raw := range properties {
		if !prebuiltParameterName.MatchString(name) {
			return invalidPrebuiltParameter("parameter name is invalid")
		}
		if _, reserved := reservedPrebuiltParameterNames[name]; reserved {
			return invalidPrebuiltParameter("parameter name is reserved")
		}
		property, ok := raw.(map[string]any)
		if !ok {
			return invalidPrebuiltParameter("parameter schema is not an object")
		}
		if err := validatePrebuiltProperty(property); err != nil {
			return err
		}
		declared[name] = struct{}{}
	}
	templateParameters := make(map[string]struct{}, len(declared)+len(runtimePrebuiltParameterNames))
	for name := range declared {
		templateParameters[name] = struct{}{}
	}
	for name := range runtimePrebuiltParameterNames {
		templateParameters[name] = struct{}{}
	}
	if err := validatePrebuiltURLTemplate(entry.ServerURL, templateParameters); err != nil {
		return err
	}
	for name, value := range entry.Headers {
		if strings.Contains(name, "{") || strings.Contains(name, "}") {
			return invalidPrebuiltParameter("header names cannot contain placeholders")
		}
		if err := validatePrebuiltTemplate(value, templateParameters); err != nil {
			return err
		}
	}
	return nil
}

// PrebuiltConfigProperties returns a bounded copy of config_schema.properties.
func PrebuiltConfigProperties(schema map[string]any) (map[string]any, error) {
	if len(schema) == 0 {
		return map[string]any{}, nil
	}
	encoded, err := json.Marshal(schema)
	if err != nil || len(encoded) > maxPrebuiltSchemaBytes {
		return nil, invalidPrebuiltParameter("configuration schema is too large")
	}
	var cloned map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber()
	if err := decoder.Decode(&cloned); err != nil {
		return nil, invalidPrebuiltParameter("configuration schema is invalid")
	}
	raw, present := cloned["properties"]
	if !present || raw == nil {
		return map[string]any{}, nil
	}
	properties, ok := raw.(map[string]any)
	if !ok || len(properties) > maxPrebuiltParameters {
		return nil, invalidPrebuiltParameter("configuration properties are invalid")
	}
	return properties, nil
}

// PrebuiltParameterNames returns the fields that claim materialization may redeem.
func PrebuiltParameterNames(entry PrebuiltServer) ([]string, error) {
	properties, err := PrebuiltConfigProperties(entry.ConfigSchema)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// MaterializePrebuiltTemplates applies validated project parameters to trusted templates.
func MaterializePrebuiltTemplates(
	entry PrebuiltServer,
	settings map[string]any,
) (string, map[string]string, error) {
	if err := ValidatePrebuiltServer(entry); err != nil {
		return "", nil, err
	}
	properties, _ := PrebuiltConfigProperties(entry.ConfigSchema)
	values := make(map[string]string, len(properties))
	for name, raw := range properties {
		property := raw.(map[string]any)
		value, present := settings[name]
		if (!present || value == nil || value == "") && property["default"] != nil {
			value = property["default"]
			present = true
		}
		if !present || value == nil || value == "" {
			if required, _ := property["required"].(bool); required {
				return "", nil, invalidPrebuiltParameter("a required parameter is missing")
			}
			continue
		}
		formatted, err := formatPrebuiltParameter(value, propertyType(property))
		if err != nil {
			return "", nil, err
		}
		values[name] = formatted
	}
	for name := range runtimePrebuiltParameterNames {
		value, present := settings[name]
		if !present {
			continue
		}
		formatted, err := formatPrebuiltParameter(value, "string")
		if err != nil {
			return "", nil, err
		}
		values[name] = formatted
	}

	endpoint, err := substitutePrebuiltTemplate(entry.ServerURL, values, true)
	if err != nil {
		return "", nil, err
	}
	headers := make(map[string]string, len(entry.Headers))
	for name, template := range entry.Headers {
		value, err := substitutePrebuiltTemplate(template, values, false)
		if err != nil {
			return "", nil, err
		}
		headers[name] = value
	}
	return endpoint, headers, nil
}

func validatePrebuiltProperty(property map[string]any) error {
	typeName := propertyType(property)
	switch typeName {
	case "string", "integer", "number", "boolean", "array":
	default:
		return invalidPrebuiltParameter("parameter type is unsupported")
	}
	if secret, _ := property["secret"].(bool); secret && typeName != "string" {
		return invalidPrebuiltParameter("secret parameters must be strings")
	}
	if typeName == "array" {
		items, ok := property["items"].(map[string]any)
		if !ok {
			return invalidPrebuiltParameter("array parameters require item metadata")
		}
		itemType, _ := items["type"].(string)
		if itemType != "string" && itemType != "integer" && itemType != "number" && itemType != "boolean" {
			return invalidPrebuiltParameter("array parameter item type is unsupported")
		}
	}
	if value, present := property["default"]; present && value != nil {
		if _, err := formatPrebuiltParameter(value, typeName); err != nil {
			return invalidPrebuiltParameter("parameter default has the wrong type")
		}
	}
	return nil
}

func propertyType(property map[string]any) string {
	value, _ := property["type"].(string)
	return strings.ToLower(strings.TrimSpace(value))
}

func validatePrebuiltURLTemplate(template string, declared map[string]struct{}) error {
	if strings.TrimSpace(template) == "" {
		return nil
	}
	if err := validatePrebuiltTemplate(template, declared); err != nil {
		return err
	}
	authorityEnd := len(template)
	if scheme := strings.Index(template, "://"); scheme >= 0 {
		if slash := strings.Index(template[scheme+3:], "/"); slash >= 0 {
			authorityEnd = scheme + 3 + slash
		}
	}
	if prebuiltPlaceholder.MatchString(template[:authorityEnd]) {
		return invalidPrebuiltParameter("URL authority cannot contain placeholders")
	}
	probe := prebuiltPlaceholder.ReplaceAllString(template, "parameter")
	parsed, err := url.Parse(probe)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return invalidPrebuiltParameter("URL template must be an HTTPS endpoint without query or fragment")
	}
	return nil
}

func validatePrebuiltTemplate(template string, declared map[string]struct{}) error {
	if len(template) > maxPrebuiltStringBytes || strings.ContainsAny(template, "\x00\r\n") {
		return invalidPrebuiltParameter("template is invalid")
	}
	for _, match := range prebuiltPlaceholder.FindAllStringSubmatch(template, -1) {
		if _, ok := declared[match[1]]; !ok {
			return invalidPrebuiltParameter("template references an undeclared parameter")
		}
	}
	withoutPlaceholders := prebuiltPlaceholder.ReplaceAllString(template, "")
	if strings.ContainsAny(withoutPlaceholders, "{}") {
		return invalidPrebuiltParameter("template contains a malformed placeholder")
	}
	return nil
}

func substitutePrebuiltTemplate(
	template string,
	values map[string]string,
	escapePath bool,
) (string, error) {
	missing := false
	result := prebuiltPlaceholder.ReplaceAllStringFunc(template, func(token string) string {
		name := token[1 : len(token)-1]
		value, ok := values[name]
		if !ok {
			missing = true
			return token
		}
		if escapePath {
			return url.PathEscape(value)
		}
		return value
	})
	if missing || strings.ContainsAny(result, "\x00\r\n") {
		return "", invalidPrebuiltParameter("template parameter is missing or invalid")
	}
	return result, nil
}

func formatPrebuiltParameter(value any, typeName string) (string, error) {
	switch typeName {
	case "string":
		text, ok := value.(string)
		if !ok || len(text) > maxPrebuiltStringBytes || strings.ContainsAny(text, "\x00\r\n") {
			return "", invalidPrebuiltParameter("string parameter is invalid")
		}
		return text, nil
	case "integer":
		switch typed := value.(type) {
		case json.Number:
			if _, err := typed.Int64(); err == nil {
				return typed.String(), nil
			}
		case float64:
			if !math.IsNaN(typed) && !math.IsInf(typed, 0) &&
				typed >= math.MinInt64 && typed <= math.MaxInt64 && math.Trunc(typed) == typed {
				return strconv.FormatInt(int64(typed), 10), nil
			}
		case int:
			return strconv.Itoa(typed), nil
		case int64:
			return strconv.FormatInt(typed, 10), nil
		}
	case "number":
		switch typed := value.(type) {
		case json.Number:
			if parsed, err := typed.Float64(); err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0) {
				return typed.String(), nil
			}
		case float64:
			if !math.IsNaN(typed) && !math.IsInf(typed, 0) {
				return strconv.FormatFloat(typed, 'g', -1, 64), nil
			}
		case int:
			return strconv.Itoa(typed), nil
		case int64:
			return strconv.FormatInt(typed, 10), nil
		}
	case "boolean":
		if typed, ok := value.(bool); ok {
			return strconv.FormatBool(typed), nil
		}
	case "array":
		if _, ok := value.([]any); ok {
			return "", invalidPrebuiltParameter("array parameters cannot be used in templates")
		}
	}
	return "", invalidPrebuiltParameter(fmt.Sprintf("parameter does not match type %s", typeName))
}

func invalidPrebuiltParameter(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidPrebuiltParameters, reason)
}
