package api

import (
	"net/http"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
)

// A credential save must work in BOTH master-key states, and against a project
// that has no vault yet.
//
// # THE TWO DEFECTS THIS FILE GUARDS
//
// The compatibility configurations Create and Update now seal every
// schema-declared password field into the project Fernet vault, and they FAIL
// CLOSED when no vault answers. That is correct: the earlier code stored the
// submitted `api_key` verbatim in p_{project}.configuration, and
// migrations/shared/0072 grants the read of that column to the project VIEWER
// role. Failing closed is only correct while the vault path actually works, and
// it did not.
//
//  1. THE KEY REPRESENTATION. configurationSecretSealer validated
//     SECRETS_MASTER_KEY with v2secrets.MasterKeyFromEnv and passed that
//     function's result to NewCurrentSecretVaultRepository. The function
//     returns the DECODED 32 key bytes; the repository takes the ENCODED
//     44-byte form. The repository rejected the key, the sealer was nil, and
//     EVERY credential save answered 503. deploy/docker-compose.yml REQUIRES
//     the variable (`:?`), so that is the default deployment.
//
//  2. THE ABSENT VAULT. migrations/001_initial.sql inserts centry.project id 1
//     and creates p_1 directly. It never calls the provisioner, so the default
//     project of a fresh install holds no centry.secrets_key row. The save then
//     had nothing to seal into and answered 503 as well.
//
// # WHY IT IS A SEPARATE FILE
//
// TestTheCredentialScreenWorksForASeededMemberAndForNoOtherProject runs with no
// SECRETS_MASTER_KEY, because ci-go.yml sets none. That state exercises the
// UNWRAPPED branch only, so it cannot see defect 1 at all. The variable is read
// when the router is built, so covering the other state takes a second router
// and therefore a second test.
func TestACredentialSaveWorksWithAWrappedMasterKeyAndNoVault(t *testing.T) {
	// A valid Fernet key: the URL-safe base64 encoding of 32 bytes. The value
	// is fixed so that a failure is reproducible.
	t.Setenv(v2secrets.MasterKeyEnvVar, "ZWxpdGVhLXNlYWxlci10ZXN0LW1hc3Rlci1rZXktMzI=")

	pool := newCredentialJourneyPool(t)
	seedCredentialJourneyMember(t, pool)

	router := NewRouter(RouterConfig{
		Pool:               pool,
		AuthValidator:      apimw.TokenValidator(credentialJourneyValidator{}),
		PrincipalValidator: testPrincipalValidator{},
		WebhookRepo:        emptyWebhookRepo{},
		EventSource:        newEventSource(),
	})

	status, body := serveJourney(t, router, http.MethodPost,
		"/api/v2/configurations/configurations/1", map[string]any{
			"elitea_title": "wrapped-credential",
			"label":        "wrapped-credential",
			"type":         "open_ai",
			// api_base is present because the type's own schema requires it and
			// the create refuses a body without it before it reaches the sealer
			// (internal/api/v2/configurations/required_fields.go). The value is
			// the one assertSealedAPIKey checks survived the seal untouched.
			"data": map[string]any{"api_key": "sk-wrapped", "api_base": "https://api.openai.com/v1"},
		})
	if status != http.StatusCreated {
		t.Fatalf("the credential save answered %d, want 201. Body: %s\n"+
			"  A 503 here means the sealer is nil or the project has no vault. "+
			"Read the two defects named at the top of this file.", status, body)
	}

	created := decodeJourneyJSON(t, body)
	secretName := assertSealedAPIKey(t, created)
	assertVaultHoldsTheKey(t, pool, secretName, "sk-wrapped")
}
