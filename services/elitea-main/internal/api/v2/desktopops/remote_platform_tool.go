package desktopops

// executeRemoteToolkitTool for a platform MCP server (issue 1231, ADR-0029
// decision 3): toolkit id 0 and the `toolkit_ref` resolveApplicationVersion
// gives a `platform_mcp` tool. Every check of a saved toolkit's call applies
// (live turn, tool_not_in_agent, tool_blocked, toolkit_ref_mismatch, the
// sensitive-tool confirmation, the rate limit, the audit event); what differs
// is the executor. A platform server is Main's own fixed internal builder, so
// the tool runs in process through the MCP package's dispatch, as the calling
// user: no worker, no command, no token minted.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/desktopwire"
)

const (
	// platformToolTimeout bounds one in-process platform call, the 60
	// seconds the operation documents for a worker run.
	platformToolTimeout = 60 * time.Second
	// platformReplayTTL and platformReplayCap bound the replay memory: a
	// retry with the same Idempotency-Key within the TTL answers the first
	// attempt's result and never runs the tool again.
	platformReplayTTL = 15 * time.Minute
	platformReplayCap = 4096
)

// ErrPlatformToolsUnavailable is a bridge nothing was bound to: the
// deployment mounts no MCP server.
var ErrPlatformToolsUnavailable = errors.New("platform MCP tools are not composed")

// PlatformToolCall is one tool of one platform MCP server, in one project.
type PlatformToolCall struct {
	ProjectID   int64
	ToolkitType string
	ToolName    string
	Arguments   json.RawMessage
}

// PlatformToolResult is what the server's tool answered (v2mcp's
// PlatformToolOutcome, restated so this package does not import the MCP
// server).
type PlatformToolResult struct {
	// Disabled: MCP exposure is off on this deployment; nothing ran.
	Disabled bool
	// Unknown: the server has no such tool; Available names its tools.
	Unknown   bool
	Available []string
	// IsError: the tool refused or failed (Text says why).
	IsError bool
	Text    string
}

// PlatformToolCaller runs a platform MCP tool as the caller authenticated on
// the request.
type PlatformToolCaller interface {
	CallPlatformTool(*http.Request, PlatformToolCall) (PlatformToolResult, error)
}

// PlatformToolBridge is the late-bound PlatformToolCaller: the remote toolkit
// route is composed before the router builds the MCP server, so the router
// binds it (api.RouterConfig.PlatformMCPTools). Unbound, every call answers
// ErrPlatformToolsUnavailable.
type PlatformToolBridge struct {
	caller atomic.Pointer[PlatformToolCaller]
}

func NewPlatformToolBridge() *PlatformToolBridge { return &PlatformToolBridge{} }

// Bind sets the caller; a nil caller is ignored.
func (b *PlatformToolBridge) Bind(caller PlatformToolCaller) {
	if b != nil && caller != nil {
		b.caller.Store(&caller)
	}
}

// Bound reports whether a caller was bound.
func (b *PlatformToolBridge) Bound() bool { return b != nil && b.caller.Load() != nil }

func (b *PlatformToolBridge) CallPlatformTool(request *http.Request, call PlatformToolCall) (PlatformToolResult, error) {
	if b == nil {
		return PlatformToolResult{}, ErrPlatformToolsUnavailable
	}
	caller := b.caller.Load()
	if caller == nil {
		return PlatformToolResult{}, ErrPlatformToolsUnavailable
	}
	return (*caller).CallPlatformTool(request, call)
}

// platformReplays remembers each platform call by (caller, Idempotency-Key),
// on this replica: a saved toolkit's run is idempotent on its admission row,
// and an in-process call has no such row, so a retry after a dropped answer
// would otherwise run a write twice.
type platformReplays struct {
	mu      sync.Mutex
	entries map[string]*platformReplay
	now     func() time.Time
}

type platformReplay struct {
	digest  string
	done    bool
	status  int
	body    map[string]any
	expires time.Time
}

func newPlatformReplays(now func() time.Time) *platformReplays {
	return &platformReplays{entries: map[string]*platformReplay{}, now: now}
}

type replayVerdict int

const (
	replayNew replayVerdict = iota
	replayInProgress
	replayDone
	replayConflict
)

// begin claims key for a call whose input digest is digest.
func (p *platformReplays) begin(key, digest string) (replayVerdict, *platformReplay) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for k, entry := range p.entries {
		if entry.done && now.After(entry.expires) {
			delete(p.entries, k)
		}
	}
	if entry, ok := p.entries[key]; ok {
		switch {
		case entry.digest != digest:
			return replayConflict, nil
		case !entry.done:
			return replayInProgress, nil
		default:
			return replayDone, entry
		}
	}
	if len(p.entries) >= platformReplayCap {
		var oldestKey string
		var oldest time.Time
		for k, entry := range p.entries {
			if entry.done && (oldestKey == "" || entry.expires.Before(oldest)) {
				oldestKey, oldest = k, entry.expires
			}
		}
		if oldestKey == "" {
			return replayInProgress, nil
		}
		delete(p.entries, oldestKey)
	}
	p.entries[key] = &platformReplay{digest: digest}
	return replayNew, nil
}

// finish records the answer of key; a call that did not reach the tool
// (forget) leaves no entry, so its retry runs.
func (p *platformReplays) finish(key string, status int, body map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry, ok := p.entries[key]; ok {
		entry.done, entry.status, entry.body = true, status, body
		entry.expires = p.now().Add(platformReplayTTL)
	}
}

func (p *platformReplays) forget(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.entries, key)
}

func platformCallDigest(toolkitType, toolName, argumentsSHA256 string) string {
	digest := sha256.Sum256([]byte(toolkitType + "\x00" + toolName + "\x00" + argumentsSHA256))
	return hex.EncodeToString(digest[:])
}

// runPlatformTool runs the authorized call once per Idempotency-Key and
// answers it. It is reached after every check serve makes; only a call it
// admits is charged to the caller's rate.
func (h *remoteToolkitHandler) runPlatformTool(
	writer http.ResponseWriter,
	request *http.Request,
	call *remoteCall,
	idempotencyKey string,
	arguments json.RawMessage,
	charge func() bool,
) {
	key := "actor:" + strconv.FormatInt(call.actor, 10) + ":" + idempotencyKey
	verdict, replay := h.replays.begin(key, platformCallDigest(call.toolkitType, call.toolName, call.argumentsSHA256))
	switch verdict {
	case replayConflict:
		call.outcome = "idempotency_conflict"
		desktopwire.WriteError(writer, http.StatusConflict, "idempotency_conflict",
			"This request key already belongs to a different tool input.")
		return
	case replayInProgress:
		call.outcome = "in_progress"
		desktopwire.WriteJSON(writer, http.StatusConflict, map[string]any{
			"ok": false, "error": "remote_toolkit_in_progress", "toolkit_id": 0,
			"message": "The call with this Idempotency-Key is still running. Retry later with the same key and body; " +
				"the retry answers its result once it finishes. It never runs the tool again.",
		})
		return
	case replayDone:
		call.outcome = "replayed"
		desktopwire.WriteJSON(writer, replay.status, replay.body)
		return
	}
	if !charge() {
		h.replays.forget(key)
		return
	}
	// Detached from the caller's cancellation (its values, the authenticated
	// user among them, are kept): a client that gave up and retries with the
	// same key gets this run's answer instead of a second run.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), platformToolTimeout)
	defer cancel()
	result, err := h.platform.CallPlatformTool(request.WithContext(ctx), PlatformToolCall{
		ProjectID: call.project, ToolkitType: call.toolkitType, ToolName: call.toolName, Arguments: arguments,
	})
	if err != nil {
		h.replays.forget(key)
		call.outcome = "run_error"
		if errors.Is(err, ErrPlatformToolsUnavailable) {
			desktopwire.WriteError(writer, http.StatusNotImplemented, "remote_toolkit_unavailable",
				"This deployment serves no platform MCP server.")
			return
		}
		writer.Header().Set("Retry-After", "1")
		desktopwire.WriteError(writer, http.StatusServiceUnavailable, "remote_toolkit_unavailable_now",
			"The platform MCP server could not be reached right now. Retry.")
		return
	}
	status, body := platformAnswer(call, result)
	call.outcome = platformOutcome(result)
	h.replays.finish(key, status, body)
	desktopwire.WriteJSON(writer, status, body)
}

func platformOutcome(result PlatformToolResult) string {
	switch {
	case result.Disabled:
		return "mcp_disabled"
	case result.Unknown:
		return "unknown_tool"
	case result.IsError:
		return "tool_error"
	default:
		return "ok"
	}
}

// platformAnswer is the RemoteToolkitCallResult of a platform call, in the
// shapes a saved toolkit's run answers.
func platformAnswer(call *remoteCall, result PlatformToolResult) (int, map[string]any) {
	body := map[string]any{
		"ok": false, "toolkit_id": 0, "toolkit_type": call.toolkitType, "tool_name": call.toolName,
	}
	switch {
	case result.Disabled:
		body["error"] = "remote_tool_unsupported"
		body["reason"] = "unsupported_toolkit"
		body["message"] = "MCP exposure is disabled on this deployment, so its platform MCP servers cannot be called."
		return http.StatusUnprocessableEntity, body
	case result.Unknown:
		body["error"] = "remote_tool_unsupported"
		body["reason"] = "unknown_tool"
		message := "This platform MCP server has no tool named " + strconv.Quote(call.toolName) + "."
		if len(result.Available) > 0 {
			message += " Its tools: " + strings.Join(result.Available, ", ") + "."
		}
		body["message"] = message
		return http.StatusUnprocessableEntity, body
	case result.IsError:
		body["status"] = "tool_error"
		body["error"] = result.Text
		return http.StatusOK, body
	}
	body["ok"] = true
	body["status"] = "ok"
	if raw := json.RawMessage(strings.TrimSpace(result.Text)); len(raw) > 0 && json.Valid(raw) {
		body["result"] = raw
	} else {
		body["result"] = result.Text
	}
	return http.StatusOK, body
}
