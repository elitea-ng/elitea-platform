package workspace_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

// forgeStore is a fakeStore whose thread has a pull-request credential.
type forgeStore struct {
	*fakeStore
	credential map[string]any
	err        error
}

func (f *forgeStore) ForgeCredential(_ context.Context, claim workspaceruntime.ClaimRef) (workspaceruntime.ForgeCredential, error) {
	if err := f.check(claim); err != nil {
		return workspaceruntime.ForgeCredential{}, err
	}
	return workspaceruntime.ForgeCredential{Forge: "github", Configuration: f.credential}, f.err
}

func TestForgeCredentialIsAClaimBoundSecret(t *testing.T) {
	store := &forgeStore{fakeStore: newStore(), credential: map[string]any{
		"base_url": "https://api.github.com", "access_token": "ghp_secret",
	}}
	client := dial(t, store, verifier{})
	response, err := client.WorkspaceForgeCredential(context.Background(), &runtimev1.WorkspaceForgeCredentialRequestV1{Claim: claim(2)})
	if err != nil || response.GetRefusal() != nil || response.GetRejection() != nil {
		t.Fatalf("answer = %v %v", response, err)
	}
	var credential map[string]any
	if err := json.Unmarshal([]byte(response.GetConfigurationJson()), &credential); err != nil ||
		credential["access_token"] != "ghp_secret" || response.GetForge() != "github" {
		t.Fatalf("credential = %q", response.GetConfigurationJson())
	}
	// A stale epoch gets nothing.
	response, _ = client.WorkspaceForgeCredential(context.Background(), &runtimev1.WorkspaceForgeCredentialRequestV1{Claim: claim(1)})
	if response.GetRefusal().GetCode() != runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_STALE_EPOCH ||
		response.GetConfigurationJson() != "" {
		t.Fatalf("stale = %v", response)
	}
	for code, err := range map[runtimev1.WorkspaceRefusalCodeV1]error{
		runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_NO_FORGE_CREDENTIAL:    workspaceruntime.Refuse(workspaceruntime.RefusalNoForgeCredential),
		runtimev1.WorkspaceRefusalCodeV1_WORKSPACE_REFUSAL_CODE_V1_PULL_REQUESTS_DISABLED: workspaceruntime.Refuse(workspaceruntime.RefusalPullRequestsDisabled),
	} {
		store.err = err
		response, _ = client.WorkspaceForgeCredential(context.Background(), &runtimev1.WorkspaceForgeCredentialRequestV1{Claim: claim(2)})
		if response.GetRefusal().GetCode() != code {
			t.Fatalf("%v = %v", code, response)
		}
	}
	// Over the bound: refused, not sent.
	store.err, store.credential = nil, map[string]any{"app_private_key": strings.Repeat("k", workspaceruntime.MaxForgeCredentialBytes)}
	response, _ = client.WorkspaceForgeCredential(context.Background(), &runtimev1.WorkspaceForgeCredentialRequestV1{Claim: claim(2)})
	if response.GetRejection() == nil || response.GetConfigurationJson() != "" {
		t.Fatalf("oversized = %v", response.GetRejection())
	}
}

func TestForgeCredentialRefusesAStaleFenceAndAStoreWithoutIt(t *testing.T) {
	store := &forgeStore{fakeStore: newStore(), credential: map[string]any{"access_token": "ghp_secret"}}
	client := dial(t, store, verifier{err: runtimedomain.ErrStaleFence})
	response, err := client.WorkspaceForgeCredential(context.Background(), &runtimev1.WorkspaceForgeCredentialRequestV1{Claim: claim(2)})
	if err != nil || response.GetRejection().GetCode() != runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_STALE_FENCE ||
		len(store.claims) != 0 {
		t.Fatalf("stale fence = %v %v", response, err)
	}
	// A Store without the call (an older composition) is unavailable.
	plain := dial(t, newStore(), verifier{})
	response, _ = plain.WorkspaceForgeCredential(context.Background(), &runtimev1.WorkspaceForgeCredentialRequestV1{Claim: claim(2)})
	if response.GetRejection() == nil || response.GetConfigurationJson() != "" {
		t.Fatalf("plain = %v", response)
	}
}
