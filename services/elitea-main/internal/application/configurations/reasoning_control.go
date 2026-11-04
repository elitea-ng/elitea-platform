package configurations

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

// reasoning_control.go — how a model's thinking is controlled (ADR-0038
// decisions 1 and 3, #1452).
//
// The Rust translation table (libs/rust/llm-wire `thinking`) owns the wire
// forms. This file owns the two catalogue keys that select a form, their
// validation on every llm_model write, the family defaults of shared
// migration 0225, and the canonical reasoning_effort value with its save-time
// conflict check. The values and the conflict texts are the table's, byte for
// byte: conformance/llm-caller/contract.json pins both.

// The llm_model `data` keys.
const (
	ReasoningControlField   = "reasoning_control"
	ReasoningToolStepsField = "reasoning_tool_steps"
)

// The five control forms.
const (
	ReasoningControlEffort   = "effort"
	ReasoningControlBudget   = "budget"
	ReasoningControlToggle   = "toggle"
	ReasoningControlFixedOn  = "fixed_on"
	ReasoningControlFixedOff = "fixed_off"
)

// ReasoningControls lists the accepted forms in a stable order, for a refusal
// message and for the registry schema's enum.
func ReasoningControls() []string {
	return []string{
		ReasoningControlEffort, ReasoningControlBudget, ReasoningControlToggle,
		ReasoningControlFixedOn, ReasoningControlFixedOff,
	}
}

// IsReasoningControl reports whether value is one of ReasoningControls.
func IsReasoningControl(value string) bool {
	for _, control := range ReasoningControls() {
		if value == control {
			return true
		}
	}
	return false
}

// The canonical reasoning_effort values (ADR-0038 decision 1).
const (
	ReasoningEffortAuto   = "auto"
	ReasoningEffortOff    = "off"
	ReasoningEffortLow    = "low"
	ReasoningEffortMedium = "medium"
	ReasoningEffortHigh   = "high"
	// reasoningEffortNone is the stored value of an agent saved before
	// ADR-0038. It is read as off.
	reasoningEffortNone = "none"
)

// ReasoningEfforts lists the canonical values in a stable order.
func ReasoningEfforts() []string {
	return []string{
		ReasoningEffortAuto, ReasoningEffortOff, ReasoningEffortLow,
		ReasoningEffortMedium, ReasoningEffortHigh,
	}
}

// CanonicalReasoningEffort reads a stored or submitted reasoning_effort. It
// returns the canonical value and true, or "" and false for a value that is
// not one. "none" reads as off.
func CanonicalReasoningEffort(value string) (string, bool) {
	switch value {
	case ReasoningEffortAuto, ReasoningEffortOff, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh:
		return value, true
	case reasoningEffortNone:
		return ReasoningEffortOff, true
	default:
		return "", false
	}
}

// ErrInvalidReasoningEffort refuses a reasoning_effort that is not canonical.
var ErrInvalidReasoningEffort = errors.New(
	"reasoning_effort must be one of: " + strings.Join(ReasoningEfforts(), ", "))

// The conflict texts. They are the llm-wire table's Conflict::message.
const (
	reasoningConflictAlwaysThinks = "this model always thinks: set reasoning_effort to auto, low, medium or high, not off"
	reasoningConflictCannotThink  = "this model cannot think: set reasoning_effort to auto or off, not a level"
)

// ReasoningEffortConflict reports a canonical setting the control form
// cannot honour, as a readable error, or nil. An unknown or empty control
// has no conflict: the caller then keeps its previous form.
func ReasoningEffortConflict(control, effort string) error {
	switch control {
	case ReasoningControlFixedOn:
		if effort == ReasoningEffortOff {
			return errors.New(reasoningConflictAlwaysThinks)
		}
	case ReasoningControlFixedOff:
		if effort == ReasoningEffortLow || effort == ReasoningEffortMedium || effort == ReasoningEffortHigh {
			return errors.New(reasoningConflictCannotThink)
		}
	}
	return nil
}

// ReasoningControlRefusesTemperature reports whether a model of this form
// refuses temperature beside a reasoning setting. A toggle model and a fixed
// form take it; effort, budget and an undeclared form keep the rule from
// before ADR-0038: a reasoning model sends no temperature.
func ReasoningControlRefusesTemperature(control string) bool {
	switch control {
	case ReasoningControlToggle, ReasoningControlFixedOn, ReasoningControlFixedOff:
		return false
	default:
		return true
	}
}

// ErrInvalidReasoningControl and ErrInvalidReasoningToolSteps refuse a
// malformed key on an llm_model write.
var (
	ErrInvalidReasoningControl = errors.New(
		"data.reasoning_control must be one of: " + strings.Join(ReasoningControls(), ", "))
	ErrInvalidReasoningToolSteps = errors.New("data.reasoning_tool_steps must be a boolean")
)

// ValidateLLMModelReasoning checks the two reasoning keys of an llm_model
// `data` object. An absent or null key is valid: the row then takes the
// family default, or its caller's previous form.
//
// A thinking form on a row that says supports_reasoning false is refused: the
// row would promise a control the model does not have. fixed_off is the one
// form such a row may declare.
func ValidateLLMModelReasoning(data map[string]any) error {
	if raw, present := data[ReasoningControlField]; present && raw != nil {
		value, isString := raw.(string)
		if !isString || !IsReasoningControl(value) {
			return ErrInvalidReasoningControl
		}
		if supports, declared := data["supports_reasoning"].(bool); declared && !supports && value != ReasoningControlFixedOff {
			return fmt.Errorf("data.reasoning_control %s needs supports_reasoning true; a model that cannot think takes fixed_off", value)
		}
	}
	if raw, present := data[ReasoningToolStepsField]; present && raw != nil {
		if _, isBool := raw.(bool); !isBool {
			return ErrInvalidReasoningToolSteps
		}
	}
	return nil
}

// ModelReasoningDefault is one row of elitea_runtime.model_reasoning_defaults
// (shared migration 0225), its pattern compiled.
type ModelReasoningDefault struct {
	Pattern   *regexp.Regexp
	Control   string
	ToolSteps bool
}

// CompileModelReasoningDefault builds a default from a stored row. The
// pattern is matched case-insensitively.
func CompileModelReasoningDefault(pattern, control string, toolSteps bool) (ModelReasoningDefault, error) {
	if !IsReasoningControl(control) {
		return ModelReasoningDefault{}, ErrInvalidReasoningControl
	}
	compiled, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return ModelReasoningDefault{}, fmt.Errorf("model reasoning default %q: %w", pattern, err)
	}
	return ModelReasoningDefault{Pattern: compiled, Control: control, ToolSteps: toolSteps}, nil
}

// ApplyModelReasoningDefaults resolves an llm_model item's control form and
// tool-step rule. The item's own keys win. A family default applies only to
// an item that says supports_reasoning true, and only to a key the item does
// not declare. defaults are in priority order: the first match wins.
func ApplyModelReasoningDefaults(item *CurrentModelCatalogItem, defaults []ModelReasoningDefault) {
	if item == nil || (item.ReasoningControl != nil && item.ReasoningToolSteps != nil) {
		return
	}
	if item.SupportsReasoning == nil || !*item.SupportsReasoning {
		return
	}
	for _, rule := range defaults {
		if rule.Pattern == nil || !rule.Pattern.MatchString(item.Name) {
			continue
		}
		if item.ReasoningControl == nil {
			control := rule.Control
			item.ReasoningControl = &control
		}
		if item.ReasoningToolSteps == nil {
			toolSteps := rule.ToolSteps
			item.ReasoningToolSteps = &toolSteps
		}
		return
	}
}

// OpenAIThinking is the thinking form of one OpenAI-compatible
// chat-completions request: the Go mirror of llm-wire
// `thinking::openai_form` for a caller that makes one request per answer
// (predict_llm and the AI drafts). conformance/llm-caller/contract.json pins
// both.
type OpenAIThinking struct {
	// ReasoningEffort is the `reasoning_effort` member; "" means absent.
	ReasoningEffort string
	// EnableThinking is `chat_template_kwargs.enable_thinking`; nil means
	// absent.
	EnableThinking *bool
	// RefusesTemperature reports that the request must carry no temperature:
	// a model that thinks by effort or budget refuses one while it thinks.
	RefusesTemperature bool
}

// SingleCallOpenAIThinking translates a canonical setting for a model of
// the given control form into one request's form. An empty setting sends
// nothing, as before ADR-0038. auto thinks at the medium level, as the first
// step of an agent turn does (llm-wire `Thinking::single_call`). An empty
// control is the form callers used before ADR-0038: reasoning_effort.
//
// The setting must be canonical (CanonicalReasoningEffort) and must not
// conflict with the control (ReasoningEffortConflict); a fixed form sends
// nothing whatever the setting.
func SingleCallOpenAIThinking(control, setting string) OpenAIThinking {
	if setting == "" {
		return OpenAIThinking{}
	}
	level := setting
	if setting == ReasoningEffortAuto {
		level = ReasoningEffortMedium
	}
	thinks := level != ReasoningEffortOff
	switch control {
	case ReasoningControlToggle:
		return OpenAIThinking{EnableThinking: &thinks}
	case ReasoningControlFixedOn, ReasoningControlFixedOff:
		return OpenAIThinking{}
	default: // effort, budget, and an undeclared form
		if !thinks {
			return OpenAIThinking{ReasoningEffort: reasoningEffortNone}
		}
		return OpenAIThinking{ReasoningEffort: level, RefusesTemperature: true}
	}
}

// FindModelCatalogItem returns the catalogue item a model name resolves to
// for a project: the project's own row of that name first, then a shared
// one, as the gateway resolves the name. Nil when the catalogue has none.
func FindModelCatalogItem(items []CurrentModelCatalogItem, name string, projectID int32) *CurrentModelCatalogItem {
	var shared *CurrentModelCatalogItem
	for i := range items {
		item := &items[i]
		if item.Name != name {
			continue
		}
		if item.ProjectID == projectID {
			return item
		}
		if shared == nil {
			shared = item
		}
	}
	return shared
}

// ModelItemReasoningControl is the control form a catalogue item declares,
// or resolves from the family defaults. A model that says
// supports_reasoning false and declares nothing cannot think: fixed_off. A
// missing item, or one that declares nothing, returns "": the caller keeps
// the form from before ADR-0038.
func ModelItemReasoningControl(item *CurrentModelCatalogItem) string {
	if item == nil {
		return ""
	}
	if item.ReasoningControl != nil {
		return *item.ReasoningControl
	}
	if item.SupportsReasoning != nil && !*item.SupportsReasoning {
		return ReasoningControlFixedOff
	}
	return ""
}

// ModelReasoningInfo is one model's reasoning data, from one catalogue read
// (ADR-0038 decision 3).
type ModelReasoningInfo struct {
	// Found is false when the catalogue has no row of the name.
	Found bool
	// SupportsReasoning is the row's supports_reasoning.
	SupportsReasoning bool
	// Control is ModelItemReasoningControl: the declared or family form,
	// fixed_off for a model that cannot think and declares none, "" for a
	// reasoning model that declares none.
	Control string
	// ToolSteps is reasoning_tool_steps, resolved.
	ToolSteps bool
}

// ModelItemReasoningInfo reads an item's reasoning data. A nil item is not
// found.
func ModelItemReasoningInfo(item *CurrentModelCatalogItem) ModelReasoningInfo {
	if item == nil {
		return ModelReasoningInfo{}
	}
	return ModelReasoningInfo{
		Found:             true,
		SupportsReasoning: item.SupportsReasoning != nil && *item.SupportsReasoning,
		Control:           ModelItemReasoningControl(item),
		ToolSteps:         item.ReasoningToolSteps != nil && *item.ReasoningToolSteps,
	}
}

// SaveReasoningConflict is the save-time rule, the same on every surface (an
// agent version, a toolkit's settings, a chat participant): a setting the
// model cannot honour (a level on a model that cannot think, off on one that
// always thinks) is refused with the table's readable text. A model the
// catalogue does not know is not judged: admission and the run read it again.
func SaveReasoningConflict(info ModelReasoningInfo, effort string) error {
	if !info.Found || effort == "" {
		return nil
	}
	return ReasoningEffortConflict(info.Control, effort)
}

// RunReasoningEffort is the run-time rule, the same on every surface (agent
// admission, a toolkit's engine call): a STORED setting the model cannot
// honour, or one that is not canonical, runs as auto, and is logged at warn
// with the model and the source. It is never refused at run time: the value
// was saved before the model or the rule changed, and the caller cannot fix
// it from a running turn. An empty setting stays empty.
//
// A request body that names a level itself (predict_llm, the AI drafts) is
// not stored: it keeps the save-time refusal.
func RunReasoningEffort(ctx context.Context, control, effort, model, source string) string {
	if effort == "" {
		return ""
	}
	canonical, known := CanonicalReasoningEffort(effort)
	reason := ""
	switch {
	case !known:
		reason = ErrInvalidReasoningEffort.Error()
	default:
		if conflict := ReasoningEffortConflict(control, canonical); conflict != nil {
			reason = conflict.Error()
		}
	}
	if reason == "" {
		return canonical
	}
	slog.WarnContext(ctx, "a stored reasoning_effort the model cannot honour runs as auto",
		"model", model, "source", source, "stored", effort, "reason", reason)
	return ReasoningEffortAuto
}
