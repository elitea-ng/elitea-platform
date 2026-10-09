package storage

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"strings"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/guardrails"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpregistry"
)

// Bounds of the nested-agent walk: how deep an `application` tool chain is
// followed from the turn's agent, and how many versions are read in total.
// The SDK nests agents a handful of levels at most; a chain past these bounds
// is refused rather than walked.
const (
	maxRemoteToolNestingDepth = 4
	maxRemoteToolVersionReads = 32
)

var (
	// ErrRemoteToolNotInAgent: the toolkit is not one of the named version's
	// tools, the tool is not in its selection, the version is not the turn's
	// agent or reachable from it, or the turn has no agent at all.
	ErrRemoteToolNotInAgent = errors.New("the tool is not one the running agent may call")
	// ErrRemoteToolBlocked: the guardrails policy blocks the toolkit or tool —
	// the predicates the agent freeze drops them with.
	ErrRemoteToolBlocked = errors.New("the tool is blocked by the guardrails policy")
	// ErrRemoteToolkitRefMismatch: the toolkit_ref is not the one the resolved
	// version hands out for this toolkit.
	ErrRemoteToolkitRefMismatch = errors.New("the toolkit reference does not name this toolkit of this version")
)

// ClientGuardrailResolver is the platform guardrails policy, the same source
// the agent freeze reads.
type ClientGuardrailResolver interface {
	ResolveCurrentAgentGuardrails(context.Context) (guardrails.Policy, error)
}

// WithGuardrails gives the service the policy AuthorizeRemoteTool needs. It is
// separate from the constructor because resolveApplicationVersion does not
// need it; AuthorizeRemoteTool answers ErrContentUnavailable without it.
func (service *ClientApplicationVersionService) WithGuardrails(policy ClientGuardrailResolver) *ClientApplicationVersionService {
	if service != nil && policy != nil {
		service.guardrails = policy
	}
	return service
}

// RemoteToolAuthorization is one remote toolkit call to authorize.
type RemoteToolAuthorization struct {
	ProjectID int64
	ActorID   int64
	// TurnApplicationID and TurnVersionID are the live local turn's agent: the
	// version its answering participant is mapped to (0 for a model turn).
	TurnApplicationID int64
	TurnVersionID     int64
	// ApplicationID and VersionID are the version the desktop says it is
	// running: the turn's agent, or a nested agent reachable from it.
	ApplicationID int64
	VersionID     int64
	ToolkitID     int64
	ToolkitRef    string
	ToolName      string
}

// RemoteToolGrant is what the authorization resolved.
type RemoteToolGrant struct {
	ToolkitType string
	ToolkitName string
	// Sensitive is set when the policy marks the tool sensitive: the call may
	// run only with the caller's confirmation.
	Sensitive *guardrails.SensitiveAction
	// LLMModel and LLMSettings are the VERSION's model (resolved against the
	// project's model catalogue by the freeze) and its temperature,
	// max_tokens and reasoning_effort, for a toolkit that calls a model. The
	// caller never chooses them.
	LLMModel    string
	LLMSettings json.RawMessage
}

// AuthorizeRemoteTool decides whether one remote toolkit call has the
// authority a cloud chat turn of the same agent would have (ADR-0029 decision
// 5b). It answers the grant, or ErrRemoteToolNotInAgent, ErrRemoteToolBlocked,
// ErrRemoteToolkitRefMismatch, ErrClientApplicationVersionUnresolvable,
// ErrContentUnavailable or the context's error.
//
// The rules, each the chat turn's own:
//
//   - The named version is the turn's agent version, or a nested agent
//     reachable from it through `application` tools. A nested agent's tools
//     are what a cloud turn of the parent can call through that child, so the
//     desktop, which runs the child locally from its own resolved version, may
//     call them too — and nothing a turn of the parent could not reach.
//   - The version is frozen with the SAME freeze a cloud turn uses (the 5a
//     projection's), so the toolkit must be one of its frozen tools and the
//     tool one of that toolkit's frozen selected_tools, exactly the selection
//     the 5a document carries. A selection the author left empty means every
//     tool of the toolkit, as the SDK reads it (all_tools); one the freeze
//     emptied by removing blocked names means none.
//   - A toolkit or tool the guardrails policy blocks (the freeze's predicates,
//     ToolkitBlocked and ToolBlocked) is refused as blocked.
//   - The toolkit_ref must be the one 5a hands out for this toolkit of this
//     version.
//   - A sensitive tool (the SDK's sensitive-tool HITL classifier) is granted
//     with its approval copy; the caller must then hold a confirmation.
func (service *ClientApplicationVersionService) AuthorizeRemoteTool(
	ctx context.Context,
	request RemoteToolAuthorization,
) (RemoteToolGrant, error) {
	if service == nil || service.versions == nil || service.freezer == nil || service.guardrails == nil || ctx == nil {
		return RemoteToolGrant{}, ErrContentUnavailable
	}
	if err := ctx.Err(); err != nil {
		return RemoteToolGrant{}, err
	}
	identity := ClientVersionIdentity{ProjectID: request.ProjectID, ApplicationID: request.ApplicationID, VersionID: request.VersionID}
	if !identity.valid() || request.ActorID <= 0 || request.ActorID > math.MaxInt32 ||
		request.ToolkitID <= 0 || request.ToolkitID > math.MaxInt32 || request.ToolName == "" {
		return RemoteToolGrant{}, ErrRemoteToolNotInAgent
	}
	if request.TurnApplicationID <= 0 || request.TurnVersionID <= 0 {
		return RemoteToolGrant{}, ErrRemoteToolNotInAgent
	}
	reachable, err := service.reachableFromTurn(ctx, request)
	if err != nil {
		return RemoteToolGrant{}, err
	}
	if !reachable {
		return RemoteToolGrant{}, ErrRemoteToolNotInAgent
	}

	record, frozen, err := freezeSavedApplicationVersion(
		ctx, service.versions, service.freezer, request.ProjectID, request.ActorID,
		uint64(request.ApplicationID), uint64(request.VersionID),
	)
	if err != nil {
		return RemoteToolGrant{}, clientFreezeError(ctx, err)
	}
	defer clearContentBytes(frozen)

	// The SAVED entry decides the toolkit's type, so a toolkit the freeze
	// dropped as blocked is still recognised (and refused as blocked, not as
	// absent).
	savedTool, found, err := savedToolkitEntry(record.VersionDetails, request.ToolkitID)
	if err != nil {
		return RemoteToolGrant{}, ErrClientApplicationVersionUnresolvable
	}
	if !found {
		return RemoteToolGrant{}, ErrRemoteToolNotInAgent
	}
	policy, err := service.guardrails.ResolveCurrentAgentGuardrails(ctx)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return RemoteToolGrant{}, contextErr
		}
		return RemoteToolGrant{}, ErrContentUnavailable
	}
	if policy.ToolkitBlocked(savedTool.toolkitType) || policy.ToolBlocked(savedTool.toolkitType, request.ToolName) {
		return RemoteToolGrant{}, ErrRemoteToolBlocked
	}

	frozenTool, found, err := frozenToolkitEntry(frozen, request.ToolkitID)
	if err != nil {
		return RemoteToolGrant{}, ErrClientApplicationVersionUnresolvable
	}
	if !found || frozenTool.toolkitType != savedTool.toolkitType {
		// The freeze omitted it for another reason (a toolkit schema this
		// runtime does not have): a cloud turn could not call it either.
		slog.InfoContext(ctx, "remote toolkit call: toolkit absent from the frozen version",
			"project_id", request.ProjectID, "toolkit_id", request.ToolkitID)
		return RemoteToolGrant{}, ErrRemoteToolNotInAgent
	}
	want := ClientToolkitRef(identity, request.ToolkitID, frozenTool.toolkitType)
	if subtle.ConstantTimeCompare([]byte(want), []byte(request.ToolkitRef)) != 1 {
		return RemoteToolGrant{}, ErrRemoteToolkitRefMismatch
	}
	// Membership is decided on the SAME selection the desktop's document
	// carries (clientToolkitSelection): the frozen one, where a selection the
	// freeze emptied means no tool, not every tool. Blocked names were refused
	// above as blocked.
	selected, allTools := clientToolkitSelection(savedTool.selected, frozenTool.selected)
	if !allTools && !containsExact(selected, request.ToolName) {
		return RemoteToolGrant{}, ErrRemoteToolNotInAgent
	}
	grant := RemoteToolGrant{ToolkitType: frozenTool.toolkitType, ToolkitName: frozenTool.toolkitName}
	grant.LLMModel, grant.LLMSettings = frozenRunModel(frozen)
	label := frozenTool.toolkitName
	if label == "" {
		label = frozenTool.toolkitType
	}
	if action, sensitive := policy.SensitiveAction(request.ToolName, label, frozenTool.toolkitType, frozenTool.toolkitType, frozenTool.toolkitName); sensitive {
		grant.Sensitive = &action
	}
	return grant, nil
}

// reachableFromTurn walks the turn's agent and its nested `application`
// tools, breadth first and bounded, over the SAVED versions.
func (service *ClientApplicationVersionService) reachableFromTurn(ctx context.Context, request RemoteToolAuthorization) (bool, error) {
	type node struct{ applicationID, versionID int64 }
	target := node{request.ApplicationID, request.VersionID}
	frontier := []node{{request.TurnApplicationID, request.TurnVersionID}}
	seen := map[node]bool{}
	reads := 0
	for depth := 0; depth <= maxRemoteToolNestingDepth && len(frontier) > 0; depth++ {
		var next []node
		for _, current := range frontier {
			if current == target {
				return true, nil
			}
			if seen[current] {
				continue
			}
			seen[current] = true
			if depth == maxRemoteToolNestingDepth {
				continue
			}
			if reads == maxRemoteToolVersionReads {
				return false, nil
			}
			reads++
			record, err := service.versions.ReadCurrentApplicationVersion(ctx, request.ProjectID, current.applicationID, current.versionID)
			if err != nil {
				if contextErr := ctx.Err(); contextErr != nil {
					return false, contextErr
				}
				if errors.Is(err, ErrContentNotFound) {
					continue
				}
				return false, ErrContentUnavailable
			}
			children, err := savedNestedApplications(record.VersionDetails)
			if err != nil {
				continue
			}
			for _, child := range children {
				next = append(next, node{child[0], child[1]})
			}
		}
		frontier = next
	}
	return false, nil
}

func clientFreezeError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, ErrContentNotFound) {
		return ErrRemoteToolNotInAgent
	}
	var stage *savedApplicationVersionFreezeError
	if errors.As(err, &stage) && !stage.read &&
		errors.Is(err, agentexecutionapp.ErrUnsupportedCurrentAgentStart) &&
		!errors.Is(err, configurationapp.ErrCurrentToolkitSettingsDependency) {
		return ErrClientApplicationVersionUnresolvable
	}
	return ErrContentUnavailable
}

type clientToolkitEntry struct {
	toolkitType string
	toolkitName string
	selected    []string
}

func decodeClientTools(document json.RawMessage) ([]map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	var version struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := decoder.Decode(&version); err != nil {
		return nil, err
	}
	return version.Tools, nil
}

// savedToolkitEntry finds a toolkit (not an agent, not a platform MCP) by id
// in a SAVED version's tools.
func savedToolkitEntry(document json.RawMessage, toolkitID int64) (clientToolkitEntry, bool, error) {
	return findClientToolkit(document, toolkitID)
}

// frozenToolkitEntry finds the same toolkit in the FROZEN version, whose
// settings carry the selection the freeze kept.
func frozenToolkitEntry(document json.RawMessage, toolkitID int64) (clientToolkitEntry, bool, error) {
	return findClientToolkit(document, toolkitID)
}

func findClientToolkit(document json.RawMessage, toolkitID int64) (clientToolkitEntry, bool, error) {
	tools, err := decodeClientTools(document)
	if err != nil {
		return clientToolkitEntry{}, false, err
	}
	for _, tool := range tools {
		toolType, _ := tool["type"].(string)
		if toolType == "" || toolType == "application" {
			continue
		}
		if _, internal := mcpregistry.InternalBuilderForType(toolType); internal {
			continue
		}
		id, ok := positiveClientJSONInteger(tool["id"])
		if !ok || id != toolkitID {
			continue
		}
		entry := clientToolkitEntry{toolkitType: toolType}
		if name, ok := tool["toolkit_name"].(string); ok && name != "" {
			entry.toolkitName = name
		} else if name, ok := tool["name"].(string); ok {
			entry.toolkitName = name
		}
		entry.selected = selectedToolNames(tool)
		return entry, true, nil
	}
	return clientToolkitEntry{}, false, nil
}

// savedNestedApplications answers the (application id, version id) pairs of a
// saved version's `application` tools.
func savedNestedApplications(document json.RawMessage) ([][2]int64, error) {
	tools, err := decodeClientTools(document)
	if err != nil {
		return nil, err
	}
	var children [][2]int64
	for _, tool := range tools {
		if tool["type"] != "application" {
			continue
		}
		settings, ok := tool["settings"].(map[string]any)
		if !ok {
			continue
		}
		applicationID, okApplication := positiveClientJSONInteger(settings["application_id"])
		versionID, okVersion := positiveClientJSONInteger(settings["application_version_id"])
		if okApplication && okVersion {
			children = append(children, [2]int64{applicationID, versionID})
		}
	}
	return children, nil
}

func containsExact(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// frozenRunModel reads the frozen version's model and the model settings a
// tool run accepts (toolkitcalltool's validModelSettings: temperature 0-2,
// max_tokens -1..1048576, a known reasoning_effort). A value outside those
// bounds is left out rather than failing the call.
func frozenRunModel(frozen json.RawMessage) (string, json.RawMessage) {
	var version struct {
		LLMSettings map[string]json.RawMessage `json:"llm_settings"`
	}
	if json.Unmarshal(frozen, &version) != nil || version.LLMSettings == nil {
		return "", nil
	}
	var model string
	if raw, ok := version.LLMSettings["model_name"]; ok {
		_ = json.Unmarshal(raw, &model)
	}
	model = strings.TrimSpace(model)
	if len(model) > 256 || strings.ContainsAny(model, "\x00\r\n") {
		model = ""
	}
	settings := map[string]any{}
	var temperature float64
	if raw, ok := version.LLMSettings["temperature"]; ok && json.Unmarshal(raw, &temperature) == nil &&
		!math.IsNaN(temperature) && temperature >= 0 && temperature <= 2 {
		settings["temperature"] = temperature
	}
	var maxTokens int64
	if raw, ok := version.LLMSettings["max_tokens"]; ok && json.Unmarshal(raw, &maxTokens) == nil &&
		maxTokens >= -1 && maxTokens <= 1048576 {
		settings["max_tokens"] = maxTokens
	}
	var effort string
	if raw, ok := version.LLMSettings["reasoning_effort"]; ok && json.Unmarshal(raw, &effort) == nil {
		switch effort {
		case "none", "minimal", "low", "medium", "high", "xhigh", "max":
			settings["reasoning_effort"] = effort
		}
	}
	if len(settings) == 0 {
		return model, nil
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return model, nil
	}
	return model, encoded
}
