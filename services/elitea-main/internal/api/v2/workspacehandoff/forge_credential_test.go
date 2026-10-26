package workspacehandoff

import (
	"context"
	"net/http"
	"testing"

	handoffapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// fakeForgeHandoff is a fakeHandoff whose deployment can store a thread's
// pull-request credential.
type fakeForgeHandoff struct {
	fakeHandoff
	set       []handoffapp.SetForgeCredentialRequest
	setErr    error
	available bool
}

func (f *fakeForgeHandoff) SetForgeCredential(_ context.Context, request handoffapp.SetForgeCredentialRequest) (handoffapp.Handoff, error) {
	f.set = append(f.set, request)
	if f.setErr != nil {
		return handoffapp.Handoff{}, f.setErr
	}
	return handoffapp.Handoff{ForgeCredential: request.Credential}, nil
}

func (f *fakeForgeHandoff) ForgeCredentialsAvailable() bool { return f.available }

const pullRequestCredentialURL = "/api/v2/elitea_core/workspace_handoff_pull_request_credential/prompt_lib/1/" + conversation

func TestPullRequestCredentialIsSetAndClearedByTheOwnerFromAnyClient(t *testing.T) {
	handoff := &fakeForgeHandoff{available: true}
	h := &handler{handoff: handoff, store: &fakeStore{}, conversations: fakeConversations{}}
	browser := &auth.User{ID: "7", UserID: "7", AuthType: "session"}
	response := serve(t, h.setPullRequestCredential, browser, []string{SendPermission}, http.MethodPut,
		PullRequestCredentialPath, pullRequestCredentialURL, `{"credential":{"type":"gitlab","elitea_title":"gh-team","private":true}}`)
	answer := decode(t, response)
	credential, _ := answer["pull_request_credential"].(map[string]any)
	if response.Code != http.StatusOK || credential["elitea_title"] != "gh-team" || credential["private"] != true ||
		credential["type"] != "gitlab" {
		t.Fatalf("set = %d %v", response.Code, answer)
	}
	if len(handoff.set) != 1 || handoff.set[0].ActorUserID != 7 || handoff.set[0].Credential == nil ||
		handoff.set[0].Thread.ConversationUUID != conversation || handoff.set[0].AuditRoute != PullRequestCredentialPath {
		t.Fatalf("request = %+v", handoff.set)
	}
	response = serve(t, h.setPullRequestCredential, nativeUser(), []string{SendPermission}, http.MethodPut,
		PullRequestCredentialPath, pullRequestCredentialURL, `{"credential":null}`)
	if answer := decode(t, response); response.Code != http.StatusOK || answer["pull_request_credential"] != nil ||
		handoff.set[1].Credential != nil {
		t.Fatalf("clear = %d %v", response.Code, answer)
	}
}

func TestPullRequestCredentialRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		available bool
		body      string
		err       error
		status    int
		code      string
	}{
		"no credential key":   {available: true, body: `{}`, status: http.StatusUnprocessableEntity, code: "invalid_handoff"},
		"an extra key":        {available: true, body: `{"credential":null,"token":"x"}`, status: http.StatusUnprocessableEntity, code: "invalid_handoff"},
		"not an object":       {available: true, body: `{"credential":"gh"}`, status: http.StatusUnprocessableEntity, code: "invalid_handoff"},
		"not composed":        {available: false, body: `{"credential":null}`, status: http.StatusServiceUnavailable, code: "handoff_unavailable"},
		"policy off":          {available: true, body: `{"credential":{"elitea_title":"gh"}}`, err: handoffapp.ErrPullRequestsDisabled, status: http.StatusForbidden, code: "pull_requests_disabled"},
		"not a github config": {available: true, body: `{"credential":{"elitea_title":"gh"}}`, err: handoffapp.ErrForgeCredentialInvalid, status: http.StatusUnprocessableEntity, code: "pull_request_credential_invalid"},
		"someone else":        {available: true, body: `{"credential":null}`, err: handoffapp.ErrNotFound, status: http.StatusNotFound, code: "handoff_not_found"},
		"back on the desktop": {available: true, body: `{"credential":null}`, err: handoffapp.ErrNotInCloud, status: http.StatusConflict, code: "handoff_not_in_cloud"},
	} {
		t.Run(name, func(t *testing.T) {
			h := &handler{handoff: &fakeForgeHandoff{available: tc.available, setErr: tc.err}, store: &fakeStore{}, conversations: fakeConversations{}}
			response := serve(t, h.setPullRequestCredential, nativeUser(), []string{SendPermission}, http.MethodPut,
				PullRequestCredentialPath, pullRequestCredentialURL, tc.body)
			if answer := decode(t, response); response.Code != tc.status || answer["error"] != tc.code {
				t.Fatalf("= %d %v", response.Code, answer)
			}
		})
	}
}

func TestPolicyAndStateCarryThePullRequestFields(t *testing.T) {
	handoff := &fakeForgeHandoff{available: true}
	handoff.policy = handoffapp.Policy{Enabled: true, PullRequests: true, PullRequestApproval: true}
	handoff.view = handoffapp.View{Present: true, Handoff: handoffapp.Handoff{
		OwnerUserID: 7, ForgeCredential: &handoffapp.ForgeCredential{Type: "github", EliteaTitle: "gh-team"},
	}}
	h := &handler{handoff: handoff, store: &fakeStore{}, conversations: fakeConversations{}}
	response := serve(t, h.policy, nativeUser(), []string{SendPermission}, http.MethodGet, PolicyPath,
		"/api/v2/elitea_core/workspace_handoff_policy/prompt_lib/1", "")
	if answer := decode(t, response); answer["pull_requests"] != true || answer["pull_request_approval"] != true ||
		answer["pull_request_credentials"] != true {
		t.Fatalf("policy = %v", answer)
	}
	response = serve(t, h.state, nativeUser(), []string{ReadPermission}, http.MethodGet, HandoffPath, handoffURL, "")
	answer := decode(t, response)
	if credential, _ := answer["pull_request_credential"].(map[string]any); credential["elitea_title"] != "gh-team" ||
		credential["private"] != false {
		t.Fatalf("state = %v", answer)
	}
	// A deployment without the Configurations runtime cannot store one.
	plain := &handler{handoff: &fakeHandoff{}, store: &fakeStore{}, conversations: fakeConversations{}}
	response = serve(t, plain.policy, nativeUser(), []string{SendPermission}, http.MethodGet, PolicyPath,
		"/api/v2/elitea_core/workspace_handoff_policy/prompt_lib/1", "")
	if answer := decode(t, response); answer["pull_request_credentials"] != false || answer["pull_requests"] != false {
		t.Fatalf("plain policy = %v", answer)
	}
}
