package main

import (
	"context"
	"errors"
	"testing"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
)

type recordingExpander struct {
	requests []configurationapp.CurrentExpansionRequest
	answer   map[string]any
	err      error
}

func (e *recordingExpander) Expand(_ context.Context, request configurationapp.CurrentExpansionRequest) (map[string]any, error) {
	e.requests = append(e.requests, request)
	return e.answer, e.err
}

func TestForgeCredentialResolverExpandsAGitHubReferenceAsAToolkitDoes(t *testing.T) {
	expander := &recordingExpander{answer: map[string]any{"github_configuration": map[string]any{
		"base_url": "https://api.github.com", "access_token": "ghp_x", "elitea_title": "gh", "private": true,
		"configuration_type": "github", "configuration_uuid": "u", "configuration_project_id": int32(9),
	}}}
	resolver := newForgeCredentialResolver(expander)
	got, err := resolver.ResolveForgeCredential(context.Background(), 1, 7,
		workspacehandoff.ForgeCredential{EliteaTitle: "gh", Private: true}, true)
	if err != nil || got["access_token"] != "ghp_x" || got["base_url"] != "https://api.github.com" {
		t.Fatalf("credential = %v %v", got, err)
	}
	for _, key := range forgeCredentialMetadata {
		if _, present := got[key]; present {
			t.Fatalf("the expansion's own key %q reached the worker", key)
		}
	}
	request := expander.requests[0]
	reference, _ := request.Payload["github_configuration"].(map[string]any)
	if request.CurrentProjectID != 1 || request.UserID == nil || *request.UserID != 7 || !request.Unsecret ||
		reference["elitea_title"] != "gh" || reference["private"] != true {
		t.Fatalf("request = %+v", request)
	}
}

func TestForgeCredentialResolverRefusesWhatIsNotAGitHubConfiguration(t *testing.T) {
	for name, expander := range map[string]*recordingExpander{
		"another type": {answer: map[string]any{"github_configuration": map[string]any{"configuration_type": "gitlab"}}},
		"not found":    {err: &configurationapp.CurrentExpansionError{Code: configurationapp.CurrentExpansionNotFoundCode}},
		"forbidden":    {err: &configurationapp.CurrentExpansionError{Code: configurationapp.CurrentExpansionForbiddenCode}},
	} {
		_, err := newForgeCredentialResolver(expander).ResolveForgeCredential(context.Background(), 1, 7,
			workspacehandoff.ForgeCredential{EliteaTitle: "gh"}, false)
		if !errors.Is(err, workspacehandoff.ErrForgeCredentialInvalid) {
			t.Fatalf("%s = %v", name, err)
		}
	}
	// The vault or the database: retryable, not "invalid".
	dependency := &recordingExpander{err: &configurationapp.CurrentExpansionError{Code: configurationapp.CurrentExpansionDependencyCode}}
	if _, err := newForgeCredentialResolver(dependency).ResolveForgeCredential(context.Background(), 1, 7,
		workspacehandoff.ForgeCredential{EliteaTitle: "gh"}, true); !errors.Is(err, workspacehandoff.ErrUnavailable) {
		t.Fatalf("dependency = %v", err)
	}
	if newForgeCredentialResolver(nil) != nil {
		t.Fatal("a nil expander composes a resolver")
	}
}
