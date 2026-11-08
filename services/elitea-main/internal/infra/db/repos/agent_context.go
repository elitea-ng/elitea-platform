package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/contextsettings"
)

type currentAgentContextProjector interface {
	projectAgentContext(context.Context, sqlExecutor, int64, outputapp.NodeEventFrame) error
}

type noopCurrentAgentContextProjector struct{}

func (noopCurrentAgentContextProjector) projectAgentContext(context.Context, sqlExecutor, int64, outputapp.NodeEventFrame) error {
	return nil
}

type postgresCurrentAgentContextProjector struct{}

// Called only inside the accepted-event transaction, after ownership and
// sequence fencing. Replay duplicates never reapply this projection.
func (postgresCurrentAgentContextProjector) projectAgentContext(ctx context.Context, tx sqlExecutor, projectID int64, frame outputapp.NodeEventFrame) error {
	event, measurement, ok, err := decodeRootAgentContextFrame(frame)
	if err != nil || !ok {
		return err
	}
	projectDatabaseID, ok := currentAgentDatabaseID(projectID)
	if !ok {
		return outputapp.ErrInvalidNodeEventOutput
	}
	binding, bound, err := loadCurrentAgentTraceBinding(ctx, tx, frame.Fence.ExecutionID, frame.Fence.Generation, projectDatabaseID)
	if err != nil {
		return err
	}
	if !bound || event.StreamID != binding.streamID || event.MessageID != binding.messageID ||
		event.ExecutionGeneration != binding.executionGeneration || event.SIOEvent != binding.sioEvent {
		return errors.New("current agent context conflicts with immutable admission")
	}
	schema, err := currentProjectSchema(projectID)
	if err != nil {
		return err
	}
	groupID, err := lockCurrentAgentMessageGroup(ctx, tx, schema, frame.Fence.ExecutionID, binding)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(contextsettings.RuntimeContext{
		Measurement: measurement, ExecutionID: frame.Fence.ExecutionID,
		Generation: frame.Fence.Generation, ExecutionGeneration: binding.executionGeneration,
		ResponseMessageID: binding.messageID,
	})
	if err != nil {
		return fmt.Errorf("encode current agent context: %w", err)
	}
	// A measurement without provider usage is the one the runtime sends
	// before a model request: its time is the start of the request whose
	// prompt cache an idle compaction reads (ADR-0050 decision 9b2).
	_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET meta = CASE WHEN $3::boolean
    THEN jsonb_set(jsonb_set(COALESCE(meta, '{}'::jsonb), '{runtime_context}',
         $2::jsonb || jsonb_build_object('recorded_at', CURRENT_TIMESTAMP)),
         '{runtime_request_started_at}', to_jsonb(CURRENT_TIMESTAMP))
    ELSE jsonb_set(COALESCE(meta, '{}'::jsonb), '{runtime_context}',
         $2::jsonb || jsonb_build_object('recorded_at', CURRENT_TIMESTAMP)) END
WHERE id = $1`, schema+".chat_message_group"), groupID, raw, measurement.ProviderUsage == nil)
	if err != nil {
		return fmt.Errorf("persist current agent context: %w", err)
	}
	return nil
}

// rootAgentContextEvent is the browser frame of an agent_context_status.
type rootAgentContextEvent struct {
	Type                string `json:"type"`
	StreamID            string `json:"stream_id"`
	MessageID           string `json:"message_id"`
	ExecutionGeneration string `json:"execution_generation"`
	SIOEvent            string `json:"sio_event"`
	ResponseMetadata    struct {
		currentAgentTextOwner
		ParentAgentCallID string          `json:"parent_agent_call_id"`
		ModelScope        string          `json:"model_scope"`
		ContextStatus     json.RawMessage `json:"context_status"`
	} `json:"response_metadata"`
}

// decodeRootAgentContextFrame decodes the root agent's context measurement;
// ok is false for any other frame.
func decodeRootAgentContextFrame(frame outputapp.NodeEventFrame) (rootAgentContextEvent, contextsettings.Measurement, bool, error) {
	var event rootAgentContextEvent
	if err := json.Unmarshal(frame.BrowserData, &event); err != nil {
		return event, contextsettings.Measurement{}, false, errors.New("decode current agent context event")
	}
	if event.Type != "agent_context_status" {
		return event, contextsettings.Measurement{}, false, nil
	}
	// Children and graph nodes have independent model histories. They never
	// replace the root composer's meter or get added into a fictitious total.
	if event.ResponseMetadata.isChild() || event.ResponseMetadata.ParentAgentCallID != "" || event.ResponseMetadata.ModelScope != "agent" {
		return event, contextsettings.Measurement{}, false, nil
	}
	measurement, err := contextsettings.DecodeMeasurement(event.ResponseMetadata.ContextStatus)
	if err != nil {
		return event, contextsettings.Measurement{}, false, err
	}
	return event, measurement, true, nil
}
