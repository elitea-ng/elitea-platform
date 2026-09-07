package api_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestNilGatedRouterFieldsAreWiredOrDeclared is a source-level gate, not a
// behavioural test.
//
// router.go registers whole route groups behind `if cfg.X != nil`. When X is
// never populated, those routes are never registered and the API answers 404 —
// a 404 that is indistinguishable from a typo'd path, from a client bug, or from
// a route that was never specified. Nothing fails, nothing logs, and the gap can
// survive an entire replatform.
//
// It already has, three times independently:
//
//	AppsRepo   — a complete 425-line repository existed and nothing called its
//	             constructor. Agent creation 404'd in every deployment (#115).
//	ConvsRepo  — a 951-line repository existed with zero callers; 23
//	             conversation/message routes absent (#123). Filed initially as
//	             "no implementation at all", which was wrong: the check grepped
//	             for the interface NAME, and Go interfaces are satisfied
//	             structurally, so an implementation never mentions them.
//	admin UI   — a different shape of the same class: the E2E image substitutes a
//	             placeholder for the real admin SPA, so those journeys could never
//	             have tested it (#122).
//
// Each was found by accident, late, by someone chasing an unrelated symptom.
// This test makes the fourth one fail immediately and by name.
//
// It deliberately reads the SOURCE rather than constructing a router: the defect
// is "this field is never assigned anywhere in the composition root", which is a
// property of the wiring, not of any runtime behaviour a unit test could observe.
func TestNilGatedRouterFieldsAreWiredOrDeclared(t *testing.T) {
	t.Parallel()

	// Fields that are nil-gated on purpose and are NOT expected to be wired in
	// the default composition root. Each needs a reason, and a reason that names
	// a tracking issue where the absence is a gap rather than a design choice.
	//
	// This is not a dumping ground: an entry here asserts "the routes behind this
	// field are knowingly absent". Adding one to silence the test, without
	// meaning it, reintroduces exactly the invisibility this guards against.
	declaredAbsent := map[string]string{
		// #126 step 1 removed Predictor, LLMService, ChatService,
		// PipelineRunner, ToolTester and MCPSyncer from RouterConfig entirely,
		// together with internal/infra/indexersvc — the prototype Redis RPC
		// client that was their only implementation. It published raw JSON to
		// the "elitea_rpc" channel that pylon-indexer serves through arbiter's
		// gzip(pickle(...)) codec, so every call was dropped on decode; wiring
		// it would have traded an immediate 404 for a 30s timeout and a 500.
		//
		// They were deleted rather than repaired because the replacement
		// transport already ships (runtimecomposition + the Redis command
		// stream + services/elitea-worker-python, deployed in
		// deploy/centry-hybrid/pov-compose.yml), and elitea-docs'
		// spec-transport-implementation.mdx lists indexersvc/rpc.go under
		// "Delete after bounded dispatch/control/output adapters land".
		//
		// Twelve permanently-404 routes went with them. The capabilities behind
		// those routes are recorded in #192 (inbound webhook pipeline trigger),
		// #193 (scheduled pipeline execution), #93 (chat dispatch/streaming)
		// and #194 (AI draft generation, tool testing, MCP tool sync) so the
		// deletion does not erase them — which is the failure this codebase has
		// produced four times (#123, #134, #136, #149).
		//
		// Their entries are gone from this map. The unknown-field check below
		// fails if anyone re-adds one.
		// Wired in main.go as of #126 follow-up: ConvsRepo, SkillsRepo, FoldersRepo,
		// TagsRepo, AnalyticsRepo and WebhookRepo were all pre-existing
		// repositories with zero callers. Their entries are gone from this map,
		// and the stale-entry check below fails if anyone re-adds them.
		//
		// WebhookRepo's entry used to read "webhooks not implemented in the Go
		// stack" — false, and false in the same way the ConvsRepo and Indexer
		// claims were. `dbrepos.NewWebhooksRepo(pool)` existed the whole time and
		// satisfies `webhook.Repository`; that was confirmed with a compile-time
		// assertion, and the assertion was mutation-checked by pointing it at
		// TagsRepo, which fails with "missing method Create".
		//
		// It hid one level deeper than the other five: its gate MOUNTS a
		// subrouter rather than declaring routes inline, so counting r.Get/r.Post
		// within the gated block yields zero and the field looks inert. The five
		// routes live in the handler's Routes(). Any future audit of this pattern
		// has to follow Mount targets, or it will undercount exactly here.
		// Never assigned by cmd/elitea-main: GatewayProxy is what every real
		// deployment composes, and the LiteLLM facade that once could have
		// filled this field is deleted. It stays as an unwired seam for an
		// alternative backend behind the same Auth+Project middleware — see
		// router.go's "/llm has one composed backend" comment.
		"LLMProxy": "optional by design: the LLM data plane is a separate deployment (services/elitea-llm-gateway), reached via GatewayProxy",
		// EventSource is the NATS arm of the project-SSE fallback pair. It is
		// genuinely optional — but ONLY because RedisClient, the other arm, is
		// now wired (see the fallback-pair check below, which is what makes
		// this entry safe). No elitea-main deployment is given a NATS endpoint:
		// deploy/helm/nats is the LLM gateway's broker and elitea-main's only
		// NATS-shaped variable is GATEWAY_NATS_URL, the gateway *client*.
		//
		// The previous pair of entries here read "falls back to RedisClient"
		// and "the EventSource fallback" — each justified by the other, so both
		// being nil satisfied the allowlist while the route they gate was
		// entirely absent (#152). That is the hole the check below closes.
		// Shadow, ShadowMetrics, CutoverRouter and CutoverTracker are gone
		// from RouterConfig entirely (#383). Their entries here read "cutover
		// machinery, enabled per-deployment" — a reason that was never true:
		// no deployment enabled them, no composition root ever assigned them,
		// and the routes behind them answered 404 everywhere. That is the
		// exact shape this map is meant to make visible, and the entries hid
		// it instead, because the map cannot tell a deliberate absence from a
		// forgotten one when the reason is written by the same hand that
		// forgot. The fields, the pylon reverse proxy and the shadow
		// comparator behind them, and the internal-admin token that gated
		// their routes, are all deleted. TestNoPylonBridgeWiringReturns fails
		// if any of it comes back.
		"EventSource": "#152 — the NATS arm; no elitea-main deployment runs NATS, and the Redis arm of this pair IS wired",
	}

	root := repoRootFrom(t)
	// Both router sources, not just router.go (#367). router.go was the only
	// file this test read, and the defect class moved: production_router.go's
	// mountReviewedProductionRoutes carries 25 more `cfg.X != nil` gates that
	// nothing checked. A gate is a gate whichever file declares it, so the
	// scan is driven by this list and a third router file is added here.
	routerFiles := []string{
		"internal/api/router.go",
		"internal/api/production_router.go",
	}
	routerSources := map[string]string{}
	for _, name := range routerFiles {
		routerSources[name] = readFile(t, filepath.Join(root, name))
	}
	mainSrc := readFile(t, filepath.Join(root, "cmd/elitea-main/main.go"))

	gated := regexp.MustCompile(`cfg\.([A-Za-z][A-Za-z0-9]*) != nil`)
	// Field name to the router file that gates it, for error messages that
	// name the file a reader has to open.
	seen := map[string]string{}
	for _, name := range routerFiles {
		for _, m := range gated.FindAllStringSubmatch(routerSources[name], -1) {
			if _, already := seen[m[1]]; !already {
				seen[m[1]] = name
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("found no `cfg.X != nil` gates in " + strings.Join(routerFiles, " or ") +
			" — this test's premise no longer holds; " +
			"either the pattern changed or the regex is wrong. Do not delete this test without replacing the guarantee.")
	}

	assigned := func(field string) bool {
		// Assigned in the RouterConfig literal, e.g. "\tAppsRepo:  appsRepo,"
		return regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(field) + `:\s`).MatchString(mainSrc)
	}

	// --- Fallback pairs -------------------------------------------------
	//
	// The per-field allowlist above cannot see a two-arm gate:
	//
	//	if cfg.EventSource != nil      { r.Mount("/events/…", …) }
	//	else if cfg.RedisClient != nil { r.Mount("/events/…", …) }
	//
	// Each arm is individually "optional" — either one alone registers the
	// route — so each got an allowlist entry justified by the other, and the
	// pair passed the gate with BOTH nil and the route registered by neither.
	// That is #152, and it is a strictly worse failure than a single unwired
	// field: the reason text actively asserts the feature is fine.
	//
	// So: a fallback chain is only satisfiable by at least one member being
	// wired. The pairs are read out of router.go rather than listed here, so a
	// chain added later is covered without anyone remembering this test.
	// Matched by locating each `} else if cfg.B != nil {` and walking BACK to the
	// nearest preceding `if cfg.A != nil {`, rather than with one `(?s)if …
	// .*? … else if …` regex. The single-regex form silently mis-pairs: `.*?`
	// is leftmost-first, so an unrelated earlier `if cfg.X != nil {` claims the
	// match and the real chain is consumed inside it. That version passed this
	// very mutation (both arms nil, both allowlisted) — the bug it exists to
	// catch — while reporting a pair that was never there.
	ifGateRe := regexp.MustCompile(`if cfg\.([A-Za-z][A-Za-z0-9]*) != nil \{`)
	elseIfGateRe := regexp.MustCompile(`\}\s*else if cfg\.([A-Za-z][A-Za-z0-9]*) != nil \{`)

	// Pairs are matched WITHIN one file. Concatenating the router sources and
	// scanning the join would let an `else if` in production_router.go walk
	// back into router.go and report a chain that spans two files and exists
	// in neither.
	type fallbackPair struct{ file, first, second string }
	var pairs []fallbackPair
	for _, name := range routerFiles {
		src := routerSources[name]
		for _, loc := range elseIfGateRe.FindAllStringSubmatchIndex(src, -1) {
			second := src[loc[2]:loc[3]]
			prior := ifGateRe.FindAllStringSubmatch(src[:loc[0]], -1)
			if len(prior) == 0 {
				continue
			}
			pairs = append(pairs, fallbackPair{file: name, first: prior[len(prior)-1][1], second: second})
		}
	}
	if len(pairs) == 0 {
		t.Fatal("found no `if cfg.A != nil { … } else if cfg.B != nil { … }` fallback chains in " +
			strings.Join(routerFiles, " or ") + " — " +
			"either the last one was removed (delete this block) or the pattern changed. " +
			"Do not delete this without replacing the guarantee: a fallback pair with both arms nil " +
			"is how #152 shipped a route that existed in no deployment.")
	}
	for _, pair := range pairs {
		first, second := pair.first, pair.second
		if assigned(first) || assigned(second) {
			continue
		}
		t.Errorf("RouterConfig.%s and RouterConfig.%s form a fallback pair in %s "+
			"(`if cfg.%s != nil … else if cfg.%s != nil …`) and NEITHER is assigned in cmd/elitea-main/main.go.\n"+
			"  The routes behind that chain are registered by no arm and answer 404 in every deployment.\n"+
			"  An allowlist entry for one arm that points at the other is circular and does NOT satisfy this:\n"+
			"  at least one member has to be genuinely wired. (#152)",
			first, second, pair.file, first, second)
	}

	// --- Flag-dark composition -------------------------------------------
	//
	// Extending the scan to production_router.go found nothing, and that is
	// the point (#367). All 25 of its gates ARE assigned in main.go's
	// RouterConfig literal, so the question this test asks — "does the
	// composition root assign this field?" — answers yes for every one of
	// them. The defect class changed shape rather than going away:
	//
	//	var currentApplicationSkills *applicationskillsapi.CurrentApplicationSkillsRoute
	//	if currentApplicationSkillsSettings.Enabled { currentApplicationSkills = … }
	//	…
	//	CurrentApplicationSkills: currentApplicationSkills,
	//
	// The field is assigned. It is assigned nil, in every deployment, because
	// the flag defaults off and no deployment sets it. A textual assignment
	// check cannot tell that apart from a real wiring, so it passed while the
	// route it gates was registered nowhere.
	//
	// So the flags themselves are checked. A composition-root flag that no
	// deployment sets is a branch that runs in no deployment: whatever it
	// composes is dark, and — worse than a 404 — it can leave a lower-priority
	// route to answer instead. That is exactly what happened to
	// application_skills, where chi fell through to a handler that ignores
	// {appVersionID} and returns every skill in the project, at 200.
	//
	// An entry here asserts "no deployment sets this flag, and that is
	// intended". It carries the same obligation as declaredAbsent above: a
	// reason, and a tracking issue.
	darkFlags := map[string]string{
		// Off on purpose. The reason recorded here before was the retired
		// LiteLLM lifecycle facade; that reason is stale (#460), because the
		// lifecycle takes no LLM transport now.
		//
		// It also cannot be set on its own.
		// currentConfigurationsConfigFromEnv rejects a deployment that sets it
		// while ELITEA_CONFIGURATIONS_ENABLED is off, so it can never be the
		// only flag standing between a route and its handler.
		//
		// Two earlier claims here were wrong, and the correction is the point
		// of this entry. "Nothing answers in its place" and "dark here means
		// 404" both fail: router.go mounts the compatibility configuration
		// handler on the same three methods and the same two paths, with no
		// flag on the mount. So the flag does not darken a route — it decides
		// WHICH of two handlers answers.
		// TestConfigurationWriteRouteWinnerDependsOnTheMutationComposition
		// runs both compositions and records the answer: the reviewed route
		// wins when it is composed, and the compatibility mount serves the
		// path in every shipped install, where it is not.
		"ELITEA_CONFIGURATIONS_MUTATION_ENABLED": "#460 — the compatibility write routes serve this path in every install; turning the flag on is a cutover to a second route with a different request shape",
		// ELITEA_INDEX_TYPES_ENABLED was listed here, first against #367 and
		// then against #394. Its route answered {document_types, image_types,
		// code_types} only, and the published contract for the same path is
		// DocumentLoadersResponse — {items, total} — so turning the flag on made
		// the shipped generated client drift. #394 removed the conflict rather
		// than the flag: internal/api/v2/indextypes now answers the published
		// envelope BESIDE the Pylon keys, from one snapshot read, and
		// deploy/helm/elitea/values-standalone.yaml sets the flag. The entry is
		// gone because the check above turns a kept entry into a failure once a
		// deployment assigns the flag.
		//
		// Keep the snapshot files with it. scripts/contract/
		// sync_index_types_snapshot.py holds the snapshot to the locked SDK
		// revision, and ci-python.yml runs that generator with --check against
		// internal/runtimecomposition/current_index_types_snapshot.json and
		// internal/api/v2/indextypes/testdata/current_index_types_ui_response.json.
		// Deleting either file turns that gate red.
		//
		// The chart DERIVES the flag now (deploy/helm/elitea/values.yaml ships
		// it empty, elitea-main.indexTypesEnabled fills it): on where the
		// install authenticates, off where it does not. The prototype handler
		// that used to answer the path with a 501 refusal is deleted with the
		// route, so an install that turns the capability off has no handler
		// for the path at all — which is a deliberate 404, stated in the
		// chart, and not a silent second answer.
		// ELITEA_APPLICATION_SKILLS_ENABLED was listed here, with the same
		// conflict: the route it composes answered {skills, max_skills} and
		// elitea-web reads the SkillsList envelope. #395 removed the conflict
		// rather than the flag — the route now answers both key sets from one
		// row read — and deploy/helm/elitea/values-standalone.yaml sets the
		// flag. The entry is gone because the check above turns a kept entry
		// into a failure once a deployment assigns the flag.
	}

	flagFiles, globErr := filepath.Glob(filepath.Join(root, "cmd/elitea-main/*_config.go"))
	if globErr != nil {
		t.Fatalf("glob composition-root flag readers: %v", globErr)
	}
	flagLookupRe := regexp.MustCompile(`lookup\("(ELITEA_[A-Z0-9_]*_ENABLED)"\)`)
	compositionFlags := map[string]string{}
	for _, file := range flagFiles {
		src := readFile(t, file)
		for _, m := range flagLookupRe.FindAllStringSubmatch(src, -1) {
			if _, already := compositionFlags[m[1]]; !already {
				compositionFlags[m[1]] = filepath.Base(file)
			}
		}
	}
	if len(compositionFlags) == 0 {
		t.Fatal("found no `lookup(\"ELITEA_…_ENABLED\")` reads in cmd/elitea-main/*_config.go — " +
			"either the composition-root flag readers moved or the pattern changed. " +
			"Do not delete this block without replacing the guarantee: a flag no deployment sets " +
			"is how #367 shipped a route that answered with the wrong project's skills.")
	}

	deployed := deployAssignedEnv(t, filepath.Join(root, "..", "..", "deploy"))
	darkUndeclared, staleDark, orphanedDark := gradeCompositionFlags(
		compositionFlags,
		deployed,
		darkFlags,
	)

	for _, flag := range darkUndeclared {
		t.Errorf("%s is read by cmd/elitea-main/%s, and no file under deploy/ sets it.\n"+
			"  Every branch behind it is dead in every deployment, so whatever it composes is\n"+
			"  unreachable — and any lower-priority route on the same path answers in its place.\n"+
			"  Either set it in a deployment, delete the flag and its branch, or add it to darkFlags\n"+
			"  in this test WITH an issue reference. (#367)", flag, compositionFlags[flag])
	}
	for _, flag := range staleDark {
		t.Errorf("darkFlags lists %s, but %s sets it.\n"+
			"  Remove the entry: a stale allowlist hides the next real regression for this flag.",
			flag, deployed[flag])
	}
	for _, flag := range orphanedDark {
		t.Errorf("darkFlags lists %s, but no reader in cmd/elitea-main/*_config.go looks it up.\n"+
			"  The flag was removed and the entry was left behind, where it reads as a live\n"+
			"  claim about a branch that no longer exists. Delete the entry.", flag)
	}

	var unwired, staleAllowlist []string
	for field := range seen {
		isAssigned := assigned(field)
		_, declared := declaredAbsent[field]

		switch {
		case isAssigned && declared:
			staleAllowlist = append(staleAllowlist, field)
		case !isAssigned && !declared:
			unwired = append(unwired, field)
		}
	}
	sort.Strings(unwired)
	sort.Strings(staleAllowlist)

	for _, f := range unwired {
		t.Errorf("RouterConfig.%s gates routes in %s but is never assigned in cmd/elitea-main/main.go.\n"+
			"  Those routes are silently unregistered and answer 404 in every deployment.\n"+
			"  Either wire it, or add it to declaredAbsent in this test WITH an issue reference —\n"+
			"  so the gap is visible instead of being discovered by someone debugging a 404.", f, seen[f])
	}
	for _, f := range staleAllowlist {
		t.Errorf("RouterConfig.%s is now wired in main.go but is still listed in declaredAbsent.\n"+
			"  Remove the entry: a stale allowlist hides the next real regression for this field.", f)
	}

	// --- Allowlist entries for fields that no longer exist ---------------
	//
	// The stale check above only fires for a field that is STILL gated in
	// router.go. Delete the gate — as #126 did for Predictor, LLMService,
	// ChatService, PipelineRunner, ToolTester and MCPSyncer — and the entry
	// simply stops being consulted: it rots silently, and the next reader
	// takes it as a live statement about the router. That is the same
	// "documentation drifts away from the code with nothing checking" failure
	// the allowlist exists to prevent, so it is checked too.
	var orphaned []string
	for field := range declaredAbsent {
		if _, gatedSomewhere := seen[field]; !gatedSomewhere {
			orphaned = append(orphaned, field)
		}
	}
	sort.Strings(orphaned)
	for _, f := range orphaned {
		t.Errorf("declaredAbsent lists RouterConfig.%s, but no `cfg.%s != nil` gate exists in %s.\n"+
			"  The field or its gate was removed and the entry was left behind, where it reads as a\n"+
			"  live claim about routes that no longer exist. Delete the entry.",
			f, f, strings.Join(routerFiles, " or "))
	}
}

// gradeCompositionFlags is the whole judgement the dark-flag check makes, in
// one place so that the judgement itself can be tested.
//
// It used to be an inline loop, and that made the check unfalsifiable: the only
// way to see it work was to break the repository and watch the real test go
// red. TestCompositionFlagGradingCatchesADarkFlagWithNoJustification below
// drives it with synthetic inputs instead, so the three verdicts stay proved
// while the real inputs move.
//
//   - darkUndeclared: read by the composition root, set by no deployment, and
//     not declared dark. This is the #367 shape — a branch that runs nowhere,
//     and a lower-priority route answering on the path in its place.
//   - staleDark: declared dark AND set by a deployment. The claim is no longer
//     true, and leaving it hides the next real regression for that flag.
//   - orphanedDark: declared dark, and no composition-root reader looks it up.
//     The flag went away and the entry stayed behind as a live-looking claim.
func gradeCompositionFlags(
	compositionFlags map[string]string,
	deployed map[string]string,
	darkFlags map[string]string,
) (darkUndeclared, staleDark, orphanedDark []string) {
	for flag := range compositionFlags {
		_, isDeployed := deployed[flag]
		_, declared := darkFlags[flag]

		switch {
		case isDeployed && declared:
			staleDark = append(staleDark, flag)
		case !isDeployed && !declared:
			darkUndeclared = append(darkUndeclared, flag)
		}
	}
	for flag := range darkFlags {
		if _, read := compositionFlags[flag]; !read {
			orphanedDark = append(orphanedDark, flag)
		}
	}
	sort.Strings(darkUndeclared)
	sort.Strings(staleDark)
	sort.Strings(orphanedDark)
	return darkUndeclared, staleDark, orphanedDark
}

// TestCompositionFlagGradingCatchesADarkFlagWithNoJustification proves the
// check still refuses a flag that has neither a deployment assignment nor a
// justification (#395's acceptance criterion).
//
// #395 removed ELITEA_APPLICATION_SKILLS_ENABLED from darkFlags, because
// deploy/helm/elitea/values-standalone.yaml now sets it. Removing an entry from
// an allowlist is exactly the edit that can turn a gate into a no-op, so the
// grading is exercised here on its own inputs.
func TestCompositionFlagGradingCatchesADarkFlagWithNoJustification(t *testing.T) {
	t.Parallel()

	darkUndeclared, staleDark, orphanedDark := gradeCompositionFlags(
		map[string]string{
			// Read, set by nobody, justified by nobody.
			"ELITEA_NEW_THING_ENABLED": "new_thing_config.go",
			// Read, set by nobody, justified. Allowed.
			"ELITEA_DARK_THING_ENABLED": "dark_thing_config.go",
			// Read and set. Allowed.
			"ELITEA_LIVE_THING_ENABLED": "live_thing_config.go",
			// Read, set, AND still declared dark. The claim is stale.
			"ELITEA_WAS_DARK_ENABLED": "was_dark_config.go",
		},
		map[string]string{
			"ELITEA_LIVE_THING_ENABLED": "values.yaml:1",
			"ELITEA_WAS_DARK_ENABLED":   "values-standalone.yaml:2",
		},
		map[string]string{
			"ELITEA_DARK_THING_ENABLED": "#1 — justified",
			"ELITEA_WAS_DARK_ENABLED":   "#2 — no longer true",
			"ELITEA_DELETED_ENABLED":    "#3 — the reader is gone",
		},
	)

	if len(darkUndeclared) != 1 || darkUndeclared[0] != "ELITEA_NEW_THING_ENABLED" {
		t.Fatalf(
			"a flag with no deployment assignment and no justification must fail the check; got %v",
			darkUndeclared,
		)
	}
	if len(staleDark) != 1 || staleDark[0] != "ELITEA_WAS_DARK_ENABLED" {
		t.Fatalf("stale dark entries = %v", staleDark)
	}
	if len(orphanedDark) != 1 || orphanedDark[0] != "ELITEA_DELETED_ENABLED" {
		t.Fatalf("orphaned dark entries = %v", orphanedDark)
	}
}

// TestDeployAssignedEnvReadsAssignmentsNotMentions pins the input side of the
// grading above: only a line that actually turns a flag ON counts as a
// deployment assignment.
func TestDeployAssignedEnvReadsAssignmentsNotMentions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "values.yaml"), []byte(
		"main:\n"+
			"  env:\n"+
			"    # ELITEA_COMMENTED_ENABLED: \"true\" — a comment is not a deployment\n"+
			"    ELITEA_OFF_ENABLED: \"false\"\n"+
			"    ELITEA_ON_ENABLED: \"true\"\n"+
			"    ELITEA_TRAILING_ENABLED: \"true\" # with a reason\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "runbook.md"), []byte(
		"    ELITEA_DOCUMENTED_ENABLED: \"true\"\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}

	assigned := deployAssignedEnv(t, dir)
	for _, flag := range []string{"ELITEA_ON_ENABLED", "ELITEA_TRAILING_ENABLED"} {
		if _, found := assigned[flag]; !found {
			t.Fatalf("%s is assigned ON and must count as deployed: %v", flag, assigned)
		}
	}
	for _, flag := range []string{
		"ELITEA_COMMENTED_ENABLED",
		"ELITEA_OFF_ENABLED",
		"ELITEA_DOCUMENTED_ENABLED",
	} {
		if where, found := assigned[flag]; found {
			t.Fatalf("%s must not count as deployed, but %s claimed it", flag, where)
		}
	}
}

// deployAssignedEnv reports every ELITEA_* variable that a file under deploy/
// actually ASSIGNS, mapped to the file:line that assigns it.
//
// "Assigns", not "mentions". deploy/docker-compose.standalone-full.yml:22 reads
//
//	# ELITEA_CONFIGURATIONS_ENABLED=true, which nothing here sets.
//
// so a plain substring search — or a regex run over the whole file rather than
// per line — reports that flag as configured on the strength of a comment
// saying it is not. That inverted answer is worse than no check: it turns the
// gate green for precisely the flag it is meant to catch.
//
// Documentation is excluded for the same reason. deploy/INDEX_V2_CUTOVER.md
// contains a fenced YAML sample with `ELITEA_RUNTIME_ENABLED: "true"` in it; a
// runbook showing an operator what to type is not a deployment that types it.
func deployAssignedEnv(t *testing.T, dir string) map[string]string {
	t.Helper()

	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		// Not "nothing found, so everything is fine". A missing deploy/ means
		// this check silently stops checking, which is how a gate keeps
		// reporting success after the thing it reads moves away.
		t.Fatalf("deploy/ not found at %s: the flag check cannot run without it (%v)", dir, err)
	}

	configExt := map[string]bool{
		".yml": true, ".yaml": true, ".env": true, ".sh": true, ".tpl": true, ".conf": true,
	}
	// `ELITEA_X: value` (compose) or `ELITEA_X=value` (env file, shell).
	assignRe := regexp.MustCompile(`^(ELITEA_[A-Z0-9_]+)\s*[:=]\s*(.*)$`)
	// `- name: ELITEA_X` (a Kubernetes env entry, whose value is the next key).
	namedRe := regexp.MustCompile(`^-?\s*name:\s*"?(ELITEA_[A-Z0-9_]+)"?\s*$`)

	assigned := map[string]string{}
	walkErr := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !configExt[filepath.Ext(path)] {
			return nil
		}
		body, readErr := os.ReadFile(path) //nolint:gosec // fixed, test-local path
		if readErr != nil {
			return readErr
		}
		for number, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
				continue
			}
			trimmed = strings.TrimPrefix(trimmed, "export ")
			name := ""
			if m := assignRe.FindStringSubmatch(trimmed); m != nil {
				// Assigned OFF is not assigned. A flag set to "false" leaves
				// every branch behind it exactly as unreachable as no
				// assignment at all, so counting it as deployed would report
				// the flag as live on the strength of the line that turns it
				// off — the same inverted answer the comment rule above
				// avoids. It also inverts the darkFlags check: a chart that
				// documents a dark flag as "false" would make the darkFlags
				// entry look stale, and deleting that entry is what would
				// then let someone flip the flag to "true" unchallenged.
				if !envValueIsOn(m[2]) {
					continue
				}
				name = m[1]
			} else if m := namedRe.FindStringSubmatch(trimmed); m != nil {
				name = m[1]
			} else {
				continue
			}
			if _, already := assigned[name]; !already {
				assigned[name] = fmt.Sprintf("%s:%d", filepath.Base(path), number+1)
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", dir, walkErr)
	}
	return assigned
}

// envValueIsOn reports whether a deploy-file assignment turns a flag ON.
//
// Only the ELITEA_*_ENABLED booleans this file grades are ever read back, so a
// non-boolean value (a URL, a path, a port) counts as assigned. The quotes are
// stripped because the Helm values files write "false" and the compose files
// write false, and a trailing comment is dropped because `FLAG: false # why`
// is a normal way to write the line.
//
// GRADE BOOLEANS ONLY. Do not use this helper for a variable that holds a
// number. It reads "0" as off, because a boolean flag written as 0 is off.
// But 0 is also a correct value for a worker count, a replica count or a
// port. For those variables this helper answers "not assigned" when the
// deployment did assign them, which is the inverted answer the caller above
// exists to prevent. If you must grade a numeric variable, give it a
// different helper.
func envValueIsOn(raw string) bool {
	value := strings.TrimSpace(raw)
	if idx := strings.Index(value, "#"); idx >= 0 {
		value = strings.TrimSpace(value[:idx])
	}
	value = strings.Trim(value, `"'`)
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "false", "off", "no", "0":
		return false
	}
	return true
}

func repoRootFrom(t *testing.T) string {
	t.Helper()
	// This test lives in internal/api/, so the service root is two levels up.
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve service root: %v", err)
	}
	return root
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // fixed, test-local path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if strings.TrimSpace(string(b)) == "" {
		t.Fatalf("%s is empty", path)
	}
	return string(b)
}
