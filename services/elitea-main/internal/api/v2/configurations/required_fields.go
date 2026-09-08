// required_fields.go refuses a configuration CREATE whose body does not carry
// the fields the type's own schema declares as required.
//
// # The defect this closes
//
// The create route validated exactly two things: that a provider credential
// does not point at this platform (selfref.go) and that a declared password
// could be sealed (secret_sealing.go). Everything else was stored as sent. So
//
//	POST /configurations/configurations/{project}
//	{"elitea_title":"…","type":"pgvector"}
//
// answered 201 and wrote a row with an empty label and an empty `data`. The
// reference implementation answers 400 for that body, because it parses it
// into the registry's Pydantic model first, and the three fields that model
// requires are the three that row has none of.
//
// The row that gets written is not merely incomplete, it is a row that no
// reader can use and that no screen can explain:
//
//   - an llm-section row with no label is REJECTED by the model catalogue
//     reader as an error rather than skipped (infra/db/repos/models.go), so
//     ONE such row empties the whole catalogue for the project;
//   - a credential with no `data` names no endpoint, so every toolkit that
//     resolves it fails at the first call with a message about the provider;
//   - an embedding model with no `ai_credentials` link is refused admission,
//     which the user sees only as `status_ok = false` on a row that looks
//     complete.
//
// Each of those is discovered somewhere else, later, by somebody who did not
// write the row. A 400 naming the field is the same information, in the
// request that could still act on it.
//
// # The schema is the authority, not a hand-written list
//
// The registry snapshot every deployment already embeds
// (application/configurations/current_available_snapshot.json) carries, per
// type, the full ConfigurationCreateBase schema with that type's own data
// model substituted into its `data` property — including both `required`
// lists. Reading them is therefore the same rule the reference applies, and a
// type added to the snapshot arrives with its requirements already stated.
// A hand-written table would be a second, silently diverging answer to
// "what does an azure_open_ai credential need".
//
// # What it deliberately does NOT do
//
//   - It is not a JSON Schema validator. It checks PRESENCE of the required
//     keys, refuses an explicit null where the schema does not allow one, and
//     refuses a non-object where the schema requires an object (which is what
//     a malformed `{"ai_credentials": "some_title"}` reference is). It does
//     not check formats, patterns, enums or scalar types: those refusals would
//     be new behaviour of this route rather than the missing half of it.
//   - It does not walk INTO a `$ref`. `data` is an inline object schema and is
//     walked; a referenced model such as `AiCredentials` is checked for its
//     KIND and no further. The reason is measured rather than tidy: that model
//     declares `private` required, and this platform's own writers omit it —
//     the admin model-adoption dialog and this route's own model fixtures both
//     send `{"elitea_title": …}` — because the expander reads a missing
//     `private` as false (application/configurations/expand.go). Enforcing the
//     referenced model's own required list would refuse a reference the
//     platform resolves everywhere else, which is a new rule rather than a
//     missing one.
//   - It does not check the DATA of a row filed under a section that is not
//     the type's own. The data schema describes the type's own object, and
//     this route also serves compatibility shapes that deliberately are not
//     one: the web model picker's row is `section: "models"` with
//     `type: "open_ai"` and carries `{model, ai_credentials}` rather than an
//     OpenAI credential (deploy/scripts/seed-llm-api.py writes it). The four
//     top-level fields are still required there, because they are the row and
//     not the provider.
//   - On UPDATE it checks the `data` object of a MODEL row and nothing else.
//     The compatibility PUT is a PARTIAL write by contract (see
//     Handler.Update): a body that carries only `{"shared":true}` must not be
//     refused for the fields it deliberately omits. But the `data` COLUMN is
//     replaced whole, so a `data` object the body does carry is a complete
//     statement of that column — a field it omits is a field the stored row
//     loses. For the five model types that column is authored whole by
//     whoever writes it (a wire name, a credential link, a few flags, no
//     secret), so it is held to the type's own required list. A CREDENTIAL
//     row's is not: an untouched password is deliberately omitted on an edit
//     (`LlmProviderDraft`), and refusing that body would refuse the ordinary
//     edit of a working credential. See refuseIncompleteUpdatedModelData.
//   - It says nothing about a type the registry does not carry. An unknown
//     type has no schema, so there is nothing to be missing, and the route
//     keeps storing it exactly as before.
package configurations

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// The two depths the walk runs at.
//
// requiredFieldDepthRow checks the four top-level fields and nothing inside
// them. requiredFieldDepthData additionally checks the type's own `data`
// contract; it is a bound as well as a mode, so a future snapshot with a
// recursive `$ref` cannot make this function spin.
const (
	requiredFieldDepthRow  = 1
	requiredFieldDepthData = 4
)

// configurationTitleAliases are the two keys that satisfy `elitea_title`.
//
// The stored column is written with `firstStrVal(body, "elitea_title", "name")`
// — `name` is the accepted legacy alias — so a body that carries only `name`
// stores a title and must not be refused for missing one. The schema knows
// nothing of the alias, which is why it is stated here beside the reader that
// applies it.
var configurationTitleAliases = []string{"elitea_title", "name"}

// missingRequiredConfigurationFields returns the dotted paths of the required
// fields a create body does not satisfy, in a stable order.
//
// An empty result means the body carries everything the schema names. A nil
// or unreadable schema also returns none: this function refuses a body only on
// evidence, never because it could not read the rule.
func missingRequiredConfigurationFields(
	rawSchema json.RawMessage,
	body map[string]any,
	maxDepth int,
) []string {
	if len(rawSchema) == 0 || body == nil {
		return nil
	}
	var schema map[string]any
	if err := json.Unmarshal(rawSchema, &schema); err != nil {
		return nil
	}
	missing := missingObjectFields(schema, body, "", definitionsOf(schema, nil), 0, maxDepth)
	sort.Strings(missing)
	return missing
}

// missingObjectFields walks one object schema against one object value.
func missingObjectFields(
	schema map[string]any,
	value map[string]any,
	path string,
	defs map[string]any,
	depth int,
	maxDepth int,
) []string {
	required := stringsOf(schema["required"])
	if len(required) == 0 || depth >= maxDepth {
		return nil
	}
	properties, _ := schema["properties"].(map[string]any)

	missing := make([]string, 0, len(required))
	for _, name := range required {
		full := path + name
		raw, present := lookupRequiredValue(value, name, path)
		if !present {
			missing = append(missing, full)
			continue
		}
		property, _ := properties[name].(map[string]any)
		if raw == nil {
			// An explicit null satisfies a field whose schema admits one — the
			// embedding model's `ai_credentials` is declared that way — and
			// fails one that does not.
			if !nullAllowed(property) {
				missing = append(missing, full)
			}
			continue
		}
		object, inline, isObject := objectSchemaFor(property, defs)
		if !isObject {
			continue
		}
		child, ok := raw.(map[string]any)
		if !ok {
			// The field is there and is not the kind of thing the schema
			// describes. A credential reference written as the bare title
			// string rather than as `{elitea_title, private}` is this case,
			// and storing it makes a row that resolves for nobody.
			missing = append(missing, full)
			continue
		}
		if !inline {
			continue
		}
		missing = append(missing,
			missingObjectFields(object, child, full+".", definitionsOf(object, defs), depth+1, maxDepth)...)
	}
	if len(missing) == 0 {
		return nil
	}
	return missing
}

// lookupRequiredValue reads one required field, honouring the title alias at
// the top level only. `data.name` is a provider's own field and has no alias.
func lookupRequiredValue(value map[string]any, name, path string) (any, bool) {
	if raw, present := value[name]; present {
		return raw, true
	}
	if path != "" || name != configurationTitleAliases[0] {
		return nil, false
	}
	for _, alias := range configurationTitleAliases[1:] {
		if raw, present := value[alias]; present {
			return raw, true
		}
	}
	return nil, false
}

// objectSchemaFor resolves a property schema to the object schema it describes,
// following a `$ref` into `$defs` and looking inside an `anyOf` for the one
// member that is an object. It reports false for anything that is not an
// object contract, which is every scalar field.
//
// `inline` distinguishes a schema written out in this type's own document from
// one reached through a `$ref`. Only an inline schema is walked further — see
// the package comment for the measured reason.
func objectSchemaFor(property map[string]any, defs map[string]any) (object map[string]any, inline bool, ok bool) {
	if property == nil {
		return nil, false, false
	}
	if reference, isReference := property["$ref"].(string); isReference {
		definition, resolved := resolveDefinition(reference, defs)
		return definition, false, resolved
	}
	if declared, isTyped := property["type"].(string); isTyped && declared == "object" {
		if _, hasProperties := property["properties"].(map[string]any); hasProperties {
			return property, true, true
		}
		return nil, false, false
	}
	for _, member := range anyOfMembers(property) {
		if resolved, memberInline, memberOK := objectSchemaFor(member, defs); memberOK {
			return resolved, memberInline, true
		}
	}
	return nil, false, false
}

// resolveDefinition follows a local `#/$defs/Name` pointer. Any other pointer
// form is unresolvable here and is treated as "no object contract", which
// leaves the field checked for presence alone.
func resolveDefinition(reference string, defs map[string]any) (map[string]any, bool) {
	const prefix = "#/$defs/"
	if !strings.HasPrefix(reference, prefix) || defs == nil {
		return nil, false
	}
	definition, ok := defs[strings.TrimPrefix(reference, prefix)].(map[string]any)
	return definition, ok
}

// definitionsOf returns the `$defs` a schema declares, or the inherited ones.
// The pinned snapshot puts `$defs` on the DATA sub-schema rather than on the
// document root, so the walk carries the nearest set down with it.
func definitionsOf(schema map[string]any, inherited map[string]any) map[string]any {
	if defs, ok := schema["$defs"].(map[string]any); ok {
		return defs
	}
	return inherited
}

// nullAllowed reports whether the schema admits an explicit JSON null.
func nullAllowed(property map[string]any) bool {
	if property == nil {
		return true
	}
	if declared, ok := property["type"].(string); ok {
		return declared == "null"
	}
	for _, member := range anyOfMembers(property) {
		if declared, ok := member["type"].(string); ok && declared == "null" {
			return true
		}
	}
	// A property with neither a type nor an anyOf states no shape at all, so
	// there is nothing for a null to contradict.
	_, hasAnyOf := property["anyOf"]
	_, hasRef := property["$ref"]
	return !hasAnyOf && !hasRef
}

func anyOfMembers(property map[string]any) []map[string]any {
	raw, ok := property["anyOf"].([]any)
	if !ok {
		return nil
	}
	members := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		if member, ok := entry.(map[string]any); ok {
			members = append(members, member)
		}
	}
	return members
}

func stringsOf(raw any) []string {
	entries, ok := raw.([]any)
	if !ok {
		return nil
	}
	values := make([]string, 0, len(entries))
	for _, entry := range entries {
		if value, ok := entry.(string); ok && value != "" {
			values = append(values, value)
		}
	}
	return values
}

// requiredConfigurationFieldsMessage is the refusal the route answers. It names
// every missing path, because a client that fixes one field at a time learns
// the next one only by trying again.
func requiredConfigurationFieldsMessage(missing []string) string {
	return "the configuration is missing required fields: " + strings.Join(missing, ", ")
}

// refuseIncompleteUpdatedModelData applies the create's required-field rule to
// the `data` object of a partial UPDATE, for the five MODEL types alone.
//
// # The state this closes
//
// A platform model whose `ai_credentials` link an update dropped is stored and
// listed. It is not served: the registry declares the link required, so
// provider admission refuses the row and writes `status_ok = false`, and every
// reader — the model catalogue, the tier defaults, the LLM gateway — selects on
// `status_ok = true`. The operator who cleared it sees a 200 and a row that
// looks complete on the only screen that shows it.
//
// The create already answers 400 for that body. Leaving the update lenient made
// the same row reachable in two steps instead of one.
//
// # Why the scope is the model types
//
// The `data` column is replaced whole by this route, so a present `data` object
// states the whole column. For a model that object is authored whole as well —
// a wire name, a credential link and a few boolean flags, no secret — so the
// create's contract applies to it unchanged. For a CREDENTIAL it is not: a
// secret the operator did not retype is deliberately absent from an edit body
// (see `LlmProviderDraft`), and holding that body to the type's required list
// would refuse the ordinary edit of a working credential. `globalModelSections`
// is the same map the platform-model surface derives a section from, and is
// pinned against the registry snapshot by its own test.
func (h *Handler) refuseIncompleteUpdatedModelData(
	body map[string]any, configType string,
) *configurationWriteFailure {
	if _, isModel := globalModelSections[configType]; !isModel {
		return nil
	}
	// An ABSENT `data` key writes no column, so there is nothing it could
	// remove. That is the partial-update contract, and it is why a rename does
	// not have to restate the link.
	data, isObject := body["data"].(map[string]any)
	if !isObject {
		return nil
	}
	dataSchema, known := h.catalog.DataSchemaByType(configType)
	if !known {
		return nil
	}
	missing := missingRequiredUpdatedDataFields(dataSchema, data)
	if len(missing) == 0 {
		return nil
	}
	return &configurationWriteFailure{
		status:  http.StatusBadRequest,
		message: requiredUpdatedDataFieldsMessage(missing),
	}
}

// missingRequiredUpdatedDataFields walks one type's `data` schema against the
// `data` object an update carries, and returns the dotted paths it fails, in a
// stable order.
//
// The same walk the create runs, entered one level down. The depth bound is
// reduced by the level that is skipped, so the two reach the same distance
// into a nested contract.
func missingRequiredUpdatedDataFields(dataSchema, data map[string]any) []string {
	if dataSchema == nil || data == nil {
		return nil
	}
	missing := missingObjectFields(
		dataSchema, data, "data.", definitionsOf(dataSchema, nil), 0, requiredFieldDepthData-1)
	sort.Strings(missing)
	return missing
}

// requiredUpdatedDataFieldsMessage names the fields and the mechanism.
//
// The mechanism is half the information: an operator who cleared one field
// reads "missing required fields" as being about the field they did not send,
// not about the column the write replaces.
func requiredUpdatedDataFieldsMessage(missing []string) string {
	return "the update would leave the configuration without required fields: " +
		strings.Join(missing, ", ") +
		". The data column is replaced whole, so a field this body omits is removed from the stored row."
}
