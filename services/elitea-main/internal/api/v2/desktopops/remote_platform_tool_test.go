package desktopops

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/guardrails"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

const platformURL = "/api/v2/elitea_core/remote_toolkit_call/prompt_lib/3/0"

type fakePlatform struct {
	mu     sync.Mutex
	result PlatformToolResult
	err    error
	calls  []PlatformToolCall
	users  []string
}

func (f *fakePlatform) CallPlatformTool(r *http.Request, call PlatformToolCall) (PlatformToolResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	if user, ok := auth.UserFromContext(r.Context()); ok {
		f.users = append(f.users, user.UserID)
	}
	return f.result, f.err
}

func platformHandler(platform PlatformToolCaller) (*remoteToolkitHandler, *fakeAuthorizer, *fakeToolRuns, *recordedAudit) {
	runs := &fakeToolRuns{}
	h, _, authorizer, recorder := remoteHandler(runs, "rust")
	h.platform = platform
	authorizer.grant = storage.RemoteToolGrant{
		ToolkitType: "mcp_elitea_internal_applications", ToolkitName: "Elitea Applications", PlatformMCP: true,
	}
	return h, authorizer, runs, recorder
}

// TestRemoteToolkitCallPlatformMCP: a platform MCP server's tool (toolkit id
// 0) runs in process as the caller, behind every check a saved toolkit's call
// has (issue 1231).
func TestRemoteToolkitCallPlatformMCP(t *testing.T) {
	listing := remoteBody(`"tool_name":"get_elitea_core_applications","arguments":{"limit":5}`)
	for _, tc := range []struct {
		name       string
		authorize  func(*fakeAuthorizer)
		result     PlatformToolResult
		wantStatus int
		wantError  string
		wantCalls  int
		check      func(t *testing.T, body map[string]any)
	}{
		{
			name: "allowed builder", result: PlatformToolResult{Text: `{"total":1,"rows":[{"id":"4"}]}`},
			wantStatus: http.StatusOK, wantCalls: 1,
			check: func(t *testing.T, body map[string]any) {
				if body["ok"] != true || body["status"] != "ok" || body["toolkit_type"] != "mcp_elitea_internal_applications" ||
					body["result"].(map[string]any)["total"] != float64(1) {
					t.Fatalf("body = %v", body)
				}
			},
		},
		{
			name:       "builder not selected",
			authorize:  func(a *fakeAuthorizer) { a.err = storage.ErrRemoteToolNotInAgent },
			wantStatus: http.StatusForbidden, wantError: "tool_not_in_agent",
		},
		{
			name:       "ref of another version",
			authorize:  func(a *fakeAuthorizer) { a.err = storage.ErrRemoteToolkitRefMismatch },
			wantStatus: http.StatusForbidden, wantError: "toolkit_ref_mismatch",
		},
		{
			name:       "blocked builder",
			authorize:  func(a *fakeAuthorizer) { a.err = storage.ErrRemoteToolBlocked },
			wantStatus: http.StatusForbidden, wantError: "tool_blocked",
		},
		{
			name:       "a saved toolkit's grant under toolkit id 0",
			authorize:  func(a *fakeAuthorizer) { a.grant = storage.RemoteToolGrant{ToolkitType: "github"} },
			wantStatus: http.StatusForbidden, wantError: "toolkit_ref_mismatch",
		},
		{
			// A viewer holds tool.execute but not the operation's own
			// permission: the server's tool refuses, as on the MCP endpoint.
			name: "viewer refused by the operation's permission",
			result: PlatformToolResult{IsError: true, Text: "using internal MCP tool 'post_elitea_core_applications' in this project " +
				"requires the 'models.applications.applications.create' permission, which this caller does not hold. Nothing was executed."},
			wantStatus: http.StatusOK, wantCalls: 1,
			check: func(t *testing.T, body map[string]any) {
				if body["ok"] != false || body["status"] != "tool_error" || !strings.Contains(body["error"].(string), "does not hold") {
					t.Fatalf("body = %v", body)
				}
			},
		},
		{
			name: "editor runs the write", result: PlatformToolResult{Text: `{"id":9}`},
			wantStatus: http.StatusOK, wantCalls: 1,
		},
		{
			name: "unknown tool names the server's tools", result: PlatformToolResult{Unknown: true, Available: []string{"a_tool", "b_tool"}},
			wantStatus: http.StatusUnprocessableEntity, wantError: "remote_tool_unsupported", wantCalls: 1,
			check: func(t *testing.T, body map[string]any) {
				if body["reason"] != "unknown_tool" || !strings.Contains(body["message"].(string), "a_tool, b_tool") {
					t.Fatalf("body = %v", body)
				}
			},
		},
		{
			name: "mcp disabled", result: PlatformToolResult{Disabled: true, IsError: true},
			wantStatus: http.StatusUnprocessableEntity, wantError: "remote_tool_unsupported", wantCalls: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			platform := &fakePlatform{result: tc.result}
			h, authorizer, runs, recorder := platformHandler(platform)
			if tc.authorize != nil {
				tc.authorize(authorizer)
			}
			response := serveWith(t, http.MethodPost, RemoteToolkitPath, platformURL, listing, desktopToken(), h.serve)
			if response.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", response.Code, tc.wantStatus, response.Body)
			}
			var body map[string]any
			_ = json.Unmarshal(response.Body.Bytes(), &body)
			if tc.wantError != "" && body["error"] != tc.wantError {
				t.Fatalf("error = %v, want %s", body["error"], tc.wantError)
			}
			if len(platform.calls) != tc.wantCalls || runs.calls != 0 {
				t.Fatalf("platform calls %d (want %d), worker runs %d", len(platform.calls), tc.wantCalls, runs.calls)
			}
			if authorizer.request.ToolkitID != 0 || authorizer.request.ToolkitRef != remoteRef {
				t.Fatalf("authorization = %+v", authorizer.request)
			}
			if tc.wantCalls == 1 {
				call := platform.calls[0]
				if call.ProjectID != 3 || call.ToolkitType != "mcp_elitea_internal_applications" ||
					call.ToolName != "get_elitea_core_applications" || string(call.Arguments) != `{"limit":5}` ||
					platform.users[0] != "7" {
					t.Fatalf("platform call = %+v as %v", call, platform.users)
				}
			}
			if len(recorder.events) != 1 || recorder.events[0].EntityType != remoteToolkitAuditEntity {
				t.Fatalf("audit = %+v", recorder.events)
			}
			if tc.check != nil {
				tc.check(t, body)
			}
		})
	}
}

func TestRemoteToolkitPlatformMCPKeepsEveryGate(t *testing.T) {
	t.Run("no platform caller answers 501", func(t *testing.T) {
		h, _, _, _ := platformHandler(nil)
		h.platform = nil
		response := serveWith(t, http.MethodPost, RemoteToolkitPath, platformURL, remoteBody(`"tool_name":"x"`), desktopToken(), h.serve)
		if response.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d", response.Code)
		}
	})
	t.Run("an unbound bridge answers 501", func(t *testing.T) {
		h, _, _, _ := platformHandler(NewPlatformToolBridge())
		response := serveWith(t, http.MethodPost, RemoteToolkitPath, platformURL, remoteBody(`"tool_name":"x"`), desktopToken(), h.serve)
		if response.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, body %s", response.Code, response.Body)
		}
	})
	t.Run("a platform server has no MCP authorisation", func(t *testing.T) {
		platform := &fakePlatform{}
		h, _, _, _ := platformHandler(platform)
		response := serveWith(t, http.MethodPost, RemoteToolkitPath, platformURL,
			remoteBody(`"tool_name":"x","mcp_authorization_reference":"mcpref_0123456789abcdef0123456789abcdef"`), desktopToken(), h.serve)
		if response.Code != http.StatusBadRequest || len(platform.calls) != 0 {
			t.Fatalf("status = %d, calls %d", response.Code, len(platform.calls))
		}
	})
	t.Run("a sensitive tool needs a confirmation", func(t *testing.T) {
		platform := &fakePlatform{result: PlatformToolResult{Text: `{}`}}
		h, authorizer, _, _ := platformHandler(platform)
		authorizer.grant.Sensitive = &guardrails.SensitiveAction{ActionLabel: "delete", PolicyMessage: "Approve first."}
		response := serveWith(t, http.MethodPost, RemoteToolkitPath, platformURL, remoteBody(`"tool_name":"delete_application"`), desktopToken(), h.serve)
		if response.Code != http.StatusConflict || len(platform.calls) != 0 ||
			confirmationInterrupt(t, response)["toolkit_type"] != "mcp_elitea_internal_applications" {
			t.Fatalf("status = %d, calls %d, body %s", response.Code, len(platform.calls), response.Body)
		}
	})
	t.Run("the rate limit charges admitted calls only", func(t *testing.T) {
		platform := &fakePlatform{result: PlatformToolResult{Text: `{}`}}
		h, _, _, _ := platformHandler(platform)
		for i := 0; i < RemoteToolkitCallsPerMinute; i++ {
			h.limiter.Fail("actor:7")
		}
		response := serveWith(t, http.MethodPost, RemoteToolkitPath, platformURL, remoteBody(`"tool_name":"x"`), desktopToken(), h.serve)
		if response.Code != http.StatusTooManyRequests || len(platform.calls) != 0 {
			t.Fatalf("status = %d, calls %d", response.Code, len(platform.calls))
		}
	})
}

// A retry with the same Idempotency-Key answers the first run's result and
// never runs the tool again; the same key with other arguments is a conflict.
func TestRemoteToolkitPlatformMCPNeverRunsAReplayTwice(t *testing.T) {
	platform := &fakePlatform{result: PlatformToolResult{Text: `{"id":9}`}}
	h, _, _, _ := platformHandler(platform)
	body := remoteBody(`"tool_name":"post_elitea_core_applications","arguments":{"name":"a"}`)
	first := serveWith(t, http.MethodPost, RemoteToolkitPath, platformURL, body, desktopToken(), h.serve)
	second := serveWith(t, http.MethodPost, RemoteToolkitPath, platformURL, body, desktopToken(), h.serve)
	if first.Code != http.StatusOK || second.Code != http.StatusOK || first.Body.String() != second.Body.String() || len(platform.calls) != 1 {
		t.Fatalf("first %d %s, second %d %s, calls %d", first.Code, first.Body, second.Code, second.Body, len(platform.calls))
	}
	other := serveWith(t, http.MethodPost, RemoteToolkitPath, platformURL,
		remoteBody(`"tool_name":"post_elitea_core_applications","arguments":{"name":"b"}`), desktopToken(), h.serve)
	if other.Code != http.StatusConflict || !strings.Contains(other.Body.String(), "idempotency_conflict") || len(platform.calls) != 1 {
		t.Fatalf("other body: %d %s", other.Code, other.Body)
	}
	// A call that never reached the tool leaves nothing to replay.
	failing := &fakePlatform{err: ErrPlatformToolsUnavailable}
	h, _, _, _ = platformHandler(failing)
	_ = serveWith(t, http.MethodPost, RemoteToolkitPath, platformURL, body, desktopToken(), h.serve)
	failing.err = nil
	failing.result = PlatformToolResult{Text: `{}`}
	again := serveWith(t, http.MethodPost, RemoteToolkitPath, platformURL, body, desktopToken(), h.serve)
	if again.Code != http.StatusOK || len(failing.calls) != 2 {
		t.Fatalf("after a failure: %d, calls %d", again.Code, len(failing.calls))
	}
}
