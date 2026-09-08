package configurations

// The DECISION half of the stored TOOLKIT check (toolkit_check.go's stored
// path, stored_check.go's `checkStoredToolkitRow`), with no database and no
// provider in the way.
//
// It is the twin of stored_check_test.go for the other credential family, and
// it asserts the same discriminating property that file names: what the CHECKER
// RECEIVED. A github row holds a `{{secret.NAME}}` reference where the token
// used to be, so a handler that passed the stored data straight through would
// ask GitHub to authenticate a template string — and would report every saved
// toolkit credential in the platform as refused, including the ones that work.

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// recordingToolkitChecker records what it was handed and answers a canned
// outcome. The assertions read the recording, for the reason
// stored_check_test.go's header gives: "the check reported X" and "the check
// happened" are different facts.
type recordingToolkitChecker struct {
	types   []string
	data    []map[string]any
	outcome ToolkitCheckOutcome
}

func (c *recordingToolkitChecker) CheckToolkit(_ context.Context, configType string, data map[string]any) ToolkitCheckOutcome {
	c.types = append(c.types, configType)
	c.data = append(c.data, data)
	return c.outcome
}

// sealedToolkitCredentialRow is a SAVED github credential as the sealing change
// stores one: the token column holds a reference, not the token.
func sealedToolkitCredentialRow() storedConfigurationRow {
	author := 4964
	return storedConfigurationRow{
		id:         12,
		uuid:       "22222222-2222-4222-8222-222222222222",
		configType: "github",
		data: map[string]any{
			"base_url":     "https://api.github.com",
			"access_token": "{{secret.7f3c9a2b4d5e6f708192a3b4c5d6e7f8}}",
		},
		authorID: &author,
	}
}

func TestAStoredToolkitCredentialIsCheckedWithTheRedeemedSecret(t *testing.T) {
	resolver := &recordingStoredResolver{resolved: map[string]any{
		"base_url":     "https://api.github.com",
		"access_token": "ghp-redeemed-from-the-vault",
	}}
	checker := &recordingToolkitChecker{outcome: ToolkitCheckOutcome{Reason: ToolkitCheckReasonOK, Message: "Connection successful"}}
	handler := storedCheckHandler(
		WithStoredConfigurationResolver(resolver),
		WithToolkitConnectionChecker(checker),
	)

	result, status := handler.checkStoredRow(context.Background(), "7", sealedToolkitCredentialRow())

	if status != http.StatusOK || !result.Success {
		t.Fatalf("status = %d success = %v, want %d and true", status, result.Success, http.StatusOK)
	}
	if result.Reason != ToolkitCheckReasonOK {
		t.Fatalf("the route must publish the probe's reason, got %q", result.Reason)
	}
	if len(checker.data) != 1 {
		t.Fatalf("the checker was called %d times, want 1", len(checker.data))
	}
	if got := checker.data[0]["access_token"]; got != "ghp-redeemed-from-the-vault" {
		t.Fatalf("the checker received access_token %v, want the redeemed value.\n"+
			"  The stored row holds a {{secret.NAME}} reference; passing it through asks the "+
			"provider to authenticate a template string, and a working credential reads as refused.", got)
	}
	if checker.types[0] != "github" {
		t.Fatalf("the checker was called for type %q, want the STORED type", checker.types[0])
	}
	if len(resolver.requests) != 1 || resolver.requests[0].ProjectID != 7 {
		t.Fatalf("the row must be resolved once, against the project the schema was built from: %+v", resolver.requests)
	}
}

func TestAStoredToolkitCredentialTheProviderRefusesIsReportedAsAuthFailed(t *testing.T) {
	resolver := &recordingStoredResolver{resolved: map[string]any{"access_token": "wrong"}}
	checker := &recordingToolkitChecker{outcome: ToolkitCheckOutcome{
		Reason: ToolkitCheckReasonAuthFailed, Message: toolkitCheckAuthFailedMessage,
	}}
	handler := storedCheckHandler(
		WithStoredConfigurationResolver(resolver),
		WithToolkitConnectionChecker(checker),
	)

	result, status := handler.checkStoredRow(context.Background(), "7", sealedToolkitCredentialRow())

	if status != http.StatusBadRequest || result.Success {
		t.Fatalf("a refusal must be 400 with success false, got %d %+v", status, result)
	}
	if result.Reason != ToolkitCheckReasonAuthFailed {
		t.Fatalf("reason = %q, want auth_failed", result.Reason)
	}
	// The body the route writes carries the reason beside the message, which is
	// what the credential card switches on.
	body := storedConnectionCheckBody(result)
	if body["reason"] != ToolkitCheckReasonAuthFailed || body["success"] != false {
		t.Fatalf("the response body must publish the refusal: %+v", body)
	}
}

func TestAStoredToolkitCredentialThatDoesNotResolveIsNeverProbed(t *testing.T) {
	resolver := &recordingStoredResolver{err: errors.New("the vault has no such secret")}
	checker := &recordingToolkitChecker{outcome: ToolkitCheckOutcome{Reason: ToolkitCheckReasonOK}}
	handler := storedCheckHandler(
		WithStoredConfigurationResolver(resolver),
		WithToolkitConnectionChecker(checker),
	)

	result, status := handler.checkStoredRow(context.Background(), "7", sealedToolkitCredentialRow())

	if status != http.StatusBadRequest || result.Success {
		t.Fatalf("an unresolvable row must refuse, got %d %+v", status, result)
	}
	if len(checker.data) != 0 {
		t.Fatal("a row whose secret did not redeem must never reach the provider")
	}
}

func TestAStoredToolkitCheckWithoutItsDependenciesRefusesAndDoesNotPanic(t *testing.T) {
	cases := []struct {
		name string
		opts []Option
	}{
		{name: "no resolver", opts: []Option{WithToolkitConnectionChecker(&recordingToolkitChecker{})}},
		{name: "no checker", opts: []Option{
			WithStoredConfigurationResolver(&recordingStoredResolver{resolved: map[string]any{}}),
			WithToolkitConnectionChecker(nil),
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			handler := storedCheckHandler(testCase.opts...)
			result, status := handler.checkStoredRow(context.Background(), "7", sealedToolkitCredentialRow())
			if result.Success || status != http.StatusBadRequest {
				t.Fatalf("an uncomposed check must refuse, got %d %+v", status, result)
			}
		})
	}
}

func TestTheDefaultToolkitCheckerIsComposedByEveryHandler(t *testing.T) {
	// The composition root supplies no toolkit checker (it needs nothing the
	// root owns), so NewHandler has to. A nil one would make every toolkit type
	// answer "not supported yet" again, which is the state this whole change
	// exists to leave.
	if storedCheckHandler().toolkitChecker == nil {
		t.Fatal("NewHandler must always compose a toolkit connection checker")
	}
}
