package repos

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"

	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type AgentExecutionResultsRepository struct {
	projects projectStore
}

func NewAgentExecutionResultsRepository(pool *pgxpool.Pool) (*AgentExecutionResultsRepository, error) {
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		return nil, err
	}
	return newAgentExecutionResultsRepository(projects)
}

func newAgentExecutionResultsRepository(projects projectStore) (*AgentExecutionResultsRepository, error) {
	if projects == nil {
		return nil, errors.New("agent result project database is required")
	}
	return &AgentExecutionResultsRepository{projects: projects}, nil
}

type currentAgentFullMessage struct {
	ReplacePipelineProvisional bool
	Content                    string
	ResultReference            bool
	ThreadID                   string
	References                 json.RawMessage
	InvokedSkills              json.RawMessage
	ResponseMetadata           json.RawMessage
	OutputLimitReached         bool
}

type currentAgentHITLPause struct {
	ThreadID      string
	Interrupt     json.RawMessage
	Interrupts    json.RawMessage
	InvokedSkills json.RawMessage
}

type currentAgentAuthorizationPause struct {
	ThreadID      string
	Requests      json.RawMessage
	InvokedSkills json.RawMessage
}

type currentAgentTerminal struct {
	Cursor             int64
	FullMessage        *currentAgentFullMessage
	HITLPause          *currentAgentHITLPause
	AuthorizationPause *currentAgentAuthorizationPause
	// AttachmentContents rides the terminal frame rather than being read back
	// out of the durable node event the way FullMessage is (#607). It has to:
	// the node event is the browser-facing `full_message`, and putting every
	// attached file's text on it would ship the whole document to the UI on
	// every turn -- the reason the proto rejected result_artifact as the
	// carrier. So this is the one part of the terminal that is taken from the
	// frame itself, already sanitised by
	// outputapp.AcceptedAgentExecutionAttachmentContents.
	AttachmentContents []outputapp.AgentExecutionAttachmentContent
}

type agentExecutionTerminalNodeEventQuerier interface {
	GetAgentExecutionTerminalNodeEvent(
		context.Context,
		sqlcgen.GetAgentExecutionTerminalNodeEventParams,
	) (sqlcgen.GetAgentExecutionTerminalNodeEventRow, error)
}

type currentAgentTerminalWriter interface {
	LockCurrentAgentResponseForTerminal(
		context.Context,
		sqlcgen.LockCurrentAgentResponseForTerminalParams,
	) (int32, error)
	InsertCurrentAgentTextItem(context.Context, int64) (int32, error)
	InsertCurrentAgentTextContent(
		context.Context,
		sqlcgen.InsertCurrentAgentTextContentParams,
	) error
	DeleteCurrentAgentProvisionalText(
		context.Context,
		sqlcgen.DeleteCurrentAgentProvisionalTextParams,
	) error
	FinalizeCurrentAgentFullMessage(
		context.Context,
		sqlcgen.FinalizeCurrentAgentFullMessageParams,
	) (int64, error)
	FinalizeCurrentAgentHITLPause(
		context.Context,
		sqlcgen.FinalizeCurrentAgentHITLPauseParams,
	) (int64, error)
	FinalizeCurrentAgentMixedPause(context.Context, sqlcgen.FinalizeCurrentAgentMixedPauseParams) (int64, error)
	FinalizeCurrentAgentAuthorizationPause(
		context.Context,
		sqlcgen.FinalizeCurrentAgentAuthorizationPauseParams,
	) (int64, error)
	GetCurrentAgentInvokedSkills(
		context.Context,
		int64,
	) (string, error)
	UpdateCurrentAgentAttachmentContent(
		context.Context,
		sqlcgen.UpdateCurrentAgentAttachmentContentParams,
	) (int64, error)
}

func (r *AgentExecutionResultsRepository) ProjectAgentExecution(ctx context.Context, projection outputapp.AgentExecutionProjection) (outputapp.ProjectionOutcome, error) {
	if err := projection.Frame.Validate(); err != nil || projection.Frame.Sequence > math.MaxInt64 {
		return outputapp.ProjectionOutcome{}, err
	}
	record, projectID, err := agentExecutionOutputRecord(projection.Frame)
	if err != nil {
		return outputapp.ProjectionOutcome{}, err
	}
	projectDatabaseID, ok := currentAgentDatabaseID(projectID)
	if !ok {
		return outputapp.ProjectionOutcome{}, outputapp.ErrInvalidAgentExecutionOutput
	}
	var outcome outputapp.ProjectionOutcome
	cancellationWon := false
	err = r.projects.WithinProjectTx(ctx, projectID, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		terminal, err := loadCurrentAgentTerminal(ctx, tx, projectDatabaseID, projection)
		if err != nil {
			return err
		}
		existing, err := loadExistingOutput(ctx, tx, record)
		if err == nil {
			if sameDurableOutput(existing, record) {
				outcome = outputapp.ProjectionOutcome{Inserted: false, Cursor: uint64(terminal.Cursor), CommittedSequence: record.Sequence}
				return nil
			}
			if sameCanonicalCancellation(existing, record) {
				cancellationWon = true
				return nil
			}
			return outputapp.ErrAgentExecutionOutputConflict
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		inserted, err := insertOutputInbox(ctx, tx, record)
		if err != nil {
			return err
		}
		if !inserted.Inserted {
			existing, loadErr := loadExistingOutput(ctx, tx, record)
			if loadErr == nil {
				if sameDurableOutput(existing, record) {
					outcome = outputapp.ProjectionOutcome{Inserted: false, Cursor: uint64(terminal.Cursor), CommittedSequence: record.Sequence}
					return nil
				}
				if sameCanonicalCancellation(existing, record) {
					cancellationWon = true
					return nil
				}
				return outputapp.ErrAgentExecutionOutputConflict
			}
			if !errors.Is(loadErr, pgx.ErrNoRows) {
				return loadErr
			}
			if inserted.CancellationRejected {
				if err := materializeCanonicalCancellation(ctx, tx, record, false); err != nil {
					return err
				}
				cancellationWon = true
				return nil
			}
			if inserted.DeadlineRejected {
				return outputapp.ErrOutputDeadlineExceeded
			}
			return runtimedomain.ErrStaleFence
		}
		if err := persistCurrentAgentTerminal(ctx, tx, projection.Expected, terminal); err != nil {
			return err
		}
		if err := markOutputProjected(ctx, tx, record.EventID); err != nil {
			return err
		}
		outcome = outputapp.ProjectionOutcome{Inserted: true, Cursor: uint64(terminal.Cursor), CommittedSequence: record.Sequence}
		return nil
	})
	if err != nil {
		return outputapp.ProjectionOutcome{}, err
	}
	if cancellationWon {
		return outputapp.ProjectionOutcome{}, outputapp.ErrOutputCancelled
	}
	return outcome, nil
}

func agentExecutionOutputRecord(frame outputapp.AgentExecutionFrame) (outputRecord, int64, error) {
	resourceProjectID, err := parseProjectID(frame.ResourceProjectID)
	if err != nil {
		return outputRecord{}, 0, outputapp.ErrInvalidAgentExecutionOutput
	}
	projectionProjectID, err := parseProjectID(frame.ProjectionProjectID)
	if err != nil {
		return outputRecord{}, 0, outputapp.ErrInvalidAgentExecutionOutput
	}
	return frameOutputRecord(
		frame.EventID, frame.LogicalOutputID, frame.StreamID, frame.TenantID,
		resourceProjectID, projectionProjectID, frame.Sequence,
		frame.ClaimHandoffWatermark, frame.OccurredAt, frame.Fence,
		payloadTypeAgentExecutionResult, frame.PayloadDigest, frame.EncodedResult,
		frame.Settlement, frame.EncodedSettlement,
	), projectionProjectID, nil
}

func loadCurrentAgentTerminal(ctx context.Context, tx sqlExecutor, projectID int32, projection outputapp.AgentExecutionProjection) (currentAgentTerminal, error) {
	frame := projection.Frame
	artifact := frame.Result.ResultArtifact
	queries, ok := tx.(agentExecutionTerminalNodeEventQuerier)
	if !ok {
		return currentAgentTerminal{}, errors.New("agent execution terminal query is unavailable")
	}
	row, err := queries.GetAgentExecutionTerminalNodeEvent(
		ctx,
		sqlcgen.GetAgentExecutionTerminalNodeEventParams{
			ExecutionID:         frame.Fence.ExecutionID,
			Generation:          int64(frame.Fence.Generation),
			ProjectionProjectID: projectID,
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return currentAgentTerminal{}, outputapp.ErrAgentExecutionResultMismatch
	}
	if err != nil {
		return currentAgentTerminal{}, fmt.Errorf("load durable agent terminal event: %w", err)
	}
	digest, err := storedDigest(row.LastNodeEventDigest)
	if err != nil || row.LastNodeCursor == nil || *row.LastNodeCursor <= 0 || row.LastNodeSequence != int64(frame.Sequence)-1 ||
		len(row.LastNodeEventBytes) != int(artifact.ByteLength) || runtimedomain.SHA256(row.LastNodeEventBytes) != digest || digest != artifact.Digest ||
		artifact.ImmutableVersion != "sha256:"+hex.EncodeToString(digest[:]) {
		return currentAgentTerminal{}, outputapp.ErrAgentExecutionResultMismatch
	}
	var event struct {
		Type                string          `json:"type"`
		StreamID            string          `json:"stream_id"`
		MessageID           string          `json:"message_id"`
		Content             json.RawMessage `json:"content"`
		References          json.RawMessage `json:"references"`
		SIOEvent            string          `json:"sio_event"`
		ExecutionGeneration string          `json:"execution_generation"`
		ResponseMetadata    json.RawMessage `json:"response_metadata"`
	}
	if json.Unmarshal(row.LastNodeEventBytes, &event) != nil ||
		event.StreamID != projection.Expected.ClientStreamID || event.MessageID != projection.Expected.ClientMessageID ||
		event.SIOEvent != projection.Expected.SIOEvent || event.ExecutionGeneration != projection.Expected.ClientExecutionGeneration {
		return currentAgentTerminal{}, outputapp.ErrAgentExecutionResultMismatch
	}
	terminal := currentAgentTerminal{
		Cursor:             *row.LastNodeCursor,
		AttachmentContents: frame.Result.AttachmentContents,
	}
	switch frame.Result.TerminalState {
	case outputapp.AgentExecutionTerminalCompleted:
		if artifact.ArtifactID != "node-event:"+frame.Fence.ExecutionID+":full-message" || event.Type != "full_message" {
			return currentAgentTerminal{}, outputapp.ErrAgentExecutionResultMismatch
		}
		fullMessage, err := decodeCurrentAgentFullMessage(event.Content, event.References, event.ResponseMetadata)
		if err != nil {
			return currentAgentTerminal{}, err
		}
		terminal.FullMessage = &fullMessage
	case outputapp.AgentExecutionTerminalPausedHITL:
		if artifact.ArtifactID != "node-event:"+frame.Fence.ExecutionID+":hitl-interrupt" || event.Type != "agent_hitl_interrupt" {
			return currentAgentTerminal{}, outputapp.ErrAgentExecutionResultMismatch
		}
		pause, err := decodeCurrentAgentHITLPause(event.Content, event.ResponseMetadata)
		if err != nil {
			return currentAgentTerminal{}, err
		}
		terminal.HITLPause = &pause
		var metadata map[string]json.RawMessage
		if json.Unmarshal(event.ResponseMetadata, &metadata) != nil {
			return currentAgentTerminal{}, outputapp.ErrAgentExecutionResultMismatch
		}
		if _, mixed := metadata["authorization_requests"]; mixed {
			authorization, err := decodeCurrentAgentAuthorizationPause(event.Content, event.ResponseMetadata)
			if err != nil {
				return currentAgentTerminal{}, err
			}
			var hitl, requests []map[string]any
			if json.Unmarshal(pause.Interrupts, &hitl) != nil || json.Unmarshal(authorization.Requests, &requests) != nil || len(hitl)+len(requests) > 16 {
				return currentAgentTerminal{}, outputapp.ErrAgentExecutionResultMismatch
			}
			for _, request := range requests {
				for _, interrupt := range hitl {
					if currentAgentAuthorizationIdentity(request) == interrupt["interrupt_id"] {
						return currentAgentTerminal{}, outputapp.ErrAgentExecutionResultMismatch
					}
				}
			}
			terminal.AuthorizationPause = &authorization
		}
	case outputapp.AgentExecutionTerminalPausedAuthorization:
		if artifact.ArtifactID != "node-event:"+frame.Fence.ExecutionID+":mcp-authorization-required" ||
			event.Type != "mcp_authorization_required" {
			return currentAgentTerminal{}, outputapp.ErrAgentExecutionResultMismatch
		}
		pause, err := decodeCurrentAgentAuthorizationPause(event.Content, event.ResponseMetadata)
		if err != nil {
			return currentAgentTerminal{}, err
		}
		terminal.AuthorizationPause = &pause
	default:
		return currentAgentTerminal{}, outputapp.ErrAgentExecutionResultMismatch
	}
	return terminal, nil
}

func decodeCurrentAgentFullMessage(contentJSON, references, responseMetadata json.RawMessage) (currentAgentFullMessage, error) {
	var content string
	resultReference := string(contentJSON) == "null"
	if !resultReference && json.Unmarshal(contentJSON, &content) != nil {
		return currentAgentFullMessage{}, outputapp.ErrAgentExecutionResultMismatch
	}
	var metadata struct {
		ResultRef          json.RawMessage `json:"result_ref_v1"`
		ThreadID           string          `json:"thread_id"`
		InvokedSkills      json.RawMessage `json:"invoked_skills"`
		OutputLimitReached bool            `json:"output_limit_reached"`
		ApplicationDetails struct {
			AgentType      string `json:"agent_type"`
			VersionDetails struct {
				AgentType string `json:"agent_type"`
			} `json:"version_details"`
		} `json:"application_details"`
	}
	if json.Unmarshal(responseMetadata, &metadata) != nil || resultReference != (len(metadata.ResultRef) != 0) || metadata.ThreadID == "" ||
		len(content) > 4*1024*1024 || strings.ContainsRune(content, '\x00') {
		return currentAgentFullMessage{}, outputapp.ErrAgentExecutionResultMismatch
	}
	invokedSkills, err := mergeCurrentAgentInvokedSkills(nil, metadata.InvokedSkills)
	if err != nil {
		return currentAgentFullMessage{}, outputapp.ErrAgentExecutionResultMismatch
	}
	return currentAgentFullMessage{
		ReplacePipelineProvisional: metadata.ApplicationDetails.AgentType == "pipeline" || metadata.ApplicationDetails.VersionDetails.AgentType == "pipeline",
		Content:                    content,
		ResultReference:            resultReference,
		ThreadID:                   metadata.ThreadID,
		References:                 cloneJSONOrDefault(references, []byte("[]")),
		InvokedSkills:              invokedSkills,
		ResponseMetadata:           append(json.RawMessage(nil), responseMetadata...),
		OutputLimitReached:         metadata.OutputLimitReached,
	}, nil
}

func decodeCurrentAgentHITLPause(contentJSON, responseMetadata json.RawMessage) (currentAgentHITLPause, error) {
	var content string
	var metadata struct {
		ThreadID       string          `json:"thread_id"`
		HITLInterrupt  json.RawMessage `json:"hitl_interrupt"`
		HITLInterrupts json.RawMessage `json:"hitl_interrupts"`
		InvokedSkills  json.RawMessage `json:"invoked_skills"`
	}
	if json.Unmarshal(contentJSON, &content) != nil || content == "" || len(content) > 64*1024 ||
		strings.ContainsRune(content, '\x00') || json.Unmarshal(responseMetadata, &metadata) != nil || metadata.ThreadID == "" {
		return currentAgentHITLPause{}, outputapp.ErrAgentExecutionResultMismatch
	}
	var interrupt map[string]any
	var interrupts []map[string]any
	if json.Unmarshal(metadata.HITLInterrupt, &interrupt) != nil ||
		json.Unmarshal(metadata.HITLInterrupts, &interrupts) != nil ||
		len(interrupts) == 0 || len(interrupts) > 16 || !reflect.DeepEqual(interrupt, interrupts[0]) {
		return currentAgentHITLPause{}, outputapp.ErrAgentExecutionResultMismatch
	}
	interruptID, _ := interrupt["interrupt_id"].(string)
	if interruptID == "" {
		return currentAgentHITLPause{}, outputapp.ErrAgentExecutionResultMismatch
	}
	seen := make(map[string]struct{}, len(interrupts))
	for _, pending := range interrupts {
		pendingID, _ := pending["interrupt_id"].(string)
		if pendingID == "" || !validInProcessHITLInterrupt(pending) {
			return currentAgentHITLPause{}, outputapp.ErrAgentExecutionResultMismatch
		}
		if _, duplicate := seen[pendingID]; duplicate {
			return currentAgentHITLPause{}, outputapp.ErrAgentExecutionResultMismatch
		}
		seen[pendingID] = struct{}{}
	}
	invokedSkills, err := mergeCurrentAgentInvokedSkills(nil, metadata.InvokedSkills)
	if err != nil {
		return currentAgentHITLPause{}, outputapp.ErrAgentExecutionResultMismatch
	}
	return currentAgentHITLPause{
		ThreadID:      metadata.ThreadID,
		Interrupt:     append(json.RawMessage(nil), metadata.HITLInterrupt...),
		Interrupts:    append(json.RawMessage(nil), metadata.HITLInterrupts...),
		InvokedSkills: invokedSkills,
	}, nil
}

func decodeCurrentAgentAuthorizationPause(contentJSON, responseMetadata json.RawMessage) (currentAgentAuthorizationPause, error) {
	var content string
	var metadata struct {
		ThreadID              string          `json:"thread_id"`
		InterruptID           string          `json:"interrupt_id"`
		ToolRunID             string          `json:"tool_run_id"`
		AuthorizationRequests json.RawMessage `json:"authorization_requests"`
		InvokedSkills         json.RawMessage `json:"invoked_skills"`
	}
	if json.Unmarshal(contentJSON, &content) != nil || content == "" || len(content) > 64*1024 ||
		strings.ContainsRune(content, '\x00') || json.Unmarshal(responseMetadata, &metadata) != nil ||
		metadata.ThreadID == "" || len(metadata.ThreadID) > 256 || strings.ContainsRune(metadata.ThreadID, '\x00') {
		return currentAgentAuthorizationPause{}, outputapp.ErrAgentExecutionResultMismatch
	}
	var requests []map[string]any
	if json.Unmarshal(metadata.AuthorizationRequests, &requests) != nil || len(requests) == 0 || len(requests) > 16 {
		return currentAgentAuthorizationPause{}, outputapp.ErrAgentExecutionResultMismatch
	}
	seen := make(map[string]struct{}, len(requests))
	for _, request := range requests {
		requestID := currentAgentAuthorizationIdentity(request)
		serverURL, _ := request["server_url"].(string)
		if !validCurrentAgentAuthorizationText(requestID, 512) ||
			!validCurrentAgentAuthorizationText(serverURL, 4096) {
			return currentAgentAuthorizationPause{}, outputapp.ErrAgentExecutionResultMismatch
		}
		if _, duplicate := seen[requestID]; duplicate {
			return currentAgentAuthorizationPause{}, outputapp.ErrAgentExecutionResultMismatch
		}
		seen[requestID] = struct{}{}
		if rawHierarchy, exists := request["metadata"]; exists && rawHierarchy != nil {
			hierarchy, ok := rawHierarchy.(map[string]any)
			if !ok || !validCurrentAgentAuthorizationHierarchy(hierarchy) {
				return currentAgentAuthorizationPause{}, outputapp.ErrAgentExecutionResultMismatch
			}
		}
	}
	lastID := currentAgentAuthorizationIdentity(requests[len(requests)-1])
	terminalID := metadata.InterruptID
	if terminalID == "" {
		terminalID = metadata.ToolRunID
	}
	if terminalID != lastID {
		return currentAgentAuthorizationPause{}, outputapp.ErrAgentExecutionResultMismatch
	}
	invokedSkills, err := mergeCurrentAgentInvokedSkills(nil, metadata.InvokedSkills)
	if err != nil {
		return currentAgentAuthorizationPause{}, outputapp.ErrAgentExecutionResultMismatch
	}
	return currentAgentAuthorizationPause{
		ThreadID:      metadata.ThreadID,
		Requests:      append(json.RawMessage(nil), metadata.AuthorizationRequests...),
		InvokedSkills: invokedSkills,
	}, nil
}

func currentAgentAuthorizationIdentity(request map[string]any) string {
	for _, key := range []string{"interrupt_id", "tool_run_id", "tool_call_id"} {
		if value, _ := request[key].(string); value != "" {
			return value
		}
	}
	return ""
}

func validCurrentAgentAuthorizationText(value string, limit int) bool {
	return value != "" && len(value) <= limit && !strings.ContainsRune(value, '\x00')
}

func validCurrentAgentAuthorizationHierarchy(hierarchy map[string]any) bool {
	for _, key := range []string{"parent_agent_name", "parent_agent_call_id", "child_thread_id", "thread_id", "checkpoint_ns", "langgraph_node"} {
		if value, exists := hierarchy[key]; exists && value != nil {
			text, ok := value.(string)
			if !ok || len(text) > 512 || strings.ContainsRune(text, '\x00') {
				return false
			}
		}
	}
	if rawPath, exists := hierarchy["parent_agent_path"]; exists && rawPath != nil {
		path, ok := rawPath.([]any)
		if !ok || len(path) > 3 {
			return false
		}
		for _, rawEntry := range path {
			entry, ok := rawEntry.(map[string]any)
			if !ok {
				return false
			}
			for _, key := range []string{"name", "call_id"} {
				if value, exists := entry[key]; exists && value != nil {
					text, ok := value.(string)
					if !ok || len(text) > 512 || strings.ContainsRune(text, '\x00') {
						return false
					}
				}
			}
		}
	}
	return true
}

// validInProcessHITLInterrupt admits root and synchronously nested pauses. A
// child-thread identity or explicit routing marker belongs to the parallel
// child-dispatch protocol and remains outside this continuation slice.
func validInProcessHITLInterrupt(interrupt map[string]any) bool {
	for _, key := range []string{"child_thread_id", "via_call_id", "_via_call_id"} {
		if value, exists := interrupt[key]; exists && value != nil && value != "" {
			return false
		}
	}
	callID, hasCallID := interrupt["parent_agent_call_id"]
	if hasCallID && !validCurrentAgentHITLText(callID, 512) {
		return false
	}
	pathValue, hasPath := interrupt["parent_agent_path"]
	if !hasPath || pathValue == nil {
		return !hasCallID
	}
	path, ok := pathValue.([]any)
	if !ok || len(path) == 0 || len(path) > 3 || !hasCallID {
		return false
	}
	for index, raw := range path {
		entry, ok := raw.(map[string]any)
		if !ok || !validCurrentAgentHITLText(entry["name"], 256) ||
			!validCurrentAgentHITLText(entry["call_id"], 512) {
			return false
		}
		if index == len(path)-1 && entry["call_id"] != callID {
			return false
		}
	}
	return true
}

func validCurrentAgentHITLText(value any, limit int) bool {
	text, ok := value.(string)
	return ok && text != "" && len(text) <= limit && !strings.ContainsRune(text, '\x00')
}

func persistCurrentAgentTerminal(ctx context.Context, tx sqlExecutor, expected outputapp.ExpectedAgentExecution, terminal currentAgentTerminal) error {
	writer, ok := tx.(currentAgentTerminalWriter)
	if !ok {
		return errors.New("current agent terminal writer is unavailable")
	}
	messageGroupID, err := writer.LockCurrentAgentResponseForTerminal(
		ctx,
		sqlcgen.LockCurrentAgentResponseForTerminalParams{
			MessageID:           expected.ClientMessageID,
			ConversationID:      expected.ClientStreamID,
			ExecutionID:         expected.ExecutionID,
			ExecutionGeneration: expected.ClientExecutionGeneration,
		},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("current agent response message group is unavailable")
	}
	if err != nil {
		return fmt.Errorf("lock current agent response message group: %w", err)
	}
	existingSkills, err := writer.GetCurrentAgentInvokedSkills(ctx, int64(messageGroupID))
	if err != nil {
		return fmt.Errorf("load current agent invoked skills: %w", err)
	}
	if err := persistCurrentAgentAttachmentContents(
		ctx, writer, int64(messageGroupID), terminal.AttachmentContents,
	); err != nil {
		return err
	}
	if terminal.FullMessage != nil && terminal.HITLPause == nil && terminal.AuthorizationPause == nil {
		message := terminal.FullMessage
		if message.ResultReference {
			content, err := resolveCurrentAgentResultContent(ctx, tx, expected, message.ResponseMetadata)
			if err != nil {
				return err
			}
			message.Content = content
		}
		invokedSkills, err := mergeCurrentAgentInvokedSkills([]byte(existingSkills), message.InvokedSkills)
		if err != nil {
			return outputapp.ErrAgentExecutionResultMismatch
		}
		if err := writer.DeleteCurrentAgentProvisionalText(
			ctx,
			sqlcgen.DeleteCurrentAgentProvisionalTextParams{
				ReplacePipelineProvisional: message.ReplacePipelineProvisional,
				MessageGroupID:             int64(messageGroupID),
				ExecutionID:                expected.ExecutionID,
				Generation:                 int64(expected.Generation),
			},
		); err != nil {
			return fmt.Errorf("delete current agent provisional text: %w", err)
		}
		itemID, err := writer.InsertCurrentAgentTextItem(ctx, int64(messageGroupID))
		if err != nil {
			return fmt.Errorf("insert current agent text item: %w", err)
		}
		if err := writer.InsertCurrentAgentTextContent(
			ctx,
			sqlcgen.InsertCurrentAgentTextContentParams{ItemID: int64(itemID), Content: message.Content},
		); err != nil {
			return fmt.Errorf("insert current agent text content: %w", err)
		}
		rows, err := writer.FinalizeCurrentAgentFullMessage(
			ctx,
			sqlcgen.FinalizeCurrentAgentFullMessageParams{
				ThreadID:           message.ThreadID,
				ReferencesJson:     []byte(message.References),
				InvokedSkills:      []byte(invokedSkills),
				OutputLimitReached: message.OutputLimitReached,
				MessageGroupID:     int64(messageGroupID),
			},
		)
		if err != nil || rows != 1 {
			return fmt.Errorf("finalize current agent response message group: %w", terminalWriteError(err))
		}
		return nil
	}
	if terminal.HITLPause != nil && terminal.AuthorizationPause != nil && terminal.FullMessage == nil {
		pause, authorization := terminal.HITLPause, terminal.AuthorizationPause
		if pause.ThreadID != authorization.ThreadID {
			return outputapp.ErrAgentExecutionResultMismatch
		}
		skills, err := mergeCurrentAgentInvokedSkills([]byte(existingSkills), pause.InvokedSkills)
		if err != nil {
			return outputapp.ErrAgentExecutionResultMismatch
		}
		skills, err = mergeCurrentAgentInvokedSkills(skills, authorization.InvokedSkills)
		if err != nil {
			return outputapp.ErrAgentExecutionResultMismatch
		}
		rows, err := writer.FinalizeCurrentAgentMixedPause(ctx, sqlcgen.FinalizeCurrentAgentMixedPauseParams{
			ThreadID: pause.ThreadID, HitlInterrupt: []byte(pause.Interrupt), HitlInterrupts: []byte(pause.Interrupts),
			AuthorizationRequests: []byte(authorization.Requests), InvokedSkills: []byte(skills), MessageGroupID: int64(messageGroupID),
		})
		if err != nil || rows != 1 {
			return fmt.Errorf("persist current agent mixed pause: %w", terminalWriteError(err))
		}
		return nil
	}
	if terminal.HITLPause != nil && terminal.FullMessage == nil && terminal.AuthorizationPause == nil {
		pause := terminal.HITLPause
		invokedSkills, err := mergeCurrentAgentInvokedSkills([]byte(existingSkills), pause.InvokedSkills)
		if err != nil {
			return outputapp.ErrAgentExecutionResultMismatch
		}
		rows, err := writer.FinalizeCurrentAgentHITLPause(
			ctx,
			sqlcgen.FinalizeCurrentAgentHITLPauseParams{
				ThreadID:       pause.ThreadID,
				HitlInterrupt:  []byte(pause.Interrupt),
				HitlInterrupts: []byte(pause.Interrupts),
				InvokedSkills:  []byte(invokedSkills),
				MessageGroupID: int64(messageGroupID),
			},
		)
		if err != nil || rows != 1 {
			return fmt.Errorf("persist current agent HITL pause: %w", terminalWriteError(err))
		}
		return nil
	}
	if terminal.AuthorizationPause != nil && terminal.FullMessage == nil && terminal.HITLPause == nil {
		pause := terminal.AuthorizationPause
		invokedSkills, err := mergeCurrentAgentInvokedSkills([]byte(existingSkills), pause.InvokedSkills)
		if err != nil {
			return outputapp.ErrAgentExecutionResultMismatch
		}
		rows, err := writer.FinalizeCurrentAgentAuthorizationPause(
			ctx,
			sqlcgen.FinalizeCurrentAgentAuthorizationPauseParams{
				ThreadID:              pause.ThreadID,
				AuthorizationRequests: []byte(pause.Requests),
				InvokedSkills:         []byte(invokedSkills),
				MessageGroupID:        int64(messageGroupID),
			},
		)
		if err != nil || rows != 1 {
			return fmt.Errorf("persist current agent authorization pause: %w", terminalWriteError(err))
		}
		return nil
	}
	return outputapp.ErrAgentExecutionResultMismatch
}

// persistCurrentAgentAttachmentContents writes back the text the worker
// extracted from this turn's attachments (#607), inside the terminal
// transaction and under the lock LockCurrentAgentResponseForTerminal already
// took on the response group.
//
// IT RUNS FOR ALL THREE TERMINAL STATES, before the branch that separates them,
// not only for a completed answer. A turn that pauses on HITL or on an MCP
// authorization prompt has ALREADY read its attachments -- the extraction
// happens while the agent builds its first message, long before an interrupt --
// and the conversation it paused in is exactly the one that will ask a
// follow-up question about the file. Persisting only on completion would drop
// the text of every file attached to a turn that paused, which is the turn most
// likely to be continued.
//
// A row that reports 0 rows affected is a NO-OP, not an error, and that is
// deliberate. 0 rows means the SQL scope refused the id -- the item is not on
// this turn's question group -- which is the protection working, and the
// protection has already been applied by the time the count comes back. Turning
// it into an error here would fail the terminal projection and lose the
// assistant's answer over a row that was never written, i.e. hand a worker that
// names a foreign id the power to destroy the turn it was running. The same
// reasoning as AcceptedAgentExecutionAttachmentContents' drop rule, one layer
// down. A real database error is still returned: that is the transaction being
// unable to proceed, not a refused write.
func persistCurrentAgentAttachmentContents(
	ctx context.Context,
	writer currentAgentTerminalWriter,
	responseMessageGroupID int64,
	contents []outputapp.AgentExecutionAttachmentContent,
) error {
	for _, entry := range contents {
		itemID, err := currentPGUUID(entry.ItemID)
		if err != nil {
			// Unreachable: the acceptance rule already demands a canonical
			// uuid. Skipping rather than failing keeps the invariant's one
			// possible future violation from costing the answer.
			continue
		}
		if _, err := writer.UpdateCurrentAgentAttachmentContent(
			ctx,
			sqlcgen.UpdateCurrentAgentAttachmentContentParams{
				Content:                []byte(entry.Content),
				ItemID:                 itemID,
				ResponseMessageGroupID: responseMessageGroupID,
			},
		); err != nil {
			return fmt.Errorf("persist current agent attachment content: %w", err)
		}
	}
	return nil
}

func terminalWriteError(err error) error {
	if err != nil {
		return err
	}
	return errors.New("current agent terminal row was not updated")
}

func cloneJSONOrDefault(value, fallback []byte) json.RawMessage {
	if len(value) == 0 || !json.Valid(value) {
		return append(json.RawMessage(nil), fallback...)
	}
	return append(json.RawMessage(nil), value...)
}

var _ outputapp.AgentExecutionProjector = (*AgentExecutionResultsRepository)(nil)
