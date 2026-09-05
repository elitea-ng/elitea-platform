package configurations

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/jackc/pgx/v5"
)

// The compatibility Create and Update used to marshal the caller's `data`
// object into p_{project}.configuration verbatim. The provider api_key was
// then stored in clear text, and migrations/shared/0072 grants the read of
// that column to the project VIEWER role.
//
// These tests pin the two rules that close it: the row keeps a reference, and
// a deployment with no vault REFUSES the write.

type stubSecretSealer struct{}

func (stubSecretSealer) SealProjectHiddenSecrets(
	_ context.Context,
	_ pgx.Tx,
	_ int64,
	_ []configurationapp.HiddenSecretMutation,
) error {
	return nil
}

var hiddenSecretReference = regexp.MustCompile(`^\{\{secret\.[0-9a-f]{32}\}\}$`)

func TestSealConfigurationSecretsReplacesTheProviderKey(t *testing.T) {
	t.Parallel()
	handler := NewHandler(nil, WithSecretSealer(stubSecretSealer{}))

	data := map[string]any{
		"api_key":  "sk-live-secret-value",
		"api_base": "https://api.openai.com/v1",
	}
	sealed, mutations, failure := handler.sealConfigurationSecrets(context.Background(), "open_ai", data)
	if failure != nil {
		t.Fatalf("the write was refused: %d %s", failure.status, failure.message)
	}
	reference, _ := sealed["api_key"].(string)
	if !hiddenSecretReference.MatchString(reference) {
		t.Fatalf("api_key must become a hidden-secret reference, got %q", reference)
	}
	if strings.Contains(reference, "sk-live") {
		t.Fatalf("the stored value still holds the credential: %q", reference)
	}
	if sealed["api_base"] != "https://api.openai.com/v1" {
		t.Fatalf("api_base must not change, got %v", sealed["api_base"])
	}
	if data["api_key"] != "sk-live-secret-value" {
		t.Fatalf("the caller's object must not change, got %v", data["api_key"])
	}
	if len(mutations) != 1 {
		t.Fatalf("expected one vault mutation, got %d", len(mutations))
	}
	if mutations[0].Value != "sk-live-secret-value" {
		t.Fatalf("the vault must receive the plaintext, got %q", mutations[0].Value)
	}
}

// A type the pinned catalogue does not describe is REFUSED (#G3).
//
// The catalogue is the only authority on which field is a secret, so "no
// entry" used to mean "no field is a secret" and the whole object was stored
// verbatim. A `token`, an `api_key` or a password then sat in clear text in
// p_{project}.configuration, and migrations/shared/0072 grants the read of
// that column to the project VIEWER role. Absence must fail closed.
func TestSealConfigurationSecretsRefusesAnUnknownType(t *testing.T) {
	t.Parallel()
	handler := NewHandler(nil, WithSecretSealer(stubSecretSealer{}))

	data := map[string]any{"token": "plain"}
	sealed, mutations, failure := handler.sealConfigurationSecrets(
		context.Background(), "not_a_registered_type", data)
	if failure == nil {
		t.Fatalf("an undescribed type was accepted; it would store %v in plaintext", sealed)
	}
	if failure.status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", failure.status, http.StatusBadRequest)
	}
	if failure.message == "" {
		t.Fatal("the refusal carries no message, so the caller cannot act on it")
	}
	// The refusal must not echo the value it refused.
	if strings.Contains(failure.message, "plain") {
		t.Fatalf("the message repeats the submitted value: %q", failure.message)
	}
	if sealed != nil || len(mutations) != 0 {
		t.Fatalf("a refused write returned data=%v mutations=%d", sealed, len(mutations))
	}
}

// A type with no `type` at all is the same refusal. The create route reads
// `type` from the body, so an absent one used to store free-form data with no
// schema behind it.
func TestSealConfigurationSecretsRefusesAnAbsentType(t *testing.T) {
	t.Parallel()
	handler := NewHandler(nil, WithSecretSealer(stubSecretSealer{}))

	_, _, failure := handler.sealConfigurationSecrets(
		context.Background(), "", map[string]any{"api_key": "sk-live-secret-value"})
	if failure == nil {
		t.Fatal("a write with no configuration type must be refused")
	}
	if failure.status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", failure.status, http.StatusBadRequest)
	}
}

// An EMPTY data object still passes. The row then carries nothing to seal, and
// refusing it would break a write that stores only `shared` or a label.
func TestSealConfigurationSecretsAllowsAnEmptyObject(t *testing.T) {
	t.Parallel()
	handler := NewHandler(nil)

	sealed, mutations, failure := handler.sealConfigurationSecrets(
		context.Background(), "not_a_registered_type", map[string]any{})
	if failure != nil {
		t.Fatalf("an empty object was refused: %d %s", failure.status, failure.message)
	}
	if len(sealed) != 0 || len(mutations) != 0 {
		t.Fatalf("sealed=%v mutations=%d, want empty", sealed, len(mutations))
	}
}

// The three types this platform could dispatch to and could not describe.
// Each one now seals its key and keeps its other fields (#G3).
func TestSealConfigurationSecretsSealsTheGatewayOnlyCredentialTypes(t *testing.T) {
	t.Parallel()
	handler := NewHandler(nil, WithSecretSealer(stubSecretSealer{}))

	for _, configType := range []string{"anthropic", "open_ai_azure", "vllm"} {
		data := map[string]any{
			"api_key":  "sk-live-secret-value",
			"api_base": "https://provider.example.test",
		}
		sealed, mutations, failure := handler.sealConfigurationSecrets(
			context.Background(), configType, data)
		if failure != nil {
			t.Fatalf("%s: the write was refused: %d %s", configType, failure.status, failure.message)
		}
		reference, _ := sealed["api_key"].(string)
		if !hiddenSecretReference.MatchString(reference) {
			t.Fatalf("%s: api_key must become a hidden-secret reference, got %q", configType, reference)
		}
		if len(mutations) != 1 || mutations[0].Value != "sk-live-secret-value" {
			t.Fatalf("%s: the vault must receive the one plaintext, got %d mutations",
				configType, len(mutations))
		}
		if sealed["api_base"] != "https://provider.example.test" {
			t.Fatalf("%s: api_base must not change, got %v", configType, sealed["api_base"])
		}
	}
}

// An unknown field of a KNOWN type is kept. ExtractCurrentConfigurationSecrets
// refuses one, and this route must not: it accepts free-form data today.
func TestSealConfigurationSecretsKeepsAnUnknownField(t *testing.T) {
	t.Parallel()
	handler := NewHandler(nil, WithSecretSealer(stubSecretSealer{}))

	data := map[string]any{
		"api_key":     "sk-live-secret-value",
		"api_base":    "https://api.openai.com/v1",
		"extra_field": "kept",
	}
	sealed, _, failure := handler.sealConfigurationSecrets(context.Background(), "open_ai", data)
	if failure != nil {
		t.Fatalf("the write was refused: %d %s", failure.status, failure.message)
	}
	if sealed["extra_field"] != "kept" {
		t.Fatalf("an unknown field must survive, got %v", sealed["extra_field"])
	}
}

// An existing reference round-trips. A read-modify-write of a sealed row must
// not create a second vault entry.
func TestSealConfigurationSecretsKeepsAnExistingReference(t *testing.T) {
	t.Parallel()
	handler := NewHandler(nil, WithSecretSealer(stubSecretSealer{}))

	reference := "{{secret.0123456789abcdef0123456789abcdef}}"
	sealed, mutations, failure := handler.sealConfigurationSecrets(
		context.Background(), "open_ai", map[string]any{"api_key": reference})
	if failure != nil {
		t.Fatalf("the write was refused: %d %s", failure.status, failure.message)
	}
	if len(mutations) != 0 {
		t.Fatalf("expected no vault mutation, got %d", len(mutations))
	}
	if sealed["api_key"] != reference {
		t.Fatalf("the reference must survive, got %v", sealed["api_key"])
	}
}

// Fail closed. A deployment with no vault must refuse the write, because a
// plaintext fallback is the defect.
func TestSealConfigurationSecretsRefusesWithoutAVault(t *testing.T) {
	t.Parallel()
	handler := NewHandler(nil)

	_, _, failure := handler.sealConfigurationSecrets(
		context.Background(), "open_ai", map[string]any{"api_key": "sk-live-secret-value"})
	if failure == nil {
		t.Fatal("a write that carries a credential must be refused without a vault")
	}
	if failure.status != 503 {
		t.Fatalf("expected 503, got %d", failure.status)
	}
}
