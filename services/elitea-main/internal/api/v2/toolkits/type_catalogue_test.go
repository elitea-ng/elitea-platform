package toolkits_test

// The served toolkit TYPE catalogue: what it holds, what it says about each
// type, and what it refuses to say.
//
// The catalogue used to be the eight hand-written entries in handler.go. The
// digest-pinned SDK snapshot held fifty-two types, every one of them loaded
// into the process at startup, and forty-four of them were offered to nobody.
// These tests are written over the REAL pinned files rather than fixtures,
// because a fixture cannot fail when the snapshot regenerates and the whole
// point of the catalogue is that it tracks the admitted SDK.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/guardrails"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
)

// nativeToolkitTypes are the hand-written types elitea_core owns rather than
// the pinned SDK catalogue. Four of them (application, custom, database,
// datasource) are not SDK toolkits at all, so no worker capability answer
// describes them. The fifth, imagegen, IS worker-implemented (elitea_sdk/
// tools/imagegen, Python-only, #864) but is still hand-written here rather
// than catalogued, because its credential is the project's own
// image_generation model rather than a pinned-snapshot settings shape; see
// nativeWorkerGatedToolkitTypes in type_catalogue.go for how it still gets a
// real worker-capability verdict despite being hand-written.
var nativeToolkitTypes = []string{"application", "custom", "database", "datasource", "imagegen"}

// pythonUnsupportedTypes are the SDK types the admitted Python worker image
// cannot build, expressed as TYPES rather than as the import keys the snapshot
// records. The pair kubernetes/k8s is the reason this list is written out: the
// SDK registers that toolkit under `k8s` and publishes it as `kubernetes`, so a
// capability check keyed on the type would never match it and the tile would be
// offered by a deployment that cannot run it.
//
// google_places, rally, service_now and slack used to be here too. #869 added
// their four measured third-party API clients (googlemaps, pyral, pysnc,
// slack_sdk) to the worker's agent-current extra, so the admitted image now
// imports all four.
var pythonUnsupportedTypes = []string{
	"aws", "azure", "azure_search", "bigquery", "delta_lake", "gcp",
	"kubernetes", "localgit", "yagmail",
}

func pinnedCatalogue(t *testing.T) *runtimecomposition.CurrentToolkitCatalogueSnapshot {
	t.Helper()
	catalogue, err := runtimecomposition.LoadPinnedCurrentToolkitCatalogueSnapshot()
	if err != nil {
		t.Fatalf("load pinned toolkit catalogue snapshot: %v", err)
	}
	return catalogue
}

func pinnedWorkerCapability(t *testing.T, implementation string) *runtimecomposition.WorkerToolkitCapability {
	t.Helper()
	capability, err := runtimecomposition.LoadPinnedWorkerToolkitCapability(implementation)
	if err != nil {
		t.Fatalf("load pinned %s worker toolkit capability: %v", implementation, err)
	}
	return capability
}

// catalogueOptions wires the handler the way the composition root does.
func catalogueOptions(t *testing.T, implementation string) []toolkits.Option {
	t.Helper()
	options := []toolkits.Option{
		toolkits.WithArgumentSchemas(pinnedSnapshot(t)),
		toolkits.WithSettingsDefinitions(pinnedSettingsDefinitions(t)),
		toolkits.WithCatalogue(pinnedCatalogue(t)),
	}
	if implementation != "" {
		options = append(options, toolkits.WithWorkerCapability(
			pinnedWorkerCapability(t, implementation),
		))
	}
	return options
}

func typeSchemaOf(t *testing.T, body map[string]any, toolkitType string) map[string]any {
	t.Helper()
	schema, ok := body[toolkitType].(map[string]any)
	if !ok {
		t.Fatalf("catalogue has no %q type; have %v", toolkitType, keysOf(body))
	}
	return schema
}

func metadataOf(t *testing.T, body map[string]any, toolkitType string) map[string]any {
	t.Helper()
	metadata, ok := typeSchemaOf(t, body, toolkitType)["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("%s carries no metadata object", toolkitType)
	}
	return metadata
}

// Every type the SDK snapshot holds is served, and the four elitea_core-native
// types survive beside them.
func TestToolkitTypeCatalogueServesEverySDKTypeAndTheNativeFour(t *testing.T) {
	t.Parallel()

	body := getToolkitTypeCatalogue(t, catalogueOptions(t, "python")...)

	want := map[string]struct{}{}
	for _, toolkitType := range pinnedCatalogue(t).ToolkitTypes() {
		want[toolkitType] = struct{}{}
	}
	for _, toolkitType := range nativeToolkitTypes {
		want[toolkitType] = struct{}{}
	}
	if len(body) != len(want) {
		t.Errorf("served %d types, want %d", len(body), len(want))
	}
	for toolkitType := range want {
		if _, served := body[toolkitType]; !served {
			t.Errorf("type %q is in the catalogue and is not served", toolkitType)
		}
	}
	for toolkitType := range body {
		if _, expected := want[toolkitType]; !expected {
			t.Errorf("type %q is served and is in neither source", toolkitType)
		}
	}
	// The number is stated so that a snapshot regeneration that silently drops
	// half the registry fails here rather than shrinking the create page.
	if len(body) < 52 {
		t.Errorf("served %d types; the pinned SDK revision holds 52 plus five native ones", len(body))
	}
}

// Table-driven over EVERY served type: a schema, a metadata block, and a
// capability verdict. A percentage cannot express "every type is covered"; this
// enumeration can, and it fails when a new type arrives without one of the
// three.
func TestEveryServedTypeHasSchemaMetadataAndCapabilityVerdict(t *testing.T) {
	t.Parallel()

	body := getToolkitTypeCatalogue(t, catalogueOptions(t, "python")...)
	catalogue := pinnedCatalogue(t)
	unsupported := map[string]struct{}{}
	for _, toolkitType := range pythonUnsupportedTypes {
		unsupported[toolkitType] = struct{}{}
	}

	for _, toolkitType := range sortedKeys(body) {
		t.Run(toolkitType, func(t *testing.T) {
			schema := typeSchemaOf(t, body, toolkitType)
			if schema["type"] != "object" {
				t.Errorf("type=%#v, want %q", schema["type"], "object")
			}
			properties, ok := schema["properties"].(map[string]any)
			if !ok || len(properties) == 0 {
				t.Fatalf("%s serves no settings properties, so its create form is empty", toolkitType)
			}
			metadata := metadataOf(t, body, toolkitType)

			_, catalogued := catalogue.ToolkitImportKey(toolkitType)
			if !catalogued {
				// A native type carries a label and no verdict.
				if label, _ := metadata["label"].(string); label == "" {
					t.Errorf("native type %s carries no label", toolkitType)
				}
				if _, present := metadata["unavailable"]; present {
					t.Errorf("native type %s carries a worker verdict it cannot have", toolkitType)
				}
				return
			}

			hidden, _ := metadata["hidden"].(bool)
			label, _ := metadata["label"].(string)
			reason, _ := metadata["unavailable_reason"].(string)
			unavailable, _ := metadata["unavailable"].(bool)

			if _, refused := unsupported[toolkitType]; refused {
				if !unavailable || !hidden || reason == "" {
					t.Errorf(
						"%s cannot be built by the admitted worker and is served hidden=%v unavailable=%v reason=%q",
						toolkitType, hidden, unavailable, reason,
					)
				}
				// Refused, and still SERVED: the admin surface has to be able
				// to tell a withheld type from one the platform never had.
				if _, served := body[toolkitType]; !served {
					t.Errorf("%s was dropped instead of served with a reason", toolkitType)
				}
				return
			}
			// A supported type is either labelled, or withheld for having no
			// label at all. mcp_config is the one unlabelled type: the
			// reference chooser shows no tile for it, so neither does this one.
			if label == "" {
				if !unavailable || !hidden || reason == "" {
					t.Errorf("%s carries no label and is offered as a tile", toolkitType)
				}
				return
			}
			if unavailable {
				t.Errorf("%s is buildable and is marked unavailable: %q", toolkitType, reason)
			}
		})
	}
}

// Absence is not unavailability. A deployment that has not said which worker it
// runs keeps the whole catalogue rather than losing every tile.
func TestCatalogueWithoutAWorkerCapabilityOffersEveryType(t *testing.T) {
	t.Parallel()

	body := getToolkitTypeCatalogue(t, catalogueOptions(t, "")...)

	for _, toolkitType := range pythonUnsupportedTypes {
		metadata := metadataOf(t, body, toolkitType)
		if _, present := metadata["unavailable"]; present {
			t.Errorf("%s is marked unavailable with no capability source configured", toolkitType)
		}
	}
	if len(body) < 52 {
		t.Errorf("served %d types with no capability source", len(body))
	}
}

// The Python image installs a measured subset of elitea-sdk[all]. These nine
// types are in the registry and raise at import, so they fail at the first
// tool call rather than at create time — which is why they are withheld.
func TestPythonWorkerCapabilityWithholdsTheTypesItCannotImport(t *testing.T) {
	t.Parallel()

	body := getToolkitTypeCatalogue(t, catalogueOptions(t, "python")...)

	withheld := []string{}
	for toolkitType := range body {
		metadata := metadataOf(t, body, toolkitType)
		if unavailable, _ := metadata["unavailable"].(bool); unavailable {
			if reason, _ := metadata["unavailable_reason"].(string); reason != "" &&
				toolkitType != "mcp_config" {
				withheld = append(withheld, toolkitType)
			}
		}
	}
	sort.Strings(withheld)
	if !equalStrings(withheld, pythonUnsupportedTypes) {
		t.Errorf("withheld %v, want %v", withheld, pythonUnsupportedTypes)
	}

	// The reason names the toolkit the operator has to act on, and names the
	// IMPORT KEY, because that is the name the failure is recorded under.
	reason, _ := metadataOf(t, body, "kubernetes")["unavailable_reason"].(string)
	if !contains(reason, "k8s") {
		t.Errorf("kubernetes reason=%q, want it to name the k8s import key", reason)
	}
}

// The Rust worker materializes twenty-two families and SKIPS everything else
// with a warning, so an unsupported toolkit attaches to an agent and does
// nothing. Under that worker the catalogue is a different shape.
func TestRustWorkerCapabilityWithholdsTheFamiliesItCannotMaterialize(t *testing.T) {
	t.Parallel()

	body := getToolkitTypeCatalogue(t, catalogueOptions(t, "rust")...)

	for _, toolkitType := range []string{"github", "openapi", "sharepoint", "sql"} {
		metadata := metadataOf(t, body, toolkitType)
		if unavailable, _ := metadata["unavailable"].(bool); unavailable {
			t.Errorf("%s is a native family and is withheld", toolkitType)
		}
	}
	for _, toolkitType := range []string{"jira", "confluence", "bitbucket", "artifact"} {
		metadata := metadataOf(t, body, toolkitType)
		if unavailable, _ := metadata["unavailable"].(bool); !unavailable {
			t.Errorf("%s has no native family and is offered as creatable", toolkitType)
		}
	}
	// The k8s/kubernetes split again, from the other side: materialize.rs
	// matches the string "k8s", which no platform type equals, so the native
	// worker cannot in fact run the `kubernetes` type either.
	metadata := metadataOf(t, body, "kubernetes")
	if unavailable, _ := metadata["unavailable"].(bool); !unavailable {
		t.Error("kubernetes is offered under the native worker, whose match arm is \"k8s\"")
	}
}

// imagegen is hand-written (its credential is the project's own
// image_generation model, not a pinned-snapshot settings shape) but it IS a
// real, worker-implemented toolkit (elitea_sdk/tools/imagegen, #864) — unlike
// the other four hand-written types, it must still carry a genuine worker
// verdict: buildable under Python, hidden under Rust (no Rust family exists
// for it), and offered with no verdict when no capability source is wired.
func TestImageGenIsHandWrittenButStillWorkerGated(t *testing.T) {
	t.Parallel()

	python := getToolkitTypeCatalogue(t, catalogueOptions(t, "python")...)
	pythonMetadata := metadataOf(t, python, "imagegen")
	if unavailable, _ := pythonMetadata["unavailable"].(bool); unavailable {
		t.Errorf("imagegen is Python-buildable and is marked unavailable: %#v", pythonMetadata)
	}

	rust := getToolkitTypeCatalogue(t, catalogueOptions(t, "rust")...)
	rustMetadata := metadataOf(t, rust, "imagegen")
	hidden, _ := rustMetadata["hidden"].(bool)
	unavailable, _ := rustMetadata["unavailable"].(bool)
	reason, _ := rustMetadata["unavailable_reason"].(string)
	if !hidden || !unavailable || reason == "" {
		t.Errorf(
			"imagegen has no Rust toolkit family and should be served hidden=true unavailable=true with a reason; got hidden=%v unavailable=%v reason=%q",
			hidden, unavailable, reason,
		)
	}
	if !contains(reason, "imagegen") {
		t.Errorf("imagegen reason=%q, want it to name the imagegen type", reason)
	}

	unset := getToolkitTypeCatalogue(t, catalogueOptions(t, "")...)
	unsetMetadata := metadataOf(t, unset, "imagegen")
	if _, present := unsetMetadata["unavailable"]; present {
		t.Errorf("imagegen is marked unavailable with no capability source configured: %#v", unsetMetadata)
	}
}

// The four hand-written types keep their hand-written schemas. They carry
// client contract the SDK model does not, and three of the four are not SDK
// toolkits at all.
func TestHandWrittenTypesSurviveTheSDKCatalogue(t *testing.T) {
	t.Parallel()

	body := getToolkitTypeCatalogue(t, catalogueOptions(t, "python")...)

	for _, toolkitType := range nativeToolkitTypes {
		schema := typeSchemaOf(t, body, toolkitType)
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s serves no properties", toolkitType)
		}
		if _, ok := properties["selected_tools"]; !ok {
			t.Errorf("%s serves no selected_tools", toolkitType)
		}
	}

	// openapi is BOTH hand-written and an SDK type. The hand-written entry
	// wins: its spec description states that a URL is not fetched, and its
	// ui_component picks the specification editor.
	spec, ok := typeSchemaOf(t, body, "openapi")["properties"].(map[string]any)["spec"].(map[string]any)
	if !ok {
		t.Fatal("openapi serves no spec property")
	}
	if spec["ui_component"] != "openapi_spec" {
		t.Errorf("openapi spec ui_component=%#v, want openapi_spec", spec["ui_component"])
	}
	if description, _ := spec["description"].(string); !contains(description, "A URL is not fetched") {
		t.Errorf("openapi spec description=%q lost its statement about URLs", description)
	}
	// And its hand-written metadata survives the merge, with the reference
	// deployment's two injected fields added beneath it.
	metadata := metadataOf(t, body, "openapi")
	if metadata["label"] != "OpenAPI" || metadata["icon_url"] != "openapi.svg" {
		t.Errorf("openapi metadata lost its hand-written label or icon: %#v", metadata)
	}
	if _, present := metadata["check_connection_supported"]; !present {
		t.Error("openapi metadata carries no check_connection_supported")
	}
}

// name_required tells the create form whether to ask for a toolkit name. The
// reference derives it from the SDK's toolkit_name annotation. It is added only
// to types the SDK defines and the hand-written map does not, so the four
// working forms are unchanged.
func TestNameRequiredIsServedForSDKTypesOnly(t *testing.T) {
	t.Parallel()

	body := getToolkitTypeCatalogue(t, catalogueOptions(t, "python")...)

	// confluence declares no toolkit_name field, so a name is required.
	if required, _ := typeSchemaOf(t, body, "confluence")["name_required"].(bool); !required {
		t.Error("confluence does not require a toolkit name")
	}
	// elastic and keycloak are the two types at this SDK revision that name
	// themselves from a settings field.
	for _, toolkitType := range []string{"elastic", "keycloak"} {
		required, present := typeSchemaOf(t, body, toolkitType)["name_required"].(bool)
		if !present || required {
			t.Errorf("%s name_required=%v, want false: it takes its name from a settings field",
				toolkitType, required)
		}
	}
	// custom is hand-written and keeps exactly what it served before.
	if _, present := typeSchemaOf(t, body, "custom")["name_required"]; present {
		t.Error("custom gained a name_required key, which changes a working create form")
	}
}

// Guardrails run last and stay terminal. A type the operator blocked must not
// come back because the worker can build it.
func TestGuardrailsRemoveAServedTypeTheWorkerSupports(t *testing.T) {
	t.Parallel()

	blocked := &guardrailSourceStub{policy: guardrails.NewPolicy(guardrails.PolicyInput{
		BlockedToolkits: []string{"github", "confluence"},
	})}
	options := append(catalogueOptions(t, "python"), toolkits.WithGuardrails(blocked))

	body := getToolkitTypeCatalogue(t, options...)

	for _, toolkitType := range []string{"github", "confluence"} {
		if _, served := body[toolkitType]; served {
			t.Errorf("%s is blocked by guardrails and is still served", toolkitType)
		}
	}
	if _, served := body["jira"]; !served {
		t.Error("jira is not blocked and was removed")
	}
}

// The catalogue is package-level state shared by every request. Serving it must
// not write to it.
func TestServingTheCatalogueDoesNotMutateSharedState(t *testing.T) {
	t.Parallel()

	options := catalogueOptions(t, "python")
	first := getToolkitTypeCatalogue(t, options...)

	// Write into the served tree the way a caller might.
	metadataOf(t, first, "github")["label"] = "tampered"
	typeSchemaOf(t, first, "artifact")["properties"] = map[string]any{}

	second := getToolkitTypeCatalogue(t, options...)
	if label, _ := metadataOf(t, second, "github")["label"].(string); label != "GitHub" {
		t.Errorf("github label=%q after a caller wrote to a previous response", label)
	}
	if properties, _ := typeSchemaOf(t, second, "artifact")["properties"].(map[string]any); len(properties) == 0 {
		t.Error("artifact lost its properties after a caller wrote to a previous response")
	}
}

// The types that can build an index are the ones whose tool list carries
// index_data. Before the catalogue was served, that was artifact and datasource
// alone, out of the seventeen indexing families the worker image build-gates.
func TestIndexCapableTypesCoverTheIndexingFamilies(t *testing.T) {
	t.Parallel()

	body := getToolkitTypeCatalogue(t, catalogueOptions(t, "python")...)

	indexing := []string{
		"ado_plans", "ado_repos", "ado_wiki", "bitbucket", "confluence",
		"figma", "github", "gitlab", "jira", "qtest", "sharepoint",
		"testrail", "xray_cloud", "zephyr_enterprise", "zephyr_essential",
		"zephyr_scale",
	}
	for _, toolkitType := range indexing {
		schema := selectedToolsSchema(t, body, toolkitType)
		args, ok := schema["args_schemas"].(map[string]any)
		if !ok {
			t.Errorf("%s serves no args_schemas", toolkitType)
			continue
		}
		if _, ok := args["index_data"]; !ok {
			t.Errorf("%s is an indexing family and serves no index_data tool", toolkitType)
		}
	}
}

// The web unit tests group the chooser from a committed copy of THIS
// catalogue's metadata. A fixture nobody compares with the server becomes a
// second, quieter source of truth: the chooser tests keep passing while the
// server serves something else. This test is the comparison.
//
// It reads the fixture the web suite imports and asserts, for every served
// type, that the label, categories and hidden flag are the ones this handler
// produces. Regenerate the fixture from the served catalogue when it fails —
// never edit it by hand to match.
func TestTheWebChooserFixtureMatchesTheServedCatalogue(t *testing.T) {
	t.Parallel()

	root := repositoryRootFromTest(t)
	raw, err := os.ReadFile(filepath.Join(
		root, "apps", "elitea-web", "src", "entities", "toolkit", "model",
		"__fixtures__", "servedToolkitTypeCatalogue.json",
	))
	if err != nil {
		t.Fatalf("read the web chooser fixture: %v", err)
	}
	var fixture map[string]struct {
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode the web chooser fixture: %v", err)
	}

	body := getToolkitTypeCatalogue(t, catalogueOptions(t, "python")...)
	if len(fixture) != len(body) {
		t.Fatalf("the fixture holds %d types and the catalogue serves %d", len(fixture), len(body))
	}
	for toolkitType, entry := range fixture {
		served, ok := body[toolkitType]
		if !ok {
			t.Errorf("the fixture holds %q, which is not served", toolkitType)
			continue
		}
		metadata, _ := served.(map[string]any)["metadata"].(map[string]any)
		for _, key := range []string{"label", "hidden", "unavailable_reason"} {
			want, wanted := entry.Metadata[key]
			got, gotten := metadata[key]
			if wanted != gotten || (wanted && !reflect.DeepEqual(want, got)) {
				t.Errorf("%s metadata[%q]: fixture=%#v served=%#v", toolkitType, key, want, got)
			}
		}
		if !reflect.DeepEqual(entry.Metadata["categories"], metadata["categories"]) {
			t.Errorf("%s categories: fixture=%#v served=%#v",
				toolkitType, entry.Metadata["categories"], metadata["categories"])
		}
	}
}

func repositoryRootFromTest(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for depth := 0; depth < 8; depth++ {
		if _, err := os.Stat(filepath.Join(directory, "go.work")); err == nil {
			return directory
		}
		directory = filepath.Dir(directory)
	}
	t.Fatal("repository root not found from the test working directory")
	return ""
}

func sortedKeys(body map[string]any) []string {
	names := make([]string, 0, len(body))
	for name := range body {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 ||
		indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return index
		}
	}
	return -1
}
