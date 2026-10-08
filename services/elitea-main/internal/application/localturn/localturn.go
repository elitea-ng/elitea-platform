// Package localturn records agent turns that run on the user's machine
// (ADR-0029 decision 5c, client contract 1.5).
//
// The desktop runs the agent loop locally and never joins the worker
// protocol. The platform's part is two calls:
//
//   - Start opens an execution owned by the caller in one project
//     conversation. Its id is what the desktop sends to /llm as
//     X-Elitea-Execution-Id, so model usage is attributed to the turn. Start
//     is refused while the native client policy's `local_work.allowed` is
//     false. Its answer carries the turn's memory recall, computed by the same
//     resolver a cloud turn's admission uses, from the primary database, on
//     every call (decision 8): it is never cached here or on the device.
//   - Commit appends the turn to the conversation through the existing chat
//     and trace projection tables, marks it `executed_by: desktop`, stamps
//     `memories_used`, and writes an audit event with a bounded summary of
//     the commands the client ran and the paths it touched. It is idempotent
//     on the execution id.
//
// A turn the desktop never commits expires after DeadlineTTL, the cloud agent
// deadline, the way an unclaimed cloud run does.
//
// The server cannot verify what happened on the device. The audit is a record
// of what the client reported, not of what it did.
package localturn

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

// Bounds. The request bounds keep one commit a bounded write; the report
// bounds keep the record of what the client did a bounded row.
const (
	// DeadlineTTL is how long a started turn may stay uncommitted. It is the
	// cloud agent deadline (runtimecomposition agentDeadlineTTL).
	DeadlineTTL = 24 * time.Hour

	MaxUserMessageBytes      = 256 * 1024
	MaxAssistantMessageBytes = 1024 * 1024
	MaxAssistantErrorBytes   = 4 * 1024
	MaxToolCalls             = 512
	MaxThinkingSteps         = 512
	MaxHITLExchanges         = 64
	MaxHITLFieldBytes        = 4 * 1024
	MaxHITLTokenBytes        = 128

	// The local work report: at most MaxReportedCommands commands of at most
	// MaxReportedCommandBytes each, and MaxReportedPaths paths of at most
	// MaxReportedPathBytes. Longer entries are cut and further entries are
	// dropped, never refused; the totals record what the client reported.
	MaxReportedCommands     = 50
	MaxReportedCommandBytes = 300
	MaxReportedPaths        = 200
	MaxReportedPathBytes    = 400

	// auditPreviewItems is how many commands and paths the audit event names.
	auditPreviewItems = 3
	// auditEntityType is the audit event's entity type for both calls.
	auditEntityType = "local_turn"
	// ExecutedByDesktop is the `executed_by` meta value of a committed turn.
	ExecutedByDesktop = "desktop"
)

var (
	ErrInvalid = errors.New("invalid local turn request")
	// ErrLocalWorkDisabled: the native client policy does not allow local work.
	ErrLocalWorkDisabled = errors.New("local work is disabled by the native client policy")
	// ErrUnavailable: the policy or the memory recall could not be read. The
	// start is refused rather than run without either (decision 8: a turn
	// that cannot reach start does not run).
	ErrUnavailable = errors.New("local turn dependencies are unavailable")
	// ErrNotFound: the conversation, or the execution, does not exist for this
	// caller. One error for both, so a caller learns nothing about another
	// caller's turns.
	ErrNotFound = errors.New("local turn target not found")
	// ErrParticipant: the answering participant is not an agent or model
	// participant of the conversation.
	ErrParticipant = errors.New("local turn participant is not in the conversation")
	// ErrConflict: the question id is already bound to another conversation
	// or participant, or names a message that already exists.
	ErrConflict = errors.New("local turn request conflicts with an existing turn")
	// ErrAlreadyCommitted: the execution was committed with a different body,
	// or a start was retried after the commit.
	ErrAlreadyCommitted = errors.New("local turn is already committed")
	// ErrExpired: the execution passed its deadline before the commit.
	ErrExpired = errors.New("local turn has expired")
)

// PolicyReader is the native client policy (*nativepolicy.Service).
type PolicyReader interface {
	Policy(ctx context.Context) (platformconfig.NativeClientPolicy, error)
}

// Store persists local turn executions and their commit.
type Store interface {
	StartLocalTurn(ctx context.Context, record StartRecord) (StartedTurn, error)
	CommitLocalTurn(ctx context.Context, record CommitRecord) (CommittedTurn, error)
}

// IDGenerator mints an execution id: 32 lowercase hex characters.
type IDGenerator func() (string, error)

// StartRequest is one start call.
type StartRequest struct {
	ProjectID        int64
	ActorUserID      int64
	TokenID          string
	NativeClientID   string
	ConversationUUID string
	QuestionID       string
	UserInput        string
	// ParticipantID names the conversation's agent participant that answers;
	// 0 means the conversation's model (dummy) participant.
	ParticipantID int64
	// AuditRoute is the matched route pattern for the audit event.
	AuditRoute string
}

// MemoryRecall is the turn's recalled memory, the same value a cloud turn's
// admission splices onto its instructions.
type MemoryRecall struct {
	Text  string
	Count int
	IDs   []string
}

// StartOutcome answers a start.
type StartOutcome struct {
	ExecutionID       string
	QuestionID        string
	ResponseMessageID string
	ConversationUUID  string
	ParticipantID     int64
	ExpiresAt         time.Time
	Created           bool
	Recall            MemoryRecall
}

// StartRecord is what the store writes for a start.
type StartRecord struct {
	ExecutionID       string
	ProjectID         int64
	ActorUserID       int64
	TokenID           string
	NativeClientID    string
	ConversationUUID  string
	QuestionID        string
	ResponseMessageID string
	ParticipantID     int64
	MemoriesUsed      int
	TTL               time.Duration
}

// StartedTurn is the stored execution after a start (new or replayed).
type StartedTurn struct {
	ExecutionID       string
	ResponseMessageID string
	ParticipantID     int64
	ExpiresAt         time.Time
	Created           bool
}

// HITLExchange is one approval or question the desktop resolved during the
// turn.
type HITLExchange struct {
	InterruptID string `json:"interrupt_id"`
	Kind        string `json:"kind"`
	ToolRunID   string `json:"tool_run_id,omitempty"`
	Prompt      string `json:"prompt"`
	Decision    string `json:"decision"`
	Value       string `json:"value,omitempty"`
}

// CommandRecord is one command the client reports it ran.
type CommandRecord struct {
	Command  string `json:"command"`
	ExitCode *int   `json:"exit_code,omitempty"`
}

// LocalWorkReport is what the client reports it did on the device.
type LocalWorkReport struct {
	SandboxMode string          `json:"sandbox_mode,omitempty"`
	Enforcement string          `json:"enforcement,omitempty"`
	Commands    []CommandRecord `json:"commands"`
	Paths       []string        `json:"paths_touched"`
}

// CommitRequest is one commit call.
type CommitRequest struct {
	ProjectID        int64
	ActorUserID      int64
	ExecutionID      string
	UserMessage      string
	AssistantMessage string
	AssistantIsError bool
	AssistantError   string
	// ToolCalls is the runtime's `tool_calls` object, keyed by run id, in the
	// shape a worker streams in partial_message.response_metadata.
	ToolCalls json.RawMessage
	// ThinkingSteps are the runtime's `thinking_steps` entries.
	ThinkingSteps []json.RawMessage
	HITLExchanges []HITLExchange
	Report        LocalWorkReport
	AuditRoute    string
}

// CommitRecord is what the store writes for a commit.
type CommitRecord struct {
	ProjectID        int64
	ActorUserID      int64
	ExecutionID      string
	UserMessage      string
	AssistantMessage string
	// QuestionMeta and ResponseMeta are server-built JSON objects.
	QuestionMeta  json.RawMessage
	ResponseMeta  json.RawMessage
	ToolCalls     json.RawMessage
	ThinkingSteps []json.RawMessage
	Digest        [32]byte
}

// CommittedTurn answers a commit.
type CommittedTurn struct {
	ExecutionID       string
	ConversationUUID  string
	QuestionMessageID string
	ResponseMessageID string
	MemoriesUsed      int
	CommittedAt       time.Time
	Created           bool
}

// Service is the local turn use case.
type Service struct {
	store    Store
	policy   PolicyReader
	memories agentexecutionapp.CurrentMemoryRecallResolver
	recorder audit.Recorder
	newID    IDGenerator
	now      func() time.Time
	logger   *slog.Logger
}

// NewService builds the use case. Every dependency is required: a start
// without a policy reader could not refuse, and one without a recall resolver
// would break the next-turn memory guarantee.
func NewService(
	store Store,
	policy PolicyReader,
	memories agentexecutionapp.CurrentMemoryRecallResolver,
	recorder audit.Recorder,
	newID IDGenerator,
	logger *slog.Logger,
) (*Service, error) {
	if store == nil || policy == nil || memories == nil || recorder == nil || newID == nil {
		return nil, errors.New("local turn dependencies are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		store: store, policy: policy, memories: memories, recorder: recorder,
		newID: newID, now: time.Now, logger: logger,
	}, nil
}

var (
	canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	executionID   = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// ValidExecutionID reports whether id has the execution id shape.
func ValidExecutionID(id string) bool { return executionID.MatchString(id) }

// ValidUUID reports whether id is a lowercase canonical UUID.
func ValidUUID(id string) bool { return canonicalUUID.MatchString(id) }

// Start opens (or replays) a local turn and answers its memory recall.
func (s *Service) Start(ctx context.Context, request StartRequest) (StartOutcome, error) {
	started := s.now()
	if !validStart(request) {
		return StartOutcome{}, ErrInvalid
	}
	policy, err := s.policy.Policy(ctx)
	if err != nil {
		s.logger.ErrorContext(ctx, "local turn: native client policy unreadable", "err", err)
		return StartOutcome{}, ErrUnavailable
	}
	if !policy.LocalWork.Allowed {
		return StartOutcome{}, ErrLocalWorkDisabled
	}
	// Recall FIRST and always fresh: the store writes nothing to the
	// conversation at start, so the user's previous turn is still the one the
	// reservation rule compares against (MemoriesRepo.ResolveCurrentMemoryRecall).
	recall, err := s.memories.ResolveCurrentMemoryRecall(
		ctx, request.ProjectID, request.ActorUserID, strings.TrimSpace(request.UserInput))
	if err != nil {
		s.logger.ErrorContext(ctx, "local turn: memory recall failed", "err", err)
		return StartOutcome{}, ErrUnavailable
	}
	id, err := s.newID()
	if err != nil || !ValidExecutionID(id) {
		return StartOutcome{}, fmt.Errorf("local turn: mint execution id: %w", errors.Join(err, ErrUnavailable))
	}
	turn, err := s.store.StartLocalTurn(ctx, StartRecord{
		ExecutionID: id, ProjectID: request.ProjectID, ActorUserID: request.ActorUserID,
		TokenID: request.TokenID, NativeClientID: request.NativeClientID,
		ConversationUUID: request.ConversationUUID, QuestionID: request.QuestionID,
		ResponseMessageID: ResponseMessageID(request.QuestionID),
		ParticipantID:     request.ParticipantID, MemoriesUsed: recall.Count, TTL: DeadlineTTL,
	})
	if err != nil {
		return StartOutcome{}, err
	}
	if turn.Created {
		s.record(ctx, request.ActorUserID, request.ProjectID, request.AuditRoute, started,
			"local turn started", turn.ExecutionID)
	}
	ids := recall.IDs
	if ids == nil {
		ids = []string{}
	}
	return StartOutcome{
		ExecutionID: turn.ExecutionID, QuestionID: request.QuestionID,
		ResponseMessageID: turn.ResponseMessageID, ConversationUUID: request.ConversationUUID,
		ParticipantID: turn.ParticipantID, ExpiresAt: turn.ExpiresAt, Created: turn.Created,
		Recall: MemoryRecall{Text: recall.Text, Count: recall.Count, IDs: ids},
	}, nil
}

// Commit appends the turn to the conversation, once.
func (s *Service) Commit(ctx context.Context, request CommitRequest) (CommittedTurn, error) {
	started := s.now()
	if !validCommit(request) {
		return CommittedTurn{}, ErrInvalid
	}
	report := boundReport(request.Report)
	exchanges := request.HITLExchanges
	if exchanges == nil {
		exchanges = []HITLExchange{}
	}
	resolved := make([]string, 0, len(exchanges))
	for _, exchange := range exchanges {
		resolved = append(resolved, exchange.InterruptID)
	}
	questionMeta, err := json.Marshal(map[string]any{
		"executed_by":        ExecutedByDesktop,
		"local_execution_id": request.ExecutionID,
	})
	if err != nil {
		return CommittedTurn{}, err
	}
	responseMeta, err := json.Marshal(map[string]any{
		"executed_by":                 ExecutedByDesktop,
		"local_execution_id":          request.ExecutionID,
		"is_error":                    request.AssistantIsError,
		"error":                       request.AssistantError,
		"hitl_exchanges":              exchanges,
		"resolved_hitl_interrupt_ids": resolved,
		"local_work":                  report,
	})
	if err != nil {
		return CommittedTurn{}, err
	}
	digest, err := commitDigest(request)
	if err != nil {
		return CommittedTurn{}, ErrInvalid
	}
	turn, err := s.store.CommitLocalTurn(ctx, CommitRecord{
		ProjectID: request.ProjectID, ActorUserID: request.ActorUserID,
		ExecutionID: request.ExecutionID, UserMessage: request.UserMessage,
		AssistantMessage: request.AssistantMessage,
		QuestionMeta:     questionMeta, ResponseMeta: responseMeta,
		ToolCalls: request.ToolCalls, ThinkingSteps: request.ThinkingSteps, Digest: digest,
	})
	if err != nil {
		return CommittedTurn{}, err
	}
	// The SAME post-admission stamp a cloud turn gets
	// (CurrentMemoryRecallResolver.RecordCurrentMemoryUsage), after the
	// transcript is written. Repeated on a replayed commit, so a stamp a
	// failed first attempt missed is written by the retry; the write is a
	// merge and idempotent.
	if turn.MemoriesUsed > 0 {
		if err := s.memories.RecordCurrentMemoryUsage(ctx, request.ProjectID, turn.ResponseMessageID, turn.MemoriesUsed); err != nil {
			s.logger.WarnContext(ctx, "local turn: memories_used stamp failed",
				"execution_id", request.ExecutionID, "err", err)
		}
	}
	if turn.Created {
		s.record(ctx, request.ActorUserID, request.ProjectID, request.AuditRoute, started,
			commitAuditAction(report), request.ExecutionID)
	}
	return turn, nil
}

func (s *Service) record(ctx context.Context, userID, projectID int64, route string, started time.Time, action, executionID string) {
	status := int32(200)
	duration := float64(s.now().Sub(started).Microseconds()) / 1000
	s.recorder.Record(ctx, audit.Event{
		Timestamp:  s.now().UTC(),
		UserID:     audit.ID(userID),
		ProjectID:  audit.ID(projectID),
		EventType:  "agent",
		Action:     action,
		HTTPMethod: "POST",
		HTTPRoute:  route,
		StatusCode: &status,
		DurationMS: &duration,
		EntityType: auditEntityType,
		EntityName: executionID,
	})
}

// ResponseMessageID is the answer's message uuid for a question: derived, so a
// retried start answers the same id. The namespace is this package's own, so
// it never equals the id a cloud admission derives for the same question.
func ResponseMessageID(questionID string) string {
	return uuid.NewSHA1(localTurnNamespace, []byte(questionID+"\x00local-response")).String()
}

var localTurnNamespace = uuid.MustParse("6a0e7a36-63d1-4f3b-9a51-0c2d1d6b2a29")

func validStart(request StartRequest) bool {
	return request.ProjectID > 0 && request.ProjectID <= 2147483647 &&
		request.ActorUserID > 0 && request.ActorUserID <= 2147483647 &&
		request.TokenID != "" && request.ParticipantID >= 0 && request.ParticipantID <= 2147483647 &&
		ValidUUID(request.ConversationUUID) && ValidUUID(request.QuestionID) &&
		validText(request.UserInput, MaxUserMessageBytes, false)
}

func validCommit(request CommitRequest) bool {
	if request.ProjectID <= 0 || request.ProjectID > 2147483647 ||
		request.ActorUserID <= 0 || request.ActorUserID > 2147483647 ||
		!ValidExecutionID(request.ExecutionID) ||
		!validText(request.UserMessage, MaxUserMessageBytes, false) ||
		!validText(request.AssistantMessage, MaxAssistantMessageBytes, true) ||
		!validText(request.AssistantError, MaxAssistantErrorBytes, true) ||
		len(request.ThinkingSteps) > MaxThinkingSteps ||
		len(request.HITLExchanges) > MaxHITLExchanges {
		return false
	}
	if !request.AssistantIsError && request.AssistantError != "" {
		return false
	}
	if mode := request.Report.SandboxMode; mode != "" && !platformconfig.ValidSandboxMode(mode) {
		return false
	}
	switch request.Report.Enforcement {
	case "", "full", "partial", "none":
	default:
		return false
	}
	if len(request.Report.Commands) > 10*MaxReportedCommands || len(request.Report.Paths) > 10*MaxReportedPaths {
		return false
	}
	for _, exchange := range request.HITLExchanges {
		if !validToken(exchange.InterruptID) || !validToken(exchange.Kind) || !validToken(exchange.Decision) ||
			(exchange.ToolRunID != "" && !validToken(exchange.ToolRunID)) ||
			!validText(exchange.Prompt, MaxHITLFieldBytes, true) ||
			!validText(exchange.Value, MaxHITLFieldBytes, true) {
			return false
		}
	}
	return true
}

func validText(value string, limit int, allowEmpty bool) bool {
	if value == "" {
		return allowEmpty
	}
	return len(value) <= limit && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

// validToken is the shape of an identifier-like HITL field: 1 to 128 bytes of
// printable ASCII without spaces.
func validToken(value string) bool {
	if value == "" || len(value) > MaxHITLTokenBytes {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] <= ' ' || value[i] > '~' {
			return false
		}
	}
	return true
}

// boundedReport is the stored local work record: the bounded entries plus the
// totals the client reported.
type boundedReport struct {
	SandboxMode   string          `json:"sandbox_mode"`
	Enforcement   string          `json:"enforcement"`
	Commands      []CommandRecord `json:"commands"`
	CommandsTotal int             `json:"commands_total"`
	Paths         []string        `json:"paths_touched"`
	PathsTotal    int             `json:"paths_total"`
	Reported      string          `json:"reported_by"`
}

func boundReport(report LocalWorkReport) boundedReport {
	out := boundedReport{
		SandboxMode: report.SandboxMode, Enforcement: report.Enforcement,
		Commands: []CommandRecord{}, Paths: []string{}, Reported: "client",
		CommandsTotal: len(report.Commands), PathsTotal: len(report.Paths),
	}
	for _, command := range report.Commands {
		if len(out.Commands) == MaxReportedCommands {
			break
		}
		text := cleanReported(command.Command, MaxReportedCommandBytes)
		if text == "" {
			continue
		}
		out.Commands = append(out.Commands, CommandRecord{Command: text, ExitCode: command.ExitCode})
	}
	for _, path := range report.Paths {
		if len(out.Paths) == MaxReportedPaths {
			break
		}
		if text := cleanReported(path, MaxReportedPathBytes); text != "" {
			out.Paths = append(out.Paths, text)
		}
	}
	return out
}

// cleanReported makes one reported string storable: control characters and
// format characters (bidi overrides and isolates, zero-width marks) become
// spaces, invalid UTF-8 is dropped, and the result is cut to limit bytes on a
// rune boundary.
func cleanReported(value string, limit int) string {
	var builder strings.Builder
	for _, r := range strings.ToValidUTF8(value, "") {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			r = ' '
		}
		if builder.Len()+utf8.RuneLen(r) > limit {
			break
		}
		builder.WriteRune(r)
	}
	return strings.TrimSpace(builder.String())
}

// commitAuditAction is the audit event's action: a bounded summary of what the
// client reported (audit.Event.Action holds 512 characters).
func commitAuditAction(report boundedReport) string {
	preview := func(items []string) string {
		if len(items) > auditPreviewItems {
			items = items[:auditPreviewItems]
		}
		for i, item := range items {
			if utf8.RuneCountInString(item) > 60 {
				items[i] = string([]rune(item)[:60]) + "…"
			}
		}
		return strings.Join(items, "; ")
	}
	commands := make([]string, 0, len(report.Commands))
	for _, command := range report.Commands {
		commands = append(commands, command.Command)
	}
	paths := append([]string(nil), report.Paths...)
	action := "local turn committed: " + strconv.Itoa(report.CommandsTotal) + " commands"
	if len(commands) > 0 {
		action += " (" + preview(commands) + ")"
	}
	action += ", " + strconv.Itoa(report.PathsTotal) + " paths"
	if len(paths) > 0 {
		action += " (" + preview(paths) + ")"
	}
	if report.SandboxMode != "" {
		action += ", sandbox " + report.SandboxMode
	}
	if report.Enforcement != "" {
		action += ", enforcement " + report.Enforcement
	}
	if runes := []rune(action); len(runes) > 512 {
		action = string(runes[:511]) + "…"
	}
	return action
}

// commitDigest identifies a commit body, so a retried commit is recognised as
// the same one. It hashes the normalized request: the JSON of a struct with
// fixed field order and compacted raw parts.
func commitDigest(request CommitRequest) ([32]byte, error) {
	compact := func(raw json.RawMessage) (json.RawMessage, error) {
		if len(bytes.TrimSpace(raw)) == 0 {
			return json.RawMessage("null"), nil
		}
		var buffer bytes.Buffer
		if err := json.Compact(&buffer, raw); err != nil {
			return nil, err
		}
		return json.RawMessage(buffer.Bytes()), nil
	}
	toolCalls, err := compact(request.ToolCalls)
	if err != nil {
		return [32]byte{}, err
	}
	steps := make([]json.RawMessage, 0, len(request.ThinkingSteps))
	for _, step := range request.ThinkingSteps {
		compacted, err := compact(step)
		if err != nil {
			return [32]byte{}, err
		}
		steps = append(steps, compacted)
	}
	encoded, err := json.Marshal(struct {
		ExecutionID      string            `json:"execution_id"`
		UserMessage      string            `json:"user_message"`
		AssistantMessage string            `json:"assistant_message"`
		AssistantIsError bool              `json:"assistant_is_error"`
		AssistantError   string            `json:"assistant_error"`
		ToolCalls        json.RawMessage   `json:"tool_calls"`
		ThinkingSteps    []json.RawMessage `json:"thinking_steps"`
		HITLExchanges    []HITLExchange    `json:"hitl_exchanges"`
		Report           LocalWorkReport   `json:"local_work"`
	}{
		request.ExecutionID, request.UserMessage, request.AssistantMessage,
		request.AssistantIsError, request.AssistantError, toolCalls, steps,
		request.HITLExchanges, request.Report,
	})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}
