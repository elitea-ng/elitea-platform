package delegationgrant

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Tool is one toolkit reference of a callee version: a row of elitea_tools
// bound to the version, in the shape the worker's frozen tool list carries
// (libs/rust/agent-runtime/src/delegation.rs GrantSet::from_tools reads
// `id`, `type`, `settings`).
type Tool struct {
	// ID is the toolkit row id; 0 for a built-in toolkit with no row.
	ID       int64
	Type     string
	Settings map[string]any
}

// Walk bounds, the worker's.
const (
	maxHostWalkDepth = 16
	maxCredentialLen = 64
)

// FromTools is the grant set a callee holds, read from its toolkit references
// as the worker reads them: the toolkits (not nested applications, which are
// edges of their own), the tool selection on each, the credential reference
// each names, the hosts the toolkits are configured to reach, and the data
// classes the callee declares (nil when it declares none). It mirrors
// GrantSet::from_tools and declared_hosts in the worker; both run the
// `tools_cases` of libs/proto/contracts/delegation-grant-digest-vectors-v1.json.
func FromTools(tools []Tool, dataClasses []string) GrantSet {
	set := GrantSet{DataClasses: dataClasses}
	hosts := map[string]bool{}
	for i, tool := range tools {
		if i >= MaxToolkits {
			break
		}
		if tool.Type == "" || tool.Type == "application" {
			continue
		}
		key := "builtin:" + tool.Type
		if tool.ID > 0 {
			key = "toolkit:" + strconv.FormatInt(tool.ID, 10)
		}
		set.Toolkits = append(set.Toolkits, ToolkitGrant{
			Key:        key,
			Tools:      selection(tool.Settings),
			Credential: credentialReference(tool.Settings),
		})
		if tool.Type != "openapi" && len(hosts) < MaxHosts {
			walkHosts(tool.Settings, 0, hosts)
		}
	}
	for host := range hosts {
		set.EgressHosts = append(set.EgressHosts, host)
	}
	sort.Strings(set.EgressHosts)
	if len(set.EgressHosts) > MaxHosts {
		set.EgressHosts = set.EgressHosts[:MaxHosts]
	}
	return set.Normalize()
}

// selection is the tool selection of a toolkit: `selected_tools` when it
// names any, else every tool but `excluded_tools`.
func selection(settings map[string]any) Tools {
	if selected := names(settings, "selected_tools"); len(selected) > 0 {
		return Tools{Mode: ToolsOnly, Names: selected}
	}
	return Tools{Mode: ToolsAllExcept, Names: names(settings, "excluded_tools")}
}

func names(settings map[string]any, field string) []string {
	list, _ := settings[field].([]any)
	var out []string
	for i, item := range list {
		if i >= MaxToolNames {
			break
		}
		if text, ok := item.(string); ok {
			if text = strings.TrimSpace(text); text != "" {
				out = append(out, text)
			}
		}
	}
	return out
}

// credentialReference is the toolkit's credential reference id, as the
// governed toolkit client reads it: `credential_reference_id` at the top
// level, else the first `*_configuration` object (by key order) that has it;
// kept only when it is at most 64 characters of letters, digits and '-'.
func credentialReference(settings map[string]any) string {
	const key = "credential_reference_id"
	reference, found := settings[key].(string)
	if !found {
		keys := make([]string, 0, len(settings))
		for name := range settings {
			if strings.HasSuffix(name, "_configuration") {
				keys = append(keys, name)
			}
		}
		sort.Strings(keys)
		for _, name := range keys {
			if object, ok := settings[name].(map[string]any); ok {
				if value, ok := object[key].(string); ok {
					reference, found = value, true
					break
				}
			}
		}
	}
	if !found || len(reference) > maxCredentialLen {
		return ""
	}
	for i := 0; i < len(reference); i++ {
		c := reference[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && c != '-' {
			return ""
		}
	}
	return reference
}

func walkHosts(value any, depth int, hosts map[string]bool) {
	if depth > maxHostWalkDepth || len(hosts) >= MaxHosts {
		return
	}
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			switch c := child.(type) {
			case string:
				if urlField(key) {
					if host, ok := explicitURLHost(c); ok {
						hosts[host] = true
					}
				}
			default:
				walkHosts(child, depth+1, hosts)
			}
		}
	case []any:
		for _, item := range v {
			walkHosts(item, depth+1, hosts)
		}
	}
}

func urlField(key string) bool {
	key = strings.ToLower(key)
	switch key {
	case "url", "endpoint", "host", "api_base", "server":
		return true
	}
	return strings.HasSuffix(key, "_url") || strings.HasSuffix(key, "_endpoint") || strings.HasSuffix(key, "_host")
}

// explicitURLHost is the lowercase host of an explicit http, https, ws or wss
// URL; a value without a scheme, a template, or another scheme is not read.
func explicitURLHost(text string) (string, bool) {
	text = strings.TrimSpace(text)
	if !strings.Contains(text, "://") || len(text) > 2048 || strings.ContainsAny(text, "{}$%") {
		return "", false
	}
	parsed, err := url.Parse(text)
	if err != nil {
		return "", false
	}
	switch parsed.Scheme {
	case "http", "https", "ws", "wss":
	default:
		return "", false
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", false
	}
	if strings.Contains(host, ":") { // an IPv6 literal keeps its brackets
		host = "[" + host + "]"
	}
	return host, true
}
