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
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/desktopwire"
)

const (
	// platformToolTimeout bounds one in-process platform call, the 60
	// seconds the operation documents for a worker run.
	platformToolTimeout = 60 * time.Second
	// platformReplayWait is how long a retry that finds its key still in
	// progress (on any replica) waits for that attempt's answer before
	// answering 409 remote_toolkit_in_progress; platformReplayPoll is how
	// often it looks.
	platformReplayWait = 30 * time.Second
	platformReplayPoll = 250 * time.Millisecond
	// platformReplayStoreTimeout bounds one replay store call.
	platformReplayStoreTimeout = 5 * time.Second
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

func platformCallDigest(toolkitType, toolName, argumentsSHA256 string) string {
	digest := sha256.Sum256([]byte(toolkitType + "\x00" + toolName + "\x00" + argumentsSHA256))
	return hex.EncodeToString(digest[:])
}

// runPlatformTool runs the authorized call once per Idempotency-Key, on any
// replica, and answers it. It is reached after every check serve makes; only
// a call it admits is charged to the caller's rate.
func (h *remoteToolkitHandler) runPlatformTool(
	writer http.ResponseWriter,
	request *http.Request,
	call *remoteCall,
	idempotencyKey string,
	arguments json.RawMessage,
	charge func() bool,
) {
	key := platformReplayKey(call.actor, call.project, idempotencyKey)
	digest := platformCallDigest(call.toolkitType, call.toolName, call.argumentsSHA256)
	// The store's own calls are bounded and detached from the caller's
	// cancellation, like the tool run below: a claim must not be left half
	// written because the client hung up.
	storeCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(request.Context()), platformReplayStoreTimeout)
	}
	claim, err := h.claimPlatformCall(request.Context(), key, digest, storeCtx)
	if err != nil {
		h.writeReplayUnavailable(request.Context(), writer, call, err)
		return
	}
	switch claim.Verdict {
	case ReplayConflict:
		call.outcome = "idempotency_conflict"
		desktopwire.WriteError(writer, http.StatusConflict, "idempotency_conflict",
			"This request key already belongs to a different tool input.")
		return
	case ReplayInProgress:
		call.outcome = "in_progress"
		desktopwire.WriteJSON(writer, http.StatusConflict, map[string]any{
			"ok": false, "error": "remote_toolkit_in_progress", "toolkit_id": 0,
			"message": "The call with this Idempotency-Key is still running. Retry later with the same key and body; " +
				"the retry answers its result once it finishes. It never runs the tool again.",
		})
		return
	case ReplayDone:
		call.outcome = "replayed"
		writePlatformReplay(writer, call, claim.Result)
		return
	}
	release := func() {
		ctx, cancel := storeCtx()
		defer cancel()
		if err := h.replays.Release(ctx, key, claim); err != nil {
			// The key stays "in progress" until the bucket expires it: a
			// retry is answered 409 and never runs the tool, which is the
			// safe side of a lost release.
			slog.WarnContext(request.Context(), "platform MCP replay: release failed", "err", err)
		}
	}
	if !charge() {
		release()
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
		release()
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
	encoded, err := json.Marshal(body)
	if err != nil {
		encoded = nil
	}
	recordCtx, recordCancel := storeCtx()
	defer recordCancel()
	if err := h.replays.Complete(recordCtx, key, claim, PlatformReplayResult{
		Status: status, Body: encoded, Omitted: encoded == nil || len(encoded) > platformReplayMaxBody, OK: body["ok"] == true,
	}); err != nil {
		// The tool ran; its answer goes to this caller. The key stays "in
		// progress" until it expires, so a retry is refused and never runs
		// the tool again.
		slog.WarnContext(request.Context(), "platform MCP replay: recording the answer failed", "err", err)
	}
	if encoded == nil {
		desktopwire.WriteJSON(writer, status, body)
		return
	}
	desktopwire.WriteJSON(writer, status, json.RawMessage(encoded))
}

// claimPlatformCall claims key, and while another attempt holds it waits up
// to h.replayWait for that attempt's answer (or for it to release the key,
// which this call then claims).
func (h *remoteToolkitHandler) claimPlatformCall(
	requestCtx context.Context, key, digest string, storeCtx func() (context.Context, context.CancelFunc),
) (PlatformReplay, error) {
	deadline := h.now().Add(h.replayWait)
	for {
		ctx, cancel := storeCtx()
		claim, err := h.replays.Claim(ctx, key, digest)
		cancel()
		if err != nil || claim.Verdict != ReplayInProgress || !h.now().Before(deadline) {
			return claim, err
		}
		select {
		case <-requestCtx.Done():
			// The caller left; it retries with the same key.
			return claim, nil
		case <-time.After(h.replayPoll):
		}
	}
}

// writeReplayUnavailable answers a call the replay store could not decide:
// fail closed. Running without the claim could run a retried write twice.
func (h *remoteToolkitHandler) writeReplayUnavailable(ctx context.Context, writer http.ResponseWriter, call *remoteCall, err error) {
	slog.WarnContext(ctx, "platform MCP replay store unavailable; refusing the call", "err", err)
	call.outcome = "replay_store_unavailable"
	writer.Header().Set("Retry-After", "1")
	desktopwire.WriteError(writer, http.StatusServiceUnavailable, "remote_toolkit_unavailable_now",
		"The server could not record this call's Idempotency-Key right now, so it did not run it. Retry with the same key and body.")
}

// writePlatformReplay answers a retry with the first attempt's answer. One too
// large to keep answers that the call ran, without running it again.
func writePlatformReplay(writer http.ResponseWriter, call *remoteCall, result PlatformReplayResult) {
	if !result.Omitted && len(result.Body) > 0 {
		desktopwire.WriteJSON(writer, result.Status, result.Body)
		return
	}
	outcome := "failed"
	if result.OK {
		outcome = "succeeded"
	}
	desktopwire.WriteJSON(writer, http.StatusOK, map[string]any{
		"ok": false, "toolkit_id": 0, "toolkit_type": call.toolkitType, "tool_name": call.toolName,
		"status": "tool_error", "result_omitted": true, "replayed_status": result.Status,
		"error": "This call already ran with this Idempotency-Key (it " + outcome + ", HTTP " + strconv.Itoa(result.Status) +
			"), and its result was too large to keep for a retry. It was not run again.",
	})
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
