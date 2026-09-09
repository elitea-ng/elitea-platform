package oapiserver_test

// Conformance test for api/openapi/v2.yaml against the real chi router
// (UI reimplementation spec §5.1 / §9.3 unit W1).
//
// The generated server in internal/api/oapiserver is never mounted (preflight
// fact P3), so this test is the only thing that fails when the spec and the
// hand-assembled router in internal/api/router.go disagree.
//
// Forward direction (asserted here): every operationId in v2.yaml must
// resolve to a registered route.
//
// Reverse direction (manifest-driven, by design): the spec covers ~23% of the
// router surface on purpose, so there is no global router→spec assertion.
// The reverse check reads the new UI's endpoint manifest (spec §5.3). That
// manifest is apps/elitea-web/src/shared/api/endpoints.manifest.json. The
// check requires v2.yaml to cover every endpoint the app calls. The
// hand-written Wave-2 endpoints are held in testdata/reverse_check_allowlist.txt,
// which may only shrink — see the manifest_reverse_check subtest below and
// oapiserver.MissingFromSpec.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api"
	v2analytics "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/analytics"
	v2applicationskills "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/applicationskills"
	v2auth "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/auth"
	v2convs "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	v2deepwiki "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/deepwiki"
	v2evaluation "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
	v2events "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/events"
	v2folders "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/folders"
	v2indextypes "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/indextypes"
	v2inventory "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/inventory"
	v2memories "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/memories"
	v2pipelinetriggers "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/pipelinetriggers"
	v2skills "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
	v2social "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/social"
	v2tags "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/tags"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/webhook"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/oapiserver"
)

const (
	// specPath is api/openapi/v2.yaml relative to this package directory.
	specPath = "../../../api/openapi/v2.yaml"
	// manifestPath is the new UI's endpoint manifest, relative to this package
	// directory (repo root is five levels up). It is git-tracked.
	//
	// DEFECT this constant fixes: it named apps/elitea-web/parity/manifest.json,
	// which is the parity SHARD INDEX. That file holds only version /
	// generatedFrom / shards, and its schema forbids extra keys, so the
	// `endpoints` field never existed. json.Unmarshal left the slice nil, the
	// reverse check looped over nothing, and the subtest reported PASS while it
	// measured zero endpoints.
	manifestPath = "../../../../../apps/elitea-web/src/shared/api/endpoints.manifest.json"
	// allowlistPath holds the endpoint ids the spec does not describe yet.
	allowlistPath = "testdata/reverse_check_allowlist.txt"

	// Sanity floors: the spec has 152 operationIds today (it had 78 when this
	// floor was first written, and the floor was never raised with it).
	// chi.Walk over the full-surface test config yields 313 method+pattern
	// registrations, 277 after the compat-shim exclusion in CollectRoutes
	// (4 shim patterns x 9 methods). It was 325/289 until #126 deleted the
	// twelve routes gated on the retired prototype indexer transport.
	// If either input collapses, the conformance loop would vacuously pass —
	// so guard the inputs themselves.
	//
	// MEASURED 2026-09-06, after the three Inventory operations landed: 185
	// spec operations and 457 collected routes. Both numbers above are the
	// record of an earlier day and neither has been kept current; the floors
	// are minima, so they held anyway. The measurement is written down here
	// rather than folded into the prose, because a prose number nobody
	// re-measures is how the 152 and the 277 stopped being true.
	minSpecOperations = 145
	minRouterRoutes   = 270
	// minManifestEndpoints guards the reverse check's own input. The manifest
	// holds 179 endpoints. A moved, renamed or reshaped file must fail loudly,
	// not silently measure nothing.
	minManifestEndpoints = 170
	// maxAllowlistEntries pins the committed size of the allowlist. The number
	// may only go down. A new undocumented endpoint must fail the gate.
	//
	// 94 -> 93 when issue #194 routed and described
	// POST /elitea_core/predict_llm/prompt_lib/{project_id}. That path carries
	// THREE manifest ids (pipelines.generateContentStreaming,
	// agents.generateContentBlocking, chatMessages.generateContentBlocking),
	// all with a null operationId, so one spec operation covers all three by
	// path. Only the first was ever on the allowlist — the other two were
	// failing this gate outright, because the allowlist was already AT its cap
	// and could not take them. Describing the endpoint was the only sanctioned
	// way out, and it needed the route to exist first.
	//
	// 93 -> 78 (issue 621), when pathCoveredBySpec was repaired. It compared
	// the manifest path against the BASED spec candidate ("/api/v2/..."), which
	// no manifest entry carries, so the path arm of the reverse check never
	// matched and this list could never shrink. Fifteen ids the document
	// already described came off it in one step. See pathCoveredBySpec.
	//
	// 78 -> 76 (#254 P1), when the three AI-draft routes were served and
	// described. Two ids came off: applications.generateAgentDraft and
	// skills.generateDraft, the two manifest entries on those paths.
	//
	// 76 -> 65 (#874), when skill versioning was described: the eleven
	// skills.* ids (getSkill, getSkillVersion, updateSkill,
	// updateSkillVersion, createSkillVersion, deleteSkill,
	// deleteSkillVersion, setDefaultVersion, import, export, exportVersion)
	// all came off in the same change that gave skills real multi-version
	// history, compare and rollback.
	maxAllowlistEntries = 65
)

// buildFullSurfaceConfig returns a RouterConfig for the real production
// router (api.NewRouter) with every optional dependency satisfied by an inert
// stub, so that ALL conditionally-registered route groups are present. Stubs
// are zero-value structs embedding the dependency interface: they satisfy the
// interface at compile time and are never invoked, because this test only
// walks the route table and never serves a request.
//
// Deliberately left nil (their surface is not part of the public HTTP API the
// spec describes): AdminUI (static SPA mount), Shadow/Cutover (/internal/*),
// Pool/Storage/RedisClient (constructor-injected, nil-safe at registration).
func buildFullSurfaceConfig() api.RouterConfig {
	return api.RouterConfig{
		Auth: api.AuthDeps{
			SessionHandler: &v2auth.SessionHandler{},
			OIDCHandler:    &v2auth.OIDCHandler{},
		},
		AppsRepo: struct{ applications.Repository }{},

		// Agent Evaluation (#617). All three are MANDATORY here, not optional
		// stubs, and the third is the one that is easy to forget: the run
		// routes are gated on `EvalRunsRepo != nil && EvalDimensionsRepo != nil`
		// together, because a run's snapshot is built from the dimension
		// library. Leave EvalDimensionsRepo nil and the five run operations the
		// spec declares resolve to no route at all, and the forward check fails
		// with a message about the spec rather than about this config.
		EvalDatasetsRepo:   struct{ v2evaluation.DatasetRepository }{},
		EvalRunsRepo:       struct{ v2evaluation.RunRepository }{},
		EvalDimensionsRepo: struct{ v2evaluation.Repository }{},
		SkillsRepo:         struct{ v2skills.Repository }{},
		FoldersRepo:        struct{ v2folders.Repository }{},
		MemoriesRepo:       struct{ v2memories.Repository }{},
		TagsRepo:           struct{ v2tags.Repository }{},
		AnalyticsRepo:      struct{ v2analytics.Repository }{},
		ConvsRepo:          struct{ v2convs.Repository }{},
		WebhookRepo:        struct{ webhook.Repository }{},
		EventSource:        struct{ v2events.EventSource }{},
		LLMProxy:           http.NotFoundHandler(),

		// CurrentAvatarRoute has no interface-typed dependency this stub
		// scheme can zero-value: a bare &v2social.CurrentAvatarRoute{} still
		// satisfies the != nil check that gates route registration in
		// production_router.go, and this test only chi.Walks the router — it
		// never serves a request through the stub.
		CurrentSocialAvatar: &v2social.CurrentAvatarRoute{},

		// Same scheme for the DeepWiki facade: a zero Route satisfies the
		// != nil check that gates registration, and this test only walks the
		// router. Its ServeHTTP answers 503 for a zero value rather than
		// panicking, so even a served request would be harmless here.
		DeepWiki: &v2deepwiki.Route{},

		// The Inventory facade, the SECOND facade over the same provider SPI,
		// mounted by the same four lines in production_router.go and stubbed
		// the same way. It is MANDATORY here, not optional: v2.yaml now
		// describes invokeInventoryTool, getInventoryInvocation and
		// cancelInventoryInvocation, and those three operations resolve to no
		// route at all unless this field is non-nil.
		Inventory: &v2inventory.Route{},

		// The index-types and attached-skills reads (#394, #395). Both are
		// MANDATORY here, not optional stubs: each is the ONLY handler for a
		// path the spec declares (getDocumentLoaders, listApplicationSkills).
		// The prototype mounts in router.go used to cover those two operations
		// for this walk, and deleting them left the spec describing two routes
		// the full surface did not register. Same zero-value scheme as above —
		// each ServeHTTP answers 404 for a zero value rather than panicking.
		CurrentIndexTypes:        &v2indextypes.CurrentIndexTypesRoute{},
		CurrentApplicationSkills: &v2applicationskills.CurrentApplicationSkillsRoute{},

		// The unattended pipeline entry points (issues 192, 193). MANDATORY
		// here, not an optional stub: this handler is the only one for the
		// eight operations v2.yaml now describes, and all eight resolve to no
		// route unless the field is non-nil. A handler with NO pool is the
		// right stub — every route answers 503 for one rather than panicking,
		// and this walk never serves a request.
		PipelineTriggers: v2pipelinetriggers.NewHandler(nil),
	}
}

// loadManifestEndpoints reads the UI endpoint manifest and guards its shape.
//
// The file is git-tracked, so its absence is a breakage, not a unit that has
// not landed. The floor catches a reshaped or emptied file, which is how this
// check went inert before: a nil slice makes the loop below assert nothing.
func loadManifestEndpoints(t *testing.T) []oapiserver.ManifestEndpoint {
	t.Helper()
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("reading %s: %v", manifestPath, err)
	}
	var manifest struct {
		Endpoints []oapiserver.ManifestEndpoint `json:"endpoints"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parsing %s: %v", manifestPath, err)
	}
	if len(manifest.Endpoints) < minManifestEndpoints {
		t.Fatalf("%s yielded only %d endpoints (want >= %d) — did the file move or change shape?",
			manifestPath, len(manifest.Endpoints), minManifestEndpoints)
	}
	return manifest.Endpoints
}

// loadReverseCheckAllowlist reads the ids of the hand-written endpoints that
// the spec does not describe yet. One id per line. `#` starts a comment.
func loadReverseCheckAllowlist(t *testing.T) map[string]struct{} {
	t.Helper()
	data, err := os.ReadFile(allowlistPath)
	if err != nil {
		t.Fatalf("reading %s: %v", allowlistPath, err)
	}
	allowed := make(map[string]struct{})
	for _, line := range strings.Split(string(data), "\n") {
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if _, duplicate := allowed[line]; duplicate {
			t.Errorf("%s lists %q twice", allowlistPath, line)
		}
		allowed[line] = struct{}{}
	}
	if len(allowed) > maxAllowlistEntries {
		t.Fatalf("%s holds %d ids (max %d). The allowlist may only shrink: describe the new endpoint in api/openapi/v2.yaml instead.",
			allowlistPath, len(allowed), maxAllowlistEntries)
	}
	return allowed
}

func TestSpecRouterConformance(t *testing.T) {
	router := api.NewRouter(buildFullSurfaceConfig())

	routes, err := oapiserver.CollectRoutes(router)
	if err != nil {
		t.Fatalf("collecting routes: %v", err)
	}
	if routes.Len() < minRouterRoutes {
		t.Fatalf("router walk found only %d method+pattern registrations (want >= %d) — did a route group lose its stub dependency in buildFullSurfaceConfig?", routes.Len(), minRouterRoutes)
	}

	ops, err := oapiserver.LoadSpecOperations(specPath)
	if err != nil {
		t.Fatalf("loading spec: %v", err)
	}
	if len(ops) < minSpecOperations {
		t.Fatalf("spec declares only %d operations (want >= %d) — v2.yaml truncated?", len(ops), minSpecOperations)
	}

	// EVERY candidate must resolve, not just one.
	//
	// DEFECT this guards: the spec declared a second server base,
	// `/api/v2/elitea_core`, that no operation resolves under, because every
	// path already carries its own plugin prefix. A generated client takes
	// the first server as its default base, so every call 404'd. This loop
	// stopped at the first candidate that matched. The good `/api/v2` base
	// always rescued the bogus one. The check therefore passed for a spec no
	// generator could use.
	//
	// A base that serves no operation is a defect,
	// however many bases the document declares.
	t.Run("every_spec_operation_resolves_to_a_route", func(t *testing.T) {
		var unmatched []string
		for _, op := range ops {
			var dead []string
			for _, cand := range op.CandidatePaths() {
				if !routes.Resolves(op.Method, cand) {
					dead = append(dead, cand)
				}
			}
			if len(dead) > 0 {
				unmatched = append(unmatched, fmt.Sprintf(
					"  %-28s %-6s %s\n    no route for: %s",
					op.OperationID, op.Method, op.Path,
					strings.Join(dead, " , ")))
			}
		}
		if len(unmatched) > 0 {
			t.Errorf("%d of %d spec operations in api/openapi/v2.yaml do not resolve to a registered chi route under every declared server base:\n%s\n\nEither register the route in internal/api/router.go, or fix the spec entry or the `servers:` base (spec §5.1).",
				len(unmatched), len(ops), strings.Join(unmatched, "\n"))
		}
	})

	// The reverse check is manifest-driven by design (the spec covers ~23% of
	// the router). It gates what the new UI actually calls.
	t.Run("manifest_reverse_check", func(t *testing.T) {
		endpoints := loadManifestEndpoints(t)
		allowed := loadReverseCheckAllowlist(t)

		stillMissing := make(map[string]struct{}, len(allowed))
		for _, ep := range oapiserver.MissingFromSpec(ops, endpoints) {
			if _, ok := allowed[ep.ID]; ok {
				stillMissing[ep.ID] = struct{}{}
				continue
			}
			t.Errorf("manifest endpoint %s (%s %s, operationId=%q) is not covered by api/openapi/v2.yaml.\nDescribe it in the spec, or add its id to %s and raise nothing — the allowlist may only shrink (spec §5.3).",
				ep.ID, ep.Method, ep.Path, ep.OperationID, allowlistPath)
		}
		for id := range allowed {
			if _, ok := stillMissing[id]; !ok {
				t.Errorf("%s lists %q, but the spec now covers that endpoint. Delete the line.", allowlistPath, id)
			}
		}
	})

	// Keep the reverse-check hook itself honest until the real manifest
	// exists: a manifest built from the spec's own operations must be fully
	// covered, and a bogus entry must be reported.
	t.Run("reverse_check_hook_selftest", func(t *testing.T) {
		selfManifest := make([]oapiserver.ManifestEndpoint, 0, len(ops))
		for _, op := range ops {
			selfManifest = append(selfManifest, oapiserver.ManifestEndpoint{
				ID:          "self." + op.OperationID,
				Method:      op.Method,
				Path:        op.CandidatePaths()[0],
				OperationID: op.OperationID,
			})
		}
		if missing := oapiserver.MissingFromSpec(ops, selfManifest); len(missing) != 0 {
			t.Errorf("self-manifest built from the spec reported %d missing endpoints: %+v", len(missing), missing)
		}

		bogus := []oapiserver.ManifestEndpoint{
			{ID: "bogus.byId", Method: "GET", Path: "/api/v2/nope", OperationID: "doesNotExistAnywhere"},
			{ID: "bogus.byPath", Method: "GET", Path: "/api/v2/definitely/not/in/the/spec"},
		}
		if missing := oapiserver.MissingFromSpec(ops, bogus); len(missing) != 2 {
			t.Errorf("reverse-check hook failed to flag bogus manifest entries: got %d missing, want 2", len(missing))
		}
	})
}
