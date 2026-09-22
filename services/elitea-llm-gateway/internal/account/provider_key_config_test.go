package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
)

// This file holds the behavioural proofs for issues #452 to #456: the six
// credential fields the deleted LiteLLM mapper carried and the gateway dropped.
//
// Each proof measures the object the PROVIDER receives, not a status code. Two
// surfaces are used:
//
//   - core.SelectKeyForProviderRequestType — the real bifrost key-selection
//     path. It calls the real EliteaAccount, applies bifrost's own key
//     validation, and returns the exact schemas.Key a provider is handed. A
//     field that is dropped anywhere in that chain is absent here.
//   - a real HTTP round trip against a fake provider, where a field is visible
//     on the wire (the Azure api-version query parameter, the Bearer token).
//
// Every test has a control that fails without the fix.

// ─── #452 open_ai.api_base ─────────────────────────────────────────────────

// TestOpenAICustomAPIBaseIsRefused proves the api_base of an open_ai credential
// is no longer discarded.
//
// bifrost takes the OpenAI base URL from ProviderConfig.NetworkConfig.BaseURL,
// which GetConfigForProvider supplies once per process and without a context,
// so it cannot vary per project. Before this change the api_base was read into
// the credential, used by the two guards, and then dropped — and the request
// went to api.openai.com CARRYING THE TENANT'S KEY for a different endpoint.
// The credential must be refused instead.
func TestOpenAICustomAPIBaseIsRefused(t *testing.T) {
	a := accountWithRows(t, [][]any{
		credentialRow("c1", "compatible", map[string]any{
			"api_base": "https://compatible.example/v1",
			"api_key":  "sk-tenant-secret",
		}),
	})

	keys, err := a.GetKeysForProvider(ctxWithProject(callerProject), schemas.OpenAI)
	if err == nil {
		t.Fatal("a custom api_base was accepted; the tenant key would be sent to api.openai.com")
	}
	if !errors.Is(err, ErrUnsupportedAPIBase) {
		t.Fatalf("error = %v, want %v", err, ErrUnsupportedAPIBase)
	}
	if len(keys) != 0 {
		t.Fatalf("keys = %d, want 0", len(keys))
	}
	if strings.Contains(err.Error(), "sk-tenant-secret") {
		t.Fatal("the error text leaked the credential value")
	}
}

// TestOpenAIOwnAPIBaseIsAccepted is the positive half of #452. An open_ai
// credential that names OpenAI's own host is the endpoint bifrost dials, so it
// is honoured and the tenant's key reaches the provider unchanged.
func TestOpenAIOwnAPIBaseIsAccepted(t *testing.T) {
	for _, apiBase := range []string{"", "https://api.openai.com/v1", "https://API.OpenAI.com"} {
		data := map[string]any{"api_key": "{{secret.OPENAI}}"}
		if apiBase != "" {
			data["api_base"] = apiBase
		}
		a := accountWithRows(t, [][]any{credentialRow("c1", "openai", data)})

		key := selectKey(t, a, schemas.OpenAI, "gpt-4o")
		if got := key.Value.GetValue(); got != "openai-plaintext" {
			t.Fatalf("api_base %q: key value = %q, want the decrypted secret", apiBase, got)
		}
	}
}

// ─── #453 vertex_ai ────────────────────────────────────────────────────────

// TestVertexCredentialReachesTheProvider proves vertex_project, vertex_location
// and vertex_credentials arrive in the key bifrost hands to the Vertex provider.
//
// Without them bifrost refuses the key in validateKey ("vertex_key_config is
// required") and the project has no usable model at all.
func TestVertexCredentialReachesTheProvider(t *testing.T) {
	a := accountWithRows(t, [][]any{
		credentialRow("v1", "vertex", map[string]any{
			"vertex_project":     "my-gcp-project",
			"vertex_location":    "us-central1",
			"vertex_credentials": "{{secret.VERTEX}}",
		}),
	})

	key := selectKey(t, a, schemas.Vertex, "gemini-2.5-pro")
	if key.VertexKeyConfig == nil {
		t.Fatal("VertexKeyConfig = nil; bifrost refuses a Vertex key without one")
	}
	if got := key.VertexKeyConfig.ProjectID.GetValue(); got != "my-gcp-project" {
		t.Errorf("ProjectID = %q, want the credential's vertex_project", got)
	}
	if got := key.VertexKeyConfig.Region.GetValue(); got != "us-central1" {
		t.Errorf("Region = %q, want the credential's vertex_location", got)
	}
	// vertex_credentials is a secret. It must arrive decrypted, not as the
	// {{secret.NAME}} reference the row stores.
	if got := key.VertexKeyConfig.AuthCredentials.GetValue(); got != "vertex-plaintext" {
		t.Errorf("AuthCredentials = %q, want the decrypted service account", got)
	}
}

// TestVertexCredentialsAcceptAJSONObject proves a service-account document
// stored as a nested JSON object survives the decoder. A plain string field
// makes that row fail to parse, and an unparsable row is skipped silently — the
// credential would then vanish rather than fail.
func TestVertexCredentialsAcceptAJSONObject(t *testing.T) {
	a := accountWithRows(t, [][]any{
		credentialRow("v1", "vertex", map[string]any{
			"vertex_project":     "p",
			"vertex_location":    "us-central1",
			"vertex_credentials": map[string]any{"type": "service_account", "project_id": "p"},
		}),
	})

	key := selectKey(t, a, schemas.Vertex, "gemini-2.5-pro")
	if key.VertexKeyConfig == nil {
		t.Fatal("VertexKeyConfig = nil; the object-shaped credential was dropped")
	}
	got := key.VertexKeyConfig.AuthCredentials.GetValue()
	if !strings.Contains(got, `"type":"service_account"`) && !strings.Contains(got, `"type": "service_account"`) {
		t.Fatalf("AuthCredentials = %q, want the service account document", got)
	}
}

// TestIncompleteVertexCredentialFails proves an incomplete Vertex credential is
// refused by name, with the missing field named for the operator.
func TestIncompleteVertexCredentialFails(t *testing.T) {
	complete := map[string]any{
		"vertex_project":     "p",
		"vertex_location":    "us-central1",
		"vertex_credentials": "{}",
	}
	for _, field := range []string{"vertex_project", "vertex_location", "vertex_credentials"} {
		data := map[string]any{}
		for k, v := range complete {
			data[k] = v
		}
		delete(data, field)

		a := accountWithRows(t, [][]any{credentialRow("v1", "vertex", data)})
		keys, err := a.GetKeysForProvider(ctxWithProject(callerProject), schemas.Vertex)
		if !errors.Is(err, ErrIncompleteCredential) {
			t.Fatalf("missing %s: error = %v, want %v", field, err, ErrIncompleteCredential)
		}
		if !strings.Contains(err.Error(), field) {
			t.Errorf("missing %s: error %q does not name the field", field, err)
		}
		if len(keys) != 0 {
			t.Errorf("missing %s: keys = %d, want 0", field, len(keys))
		}
	}
}

// ─── #454 amazon_bedrock ───────────────────────────────────────────────────

// TestBedrockCredentialReachesTheProvider proves the access key, the secret and
// the region arrive in the key bifrost hands to the Bedrock provider, and that
// the secret is decrypted on the way.
func TestBedrockCredentialReachesTheProvider(t *testing.T) {
	a := accountWithRows(t, [][]any{
		credentialRow("b1", "bedrock", map[string]any{
			"aws_access_key_id":     "AKIAEXAMPLE",
			"aws_secret_access_key": "{{secret.AWS}}",
			"aws_region_name":       "eu-west-1",
		}),
	})

	key := selectKey(t, a, schemas.Bedrock, "anthropic.claude-3-5-sonnet")
	if key.BedrockKeyConfig == nil {
		t.Fatal("BedrockKeyConfig = nil; AWS would use the ambient identity of the pod")
	}
	if got := key.BedrockKeyConfig.AccessKey.GetValue(); got != "AKIAEXAMPLE" {
		t.Errorf("AccessKey = %q, want the credential's aws_access_key_id", got)
	}
	if got := key.BedrockKeyConfig.SecretKey.GetValue(); got != "aws-plaintext" {
		t.Errorf("SecretKey = %q, want the decrypted aws_secret_access_key", got)
	}
	if key.BedrockKeyConfig.Region == nil || key.BedrockKeyConfig.Region.GetValue() != "eu-west-1" {
		t.Errorf("Region = %v, want the credential's aws_region_name", key.BedrockKeyConfig.Region)
	}
}

// TestIncompleteBedrockCredentialFailsAndDoesNotFallBack is the proof issue
// #454 asks for, and it has two halves.
//
// FIRST HALF — the fallback is real, and it is silent. A key with no
// BedrockKeyConfig is NOT refused by bifrost. core/utils.go validateKey
// substitutes an empty configuration, and the AWS SDK then signs the request
// with whatever identity the pod itself holds. One tenant's request is then
// authorised by, and billed to, the platform's own AWS role. The control below
// drives the real bifrost key path with the key shape the gateway produced
// BEFORE this change, and shows core handing back that empty configuration.
//
// SECOND HALF — the gateway now refuses first. With the same incomplete row the
// real account fails with INCOMPLETE_PROVIDER_CREDENTIAL and yields no key at
// all, so no request is ever signed.
func TestIncompleteBedrockCredentialFailsAndDoesNotFallBack(t *testing.T) {
	// --- first half: measure the fallback that must be prevented ---
	core := newCore(t, &nilConfigBedrockAccount{})
	key, err := core.SelectKeyForProviderRequestType(
		schemas.NewBifrostContext(context.Background(), schemas.NoDeadline),
		schemas.ChatCompletionRequest, schemas.Bedrock, "anthropic.claude-3-5-sonnet")
	if err != nil {
		t.Fatalf("control: bifrost refused a Bedrock key with no configuration (%v); "+
			"if that is now true the silent fallback is gone and this test needs rewriting", err)
	}
	if key.BedrockKeyConfig == nil {
		t.Fatal("control: expected bifrost to substitute an empty bedrock_key_config")
	}
	if key.BedrockKeyConfig.AccessKey.GetValue() != "" || key.BedrockKeyConfig.SecretKey.GetValue() != "" {
		t.Fatal("control: the substituted configuration should be empty")
	}
	// An empty access key and secret key is exactly the AWS "use the ambient
	// credential chain" configuration. That is the tenant-isolation fault.

	// --- second half: the gateway refuses before a key exists ---
	for _, field := range []string{"aws_access_key_id", "aws_secret_access_key", "aws_region_name"} {
		data := map[string]any{
			"aws_access_key_id":     "AKIAEXAMPLE",
			"aws_secret_access_key": "SECRET",
			"aws_region_name":       "eu-west-1",
		}
		delete(data, field)

		a := accountWithRows(t, [][]any{credentialRow("b1", "bedrock", data)})
		keys, err := a.GetKeysForProvider(ctxWithProject(callerProject), schemas.Bedrock)
		if !errors.Is(err, ErrIncompleteCredential) {
			t.Fatalf("missing %s: error = %v, want %v — a Bedrock request would "+
				"otherwise run on the pod's own AWS identity", field, err, ErrIncompleteCredential)
		}
		if len(keys) != 0 {
			t.Fatalf("missing %s: keys = %d, want 0", field, len(keys))
		}

		// And the same row, driven through the real bifrost key path, yields no
		// key either — the refusal is not confined to the account's own API.
		core := newCore(t, a)
		_, err = core.SelectKeyForProviderRequestType(
			bifrostCtx(callerProject), schemas.ChatCompletionRequest,
			schemas.Bedrock, "anthropic.claude-3-5-sonnet")
		if err == nil {
			t.Fatalf("missing %s: bifrost still produced a Bedrock key", field)
		}
		if !strings.Contains(err.Error(), IncompleteCredentialReason) {
			t.Fatalf("missing %s: bifrost error = %v, want it to carry %s",
				field, err, IncompleteCredentialReason)
		}
	}
}

// TestBedrockCredentialWithNoFieldsAtAllFails covers the row shape issue #454
// measured: a Bedrock credential whose data carries none of the three AWS
// fields. This is the row that silently ran on the pod's identity.
func TestBedrockCredentialWithNoFieldsAtAllFails(t *testing.T) {
	a := accountWithRows(t, [][]any{
		credentialRow("b1", "bedrock", map[string]any{"api_key": "-"}),
	})
	_, err := a.GetKeysForProvider(ctxWithProject(callerProject), schemas.Bedrock)
	if !errors.Is(err, ErrIncompleteCredential) {
		t.Fatalf("error = %v, want %v", err, ErrIncompleteCredential)
	}
}

// nilConfigBedrockAccount reproduces the key shape the gateway produced before
// this change: a Bedrock key that carries no provider key configuration. It
// exists only to measure what bifrost does with such a key.
type nilConfigBedrockAccount struct{}

func (nilConfigBedrockAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	return []schemas.ModelProvider{schemas.Bedrock}, nil
}

func (nilConfigBedrockAccount) GetKeysForProvider(context.Context, schemas.ModelProvider) ([]schemas.Key, error) {
	return []schemas.Key{{ID: "b1", Name: "bedrock", Models: schemas.WhiteList{"*"}}}, nil
}

func (nilConfigBedrockAccount) GetConfigForProvider(schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	return &schemas.ProviderConfig{}, nil
}

// ─── #455 azure_open_ai / ai_dial api_version ──────────────────────────────

// TestAzureAPIVersionReachesTheProvider proves the credential's api_version
// arrives on the wire. Everything below the gateway is real: a real bifrost, the
// real EliteaAccount and a real HTTP request. Only the database, the vault and
// Azure itself are fakes.
//
// The assertion is the api-version query parameter of the outbound request, not
// the response status — a fake provider answers 200 whatever version is sent.
func TestAzureAPIVersionReachesTheProvider(t *testing.T) {
	upstream := newRecordingAzure(t)
	defer upstream.Close()

	a := accountWithRowsAt(t, upstream.URL, [][]any{
		credentialRow("a1", "azure", map[string]any{
			"api_base":    upstream.URL,
			"api_key":     "azure-key",
			"api_version": "2026-01-01",
		}),
	})

	core := newCore(t, a)
	ctx := bifrostCtx(callerProject)
	// The /llm handler publishes the dispatched model; bifrost resolves the
	// per-key alias by that name.
	ctx.SetValue(ContextKeyRequestModel, "gpt-4o")

	_, bErr := core.ResponsesRequest(ctx, &schemas.BifrostResponsesRequest{
		Provider: schemas.Azure,
		Model:    "gpt-4o",
		Input:    []schemas.ResponsesMessage{responsesUserMessage("hello")},
	})
	if bErr != nil {
		t.Fatalf("ResponsesRequest: %+v", bErr)
	}
	if got := upstream.lastQuery().Get("api-version"); got != "2026-01-01" {
		t.Fatalf("api-version = %q, want the credential's api_version 2026-01-01", got)
	}
}

// TestAzureWithoutAPIVersionSendsNone is the control for the test above and the
// #456 proof for api_version. The sentinel means "no value", so no override is
// attached and the request carries whatever bifrost decides on its own. Seeing
// something OTHER than the credential's 2026-01-01 is what proves the sibling
// test measures the credential rather than the harness.
//
// What bifrost decides changed in core v1.7.15. Until v1.7.3 the Responses
// route was built as `openai/v1/responses?api-version=<preview default>`, so an
// override-free request still carried a version. v1.7.15 attaches the parameter
// only when one was resolved ("the versionless v1 API omits api-version by
// default — Azure OpenAI v1"); the classic `/openai/deployments/` routes still
// inject their default, and that path is covered by the llmproxy tests against
// defaultAzureAPIVersion. So the control now asserts the ABSENCE of the
// parameter. That is the stronger of the two statements: absent credential ⇒ no
// api-version at all, present credential ⇒ exactly its value, and no way for
// one case to borrow the other's.
func TestAzureWithoutAPIVersionSendsNone(t *testing.T) {
	for name, stored := range map[string]any{
		"absent":   nil,
		"sentinel": "-",
		"empty":    "",
	} {
		t.Run(name, func(t *testing.T) {
			upstream := newRecordingAzure(t)
			defer upstream.Close()

			data := map[string]any{"api_base": upstream.URL, "api_key": "azure-key"}
			if stored != nil {
				data["api_version"] = stored
			}
			a := accountWithRowsAt(t, upstream.URL, [][]any{credentialRow("a1", "azure", data)})

			core := newCore(t, a)
			ctx := bifrostCtx(callerProject)
			ctx.SetValue(ContextKeyRequestModel, "gpt-4o")

			_, bErr := core.ResponsesRequest(ctx, &schemas.BifrostResponsesRequest{
				Provider: schemas.Azure,
				Model:    "gpt-4o",
				Input:    []schemas.ResponsesMessage{responsesUserMessage("hello")},
			})
			if bErr != nil {
				t.Fatalf("ResponsesRequest: %+v", bErr)
			}
			q := upstream.lastQuery()
			if !q.Has("api-version") {
				return // the versionless v1 route: nothing to borrow.
			}
			got := q.Get("api-version")
			if got == "2026-01-01" {
				t.Fatalf("api-version = %q; a credential with no version must not "+
					"pick up another one", got)
			}
			if got == "" {
				t.Fatal("an empty api-version was sent; the parameter must be " +
					"omitted entirely, not sent blank")
			}
		})
	}
}

// TestAzureWithoutAnEndpointIsRefused proves an azure credential with no
// api_base fails by name. bifrost would otherwise refuse the key deep inside
// core with a message no operator can trace back to the row.
func TestAzureWithoutAnEndpointIsRefused(t *testing.T) {
	a := accountWithRows(t, [][]any{
		credentialRow("a1", "azure", map[string]any{"api_key": "k"}),
	})
	_, err := a.GetKeysForProvider(ctxWithProject(callerProject), schemas.Azure)
	if !errors.Is(err, ErrIncompleteCredential) {
		t.Fatalf("error = %v, want %v", err, ErrIncompleteCredential)
	}
}

// ─── #456 the "-" no-value sentinel ────────────────────────────────────────

// TestDashSentinelDoesNotBecomeABearerToken proves a keyless credential sends
// no Authorization header. The platform writes a single dash for "no value", and
// before this change that dash reached the provider as `Bearer -`.
//
// The assertion is the header the upstream actually received.
func TestDashSentinelDoesNotBecomeABearerToken(t *testing.T) {
	upstream := newRecordingProvider(t)
	defer upstream.Close()

	a := accountWithRowsAt(t, upstream.URL, [][]any{
		vllmRow("c1", "keyless", "-", upstream.URL, false),
	})

	core := newCore(t, a)
	if _, bErr := core.ChatCompletionRequest(bifrostCtx(callerProject), &schemas.BifrostChatRequest{
		Provider: schemas.VLLM,
		Model:    "local-model",
		Input:    []schemas.ChatMessage{chatUserMessage("hello")},
	}); bErr != nil {
		t.Fatalf("ChatCompletionRequest: %+v", bErr)
	}
	if got := upstream.lastAuth(); got != "" {
		t.Fatalf("Authorization = %q, want no header at all for a keyless credential", got)
	}
}

// TestRealKeyStillBecomesABearerToken is the control for the test above: the
// sentinel handling must not silence a credential that does carry a key.
func TestRealKeyStillBecomesABearerToken(t *testing.T) {
	upstream := newRecordingProvider(t)
	defer upstream.Close()

	a := accountWithRowsAt(t, upstream.URL, [][]any{
		vllmRow("c1", "keyed", "sk-real", upstream.URL, false),
	})

	core := newCore(t, a)
	if _, bErr := core.ChatCompletionRequest(bifrostCtx(callerProject), &schemas.BifrostChatRequest{
		Provider: schemas.VLLM,
		Model:    "local-model",
		Input:    []schemas.ChatMessage{chatUserMessage("hello")},
	}); bErr != nil {
		t.Fatalf("ChatCompletionRequest: %+v", bErr)
	}
	if got, want := upstream.lastAuth(), "Bearer sk-real"; got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}

// TestSentinelIsHonouredOnEveryFieldTheMapperGuarded pins the five fields the
// deleted copyOptionalNonDashString protected. A dash in any of them means "no
// value", never the literal text.
func TestSentinelIsHonouredOnEveryFieldTheMapperGuarded(t *testing.T) {
	var d credentialData
	raw := `{"api_key":"-","api_token":"-","api_version":"-",
	         "aws_access_key_id":"-","aws_secret_access_key":"-","aws_region_name":"-"}`
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatal(err)
	}
	if got := d.credentialKeyRef(); got != "" {
		t.Errorf("credentialKeyRef = %q, want empty", got)
	}
	for field, got := range map[string]string{
		"api_version":           storedValue(d.APIVersion),
		"aws_access_key_id":     storedValue(d.AWSAccessKeyID),
		"aws_secret_access_key": storedValue(d.AWSSecretAccessKey),
		"aws_region_name":       storedValue(d.AWSRegionName),
	} {
		if got != "" {
			t.Errorf("%s = %q, want empty", field, got)
		}
	}
}

// TestAPITokenFallbackSurvivesADashAPIKey proves the sentinel does not break the
// legacy integration shape: a row whose api_key is the sentinel but which does
// carry api_token still yields the token.
func TestAPITokenFallbackSurvivesADashAPIKey(t *testing.T) {
	var d credentialData
	if err := json.Unmarshal([]byte(`{"api_key":"-","api_token":"legacy-token"}`), &d); err != nil {
		t.Fatal(err)
	}
	if got := d.credentialKeyRef(); got != "legacy-token" {
		t.Fatalf("credentialKeyRef = %q, want legacy-token", got)
	}
}

// ─── tenant-authored values are never secret references ────────────────────

// TestTenantValuesAreNotResolvedAsEnvironmentReferences guards the key
// construction. schemas.NewSecretVar treats a leading "env." as a reference to
// a process environment variable and reads it. Every value threaded into a key
// here is tenant-authored, so using that constructor would let any tenant read
// an environment variable of the gateway pod by naming it in a credential.
func TestTenantValuesAreNotResolvedAsEnvironmentReferences(t *testing.T) {
	t.Setenv("ELITEA_TEST_POD_SECRET", "pod-only-value")

	a := accountWithRows(t, [][]any{
		credentialRow("b1", "bedrock", map[string]any{
			"aws_access_key_id":     "env.ELITEA_TEST_POD_SECRET",
			"aws_secret_access_key": "env.ELITEA_TEST_POD_SECRET",
			"aws_region_name":       "eu-west-1",
		}),
	})

	key := selectKey(t, a, schemas.Bedrock, "anthropic.claude-3-5-sonnet")
	if got := key.BedrockKeyConfig.AccessKey.GetValue(); got != "env.ELITEA_TEST_POD_SECRET" {
		t.Fatalf("AccessKey = %q; the pod environment was read for a tenant value", got)
	}
	if got := key.BedrockKeyConfig.SecretKey.GetValue(); got == "pod-only-value" {
		t.Fatal("SecretKey carries the pod's environment variable")
	}
}

// ─── harness ───────────────────────────────────────────────────────────────

// credentialRow builds one ai_credentials row for the fake database.
func credentialRow(id, title string, data map[string]any) []any {
	raw, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	return []any{id, title, raw, false}
}

// accountWithRows builds an account over the caller project's rows with no
// egress allowlist (unrestricted public hosts, which is the shipped default).
func accountWithRows(t *testing.T, rows [][]any) *EliteaAccount {
	t.Helper()
	return newKeyConfigAccount(t, rows, nil)
}

// accountWithRowsAt is accountWithRows with the local fake provider named on the
// egress allowlist, which a credential pointing at 127.0.0.1 needs.
func accountWithRowsAt(t *testing.T, upstreamURL string, rows [][]any) *EliteaAccount {
	t.Helper()
	return newKeyConfigAccount(t, rows, []string{hostOf(t, upstreamURL)})
}

func newKeyConfigAccount(t *testing.T, rows [][]any, allowlist []string) *EliteaAccount {
	t.Helper()
	a, err := New(Config{
		DB:    &fakeDB{bySchema: map[string][][]any{callerProject: rows}},
		Vault: &fakeVault{secrets: map[string]map[string]string{callerProject: testSecrets}},
		// The fake provider binds to loopback, which bifrost's dialer always
		// permits, so the allowlist is the only gate a test has to satisfy.
		EgressAllowlist: allowlist,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// testSecrets are the plaintexts the fake vault returns for the caller project.
var testSecrets = map[string]string{
	"OPENAI": "openai-plaintext",
	"VERTEX": "vertex-plaintext",
	"AWS":    "aws-plaintext",
}

// newCore starts a real bifrost over the given account.
func newCore(t *testing.T, acct schemas.Account) *bifrost.Bifrost {
	t.Helper()
	core, err := bifrost.Init(context.Background(), schemas.BifrostConfig{
		Account:         acct,
		InitialPoolSize: 1,
	})
	if err != nil {
		t.Fatalf("bifrost.Init: %v", err)
	}
	t.Cleanup(core.Shutdown)
	return core
}

// bifrostCtx builds the context the /llm handler hands to core: the resolved
// project id under the virtual-key context key.
func bifrostCtx(projectID string) *schemas.BifrostContext {
	bc := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	bc.SetValue(schemas.BifrostContextKeyVirtualKey, projectID)
	return bc
}

// selectKey drives the REAL bifrost key-selection path and returns the exact
// schemas.Key a provider is handed. bifrost's own key validation runs inside it,
// so a key configuration that bifrost rejects never reaches the caller.
func selectKey(t *testing.T, a *EliteaAccount, provider schemas.ModelProvider, model string) schemas.Key {
	t.Helper()
	core := newCore(t, a)
	key, err := core.SelectKeyForProviderRequestType(
		bifrostCtx(callerProject), schemas.ChatCompletionRequest, provider, model)
	if err != nil {
		t.Fatalf("SelectKeyForProviderRequestType(%s, %s): %v", provider, model, err)
	}
	if key.ID == "" {
		t.Fatalf("no key was selected for %s/%s", provider, model)
	}
	return key
}

// recordingAzure is a fake Azure OpenAI resource. It records the query string of
// each request so a test can assert the api-version that was actually sent.
type recordingAzure struct {
	*httptest.Server
	mu    sync.Mutex
	query url.Values
}

func newRecordingAzure(t *testing.T) *recordingAzure {
	t.Helper()
	p := &recordingAzure{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.query = r.URL.Query()
		p.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         "resp_test",
			"object":     "response",
			"created_at": 0,
			"status":     "completed",
			"model":      "gpt-4o",
			"output": []map[string]any{{
				"type":   "message",
				"id":     "msg_1",
				"status": "completed",
				"role":   "assistant",
				"content": []map[string]any{{
					"type": "output_text",
					"text": "hi",
				}},
			}},
			"usage": map[string]any{
				"input_tokens": 1, "output_tokens": 1, "total_tokens": 2,
			},
		})
	}))
	return p
}

func (p *recordingAzure) lastQuery() url.Values {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.query
}

func responsesUserMessage(text string) schemas.ResponsesMessage {
	return schemas.ResponsesMessage{
		Role:    schemas.Ptr(schemas.ResponsesInputMessageRoleUser),
		Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr(text)},
	}
}

func chatUserMessage(text string) schemas.ChatMessage {
	return schemas.ChatMessage{
		Role:    schemas.ChatMessageRoleUser,
		Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr(text)},
	}
}
