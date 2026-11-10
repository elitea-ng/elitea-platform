package control

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

// Bounds of one ReportEphemeralAgents request (ADR-0050 decisions 4 and 9d,
// #1614). MaxEphemeralAgents is the runtime's per-turn spawn cap.
const (
	MaxEphemeralAgents        = 12
	maxEphemeralLabelChars    = 60
	maxEphemeralAvatarBytes   = 128
	maxEphemeralCallIDBytes   = 256
	maxEphemeralBriefRefBytes = 8 * 1024
	maxEphemeralOrdinal       = 64
)

var (
	ephemeralAgentID     = regexp.MustCompile(`^a_[0-9a-f]{8}$`)
	ephemeralDigest      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	ephemeralErrorCode   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	ephemeralAgentStatus = map[string]struct{}{
		"running": {}, "completed": {}, "failed": {}, "budget_exhausted": {}, "cancelled": {},
	}
)

// EphemeralAgent is one spawn of a ReportEphemeralAgents request after its
// shape is checked. It names the profile and the brief by digest and by the
// ids the brief referenced, never by their text.
type EphemeralAgent struct {
	AgentID       string
	ParentCallID  string
	Ordinal       int
	Label         string
	DisplayName   string
	Avatar        string
	ProfileDigest string
	// BriefDigest and BriefRefs are both empty for a spawn without a brief.
	BriefDigest  string
	BriefRefs    json.RawMessage
	Status       string
	Steps        int64
	InputTokens  int64
	OutputTokens int64
	ErrorCode    string
	StartedAt    time.Time
	// FinishedAt is zero while the child runs.
	FinishedAt time.Time
}

// EphemeralAgents keeps the sub-agents a claimed execution started
// (elitea_runtime.ephemeral_agents, shared/0232): repos.EphemeralAgents in
// production.
type EphemeralAgents interface {
	// RecordEphemeralAgents upserts one row per agent, keyed by (execution,
	// agent id). The project, the conversation and the parent agent are
	// read from the execution kernel at the fence's generation, never from
	// the worker.
	RecordEphemeralAgents(ctx context.Context, fence runtimedomain.Fence, agents []EphemeralAgent) error
}

// WithEphemeralAgents serves ReportEphemeralAgents. Nil answers
// UNSUPPORTED_CAPABILITY, and the worker keeps the spawns in its own log.
func (s *Server) WithEphemeralAgents(agents EphemeralAgents) *Server {
	s.ephemeralAgents = agents
	return s
}

// ReportEphemeralAgents records the sub-agents one claimed execution started
// with spawn_agent. The peer is authenticated and the fence verified live,
// exactly as ReportEgressSummary does; a report for a released or stale
// claim is refused. A report is a cumulative snapshot, so a retried report
// upserts the same rows. Any malformed spawn refuses the whole report.
func (s *Server) ReportEphemeralAgents(ctx context.Context, request *runtimev1.ReportEphemeralAgentsRequestV1) (*runtimev1.ReportEphemeralAgentsResponseV1, error) {
	if request == nil || request.GetIdentity() == nil || request.GetFence() == nil || hasUnknownFields(request.ProtoReflect()) {
		return ephemeralAgentsRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The sub-agent report is malformed.", false), nil
	}
	agents, ok := ephemeralAgentsFromWire(request.GetAgents())
	if !ok {
		return ephemeralAgentsRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The sub-agent report is over its bounds or malformed.", false), nil
	}
	if s.ephemeralAgents == nil {
		return ephemeralAgentsRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_UNSUPPORTED_CAPABILITY, "Sub-agent reports are not served by this deployment.", false), nil
	}
	workloadIdentity, err := s.authorizer.AuthorizeWorkload(ctx, request.GetFence().GetWorkloadSessionId(), request.GetFence().GetProducerId())
	if err != nil || workloadIdentity == "" {
		refusalCode, refusalMessage, refusalRetryable := workloadRefusal(err)
		return ephemeralAgentsRejection(refusalCode, refusalMessage, refusalRetryable), nil
	}
	fence, err := fenceDomain(request.GetIdentity(), request.GetFence(), workloadIdentity)
	if err != nil {
		return ephemeralAgentsRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_PROTOCOL_VIOLATION, "The execution fence is malformed.", false), nil
	}
	if err := s.claims.VerifyActive(ctx, fence); err != nil {
		code, message, retryable := safeRuntimeError(err)
		return ephemeralAgentsRejection(code, message, retryable), nil
	}
	if len(agents) == 0 {
		return &runtimev1.ReportEphemeralAgentsResponseV1{}, nil
	}
	if err := s.ephemeralAgents.RecordEphemeralAgents(ctx, fence, agents); err != nil {
		slog.WarnContext(ctx, "sub-agent report not stored", "execution_id", fence.ExecutionID)
		return ephemeralAgentsRejection(runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_DEPENDENCY_UNAVAILABLE, "The sub-agent report could not be stored; retry.", true), nil
	}
	return &runtimev1.ReportEphemeralAgentsResponseV1{}, nil
}

// ephemeralAgentsFromWire checks every spawn against the table's own
// constraints, so a report the database would refuse is a protocol error,
// not a retry.
func ephemeralAgentsFromWire(wire []*runtimev1.EphemeralAgentV1) ([]EphemeralAgent, bool) {
	if len(wire) > MaxEphemeralAgents {
		return nil, false
	}
	agents := make([]EphemeralAgent, 0, len(wire))
	seen := make(map[string]struct{}, len(wire))
	for _, agent := range wire {
		if agent == nil || hasUnknownFields(agent.ProtoReflect()) {
			return nil, false
		}
		if _, dup := seen[agent.GetAgentId()]; dup || !ephemeralAgentID.MatchString(agent.GetAgentId()) {
			return nil, false
		}
		seen[agent.GetAgentId()] = struct{}{}
		if callID := agent.GetParentCallId(); callID == "" || len(callID) > maxEphemeralCallIDBytes || !plainText(callID) {
			return nil, false
		}
		if agent.GetOrdinal() < 1 || agent.GetOrdinal() > maxEphemeralOrdinal {
			return nil, false
		}
		if !boundedText(agent.GetLabel(), maxEphemeralLabelChars) || !boundedText(agent.GetDisplayName(), maxEphemeralLabelChars) ||
			len(agent.GetAvatar()) > maxEphemeralAvatarBytes || !plainText(agent.GetAvatar()) {
			return nil, false
		}
		if !ephemeralDigest.MatchString(agent.GetProfileDigest()) {
			return nil, false
		}
		refs, ok := briefRefs(agent.GetBriefDigest(), agent.GetBriefRefsJson())
		if !ok {
			return nil, false
		}
		if _, ok := ephemeralAgentStatus[agent.GetStatus()]; !ok {
			return nil, false
		}
		if code := agent.GetErrorCode(); code != "" && !ephemeralErrorCode.MatchString(code) {
			return nil, false
		}
		started, finished := agent.GetStartedAtUnixMillis(), agent.GetFinishedAtUnixMillis()
		if started <= 0 || finished < 0 || (finished != 0 && finished < started) ||
			agent.GetSteps() > math.MaxInt32 || agent.GetInputTokens() > 1<<62 || agent.GetOutputTokens() > 1<<62 {
			return nil, false
		}
		var finishedAt time.Time
		if finished != 0 {
			finishedAt = time.UnixMilli(finished).UTC()
		}
		agents = append(agents, EphemeralAgent{
			AgentID: agent.GetAgentId(), ParentCallID: agent.GetParentCallId(), Ordinal: int(agent.GetOrdinal()),
			Label: agent.GetLabel(), DisplayName: agent.GetDisplayName(), Avatar: agent.GetAvatar(),
			ProfileDigest: agent.GetProfileDigest(), BriefDigest: agent.GetBriefDigest(), BriefRefs: refs,
			Status: agent.GetStatus(), Steps: int64(agent.GetSteps()),
			InputTokens: int64(agent.GetInputTokens()), OutputTokens: int64(agent.GetOutputTokens()), //nolint:gosec // bounded above
			ErrorCode: agent.GetErrorCode(), StartedAt: time.UnixMilli(started).UTC(), FinishedAt: finishedAt,
		})
	}
	return agents, true
}

func boundedText(text string, maxChars int) bool {
	return plainText(text) && utf8.RuneCountInString(text) <= maxChars
}

// plainText answers whether text is valid UTF-8 without a control
// character (a NUL is refused by PostgreSQL text, the others by the record).
func plainText(text string) bool {
	return utf8.ValidString(text) && !strings.ContainsFunc(text, unicode.IsControl)
}

// briefRefs answers the brief's references as a JSON object, or nil for a
// spawn without a brief. A digest needs its references and the other way
// round.
func briefRefs(digest, raw string) (json.RawMessage, bool) {
	if digest == "" && raw == "" {
		return nil, true
	}
	if !ephemeralDigest.MatchString(digest) || raw == "" || len(raw) > maxEphemeralBriefRefBytes {
		return nil, false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &object); err != nil || object == nil {
		return nil, false
	}
	return json.RawMessage(raw), true
}

func ephemeralAgentsRejection(code runtimev1.RuntimeErrorCodeV1, message string, retryable bool) *runtimev1.ReportEphemeralAgentsResponseV1 {
	return &runtimev1.ReportEphemeralAgentsResponseV1{Rejection: runtimeError(code, message, retryable)}
}
