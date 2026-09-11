package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
)

const (
	currentAgentTraceMaxStringRunes = 2048
	currentAgentTraceMaxIconBytes   = 8 * 1024
)

var currentAgentHierarchyKeys = [...]string{
	"parent_agent_name",
	"parent_agent_call_id",
	"parent_agent_path",
	"sibling_ordinal",
	"child_thread_id",
	"thread_id",
}

var currentAgentToolMetadataKeys = [...]string{
	"parent_agent_name",
	"parent_agent_call_id",
	"parent_agent_path",
	"sibling_ordinal",
	"child_thread_id",
	"thread_id",
	"agent_type",
	"checkpoint_ns",
	"display_name",
	"hitl_deferred",
	"langgraph_node",
	"original_name",
	"toolkit_name",
	"toolkit_type",
}

var currentAgentTransientInputKeys = map[string]struct{}{
	"hitl_decisions":    {},
	"state_types":       {},
	"parallel_tasks":    {},
	"messages":          {},
	"chat_history":      {},
	"_pipeline_blocked": {},
	"hitl_resume":       {},
	"hitl_action":       {},
	"hitl_value":        {},
}

type currentAgentTraceProjector interface {
	projectAgentTraceDelta(context.Context, sqlExecutor, int64, outputapp.NodeEventFrame) error
}

type noopCurrentAgentTraceProjector struct{}

func (noopCurrentAgentTraceProjector) projectAgentTraceDelta(
	context.Context,
	sqlExecutor,
	int64,
	outputapp.NodeEventFrame,
) error {
	return nil
}

// postgresCurrentAgentTraceProjector writes current partial_message deltas to
// the existing tenant chat tables. elitea_runtime owns only execution fencing
// and immutable browser correlation; it deliberately does not duplicate chat
// history, tool calls, thinking steps or checkpoint state.
type postgresCurrentAgentTraceProjector struct {
	readySchemas sync.Map
	// toolRecordsReady caches the presence of
	// elitea_runtime.tool_call_records, and only the POSITIVE answer. A
	// database that has not run shared migration 0119 must keep projecting
	// chat turns exactly as before — a missing analytics table cannot be
	// allowed to fail the transaction that writes the transcript — and a
	// negative answer is not cached, so the first turn after the migration
	// starts recording.
	toolRecordsReady atomic.Bool
}

type currentAgentTraceDelta struct {
	streamID            string
	messageID           string
	executionGeneration string
	sioEvent            string
	toolCalls           []currentAgentToolCall
	thinkingSteps       []map[string]any
	invokedSkills       json.RawMessage
}

type currentAgentToolCall struct {
	key   string
	entry map[string]any
}

type currentAgentTraceRow struct {
	id                int64
	messageGroupID    int64
	kind              string
	runID             string
	parentAgentName   string
	parentAgentCallID string
	startedAt         *time.Time
	finishedAt        *time.Time
	isError           bool
	hasVisibleContent bool
	toolName          string
	toolInputs        any
	toolOutput        *string
	finishReason      string
	stepType          string
	text              *string
	thinking          *string
	modelName         string
	attrs             map[string]any
}

type currentAgentTraceBinding struct {
	streamID            string
	messageID           string
	executionGeneration string
	sioEvent            string
}

type currentAgentTraceBindingQuerier interface {
	GetCurrentAgentTraceBinding(
		context.Context,
		sqlcgen.GetCurrentAgentTraceBindingParams,
	) (sqlcgen.GetCurrentAgentTraceBindingRow, error)
}

func (p *postgresCurrentAgentTraceProjector) projectAgentTraceDelta(
	ctx context.Context,
	tx sqlExecutor,
	projectID int64,
	frame outputapp.NodeEventFrame,
) error {
	delta, recognized, err := decodeCurrentAgentTraceDelta(frame.BrowserData)
	if err != nil || !recognized {
		return err
	}
	projectDatabaseID, ok := currentAgentDatabaseID(projectID)
	if !ok {
		return outputapp.ErrInvalidNodeEventOutput
	}
	binding, bound, err := loadCurrentAgentTraceBinding(
		ctx,
		tx,
		frame.Fence.ExecutionID,
		frame.Fence.Generation,
		projectDatabaseID,
	)
	if err != nil {
		return err
	}
	if !bound {
		return errors.New("current agent trace binding is unavailable")
	}
	if delta.streamID != binding.streamID || delta.messageID != binding.messageID ||
		delta.executionGeneration != binding.executionGeneration ||
		delta.sioEvent != binding.sioEvent {
		return errors.New("current agent trace delta conflicts with immutable admission")
	}

	schema, ready, err := p.currentSchemaReady(ctx, tx, projectID)
	if err != nil {
		return err
	}
	if !ready {
		return errors.New("current agent trace schema is unavailable")
	}
	messageGroupID, err := lockCurrentAgentMessageGroup(
		ctx,
		tx,
		schema,
		frame.Fence.ExecutionID,
		binding,
	)
	if err != nil {
		return err
	}
	if len(delta.invokedSkills) > 0 {
		if err := persistCurrentAgentTraceInvokedSkills(
			ctx,
			tx,
			schema,
			messageGroupID,
			delta.invokedSkills,
		); err != nil {
			return err
		}
	}
	existing, err := loadCurrentAgentTraceRows(ctx, tx, schema, messageGroupID)
	if err != nil {
		return err
	}
	desired, err := mergeCurrentAgentTraceRows(messageGroupID, existing, delta)
	if err != nil {
		return err
	}
	if err := reconcileCurrentAgentTraceRows(
		ctx,
		tx,
		schema,
		messageGroupID,
		existing,
		desired,
	); err != nil {
		return err
	}
	if err := p.recordAgentToolCalls(
		ctx,
		tx,
		projectID,
		messageGroupID,
		frame.Fence.ExecutionID,
		desired,
		frame.OccurredAt,
	); err != nil {
		return err
	}
	return nil
}

func (p *postgresCurrentAgentTraceProjector) currentSchemaReady(
	ctx context.Context,
	tx sqlExecutor,
	projectID int64,
) (string, bool, error) {
	if tx == nil || projectID <= 0 {
		return "", false, errors.New("invalid current agent trace schema binding")
	}
	schema, err := currentProjectSchema(projectID)
	if err != nil {
		return "", false, err
	}
	if _, ok := p.readySchemas.Load(projectID); ok {
		return schema, true, nil
	}
	var ready bool
	err = tx.QueryRow(ctx, `
SELECT to_regclass($1) IS NOT NULL
   AND to_regclass($2) IS NOT NULL
   AND to_regclass($3) IS NOT NULL`,
		schema+".chat_conversations",
		schema+".chat_message_group",
		schema+".chat_message_trace_step",
	).Scan(&ready)
	if err != nil {
		return "", false, fmt.Errorf("inspect current agent trace schema: %w", err)
	}
	if ready {
		p.readySchemas.Store(projectID, struct{}{})
	}
	return schema, ready, nil
}

func decodeCurrentAgentTraceDelta(
	raw json.RawMessage,
) (currentAgentTraceDelta, bool, error) {
	var event struct {
		Type                string          `json:"type"`
		StreamID            string          `json:"stream_id"`
		MessageID           string          `json:"message_id"`
		ExecutionGeneration string          `json:"execution_generation"`
		SIOEvent            string          `json:"sio_event"`
		ResponseMetadata    json.RawMessage `json:"response_metadata"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return currentAgentTraceDelta{}, false, errors.New("decode current agent trace event")
	}
	if event.Type != "partial_message" {
		return currentAgentTraceDelta{}, false, nil
	}
	if !validCurrentAgentCorrelation(event.StreamID) ||
		!validCurrentAgentCorrelation(event.MessageID) ||
		!validCurrentAgentCorrelation(event.ExecutionGeneration) ||
		(event.SIOEvent != "chat_predict" && event.SIOEvent != "chat_continue_predict") {
		return currentAgentTraceDelta{}, false, errors.New("current agent trace correlation is invalid")
	}
	var metadata struct {
		ToolCalls     json.RawMessage   `json:"tool_calls"`
		ThinkingSteps []json.RawMessage `json:"thinking_steps"`
		InvokedSkills json.RawMessage   `json:"invoked_skills"`
	}
	if err := json.Unmarshal(event.ResponseMetadata, &metadata); err != nil {
		return currentAgentTraceDelta{}, false, errors.New("decode current agent trace metadata")
	}
	toolCalls, err := decodeOrderedCurrentAgentToolCalls(metadata.ToolCalls)
	if err != nil {
		return currentAgentTraceDelta{}, false, err
	}
	thinkingSteps := make([]map[string]any, 0, len(metadata.ThinkingSteps))
	for _, rawStep := range metadata.ThinkingSteps {
		step, err := decodeCurrentAgentJSONObject(rawStep)
		if err != nil {
			return currentAgentTraceDelta{}, false, errors.New("decode current agent thinking delta")
		}
		thinkingSteps = append(thinkingSteps, step)
	}
	var invokedSkills json.RawMessage
	if len(bytes.TrimSpace(metadata.InvokedSkills)) > 0 {
		invokedSkills, err = mergeCurrentAgentInvokedSkills(nil, metadata.InvokedSkills)
		if err != nil {
			return currentAgentTraceDelta{}, false, err
		}
	}
	return currentAgentTraceDelta{
		streamID:            event.StreamID,
		messageID:           event.MessageID,
		executionGeneration: event.ExecutionGeneration,
		sioEvent:            event.SIOEvent,
		toolCalls:           toolCalls,
		thinkingSteps:       thinkingSteps,
		invokedSkills:       invokedSkills,
	}, true, nil
}

func decodeOrderedCurrentAgentToolCalls(raw json.RawMessage) ([]currentAgentToolCall, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("current agent tool-call delta must be an object")
	}
	result := make([]currentAgentToolCall, 0, 1)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, errors.New("decode current agent tool-call key")
		}
		key, ok := keyToken.(string)
		if !ok || !validCurrentAgentCorrelation(key) {
			return nil, errors.New("current agent tool-call key is invalid")
		}
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, errors.New("decode current agent tool-call delta")
		}
		entry, ok := value.(map[string]any)
		if !ok {
			return nil, errors.New("current agent tool-call delta must contain objects")
		}
		result = append(result, currentAgentToolCall{key: key, entry: entry})
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, errors.New("decode current agent tool-call object")
	}
	if token, err = decoder.Token(); !errors.Is(err, io.EOF) || token != nil {
		return nil, errors.New("current agent tool-call delta has trailing data")
	}
	return result, nil
}

func decodeCurrentAgentJSONObject(raw json.RawMessage) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) || token != nil {
		return nil, errors.New("JSON value has trailing data")
	}
	result, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("JSON value must be an object")
	}
	return result, nil
}

func validCurrentAgentCorrelation(value string) bool {
	return value != "" && len(value) <= 512 && utf8.ValidString(value) &&
		!strings.ContainsAny(value, "\x00\r\n")
}

func loadCurrentAgentTraceBinding(
	ctx context.Context,
	tx sqlExecutor,
	executionID string,
	generation uint64,
	projectID int32,
) (currentAgentTraceBinding, bool, error) {
	querier, ok := tx.(currentAgentTraceBindingQuerier)
	if !ok {
		return currentAgentTraceBinding{}, false, errors.New("current agent trace binding query is unavailable")
	}
	row, err := querier.GetCurrentAgentTraceBinding(ctx, sqlcgen.GetCurrentAgentTraceBindingParams{
		ExecutionID: executionID,
		Generation:  int64(generation),
		ProjectID:   projectID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return currentAgentTraceBinding{}, false, nil
	}
	if err != nil {
		return currentAgentTraceBinding{}, false, fmt.Errorf("load current agent trace binding: %w", err)
	}
	return currentAgentTraceBinding{
		streamID:            row.ClientStreamID,
		messageID:           row.ClientMessageID,
		executionGeneration: row.ClientExecutionGeneration,
		sioEvent:            row.SioEvent,
	}, true, nil
}

func lockCurrentAgentMessageGroup(
	ctx context.Context,
	tx sqlExecutor,
	schema string,
	executionID string,
	binding currentAgentTraceBinding,
) (int64, error) {
	var messageGroupID int64
	err := tx.QueryRow(ctx, fmt.Sprintf(`
SELECT message_group.id
FROM %s AS message_group
JOIN %s AS conversation
  ON conversation.id = message_group.conversation_id
WHERE message_group.uuid::text = $1
  AND conversation.uuid::text = $2
  AND message_group.task_id = $3
  AND message_group.meta->>'execution_generation' = $4
FOR UPDATE OF message_group`,
		schema+".chat_message_group",
		schema+".chat_conversations",
	),
		binding.messageID,
		binding.streamID,
		executionID,
		binding.executionGeneration,
	).Scan(&messageGroupID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errors.New("current agent response message group is unavailable")
	}
	if err != nil {
		return 0, fmt.Errorf("lock current agent response message group: %w", err)
	}
	return messageGroupID, nil
}

func loadCurrentAgentTraceRows(
	ctx context.Context,
	tx sqlExecutor,
	schema string,
	messageGroupID int64,
) ([]currentAgentTraceRow, error) {
	rows, err := tx.Query(ctx, fmt.Sprintf(`
SELECT id, message_group_id, kind, COALESCE(run_id, ''),
       COALESCE(parent_agent_name, ''), COALESCE(parent_agent_call_id, ''),
       started_at, finished_at, is_error, has_visible_content,
       COALESCE(tool_name, ''), tool_inputs, tool_output,
       COALESCE(finish_reason, ''), COALESCE(step_type, ''),
       text, thinking, COALESCE(model_name, ''), COALESCE(attrs, '{}'::jsonb)
FROM %s
WHERE message_group_id = $1
ORDER BY id
FOR UPDATE`, schema+".chat_message_trace_step"), messageGroupID)
	if err != nil {
		return nil, fmt.Errorf("load current agent trace rows: %w", err)
	}
	defer rows.Close()
	result := make([]currentAgentTraceRow, 0, 8)
	for rows.Next() {
		var row currentAgentTraceRow
		var toolInputs, attrs []byte
		if err := rows.Scan(
			&row.id,
			&row.messageGroupID,
			&row.kind,
			&row.runID,
			&row.parentAgentName,
			&row.parentAgentCallID,
			&row.startedAt,
			&row.finishedAt,
			&row.isError,
			&row.hasVisibleContent,
			&row.toolName,
			&toolInputs,
			&row.toolOutput,
			&row.finishReason,
			&row.stepType,
			&row.text,
			&row.thinking,
			&row.modelName,
			&attrs,
		); err != nil {
			return nil, fmt.Errorf("scan current agent trace row: %w", err)
		}
		if len(toolInputs) != 0 {
			row.toolInputs, err = decodeCurrentAgentJSONValue(toolInputs)
			if err != nil {
				return nil, errors.New("decode stored current agent tool inputs")
			}
		}
		if len(attrs) != 0 {
			value, decodeErr := decodeCurrentAgentJSONValue(attrs)
			if decodeErr != nil {
				return nil, errors.New("decode stored current agent trace attrs")
			}
			row.attrs, _ = value.(map[string]any)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate current agent trace rows: %w", err)
	}
	return result, nil
}

func decodeCurrentAgentJSONValue(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func mergeCurrentAgentTraceRows(
	messageGroupID int64,
	existing []currentAgentTraceRow,
	delta currentAgentTraceDelta,
) ([]currentAgentTraceRow, error) {
	toolCalls, thinkingSteps := reconstructCurrentAgentTrace(existing)
	positions := make(map[string]int, len(toolCalls))
	for index, toolCall := range toolCalls {
		positions[toolCall.key] = index
	}
	for _, incoming := range delta.toolCalls {
		incoming.entry = sanitizeCurrentAgentJSON(incoming.entry).(map[string]any)
		if index, ok := positions[incoming.key]; ok {
			merged, err := mergeAgentToolOutputChunk(toolCalls[index].entry, incoming.entry)
			if err != nil {
				return nil, err
			}
			incoming.entry = merged
			toolCalls[index] = incoming
		} else {
			merged, err := mergeAgentToolOutputChunk(nil, incoming.entry)
			if err != nil {
				return nil, err
			}
			incoming.entry = merged
			positions[incoming.key] = len(toolCalls)
			toolCalls = append(toolCalls, incoming)
		}
	}
	toolCalls = dedupeCurrentAgentToolCalls(toolCalls)
	thinkingSteps = mergeCurrentAgentThinkingSteps(thinkingSteps, delta.thinkingSteps)

	desired := make([]currentAgentTraceRow, 0, len(toolCalls)+len(thinkingSteps))
	for _, toolCall := range toolCalls {
		row, err := currentAgentToolCallToRow(messageGroupID, toolCall)
		if err != nil {
			return nil, err
		}
		desired = append(desired, row)
	}
	for _, step := range thinkingSteps {
		row, err := currentAgentThinkingStepToRow(messageGroupID, step)
		if err != nil {
			return nil, err
		}
		desired = append(desired, row)
	}
	return desired, nil
}

func reconstructCurrentAgentTrace(
	rows []currentAgentTraceRow,
) ([]currentAgentToolCall, []map[string]any) {
	toolCalls := make([]currentAgentToolCall, 0, len(rows))
	thinkingSteps := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		switch row.kind {
		case "tool_call":
			metadata := cloneCurrentAgentMap(currentAgentMap(row.attrs, "metadata"))
			metadata["parent_agent_name"] = currentAgentNilIfEmpty(row.parentAgentName)
			metadata["parent_agent_call_id"] = currentAgentNilIfEmpty(row.parentAgentCallID)
			entry := map[string]any{
				"run_id":           currentAgentNilIfEmpty(row.runID),
				"tool_run_id":      currentAgentNilIfEmpty(row.runID),
				"tool_name":        currentAgentNilIfEmpty(row.toolName),
				"metadata":         metadata,
				"tool_inputs":      row.toolInputs,
				"tool_output":      stringPointerValue(row.toolOutput),
				"finish_reason":    currentAgentNilIfEmpty(row.finishReason),
				"error":            nil,
				"timestamp_start":  currentAgentTimeString(row.startedAt),
				"timestamp_finish": currentAgentTimeString(row.finishedAt),
			}
			if chunk, ok := row.attrs["tool_output_chunk_v1"]; ok {
				entry["tool_output_chunk_v1"] = chunk
			}
			if row.isError {
				if value := stringPointerValue(row.toolOutput); value != nil && value != "" {
					entry["error"] = value
				} else {
					entry["error"] = "error"
				}
			}
			if toolMeta := currentAgentMap(row.attrs, "tool_meta"); toolMeta != nil {
				entry["tool_meta"] = cloneCurrentAgentMap(toolMeta)
			}
			toolCalls = append(toolCalls, currentAgentToolCall{key: row.runID, entry: entry})
		case "thinking_step":
			responseMetadata := cloneCurrentAgentMap(currentAgentMap(row.attrs, "response_metadata"))
			responseMetadata["model_name"] = currentAgentNilIfEmpty(row.modelName)
			entry := map[string]any{
				"tool_run_id":          currentAgentNilIfEmpty(row.runID),
				"parent_agent_name":    currentAgentNilIfEmpty(row.parentAgentName),
				"parent_agent_call_id": currentAgentNilIfEmpty(row.parentAgentCallID),
				"type":                 currentAgentNilIfEmpty(row.stepType),
				"text":                 stringPointerValue(row.text),
				"thinking":             stringPointerValue(row.thinking),
				"timestamp_start":      currentAgentTimeString(row.startedAt),
				"timestamp_finish":     currentAgentTimeString(row.finishedAt),
				"message": map[string]any{
					"response_metadata": responseMetadata,
				},
			}
			for _, key := range currentAgentHierarchyKeys {
				if value, ok := row.attrs[key]; ok && value != nil {
					entry[key] = value
				}
			}
			thinkingSteps = append(thinkingSteps, entry)
		}
	}
	return toolCalls, thinkingSteps
}

func currentAgentToolCallToRow(
	messageGroupID int64,
	toolCall currentAgentToolCall,
) (currentAgentTraceRow, error) {
	entry := toolCall.entry
	hierarchy := currentAgentHierarchyMetadata(entry)
	runID := currentAgentString(entry["run_id"])
	if runID == "" {
		runID = currentAgentString(entry["tool_run_id"])
	}
	if runID == "" {
		runID = toolCall.key
	}
	toolName := currentAgentString(entry["tool_name"])
	toolMeta := currentAgentMap(entry, "tool_meta")
	if toolName == "" {
		toolName = currentAgentString(toolMeta["name"])
	}
	toolInputs := entry["tool_inputs"]
	if _, object := toolInputs.(map[string]any); !object {
		if _, array := toolInputs.([]any); !array {
			toolInputs = nil
		}
	}
	attrs := currentAgentToolCallAttrs(entry)
	if attrs == nil {
		attrs = map[string]any{}
	}
	if err := validateCurrentAgentAttrs(attrs); err != nil {
		return currentAgentTraceRow{}, err
	}
	return currentAgentTraceRow{
		messageGroupID:    messageGroupID,
		kind:              "tool_call",
		runID:             boundedCurrentAgentString(runID),
		parentAgentName:   boundedCurrentAgentString(currentAgentString(hierarchy["parent_agent_name"])),
		parentAgentCallID: boundedCurrentAgentString(currentAgentString(hierarchy["parent_agent_call_id"])),
		startedAt:         parseCurrentAgentTime(entry["timestamp_start"]),
		finishedAt:        parseCurrentAgentTime(entry["timestamp_finish"]),
		isError:           currentAgentTruthy(entry["error"]),
		hasVisibleContent: true,
		toolName:          boundedCurrentAgentString(toolName),
		toolInputs:        toolInputs,
		toolOutput:        currentAgentStringPointer(entry["tool_output"]),
		finishReason:      boundedCurrentAgentString(currentAgentString(entry["finish_reason"])),
		attrs:             attrs,
	}, nil
}

func currentAgentThinkingStepToRow(
	messageGroupID int64,
	entry map[string]any,
) (currentAgentTraceRow, error) {
	entry = sanitizeCurrentAgentJSON(entry).(map[string]any)
	hierarchy := currentAgentHierarchyMetadata(entry)
	message := currentAgentMap(entry, "message")
	responseMetadata := currentAgentMap(message, "response_metadata")
	displayMetadata := map[string]any{}
	if toolName := boundedCurrentAgentString(currentAgentString(responseMetadata["tool_name"])); toolName != "" {
		displayMetadata["tool_name"] = toolName
	}
	if len(hierarchy) != 0 {
		displayMetadata["metadata"] = hierarchy
	}
	attrs := cloneCurrentAgentMap(hierarchy)
	if len(displayMetadata) != 0 {
		attrs["response_metadata"] = displayMetadata
	}
	if err := validateCurrentAgentAttrs(attrs); err != nil {
		return currentAgentTraceRow{}, err
	}
	text := currentAgentStringPointer(entry["text"])
	thinking := currentAgentStringPointer(entry["thinking"])
	runID := currentAgentString(entry["tool_run_id"])
	if runID == "" {
		runID = currentAgentString(entry["run_id"])
	}
	parentName := currentAgentString(hierarchy["parent_agent_name"])
	if parentName == "" {
		parentName = currentAgentString(entry["parent_agent_name"])
	}
	parentCallID := currentAgentString(hierarchy["parent_agent_call_id"])
	if parentCallID == "" {
		parentCallID = currentAgentString(entry["parent_agent_call_id"])
	}
	return currentAgentTraceRow{
		messageGroupID:    messageGroupID,
		kind:              "thinking_step",
		runID:             boundedCurrentAgentString(runID),
		parentAgentName:   boundedCurrentAgentString(parentName),
		parentAgentCallID: boundedCurrentAgentString(parentCallID),
		startedAt:         parseCurrentAgentTime(entry["timestamp_start"]),
		finishedAt:        parseCurrentAgentTime(entry["timestamp_finish"]),
		hasVisibleContent: currentAgentVisible(text, thinking, hierarchy),
		stepType:          boundedCurrentAgentString(currentAgentString(entry["type"])),
		text:              text,
		thinking:          thinking,
		modelName:         boundedCurrentAgentString(currentAgentString(responseMetadata["model_name"])),
		attrs:             attrs,
	}, nil
}

func mergeCurrentAgentThinkingSteps(
	oldSteps,
	newSteps []map[string]any,
) []map[string]any {
	merged := append([]map[string]any(nil), oldSteps...)
	positions := make(map[string]int, len(merged))
	for index, step := range merged {
		if identity := currentAgentThinkingIdentity(step); identity != "" {
			positions[identity] = index
		}
	}
	for _, rawStep := range newSteps {
		step := sanitizeCurrentAgentJSON(rawStep).(map[string]any)
		identity := currentAgentThinkingIdentity(step)
		if index, ok := positions[identity]; ok && identity != "" {
			merged[index] = step
			continue
		}
		if identity != "" {
			positions[identity] = len(merged)
		}
		merged = append(merged, step)
	}
	return merged
}

func currentAgentThinkingIdentity(step map[string]any) string {
	runID := currentAgentString(step["tool_run_id"])
	if runID == "" {
		runID = currentAgentString(step["run_id"])
	}
	if runID != "" {
		return "run:" + runID
	}
	if timestamp := currentAgentString(step["timestamp_start"]); timestamp != "" {
		return "timestamp:" + timestamp
	}
	return ""
}

func dedupeCurrentAgentToolCalls(values []currentAgentToolCall) []currentAgentToolCall {
	if len(values) < 2 {
		return values
	}
	type bestEntry struct {
		value currentAgentToolCall
		rank  int
	}
	epochs := make(map[string]int)
	best := make(map[string]bestEntry)
	order := make([]string, 0, len(values))
	for _, value := range values {
		identity := currentAgentToolCallIdentity(value.entry)
		epoch := epochs[identity]
		key := identity + "\x00" + fmt.Sprint(epoch)
		rank := currentAgentToolCompleteness(value.entry)
		if current, ok := best[key]; !ok {
			best[key] = bestEntry{value: value, rank: rank}
			order = append(order, key)
		} else if rank > current.rank {
			best[key] = bestEntry{value: value, rank: rank}
		}
		if currentAgentTruthy(value.entry["tool_output"]) {
			epochs[identity] = epoch + 1
		}
	}
	result := make([]currentAgentToolCall, 0, len(order))
	for _, key := range order {
		result = append(result, best[key].value)
	}
	return result
}

func currentAgentToolCallIdentity(entry map[string]any) string {
	metadata := currentAgentMap(entry, "metadata")
	toolMeta := currentAgentMap(entry, "tool_meta")
	toolMetaMetadata := currentAgentMap(toolMeta, "metadata")
	name := currentAgentString(toolMeta["name"])
	if name == "" {
		name = currentAgentString(entry["tool_name"])
	}
	parent := currentAgentString(metadata["parent_agent_name"])
	if parent == "" {
		parent = currentAgentString(toolMetaMetadata["parent_agent_name"])
	}
	rootInstance := currentAgentString(metadata["child_thread_id"])
	if rootInstance == "" {
		rootInstance = currentAgentString(toolMetaMetadata["child_thread_id"])
	}
	if rootInstance == "" {
		path, _ := metadata["parent_agent_path"].([]any)
		if len(path) == 0 {
			path, _ = toolMetaMetadata["parent_agent_path"].([]any)
		}
		if len(path) != 0 {
			root, _ := path[0].(map[string]any)
			rootInstance = currentAgentString(root["call_id"])
			if rootInstance == "" {
				rootInstance = currentAgentString(root["name"]) + ":" + currentAgentString(root["sibling_ordinal"])
			}
		}
	}
	namespace := currentAgentString(metadata["checkpoint_ns"])
	node := ""
	if namespace != "" {
		node = strings.SplitN(namespace, ":", 2)[0]
	} else {
		node = currentAgentString(metadata["langgraph_node"])
	}
	stableInputs := entry["tool_inputs"]
	if object, ok := stableInputs.(map[string]any); ok {
		copyObject := make(map[string]any, len(object))
		for key, value := range object {
			if _, transient := currentAgentTransientInputKeys[key]; !transient {
				copyObject[key] = value
			}
		}
		stableInputs = copyObject
	}
	encodedInputs, err := json.Marshal(stableInputs)
	if err != nil {
		encodedInputs = []byte(fmt.Sprint(stableInputs))
	}
	components, _ := json.Marshal([]string{name, rootInstance, parent, node, string(encodedInputs)})
	return string(components)
}

func currentAgentToolCompleteness(entry map[string]any) int {
	if currentAgentTruthy(entry["tool_output"]) {
		return 2
	}
	if currentAgentTruthy(entry["timestamp_finish"]) {
		return 1
	}
	return 0
}

func currentAgentHierarchyMetadata(entry map[string]any) map[string]any {
	toolMeta := currentAgentMap(entry, "tool_meta")
	message := currentAgentMap(entry, "message")
	responseMetadata := currentAgentMap(message, "response_metadata")
	sources := []map[string]any{
		entry,
		currentAgentMap(entry, "metadata"),
		currentAgentMap(toolMeta, "metadata"),
		currentAgentMap(responseMetadata, "metadata"),
	}
	result := map[string]any{}
	for _, key := range currentAgentHierarchyKeys {
		for _, source := range sources {
			value, ok := source[key]
			if !ok || value == nil {
				continue
			}
			normalized, ok := normalizeCurrentAgentHierarchyValue(key, value)
			if ok {
				result[key] = normalized
				break
			}
		}
	}
	return result
}

func currentAgentToolCallAttrs(entry map[string]any) map[string]any {
	attrs := map[string]any{}
	if chunk, ok := entry["tool_output_chunk_v1"]; ok {
		attrs["tool_output_chunk_v1"] = chunk
	}
	metadata := allowlistedCurrentAgentMetadata(currentAgentMap(entry, "metadata"))
	for key, value := range currentAgentHierarchyMetadata(entry) {
		if _, exists := metadata[key]; !exists {
			metadata[key] = value
		}
	}
	if len(metadata) != 0 {
		attrs["metadata"] = metadata
	}
	rawToolMeta := currentAgentMap(entry, "tool_meta")
	toolMeta := map[string]any{}
	for _, key := range []string{"name", "display_name", "model_name", "loaded_skill"} {
		if value := boundedCurrentAgentString(currentAgentString(rawToolMeta[key])); value != "" {
			toolMeta[key] = value
		}
	}
	toolMetadata := allowlistedCurrentAgentMetadata(currentAgentMap(rawToolMeta, "metadata"))
	for key, value := range metadata {
		if _, exists := toolMetadata[key]; !exists {
			toolMetadata[key] = value
		}
	}
	if len(toolMetadata) != 0 {
		toolMeta["metadata"] = toolMetadata
	}
	if icon, ok := rawToolMeta["icon_meta"].(map[string]any); ok {
		encoded, err := json.Marshal(icon)
		if err == nil && len(encoded) <= currentAgentTraceMaxIconBytes {
			toolMeta["icon_meta"] = sanitizeCurrentAgentJSON(icon)
		}
	}
	if len(toolMeta) != 0 {
		attrs["tool_meta"] = toolMeta
	}
	if len(attrs) == 0 {
		return nil
	}
	return attrs
}

func allowlistedCurrentAgentMetadata(value map[string]any) map[string]any {
	result := map[string]any{}
	for _, key := range currentAgentToolMetadataKeys {
		raw, ok := value[key]
		if !ok || raw == nil {
			continue
		}
		if normalized, ok := normalizeCurrentAgentMetadataValue(key, raw); ok {
			result[key] = normalized
		}
	}
	return result
}

func normalizeCurrentAgentHierarchyValue(key string, value any) (any, bool) {
	if key == "parent_agent_path" {
		return normalizeCurrentAgentParentPath(value)
	}
	if key == "sibling_ordinal" {
		ordinal, ok := currentAgentPositiveInteger(value)
		return ordinal, ok
	}
	text, ok := value.(string)
	if !ok || text == "" {
		return nil, false
	}
	return boundedCurrentAgentString(text), true
}

func normalizeCurrentAgentMetadataValue(key string, value any) (any, bool) {
	if key == "hitl_deferred" {
		flag, ok := value.(bool)
		return flag, ok
	}
	return normalizeCurrentAgentHierarchyValue(key, value)
}

func normalizeCurrentAgentParentPath(value any) (any, bool) {
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}
	if len(items) > 8 {
		items = items[:8]
	}
	result := make([]any, 0, len(items))
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := boundedCurrentAgentString(currentAgentString(item["name"]))
		callID := boundedCurrentAgentString(currentAgentString(item["call_id"]))
		if name == "" && callID == "" {
			continue
		}
		normalized := map[string]any{"name": name, "call_id": callID}
		if ordinal, ok := currentAgentPositiveInteger(item["sibling_ordinal"]); ok {
			normalized["sibling_ordinal"] = ordinal
		}
		result = append(result, normalized)
	}
	return result, len(result) != 0
}

func currentAgentPositiveInteger(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		integer, err := typed.Int64()
		return integer, err == nil && integer > 0
	case int:
		return int64(typed), typed > 0
	case int64:
		return typed, typed > 0
	default:
		return 0, false
	}
}

func validateCurrentAgentAttrs(attrs map[string]any) error {
	encoded, err := json.Marshal(attrs)
	if err != nil || len(encoded) > currentAgentTraceMaxIconBytes {
		return errors.New("current agent trace attrs exceed their display bound")
	}
	return nil
}

// recordAgentToolCalls writes the durable per-tool-call record for the tools an
// AGENT called inside this turn (issue 618).
//
// This is the half the analytics TOOL dimension was missing and the half that
// matters most: the explicit tool run — the test button and MCP tools/call — is
// the small end of tool usage, and everything an agent reaches for during a
// chat turn was recorded nowhere a project-wide read could reach.
// p_<id>.chat_message_trace_step holds it, but it is per-tenant, carries no
// toolkit id, and covers chat turns only, so a Tools tab built on it would
// silently exclude the other producer.
//
// It writes in the CALLER'S TRANSACTION on purpose. The record describes the
// trace step reconciled immediately above it, and a record committed on another
// connection could outlive a rollback of the turn it claims to describe.
//
// It is keyed by (message group, run id) — the same natural key the reconcile
// above matches rows by — so a streaming turn, which re-projects the same tool
// call on every partial message as its output and finish time arrive, updates
// one record rather than counting one call many times.
//
// executionID is frame.Fence.ExecutionID — the same value
// lockCurrentAgentMessageGroup above already matched against
// message_group.task_id, and the value the gateway writes onto
// gateway.llm_request_logs.execution_id (shared 0100). Stamping it here (#875)
// is what lets a cost read correlate this tool call back to the LLM spend of
// the execution it ran inside, the way GetAgentAnalytics already correlates an
// execution to an agent — see internal/api/v2/analytics/estimate.go. Before
// this it was left NULL for every agent-turn row, so the only tool calls a
// cost read could ever reach were explicit runs (toolkit.call_tool.v1), which
// make no LLM call at all.
func (p *postgresCurrentAgentTraceProjector) recordAgentToolCalls(
	ctx context.Context,
	tx sqlExecutor,
	projectID int64,
	messageGroupID int64,
	executionID string,
	desired []currentAgentTraceRow,
	occurredAt time.Time,
) error {
	calls := make([]currentAgentTraceRow, 0, len(desired))
	for _, row := range desired {
		if row.kind == "tool_call" && row.toolName != "" && row.runID != "" {
			calls = append(calls, row)
		}
	}
	if len(calls) == 0 {
		return nil
	}
	ready, err := p.toolRecordsAvailable(ctx, tx)
	if err != nil || !ready {
		return err
	}

	for _, row := range calls {
		started := occurredAt
		if row.startedAt != nil {
			started = *row.startedAt
		}
		if started.IsZero() {
			// A call the producer cannot time is recorded at the moment its
			// frame arrived rather than dropped: a missing timestamp must not
			// silently shrink a count.
			started = time.Now().UTC()
		}
		record := ToolCallRecord{
			ProjectID: projectID,
			Source:    ToolCallSourceAgentTurn,
			SourceRef: strconv.FormatInt(messageGroupID, 10) + ":" + row.runID,
			// No toolkit id. The worker's tool metadata carries a toolkit NAME
			// and TYPE and no id (currentAgentToolMetadataKeys), and resolving
			// the name back to a row here would store a guess as a fact.
			ToolkitName: currentAgentToolkitAttr(row.attrs, "toolkit_name"),
			ToolkitType: currentAgentToolkitAttr(row.attrs, "toolkit_type"),
			ToolName:    row.toolName,
			StartedAt:   started,
			IsError:     row.isError,
			ExecutionID: executionID,
		}
		if row.finishedAt != nil && !row.finishedAt.Before(started) {
			record.FinishedAt = *row.finishedAt
		}
		if err := RecordToolCall(ctx, tx, record); err != nil {
			return err
		}
	}
	return nil
}

// currentAgentToolkitAttr reads one allowlisted toolkit attribute out of a
// projected tool-call row.
//
// The attributes are NESTED, in two places, because currentAgentToolCallAttrs
// folds the worker's `metadata` and its `tool_meta.metadata` separately and the
// worker fills whichever one its emitting frame carried. Reading the top level
// finds neither, which is the shape of bug that would leave every recorded
// toolkit name empty while every test on the trace row still passed.
func currentAgentToolkitAttr(attrs map[string]any, key string) string {
	if value := currentAgentString(currentAgentMap(attrs, "metadata")[key]); value != "" {
		return value
	}
	toolMeta := currentAgentMap(attrs, "tool_meta")
	return currentAgentString(currentAgentMap(toolMeta, "metadata")[key])
}

// toolRecordsAvailable probes the analytics record table, caching only the
// positive answer. See the field comment on toolRecordsReady.
func (p *postgresCurrentAgentTraceProjector) toolRecordsAvailable(
	ctx context.Context,
	tx sqlExecutor,
) (bool, error) {
	if p.toolRecordsReady.Load() {
		return true, nil
	}
	var ready bool
	if err := tx.QueryRow(ctx,
		`SELECT to_regclass('elitea_runtime.tool_call_records') IS NOT NULL`,
	).Scan(&ready); err != nil {
		return false, fmt.Errorf("inspect tool call record table: %w", err)
	}
	if ready {
		p.toolRecordsReady.Store(true)
	}
	return ready, nil
}

func reconcileCurrentAgentTraceRows(
	ctx context.Context,
	tx sqlExecutor,
	schema string,
	messageGroupID int64,
	existing,
	desired []currentAgentTraceRow,
) error {
	existingByKey := make(map[string]currentAgentTraceRow, len(existing))
	for _, row := range existing {
		existingByKey[currentAgentTraceNaturalKey(row, fmt.Sprintf("existing:%d", row.id))] = row
	}
	seen := make(map[string]struct{}, len(desired))
	for index, row := range desired {
		key := currentAgentTraceNaturalKey(row, fmt.Sprintf("new:%d", index))
		if current, ok := existingByKey[key]; ok {
			row.id = current.id
			if err := updateCurrentAgentTraceRow(ctx, tx, schema, row); err != nil {
				return err
			}
		} else if err := insertCurrentAgentTraceRow(ctx, tx, schema, row); err != nil {
			return err
		}
		seen[key] = struct{}{}
	}
	for key, row := range existingByKey {
		if _, ok := seen[key]; ok {
			continue
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			"DELETE FROM %s WHERE id = $1 AND message_group_id = $2",
			schema+".chat_message_trace_step",
		), row.id, messageGroupID); err != nil {
			return fmt.Errorf("delete collapsed current agent trace row: %w", err)
		}
	}
	return nil
}

func currentAgentTraceNaturalKey(row currentAgentTraceRow, fallback string) string {
	if row.runID != "" {
		return row.kind + ":run:" + row.runID
	}
	if row.kind == "thinking_step" && row.startedAt != nil {
		return row.kind + ":timestamp:" + row.startedAt.UTC().Format(time.RFC3339Nano)
	}
	return row.kind + ":object:" + fallback
}

func updateCurrentAgentTraceRow(
	ctx context.Context,
	tx sqlExecutor,
	schema string,
	row currentAgentTraceRow,
) error {
	toolInputs, attrs, err := encodeCurrentAgentTraceJSON(row)
	if err != nil {
		return err
	}
	command, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s
SET kind = $2, run_id = NULLIF($3, ''), parent_agent_name = NULLIF($4, ''),
    parent_agent_call_id = NULLIF($5, ''), started_at = $6, finished_at = $7,
    is_error = $8, has_visible_content = $9, tool_name = NULLIF($10, ''),
    tool_inputs = $11::jsonb, tool_output = $12,
    finish_reason = NULLIF($13, ''), step_type = NULLIF($14, ''),
    text = $15, thinking = $16, model_name = NULLIF($17, ''), attrs = $18::jsonb
WHERE id = $1 AND message_group_id = $19`, schema+".chat_message_trace_step"),
		row.id, row.kind, row.runID, row.parentAgentName, row.parentAgentCallID,
		row.startedAt, row.finishedAt, row.isError, row.hasVisibleContent,
		row.toolName, toolInputs, row.toolOutput, row.finishReason, row.stepType,
		row.text, row.thinking, row.modelName, attrs, row.messageGroupID,
	)
	if err != nil {
		return fmt.Errorf("update current agent trace row: %w", err)
	}
	if command.RowsAffected() != 1 {
		return errors.New("current agent trace row update lost its target")
	}
	return nil
}

func insertCurrentAgentTraceRow(
	ctx context.Context,
	tx sqlExecutor,
	schema string,
	row currentAgentTraceRow,
) error {
	toolInputs, attrs, err := encodeCurrentAgentTraceJSON(row)
	if err != nil {
		return err
	}
	command, err := tx.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s (
    message_group_id, kind, run_id, parent_agent_name, parent_agent_call_id,
    started_at, finished_at, is_error, has_visible_content, tool_name,
    tool_inputs, tool_output, finish_reason, step_type, text, thinking,
    model_name, attrs
) VALUES (
    $1, $2, NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''),
    $6, $7, $8, $9, NULLIF($10, ''), $11::jsonb, $12,
    NULLIF($13, ''), NULLIF($14, ''), $15, $16, NULLIF($17, ''), $18::jsonb
)`, schema+".chat_message_trace_step"),
		row.messageGroupID, row.kind, row.runID, row.parentAgentName,
		row.parentAgentCallID, row.startedAt, row.finishedAt, row.isError,
		row.hasVisibleContent, row.toolName, toolInputs, row.toolOutput,
		row.finishReason, row.stepType, row.text, row.thinking, row.modelName, attrs,
	)
	if err != nil {
		return fmt.Errorf("insert current agent trace row: %w", err)
	}
	if command.RowsAffected() != 1 {
		return errors.New("current agent trace row was not inserted")
	}
	return nil
}

func encodeCurrentAgentTraceJSON(row currentAgentTraceRow) ([]byte, []byte, error) {
	var toolInputs []byte
	var err error
	if row.toolInputs != nil {
		toolInputs, err = json.Marshal(row.toolInputs)
		if err != nil {
			return nil, nil, errors.New("encode current agent tool inputs")
		}
	}
	attrs := row.attrs
	if attrs == nil {
		attrs = map[string]any{}
	}
	encodedAttrs, err := json.Marshal(attrs)
	if err != nil {
		return nil, nil, errors.New("encode current agent trace attrs")
	}
	return toolInputs, encodedAttrs, nil
}

func currentAgentMap(value map[string]any, key string) map[string]any {
	if value == nil {
		return nil
	}
	result, _ := value[key].(map[string]any)
	return result
}

func cloneCurrentAgentMap(value map[string]any) map[string]any {
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func currentAgentString(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.ReplaceAll(typed, "\x00", "")
	case json.Number:
		return typed.String()
	case int:
		return fmt.Sprint(typed)
	case int64:
		return fmt.Sprint(typed)
	default:
		return ""
	}
}

func boundedCurrentAgentString(value string) string {
	value = strings.ReplaceAll(value, "\x00", "")
	if utf8.RuneCountInString(value) <= currentAgentTraceMaxStringRunes {
		return value
	}
	runes := []rune(value)
	return string(runes[:currentAgentTraceMaxStringRunes])
}

func currentAgentStringPointer(value any) *string {
	if value == nil {
		return nil
	}
	text, ok := value.(string)
	if !ok {
		return nil
	}
	text = strings.ReplaceAll(text, "\x00", "")
	return &text
}

func stringPointerValue(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func currentAgentTimeString(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func parseCurrentAgentTime(value any) *time.Time {
	text := currentAgentString(value)
	if text == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil || parsed.IsZero() {
		return nil
	}
	parsed = parsed.UTC()
	return &parsed
}

func currentAgentTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case json.Number:
		return typed.String() != "0" && typed.String() != "0.0"
	case []any:
		return len(typed) != 0
	case map[string]any:
		return len(typed) != 0
	default:
		return true
	}
}

func currentAgentVisible(text, thinking *string, hierarchy map[string]any) bool {
	return (text != nil && strings.TrimSpace(*text) != "") ||
		(thinking != nil && strings.TrimSpace(*thinking) != "") ||
		currentAgentString(hierarchy["parent_agent_name"]) != "" ||
		hierarchy["parent_agent_path"] != nil
}

func sanitizeCurrentAgentJSON(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[strings.ReplaceAll(key, "\x00", "")] = sanitizeCurrentAgentJSON(item)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			result[index] = sanitizeCurrentAgentJSON(item)
		}
		return result
	case string:
		return strings.ReplaceAll(typed, "\x00", "")
	default:
		return value
	}
}

func currentAgentNilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
