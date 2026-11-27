package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api"
)

// composedRouterSource is the file of the administration surface helper that
// builds its router (adminsurface.ComposedRouter), relative to this package.
var composedRouterSource = filepath.Join("..", "..", "internal", "api", "adminsurface", "composed.go")

// Reasons shared by several fields of surfaceRouterUnsetFields.
const (
	reasonRuntimeRoute = "a reviewed runtime route (internal/api/production_router.go) that this command builds " +
		"with its own Auth and its own Postgres permission resolver, so the recording resolver cannot answer it; " +
		"it resolves the default mode only and makes no administration decision"
	reasonSectionCache = "the cache of one administration Configuration section, dropped after a save the route " +
		"gate admitted; nil skips the drop and changes no decision"
	reasonMCP = "the MCP server and its governed inbound and outbound planes: tool calls decided in the default " +
		"mode against a project, and no administration decision"
	reasonToolkits = "a toolkit catalogue, schema or run source of the project toolkit routes, read after their " +
		"default-mode gates; it needs the runtime (worker, vault) this router does not have"
	reasonProjectDeletion = "a step of project deletion, run after the delete route's gate admitted; Drive reads " +
		"that gate either way"
	reasonEvaluation = "an Agent Evaluation route group: project routes gated in the default mode " +
		"(router.go evaluationGate), with no administration decision"
	reasonWebhooks = "the project webhooks (deliveries, dispatcher, destination guard), gated in the default " +
		"mode against {projectID}"
	reasonModelCall = "an outbound model call (predict, image generation) made after the route admitted; it needs " +
		"elitea-llm-gateway"
	reasonConfigurationsRuntime = "the Configurations runtime's connection check, provider admission or stored " +
		"credential resolver, used behind the configurations route gates; it needs the project vault"
)

// surfaceRouterUnsetFields are the api.RouterConfig fields cmd/elitea-main
// sets to a handler or a store and adminsurface.ComposedRouter leaves unset,
// each with the reason that is safe (ADR-0059 #2985). A field set in
// production and nil in the composed router is a route group, or a check
// inside one, that the administration surface tests never drive: a wrong gate
// on it would pass. A new field cmd/elitea-main sets must be set in
// internal/api/adminsurface/composed.go, or listed here with a reason a
// reviewer can check against router.go.
var surfaceRouterUnsetFields = map[string]string{
	"AdminUI": "the admin SPA shell; its administration-mode resolution is a presentation hint injected into " +
		"the page (adminui resolvePermissions), never a decision, and every route the SPA calls resolves again",
	"AgentAssertionWellKnown": "the public well-known document of the native agent assertion issuer; " +
		"no permission decision",
	"AgentHeartbeatMandates": "the mandate check of the project heartbeat routes, which mount without it " +
		"behind their default-mode gates",
	"PostureInitialGlobalAdmins": "the configured bootstrap list one access posture check reads; the " +
		"posture route mounts without it behind the same access.roles.view gate, no permission decision",
	"AgentToolsCache":         reasonSectionCache,
	"ApprovalPolicyCache":     reasonSectionCache,
	"ContentControlsCache":    reasonSectionCache,
	"DelegationCache":         reasonSectionCache,
	"RunCapsCache":            reasonSectionCache,
	"SessionPolicyCache":      reasonSectionCache,
	"ApplicationData":         reasonProjectDeletion,
	"LegacyVectorDatabases":   reasonProjectDeletion,
	"MemoryData":              reasonProjectDeletion,
	"ProjectVectorData":       reasonProjectDeletion,
	"OnProjectProvisioner":    "a callback that starts the deletion reconciler with the provisioner; no route",
	"OnToolkitCleanupJournal": "a callback that starts the toolkit deletion reconciler; no route",
	"ProjectCleanupContext":   "the lifetime of the deletion reconcilers; no route",
	"PlatformModelDefaults":   "the default model a new project is seeded with, after project create admitted",
	"AgentImportModels": "the model catalogue an agent import pins each agent's model against, read after " +
		"the import route's default-mode gate admitted; nil keeps the file's settings and changes no decision",
	"AttachmentExtensions":       "the attachment extensions the discovery document lists; no route",
	"AttachmentExtractionWarmer": "the upload-time extraction warm-up of the conversation routes; no decision",
	"CodeSandboxLanguages":       "the sandbox languages the toolkit catalogue lists; no route",
	"ProductionAuth":             "the Form sign-in routes under /auth; no administration decision",
	"Branding": "the brand resolver; nil makes router.go compose the same resolver from the pool, " +
		"so the branding routes are the same",
	"BrandingPackages": "the brand package service behind the admin branding package routes, which are mounted " +
		"either way behind their route gates and answer 503 without it; no in-handler administration check",
	"CanvasPresence":       "the cross-replica canvas presence roster; the zero value keeps the in-process one",
	"ConversationActivity": "the read-frame notifier of the conversation routes; no decision",
	"CredentialReview": "the store of the credential review routes, mounted either way behind their central " +
		"gates; the handler makes no administration check of its own and answers 503 without it",
	"PermissionTrimming": "the store of the permission trimming routes, mounted either way behind their " +
		"central gates; the handler makes no administration check of its own and answers 503 without it",
	"ConfigConnectionChecker":       reasonConfigurationsRuntime,
	"ConfigProviderAdmission":       reasonConfigurationsRuntime,
	"ConfigStoredResolver":          reasonConfigurationsRuntime,
	"DelegatedAuthToolkitSettings":  reasonConfigurationsRuntime,
	"InternalConfigurationTools":    reasonConfigurationsRuntime,
	"CurrentAgentCancel":            reasonRuntimeRoute,
	"CurrentAgentStart":             reasonRuntimeRoute,
	"CurrentAgentSteering":          reasonRuntimeRoute,
	"CurrentApplicationSkills":      reasonRuntimeRoute,
	"CurrentApplicationTask":        reasonRuntimeRoute,
	"CurrentChildTurns":             reasonRuntimeRoute,
	"CurrentConfigurationAvailable": reasonRuntimeRoute,
	"CurrentConfigurationTypes":     reasonRuntimeRoute,
	"CurrentExecutionInterrupts":    reasonRuntimeRoute,
	"CurrentIndexCancel":            reasonRuntimeRoute,
	"CurrentIndexConfiguration":     reasonRuntimeRoute,
	"CurrentIndexMeta":              reasonRuntimeRoute,
	"CurrentIndexMetaDelete":        reasonRuntimeRoute,
	"CurrentIndexScheduleDelete":    reasonRuntimeRoute,
	"CurrentIndexScheduleUpdate":    reasonRuntimeRoute,
	"CurrentIndexStart":             reasonRuntimeRoute,
	"CurrentIndexTypes":             reasonRuntimeRoute,
	"CurrentLocalTurns":             reasonRuntimeRoute,
	"CurrentModelCatalog":           reasonRuntimeRoute,
	"CurrentModelDefault":           reasonRuntimeRoute,
	"CurrentNodeRecovery":           reasonRuntimeRoute,
	"CurrentNotificationEvents":     reasonRuntimeRoute,
	"CurrentNotificationPush":       reasonRuntimeRoute,
	"CurrentNotifications":          reasonRuntimeRoute,
	"CurrentProjectInfo":            reasonRuntimeRoute,
	"CurrentProjectList":            reasonRuntimeRoute,
	"CurrentPromptContextReads":     reasonRuntimeRoute,
	"CurrentRemoteToolkit":          reasonRuntimeRoute,
	"CurrentResolvedVersion":        reasonRuntimeRoute,
	"CurrentSocialAuthors":          reasonRuntimeRoute,
	"CurrentSocialAvatar":           reasonRuntimeRoute,
	"CurrentWorkspaceHandoff":       reasonRuntimeRoute,
	"BrowserSessionView":            reasonRuntimeRoute,
	"DeepWiki":                      reasonRuntimeRoute,
	"Inventory":                     reasonRuntimeRoute,
	"ProductionRuntime":             reasonRuntimeRoute,
	"CurrentNotificationStore":      "the notification store of the MCP notification tools; " + reasonMCP,
	"GovernedMCPInbound":            reasonMCP,
	"GovernedMCPOutbound":           reasonMCP,
	"RemoteAgentMCPInbound":         reasonMCP,
	"PlatformMCPTools":              reasonMCP,
	"MCPAgentStart":                 reasonMCP,
	"MCPToolkitExecute":             reasonMCP,
	"MCPToolkitRun":                 reasonMCP,
	"MCPAuthorizationEgressGuard": "the egress guard of the MCP OAuth discovery; it refuses a destination, " +
		"never a caller",
	"ToolkitArgumentSchemas":     reasonToolkits,
	"ToolkitCatalogue":           reasonToolkits,
	"ToolkitDiscovery":           reasonToolkits,
	"ToolkitSettingsDefinitions": reasonToolkits,
	"ToolkitSettingsValidator":   reasonToolkits,
	"ToolkitToolRun":             reasonToolkits,
	"ToolkitWorkerCapability":    reasonToolkits,
	"ToolkitRegistry": "the built-in toolkit types the Configuration suggestions list; the suggestions route " +
		"is decided by its route gate, which Drive reads, and answers 503 without the source",
	"DelegationGoverned": "whether the governed profile is on, read by the delegation grant store after the " +
		"route admitted; nil reads off",
	"DomainEvents":        "the domain event publisher; nil publishes nothing and decides nothing",
	"EmailSettings":       "the e-mail settings resolver; nil makes router.go compose the same one from the pool",
	"Mailer":              "outbound mail of invitations and moderation decisions, sent after the route admitted",
	"EvalDatasetsRepo":    reasonEvaluation,
	"EvalDimensionsRepo":  reasonEvaluation,
	"EvalHumanScoresRepo": reasonEvaluation,
	"EvalOrchestrator":    reasonEvaluation,
	"EvalRunsRepo":        reasonEvaluation,
	"EvalSuitesRepo":      reasonEvaluation,
	"ReleaseGate": "the evaluation release gate of agent lifecycle and the release gate routes, project routes " +
		"gated in the default mode",
	"EventSource":            "the project SSE stream's NATS transport, gated in the default mode",
	"GatewayProxy":           "the /llm reverse proxy to elitea-llm-gateway; it needs the gateway",
	"GatewayProjectResolver": "the personal project resolution of the /llm proxy; no administration decision",
	"HealthDeps":             "the readiness checks of the open health routes; no decision",
	"HTTPActionGuard":        "the egress guard of the project HTTP action rules; it refuses a destination, never a caller",
	"HTTPActionRules":        "the project HTTP action rules, gated in the default mode against {projectID}",
	"ImageGateway":           reasonModelCall,
	"PredictCompleter":       reasonModelCall,
	"SupportAssistantStart":  "the support assistant's agent run, started after its route admitted; it needs the runtime",
	"NativeAccess": "the native access token's client lookup for the client-version gate (426); " +
		"it reads a client id, never a permission",
	"NativePolicy": "the native client policy service; nil makes router.go compose the same one from the pool",
	"ObjectStore": "the object store of uploads, icons and brand assets; the routes that need it answer 503 " +
		"without it, after their gates",
	"PATSigner": "signs the token the mint route issues after its checks; nil signs with SessionSecret",
	"PipelineTriggers": "the inbound pipeline trigger and its project routes: the inbound one authenticates " +
		"by its own secret, the others are gated in the default mode",
	"RunTokens": "the run token validator of the runtime callbacks; a run token never resolves the " +
		"administration mode",
	"SCIMClientAddresses": "the client address resolver of the SCIM and native token throttles; " +
		"it decides a rate, never a permission",
	"SuspensionChanged": "a notification after a suspension changed; no route",
	"SuspensionService": "the kill switch service the Marketplace yank calls after the moderation check " +
		"admitted; nil yanks without suspending, and the check Drive reads is the same",
	"WebhookDeliveries":       reasonWebhooks,
	"WebhookDispatcher":       reasonWebhooks,
	"WebhookDestinationGuard": reasonWebhooks,
	"WebUpstream":             "the relay of /app and /docs to elitea-web; no API route",
}

// routeGroupField reports whether a RouterConfig field holds a handler or a
// store (a pointer, interface, func, map, slice, channel or struct value), not
// a scalar setting (a flag, a name, a duration). A scalar is configuration of a
// route a handler field mounts, so it is not a route group of its own.
func routeGroupField(name string) (bool, error) {
	field, ok := reflect.TypeFor[api.RouterConfig]().FieldByName(name)
	if !ok {
		return false, fmt.Errorf("api.RouterConfig has no field %s", name)
	}
	switch field.Type.Kind() {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return false, nil
	}
	return true, nil
}

// surfaceRouterFieldFailures answers one message per route-group field
// production sets that surface does not and unset does not list, per entry of
// unset with no reason, and per stale entry (the surface sets it, production
// does not, or it is a scalar).
func surfaceRouterFieldFailures(production, surface []string, unset map[string]string) []string {
	var failures []string
	for _, name := range production {
		group, err := routeGroupField(name)
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		if !group || slices.Contains(surface, name) {
			continue
		}
		if _, listed := unset[name]; !listed {
			failures = append(failures, fmt.Sprintf(
				"cmd/elitea-main sets RouterConfig.%s and adminsurface.ComposedRouter does not: the route group "+
					"or the check it composes is invisible to the administration surface tests. Set it in "+
					"internal/api/adminsurface/composed.go, or list it in surfaceRouterUnsetFields with the reason", name))
		}
	}
	for name, reason := range unset {
		group, err := routeGroupField(name)
		switch {
		case err != nil:
			failures = append(failures, "surfaceRouterUnsetFields: "+err.Error())
		case reason == "":
			failures = append(failures, fmt.Sprintf("surfaceRouterUnsetFields.%s has no reason", name))
		case !group:
			failures = append(failures, fmt.Sprintf(
				"surfaceRouterUnsetFields.%s is a scalar setting, not a route group; remove the entry", name))
		case slices.Contains(surface, name):
			failures = append(failures, fmt.Sprintf(
				"surfaceRouterUnsetFields.%s is set by adminsurface.ComposedRouter; remove the entry", name))
		case !slices.Contains(production, name):
			failures = append(failures, fmt.Sprintf(
				"surfaceRouterUnsetFields.%s is not set by cmd/elitea-main; remove the entry", name))
		}
	}
	slices.Sort(failures)
	return failures
}

// routerConfigKeys answers the top-level keys of the api.RouterConfig literal
// a file passes to api.NewRouter, except a key whose value is the literal
// `nil`: that sets nothing. Both files pass the literal itself, never a
// variable (routerConfigLiteral fails otherwise), so no field can be set
// after it.
func routerConfigKeys(t *testing.T, file *ast.File) []string {
	t.Helper()
	var keys []string
	for _, element := range routerConfigLiteral(t, file).Elts {
		kv, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if value, ok := kv.Value.(*ast.Ident); ok && value.Name == "nil" {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); ok {
			keys = append(keys, key.Name)
		}
	}
	slices.Sort(keys)
	return keys
}

func parseRouterConfigKeys(t *testing.T, path string, src any) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return routerConfigKeys(t, file)
}

// TestTheSurfaceRouterSetsEveryRouteGroupMainSets reads the api.RouterConfig
// literal of main.go and the one of adminsurface.ComposedRouter, and fails on
// a handler or store field the first sets and the second does not, unless
// surfaceRouterUnsetFields gives the reason (ADR-0059 #2985). router.go mounts
// about eighty route groups only when their field is set; the surface tests
// (TestAdministrationSurface*, the action table, the separation matrix) drive
// the composed router, so a group it leaves nil is never checked.
func TestTheSurfaceRouterSetsEveryRouteGroupMainSets(t *testing.T) {
	t.Run("fixtures", func(t *testing.T) {
		production := parseRouterConfigKeys(t, "main.go",
			`package main
func run() { api.NewRouter(api.RouterConfig{Pool: pool, Mailer: m, PublicOrigin: o}) }`)
		surface := parseRouterConfigKeys(t, "composed.go",
			`package adminsurface
func ComposedRouter() { api.NewRouter(api.RouterConfig{Pool: pool}) }`)
		withMailer := append(slices.Clone(surface), "Mailer")
		// A key set to nil in the surface sets nothing.
		if nilMailer := parseRouterConfigKeys(t, "composed.go",
			`package adminsurface
func ComposedRouter() { api.NewRouter(api.RouterConfig{Pool: pool, Mailer: nil}) }`); !slices.Equal(nilMailer, surface) {
			t.Errorf("a key set to nil: read %v, want %v", nilMailer, surface)
		}

		// A field set in production only fails; the scalar PublicOrigin is
		// not a route group.
		failures := surfaceRouterFieldFailures(production, surface, nil)
		if len(failures) != 1 || !containsAll(failures[0], "RouterConfig.Mailer", "surfaceRouterUnsetFields") {
			t.Errorf("a handler field set in production only: got %q, want one failure naming RouterConfig.Mailer", failures)
		}
		// Set in the surface, or listed with a reason, it passes.
		if failures := surfaceRouterFieldFailures(production, withMailer, nil); len(failures) != 0 {
			t.Errorf("the field set in the surface too: got %q, want none", failures)
		}
		if failures := surfaceRouterFieldFailures(production, surface, map[string]string{"Mailer": "fixture"}); len(failures) != 0 {
			t.Errorf("the field listed with a reason: got %q, want none", failures)
		}
		// An entry with no reason, a stale one and a scalar one fail.
		for name, unset := range map[string]map[string]string{
			"no reason":                {"Mailer": ""},
			"set by the surface":       {"Pool": "fixture", "Mailer": "fixture"},
			"not set in main.go":       {"Mailer": "fixture", "GatewayProxy": "fixture"},
			"a scalar setting":         {"Mailer": "fixture", "PublicOrigin": "fixture"},
			"not a RouterConfig field": {"Mailer": "fixture", "NoSuchField": "fixture"},
		} {
			if failures := surfaceRouterFieldFailures(production, surface, unset); len(failures) != 1 {
				t.Errorf("%s: got %q, want exactly one failure", name, failures)
			}
		}
	})

	t.Run("main.go against the composed router", func(t *testing.T) {
		production := parseRouterConfigKeys(t, "main.go", nil)
		surface := parseRouterConfigKeys(t, composedRouterSource, nil)
		if len(production) < 100 || len(surface) < 20 {
			t.Fatalf("read %d fields from main.go and %d from %s; the literal reader found the wrong literal",
				len(production), len(surface), composedRouterSource)
		}
		for _, failure := range surfaceRouterFieldFailures(production, surface, surfaceRouterUnsetFields) {
			t.Error(failure)
		}
	})
}

func containsAll(s string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(s, part) {
			return false
		}
	}
	return true
}
