package account

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/maximhq/bifrost/core/providers/azure"
	"github.com/maximhq/bifrost/core/schemas"
)

// workload_identity_test.go — keyless platform credentials.
//
// The matrix below is the security claim of workload_identity.go: the pod's
// own Google identity serves a request ONLY for a shared row of the public
// project, and ONLY with the operator flag on. Every refusal asserts the
// named reason AND that no token was minted, because a refusal that still
// minted a token has already sent the platform identity somewhere.

// fakeTokens is a workloadidentity.Tokens that records every audience asked.
type fakeTokens struct {
	mu        sync.Mutex
	token     string
	err       error
	audiences []string
}

func (f *fakeTokens) IDToken(_ context.Context, audience string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audiences = append(f.audiences, audience)
	if f.err != nil {
		return "", f.err
	}
	return f.token, nil
}

func (f *fakeTokens) AccessToken(context.Context) (string, error) { return "", errors.New("unused") }

func (f *fakeTokens) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.audiences...)
}

// wiRow builds one typed ai_credentials row {id, title, data, shared, type}.
func wiRow(id, configType string, shared bool, data map[string]any) []any {
	raw, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	return []any{id, id, raw, shared, configType}
}

func vertexWIData() map[string]any {
	return map[string]any{
		"auth_mode": "workload_identity", "vertex_project": "platform-gcp", "vertex_location": "us-central1",
	}
}

func dialWIData(apiBase string) map[string]any {
	return map[string]any{"auth_mode": "workload_identity", "api_base": apiBase}
}

// wiAccount builds an account whose shared scope is publicProject, with the
// flag set as given and a fake token source.
func wiAccount(t *testing.T, enabled bool, tokens *fakeTokens, bySchema map[string][][]any, allow ...string) *EliteaAccount {
	t.Helper()
	a, err := New(Config{
		DB:                      &fakeDB{bySchema: bySchema},
		Vault:                   &fakeVault{},
		PublicProjectID:         publicProject,
		WorkloadIdentityEnabled: enabled,
		IdentityTokens:          tokens,
		EgressAllowlist:         allow,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// TestWorkloadIdentityAdmissionMatrix is the allowed/refused matrix for both
// provider types: flag off, a tenant row, a public row that is not shared, and
// the shared public row read by a tenant and by the public project itself.
func TestWorkloadIdentityAdmissionMatrix(t *testing.T) {
	const dialBase = "https://dial.example.com/v1"
	cases := []struct {
		name     string
		enabled  bool
		caller   string
		bySchema func(row []any) map[string][][]any
		shared   bool
		wantErr  error
	}{
		{"flag off", false, callerProject, func(r []any) map[string][][]any {
			return map[string][][]any{publicProject: {r}}
		}, true, ErrWorkloadIdentityNotAllowed},
		{"tenant row", true, callerProject, func(r []any) map[string][][]any {
			return map[string][][]any{callerProject: {r}}
		}, true, ErrWorkloadIdentityNotAllowed},
		{"public row not shared, read by the public project", true, publicProject, func(r []any) map[string][][]any {
			return map[string][][]any{publicProject: {r}}
		}, false, ErrWorkloadIdentityNotAllowed},
		{"shared public row, read by a tenant", true, callerProject, func(r []any) map[string][][]any {
			return map[string][][]any{publicProject: {r}}
		}, true, nil},
		{"shared public row, read by the public project", true, publicProject, func(r []any) map[string][][]any {
			return map[string][][]any{publicProject: {r}}
		}, true, nil},
	}
	for _, tc := range cases {
		t.Run("vertex/"+tc.name, func(t *testing.T) {
			tokens := &fakeTokens{token: "unused"}
			a := wiAccount(t, tc.enabled, tokens, tc.bySchema(wiRow("v-1", "vertex_ai", tc.shared, vertexWIData())))
			keys, err := a.GetKeysForProvider(bifrostCtx(tc.caller), schemas.Vertex)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || len(keys) != 0 {
					t.Fatalf("keys=%d err=%v, want a %v refusal", len(keys), err, tc.wantErr)
				}
				return
			}
			if err != nil || len(keys) != 1 {
				t.Fatalf("keys=%d err=%v, want the platform credential", len(keys), err)
			}
			cfg := keys[0].VertexKeyConfig
			if cfg == nil || cfg.ProjectID.Val != "platform-gcp" || cfg.Region.Val != "us-central1" {
				t.Fatalf("VertexKeyConfig = %+v, want the row's project and location", cfg)
			}
			// The empty AuthCredentials IS the mode: bifrost then reads ADC.
			if cfg.AuthCredentials.Val != "" {
				t.Errorf("AuthCredentials = %q, want empty (ambient identity)", cfg.AuthCredentials.Val)
			}
		})
		t.Run("dial/"+tc.name, func(t *testing.T) {
			tokens := &fakeTokens{token: "google-id-token"}
			a := wiAccount(t, tc.enabled, tokens, tc.bySchema(wiRow("d-1", DialCredentialType, tc.shared, dialWIData(dialBase))))
			ctx := bifrostCtx(tc.caller)
			keys, err := a.GetKeysForProvider(ctx, schemas.Azure)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || len(keys) != 0 {
					t.Fatalf("keys=%d err=%v, want a %v refusal", len(keys), err, tc.wantErr)
				}
				if asked := tokens.asked(); len(asked) != 0 {
					t.Errorf("a refused credential minted tokens for %v", asked)
				}
				if v, _ := ctx.Value(azure.AzureAuthorizationTokenKey).(string); v != "" {
					t.Errorf("a refused credential left a context token %q", v)
				}
				return
			}
			if err != nil || len(keys) != 1 {
				t.Fatalf("keys=%d err=%v, want the platform credential", len(keys), err)
			}
			if keys[0].Value.Val != "" {
				t.Errorf("key value = %q, want empty: the token rides the context", keys[0].Value.Val)
			}
			if got := tokens.asked(); len(got) != 1 || got[0] != "https://dial.example.com" {
				t.Errorf("audiences = %v, want the api_base origin", got)
			}
			if v, _ := ctx.Value(azure.AzureAuthorizationTokenKey).(string); v != "google-id-token" {
				t.Errorf("context token = %q, want the minted ID token", v)
			}
		})
	}
}

// TestWorkloadIdentityAmbiguousOrUnknownIsRefused: a keyless row that still
// stores the key it claims not to use, an unknown auth_mode, and the mode on a
// type it does not support are each refused, not read as a default.
func TestWorkloadIdentityAmbiguousOrUnknownIsRefused(t *testing.T) {
	vertexWithDoc := vertexWIData()
	vertexWithDoc["vertex_credentials"] = `{"type":"service_account"}`
	dialWithKey := dialWIData("https://dial.example.com")
	dialWithKey["api_key"] = "a-key"
	unknown := vertexWIData()
	unknown["auth_mode"] = "ambient"
	cases := []struct {
		name     string
		provider schemas.ModelProvider
		row      []any
		want     error
	}{
		{"vertex with vertex_credentials", schemas.Vertex, wiRow("v", "vertex_ai", true, vertexWithDoc), ErrInvalidAuthMode},
		{"dial with api_key", schemas.Azure, wiRow("d", DialCredentialType, true, dialWithKey), ErrInvalidAuthMode},
		{"unknown auth_mode", schemas.Vertex, wiRow("v", "vertex_ai", true, unknown), ErrInvalidAuthMode},
		{"azure_open_ai type", schemas.Azure, wiRow("z", "azure_open_ai", true, dialWIData("https://x.example.com")), ErrWorkloadIdentityNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tokens := &fakeTokens{token: "t"}
			a := wiAccount(t, true, tokens, map[string][][]any{publicProject: {tc.row}})
			keys, err := a.GetKeysForProvider(bifrostCtx(callerProject), tc.provider)
			if !errors.Is(err, tc.want) || len(keys) != 0 {
				t.Fatalf("keys=%d err=%v, want %v", len(keys), err, tc.want)
			}
			if got := credentialRejectionReason(err); got != strings.TrimSpace(tc.want.Error()) {
				t.Errorf("logged reason = %q, want %q", got, tc.want.Error())
			}
			if asked := tokens.asked(); len(asked) != 0 {
				t.Errorf("minted tokens for %v", asked)
			}
		})
	}
}

// TestWorkloadIdentityKeyModeVertexStillNeedsTheDocument pins the tenant
// protection: an EXPLICIT key mode with no document is still incomplete, even
// for a platform row with the flag on. Only the keyless mode drops the field.
func TestWorkloadIdentityKeyModeVertexStillNeedsTheDocument(t *testing.T) {
	data := vertexWIData()
	data["auth_mode"] = "key"
	a := wiAccount(t, true, &fakeTokens{}, map[string][][]any{publicProject: {wiRow("v", "vertex_ai", true, data)}})
	_, err := a.GetKeysForProvider(bifrostCtx(callerProject), schemas.Vertex)
	if !errors.Is(err, ErrIncompleteCredential) || !strings.Contains(err.Error(), "vertex_credentials") {
		t.Fatalf("err = %v, want INCOMPLETE_PROVIDER_CREDENTIAL naming vertex_credentials", err)
	}
}

// TestWorkloadIdentityTokenFailureRefuses: a mint failure fails the request
// with the named reason. A key returned without a token would reach bifrost's
// DefaultAzureCredential branch.
func TestWorkloadIdentityTokenFailureRefuses(t *testing.T) {
	tokens := &fakeTokens{err: errors.New("metadata: 404")}
	a := wiAccount(t, true, tokens, map[string][][]any{
		publicProject: {wiRow("d", DialCredentialType, true, dialWIData("https://dial.example.com"))},
	})
	ctx := bifrostCtx(callerProject)
	keys, err := a.GetKeysForProvider(ctx, schemas.Azure)
	if !errors.Is(err, ErrWorkloadIdentityTokenUnavailable) || len(keys) != 0 {
		t.Fatalf("keys=%d err=%v, want WORKLOAD_IDENTITY_TOKEN_UNAVAILABLE", len(keys), err)
	}
	if strings.Contains(err.Error(), "metadata: 404") {
		t.Errorf("error %q carries the token source's own message", err)
	}
	if v, _ := ctx.Value(azure.AzureAuthorizationTokenKey).(string); v != "" {
		t.Errorf("context token = %q after a failed mint", v)
	}
}

// TestWorkloadIdentityTokenAudienceOverride: token_audience wins over the
// api_base origin.
func TestWorkloadIdentityTokenAudienceOverride(t *testing.T) {
	data := dialWIData("https://dial.example.com/v1")
	data["token_audience"] = "dial-client-id.apps.example"
	tokens := &fakeTokens{token: "t"}
	a := wiAccount(t, true, tokens, map[string][][]any{publicProject: {wiRow("d", DialCredentialType, true, data)}})
	if _, err := a.GetKeysForProvider(bifrostCtx(callerProject), schemas.Azure); err != nil {
		t.Fatalf("GetKeysForProvider: %v", err)
	}
	if got := tokens.asked(); len(got) != 1 || got[0] != "dial-client-id.apps.example" {
		t.Fatalf("audiences = %v, want the token_audience", got)
	}
}

// mixedDialAccount holds a tenant's own keyed DIAL credential ("own") and the
// platform's shared keyless one ("d").
func mixedDialAccount(t *testing.T, enabled bool, tokens *fakeTokens) *EliteaAccount {
	t.Helper()
	tenantRow := wiRow("own", DialCredentialType, false, map[string]any{
		"api_base": "https://tenant-dial.example.com", "api_key": "tenant-key",
	})
	return wiAccount(t, enabled, tokens, map[string][][]any{
		callerProject: {tenantRow},
		publicProject: {wiRow("d", DialCredentialType, true, dialWIData("https://dial.example.com"))},
	})
}

// TestWorkloadIdentityDialKeySelectionFollowsTheModelLink: the context token is
// per request and every key serves every model (Models ["*"]), so bifrost
// cannot keep a keyless DIAL key apart from a keyed one. The model's link is
// the one statement of which credential serves it:
//
//   - linked to the tenant's key: that key alone, no token minted, a stale
//     token cleared — the platform token never reaches the tenant endpoint;
//   - linked to the platform's keyless row: that row alone, with its token —
//     the platform model never goes to the tenant endpoint;
//   - unlinked: refused with the named reason rather than guessed.
func TestWorkloadIdentityDialKeySelectionFollowsTheModelLink(t *testing.T) {
	t.Run("linked to the tenant key", func(t *testing.T) {
		tokens := &fakeTokens{token: "google-id-token"}
		ctx := bifrostCtx(callerProject)
		ctx.SetValue(azure.AzureAuthorizationTokenKey, "stale-token")
		ctx.SetValue(ContextKeyLinkedCredential, LinkedCredential{ProjectID: callerProject, ConfigID: "own"})
		keys, err := mixedDialAccount(t, true, tokens).GetKeysForProvider(ctx, schemas.Azure)
		if err != nil || len(keys) != 1 || keys[0].ID != "own" || keys[0].Value.Val != "tenant-key" {
			t.Fatalf("keys = %+v, err = %v, want the tenant's keyed credential alone", keys, err)
		}
		if v, _ := ctx.Value(azure.AzureAuthorizationTokenKey).(string); v != "" {
			t.Errorf("context token = %q, want it cleared for a keyed credential", v)
		}
		if asked := tokens.asked(); len(asked) != 0 {
			t.Errorf("minted tokens for %v although no keyless key was served", asked)
		}
	})
	t.Run("linked to the platform keyless row", func(t *testing.T) {
		tokens := &fakeTokens{token: "google-id-token"}
		ctx := bifrostCtx(callerProject)
		ctx.SetValue(ContextKeyLinkedCredential, LinkedCredential{ProjectID: publicProject, ConfigID: "d"})
		keys, err := mixedDialAccount(t, true, tokens).GetKeysForProvider(ctx, schemas.Azure)
		if err != nil || len(keys) != 1 || keys[0].Value.Val != "" ||
			keys[0].AzureKeyConfig == nil || keys[0].AzureKeyConfig.Endpoint.Val != "https://dial.example.com" {
			t.Fatalf("keys = %+v, err = %v, want the platform keyless credential alone", keys, err)
		}
		if v, _ := ctx.Value(azure.AzureAuthorizationTokenKey).(string); v != "google-id-token" {
			t.Errorf("context token = %q, want the minted ID token", v)
		}
	})
	t.Run("unlinked", func(t *testing.T) {
		tokens := &fakeTokens{token: "google-id-token"}
		ctx := bifrostCtx(callerProject)
		ctx.SetValue(azure.AzureAuthorizationTokenKey, "stale-token")
		keys, err := mixedDialAccount(t, true, tokens).GetKeysForProvider(ctx, schemas.Azure)
		if !errors.Is(err, ErrWorkloadIdentityAmbiguous) || len(keys) != 0 {
			t.Fatalf("keys = %+v, err = %v, want WORKLOAD_IDENTITY_CREDENTIAL_AMBIGUOUS", keys, err)
		}
		if got := credentialRejectionReason(err); got != WorkloadIdentityAmbiguousReason {
			t.Errorf("reason = %q", got)
		}
		if v, _ := ctx.Value(azure.AzureAuthorizationTokenKey).(string); v != "" {
			t.Errorf("context token = %q, want it cleared on the refusal", v)
		}
		if asked := tokens.asked(); len(asked) != 0 {
			t.Errorf("minted tokens for %v on a refused request", asked)
		}
	})
}

// TestRefusedKeylessRowDoesNotPoisonTheProvider: a keyless row the account
// refuses (here: the flag is off) is skipped on an unlinked request, so the
// tenant's own credential still serves. A request whose model LINKS the
// refused row still fails with the named reason.
func TestRefusedKeylessRowDoesNotPoisonTheProvider(t *testing.T) {
	t.Run("unlinked: skipped", func(t *testing.T) {
		tokens := &fakeTokens{token: "google-id-token"}
		keys, err := mixedDialAccount(t, false, tokens).GetKeysForProvider(bifrostCtx(callerProject), schemas.Azure)
		if err != nil || len(keys) != 1 || keys[0].ID != "own" {
			t.Fatalf("keys = %+v, err = %v, want the tenant credential with the refused row skipped", keys, err)
		}
		if asked := tokens.asked(); len(asked) != 0 {
			t.Errorf("minted tokens for %v", asked)
		}
	})
	t.Run("vertex unlinked: skipped beside a keyed row", func(t *testing.T) {
		keyed := map[string]any{
			"vertex_project": "tenant-gcp", "vertex_location": "us-central1",
			"vertex_credentials": `{"type":"service_account"}`,
		}
		a := wiAccount(t, false, &fakeTokens{}, map[string][][]any{
			callerProject: {wiRow("own-v", "vertex_ai", false, keyed)},
			publicProject: {wiRow("v", "vertex_ai", true, vertexWIData())},
		})
		keys, err := a.GetKeysForProvider(bifrostCtx(callerProject), schemas.Vertex)
		if err != nil || len(keys) != 1 || keys[0].ID != "own-v" {
			t.Fatalf("keys = %+v, err = %v, want the tenant's keyed Vertex credential", keys, err)
		}
	})
	t.Run("linked: the named reason", func(t *testing.T) {
		tokens := &fakeTokens{token: "google-id-token"}
		ctx := bifrostCtx(callerProject)
		ctx.SetValue(ContextKeyLinkedCredential, LinkedCredential{ProjectID: publicProject, ConfigID: "d"})
		keys, err := mixedDialAccount(t, false, tokens).GetKeysForProvider(ctx, schemas.Azure)
		if !errors.Is(err, ErrWorkloadIdentityNotAllowed) || len(keys) != 0 {
			t.Fatalf("keys = %+v, err = %v, want WORKLOAD_IDENTITY_NOT_ALLOWED for the linked row", keys, err)
		}
		if asked := tokens.asked(); len(asked) != 0 {
			t.Errorf("minted tokens for %v", asked)
		}
	})
}

// TestWorkloadIdentityDialWireCarriesTheBearerOnEveryRoute drives a real
// bifrost core against the fake DIAL. On all three DIAL routes the upstream
// must receive `Authorization: Bearer <id token>` and no api-key / x-api-key.
// The anthropic route is the one this proves most: bifrost builds its headers
// with isAnthropicModel=true, which reads the same context token first.
func TestWorkloadIdentityDialWireCarriesTheBearerOnEveryRoute(t *testing.T) {
	for _, tc := range []struct {
		name     string
		model    string
		protocol DialProtocol
		path     string
	}{
		{"deployment", "gpt-4o", DialProtocolAzure, "/openai/deployments/gpt-4o/chat/completions"},
		{"anthropic", "dial-sonnet-4-5", DialProtocolAnthropic, "/anthropic/v1/messages"},
		{"openai responses", "gpt-5", DialProtocolOpenAI, "/openai/v1/responses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDIAL(t)
			tokens := &fakeTokens{token: "google-id-token"}
			a := wiAccount(t, true, tokens, map[string][][]any{
				publicProject: {wiRow("dial-1", DialCredentialType, true, dialWIData(d.URL))},
			}, hostOf(t, d.URL))
			core := newCore(t, a)

			ctx := bifrostCtx(publicProject)
			ctx.SetValue(ContextKeyRequestModel, tc.model)
			ctx.SetValue(ContextKeyLinkedCredential, LinkedCredential{
				ProjectID: publicProject, ConfigID: "dial-1", DialProtocol: tc.protocol,
			})
			ctx.SetValue(ContextKeyDispatchKind, DispatchChat)
			if tc.protocol == DialProtocolOpenAI {
				ctx.SetValue(schemas.BifrostContextKeyChangeRequestType, schemas.ResponsesRequest)
			}
			if _, bErr := core.ChatCompletionRequest(ctx, reasoningChat(tc.model, nil)); bErr != nil {
				t.Fatalf("ChatCompletionRequest: %+v", bErr)
			}
			got := d.only(t)
			if got.path != tc.path {
				t.Fatalf("path = %q, want %q", got.path, tc.path)
			}
			if v := got.header.Get("Authorization"); v != "Bearer google-id-token" {
				t.Errorf("Authorization = %q, want the minted ID token", v)
			}
			for _, h := range []string{"Api-Key", "X-Api-Key"} {
				if v := got.header.Get(h); v != "" {
					t.Errorf("%s = %q, want none for a keyless credential", h, v)
				}
			}
			if asked := tokens.asked(); len(asked) == 0 || asked[0] != strings.TrimRight(d.URL, "/") {
				t.Errorf("audiences = %v, want the fake DIAL origin %q", asked, d.URL)
			}
		})
	}
}
