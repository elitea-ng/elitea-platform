package toolkits

// Provider-hub descriptors, projected as toolkit types.
//
// This is the port of `elitea_core/methods/provider_hub_schemas.py`'s
// `prepare_provider_toolkits`, `prettify_title` and `convert_args_schema`. It is
// the layer that finally makes an admitted provider's toolkits — DeepWiki's
// wikis, Inventory's knowledge graph, an image generator — appear in the
// toolkit chooser. The admission plane, the administration page and the
// registrar all landed already; the catalogue projection was the missing half,
// which is why a registered provider was visible to an operator and invisible
// to a user.
//
// # Two of pylon's mistakes are deliberately not copied
//
//  1. pylon probes `service_location_url` from the server on every catalogue
//     read. Nothing here dials a provider. The origin is reported as metadata
//     and never fetched, so a catalogue read cannot be turned into a
//     server-side request to an attacker-named host.
//  2. pylon drops an unhealthy provider out of the catalogue entirely, so a
//     toolkit a user already configured silently stops being offered. Health is
//     not consulted here at all. Admission decides, and admission is a recorded
//     decision with a reason.
//
// # What admission allows
//
// The rule mirrors `internal/providerhost/admission/gate.go`'s `resolve`,
// because a catalogue that offered a type the request path refuses would create
// a toolkit that can never run:
//
//	revoked   → never projected, in either posture.
//	active    → always projected.
//	inactive  → projected only under the `record` posture, flagged in metadata.
//
// Under `enforce`, an inactive provider's toolkits disappear from the chooser,
// which is the same answer the gate gives its callers.

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/facade"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhub"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/publicproject"
)

// ProviderManifestSource reads the admitted manifests of one project.
//
// An interface so the projection is testable from a table of manifests without
// a database. The pool-backed implementation is `providerhub.AdmittedManifests`.
type ProviderManifestSource interface {
	AdmittedManifests(ctx context.Context) ([]providerhub.AdmittedManifest, error)
}

// providerHubProjection projects admitted descriptors into toolkit types.
type providerHubProjection struct {
	manifests ProviderManifestSource
	posture   facade.AdmissionPosture
}

// poolManifestSource reads the public project's admitted manifests.
//
// The PUBLIC project, not the caller's: a facade registers itself under the
// deployment's public project at boot (`cmd/elitea-main/main.go` passes
// `PublicProjectID` into `startProviderRegistrar`), so a reader keyed on the
// caller's project would query rows nothing ever writes. This is the same
// project-union rule pylon expresses as `expand_project_ids`, reduced to the
// one member that this service actually writes.
type poolManifestSource struct {
	pool      *pgxpool.Pool
	projectID int64
}

func (s poolManifestSource) AdmittedManifests(ctx context.Context) ([]providerhub.AdmittedManifest, error) {
	if s.pool == nil {
		return nil, nil
	}
	// A deployment that has not applied migration 0107 has no admission plane
	// at all. Asking is cheaper than letting every request fail with an
	// undefined-table error, and "the plane is absent" is a correct empty
	// answer rather than a fault — the same distinction `admission.PoolStore`
	// makes.
	if !providerhub.Present(ctx, s.pool) {
		return nil, nil
	}
	return providerhub.AdmittedManifests(ctx, s.pool, s.projectID)
}

// newProviderHubProjection builds the projection over a pool.
func newProviderHubProjection(pool *pgxpool.Pool) *providerHubProjection {
	posture, err := facade.AdmissionPostureFromEnv(os.LookupEnv)
	if err != nil {
		// main.go refuses to start on an unreadable posture, so this cannot be
		// reached in a running binary. Recording is still cheaper than assuming:
		// the closed answer is `enforce`, which projects fewer types, never more.
		posture = facade.AdmissionEnforce
	}
	return &providerHubProjection{
		manifests: poolManifestSource{pool: pool, projectID: int64(publicproject.ID())},
		posture:   posture,
	}
}

func (p *providerHubProjection) Name() string { return "provider_hub" }

// ProjectToolkitTypes answers one type per admitted toolkit.
//
// A manifest that will not parse fails ONE provider, not the read: pylon wraps
// its own per-provider projection in a try/except for the same reason. The
// difference is that this records the failure as an error on the returned
// slice's behalf — the caller logs it — rather than swallowing it.
func (p *providerHubProjection) ProjectToolkitTypes(ctx context.Context) ([]ProjectedToolkitType, error) {
	if p == nil || p.manifests == nil {
		return nil, nil
	}
	manifests, err := p.manifests.AdmittedManifests(ctx)
	if err != nil {
		return nil, err
	}
	projected := make([]ProjectedToolkitType, 0, len(manifests))
	var failures []string
	for _, manifest := range manifests {
		if !admissionAllowsProjection(manifest.Status, p.posture) {
			continue
		}
		descriptor, err := providerhub.ParseDescriptor(manifest.Manifest)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", manifest.ProviderID, err))
			continue
		}
		projected = append(projected, projectProviderToolkits(manifest, descriptor)...)
	}
	if len(failures) > 0 && len(projected) == 0 {
		return nil, fmt.Errorf("provider manifests unreadable: %s", strings.Join(failures, "; "))
	}
	return projected, nil
}

// admissionAllowsProjection mirrors admission.Gate's resolve.
func admissionAllowsProjection(status string, posture facade.AdmissionPosture) bool {
	switch status {
	case "revoked":
		return false
	case "active":
		return true
	default:
		return posture != facade.AdmissionEnforce
	}
}

// projectProviderToolkits is `prepare_provider_toolkits` for one descriptor.
//
// The provider name used to compose a type name is the DESCRIPTOR's own name,
// which is also the `provider_id` the registrar files the registration under
// (`registrar.go` reads `descriptor["name"]`). Using the registration's id
// instead would be the same string, but reading it from the document keeps the
// projection a pure function of the manifest.
func projectProviderToolkits(
	manifest providerhub.AdmittedManifest,
	descriptor providerhub.Descriptor,
) []ProjectedToolkitType {
	providerName := descriptor.Name
	projected := make([]ProjectedToolkitType, 0, len(descriptor.ProvidedToolkits))
	seen := make(map[string]struct{}, len(descriptor.ProvidedToolkits))

	for _, toolkit := range descriptor.ProvidedToolkits {
		if toolkit.Name == "" {
			continue
		}
		typeName := toolkit.TypeOverride()
		if typeName == "" {
			typeName = providerName + "_" + toolkit.Name
		}
		// pylon's `if name in result: continue` — the first declaration of a
		// type name wins.
		if _, taken := seen[typeName]; taken {
			continue
		}
		seen[typeName] = struct{}{}
		projected = append(projected, ProjectedToolkitType{
			Type:   typeName,
			Schema: providerToolkitSchema(manifest, providerName, typeName, toolkit),
		})
	}
	return projected
}

func providerToolkitSchema(
	manifest providerhub.AdmittedManifest,
	providerName, typeName string,
	toolkit providerhub.ToolkitDescriptor,
) map[string]any {
	properties, required := providerToolkitSettings(toolkit.ToolkitConfig)

	toolNames := make([]any, 0, len(toolkit.ProvidedTools))
	argsSchemas := make(map[string]any, len(toolkit.ProvidedTools))
	for _, tool := range toolkit.ProvidedTools {
		if tool.Name == "" {
			continue
		}
		toolNames = append(toolNames, tool.Name)
		argsSchemas[tool.Name] = convertArgsSchema(tool.ArgsSchema, tool.Name, tool.Describe())
	}

	properties["selected_tools"] = map[string]any{
		"type":         "array",
		"title":        "Selected Tools",
		"items":        map[string]any{"type": "string", "enum": toolNames},
		"default":      []any{},
		"args_schemas": argsSchemas,
	}
	// pylon writes FOUR hidden routing fields; two are reproduced here and two
	// are deliberately not.
	//
	// `provider` and `toolkit` name WHICH provider and WHICH of its toolkits a
	// stored row belongs to. They are the routing facts, they are what a worker
	// needs to reach the right endpoint, and a saved row without them cannot be
	// routed at all.
	//
	// `module` (`plugins.provider_worker.utils.tools`) and `class` (`Toolkit`)
	// are NOT reproduced. They are a Python import path into a pylon plugin
	// that this service does not load and will not load — AGENTS.md forbids
	// carrying plugin loading into the target architecture. Writing them would
	// put a dead import path in every stored toolkit and would read as a
	// working control.
	properties["provider"] = map[string]any{
		"type": "string", "title": "Provider", "description": "Toolkit provider",
		"default": providerName, "hidden": true,
	}
	properties["toolkit"] = map[string]any{
		"type": "string", "title": "Toolkit", "description": "Toolkit",
		"default": toolkit.Name, "hidden": true,
	}

	metadata := map[string]any{
		"icon_url": nil,
		"label":    toolkit.Name,
	}
	// The descriptor's own metadata wins, exactly as pylon's `**toolkit_metadata`
	// spread does: a provider may state its own label, icon or interface.
	for key, value := range toolkit.ToolkitMetadata {
		metadata[key] = value
	}
	// Provenance the operator can see. `admission_status` is the honest half of
	// projecting an inactive revision under the `record` posture: the type is
	// offered, and the catalogue says the decision behind it is not in force.
	metadata["provider_name"] = providerName
	metadata["provider_origin"] = manifest.Origin
	metadata["admission_status"] = manifest.Status
	metadata["admitted_revision"] = manifest.RevisionID

	return map[string]any{
		"type":          "object",
		"title":         typeName,
		"name_required": true,
		"description":   toolkit.Describe(),
		"metadata":      metadata,
		"properties":    properties,
		"required":      required,
	}
}

// providerToolkitSettings converts one toolkit_config into JSON Schema
// properties, honouring fields_order.
func providerToolkitSettings(
	config providerhub.ToolkitConfigDescriptor,
) (map[string]any, []any) {
	properties := make(map[string]any, len(config.Parameters)+6)
	required := make([]any, 0, len(config.Parameters))

	for _, key := range orderedParameterKeys(config) {
		parameter := config.Parameters[key]
		propertyKey := providerParameterPrefix + key
		properties[propertyKey] = providerParameterSchema(key, parameter)
		if parameter.IsRequired() {
			required = append(required, propertyKey)
		}
	}
	return properties, required
}

// providerParameterPrefix is pylon's `f"toolkit_configuration_{key}"`. The
// prefix is part of the stored settings shape, so it is not an implementation
// detail this port may drop.
const providerParameterPrefix = "toolkit_configuration_"

// orderedParameterKeys applies fields_order, then appends what it did not name.
//
// Map iteration in Go is randomised, so without this the served property order
// would differ between two reads of the same descriptor. pylon's `items()` is
// insertion-ordered; `fields_order` is how a provider states the order it wants.
func orderedParameterKeys(config providerhub.ToolkitConfigDescriptor) []string {
	ordered := make([]string, 0, len(config.Parameters))
	named := make(map[string]struct{}, len(config.FieldsOrder))
	for _, key := range config.FieldsOrder {
		if _, declared := config.Parameters[key]; !declared {
			continue
		}
		if _, taken := named[key]; taken {
			continue
		}
		named[key] = struct{}{}
		ordered = append(ordered, key)
	}
	remaining := make([]string, 0, len(config.Parameters))
	for key := range config.Parameters {
		if _, taken := named[key]; taken {
			continue
		}
		remaining = append(remaining, key)
	}
	sortStrings(remaining)
	return append(ordered, remaining...)
}

// providerParameterSchema is pylon's per-parameter branch.
func providerParameterSchema(key string, parameter providerhub.ParameterDescriptor) map[string]any {
	property := map[string]any{}
	declared := parameter.DeclaredType()

	switch declared {
	case "Text":
		property["type"] = "string"
		property["lines"] = 5
	case "String", "URL", "UUID":
		property["type"] = "string"
	case "Secret":
		property["type"] = "string"
		property["format"] = "password"
		property["secret"] = true
		property["writeOnly"] = true
	case "Integer", "Float":
		property["type"] = "integer"
	case "Bool":
		property["type"] = "boolean"
	case "JSON":
		if _, isList := parameter.Default.([]any); isList {
			property["type"] = "array"
		} else {
			property["type"] = "object"
		}
	default:
		property["type"] = "object"
	}
	property["title"] = prettifyTitle(key)
	if description := parameter.Describe(); description != "" {
		property["description"] = description
	}

	// json_schema_extra merges in WITHOUT overwriting a key already computed,
	// then flips the property to the client's `configuration` kind unless the
	// extra names a reference picker. Both rules are pylon's, in pylon's order:
	// the type flip runs after the merge and does clobber a `type` the merge
	// carried in.
	if len(parameter.JSONSchemaExtra) > 0 {
		for key, value := range parameter.JSONSchemaExtra {
			if _, present := property[key]; present {
				continue
			}
			property[key] = value
		}
		if !referencePickerExtra(parameter.JSONSchemaExtra) {
			property["type"] = "configuration"
		}
	}
	if parameter.Default != nil {
		property["default"] = parameter.Default
	}
	return property
}

// referencePickerExtra reports whether the extra names a reference picker,
// whose array semantics must survive the configuration flip.
func referencePickerExtra(extra map[string]any) bool {
	for _, marker := range [...]string{"toolkit_types", "agent_tags", "pipeline_tags"} {
		if _, present := extra[marker]; present {
			return true
		}
	}
	return false
}

// convertArgsSchema is pylon's `convert_args_schema`.
func convertArgsSchema(
	args map[string]providerhub.ArgDescriptor,
	title, description string,
) map[string]any {
	properties := make(map[string]any, len(args))
	required := make([]any, 0, len(args))

	names := make([]string, 0, len(args))
	for name := range args {
		names = append(names, name)
	}
	sortStrings(names)

	for _, name := range names {
		argument := args[name]
		property := map[string]any{
			"title":       prettifyTitle(name),
			"description": argumentDescription(argument, name),
		}
		switch argument.Type {
		case "String", "Text", "URL", "UUID", "Secret", "":
			property["type"] = "string"
		case "Integer":
			property["type"] = "integer"
		case "Float", "Number":
			property["type"] = "number"
		case "Boolean", "Bool":
			property["type"] = "boolean"
		case "List":
			property["type"] = "array"
			if len(argument.Enum) > 0 {
				property["items"] = map[string]any{"type": "string", "enum": argument.Enum}
			}
		default:
			property["type"] = "object"
		}
		if argument.HasDefault {
			property["default"] = argument.Default
		}
		applyNumericBounds(property, argument)
		properties[name] = property
		if argument.Required {
			required = append(required, name)
		}
	}

	schema := map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   required,
	}
	if title != "" {
		schema["title"] = title
	}
	if description != "" {
		schema["description"] = description
	}
	return schema
}

// argumentDescription falls back to the argument's own name, as pylon does.
func argumentDescription(argument providerhub.ArgDescriptor, name string) string {
	if argument.Description != "" {
		return argument.Description
	}
	return name
}

// applyNumericBounds maps pydantic's gt/ge/lt/le onto JSON Schema, for numeric
// properties only. A strict bound wins over a non-strict one in the same
// direction, which is pylon's own conflict rule.
func applyNumericBounds(property map[string]any, argument providerhub.ArgDescriptor) {
	kind, _ := property["type"].(string)
	if kind != "integer" && kind != "number" {
		return
	}
	switch {
	case argument.GreaterThan != nil:
		property["exclusiveMinimum"] = argument.GreaterThan
	case argument.GreaterEq != nil:
		property["minimum"] = argument.GreaterEq
	}
	switch {
	case argument.LessThan != nil:
		property["exclusiveMaximum"] = argument.LessThan
	case argument.LessEq != nil:
		property["maximum"] = argument.LessEq
	}
}

// titleAbbreviations is pylon's `_ABBREVIATIONS`.
var titleAbbreviations = map[string]struct{}{
	"api": {}, "url": {}, "id": {}, "uuid": {}, "http": {}, "https": {}, "uri": {},
	"sql": {}, "html": {}, "css": {}, "json": {}, "xml": {}, "jwt": {}, "oauth": {},
	"aws": {}, "gcp": {}, "ssh": {}, "ftp": {}, "smtp": {}, "ip": {}, "tcp": {},
	"udp": {}, "dns": {}, "ssl": {}, "tls": {}, "llm": {}, "ai": {},
}

var (
	lowerUpperBoundary = regexp.MustCompile(`([a-z])([A-Z])`)
	acronymBoundary    = regexp.MustCompile(`([A-Z]+)([A-Z][a-z])`)
)

// prettifyTitle is pylon's `prettify_title`: split camelCase and PascalCase,
// replace separators, upper-case a known abbreviation, capitalise the rest.
func prettifyTitle(name string) string {
	if name == "" {
		return name
	}
	spaced := lowerUpperBoundary.ReplaceAllString(name, "$1 $2")
	spaced = acronymBoundary.ReplaceAllString(spaced, "$1 $2")
	spaced = strings.NewReplacer("_", " ", "-", " ").Replace(spaced)

	words := strings.Fields(spaced)
	for index, word := range words {
		lowered := strings.ToLower(word)
		if _, known := titleAbbreviations[lowered]; known {
			words[index] = strings.ToUpper(word)
			continue
		}
		words[index] = capitalise(lowered)
	}
	return strings.Join(words, " ")
}

// capitalise is Python's `str.capitalize`: first rune upper, rest lower.
func capitalise(word string) string {
	if word == "" {
		return word
	}
	runes := []rune(word)
	return strings.ToUpper(string(runes[0])) + string(runes[1:])
}

// sortStrings keeps the projection deterministic. Go randomises map iteration,
// so a schema built by ranging over a map would differ between two reads of the
// same descriptor and would make any golden test flap.
func sortStrings(values []string) { slices.Sort(values) }
