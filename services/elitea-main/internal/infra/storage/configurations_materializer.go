package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"google.golang.org/protobuf/proto"
)

const maxCurrentFrozenConfigurationScopes = 256

var errInvalidCurrentFrozenConfiguration = errors.New("invalid frozen current configuration")

// CurrentConfigurationsMaterializer is the private data-plane adapter between
// a claimed index input and the current Configurations domain. Admission has
// already frozen configuration and nested-toolkit values while keeping vault
// references sealed. Claim time only redeems those frozen bytes; it never
// resolves a mutable configuration title or toolkit ID again.
//
// Plaintext exists only in this bounded response and in the claimed worker
// process that requested it. Each frozen configuration is unsecreted through
// its embedded configuration_project_id rather than the invoking project.
type CurrentConfigurationsMaterializer struct {
	unsecreter configurationapp.CurrentExpansionUnsecreter
	prebuilt   CurrentAgentPrebuiltMCPResolver
}

// CurrentAgentPrebuiltMCPResolver resolves one trusted prebuilt MCP definition.
type CurrentAgentPrebuiltMCPResolver interface {
	ResolveCurrentAgentPrebuiltMCP(
		context.Context,
		int32,
		int32,
		string,
		map[string]any,
		func(map[string]any) (map[string]any, error),
	) (map[string]any, bool, error)
}

func NewCurrentConfigurationsMaterializer(
	unsecreter configurationapp.CurrentExpansionUnsecreter,
) (*CurrentConfigurationsMaterializer, error) {
	return newCurrentConfigurationsMaterializer(unsecreter, nil)
}

// NewCurrentAgentConfigurationsMaterializer enables claim-time prebuilt MCP resolution.
func NewCurrentAgentConfigurationsMaterializer(
	unsecreter configurationapp.CurrentExpansionUnsecreter,
	prebuilt CurrentAgentPrebuiltMCPResolver,
) (*CurrentConfigurationsMaterializer, error) {
	if prebuilt == nil {
		return nil, errors.New("current agent prebuilt MCP resolver is required")
	}
	return newCurrentConfigurationsMaterializer(unsecreter, prebuilt)
}

func newCurrentConfigurationsMaterializer(
	unsecreter configurationapp.CurrentExpansionUnsecreter,
	prebuilt CurrentAgentPrebuiltMCPResolver,
) (*CurrentConfigurationsMaterializer, error) {
	if unsecreter == nil {
		return nil, errors.New("current configuration unsecreter is required")
	}
	return &CurrentConfigurationsMaterializer{unsecreter: unsecreter, prebuilt: prebuilt}, nil
}

func (m *CurrentConfigurationsMaterializer) MaterializeContent(
	ctx context.Context,
	authorization ContentAuthorization,
	source []byte,
	maxBytes int64,
) ([]byte, error) {
	if ctx == nil || maxBytes <= 0 || int64(len(source)) > maxBytes {
		return nil, ErrContentRejected
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if authorization.CapabilityID == executiondomain.AgentApplicationCapability ||
		authorization.CapabilityID == executiondomain.AgentAdhocCapability {
		projectID, ok := positiveCurrentMaterializationID(authorization.ResourceProjectID)
		if !ok {
			return nil, ErrContentRejected
		}
		actorID, ok := positiveCurrentMaterializationID(authorization.ActorID)
		if !ok {
			return nil, ErrContentRejected
		}
		if authorization.SemanticRole != executiondomain.AgentExecutionRequestRole {
			return nil, ErrContentRejected
		}
		return m.materializeAgentExecution(
			ctx,
			projectID,
			actorID,
			authorization.CapabilityID,
			source,
			maxBytes,
		)
	}
	if authorization.CapabilityID != executiondomain.IndexIngestCapability {
		// The shared content listener also serves validation inputs. Those bytes
		// have no frozen configured-toolkit references and remain byte-for-byte
		// stable.
		return source, nil
	}

	projectID, ok := positiveCurrentMaterializationID(authorization.ResourceProjectID)
	if !ok {
		return nil, ErrContentRejected
	}
	if _, ok := positiveCurrentMaterializationID(authorization.ActorID); !ok {
		return nil, ErrContentRejected
	}

	switch authorization.SemanticRole {
	case executiondomain.IndexToolkitConfigurationRole:
		return m.materializeToolkit(ctx, projectID, source, maxBytes)
	case executiondomain.IndexToolParametersRole,
		executiondomain.IndexLLMConfigurationRole,
		executiondomain.IndexMCPTokensRole:
		return m.materializeProjectObject(ctx, projectID, source, maxBytes)
	case executiondomain.IndexLLMModelRole:
		if !validCurrentMaterializationString(source) {
			return nil, ErrContentRejected
		}
		return source, nil
	case executiondomain.IndexEmbeddingBindingRole:
		// Admission already canonicalized and digested this non-secret
		// immutable document. It must be returned byte-for-byte so the worker
		// can verify the exact source digest and must never enter unsecreting.
		return source, nil
	default:
		return nil, ErrContentRejected
	}
}

func (m *CurrentConfigurationsMaterializer) materializeAgentExecution(
	ctx context.Context,
	projectID int32,
	actorID int32,
	capabilityID string,
	source []byte,
	maxBytes int64,
) ([]byte, error) {
	var request runtimev1.AgentExecutionInputV1
	if err := proto.Unmarshal(source, &request); err != nil {
		return nil, ErrContentRejected
	}
	canonical, err := proto.MarshalOptions{Deterministic: true}.Marshal(&request)
	if err != nil || !bytes.Equal(canonical, source) {
		clearContentBytes(canonical)
		return nil, ErrContentRejected
	}
	clearContentBytes(canonical)

	walker := currentFrozenConfigurationWalker{unsecreter: m.unsecreter}
	switch capabilityID {
	case executiondomain.AgentApplicationCapability:
		application, decodeErr := decodeCurrentMaterializationObject(request.GetApplication())
		if decodeErr != nil {
			return nil, ErrContentRejected
		}
		version, ok := application["version_details"].(map[string]any)
		if !ok || version == nil {
			return nil, ErrContentRejected
		}
		tools, ok := version["tools"].([]any)
		if !ok {
			return nil, ErrContentRejected
		}
		if err := m.materializeCurrentAgentTools(ctx, projectID, actorID, tools, &walker); err != nil {
			return nil, currentMaterializationError(ctx, err)
		}
		version["tools"] = tools
		application["version_details"] = version
		request.Application, err = encodeCurrentMaterializationObject(application, maxBytes)
		if err != nil {
			return nil, err
		}
	case executiondomain.AgentAdhocCapability:
		tools, decodeErr := decodeCurrentMaterializationArray(request.GetTools())
		if decodeErr != nil {
			return nil, ErrContentRejected
		}
		if err := m.materializeCurrentAgentTools(ctx, projectID, actorID, tools, &walker); err != nil {
			return nil, currentMaterializationError(ctx, err)
		}
		request.Tools, err = encodeCurrentMaterializationArray(tools, maxBytes)
		if err != nil {
			return nil, err
		}
	default:
		return nil, ErrContentRejected
	}

	result, err := proto.MarshalOptions{Deterministic: true}.Marshal(&request)
	if err != nil || len(result) == 0 || int64(len(result)) > maxBytes {
		clearContentBytes(result)
		return nil, ErrContentRejected
	}
	return result, nil
}

func (m *CurrentConfigurationsMaterializer) materializeCurrentAgentTools(
	ctx context.Context,
	projectID int32,
	actorID int32,
	tools []any,
	walker *currentFrozenConfigurationWalker,
) error {
	if walker == nil {
		return errInvalidCurrentFrozenConfiguration
	}
	for index, value := range tools {
		tool, ok := value.(map[string]any)
		if !ok {
			return errInvalidCurrentFrozenConfiguration
		}
		settings, ok := tool["settings"].(map[string]any)
		if !ok || settings == nil {
			return errInvalidCurrentFrozenConfiguration
		}
		toolkitType, ok := tool["type"].(string)
		if !ok || toolkitType == "" || len(toolkitType) > configurationapp.MaxCurrentToolkitSettingsIdentifier ||
			strings.ContainsAny(toolkitType, "\x00\r\n") {
			return errInvalidCurrentFrozenConfiguration
		}
		var materialized map[string]any
		var err error
		if toolkitType == "mcp_config" || strings.HasPrefix(toolkitType, "mcp_") {
			if m.prebuilt == nil {
				return errInvalidCurrentFrozenConfiguration
			}
			materialized, ok, err = m.prebuilt.ResolveCurrentAgentPrebuiltMCP(
				ctx,
				projectID,
				actorID,
				toolkitType,
				settings,
				func(admitted map[string]any) (map[string]any, error) {
					return walker.materializeOwnedMap(ctx, projectID, admitted, 0, true)
				},
			)
			if err != nil {
				return err
			}
			if !ok || materialized == nil {
				return errInvalidCurrentFrozenConfiguration
			}
		} else {
			materialized, err = walker.materializeOwnedMap(ctx, projectID, settings, 0, true)
			if err != nil {
				return err
			}
		}
		tool["settings"] = materialized
		tools[index] = tool
	}
	return nil
}

func (m *CurrentConfigurationsMaterializer) materializeToolkit(
	ctx context.Context,
	projectID int32,
	source []byte,
	maxBytes int64,
) ([]byte, error) {
	toolkit, err := decodeCurrentMaterializationObject(source)
	if err != nil {
		return nil, ErrContentRejected
	}
	if _, ok := toolkit["settings"].(map[string]any); !ok {
		return nil, ErrContentRejected
	}
	toolkitType, ok := toolkit["type"].(string)
	if !ok || toolkitType == "" || len(toolkitType) > configurationapp.MaxCurrentToolkitSettingsIdentifier ||
		strings.ContainsAny(toolkitType, "\x00\r\n") {
		return nil, ErrContentRejected
	}

	walker := currentFrozenConfigurationWalker{unsecreter: m.unsecreter}
	materialized, err := walker.materializeOwnedMap(ctx, projectID, toolkit, 0, true)
	if err != nil {
		return nil, currentMaterializationError(ctx, err)
	}
	return encodeCurrentMaterializationObject(materialized, maxBytes)
}

// currentFrozenConfigurationWalker masks nested configuration scopes while a
// parent object is unsecreted. That prevents an unresolved reference owned by
// one configuration from accidentally falling through to another project's
// vault. The masked subtree is independently materialized through the project
// identity frozen into its configuration metadata and then restored.
type currentFrozenConfigurationWalker struct {
	unsecreter     configurationapp.CurrentExpansionUnsecreter
	nodes          int
	configurations int
}

type currentFrozenConfigurationRestore struct {
	replace     bool
	replacement any
	object      map[string]*currentFrozenConfigurationRestore
	array       map[int]*currentFrozenConfigurationRestore
}

func (w *currentFrozenConfigurationWalker) materializeOwnedMap(
	ctx context.Context,
	projectID int32,
	object map[string]any,
	depth int,
	countRoot bool,
) (map[string]any, error) {
	if countRoot {
		if err := w.visit(ctx, depth); err != nil {
			return nil, err
		}
	}

	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	masked := make(map[string]any, len(object))
	restores := make(map[string]*currentFrozenConfigurationRestore)
	for _, key := range keys {
		value, restore, err := w.maskNestedConfigurations(ctx, object[key], depth+1)
		if err != nil {
			return nil, err
		}
		masked[key] = value
		if restore != nil {
			restores[key] = restore
		}
	}

	unsecreted, err := w.unsecreter.Unsecret(ctx, projectID, masked)
	if err != nil {
		return nil, err
	}
	if unsecreted == nil {
		return nil, errInvalidCurrentFrozenConfiguration
	}
	for key, restore := range restores {
		value, exists := unsecreted[key]
		if !exists {
			return nil, errInvalidCurrentFrozenConfiguration
		}
		value, err = restore.apply(value)
		if err != nil {
			return nil, err
		}
		unsecreted[key] = value
	}
	return unsecreted, nil
}

func (w *currentFrozenConfigurationWalker) maskNestedConfigurations(
	ctx context.Context,
	value any,
	depth int,
) (any, *currentFrozenConfigurationRestore, error) {
	if err := w.visit(ctx, depth); err != nil {
		return nil, nil, err
	}

	switch value := value.(type) {
	case map[string]any:
		projectID, configuration, err := currentFrozenConfigurationOwner(value)
		if err != nil {
			return nil, nil, err
		}
		if configuration {
			if w.configurations >= maxCurrentFrozenConfigurationScopes {
				return nil, nil, errInvalidCurrentFrozenConfiguration
			}
			w.configurations++
			materialized, err := w.materializeOwnedMap(ctx, projectID, value, depth, false)
			if err != nil {
				return nil, nil, err
			}
			delete(materialized, configurationapp.CurrentFrozenConfigurationMarker)
			return nil, &currentFrozenConfigurationRestore{
				replace:     true,
				replacement: materialized,
			}, nil
		}

		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		masked := make(map[string]any, len(value))
		restores := make(map[string]*currentFrozenConfigurationRestore)
		for _, key := range keys {
			child, restore, err := w.maskNestedConfigurations(ctx, value[key], depth+1)
			if err != nil {
				return nil, nil, err
			}
			masked[key] = child
			if restore != nil {
				restores[key] = restore
			}
		}
		if len(restores) == 0 {
			return masked, nil, nil
		}
		return masked, &currentFrozenConfigurationRestore{object: restores}, nil
	case []any:
		masked := make([]any, len(value))
		restores := make(map[int]*currentFrozenConfigurationRestore)
		for index := range value {
			child, restore, err := w.maskNestedConfigurations(ctx, value[index], depth+1)
			if err != nil {
				return nil, nil, err
			}
			masked[index] = child
			if restore != nil {
				restores[index] = restore
			}
		}
		if len(restores) == 0 {
			return masked, nil, nil
		}
		return masked, &currentFrozenConfigurationRestore{array: restores}, nil
	case string, json.Number, nil, bool:
		return value, nil, nil
	default:
		return nil, nil, errInvalidCurrentFrozenConfiguration
	}
}

func (w *currentFrozenConfigurationWalker) visit(ctx context.Context, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > configurationapp.MaxCurrentToolkitSettingsDepth ||
		w.nodes >= configurationapp.MaxCurrentToolkitSettingsNodes {
		return errInvalidCurrentFrozenConfiguration
	}
	w.nodes++
	return nil
}

func (r *currentFrozenConfigurationRestore) apply(value any) (any, error) {
	if r == nil {
		return value, nil
	}
	if r.replace {
		return r.replacement, nil
	}
	if len(r.object) != 0 {
		object, ok := value.(map[string]any)
		if !ok {
			return nil, errInvalidCurrentFrozenConfiguration
		}
		for key, restore := range r.object {
			child, exists := object[key]
			if !exists {
				return nil, errInvalidCurrentFrozenConfiguration
			}
			child, err := restore.apply(child)
			if err != nil {
				return nil, err
			}
			object[key] = child
		}
		return object, nil
	}
	if len(r.array) != 0 {
		array, ok := value.([]any)
		if !ok {
			return nil, errInvalidCurrentFrozenConfiguration
		}
		for index, restore := range r.array {
			if index < 0 || index >= len(array) {
				return nil, errInvalidCurrentFrozenConfiguration
			}
			child, err := restore.apply(array[index])
			if err != nil {
				return nil, err
			}
			array[index] = child
		}
		return array, nil
	}
	return value, nil
}

func currentFrozenConfigurationOwner(object map[string]any) (int32, bool, error) {
	marker, hasMarker := object[configurationapp.CurrentFrozenConfigurationMarker]
	uuid, hasUUID := object["configuration_uuid"]
	projectID, hasProjectID := object["configuration_project_id"]
	configurationType, hasType := object["configuration_type"]
	if !hasMarker {
		if hasProjectID || (hasUUID && hasType) {
			return 0, false, errInvalidCurrentFrozenConfiguration
		}
		return 0, false, nil
	}
	marked, markerOK := marker.(bool)
	if !markerOK || !marked || !hasUUID || !hasProjectID || !hasType {
		return 0, false, errInvalidCurrentFrozenConfiguration
	}
	uuidString, uuidOK := uuid.(string)
	typeString, typeOK := configurationType.(string)
	if !uuidOK || uuidString == "" || len(uuidString) > configurationapp.MaxCurrentToolkitSettingsIdentifier ||
		strings.ContainsAny(uuidString, "\x00\r\n") ||
		!typeOK || typeString == "" || len(typeString) > configurationapp.MaxCurrentToolkitSettingsIdentifier ||
		strings.ContainsAny(typeString, "\x00\r\n") {
		return 0, false, errInvalidCurrentFrozenConfiguration
	}
	parsed, ok := currentFrozenConfigurationProjectID(projectID)
	if !ok {
		return 0, false, errInvalidCurrentFrozenConfiguration
	}
	return parsed, true, nil
}

func currentFrozenConfigurationProjectID(value any) (int32, bool) {
	var parsed int64
	switch value := value.(type) {
	case json.Number:
		result, err := strconv.ParseInt(string(value), 10, 32)
		if err != nil {
			return 0, false
		}
		parsed = result
	case int32:
		parsed = int64(value)
	case int64:
		parsed = value
	case int:
		parsed = int64(value)
	default:
		return 0, false
	}
	return int32(parsed), parsed > 0 && parsed <= math.MaxInt32
}

func (m *CurrentConfigurationsMaterializer) materializeProjectObject(
	ctx context.Context,
	projectID int32,
	source []byte,
	maxBytes int64,
) ([]byte, error) {
	object, err := decodeCurrentMaterializationObject(source)
	if err != nil {
		return nil, ErrContentRejected
	}
	materialized, err := m.unsecreter.Unsecret(ctx, projectID, object)
	if err != nil {
		return nil, currentMaterializationError(ctx, err)
	}
	return encodeCurrentMaterializationObject(materialized, maxBytes)
}

func decodeCurrentMaterializationObject(source []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, ErrContentRejected
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, ErrContentRejected
	}
	return object, nil
}

func decodeCurrentMaterializationArray(source []byte) ([]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	var array []any
	if err := decoder.Decode(&array); err != nil || array == nil {
		return nil, ErrContentRejected
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, ErrContentRejected
	}
	return array, nil
}

func encodeCurrentMaterializationObject(object map[string]any, maxBytes int64) ([]byte, error) {
	encoded, err := json.Marshal(object)
	if err != nil || len(encoded) == 0 || int64(len(encoded)) > maxBytes {
		clearContentBytes(encoded)
		return nil, ErrContentRejected
	}
	return encoded, nil
}

func encodeCurrentMaterializationArray(array []any, maxBytes int64) ([]byte, error) {
	encoded, err := json.Marshal(array)
	if err != nil || len(encoded) == 0 || int64(len(encoded)) > maxBytes {
		clearContentBytes(encoded)
		return nil, ErrContentRejected
	}
	return encoded, nil
}

func validCurrentMaterializationString(source []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(source))
	var value string
	if err := decoder.Decode(&value); err != nil || value == "" {
		return false
	}
	var trailing any
	return errors.Is(decoder.Decode(&trailing), io.EOF)
}

func positiveCurrentMaterializationID(value string) (int32, bool) {
	parsed, err := strconv.ParseInt(value, 10, 32)
	return int32(parsed), err == nil && parsed > 0 && parsed <= math.MaxInt32 && strconv.FormatInt(parsed, 10) == value
}

func currentMaterializationError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, configurationapp.ErrInvalidCurrentExpansion) ||
		errors.Is(err, configurationapp.ErrCurrentExpansionNotFound) ||
		errors.Is(err, configurationapp.ErrCurrentExpansionRecursion) ||
		errors.Is(err, configurationapp.ErrCurrentExpansionForbidden) ||
		errors.Is(err, configurationapp.ErrInvalidCurrentToolkitSettings) ||
		errors.Is(err, configurationapp.ErrCurrentToolkitSettingsValidation) ||
		errors.Is(err, ErrCurrentUnsecretRejected) ||
		errors.Is(err, errInvalidCurrentFrozenConfiguration) {
		return ErrContentRejected
	}
	return ErrContentUnavailable
}

var _ ContentMaterializer = (*CurrentConfigurationsMaterializer)(nil)
