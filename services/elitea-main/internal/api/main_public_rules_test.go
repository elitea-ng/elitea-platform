package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
	forwardapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/forwardauth"
)

type unusedPublicRuleCredentials struct{}

func (unusedPublicRuleCredentials) AuthenticateCredential(
	context.Context,
	forwardapp.Source,
	forwardapp.CredentialInput,
) (forwardapp.CredentialResult, error) {
	panic("public-rule test unexpectedly authenticated a credential")
}

type unusedPublicRuleSessions struct{}

func (unusedPublicRuleSessions) Authorize(context.Context, string) (browserapp.Authorization, error) {
	panic("public-rule test unexpectedly authorized a browser session")
}

func TestCurrentMainRoutePublicRulesMatchPinnedCatalog(t *testing.T) {
	rules := CurrentMainRoutePublicRules()
	want := []struct {
		name    string
		pattern string
		matches []string
		near    []string
	}{
		{"current.admin_ui.assets", `^/admin/app/.*\.(js|css|ico|png|jpg|jpeg|gif|svg|woff|woff2|ttf|eot|map)$`, []string{"/admin/app/assets/main.js"}, []string{"/admin/app/config.json", "https://evil.example/admin/app/assets/main.js"}},
		{"current.elitea_core.robots", `^/robots\.txt$`, []string{"/robots.txt"}, []string{"/robots.txt/extra", "https://evil.example/robots.txt"}},
		{"current.elitea_core.favicon", `^/favicon\.ico$`, []string{"/favicon.ico"}, []string{"/faviconXico", "https://evil.example/favicon.ico"}},
		{"current.elitea_core.access_denied", `^/app/access_denied$`, []string{"/app/access_denied"}, []string{"/app/access_denied/extra", "https://evil.example/app/access_denied"}},
		{"current.elitea_core.webhook", `^/api/v2/elitea_core/webhook/prompt_lib/[0-9]+/[0-9]+/(github|gitlab|custom)$`, []string{"/api/v2/elitea_core/webhook/prompt_lib/7/9/github"}, []string{"/api/v2/elitea_core/webhook/prompt_lib/7/x/github", "https://evil.example/api/v2/elitea_core/webhook/prompt_lib/7/9/github"}},
		{"current.elitea_core.public_messages", `^/elitea_core/[0-9]+/messages\?session_id=.+$`, []string{"/elitea_core/7/messages?session_id=abc"}, []string{"/elitea_core/7/messages", "https://evil.example/elitea_core/7/messages?session_id=abc"}},
		{"current.runtime_interface_litellm", `^/llm/.*$`, []string{"/llm/v1/chat/completions"}, []string{"/api/llm/v1/chat/completions", "https://evil.example/llm/v1/chat/completions"}},
		// go.* rules: routes router.go registers outside every auth group.
		// A browser sub-resource carries no credential, so the edge policy
		// must allow it too. Without these rows an unauthenticated browser
		// receives a 302 to the login form in place of the asset.
		{"go.eliteacore.application_icon", `^/app/application_icon/[^?]+(\?.*)?$`, []string{"/app/application_icon/logo.png", "/app/application_icon/logo.png?v=2"}, []string{"/app/application_icon/", "https://evil.example/app/application_icon/logo.png"}},
		{"go.eliteacore.application_tool_icon", `^/app/application_tool_icon/[^?]+(\?.*)?$`, []string{"/app/application_tool_icon/github.svg"}, []string{"/app/application_tool_icon/", "https://evil.example/app/application_tool_icon/github.svg"}},
		{"go.eliteacore.icon_download", `^/icons/[0-9]+/[^/?]+(\?.*)?$`, []string{"/icons/7/logo.png", "/icons/7/logo.png?v=2"}, []string{"/icons/seven/logo.png", "/icons/7/nested/logo.png", "https://evil.example/icons/7/logo.png"}},
		{"go.social.avatar_download", `^/avatars/[0-9]+/[^/?]+(\?.*)?$`, []string{"/avatars/7/face.png"}, []string{"/avatars//face.png", "/avatars/7/nested/face.png", "https://evil.example/avatars/7/face.png"}},
		{"go.branding.bootstrap", `^/api/v2/branding/bootstrap\.js(\?.*)?$`, []string{"/api/v2/branding/bootstrap.js", "/api/v2/branding/bootstrap.js?v=abc"}, []string{"/api/v2/branding/bootstrap.jsx", "https://evil.example/api/v2/branding/bootstrap.js"}},
		{"go.branding.assets", `^/api/v2/branding/assets/[a-z-]+/[0-9a-f]{64}\.[a-z0-9]{1,8}$`, []string{"/api/v2/branding/assets/logo-full/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.svg", "/api/v2/branding/assets/font/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.woff2"}, []string{"/api/v2/branding/assets/logo-full/logo.svg", "/api/v2/branding/assets/logo-full/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.svg?v=1", "/api/v2/branding/assets/logo-full/../0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.svg", "https://evil.example/api/v2/branding/assets/logo-full/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.svg"}},
		{"go.health.healthz", `^/healthz(\?.*)?$`, []string{"/healthz", "/healthz?t=1725600000"}, []string{"/healthz/", "/healthzz", "/readyz", "/startupz", "https://evil.example/healthz"}},
		{"go.openapidocs.spec_yaml", `^/api/openapi\.yaml$`, []string{"/api/openapi.yaml"}, []string{"/api/openapi.yaml/extra", "https://evil.example/api/openapi.yaml"}},
		{"go.openapidocs.spec_json", `^/api/openapi\.json$`, []string{"/api/openapi.json"}, []string{"/api/openapi.jsonx", "https://evil.example/api/openapi.json"}},
		{"go.openapidocs.ui", `^/docs$`, []string{"/docs"}, []string{"/docs/extra", "https://evil.example/docs"}},
	}
	if len(rules) != len(want) {
		t.Fatalf("route-owned public rule count = %d, want %d", len(rules), len(want))
	}

	policy, err := forwardapp.NewPublicPolicy(rules)
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := forwardapp.NewKernel(unusedPublicRuleCredentials{}, unusedPublicRuleSessions{}, policy)
	if err != nil {
		t.Fatal(err)
	}
	for index, expected := range want {
		rule := rules[index]
		if rule.Name != expected.name || len(rule.Conditions) != 1 ||
			rule.Conditions[0].Field != forwardapp.SourceURI ||
			rule.Conditions[0].Pattern != expected.pattern || rule.MatchAll {
			t.Fatalf("rule %d = %+v, want name=%q pattern=%q", index, rule, expected.name, expected.pattern)
		}
		for _, match := range expected.matches {
			if decision := authorizePublicURI(t, kernel, match); decision.Kind != forwardapp.DecisionAllow ||
				decision.Authentication.Type != forwardapp.AuthenticationPublic ||
				decision.PublicMatch.RuleName != expected.name {
				t.Fatalf("positive %q decision = %+v", match, decision)
			}
		}
		for _, near := range expected.near {
			if decision := authorizePublicURI(t, kernel, near); decision.Kind != forwardapp.DecisionLogin {
				t.Fatalf("near-match %q decision = %+v, want login", near, decision)
			}
		}
	}
}

func TestCurrentMainRoutePublicRulesReturnsDetachedCatalog(t *testing.T) {
	rules := CurrentMainRoutePublicRules()
	rules[0].Name = "mutated"
	rules[0].Conditions[0].Pattern = ".*"
	again := CurrentMainRoutePublicRules()
	if again[0].Name != "current.admin_ui.assets" || again[0].Conditions[0].Pattern != `^/admin/app/.*\.(js|css|ico|png|jpg|jpeg|gif|svg|woff|woff2|ttf|eot|map)$` {
		t.Fatalf("catalog aliased caller mutation: %+v", again[0])
	}
}

func authorizePublicURI(t *testing.T, kernel *forwardapp.Kernel, uri string) forwardapp.Decision {
	t.Helper()
	decision, err := kernel.Authorize(context.Background(), forwardapp.Request{
		Source: forwardapp.Source{
			Method: "GET",
			Proto:  "https",
			Host:   "elitea.example",
			URI:    uri,
			IP:     "203.0.113.10",
		},
		Traversal: forwardapp.MainTraversal,
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

// TestSocketIOIsNoLongerAuthExempt is the negative pin for the rule this
// catalogue used to carry.
//
// `current.elitea_core.socket_io` (`^/socket\.io/.*$`) exempted the whole
// prefix from authentication for a handler this service never mounted — the
// Socket.IO prototype server was deleted with #126. Nothing answered there, so
// nothing was exposed; what was exposed was the STANDING STATE, in which the
// first person to mount a handler on that path inherited an anonymous entry
// point without touching this file. The rule is gone, and so are the
// `/socket.io/` forwards at both browser edges (deploy/traefik/dynamic.yml,
// deploy/traefik/dynamic.e2e.yml, deploy/gateway-api/httproute.yaml).
//
// TestCurrentMainRoutePublicRulesMatchPinnedCatalog would already fail if the
// rule came back by that name. This test fails for ANY rule, under any name,
// that makes a cookie-less /socket.io/ request public again.
func TestSocketIOIsNoLongerAuthExempt(t *testing.T) {
	policy, err := forwardapp.NewPublicPolicy(CurrentMainRoutePublicRules())
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := forwardapp.NewKernel(unusedPublicRuleCredentials{}, unusedPublicRuleSessions{}, policy)
	if err != nil {
		t.Fatal(err)
	}

	for _, uri := range []string{"/socket.io/", "/socket.io/connect", "/socket.io/?EIO=4&transport=polling"} {
		t.Run(uri, func(t *testing.T) {
			decision := authorizePublicURI(t, kernel, uri)
			if decision.Kind == forwardapp.DecisionAllow &&
				decision.Authentication.Type == forwardapp.AuthenticationPublic {
				t.Fatalf("cookie-less %q is public again: %+v", uri, decision)
			}
		})
	}
}

// TestRouterPublicRoutesStayPublicInThePolicy pins the pairing between the
// router and the forward-auth policy.
//
// Defect: router.go registers a small set of routes outside every
// authentication group, because a browser sub-resource carries no credential.
// The forward-auth edge asks CurrentMainRoutePublicRules() BEFORE the request
// reaches the router. A route that is public in one layer and absent from the
// other is answered with a 302 to the login form. The branding bootstrap
// script hit exactly that: index.html loads it with a blocking <script src>,
// and an unauthenticated browser parsed the login page as JavaScript, so
// window.elitea_brand was never set.
func TestRouterPublicRoutesStayPublicInThePolicy(t *testing.T) {
	pool := &pgxpool.Pool{}
	router := NewRouter(RouterConfig{Pool: pool})
	registered := make(map[string]struct{})
	if err := chi.Walk(router, func(_, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		registered[route] = struct{}{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	policy, err := forwardapp.NewPublicPolicy(CurrentMainRoutePublicRules())
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := forwardapp.NewKernel(unusedPublicRuleCredentials{}, unusedPublicRuleSessions{}, policy)
	if err != nil {
		t.Fatal(err)
	}

	for _, public := range []struct {
		route string
		uri   string
	}{
		{"/app/application_icon/*", "/app/application_icon/logo.png"},
		{"/app/application_tool_icon/*", "/app/application_tool_icon/github.svg"},
		{"/icons/{projectID}/{filename}", "/icons/7/logo.png"},
		{"/avatars/{projectID}/{filename}", "/avatars/7/face.png"},
		{"/api/v2/branding/bootstrap.js", "/api/v2/branding/bootstrap.js"},
		{"/healthz", "/healthz"},
		{"/api/openapi.yaml", "/api/openapi.yaml"},
		{"/api/openapi.json", "/api/openapi.json"},
		{"/docs", "/docs"},
	} {
		t.Run(public.route, func(t *testing.T) {
			if _, ok := registered[public.route]; !ok {
				t.Fatalf("router no longer registers %q; remove or repoint its public rule", public.route)
			}
			decision := authorizePublicURI(t, kernel, public.uri)
			if decision.Kind != forwardapp.DecisionAllow ||
				decision.Authentication.Type != forwardapp.AuthenticationPublic {
				t.Fatalf("cookie-less %q decision = %+v, want a public allow", public.uri, decision)
			}
		})
	}
}
