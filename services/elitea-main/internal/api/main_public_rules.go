package api

import forwardapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/forwardauth"

// CurrentMainRoutePublicRules is the compatibility catalog of every Main-plane
// route that answers without a browser session. It holds two groups.
//
// The `current.*` rules are the source-evidenced inventory registered
// dynamically by the currently deployed Main plugins. Evidence:
// testdata/baseline/main-auth-http-contracts.json
// public_rule_inventory.main_local_plane.dynamic_registration_sites.
// Each future Go route owner should take over its entry when that route moves.
//
// The `go.*` rules belong to routes that router.go itself registers outside
// every authentication group. A forward-auth edge asks this policy before the
// request reaches the router. A route that is public in router.go and absent
// here gets a 302 to the login form. A browser sub-resource
// (a <script src>, an <img src>) carries no credential, so it must stay public
// on both layers.
//
// The four operator-configured auth.yml rules remain in the versioned Form
// config.
//
// DELIBERATELY ABSENT: `current.elitea_core.socket_io` (`^/socket\.io/.*$`).
// It was an auth-EXEMPT rule for a path this service never served. The
// Socket.IO prototype server was deleted with #126 (see the NOTE in
// cmd/elitea-main/main.go), so the standing state was an anonymous entry
// point waiting for whoever mounted a handler there. The rule and the
// matching `/socket.io/` forwards at both browser edges
// (deploy/traefik/dynamic.yml, deploy/traefik/dynamic.e2e.yml,
// deploy/gateway-api/httproute.yaml) are gone together. A future socket
// surface needs its own AUTHENTICATED rule, not this one.
func CurrentMainRoutePublicRules() []forwardapp.PublicRule {
	return []forwardapp.PublicRule{
		uriRule("current.admin_ui.assets", `^/admin/app/.*\.(js|css|ico|png|jpg|jpeg|gif|svg|woff|woff2|ttf|eot|map)$`),
		uriRule("current.elitea_core.robots", `^/robots\.txt$`),
		uriRule("current.elitea_core.favicon", `^/favicon\.ico$`),
		uriRule("current.elitea_core.access_denied", `^/app/access_denied$`),
		uriRule("current.elitea_core.webhook", `^/api/v2/elitea_core/webhook/prompt_lib/[0-9]+/[0-9]+/(github|gitlab|custom)$`),
		uriRule("current.elitea_core.public_messages", `^/elitea_core/[0-9]+/messages\?session_id=.+$`),
		uriRule("current.runtime_interface_litellm", `^/llm/.*$`),
		// router.go serves the two icon directories with a file server. A
		// browser <img src> carries no credential. The query alternative
		// covers a cache-busting parameter: forwarded.URI carries the query,
		// and the policy anchors the whole string.
		uriRule("go.eliteacore.application_icon", `^/app/application_icon/[^?]+(\?.*)?$`),
		uriRule("go.eliteacore.application_tool_icon", `^/app/application_tool_icon/[^?]+(\?.*)?$`),
		// Object-store icon and avatar downloads. Same <img src> reason.
		uriRule("go.eliteacore.icon_download", `^/icons/[0-9]+/[^/?]+(\?.*)?$`),
		uriRule("go.social.avatar_download", `^/avatars/[0-9]+/[^/?]+(\?.*)?$`),
		// The UI loads branding before a browser session exists. The query
		// alternative covers the content-addressed ?v=<etag> URL that
		// branding.Handler hands out for the immutable cache entry.
		uriRule("go.branding.bootstrap", `^/api/v2/branding/bootstrap\.js(\?.*)?$`),
		// Uploaded brand assets: <img src>, <link rel="icon"> and @font-face
		// fetches carry no credential. The path is content-addressed
		// (kind/<sha256>.<ext>) and the handler admits nothing else.
		uriRule("go.branding.assets", `^/api/v2/branding/assets/[a-z-]+/[0-9a-f]{64}\.[a-z0-9]{1,8}$`),
		// The liveness endpoint (#569). router.go mounts health.RoutesWithDeps
		// at `/`, outside every auth group, and every browser edge forwards
		// `/healthz` to elitea-main. The cluster edge consults this policy
		// before it forwards, so without this row a probe on the public
		// hostname got a 302 to the login form. The body is the minimal
		// {"status":"ok"} and names no dependency. /readyz and /startupz stay
		// absent on purpose: no edge forwards them, and their bodies do name
		// each dependency's state. The query alternative covers a monitor's
		// cache-busting parameter.
		uriRule("go.health.healthz", `^/healthz(\?.*)?$`),
		// API documentation predates any session.
		uriRule("go.openapidocs.spec_yaml", `^/api/openapi\.yaml$`),
		uriRule("go.openapidocs.spec_json", `^/api/openapi\.json$`),
		uriRule("go.openapidocs.ui", `^/api/docs$`),
	}
}

func uriRule(name, pattern string) forwardapp.PublicRule {
	return forwardapp.PublicRule{
		Name: name,
		Conditions: []forwardapp.RuleCondition{{
			Field:   forwardapp.SourceURI,
			Pattern: pattern,
		}},
	}
}
