package configurations

// Catalogue defaults for a saved toolkit's settings (#978).
//
// THE DEFECT. The SDK's toolkit models declare optional fields with a default
// — qTest's `no_of_tests_shown_in_dql_search` is `int` with `default: 10` — and
// the catalogue snapshot carries that default verbatim. Nothing on the write
// path applied it. A body that omitted the key was stored without it, and the
// key then reached the worker as JSON `null`, where the SDK's model types it
// `int` and refuses it:
//
//	1 validation error for QtestConfiguration
//	no_of_tests_shown_in_dql_search
//	  Input should be a valid integer [type=int_type, input_value=None]
//
// The toolkit therefore died at MATERIALIZATION — after the save succeeded,
// on the first turn that used it, with an empty errored answer and no field to
// point at. The form sends the defaults because it renders them; every other
// writer (the API, the MCP surface, an import) did not.
//
// WHAT IS FILLED, AND WHAT IS NOT. Only a TOP-LEVEL property that the schema
// declares a `default` for AND the body does not mention at all, whose default
// is a VALUE. Five deliberate exclusions:
//
//   - A key present with an explicit `null` is left alone. Many catalogue
//     fields are `anyOf: [{...}, {type: null}]` with a non-null default, so
//     null is a value there, not an omission — replacing it would overrule a
//     user who cleared the field.
//   - A default of `null` is not written. An absent key already reads as null
//     everywhere downstream, so writing one adds nothing — while a null on a
//     `configuration_types` field (`pgvector_configuration`, `embedding_model`
//     and their kin all default to null) is a REFERENCE the save-time resolver
//     would then be handed, turning a valid save into a refusal. Only defaults
//     with a value are filled.
//   - An empty list or object default is not written either, for the same
//     reason and one more: `selected_tools` defaults to `[]` on every SDK
//     type, and an empty list is read as "no restriction" by some consumers
//     and "nothing selected" by others. Absent keeps whatever each reader
//     already does with it; writing `[]` would pick a side on the write path
//     for every toolkit saved through the API.
//   - A REFERENCE-shaped property is never filled, whatever its default says.
//     `configuration_types`, `configuration_sections`, `toolkit_types`,
//     `configuration_model` and `secret: true` fields name a saved row that
//     the reference walker resolves (toolkit_settings.go); three catalogue
//     types (azure, gcp, keycloak) declare a secret field with a `""`
//     default, and writing that would put a literal empty value where a
//     sealed reference belongs.
//   - Nested objects are not walked. A `$ref`'d credential block is resolved
//     by the reference walker (toolkit_settings.go), not defaulted here, and
//     recursing into it would fabricate a credential shape nobody saved.
//   - A schema that is not an object, or a property that is not an object, is
//     skipped rather than rejected. This runs on the write path of a toolkit
//     whose schema may be a projection, and a malformed schema must not make
//     an otherwise valid save fail.

// MaxToolkitSettingsDefaultProperties bounds the properties one call reads. It
// is the same order of magnitude as MaxCurrentToolkitSettingsNodes and exists
// for the same reason: a schema arrives from a snapshot file, and no catalogue
// entry is anywhere near this size (the largest SDK type declares ~40).
const MaxToolkitSettingsDefaultProperties = 1_024

// ApplyToolkitSettingsDefaults writes the catalogue's declared defaults into
// settings for every top-level property the caller omitted, and reports how
// many keys it added.
//
// schema is one catalogue entry's SETTINGS schema (the map carrying
// "properties"). Both arguments may be nil; a nil settings map cannot be
// written to, so it is reported as zero additions rather than replaced.
func ApplyToolkitSettingsDefaults(schema map[string]any, settings map[string]any) int {
	if schema == nil || settings == nil {
		return 0
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok || len(properties) == 0 || len(properties) > MaxToolkitSettingsDefaultProperties {
		return 0
	}
	added := 0
	for name, raw := range properties {
		if name == "" || len(name) > MaxCurrentToolkitSettingsIdentifier {
			continue
		}
		if _, present := settings[name]; present {
			continue
		}
		property, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if isToolkitReferenceProperty(property) {
			continue
		}
		value, declared := property["default"]
		if !declared || isEmptyToolkitDefault(value) {
			continue
		}
		settings[name] = cloneToolkitDefault(value, 0)
		added++
	}
	return added
}

// isEmptyToolkitDefault reports whether a declared default carries nothing an
// absent key does not already carry: null, an empty string, an empty list, or
// an empty object.
func isEmptyToolkitDefault(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return typed == ""
	case []any:
		return len(typed) == 0
	case map[string]any:
		return len(typed) == 0
	default:
		return false
	}
}

// isToolkitReferenceProperty reports whether a property names a saved row —
// a credential, a nested toolkit, a model, or a sealed secret — rather than
// holding a plain value. The key names are the ones
// currentToolkitSettingsWalker.buildFieldPlans branches on; this function must
// keep listing all of them, because a reference this misses is a reference
// this would fabricate.
func isToolkitReferenceProperty(property map[string]any) bool {
	for _, key := range []string{
		"configuration_types",
		"configuration_sections",
		"toolkit_types",
		"configuration_model",
	} {
		if value, present := property[key]; present && value != nil {
			return true
		}
	}
	return property["secret"] == true
}

// cloneToolkitDefault copies a default so the stored settings never alias the
// catalogue snapshot, which is shared process-wide and read by every request.
// Depth is bounded by MaxCurrentToolkitSettingsDepth; anything deeper is
// dropped to nil rather than shared, because sharing is the failure this
// exists to prevent.
func cloneToolkitDefault(value any, depth int) any {
	if depth > MaxCurrentToolkitSettingsDepth {
		return nil
	}
	switch typed := value.(type) {
	case map[string]any:
		clone := make(map[string]any, len(typed))
		for key, entry := range typed {
			clone[key] = cloneToolkitDefault(entry, depth+1)
		}
		return clone
	case []any:
		clone := make([]any, len(typed))
		for index, entry := range typed {
			clone[index] = cloneToolkitDefault(entry, depth+1)
		}
		return clone
	default:
		return value
	}
}
