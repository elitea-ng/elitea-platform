package configurations

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
)

const (
	MaxCurrentToolkitSettingsDepth       = 32
	MaxCurrentToolkitSettingsNodes       = 16_384
	MaxCurrentToolkitSettingsStringBytes = 4 << 20
	MaxCurrentToolkitSettingsIdentifier  = 1_024

	// CurrentFrozenConfigurationMarker is added only by ReferenceMode after a
	// configuration lookup succeeds. Claim-time materialization requires and
	// strips it before the SDK sees the current configuration shape. Rejecting
	// this key in saved input prevents user-controlled maps from forging a vault
	// owner through configuration_project_id.
	CurrentFrozenConfigurationMarker = "__elitea_frozen_configuration_v1"
)

var (
	ErrInvalidCurrentToolkitSettings    = errors.New("invalid current toolkit settings request")
	ErrCurrentToolkitSchemaNotFound     = errors.New("current toolkit schema not found")
	ErrCurrentToolkitSchemaInvalid      = errors.New("current toolkit schema is invalid")
	ErrCurrentToolkitSettingsValidation = errors.New("current toolkit settings validation failed")
	ErrCurrentToolkitSettingsDependency = errors.New("current toolkit settings dependency failed")
)

type CurrentToolkitSettingsMode string

const (
	// CurrentToolkitSettingsReferenceMode expands saved references while keeping
	// secret references sealed. It is suitable for validation and admission.
	CurrentToolkitSettingsReferenceMode CurrentToolkitSettingsMode = "references"
	// CurrentToolkitSettingsClaimMode redeems configuration and schema-declared
	// secret references for the short-lived worker claim.
	CurrentToolkitSettingsClaimMode CurrentToolkitSettingsMode = "claim"
)

type CurrentToolkitSettingsViolationCode string

const (
	CurrentToolkitConfigurationReferenceInvalidCode CurrentToolkitSettingsViolationCode = "configuration_reference_invalid"
	CurrentToolkitConfigurationNotFoundCode         CurrentToolkitSettingsViolationCode = "configuration_not_found"
	CurrentToolkitConfigurationForbiddenCode        CurrentToolkitSettingsViolationCode = "configuration_forbidden"
	CurrentToolkitConfigurationRecursionCode        CurrentToolkitSettingsViolationCode = "configuration_recursion"
	CurrentToolkitConfigurationTypeMismatchCode     CurrentToolkitSettingsViolationCode = "credential_type_mismatch"
	CurrentToolkitReferenceInvalidCode              CurrentToolkitSettingsViolationCode = "toolkit_reference_invalid"
	CurrentToolkitReferenceNotFoundCode             CurrentToolkitSettingsViolationCode = "toolkit_not_found"
	CurrentToolkitReferenceRecursionCode            CurrentToolkitSettingsViolationCode = "toolkit_recursion"
	CurrentToolkitSecretNotSealedCode               CurrentToolkitSettingsViolationCode = "secret_not_sealed"
	CurrentToolkitModelReferenceInvalidCode         CurrentToolkitSettingsViolationCode = "configuration_model_invalid"
	CurrentToolkitModelNotFoundCode                 CurrentToolkitSettingsViolationCode = "configuration_model_not_found"
)

// CurrentToolkitSettingsViolation identifies a schema field and a stable safe
// reason. It deliberately excludes configuration titles, toolkit IDs, model
// names, expected or actual configuration types, and secret values.
type CurrentToolkitSettingsViolation struct {
	Field string
	Code  CurrentToolkitSettingsViolationCode
}

// CurrentToolkitSettingsValidationError contains every independently observed
// field violation. No partially expanded settings are returned with this error.
type CurrentToolkitSettingsValidationError struct {
	Violations []CurrentToolkitSettingsViolation
}

func (e *CurrentToolkitSettingsValidationError) Error() string {
	return ErrCurrentToolkitSettingsValidation.Error()
}

func (e *CurrentToolkitSettingsValidationError) Unwrap() error {
	return ErrCurrentToolkitSettingsValidation
}

// CurrentToolkitSchema is the effective built-in/dynamic schema view required
// by the resolver. The catalog owns how snapshots from all schema sources are
// combined; the resolver only reads the top-level properties.
type CurrentToolkitSchema struct {
	Properties map[string]any
}

// CurrentToolkitSchemaCatalog returns the effective schema visible to an
// already-authorized actor in a project.
type CurrentToolkitSchemaCatalog interface {
	FindEffectiveToolkitSchema(
		context.Context,
		int32,
		int32,
		string,
	) (CurrentToolkitSchema, bool, error)
}

// CurrentNestedToolkit is the language-neutral shape returned by the current
// _expand_toolkit_reference behavior. Settings may still contain references.
type CurrentNestedToolkit struct {
	ID          int32
	ToolkitName string
	Type        string
	Settings    map[string]any
	AuthorID    *int32
	CreatedAt   *string
}

// CurrentNestedToolkitReader loads one toolkit from an already-authorized
// project. Missing or invisible rows return found=false.
type CurrentNestedToolkitReader interface {
	GetCurrentNestedToolkit(
		context.Context,
		int32,
		int32,
		int32,
	) (toolkit CurrentNestedToolkit, found bool, err error)
}

// CurrentConfigurationExpander is the consumer-owned view of
// CurrentExpansionService used for configuration_types/configuration_sections.
type CurrentConfigurationExpander interface {
	Expand(context.Context, CurrentExpansionRequest) (map[string]any, error)
}

// CurrentModelVisibility answers by exact model name and section. The
// implementation owns the current project plus shared/public visibility rule.
type CurrentModelVisibility interface {
	IsCurrentModelVisible(context.Context, int32, string, string) (bool, error)
}

type CurrentToolkitSettingsRequest struct {
	ToolkitType string
	Settings    map[string]any
	ProjectID   int32
	UserID      int32
	Mode        CurrentToolkitSettingsMode
}

// CurrentToolkitSettingsResolver ports the current schema-driven expansion
// rules without assigning credentials or model routing to providers/workers.
// It is synchronous, safe for concurrent use when its dependencies are safe,
// and returns newly owned maps.
type CurrentToolkitSettingsResolver struct {
	schemas        CurrentToolkitSchemaCatalog
	toolkits       CurrentNestedToolkitReader
	configurations CurrentConfigurationExpander
	models         CurrentModelVisibility
	unsecreter     CurrentExpansionUnsecreter
}

func NewCurrentToolkitSettingsResolver(
	schemas CurrentToolkitSchemaCatalog,
	toolkits CurrentNestedToolkitReader,
	configurations CurrentConfigurationExpander,
	models CurrentModelVisibility,
	unsecreter CurrentExpansionUnsecreter,
) (*CurrentToolkitSettingsResolver, error) {
	if schemas == nil || toolkits == nil || configurations == nil || models == nil || unsecreter == nil {
		return nil, errors.New("current toolkit settings dependencies are required")
	}
	return &CurrentToolkitSettingsResolver{
		schemas:        schemas,
		toolkits:       toolkits,
		configurations: configurations,
		models:         models,
		unsecreter:     unsecreter,
	}, nil
}

// Resolve applies only the annotations on each top-level schema property. The
// precedence is configuration, nested toolkit, secret, then model, matching the
// current expand_toolkit_settings elif chain.
func (r *CurrentToolkitSettingsResolver) Resolve(
	ctx context.Context,
	request CurrentToolkitSettingsRequest,
) (map[string]any, error) {
	if ctx == nil || request.ProjectID <= 0 || request.UserID <= 0 ||
		request.ToolkitType == "" || len(request.ToolkitType) > MaxCurrentToolkitSettingsIdentifier ||
		strings.ContainsAny(request.ToolkitType, "\x00\r\n") || request.Settings == nil ||
		(request.Mode != CurrentToolkitSettingsReferenceMode && request.Mode != CurrentToolkitSettingsClaimMode) {
		return nil, ErrInvalidCurrentToolkitSettings
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	walker := currentToolkitSettingsWalker{
		resolver: r,
		request:  request,
		active:   make(map[int32]struct{}),
	}
	settings, err := walker.cloneObject(ctx, request.Settings, 0)
	if err != nil {
		return nil, err
	}
	settings, err = walker.resolveToolkitSettings(ctx, request.ToolkitType, settings, "", 0)
	if err != nil {
		return nil, err
	}
	if len(walker.violations) != 0 {
		violations := append([]CurrentToolkitSettingsViolation(nil), walker.violations...)
		return nil, &CurrentToolkitSettingsValidationError{Violations: violations}
	}
	return settings, nil
}

type currentToolkitFieldKind uint8

const (
	currentToolkitPlainField currentToolkitFieldKind = iota
	currentToolkitConfigurationField
	currentToolkitReferenceField
	currentToolkitSecretField
	currentToolkitModelField
)

type currentToolkitFieldPlan struct {
	name                      string
	kind                      currentToolkitFieldKind
	expectedConfigurationType string
	modelSection              string
}

type currentToolkitSettingsWalker struct {
	resolver   *CurrentToolkitSettingsResolver
	request    CurrentToolkitSettingsRequest
	budget     currentToolkitSettingsBudget
	active     map[int32]struct{}
	violations []CurrentToolkitSettingsViolation
}

func (w *currentToolkitSettingsWalker) resolveToolkitSettings(
	ctx context.Context,
	toolkitType string,
	settings map[string]any,
	path string,
	depth int,
) (map[string]any, error) {
	if err := w.checkContextAndDepth(ctx, depth); err != nil {
		return nil, err
	}
	schema, found, err := w.resolver.schemas.FindEffectiveToolkitSchema(
		ctx,
		w.request.ProjectID,
		w.request.UserID,
		toolkitType,
	)
	if err != nil {
		return nil, currentToolkitSettingsDependencyError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrCurrentToolkitSchemaNotFound
	}

	plans, err := w.buildFieldPlans(ctx, schema.Properties)
	if err != nil {
		return nil, err
	}
	for _, kind := range []currentToolkitFieldKind{
		currentToolkitConfigurationField,
		currentToolkitReferenceField,
		currentToolkitSecretField,
		currentToolkitModelField,
	} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if kind == currentToolkitSecretField {
			if err := w.resolveSecretFields(ctx, settings, plans, path, depth); err != nil {
				return nil, err
			}
			continue
		}
		for _, plan := range plans {
			if plan.kind != kind {
				continue
			}
			fieldPath := currentToolkitSettingsPath(path, plan.name)
			switch kind {
			case currentToolkitConfigurationField:
				if err := w.resolveConfigurationField(ctx, settings, plan, fieldPath, depth); err != nil {
					return nil, err
				}
			case currentToolkitReferenceField:
				value, err := w.resolveToolkitReferenceValue(ctx, settings[plan.name], fieldPath, depth+1)
				if err != nil {
					return nil, err
				}
				settings[plan.name] = value
			case currentToolkitModelField:
				if err := w.resolveModelField(ctx, settings, plan, fieldPath); err != nil {
					return nil, err
				}
			}
		}
	}
	return settings, nil
}

func (w *currentToolkitSettingsWalker) buildFieldPlans(
	ctx context.Context,
	properties map[string]any,
) ([]currentToolkitFieldPlan, error) {
	if properties == nil {
		return nil, nil
	}
	if len(properties) > MaxCurrentToolkitSettingsNodes {
		return nil, ErrCurrentToolkitSchemaInvalid
	}

	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	plans := make([]currentToolkitFieldPlan, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if name == "" || len(name) > MaxCurrentToolkitSettingsIdentifier || strings.ContainsAny(name, "\x00\r\n") {
			return nil, ErrCurrentToolkitSchemaInvalid
		}
		if err := w.budget.addSchemaField(name); err != nil {
			return nil, err
		}
		property, ok := properties[name].(map[string]any)
		if !ok {
			return nil, ErrCurrentToolkitSchemaInvalid
		}
		plan := currentToolkitFieldPlan{name: name}
		configurationTypes := property["configuration_types"]
		configurationSections := property["configuration_sections"]
		switch {
		case currentToolkitSchemaTruthy(configurationTypes) || currentToolkitSchemaTruthy(configurationSections):
			plan.kind = currentToolkitConfigurationField
			if currentToolkitSchemaTruthy(configurationTypes) {
				expected, err := currentToolkitFirstSchemaString(configurationTypes)
				if err != nil || len(expected) > MaxCurrentToolkitSettingsIdentifier {
					return nil, ErrCurrentToolkitSchemaInvalid
				}
				plan.expectedConfigurationType = expected
				if err := w.budget.addStringBytes(len(expected)); err != nil {
					return nil, err
				}
			}
		case currentToolkitSchemaTruthy(property["toolkit_types"]):
			plan.kind = currentToolkitReferenceField
		case property["secret"] == true:
			plan.kind = currentToolkitSecretField
		case currentToolkitSchemaTruthy(property["configuration_model"]):
			section, ok := property["configuration_model"].(string)
			if !ok || section == "" || len(section) > MaxCurrentToolkitSettingsIdentifier || strings.ContainsAny(section, "\x00\r\n") {
				return nil, ErrCurrentToolkitSchemaInvalid
			}
			plan.kind = currentToolkitModelField
			plan.modelSection = section
			if err := w.budget.addStringBytes(len(section)); err != nil {
				return nil, err
			}
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

func (w *currentToolkitSettingsWalker) resolveConfigurationField(
	ctx context.Context,
	settings map[string]any,
	plan currentToolkitFieldPlan,
	fieldPath string,
	depth int,
) error {
	value := settings[plan.name]
	if currentToolkitJSONFalsy(value) {
		// NO CREDENTIAL: freeze the EMPTY OBJECT, never a JSON null.
		//
		// The reference implementation assigns settings.get(field) back after
		// expansion, so an absent configuration field arrived at the runtime as
		// an explicit null and a stored null stayed one. Every toolkit that
		// reads such a field then has to normalize it, and the current SDK
		// toolkits all do — `settings.get('openapi_configuration') or {}`
		// (elitea_sdk/tools/openapi/__init__.py:440,594, which additionally
		// replaces any non-dict with `{}`), `**kwargs['github_configuration']`
		// (which raises on a null and reports a missing field on `{}`),
		// `if settings.get('pgvector_configuration')` (falsy either way). `{}`
		// is what all three already mean by "no credential", so freezing it is
		// the shape the runtime contract is written against, not a new one.
		//
		// The native worker does NOT normalize, and that is what made this a
		// turn-killing defect rather than a cosmetic difference. Its OpenAPI
		// family reads
		//
		//   if let Some(configuration) = settings.get("openapi_configuration") {
		//       let configuration = configuration.as_object().ok_or_else(invalid_configuration)?;
		//
		// (libs/rust/agent-runtime/src/toolkits/families/openapi/config.rs,
		// merged_auth_settings). A null is Some(Value::Null), `as_object` fails,
		// and the WHOLE turn ends at toolset materialization with
		// `native_agent.invalid_configuration` — stored as an empty assistant
		// row flagged is_error, which renders like an answer. `{}` merges
		// nothing, so `parse_auth` finds no api_key/client_id/oauth endpoint and
		// returns OpenApiAuth::Anonymous: exactly right for an anonymous API.
		// The create form does offer a credential picker for the field (the
		// served type schema carries it, PR #352 / issue #330), but leaving it
		// empty is a legitimate answer for an API that needs no key — and it
		// stores no key at all, which is how the field goes missing here.
		//
		// openapi is the one built-in family whose credential is genuinely
		// optional. The others require it and require fields INSIDE it
		// (github's base_url, sql's host/username/password), so `{}` refuses
		// there exactly as null did — one line later, with the same code.
		//
		// The empty object carries no CurrentFrozenConfigurationMarker: nothing
		// was expanded, no vault was consulted, and the claim-time materializer
		// reads an unmarked, owner-less object as an ordinary map and leaves it
		// alone (currentFrozenConfigurationOwner in
		// internal/infra/storage/configurations_materializer.go).
		if err := w.budget.addContainer(0); err != nil {
			return err
		}
		settings[plan.name] = map[string]any{}
		return nil
	}
	reference, ok := value.(map[string]any)
	if !ok || len(reference) != 2 {
		w.addViolation(fieldPath, CurrentToolkitConfigurationReferenceInvalidCode)
		return nil
	}
	if _, ok := reference["elitea_title"]; !ok {
		w.addViolation(fieldPath, CurrentToolkitConfigurationReferenceInvalidCode)
		return nil
	}
	if _, ok := reference["private"]; !ok {
		w.addViolation(fieldPath, CurrentToolkitConfigurationReferenceInvalidCode)
		return nil
	}

	ownedReference, err := w.cloneObject(ctx, reference, depth+1)
	if err != nil {
		return err
	}
	userID := w.request.UserID
	expanded, err := w.resolver.configurations.Expand(ctx, CurrentExpansionRequest{
		Payload:          ownedReference,
		CurrentProjectID: w.request.ProjectID,
		UserID:           &userID,
		Unsecret:         w.request.Mode == CurrentToolkitSettingsClaimMode,
	})
	if err != nil {
		if contextErr := currentToolkitSettingsContextError(ctx, err); contextErr != nil {
			return contextErr
		}
		switch {
		case errors.Is(err, ErrCurrentExpansionNotFound):
			w.addViolation(fieldPath, CurrentToolkitConfigurationNotFoundCode)
			return nil
		case errors.Is(err, ErrCurrentExpansionForbidden):
			w.addViolation(fieldPath, CurrentToolkitConfigurationForbiddenCode)
			return nil
		case errors.Is(err, ErrCurrentExpansionRecursion):
			w.addViolation(fieldPath, CurrentToolkitConfigurationRecursionCode)
			return nil
		case errors.Is(err, ErrInvalidCurrentExpansion):
			w.addViolation(fieldPath, CurrentToolkitConfigurationReferenceInvalidCode)
			return nil
		default:
			return ErrCurrentToolkitSettingsDependency
		}
	}
	expanded, err = w.cloneObject(ctx, expanded, depth+1)
	if err != nil {
		return err
	}
	if len(expanded) != 0 && plan.expectedConfigurationType != "" {
		actual, ok := expanded["configuration_type"].(string)
		if ok && actual != "" && actual != plan.expectedConfigurationType {
			w.addViolation(fieldPath, CurrentToolkitConfigurationTypeMismatchCode)
		}
	}
	if w.request.Mode == CurrentToolkitSettingsReferenceMode && len(expanded) != 0 {
		if err := w.budget.addScalar(len(CurrentFrozenConfigurationMarker)); err != nil {
			return err
		}
		expanded[CurrentFrozenConfigurationMarker] = true
	}
	settings[plan.name] = expanded
	return nil
}

func (w *currentToolkitSettingsWalker) resolveToolkitReferenceValue(
	ctx context.Context,
	value any,
	fieldPath string,
	depth int,
) (any, error) {
	if err := w.checkContextAndDepth(ctx, depth); err != nil {
		return nil, err
	}
	if value == nil || value == "" {
		return value, nil
	}
	switch value := value.(type) {
	case map[string]any:
		return value, nil
	case []any:
		resolved := make([]any, len(value))
		for index, item := range value {
			itemPath := fieldPath + "[" + strconv.Itoa(index) + "]"
			resolvedItem, err := w.resolveToolkitReferenceValue(ctx, item, itemPath, depth+1)
			if err != nil {
				return nil, err
			}
			resolved[index] = resolvedItem
		}
		return resolved, nil
	}

	toolkitID, ok := currentToolkitReferenceID(value)
	if !ok {
		w.addViolation(fieldPath, CurrentToolkitReferenceInvalidCode)
		return value, nil
	}
	if _, repeated := w.active[toolkitID]; repeated {
		w.addViolation(fieldPath, CurrentToolkitReferenceRecursionCode)
		return value, nil
	}
	toolkit, found, err := w.resolver.toolkits.GetCurrentNestedToolkit(
		ctx,
		w.request.ProjectID,
		w.request.UserID,
		toolkitID,
	)
	if err != nil {
		return nil, currentToolkitSettingsDependencyError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !found {
		w.addViolation(fieldPath, CurrentToolkitReferenceNotFoundCode)
		return value, nil
	}
	if !currentNestedToolkitValid(toolkit, toolkitID) {
		return nil, ErrCurrentToolkitSettingsDependency
	}

	w.active[toolkitID] = struct{}{}
	defer delete(w.active, toolkitID)

	var expandedSettings map[string]any
	if toolkit.Settings != nil {
		expandedSettings, err = w.cloneObject(ctx, toolkit.Settings, depth+1)
		if err != nil {
			return nil, err
		}
		expandedSettings, err = w.resolveToolkitSettings(
			ctx,
			toolkit.Type,
			expandedSettings,
			fieldPath+".settings",
			depth+1,
		)
		if errors.Is(err, ErrCurrentToolkitSchemaNotFound) {
			// Current _expand_toolkit_reference preserves the nested toolkit and
			// its original settings when its expansion schema is unavailable.
			expandedSettings, err = w.cloneObject(ctx, toolkit.Settings, depth+1)
		}
		if err != nil {
			return nil, err
		}
	}

	result := map[string]any{
		"id":           toolkit.ID,
		"toolkit_name": toolkit.ToolkitName,
		"type":         toolkit.Type,
		"settings":     expandedSettings,
		"author_id":    nil,
		"created_at":   nil,
	}
	if toolkit.AuthorID != nil {
		result["author_id"] = *toolkit.AuthorID
	}
	if toolkit.CreatedAt != nil {
		result["created_at"] = *toolkit.CreatedAt
	}
	if err := w.budget.addNestedToolkit(toolkit); err != nil {
		return nil, err
	}
	return result, nil
}

func (w *currentToolkitSettingsWalker) resolveSecretFields(
	ctx context.Context,
	settings map[string]any,
	plans []currentToolkitFieldPlan,
	path string,
	depth int,
) error {
	if w.request.Mode == CurrentToolkitSettingsReferenceMode {
		// Current Provider Hub rows may predate save-time vault wrapping. A
		// frozen command must not duplicate that plaintext into durable input.
		// The schema identifies these fields without teaching this boundary any
		// provider-specific names; fail closed until the row is migrated.
		for _, plan := range plans {
			if plan.kind != currentToolkitSecretField {
				continue
			}
			value := settings[plan.name]
			if currentToolkitJSONFalsy(value) {
				continue
			}
			sealed, ok := value.(string)
			if !ok {
				w.addViolation(currentToolkitSettingsPath(path, plan.name), CurrentToolkitSecretNotSealedCode)
				continue
			}
			if _, ok := currentSecretReferenceName(sealed); !ok {
				w.addViolation(currentToolkitSettingsPath(path, plan.name), CurrentToolkitSecretNotSealedCode)
			}
		}
		return nil
	}
	if w.request.Mode != CurrentToolkitSettingsClaimMode {
		return nil
	}
	values := make(map[string]any)
	for _, plan := range plans {
		if plan.kind != currentToolkitSecretField {
			continue
		}
		value, ok := settings[plan.name].(string)
		if !ok || value == "" {
			continue
		}
		values[plan.name] = value
	}
	if len(values) == 0 {
		return nil
	}

	unsecreted, err := w.resolver.unsecreter.Unsecret(ctx, w.request.ProjectID, values)
	if err != nil {
		return currentToolkitSettingsDependencyError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(unsecreted) != len(values) {
		return ErrCurrentToolkitSettingsDependency
	}
	owned, err := w.cloneObject(ctx, unsecreted, depth+1)
	if err != nil {
		return err
	}
	for key := range values {
		value, ok := owned[key]
		if !ok {
			return ErrCurrentToolkitSettingsDependency
		}
		settings[key] = value
	}
	return nil
}

func (w *currentToolkitSettingsWalker) resolveModelField(
	ctx context.Context,
	settings map[string]any,
	plan currentToolkitFieldPlan,
	fieldPath string,
) error {
	value := settings[plan.name]
	if currentToolkitJSONFalsy(value) {
		return nil
	}
	modelName, ok := value.(string)
	if !ok || len(modelName) > MaxCurrentToolkitSettingsIdentifier || strings.ContainsAny(modelName, "\x00\r\n") {
		w.addViolation(fieldPath, CurrentToolkitModelReferenceInvalidCode)
		return nil
	}
	visible, err := w.resolver.models.IsCurrentModelVisible(ctx, w.request.ProjectID, plan.modelSection, modelName)
	if err != nil {
		return currentToolkitSettingsDependencyError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !visible {
		w.addViolation(fieldPath, CurrentToolkitModelNotFoundCode)
	}
	return nil
}

func (w *currentToolkitSettingsWalker) cloneObject(
	ctx context.Context,
	value map[string]any,
	depth int,
) (map[string]any, error) {
	if value == nil {
		return nil, nil
	}
	cloned, err := w.cloneJSON(ctx, value, depth)
	if err != nil {
		return nil, err
	}
	return cloned.(map[string]any), nil
}

func (w *currentToolkitSettingsWalker) cloneJSON(ctx context.Context, value any, depth int) (any, error) {
	if err := w.checkContextAndDepth(ctx, depth); err != nil {
		return nil, err
	}
	switch value := value.(type) {
	case map[string]any:
		if err := w.budget.addContainer(len(value)); err != nil {
			return nil, err
		}
		cloned := make(map[string]any, len(value))
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if key == CurrentFrozenConfigurationMarker ||
				len(key) > MaxCurrentToolkitSettingsIdentifier || strings.ContainsAny(key, "\x00\r\n") {
				return nil, ErrInvalidCurrentToolkitSettings
			}
			if err := w.budget.addStringBytes(len(key)); err != nil {
				return nil, err
			}
			item, err := w.cloneJSON(ctx, value[key], depth+1)
			if err != nil {
				return nil, err
			}
			cloned[key] = item
		}
		return cloned, nil
	case []any:
		if err := w.budget.addContainer(len(value)); err != nil {
			return nil, err
		}
		cloned := make([]any, len(value))
		for index, item := range value {
			clonedItem, err := w.cloneJSON(ctx, item, depth+1)
			if err != nil {
				return nil, err
			}
			cloned[index] = clonedItem
		}
		return cloned, nil
	case string:
		if err := w.budget.addScalar(len(value)); err != nil {
			return nil, err
		}
		return value, nil
	case json.Number:
		if err := w.budget.addScalar(len(value)); err != nil {
			return nil, err
		}
		return value, nil
	case nil, bool,
		float32, float64,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		if err := w.budget.addScalar(0); err != nil {
			return nil, err
		}
		return value, nil
	default:
		return nil, ErrInvalidCurrentToolkitSettings
	}
}

func (w *currentToolkitSettingsWalker) checkContextAndDepth(ctx context.Context, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > MaxCurrentToolkitSettingsDepth {
		return ErrInvalidCurrentToolkitSettings
	}
	return nil
}

func (w *currentToolkitSettingsWalker) addViolation(field string, code CurrentToolkitSettingsViolationCode) {
	w.violations = append(w.violations, CurrentToolkitSettingsViolation{Field: field, Code: code})
}

type currentToolkitSettingsBudget struct {
	nodes       int
	stringBytes int
}

func (b *currentToolkitSettingsBudget) addContainer(entries int) error {
	if entries < 0 || entries > MaxCurrentToolkitSettingsNodes-b.nodes-1 {
		return ErrInvalidCurrentToolkitSettings
	}
	b.nodes++
	return nil
}

func (b *currentToolkitSettingsBudget) addScalar(stringBytes int) error {
	if b.nodes >= MaxCurrentToolkitSettingsNodes {
		return ErrInvalidCurrentToolkitSettings
	}
	b.nodes++
	return b.addStringBytes(stringBytes)
}

func (b *currentToolkitSettingsBudget) addStringBytes(count int) error {
	if count < 0 || count > MaxCurrentToolkitSettingsStringBytes-b.stringBytes {
		return ErrInvalidCurrentToolkitSettings
	}
	b.stringBytes += count
	return nil
}

func (b *currentToolkitSettingsBudget) addSchemaField(name string) error {
	if err := b.addScalar(len(name)); err != nil {
		return err
	}
	return nil
}

func (b *currentToolkitSettingsBudget) addNestedToolkit(toolkit CurrentNestedToolkit) error {
	const metadataEntries = 6
	if err := b.addContainer(metadataEntries); err != nil {
		return err
	}
	for _, key := range []string{"id", "toolkit_name", "type", "settings", "author_id", "created_at"} {
		if err := b.addStringBytes(len(key)); err != nil {
			return err
		}
	}
	if err := b.addScalar(0); err != nil { // id
		return err
	}
	if err := b.addScalar(len(toolkit.ToolkitName)); err != nil {
		return err
	}
	if err := b.addScalar(len(toolkit.Type)); err != nil {
		return err
	}
	if err := b.addScalar(0); err != nil { // author_id
		return err
	}
	createdAtBytes := 0
	if toolkit.CreatedAt != nil {
		createdAtBytes = len(*toolkit.CreatedAt)
	}
	return b.addScalar(createdAtBytes)
}

func currentToolkitSettingsDependencyError(ctx context.Context, err error) error {
	if contextErr := currentToolkitSettingsContextError(ctx, err); contextErr != nil {
		return contextErr
	}
	return ErrCurrentToolkitSettingsDependency
}

func currentToolkitSettingsContextError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

func currentToolkitSettingsPath(parent, field string) string {
	if parent == "" {
		return field
	}
	return parent + "." + field
}

func currentToolkitSchemaTruthy(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case bool:
		return value
	case string:
		return value != ""
	case []any:
		return len(value) != 0
	case []string:
		return len(value) != 0
	case map[string]any:
		return len(value) != 0
	case int:
		return value != 0
	case int32:
		return value != 0
	case int64:
		return value != 0
	case float64:
		return value != 0
	default:
		return true
	}
}

func currentToolkitFirstSchemaString(value any) (string, error) {
	switch values := value.(type) {
	case []any:
		if len(values) == 0 {
			return "", ErrCurrentToolkitSchemaInvalid
		}
		result, ok := values[0].(string)
		if !ok || result == "" || strings.ContainsAny(result, "\x00\r\n") {
			return "", ErrCurrentToolkitSchemaInvalid
		}
		return result, nil
	case []string:
		if len(values) == 0 || values[0] == "" || strings.ContainsAny(values[0], "\x00\r\n") {
			return "", ErrCurrentToolkitSchemaInvalid
		}
		return values[0], nil
	default:
		return "", ErrCurrentToolkitSchemaInvalid
	}
}

func currentToolkitJSONFalsy(value any) bool {
	switch value := value.(type) {
	case nil:
		return true
	case bool:
		return !value
	case string:
		return value == ""
	case []any:
		return len(value) == 0
	case []string:
		return len(value) == 0
	case map[string]any:
		return len(value) == 0
	case int:
		return value == 0
	case int8:
		return value == 0
	case int16:
		return value == 0
	case int32:
		return value == 0
	case int64:
		return value == 0
	case uint:
		return value == 0
	case uint8:
		return value == 0
	case uint16:
		return value == 0
	case uint32:
		return value == 0
	case uint64:
		return value == 0
	case float32:
		return value == 0
	case float64:
		return value == 0
	case json.Number:
		return value == "0" || value == "0.0"
	default:
		return false
	}
}

func currentToolkitReferenceID(value any) (int32, bool) {
	var id int64
	switch value := value.(type) {
	case json.Number:
		parsed, err := value.Int64()
		if err != nil {
			return 0, false
		}
		id = parsed
	case int:
		id = int64(value)
	case int8:
		id = int64(value)
	case int16:
		id = int64(value)
	case int32:
		id = int64(value)
	case int64:
		id = value
	case uint:
		if uint64(value) > math.MaxInt32 {
			return 0, false
		}
		id = int64(value)
	case uint8:
		id = int64(value)
	case uint16:
		id = int64(value)
	case uint32:
		id = int64(value)
	case uint64:
		if value > math.MaxInt32 {
			return 0, false
		}
		id = int64(value)
	case float32:
		floatValue := float64(value)
		if math.Trunc(floatValue) != floatValue {
			return 0, false
		}
		id = int64(floatValue)
	case float64:
		if math.Trunc(value) != value {
			return 0, false
		}
		id = int64(value)
	default:
		return 0, false
	}
	if id <= 0 || id > math.MaxInt32 {
		return 0, false
	}
	return int32(id), true
}

func currentNestedToolkitValid(toolkit CurrentNestedToolkit, requestedID int32) bool {
	if toolkit.ID != requestedID || toolkit.ID <= 0 || toolkit.Type == "" || toolkit.ToolkitName == "" ||
		len(toolkit.Type) > MaxCurrentToolkitSettingsIdentifier ||
		len(toolkit.ToolkitName) > MaxCurrentToolkitSettingsIdentifier ||
		strings.ContainsAny(toolkit.Type, "\x00\r\n") ||
		strings.ContainsAny(toolkit.ToolkitName, "\x00\r\n") {
		return false
	}
	if toolkit.AuthorID != nil && *toolkit.AuthorID <= 0 {
		return false
	}
	return toolkit.CreatedAt == nil ||
		(len(*toolkit.CreatedAt) <= MaxCurrentToolkitSettingsIdentifier && !strings.ContainsAny(*toolkit.CreatedAt, "\x00\r\n"))
}

var _ CurrentConfigurationExpander = (*CurrentExpansionService)(nil)
