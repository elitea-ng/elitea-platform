package connectorapproval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
)

// RefusalCode settles a run whose command names a connector its project may
// not use: the `code` of its `execution.failed` event and of the stored
// refusal (elitea_runtime.command_outbox.refusal_code, shared 0207).
const RefusalCode = "CONNECTOR_NOT_APPROVED"

// ChatErrorCode is the same refusal as a chat answer's `error_code`.
const ChatErrorCode = "connector_not_approved"

// RefusalError is a command that can never be prepared as it stands: it
// names a connector the run's project may not use under approved_only (it
// was configured, or the run frozen, before the mode switched or before the
// approval was withdrawn). It has the commandbus refusal shape, so the run
// fails at once with the message.
type RefusalError struct{ Message string }

func (e *RefusalError) Error() string { return "connector not approved: " + e.Message }

// DispatchRefusal answers the code and message the run fails with.
func (e *RefusalError) DispatchRefusal() (string, string) { return RefusalCode, e.Message }

// EgressPolicyResolver is the commandbus seam CommandGate wraps
// (workeregress.Resolver).
type EgressPolicyResolver interface {
	WorkerEgressPolicy(ctx context.Context, command *runtimev1.WorkerCommandV1) (*runtimev1.WorkerEgressPolicyV1, error)
}

// InputReader returns the content of the named entries of an input bundle
// (workeregress.InputReader; repos.WorkerEgressSources).
type InputReader interface {
	InputEntries(ctx context.Context, inputBundleID string, entryIDs []string) (map[string][]byte, error)
}

// CommandGate refuses a worker command that names a connector its project
// may not use, then answers the wrapped egress policy. It sits on every
// producer's egress seam, so every command type passes it: agent runs,
// direct reads, tool calls, tool discovery and index ingests.
type CommandGate struct {
	gate   *Gate
	inputs InputReader
	next   EgressPolicyResolver
}

// NewCommandGate wraps next. A nil gate answers next alone.
func NewCommandGate(gate *Gate, inputs InputReader, next EgressPolicyResolver) (*CommandGate, error) {
	if inputs == nil || next == nil {
		return nil, errors.New("connectorapproval: the input reader and the egress resolver are required")
	}
	return &CommandGate{gate: gate, inputs: inputs, next: next}, nil
}

// WorkerEgressPolicy admits the command's connectors, then answers next.
func (c *CommandGate) WorkerEgressPolicy(ctx context.Context, command *runtimev1.WorkerCommandV1) (*runtimev1.WorkerEgressPolicyV1, error) {
	if err := c.Admit(ctx, command); err != nil {
		return nil, err
	}
	return c.next.WorkerEgressPolicy(ctx, command)
}

// commandToolkit is one toolkit a command names.
type commandToolkit struct {
	toolkitType string
	settings    map[string]any
	// toolName is the one tool a direct call or read runs; empty for an
	// agent run or a discovery.
	toolName string
}

// Admit refuses (RefusalError) a command naming an unapproved connector. A
// source that cannot be read is a plain error: the dispatch is retried.
func (c *CommandGate) Admit(ctx context.Context, command *runtimev1.WorkerCommandV1) error {
	if c == nil || c.gate == nil || command == nil {
		return nil
	}
	projectID, _ := strconv.ParseInt(strings.TrimSpace(command.GetResourceProjectId()), 10, 64)
	view, err := c.gate.ForProject(ctx, projectID, command.GetSecurityContextRef() != nil)
	if err != nil {
		return err
	}
	if !view.ApprovedOnly() {
		return nil
	}
	toolkits, err := c.toolkits(ctx, command)
	if err != nil {
		return err
	}
	for _, toolkit := range toolkits {
		decision := view.Decide(toolkit.toolkitType, toolkit.settings)
		if !decision.Allowed {
			return &RefusalError{Message: RefusalMessage(decision)}
		}
		if decision.Kind != TargetCustomMCP || !decision.Gated {
			continue
		}
		if toolkit.toolName != "" {
			if !PinNames(decision.Pins)[toolkit.toolName] {
				return &RefusalError{Message: fmt.Sprintf("The tool %q of the MCP server %s is not approved. "+
					"Request the connector again with this tool, and a connector approver must approve it.",
					truncate(toolkit.toolName, 128), truncate(decision.Target, 256))}
			}
			continue
		}
		if toolkit.settings != nil && !frozenPinsWithin(toolkit.settings[PinnedToolsSetting], decision.Pins) {
			return &RefusalError{Message: fmt.Sprintf("This run was prepared before the MCP server %s was "+
				"approved as it is now, so its tools are not pinned. Start the run again.", truncate(decision.Target, 256))}
		}
	}
	return nil
}

// frozenPinsWithin reports whether a frozen run's `pinned_tools` is present
// and names only currently approved pins.
func frozenPinsWithin(raw any, approved []PinnedTool) bool {
	list, ok := raw.([]any)
	if !ok {
		return false
	}
	allowed := map[PinnedTool]bool{}
	for _, pin := range approved {
		allowed[pin] = true
	}
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			return false
		}
		name, _ := entry["name"].(string)
		digest, _ := entry["digest"].(string)
		if !allowed[PinnedTool{Name: name, Digest: digest}] {
			return false
		}
	}
	return true
}

func (c *CommandGate) toolkits(ctx context.Context, command *runtimev1.WorkerCommandV1) ([]commandToolkit, error) {
	bundle := command.GetInputBundleRef().GetInputBundleId()
	var entryID string
	var decode func([]byte) ([]commandToolkit, error)
	switch {
	case command.GetAgentExecution() != nil:
		entryID, decode = command.GetAgentExecution().GetRequestEntryId(), toolkitsFromAgentInput
	case command.GetToolkitExecuteRead() != nil:
		entryID, decode = command.GetToolkitExecuteRead().GetRequestEntryId(), toolkitsFromReadInput
	case command.GetToolkitCallTool() != nil:
		call := command.GetToolkitCallTool()
		entryID = call.GetSettingsEntryId()
		decode = func(content []byte) ([]commandToolkit, error) {
			settings, err := settingsDocument(content)
			if err != nil {
				return nil, err
			}
			return []commandToolkit{{toolkitType: call.GetToolkitType(), settings: settings, toolName: call.GetToolName()}}, nil
		}
	case command.GetToolkitAvailableTools() != nil:
		discovery := command.GetToolkitAvailableTools()
		entryID = discovery.GetSettingsEntryId()
		decode = func(content []byte) ([]commandToolkit, error) {
			settings, err := settingsDocument(content)
			if err != nil {
				return nil, err
			}
			// A discovery lists, it runs nothing: no frozen pins to check.
			return []commandToolkit{{toolkitType: discovery.GetToolkitType(), settings: withoutPinCheck(settings)}}, nil
		}
	case command.GetIndexIngest() != nil:
		entryID, decode = command.GetIndexIngest().GetToolkitConfigurationEntryId(), toolkitsFromTypedDocument
	default:
		return nil, nil
	}
	if bundle == "" || entryID == "" {
		return nil, &RefusalError{Message: "The run's worker command names no toolkit input, so its connectors cannot be checked. Start the run again."}
	}
	contents, err := c.inputs.InputEntries(ctx, bundle, []string{entryID})
	if err != nil {
		return nil, fmt.Errorf("connectorapproval: read the command's input: %w", err)
	}
	content, ok := contents[entryID]
	if !ok {
		return nil, &RefusalError{Message: "The run's toolkit input is missing, so its connectors cannot be checked. Start the run again."}
	}
	toolkits, err := decode(content)
	if err != nil {
		return nil, &RefusalError{Message: fmt.Sprintf("The run's toolkit settings cannot be read (%v), so its connectors cannot be checked. Start the run again.", err)}
	}
	return toolkits, nil
}

// withoutPinCheck marks settings whose frozen pins are not judged (nil
// settings skip the frozen-pin check, so a copy without the URL would lose
// the connector): it returns the settings with a placeholder pin list that
// always passes.
func withoutPinCheck(settings map[string]any) map[string]any {
	if settings == nil {
		return map[string]any{PinnedToolsSetting: []any{}}
	}
	copied := make(map[string]any, len(settings)+1)
	for key, value := range settings {
		copied[key] = value
	}
	copied[PinnedToolsSetting] = []any{}
	return copied
}

var errUnreadable = errors.New("the document is not readable")

// settingsDocument decodes a toolkit settings entry: the settings object
// itself, or a toolkit document carrying `settings`.
func settingsDocument(content []byte) (map[string]any, error) {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return map[string]any{}, nil
	}
	var document map[string]any
	if err := json.Unmarshal(trimmed, &document); err != nil {
		return nil, errUnreadable
	}
	if settings, ok := document["settings"].(map[string]any); ok {
		return settings, nil
	}
	return document, nil
}

func toolkitsFromReadInput(content []byte) ([]commandToolkit, error) {
	input := &runtimev1.ToolkitExecuteReadInputV1{}
	if err := proto.Unmarshal(content, input); err != nil {
		return nil, errors.New("the direct toolkit input is not readable")
	}
	settings, err := settingsDocument(input.GetToolkit())
	if err != nil {
		return nil, err
	}
	return []commandToolkit{{toolkitType: input.GetToolkitType(), settings: settings, toolName: input.GetToolName()}}, nil
}

func toolkitsFromTypedDocument(content []byte) ([]commandToolkit, error) {
	var typed struct {
		Type     string         `json:"type"`
		Settings map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(content), &typed); err != nil {
		return nil, errUnreadable
	}
	return []commandToolkit{{toolkitType: typed.Type, settings: withoutPinCheck(typed.Settings)}}, nil
}

// toolkitsFromAgentInput reads an agent run's frozen toolkit set: the
// entries of the input's `tools` and of its application's
// `version_details.tools`.
func toolkitsFromAgentInput(content []byte) ([]commandToolkit, error) {
	input := &runtimev1.AgentExecutionInputV1{}
	if err := proto.Unmarshal(content, input); err != nil {
		return nil, errors.New("the agent input is not readable")
	}
	var toolkits []commandToolkit
	read := func(raw []byte) error {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || string(trimmed) == "null" {
			return nil
		}
		var entries []map[string]any
		if err := json.Unmarshal(trimmed, &entries); err != nil {
			return errUnreadable
		}
		for _, entry := range entries {
			toolkitType, _ := entry["type"].(string)
			settings, _ := entry["settings"].(map[string]any)
			if meta, _ := entry["meta"].(map[string]any); meta != nil {
				if internal, _ := meta["internal_builder"].(bool); internal {
					continue
				}
			}
			toolkits = append(toolkits, commandToolkit{toolkitType: toolkitType, settings: settings})
		}
		return nil
	}
	if err := read(input.GetTools()); err != nil {
		return nil, err
	}
	if application := input.GetApplication(); len(application) != 0 {
		var frozen struct {
			VersionDetails *struct {
				Tools json.RawMessage `json:"tools"`
			} `json:"version_details"`
		}
		if err := json.Unmarshal(application, &frozen); err != nil {
			return nil, errors.New("the frozen application is not readable")
		}
		if frozen.VersionDetails != nil {
			if err := read(frozen.VersionDetails.Tools); err != nil {
				return nil, err
			}
		}
	}
	return toolkits, nil
}
