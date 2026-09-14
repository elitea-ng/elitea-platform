package toolkitexecution

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/guardrails"
)

const (
	MaxCurrentReadToolIdentityBytes   = 1024
	MaxCurrentReadToolSelectionCount  = 16_384
	MaxCurrentReadToolSnapshotBytes   = 640 * 1024
	MaxCurrentReadToolArgumentsBytes  = 256 * 1024
	MaxCurrentReadToolGuardrailsBytes = 64 * 1024
)

var (
	ErrInvalidCurrentReadTool       = errors.New("invalid current direct toolkit request")
	ErrCurrentReadToolkitNotVisible = errors.New("current toolkit is not exposed through MCP")
	ErrCurrentReadToolNotSelected   = errors.New("current toolkit operation is not selected")
	ErrCurrentReadToolRestricted    = errors.New("current toolkit operation is restricted by policy")
)

// CurrentReadToolAdmissionStage identifies a bounded admission boundary
// without carrying the underlying error text across layers. Configuration
// expansion errors can contain provider-owned identifiers, so the public MCP
// handler logs this stable stage and an error code instead of err.Error().
type CurrentReadToolAdmissionStage string

const (
	CurrentReadToolAdmissionInput             CurrentReadToolAdmissionStage = "input_validation"
	CurrentReadToolAdmissionToolkitLookup     CurrentReadToolAdmissionStage = "toolkit_lookup"
	CurrentReadToolAdmissionToolkitVisibility CurrentReadToolAdmissionStage = "toolkit_visibility"
	CurrentReadToolAdmissionSelection         CurrentReadToolAdmissionStage = "selected_operation"
	CurrentReadToolAdmissionGuardrails        CurrentReadToolAdmissionStage = "guardrail_resolution"
	CurrentReadToolAdmissionPolicy            CurrentReadToolAdmissionStage = "guardrail_policy"
	CurrentReadToolAdmissionSettings          CurrentReadToolAdmissionStage = "settings_resolution"
	CurrentReadToolAdmissionResolvedSelection CurrentReadToolAdmissionStage = "resolved_selection"
	CurrentReadToolAdmissionSnapshot          CurrentReadToolAdmissionStage = "snapshot_encoding"
	CurrentReadToolAdmissionDurableWrite      CurrentReadToolAdmissionStage = "durable_admission"
)

// CurrentReadToolAdmissionError preserves errors.Is/errors.As through Unwrap
// while keeping its own Error string free of provider or credential details.
type CurrentReadToolAdmissionError struct {
	stage CurrentReadToolAdmissionStage
	cause error
}

func (e *CurrentReadToolAdmissionError) Error() string {
	return "current direct toolkit admission failed at " + string(e.stage)
}

func (e *CurrentReadToolAdmissionError) Unwrap() error { return e.cause }

// CurrentReadToolAdmissionStageOf returns an empty value for errors outside
// the direct-tool admission path, including cancellation and result waiting.
func CurrentReadToolAdmissionStageOf(err error) CurrentReadToolAdmissionStage {
	var admissionError *CurrentReadToolAdmissionError
	if errors.As(err, &admissionError) {
		return admissionError.stage
	}
	return ""
}

func currentReadToolAdmissionError(stage CurrentReadToolAdmissionStage, cause error) error {
	if cause == nil {
		return nil
	}
	return &CurrentReadToolAdmissionError{stage: stage, cause: cause}
}

// CurrentMCPToolkitSnapshot is the exact provider-neutral row state needed to
// admit one external MCP call. Settings and metadata are still protected
// references; the reader must never redeem a credential on this boundary.
type CurrentMCPToolkitSnapshot struct {
	ID       int32
	Type     string
	Name     string
	Settings map[string]any
	Meta     map[string]any
}

// CurrentMCPToolkitReader loads one exact toolkit from the already-authorized
// project. Missing or invisible rows return found=false.
type CurrentMCPToolkitReader interface {
	GetCurrentMCPToolkit(context.Context, int32, int32, int32) (CurrentMCPToolkitSnapshot, bool, error)
}

type CurrentToolkitSettingsResolver interface {
	Resolve(context.Context, configurationapp.CurrentToolkitSettingsRequest) (map[string]any, error)
}

type CurrentGuardrailResolver interface {
	ResolveCurrentAgentGuardrails(context.Context) (guardrails.Policy, error)
}

type FreezeCurrentReadToolRequest struct {
	ProjectID int32
	ActorID   int32
	ToolkitID int32
	ToolName  string
	Arguments map[string]any
}

// FrozenCurrentReadTool contains only immutable reference-mode input. Main
// stores these bytes in the input data plane; claim-time materialization is
// the first boundary allowed to redeem configuration or secret references.
type FrozenCurrentReadTool struct {
	ToolkitType    string
	ToolkitName    string
	ToolName       string
	ToolkitJSON    []byte
	ArgumentsJSON  []byte
	GuardrailsJSON []byte
}

func (f FrozenCurrentReadTool) Clone() FrozenCurrentReadTool {
	f.ToolkitJSON = append([]byte(nil), f.ToolkitJSON...)
	f.ArgumentsJSON = append([]byte(nil), f.ArgumentsJSON...)
	f.GuardrailsJSON = append([]byte(nil), f.GuardrailsJSON...)
	return f
}

// CurrentReadToolFreezer revalidates the same opt-in and selected-operation
// facts used by tools/list, then freezes schema-aware settings in reference
// mode. This second read closes the tools/list -> tools/call race without
// copying SQL into the HTTP handler.
type CurrentReadToolFreezer struct {
	toolkits   CurrentMCPToolkitReader
	settings   CurrentToolkitSettingsResolver
	guardrails CurrentGuardrailResolver
}

func NewCurrentReadToolFreezer(
	toolkits CurrentMCPToolkitReader,
	settings CurrentToolkitSettingsResolver,
	guardrails CurrentGuardrailResolver,
) (*CurrentReadToolFreezer, error) {
	if toolkits == nil || settings == nil || guardrails == nil {
		return nil, errors.New("current direct toolkit dependencies are required")
	}
	return &CurrentReadToolFreezer{toolkits: toolkits, settings: settings, guardrails: guardrails}, nil
}

func (f *CurrentReadToolFreezer) Freeze(
	ctx context.Context,
	request FreezeCurrentReadToolRequest,
) (FrozenCurrentReadTool, error) {
	if f == nil || ctx == nil || request.ProjectID <= 0 || request.ActorID <= 0 ||
		request.ToolkitID <= 0 || !validIdentity(request.ToolName) || request.Arguments == nil {
		return FrozenCurrentReadTool{}, currentReadToolAdmissionError(
			CurrentReadToolAdmissionInput, ErrInvalidCurrentReadTool,
		)
	}
	if err := ctx.Err(); err != nil {
		return FrozenCurrentReadTool{}, err
	}

	toolkit, found, err := f.toolkits.GetCurrentMCPToolkit(
		ctx, request.ProjectID, request.ActorID, request.ToolkitID,
	)
	if err != nil {
		return FrozenCurrentReadTool{}, currentReadToolAdmissionError(
			CurrentReadToolAdmissionToolkitLookup, err,
		)
	}
	if !found || toolkit.ID != request.ToolkitID || !validIdentity(toolkit.Type) ||
		!validIdentity(toolkit.Name) || toolkit.Settings == nil || toolkit.Meta == nil ||
		!availableByMCP(toolkit.Meta) {
		return FrozenCurrentReadTool{}, currentReadToolAdmissionError(
			CurrentReadToolAdmissionToolkitVisibility, ErrCurrentReadToolkitNotVisible,
		)
	}
	_, selected, err := normalizedSelectedToolNames(toolkit.Settings)
	if err != nil {
		return FrozenCurrentReadTool{}, currentReadToolAdmissionError(
			CurrentReadToolAdmissionSelection, err,
		)
	}
	if _, ok := selected[request.ToolName]; !ok {
		return FrozenCurrentReadTool{}, currentReadToolAdmissionError(
			CurrentReadToolAdmissionSelection, ErrCurrentReadToolNotSelected,
		)
	}

	policy, err := f.guardrails.ResolveCurrentAgentGuardrails(ctx)
	if err != nil {
		return FrozenCurrentReadTool{}, currentReadToolAdmissionError(
			CurrentReadToolAdmissionGuardrails, err,
		)
	}
	if policy.ToolBlocked(toolkit.Type, request.ToolName) {
		return FrozenCurrentReadTool{}, currentReadToolAdmissionError(
			CurrentReadToolAdmissionPolicy, ErrCurrentReadToolRestricted,
		)
	}
	if _, sensitive := policy.SensitiveMatch(request.ToolName, toolkit.Type, toolkit.Name); sensitive {
		return FrozenCurrentReadTool{}, currentReadToolAdmissionError(
			CurrentReadToolAdmissionPolicy, ErrCurrentReadToolRestricted,
		)
	}

	resolved, err := f.settings.Resolve(ctx, configurationapp.CurrentToolkitSettingsRequest{
		ToolkitType: toolkit.Type,
		Settings:    toolkit.Settings,
		ProjectID:   request.ProjectID,
		UserID:      request.ActorID,
		Mode:        configurationapp.CurrentToolkitSettingsReferenceMode,
	})
	if err != nil {
		return FrozenCurrentReadTool{}, currentReadToolAdmissionError(
			CurrentReadToolAdmissionSettings, err,
		)
	}
	if resolved == nil {
		return FrozenCurrentReadTool{}, currentReadToolAdmissionError(
			CurrentReadToolAdmissionSettings, ErrInvalidCurrentReadTool,
		)
	}
	canonicalSelected, resolvedSelected, err := normalizedSelectedToolNames(resolved)
	if err != nil {
		return FrozenCurrentReadTool{}, currentReadToolAdmissionError(
			CurrentReadToolAdmissionResolvedSelection, err,
		)
	}
	if _, ok := resolvedSelected[request.ToolName]; !ok {
		return FrozenCurrentReadTool{}, currentReadToolAdmissionError(
			CurrentReadToolAdmissionResolvedSelection, ErrCurrentReadToolNotSelected,
		)
	}
	// Legacy OpenAPI rows may still carry `{name: ...}` entries. The current UI
	// normalizes them to names before save; do the same for an old row inside
	// this newly owned map so Rust receives the current SDK shape. Malformed
	// entries authorize nothing and duplicates collapse deterministically.
	resolved["selected_tools"] = canonicalSelected

	toolkitJSON, err := boundedJSON(map[string]any{
		"id":           toolkit.ID,
		"type":         toolkit.Type,
		"toolkit_name": toolkit.Name,
		"settings":     resolved,
	}, MaxCurrentReadToolSnapshotBytes)
	if err != nil {
		return FrozenCurrentReadTool{}, currentReadToolAdmissionError(
			CurrentReadToolAdmissionSnapshot, err,
		)
	}
	argumentsJSON, err := boundedJSON(request.Arguments, MaxCurrentReadToolArgumentsBytes)
	if err != nil {
		return FrozenCurrentReadTool{}, currentReadToolAdmissionError(
			CurrentReadToolAdmissionSnapshot, err,
		)
	}
	guardrailsJSON, err := boundedJSON(policy.Runtime(), MaxCurrentReadToolGuardrailsBytes)
	if err != nil {
		return FrozenCurrentReadTool{}, currentReadToolAdmissionError(
			CurrentReadToolAdmissionSnapshot, err,
		)
	}

	return FrozenCurrentReadTool{
		ToolkitType: toolkit.Type, ToolkitName: toolkit.Name, ToolName: request.ToolName,
		ToolkitJSON: toolkitJSON, ArgumentsJSON: argumentsJSON, GuardrailsJSON: guardrailsJSON,
	}, nil
}

func availableByMCP(meta map[string]any) bool {
	options, ok := meta["mcp_options"].(map[string]any)
	if !ok || options == nil {
		return false
	}
	available, ok := options["available_by_mcp"].(bool)
	return ok && available
}

func normalizedSelectedToolNames(settings map[string]any) ([]any, map[string]struct{}, error) {
	raw, ok := settings["selected_tools"]
	if !ok {
		return nil, nil, ErrCurrentReadToolNotSelected
	}
	var values []any
	switch typed := raw.(type) {
	case []any:
		values = typed
	case []string:
		values = make([]any, len(typed))
		for index := range typed {
			values[index] = typed[index]
		}
	default:
		return nil, nil, ErrInvalidCurrentReadTool
	}
	if len(values) == 0 {
		return nil, nil, ErrCurrentReadToolNotSelected
	}
	if len(values) > MaxCurrentReadToolSelectionCount {
		return nil, nil, ErrInvalidCurrentReadTool
	}
	selected := make(map[string]struct{}, len(values))
	canonical := make([]any, 0, len(values))
	for _, value := range values {
		var name string
		switch typed := value.(type) {
		case string:
			name = typed
		case map[string]any:
			name, _ = typed["name"].(string)
		default:
			continue
		}
		name = strings.TrimSpace(name)
		if !validIdentity(name) {
			continue
		}
		if _, duplicate := selected[name]; duplicate {
			continue
		}
		selected[name] = struct{}{}
		canonical = append(canonical, name)
	}
	if len(canonical) == 0 {
		return nil, nil, ErrCurrentReadToolNotSelected
	}
	return canonical, selected, nil
}

func boundedJSON(value any, maximum int) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) == 0 || len(encoded) > maximum {
		return nil, ErrInvalidCurrentReadTool
	}
	return encoded, nil
}

func validIdentity(value string) bool {
	return value != "" && len(value) <= MaxCurrentReadToolIdentityBytes &&
		!strings.ContainsAny(value, "\x00\r\n")
}
