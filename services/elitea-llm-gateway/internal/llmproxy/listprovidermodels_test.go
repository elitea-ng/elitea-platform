package llmproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// listprovidermodels_test.go covers POST /llm/v1/list_provider_models.
//
// The tests that matter most are the NEGATIVE ones: a listing route reaches
// the same tenant-authored hosts the checkers reach, so the questions are "did
// it dial at all when the gate said no" and "did anything of the provider's
// body other than the ids come back". Both are asserted against a real
// loopback provider whose hit count is read.

// fakeListProvider is a loopback stand-in that answers one fixed body.
type fakeListProvider struct {
	*httptest.Server
	hits     atomic.Int64
	status   int
	body     string
	lastPath string
	lastAuth string
	lastKey  string
}

func newFakeListProvider(status int, body string) *fakeListProvider {
	fp := &fakeListProvider{status: status, body: body}
	fp.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fp.hits.Add(1)
		fp.lastPath = r.URL.Path + "?" + r.URL.RawQuery
		fp.lastAuth = r.Header.Get("Authorization")
		fp.lastKey = r.Header.Get("api-key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fp.status)
		_, _ = w.Write([]byte(fp.body))
	}))
	return fp
}

func (fp *fakeListProvider) Hits() int64 { return fp.hits.Load() }

func doListProviderModels(t *testing.T, h *Handler, body checkConnectionRequest) listProviderModelsResponse {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/list_provider_models", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	h.ListProviderModels(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the contract answers 200 with success=false)", rec.Code)
	}
	var resp listProviderModelsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

// TestListProviderModels_ListersMatchTheCheckers pins the two maps together.
//
// A lister WITHOUT a checker entry has no dialTargets, so its destination
// would never be put through the egress allowlist — the handler refuses that
// case, and this test makes sure the refusal is never the normal path. A
// checker without a lister is honest ("unsupported"), but it is still drift
// worth failing on: both maps describe the same six provider dialects.
func TestListProviderModels_ListersMatchTheCheckers(t *testing.T) {
	for providerType := range providerModelListers {
		if _, ok := checkConnectionProviders[providerType]; !ok {
			t.Fatalf("type %q lists models but has no connection checker, so its host is never gated", providerType)
		}
	}
	for providerType := range checkConnectionProviders {
		if _, ok := providerModelListers[providerType]; !ok {
			t.Fatalf("type %q can be checked but not listed; add its lister or say why", providerType)
		}
	}
}

// TestListOpenAICompatibleModels_ReadsTheListing proves the open_ai lister
// makes the real GET /models call, with the bearer header, and returns the ids
// from its body.
//
// Lister-level, like the checkers' own open_ai tests: the SSRF-guarded client
// refuses a loopback address for every class that is not self-hosted, so a
// handler-level test of this dialect could only ever prove the refusal. That
// refusal has its own test below.
func TestListOpenAICompatibleModels_ReadsTheListing(t *testing.T) {
	fp := newFakeListProvider(http.StatusOK,
		`{"object":"list","data":[{"id":"gpt-4o","object":"model","owned_by":"openai"},{"id":"gpt-4o-mini"}]}`)
	defer fp.Close()

	ids, err := listOpenAICompatibleModels(context.Background(), fp.Client(), checkConnectionRequest{
		Type: "open_ai", APIBase: fp.URL, APIKey: "sk-test",
	})
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if fp.Hits() != 1 {
		t.Fatalf("expected exactly one real round trip, got %d", fp.Hits())
	}
	if !strings.HasPrefix(fp.lastPath, "/models") {
		t.Fatalf("path = %q, want the provider's own /models listing", fp.lastPath)
	}
	if fp.lastAuth != "Bearer sk-test" {
		t.Fatalf("Authorization = %q, want the supplied key as a bearer token", fp.lastAuth)
	}
	if len(ids) != 2 || ids[0] != "gpt-4o" || ids[1] != "gpt-4o-mini" {
		t.Fatalf("ids = %v, want [gpt-4o gpt-4o-mini] in the provider's own order", ids)
	}
}

// TestListProviderModels_NeverReachesAPrivateAddress proves this route
// inherits the checkers' address-level SSRF guard: a CLOUD credential pointed
// at a loopback address is refused even when the operator's allowlist names
// that host, because the guard is about the resolved ADDRESS.
//
// The type is azure_open_ai rather than open_ai. An open_ai credential with a
// non-OpenAI api_base resolves to the vLLM provider on the request path, so it
// is a self-hosted credential, and this case would then measure the opposite
// rule — see TestListProviderModels_OpenAIWithAPrivateBaseListsLikeTheRequestPath.
func TestListProviderModels_NeverReachesAPrivateAddress(t *testing.T) {
	fp := newFakeListProvider(http.StatusOK, `{"data":[{"id":"gpt-4o"}]}`)
	defer fp.Close()

	// Allowlist says yes; Azure is a cloud class, so private is still out.
	h := newCheckConnectionHandler(fakeEgressPolicy{allow: true, configured: true})
	resp := doListProviderModels(t, h, checkConnectionRequest{
		Type: "azure_open_ai", APIBase: fp.URL, APIKey: "sk-test",
	})

	if resp.Success {
		t.Fatal("azure_open_ai must never list models from a private address")
	}
	if resp.Reason != checkConnectionReasonUnreachable {
		t.Fatalf("reason = %q, want %q", resp.Reason, checkConnectionReasonUnreachable)
	}
	if fp.Hits() != 0 {
		t.Fatalf("a private address was reached %d times", fp.Hits())
	}
}

// TestListProviderModels_OpenAIWithAPrivateBaseListsLikeTheRequestPath is the
// listing half of the defect the probe carried: the admin model picker could
// not read the models of a self-hosted OpenAI-compatible endpoint that serves
// completions perfectly well.
func TestListProviderModels_OpenAIWithAPrivateBaseListsLikeTheRequestPath(t *testing.T) {
	fp := newFakeListProvider(http.StatusOK, `{"data":[{"id":"qwen3-0.6b"}]}`)
	defer fp.Close()

	h := newCheckConnectionHandler(fakeEgressPolicy{allow: true, configured: true})
	resp := doListProviderModels(t, h, checkConnectionRequest{
		Type: "open_ai", APIBase: fp.URL + "/v1", APIKey: "sk-test",
	})

	if !resp.Success {
		t.Fatalf("resp = %+v, want the listing of a self-hosted OpenAI-compatible host", resp)
	}
	if len(resp.Models) != 1 || resp.Models[0] != "qwen3-0.6b" {
		t.Fatalf("models = %v, want [qwen3-0.6b]", resp.Models)
	}
}

// TestListProviderModels_VLLMListsFromTheOpenAISurface pins the vLLM listing
// path. vLLM serves /v1/models, never Ollama's /api/tags.
func TestListProviderModels_VLLMListsFromTheOpenAISurface(t *testing.T) {
	fp := newFakeListProvider(http.StatusOK, `{"data":[{"id":"qwen3-0.6b"}]}`)
	defer fp.Close()

	h := newCheckConnectionHandler(fakeEgressPolicy{allow: true, configured: true})
	resp := doListProviderModels(t, h, checkConnectionRequest{
		Type: "vllm", APIBase: fp.URL + "/v1",
	})

	if !resp.Success {
		t.Fatalf("resp = %+v, want a successful vLLM listing", resp)
	}
	if !strings.HasPrefix(fp.lastPath, "/v1/models") {
		t.Fatalf("path = %q, want /v1/models", fp.lastPath)
	}
}

// TestListProviderModels_ReturnsIDsAndNothingElse proves the route relays the
// ids and drops every other field of the provider's body.
//
// That is the whole disclosure rule of this endpoint: the checkers return no
// body at all, and this one returns names only. A provider that puts an
// account id, a quota or an error sentence in its listing must not have it
// arrive in an admin screen.
func TestListProviderModels_ReturnsIDsAndNothingElse(t *testing.T) {
	fp := newFakeListProvider(http.StatusOK, `{"models":[
		{"name":"llama3:latest","digest":"sha256:secret-digest","details":{"note":"do not disclose"}}
	],"account":"org-secret-4711","message":"internal detail"}`)
	defer fp.Close()

	h := newCheckConnectionHandler(fakeEgressPolicy{allow: true, configured: true})
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/list_provider_models",
		strings.NewReader(fmt.Sprintf(`{"type":"ollama","api_base":%q}`, fp.URL)))
	rec := httptest.NewRecorder()
	h.ListProviderModels(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "llama3:latest") {
		t.Fatalf("response does not carry the model id: %s", body)
	}
	for _, leaked := range []string{"org-secret-4711", "internal detail", "do not disclose", "secret-digest"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("response leaked %q from the provider body: %s", leaked, body)
		}
	}
}

// TestListProviderModels_EgressDeniedNeverDials proves the allowlist decision
// is made BEFORE any connection, exactly as it is for a check.
func TestListProviderModels_EgressDeniedNeverDials(t *testing.T) {
	fp := newFakeListProvider(http.StatusOK, `{"data":[{"id":"gpt-4o"}]}`)
	defer fp.Close()

	h := newCheckConnectionHandler(fakeEgressPolicy{allow: false})
	resp := doListProviderModels(t, h, checkConnectionRequest{
		Type: "open_ai", APIBase: fp.URL, APIKey: "sk-test",
	})

	if resp.Success || resp.Reason != checkConnectionReasonEgress {
		t.Fatalf("resp = %+v, want a refusal with reason %q", resp, checkConnectionReasonEgress)
	}
	if fp.Hits() != 0 {
		t.Fatalf("a forbidden host was dialled %d times", fp.Hits())
	}
	if len(resp.Models) != 0 {
		t.Fatalf("models = %v, want none for a refused request", resp.Models)
	}
}

// TestListProviderModels_NilPolicyFailsClosed proves a handler composed
// without an egress policy refuses instead of dialling ungated.
func TestListProviderModels_NilPolicyFailsClosed(t *testing.T) {
	fp := newFakeListProvider(http.StatusOK, `{"data":[{"id":"gpt-4o"}]}`)
	defer fp.Close()

	h := NewHandler(nil, nil, nil) // no WithEgressPolicy
	resp := doListProviderModels(t, h, checkConnectionRequest{Type: "open_ai", APIBase: fp.URL})

	if resp.Success || resp.Reason != checkConnectionReasonEgress {
		t.Fatalf("resp = %+v, want a closed failure", resp)
	}
	if fp.Hits() != 0 {
		t.Fatalf("an ungated request was dialled %d times", fp.Hits())
	}
}

// TestListProviderModels_UnsupportedTypeNeverDials proves an unknown type is
// refused with no network call.
func TestListProviderModels_UnsupportedTypeNeverDials(t *testing.T) {
	fp := newFakeListProvider(http.StatusOK, `{"data":[{"id":"gpt-4o"}]}`)
	defer fp.Close()

	h := newCheckConnectionHandler(fakeEgressPolicy{allow: true, configured: true})
	// vllm is listable now, so it is no longer one of these. anthropic still
	// is: providerConfigTypes can build a key for it, and no lister exists.
	for _, providerType := range []string{"", "anthropic", "github", "pgvector"} {
		resp := doListProviderModels(t, h, checkConnectionRequest{Type: providerType, APIBase: fp.URL})
		if resp.Success || resp.Reason != checkConnectionReasonUnsupported {
			t.Fatalf("type %q: resp = %+v, want unsupported_type", providerType, resp)
		}
	}
	if fp.Hits() != 0 {
		t.Fatalf("an unsupported type dialled the provider %d times", fp.Hits())
	}
}

// TestListProviderModels_MissingAPIBaseNeverDials proves a payload that names
// no destination is refused by the checker's own dialTargets function.
func TestListProviderModels_MissingAPIBaseNeverDials(t *testing.T) {
	h := newCheckConnectionHandler(fakeEgressPolicy{allow: true, configured: true})
	resp := doListProviderModels(t, h, checkConnectionRequest{Type: "open_ai", APIBase: "   "})
	if resp.Success || resp.Reason != checkConnectionReasonMissingBase {
		t.Fatalf("resp = %+v, want %q", resp, checkConnectionReasonMissingBase)
	}
}

// TestListProviderModels_RejectedCredentialIsNotAnEmptyList proves a provider
// refusal reports the refusal, and never an empty catalogue.
//
// An empty list would be adopted as "this provider offers no models", which is
// the absence-reads-as-correctness failure: the operator would conclude the
// credential works and the provider is empty.
func TestListProviderModels_RejectedCredentialIsNotAnEmptyList(t *testing.T) {
	for _, tc := range []struct {
		status     int
		wantReason string
	}{
		{http.StatusUnauthorized, checkConnectionReasonUnauth},
		{http.StatusForbidden, checkConnectionReasonUnauth},
		{http.StatusBadGateway, checkConnectionReasonUpstream},
	} {
		fp := newFakeListProvider(tc.status, `{"error":"nope"}`)
		h := newCheckConnectionHandler(fakeEgressPolicy{allow: true, configured: true})
		resp := doListProviderModels(t, h, checkConnectionRequest{Type: "ollama", APIBase: fp.URL})
		fp.Close()

		if resp.Success {
			t.Fatalf("status %d reported success", tc.status)
		}
		if resp.Reason != tc.wantReason {
			t.Fatalf("status %d: reason = %q, want %q", tc.status, resp.Reason, tc.wantReason)
		}
		if strings.Contains(resp.Detail, "nope") {
			t.Fatalf("detail carried the provider body: %q", resp.Detail)
		}
		if len(resp.Models) != 0 {
			t.Fatalf("status %d returned models %v for a refusal", tc.status, resp.Models)
		}
	}
}

// TestListProviderModels_UnreadableBodyIsNotACredentialVerdict proves a 200
// whose body is not a listing is reported as an upstream problem, not as a
// rejected key. Telling an operator to rotate a working key is the failure.
func TestListProviderModels_UnreadableBodyIsNotACredentialVerdict(t *testing.T) {
	fp := newFakeListProvider(http.StatusOK, `<html>not json</html>`)
	defer fp.Close()

	h := newCheckConnectionHandler(fakeEgressPolicy{allow: true, configured: true})
	resp := doListProviderModels(t, h, checkConnectionRequest{Type: "ollama", APIBase: fp.URL})

	if resp.Success {
		t.Fatal("an unreadable body reported success")
	}
	if resp.Reason != checkConnectionReasonUpstream {
		t.Fatalf("reason = %q, want %q", resp.Reason, checkConnectionReasonUpstream)
	}
	if strings.Contains(resp.Detail, "html") {
		t.Fatalf("detail carried the provider body: %q", resp.Detail)
	}
}

// TestListAzureDeployments_ReportsTheDeploymentIDs proves the Azure-shaped
// types call the deployments endpoint with the api-key header, and report the
// DEPLOYMENT id — the name a caller addresses — falling back to the model name
// only when a deployment carries none.
//
// Lister-level for the reason the open_ai test above states.
func TestListAzureDeployments_ReportsTheDeploymentIDs(t *testing.T) {
	fp := newFakeListProvider(http.StatusOK,
		`{"data":[{"id":"prod-gpt4o","model":"gpt-4o"},{"id":"","model":"text-embedding-3-large"}]}`)
	defer fp.Close()

	ids, err := listAzureDeployments(context.Background(), fp.Client(), checkConnectionRequest{
		Type: "azure_open_ai", APIBase: fp.URL, APIKey: "azure-key", APIVersion: "2024-02-01",
	})
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if !strings.Contains(fp.lastPath, "/openai/deployments") {
		t.Fatalf("path = %q, want the deployments listing", fp.lastPath)
	}
	if !strings.Contains(fp.lastPath, "api-version=2024-02-01") {
		t.Fatalf("path = %q, want the supplied api-version", fp.lastPath)
	}
	if fp.lastKey != "azure-key" {
		t.Fatalf("api-key header = %q, want the supplied key (Azure does not take a bearer)", fp.lastKey)
	}
	if fp.lastAuth != "" {
		t.Fatalf("Authorization = %q, want none for an Azure-shaped credential", fp.lastAuth)
	}
	want := []string{"prod-gpt4o", "text-embedding-3-large"}
	if len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}

// TestListAzureDeployments_DefaultsTheAPIVersion proves a credential with no
// api_version still lists, at the version the checker uses.
func TestListAzureDeployments_DefaultsTheAPIVersion(t *testing.T) {
	fp := newFakeListProvider(http.StatusOK, `{"data":[{"id":"prod-gpt4o"}]}`)
	defer fp.Close()

	if _, err := listAzureDeployments(context.Background(), fp.Client(), checkConnectionRequest{
		Type: "ai_dial", APIBase: fp.URL, APIKey: "dial-key",
	}); err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if !strings.Contains(fp.lastPath, "api-version="+defaultAzureAPIVersion) {
		t.Fatalf("path = %q, want the default api-version", fp.lastPath)
	}
}

// TestListProviderModels_OllamaListsTags proves the self-hosted class reads
// its own native listing.
func TestListProviderModels_OllamaListsTags(t *testing.T) {
	fp := newFakeListProvider(http.StatusOK,
		`{"models":[{"name":"llama3:latest","model":"llama3:latest"},{"name":"","model":"qwen2:7b"}]}`)
	defer fp.Close()

	h := newCheckConnectionHandler(fakeEgressPolicy{allow: true, configured: true})
	resp := doListProviderModels(t, h, checkConnectionRequest{Type: "ollama", APIBase: fp.URL})

	if !resp.Success {
		t.Fatalf("success = false (%q)", resp.Reason)
	}
	if !strings.HasPrefix(fp.lastPath, "/api/tags") {
		t.Fatalf("path = %q, want /api/tags", fp.lastPath)
	}
	if len(resp.Models) != 2 || resp.Models[0] != "llama3:latest" || resp.Models[1] != "qwen2:7b" {
		t.Fatalf("models = %v", resp.Models)
	}
}

// TestListBedrockFoundationModels_SignsAndReadsTheSummaries proves the Bedrock
// listing is the SIGNED control-plane call, and that its modelId values are
// what comes back.
func TestListBedrockFoundationModels_SignsAndReadsTheSummaries(t *testing.T) {
	fp := newFakeCloudListProvider(http.StatusOK, `{"modelSummaries":[
		{"modelId":"anthropic.claude-3-5-sonnet-20240620-v1:0","modelName":"Claude 3.5 Sonnet"},
		{"modelId":"amazon.titan-embed-text-v2:0"}
	]}`)
	defer fp.Close()
	client, dialer := fakeCloudListClient(t, fp)

	ids, err := listBedrockFoundationModels(context.Background(), client, checkConnectionRequest{
		Type:               "amazon_bedrock",
		AWSAccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		AWSSecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		AWSRegionName:      "us-east-1",
	})
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if got := dialer.dialledHosts(); len(got) != 1 || got[0] != "bedrock.us-east-1.amazonaws.com" {
		t.Fatalf("dialled %v, want the region's control-plane host", got)
	}
	if !strings.HasPrefix(fp.lastAuth, "AWS4-HMAC-SHA256 ") {
		t.Fatalf("Authorization = %q, want a SigV4 signature", fp.lastAuth)
	}
	if len(ids) != 2 || ids[0] != "anthropic.claude-3-5-sonnet-20240620-v1:0" || ids[1] != "amazon.titan-embed-text-v2:0" {
		t.Fatalf("ids = %v", ids)
	}
}

// TestListVertexPublisherModels_WalksEveryPublisherAndPage proves the Vertex
// listing mints a token, walks the publishers this gateway can dispatch for,
// follows one page token, reduces each resource path to the id a caller
// addresses, and treats a publisher a location does not carry as absent rather
// than as a broken credential.
func TestListVertexPublisherModels_WalksEveryPublisherAndPage(t *testing.T) {
	fp := newFakeCloudRouter(func(path, query string) (int, string) {
		switch {
		case strings.Contains(path, "/publishers/google/models"):
			if strings.Contains(query, "pageToken=more") {
				return http.StatusOK, `{"publisherModels":[{"name":"publishers/google/models/text-embedding-004"}]}`
			}
			return http.StatusOK,
				`{"publisherModels":[{"name":"publishers/google/models/gemini-1.5-pro"}],"nextPageToken":"more"}`
		case strings.Contains(path, "/publishers/anthropic/models"):
			return http.StatusOK,
				`{"publisherModels":[{"name":"publishers/anthropic/models/claude-3-5-sonnet"}]}`
		default:
			// mistralai is not carried in this location, which Vertex answers
			// as 404. It is not a verdict about the credential.
			return http.StatusNotFound, `{"error":{"message":"not found"}}`
		}
	})
	defer fp.Close()
	client, dialer := fakeCloudListClient(t, fp)

	sa := newTestServiceAccountJSON(t, "https://oauth2.googleapis.com/token")
	ids, err := listVertexPublisherModels(context.Background(), client, checkConnectionRequest{
		Type:              "vertex_ai",
		VertexProject:     "elitea-test-project",
		VertexLocation:    "us-central1",
		VertexCredentials: jsonTextField(sa),
	})
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}

	// Every hop is one of the two hosts the checker's dialTargets names, which
	// is what makes the egress gate in front of this route sufficient.
	for _, host := range dialer.dialledHosts() {
		if host != "oauth2.googleapis.com" && host != "us-central1-aiplatform.googleapis.com" {
			t.Fatalf("dialled %q, which the egress gate never saw", host)
		}
	}
	if !strings.Contains(fp.lastAuth, "ya29.fake-access-token") {
		t.Fatalf("Authorization = %q, want the minted token", fp.lastAuth)
	}

	want := []string{"gemini-1.5-pro", "text-embedding-004", "claude-3-5-sonnet"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v (google's two pages, then anthropic; mistralai is absent here)", ids, want)
	}
	for index := range want {
		if ids[index] != want[index] {
			t.Fatalf("ids = %v, want %v", ids, want)
		}
	}
}

// TestListVertexPublisherModels_GooglesRefusalIsReported proves the primary
// publisher's failure is NOT skipped the way an absent optional publisher is.
// A credential Google refuses must be reported as refused, not as a location
// that happens to carry no models.
func TestListVertexPublisherModels_GooglesRefusalIsReported(t *testing.T) {
	fp := newFakeCloudRouter(func(string, string) (int, string) {
		return http.StatusForbidden, `{"error":{"message":"permission denied"}}`
	})
	defer fp.Close()
	client, _ := fakeCloudListClient(t, fp)

	sa := newTestServiceAccountJSON(t, "https://oauth2.googleapis.com/token")
	ids, err := listVertexPublisherModels(context.Background(), client, checkConnectionRequest{
		Type:              "vertex_ai",
		VertexProject:     "elitea-test-project",
		VertexLocation:    "us-central1",
		VertexCredentials: jsonTextField(sa),
	})
	if err == nil {
		t.Fatalf("a refused listing answered with ids %v", ids)
	}
	if reason, _ := classifyListProviderModelsError(err); reason != checkConnectionReasonUnauth {
		t.Fatalf("reason = %q, want %q", reason, checkConnectionReasonUnauth)
	}
}

// TestBoundProviderModelIDs covers the three bounds and the de-duplication.
func TestBoundProviderModelIDs(t *testing.T) {
	t.Run("drops empty, oversized and control-character ids", func(t *testing.T) {
		got, truncated := boundProviderModelIDs([]string{
			" gpt-4o ", "", "   ", strings.Repeat("x", listProviderModelsMaxIDLength+1), "bad\x00id", "ok",
		})
		if truncated {
			t.Fatal("truncated = true below the cap")
		}
		want := []string{"gpt-4o", "ok"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("de-duplicates without re-ordering", func(t *testing.T) {
		got, _ := boundProviderModelIDs([]string{"b", "a", "b", "c", "a"})
		want := []string{"b", "a", "c"}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v (the provider's order, kept)", got, want)
			}
		}
	})

	t.Run("caps and says so", func(t *testing.T) {
		ids := make([]string, listProviderModelsCap+10)
		for i := range ids {
			ids[i] = fmt.Sprintf("model-%d", i)
		}
		got, truncated := boundProviderModelIDs(ids)
		if len(got) != listProviderModelsCap {
			t.Fatalf("len = %d, want the cap %d", len(got), listProviderModelsCap)
		}
		if !truncated {
			t.Fatal("truncated = false at the cap: a short list would read as the whole catalogue")
		}
	})
}

// TestListProviderModels_CapIsAppliedEndToEnd proves the cap survives the
// handler, not only the helper.
func TestListProviderModels_CapIsAppliedEndToEnd(t *testing.T) {
	entries := make([]string, 0, listProviderModelsCap+5)
	for i := 0; i < listProviderModelsCap+5; i++ {
		entries = append(entries, fmt.Sprintf(`{"name":"model-%d"}`, i))
	}
	fp := newFakeListProvider(http.StatusOK, `{"models":[`+strings.Join(entries, ",")+`]}`)
	defer fp.Close()

	h := newCheckConnectionHandler(fakeEgressPolicy{allow: true, configured: true})
	resp := doListProviderModels(t, h, checkConnectionRequest{Type: "ollama", APIBase: fp.URL})

	if !resp.Success {
		t.Fatalf("success = false (%q)", resp.Reason)
	}
	if len(resp.Models) != listProviderModelsCap {
		t.Fatalf("models = %d, want the cap %d", len(resp.Models), listProviderModelsCap)
	}
	if !resp.Truncated {
		t.Fatal("truncated = false for a capped listing")
	}
}

// TestVertexPublisherModelID covers the resource-path reduction directly.
func TestVertexPublisherModelID(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"publishers/google/models/gemini-1.5-pro", "gemini-1.5-pro"},
		{"publishers/google/models/gemini-1.5-pro/", "gemini-1.5-pro"},
		{"gemini-1.5-pro", "gemini-1.5-pro"},
		{"  publishers/meta/models/llama-3.1  ", "llama-3.1"},
		{"", ""},
	} {
		if got := vertexPublisherModelID(tc.in); got != tc.want {
			t.Fatalf("vertexPublisherModelID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- cloud fakes ----------------------------------------------------------

// fakeCloudListProvider is the cloud counterpart of fakeListProvider: it
// answers the OAuth token exchange and then one fixed listing body. It stands
// behind fakeCloudDialer (checkconnection_cloud_test.go), so the listers keep
// deriving their own production URLs.
type fakeCloudListProvider struct {
	*httptest.Server
	lastAuth string
	answer   func(path, query string) (int, string)
}

func newFakeCloudListProvider(status int, body string) *fakeCloudListProvider {
	return newFakeCloudRouter(func(string, string) (int, string) { return status, body })
}

// newFakeCloudRouter answers per request, so a test can give one publisher a
// listing, another a page token and a third a 404.
func newFakeCloudRouter(answer func(path, query string) (int, string)) *fakeCloudListProvider {
	fp := &fakeCloudListProvider{answer: answer}
	fp.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"ya29.fake-access-token","token_type":"Bearer","expires_in":3600}`))
			return
		}
		fp.lastAuth = r.Header.Get("Authorization")
		status, body := fp.answer(r.URL.Path, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	return fp
}

func fakeCloudListClient(t *testing.T, fp *fakeCloudListProvider) (*http.Client, *fakeCloudDialer) {
	t.Helper()
	base, err := url.Parse(fp.URL)
	if err != nil {
		t.Fatalf("parse fake provider url: %v", err)
	}
	dialer := &fakeCloudDialer{base: base, next: fp.Client().Transport}
	return &http.Client{Transport: dialer}, dialer
}
