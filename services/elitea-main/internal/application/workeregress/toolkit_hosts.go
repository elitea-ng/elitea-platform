package workeregress

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftauth"
)

// toolkitImplicitHosts are the hosts a toolkit type's worker client calls by
// default from a COMPILED-IN endpoint, never named in its settings, so
// HostsFromSettings cannot find them. A run that uses the type gets them.
// Keyed by the worker's toolkit type (the `type` of a toolkit entry, the
// tool_type the Rust worker dispatches on in
// libs/rust/agent-runtime/src/toolkits/materialize.rs). Each value is pinned
// against its Rust constant by TestImplicitHostsMatchTheRustClients:
//
//   - azure: families/azure/client.rs ARM_ORIGIN, TOKEN_ORIGIN
//   - bigquery: families/bigquery/client.rs API_ORIGIN, and the service
//     account token of families/gcp/client.rs TOKEN_URL
//   - gcp: families/gcp/client.rs TOKEN_URL; its tools call any https URL
//     under googleapis.com the call names (the client refuses every other
//     host), hence `googleapis.com` and `*.googleapis.com`
//   - figma: families/figma/client.rs API_ROOT
//   - google_places: families/google_places/client.rs GOOGLE_MAPS_ORIGIN,
//     GOOGLE_PLACES_ORIGIN
//   - sharepoint: families/sharepoint/client.rs GRAPH_BASE (the token is a
//     delegated one Main redeems; the worker calls no token endpoint)
//   - outlook, teams, ms_graph: families/microsoft_graph_common.rs
//     GRAPH_ORIGIN (ADR-0057 decision 10; likewise a delegated token, and
//     `login.microsoftonline.com` is a Main-only host, not a worker one)
//   - dataverse: no compiled-in host. Its org host comes from the
//     credential's `url` (HostsFromSettings), pinned to https://<org>.dynamics.com
//     by microsoftauth.NormalizeDataverseURL; a sovereign cloud or any other
//     host refuses the command (microsoftFieldHosts)
//   - slack: families/slack/client.rs SLACK_API_ORIGIN
//   - xray_cloud: families/xray_cloud/config.rs DEFAULT_BASE_URL (used when
//     the settings name none)
//   - zephyr_essential: families/zephyr_essential/config.rs DEFAULT_BASE_URL
//     (likewise)
//   - zephyr_scale: families/zephyr_scale/config.rs CLOUD_API
//   - zephyr_squad: families/zephyr_squad/client.rs API_ROOT
//   - web: the search and fetch providers of families/web/registry.rs (the
//     ORIGINS of adapters/tavily.rs, firecrawl.rs, brave.rs, ddg_instant.rs);
//     which ones a run may use is resolved at claim time, after this policy
//   - host_intel: families/web/adapters/shodan.rs ORIGINS
//   - tavily: families/web/adapters/tavily.rs ORIGINS (the toolkit is the web
//     family pinned to Tavily, #2261)
//   - delta_lake: no compiled-in host. The one host is the S3 endpoint of the
//     credential's region (families/delta_lake/config.rs s3_endpoint), derived
//     by deltaLakeHosts when the credential names a valid region and an
//     s3:// table path (#2262)
//
// Constants that are never called (a link written into a result, such as
// carrier's REPORT_LINK_PREFIX, or an OAuth scope) are not hosts here.
var toolkitImplicitHosts = map[string][]string{
	"azure":            {"management.azure.com", "login.microsoftonline.com"},
	"bigquery":         {"bigquery.googleapis.com", "oauth2.googleapis.com"},
	"gcp":              {"oauth2.googleapis.com", "googleapis.com", "*.googleapis.com"},
	"figma":            {"api.figma.com"},
	"google_places":    {"maps.googleapis.com", "places.googleapis.com"},
	"sharepoint":       {"graph.microsoft.com"},
	"outlook":          {"graph.microsoft.com"},
	"teams":            {"graph.microsoft.com"},
	"ms_graph":         {"graph.microsoft.com"},
	"slack":            {"slack.com"},
	"xray_cloud":       {"xray.cloud.getxray.app"},
	"zephyr_essential": {"prod-api.zephyr4jiracloud.com"},
	"zephyr_scale":     {"api.zephyrscale.smartbear.com"},
	"zephyr_squad":     {"prod-api.zephyr4jiracloud.com"},
	"web":              {"api.tavily.com", "api.firecrawl.dev", "api.search.brave.com", "api.duckduckgo.com"},
	"host_intel":       {"api.shodan.io"},
	"tavily":           {"api.tavily.com"},
}

// ImplicitToolkitHosts answers the compiled-in hosts of a toolkit type.
func ImplicitToolkitHosts(toolkitType string) []string {
	return append([]string(nil), toolkitImplicitHosts[strings.TrimSpace(toolkitType)]...)
}

// HostsForToolkit is HostsFromSettings for a toolkit of a known type: the
// URL-valued settings, what the type derives from a settings value that is
// not a URL (a ServiceNow instance name), and the type's compiled-in hosts.
func HostsForToolkit(toolkitType string, content []byte) ([]string, error) {
	hosts, err := hostsFromSettingsOfType(toolkitType, content)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(toolkitType) == openAPIToolkitType {
		hosts = append(hosts, openAPIHosts(content)...)
	}
	if strings.TrimSpace(toolkitType) == deltaLakeToolkitType {
		hosts = append(hosts, deltaLakeHosts(content)...)
	}
	return append(hosts, ImplicitToolkitHosts(toolkitType)...), nil
}

// openAPIToolkitType is the worker's OpenAPI toolkit type (openapi_hosts.go).
const openAPIToolkitType = "openapi"

// githubAPIHost and githubWebHost are the API and the web/git origin of
// github.com. A GitHub toolkit's `base_url` names the API, but a workspace
// `git_push` goes to the forge's web origin, which the worker derives from
// it (libs/rust/agent-runtime/src/toolkits/workspace_pull_request.rs
// `github_web_origin`): `api.github.com` is `github.com`, and GitHub
// Enterprise's `https://host/api/v3` is `host` (the base URL's own host,
// already admitted). GitLab's web origin is its base URL's host too.
const (
	githubAPIHost = "api.github.com"
	githubWebHost = "github.com"
)

// derivedHosts answers the hosts a type's worker reaches from a URL field
// besides the field's own host.
func derivedHosts(toolkitType, key, host string) []string {
	if strings.TrimSpace(toolkitType) == "github" && strings.EqualFold(key, "base_url") && host == githubAPIHost {
		return []string{githubWebHost}
	}
	return nil
}

// HostsFromTypedToolkit reads a toolkit document that names its own type
// (`{"type": ..., "settings": ...}`, an index ingest's toolkit
// configuration) with HostsForToolkit.
func HostsFromTypedToolkit(content []byte) ([]string, error) {
	var typed struct {
		Type string `json:"type"`
	}
	if trimmed := bytes.TrimSpace(content); len(trimmed) != 0 && trimmed[0] == '{' {
		if err := json.Unmarshal(trimmed, &typed); err != nil {
			return nil, errUnreadableSettings
		}
	}
	return HostsForToolkit(typed.Type, content)
}

// serviceNowInstanceDomain completes a ServiceNow `base_url` given as a bare
// instance name, as the worker does (families/service_now/config.rs:
// `https://{value}.service-now.com`).
const serviceNowInstanceDomain = ".service-now.com"

// bareLabelHost answers the host the type derives from a URL field's value
// that is a bare name, or false.
func bareLabelHost(toolkitType, key, value string) (string, bool) {
	if toolkitType != "service_now" || !strings.EqualFold(key, "base_url") {
		return "", false
	}
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 63 || strings.Contains(value, "://") || strings.HasPrefix(value, "-") || strings.HasSuffix(value, "-") {
		return "", false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return "", false
		}
	}
	return hostOf(value + serviceNowInstanceDomain)
}

// microsoftFieldHosts applies ADR-0057 decision 10 to one settings field of a
// delegated Microsoft toolkit (outlook, teams, ms_graph, dataverse). It
// reports whether the field names no worker host, and a refusal when the
// field is a Dataverse environment URL that is not an
// https://<org>.<region>.dynamics.com origin (sovereign clouds included).
//
//   - The OAuth discovery endpoint is read by Main only (code exchange,
//     refresh, discovery), never by the worker, so it adds no host.
//   - A Dataverse `url` is the one host the worker calls for that type.
//
// SharePoint and every other type are untouched.
func microsoftFieldHosts(toolkitType, key, value string) (skip bool, err error) {
	descriptor, ok := microsoftauth.For(strings.TrimSpace(toolkitType))
	if !ok || descriptor.Type() == microsoftauth.SharePoint {
		return false, nil
	}
	switch strings.ToLower(key) {
	case microsoftauth.FieldDiscoveryEndpoint:
		return true, nil
	case microsoftauth.FieldURL:
		if descriptor.Type() != microsoftauth.Dataverse {
			return false, nil
		}
		if _, err := microsoftauth.NormalizeDataverseURL(value); err != nil {
			return true, refuse("the Dataverse toolkit's environment URL must look like https://<org>.crm.dynamics.com: a sovereign cloud or any other host is not supported")
		}
	}
	return false, nil
}
