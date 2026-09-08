package deepwiki_test

// Credential resolution and the callback token (ADR-0022 decision 6).
//
// The decisive assertion in this file is
// TestARefusedHostIsNeverDecrypted: it counts vault opens, not error strings.
// "Checked before any decrypt" is a claim about ORDER, and the only way to
// hold it is to observe that the decrypt did not happen — an assertion on the
// error message would pass just as well if the vault had been opened and the
// host checked afterwards.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	deepwiki "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/deepwiki"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

// ---------------------------------------------------------------------------
// doubles
// ---------------------------------------------------------------------------

type fakeConfigurations struct {
	rows  map[int32]configurationapp.CurrentConfiguration
	calls int
}

func (f *fakeConfigurations) Get(
	_ context.Context, projectID, configurationID int32,
) (configurationapp.CurrentConfiguration, error) {
	f.calls++
	row, ok := f.rows[configurationID]
	if !ok || row.ProjectID != projectID {
		return configurationapp.CurrentConfiguration{},
			configurationapp.ErrCurrentConfigurationNotFound
	}
	return row, nil
}

// countingUnsecreter stands in for the vault. `opens` is the assertion
// surface for the check-before-decrypt order.
type countingUnsecreter struct {
	opens int
	err   error
}

func (u *countingUnsecreter) Unsecret(
	_ context.Context, _ int32, data map[string]any,
) (map[string]any, error) {
	u.opens++
	if u.err != nil {
		return nil, u.err
	}
	expanded := map[string]any{}
	for key, value := range data {
		if text, ok := value.(string); ok && strings.HasPrefix(text, "{{secret.") {
			expanded[key] = "decrypted:" + strings.TrimSuffix(
				strings.TrimPrefix(text, "{{secret."), "}}")
			continue
		}
		expanded[key] = value
	}
	return expanded, nil
}

type recordingMinter struct {
	mu      sync.Mutex
	minted  []string
	revoked []string
	err     error
	counter int
}

func (m *recordingMinter) Mint(
	_ context.Context, ownerID, projectID int64, name string, lifetime time.Duration,
) (deepwiki.CallbackGrant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return deepwiki.CallbackGrant{}, m.err
	}
	m.counter++
	uuid := "token-" + string(rune('a'+m.counter-1))
	m.minted = append(m.minted, uuid)
	return deepwiki.CallbackGrant{
		Bearer:  "bearer-for-" + uuid,
		Expires: time.Now().Add(lifetime),
		UUID:    uuid,
	}, nil
}

func (m *recordingMinter) Revoke(_ context.Context, _ int64, tokenUUID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.revoked = append(m.revoked, tokenUUID)
	return nil
}

func (m *recordingMinter) snapshot() (minted, revoked []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.minted...), append([]string(nil), m.revoked...)
}

// githubToolkit is a repository configuration whose token is still a
// placeholder — the shape a real row has before the vault is opened.
func githubToolkit(id, projectID int32, baseURL string) configurationapp.CurrentConfiguration {
	return configurationapp.CurrentConfiguration{
		ID:        id,
		ProjectID: projectID,
		Type:      "github",
		Data: map[string]any{
			"base_url":     baseURL,
			"access_token": "{{secret.GH_TOKEN}}",
			"username":     "octocat",
		},
	}
}

func testResolver(
	t *testing.T,
	rows map[int32]configurationapp.CurrentConfiguration,
	allowlist string,
) (*deepwiki.CredentialResolver, *countingUnsecreter) {
	t.Helper()
	unsecreter := &countingUnsecreter{}
	built, err := deepwiki.NewCredentialResolver(
		&fakeConfigurations{rows: rows}, unsecreter,
		deepwiki.ParseGitEgressPolicy(allowlist))
	if err != nil {
		t.Fatal(err)
	}
	return built, unsecreter
}

// testCredentialResolver is the default used by route tests: one github
// toolkit, id 42, in project 7, on an allowed host.
func testCredentialResolver(t *testing.T) *deepwiki.CredentialResolver {
	t.Helper()
	built, _ := testResolver(t, map[int32]configurationapp.CurrentConfiguration{
		42: githubToolkit(42, 7, "https://api.github.com"),
	}, "github.com,api.github.com")
	return built
}

// ---------------------------------------------------------------------------
// the ordering claim
// ---------------------------------------------------------------------------

func TestARefusedHostIsNeverDecrypted(t *testing.T) {
	rows := map[int32]configurationapp.CurrentConfiguration{
		42: githubToolkit(42, 7, "https://ghe.attacker.example"),
	}
	resolver, unsecreter := testResolver(t, rows, "github.com,api.github.com")

	_, err := resolver.Resolve(context.Background(), 7, 42, "octocat/hello", "main")
	if !errors.Is(err, deepwiki.ErrEgressRefused) {
		t.Fatalf("a host off the allowlist was accepted: %v", err)
	}
	// The whole claim, and the only assertion that can hold it.
	if unsecreter.opens != 0 {
		t.Fatalf("the vault was opened %d time(s) for a host that was then refused",
			unsecreter.opens)
	}
}

func TestAnUnsetAllowlistRefusesEverything(t *testing.T) {
	rows := map[int32]configurationapp.CurrentConfiguration{
		42: githubToolkit(42, 7, "https://api.github.com"),
	}
	resolver, unsecreter := testResolver(t, rows, "")

	_, err := resolver.Resolve(context.Background(), 7, 42, "octocat/hello", "main")
	if !errors.Is(err, deepwiki.ErrEgressRefused) {
		t.Fatalf("an unset allowlist allowed a clone: %v", err)
	}
	if !strings.Contains(err.Error(), deepwiki.GitAllowlistEnv) {
		t.Fatalf("the refusal does not name the variable to set: %v", err)
	}
	if unsecreter.opens != 0 {
		t.Fatal("the vault was opened despite an unset allowlist")
	}
}

// A host assembled from a secret has no safe order: it cannot be checked
// before the decrypt that would reveal it. Refused rather than checked against
// a string containing braces, which would "pass" only by never matching.
func TestAHostBuiltFromASecretIsRefusedOutright(t *testing.T) {
	rows := map[int32]configurationapp.CurrentConfiguration{
		42: {
			ID: 42, ProjectID: 7, Type: "github",
			Data: map[string]any{"base_url": "{{secret.GH_HOST}}"},
		},
	}
	resolver, unsecreter := testResolver(t, rows, "*")

	if _, err := resolver.Resolve(context.Background(), 7, 42, "o/r", "main"); !errors.Is(
		err, deepwiki.ErrEgressRefused) {
		t.Fatalf("a secret-derived host was accepted: %v", err)
	}
	if unsecreter.opens != 0 {
		t.Fatal("the vault was opened for a secret-derived host")
	}
}

// ---------------------------------------------------------------------------
// expansion
// ---------------------------------------------------------------------------

func TestTheExpansionMatchesWhatTheEngineReads(t *testing.T) {
	rows := map[int32]configurationapp.CurrentConfiguration{
		42: githubToolkit(42, 7, "https://api.github.com"),
	}
	resolver, unsecreter := testResolver(t, rows, "api.github.com")

	resolved, err := resolver.Resolve(context.Background(), 7, 42, "octocat/hello", "trunk")
	if err != nil {
		t.Fatal(err)
	}
	if unsecreter.opens != 1 {
		t.Fatalf("the vault was opened %d times", unsecreter.opens)
	}

	// The engine's repo_config normaliser reads the provider dict under
	// `{provider}_configuration`, and repository/branch at the TOOLKIT level.
	// Putting them in the wrong half clones nothing, with no error.
	provider, ok := resolved.Payload["github_configuration"].(map[string]any)
	if !ok {
		t.Fatalf("no github_configuration in %v", resolved.Payload)
	}
	if provider["access_token"] != "decrypted:GH_TOKEN" {
		t.Fatalf("the token was not expanded: %v", provider["access_token"])
	}
	// Field names are the platform's own and the engine's, unchanged. A
	// rename here would be a translation layer the engine never agreed to.
	if provider["base_url"] != "https://api.github.com" || provider["username"] != "octocat" {
		t.Fatalf("provider fields were rewritten: %v", provider)
	}
	if resolved.Payload["repository"] != "octocat/hello" ||
		resolved.Payload["active_branch"] != "trunk" {
		t.Fatalf("repository/branch are not at toolkit level: %v", resolved.Payload)
	}
	if resolved.Host != "api.github.com" {
		t.Fatalf("host %q", resolved.Host)
	}
}

// Each provider has its own host field and its own payload key. Reading the
// wrong one would check an empty host — which the allowlist refuses, so the
// failure would look like a misconfigured allowlist rather than a bug here.
func TestEachProviderIsReadThroughItsOwnFields(t *testing.T) {
	cases := []struct {
		configurationType string
		hostField         string
		host              string
		engineKey         string
	}{
		{"github", "base_url", "https://ghe.example.com", "github_configuration"},
		{"gitlab", "url", "https://gitlab.example.com", "gitlab_configuration"},
		{"bitbucket", "url", "https://bitbucket.example.com", "bitbucket_configuration"},
		{"ado", "organization_url", "https://ado.example.com", "ado_configuration"},
	}
	for _, testCase := range cases {
		rows := map[int32]configurationapp.CurrentConfiguration{
			42: {
				ID: 42, ProjectID: 7, Type: testCase.configurationType,
				Data: map[string]any{testCase.hostField: testCase.host},
			},
		}
		resolver, _ := testResolver(t, rows, "*.example.com")
		resolved, err := resolver.Resolve(context.Background(), 7, 42, "o/r", "main")
		if err != nil {
			t.Fatalf("%s: %v", testCase.configurationType, err)
		}
		if _, ok := resolved.Payload[testCase.engineKey]; !ok {
			t.Fatalf("%s produced %v, want a %s key",
				testCase.configurationType, resolved.Payload, testCase.engineKey)
		}
	}
}

// A configuration that exists but is not a repository toolkit — a Jira
// credential, say — is refused rather than pushed as an unknown provider.
func TestANonRepositoryConfigurationIsRefused(t *testing.T) {
	rows := map[int32]configurationapp.CurrentConfiguration{
		42: {ID: 42, ProjectID: 7, Type: "jira", Data: map[string]any{"url": "https://jira.example.com"}},
	}
	resolver, unsecreter := testResolver(t, rows, "*")

	if _, err := resolver.Resolve(context.Background(), 7, 42, "o/r", "main"); !errors.Is(
		err, deepwiki.ErrToolkitNotResolvable) {
		t.Fatalf("a jira configuration was accepted as a code toolkit: %v", err)
	}
	if unsecreter.opens != 0 {
		t.Fatal("a non-repository configuration was decrypted")
	}
}

// The project in the path decides which project's configurations are
// reachable. A configuration id from another project must not resolve.
func TestAConfigurationFromAnotherProjectDoesNotResolve(t *testing.T) {
	rows := map[int32]configurationapp.CurrentConfiguration{
		42: githubToolkit(42, 9, "https://api.github.com"), // project 9
	}
	resolver, unsecreter := testResolver(t, rows, "*")

	if _, err := resolver.Resolve(context.Background(), 7, 42, "o/r", "main"); !errors.Is(
		err, deepwiki.ErrToolkitNotResolvable) {
		t.Fatalf("project 7 reached project 9's configuration: %v", err)
	}
	if unsecreter.opens != 0 {
		t.Fatal("another project's vault was opened")
	}
}

// ---------------------------------------------------------------------------
// the allowlist's matching rules — parity with the provider's egress.py
// ---------------------------------------------------------------------------

func TestTheAllowlistMatchesTheProvidersRules(t *testing.T) {
	cases := []struct {
		allowlist string
		host      string
		allowed   bool
	}{
		{"github.com", "github.com", true},
		{"github.com", "GitHub.com", true},
		{"github.com", "api.github.com", false},
		{"*.github.com", "api.github.com", true},
		// A bare domain is NOT covered by its own wildcard, and a wildcard
		// covers DIRECT subdomains only. Both are the provider's rules, and a
		// disagreement here would start an invocation that then fails.
		{"*.github.com", "github.com", false},
		{"*.github.com", "a.b.github.com", false},
		{"github.com", "github.com:8443", true},
		{"*", "anything.example", true},
		{"", "github.com", false},
		{"github.com, gitlab.com", "gitlab.com", true},
		{"github.com\ngitlab.com", "gitlab.com", true},
	}
	for _, testCase := range cases {
		policy := deepwiki.ParseGitEgressPolicy(testCase.allowlist)
		err := policy.Allow(testCase.host)
		if (err == nil) != testCase.allowed {
			t.Fatalf("allowlist %q host %q: allowed=%v (%v)",
				testCase.allowlist, testCase.host, err == nil, err)
		}
	}
}

// ---------------------------------------------------------------------------
// the rewrite
// ---------------------------------------------------------------------------

func rewriter(t *testing.T, minter deepwiki.CallbackMinter) *deepwiki.InvokeRewriter {
	t.Helper()
	built, err := deepwiki.NewInvokeRewriter(
		testCredentialResolver(t), nil, minter, "https://elitea.test/", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return built
}

func TestTheRewriteReplacesReferencesWithMaterial(t *testing.T) {
	minter := &recordingMinter{}
	body := `{"configuration":{"parameters":{"code_toolkit":42,"llm_model":"gpt-4o",` +
		`"repository":"octocat/hello","active_branch":"main","max_tokens":64000}},` +
		`"parameters":{"query":"Document the pipeline"}}`

	rewritten, grant, err := rewriter(t, minter).Rewrite(
		context.Background(), strings.NewReader(body), 7, 11)
	if err != nil {
		t.Fatal(err)
	}

	var out struct {
		Configuration struct {
			Parameters map[string]any `json:"parameters"`
		} `json:"configuration"`
		Parameters map[string]any `json:"parameters"`
	}
	if err := json.Unmarshal(rewritten, &out); err != nil {
		t.Fatal(err)
	}
	parameters := out.Configuration.Parameters

	toolkit, ok := parameters["code_toolkit"].(map[string]any)
	if !ok {
		t.Fatalf("code_toolkit is still %v", parameters["code_toolkit"])
	}
	if _, ok := toolkit["github_configuration"]; !ok {
		t.Fatalf("no expanded provider config: %v", toolkit)
	}

	settings, ok := parameters["llm_settings"].(map[string]any)
	if !ok {
		t.Fatalf("no llm_settings: %v", parameters)
	}
	// The engine strips `/llm/v1` off api_base to find the artifact API, so
	// this exact suffix is load-bearing rather than cosmetic.
	if settings["api_base"] != "https://elitea.test/llm/v1" {
		t.Fatalf("api_base %v", settings["api_base"])
	}
	if settings["api_key"] != "bearer-for-"+grant.UUID {
		t.Fatalf("api_key is not the minted bearer: %v", settings["api_key"])
	}
	// organization is where the engine reads project_id from.
	if settings["organization"] != "7" {
		t.Fatalf("organization %v, want the path's project", settings["organization"])
	}
	if settings["model_name"] != "gpt-4o" {
		t.Fatalf("model_name %v", settings["model_name"])
	}

	// Everything the facade does not own must survive untouched: the
	// provider's contract is frozen and this is a rewrite, not a re-encoding.
	if parameters["max_tokens"] != float64(64000) {
		t.Fatalf("max_tokens was lost: %v", parameters["max_tokens"])
	}
	if out.Parameters["query"] != "Document the pipeline" {
		t.Fatalf("tool parameters were lost: %v", out.Parameters)
	}
}

// The two credential-bearing fields are replaced, never merged. A client that
// pushes its own is choosing which secret the provider clones and calls back
// with, which is the whole thing this facade exists to decide.
func TestAClientCannotPushItsOwnCredentials(t *testing.T) {
	minter := &recordingMinter{}
	body := `{"configuration":{"parameters":{` +
		`"code_toolkit":42,` +
		`"llm_settings":{"api_base":"https://attacker.example","api_key":"stolen","organization":"999"}` +
		`}}}`

	rewritten, _, err := rewriter(t, minter).Rewrite(
		context.Background(), strings.NewReader(body), 7, 11)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rewritten), "attacker.example") ||
		strings.Contains(string(rewritten), "stolen") {
		t.Fatalf("client-supplied llm_settings survived: %s", rewritten)
	}
	if strings.Contains(string(rewritten), `"organization":"999"`) {
		t.Fatalf("the client chose the billing project: %s", rewritten)
	}
}

// The TOOL-level llm_settings is the other door. The provider merges a tool's
// parameters over the configuration's, so a block there replaced the facade's
// wholesale: the wiki chat, which sends max_tokens and the model that way,
// reached the engine without api_base (DWIKI-014b); an attacker could have
// reached it with their own. Tuning is carried, transport is dropped.
func TestAToolLevelLLMSettingsIsLiftedNotMerged(t *testing.T) {
	minter := &recordingMinter{}
	body := `{"configuration":{"parameters":{"code_toolkit":42,"llm_model":"gpt-4o"}},` +
		`"parameters":{"question":"What does it do?",` +
		`"llm_settings":{"max_tokens":4096,"model_name":"gpt-4o",` +
		`"api_base":"https://attacker.example","api_key":"stolen","organization":"999"}}}`

	rewritten, grant, err := rewriter(t, minter).Rewrite(
		context.Background(), strings.NewReader(body), 7, 11)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rewritten), "attacker.example") ||
		strings.Contains(string(rewritten), "stolen") ||
		strings.Contains(string(rewritten), `"organization":"999"`) {
		t.Fatalf("tool-level llm_settings survived the rewrite: %s", rewritten)
	}
	var out struct {
		Configuration struct {
			Parameters map[string]any `json:"parameters"`
		} `json:"configuration"`
		Parameters map[string]any `json:"parameters"`
	}
	if err := json.Unmarshal(rewritten, &out); err != nil {
		t.Fatal(err)
	}
	if _, present := out.Parameters["llm_settings"]; present {
		t.Fatalf("the tool-level block must be gone, or the host merges it over the facade's: %v", out.Parameters)
	}
	if out.Parameters["question"] != "What does it do?" {
		t.Fatalf("the other tool parameters were lost: %v", out.Parameters)
	}
	settings, _ := out.Configuration.Parameters["llm_settings"].(map[string]any)
	if settings["api_base"] != "https://elitea.test/llm/v1" || settings["api_key"] != "bearer-for-"+grant.UUID {
		t.Fatalf("the facade's transport did not survive: %v", settings)
	}
	if settings["max_tokens"] != float64(4096) {
		t.Fatalf("max_tokens was not carried: %v", settings)
	}
	if settings["model_name"] != "gpt-4o" {
		t.Fatalf("model_name %v", settings["model_name"])
	}
}

// A tool-level llm_settings that is not an object is a malformed request,
// refused rather than silently dropped.
func TestAMalformedToolLevelLLMSettingsIsRefused(t *testing.T) {
	body := `{"configuration":{"parameters":{"code_toolkit":42}},"parameters":{"llm_settings":"nope"}}`
	_, _, err := rewriter(t, &recordingMinter{}).Rewrite(context.Background(), strings.NewReader(body), 7, 11)
	if !errors.Is(err, deepwiki.ErrInvokeRejected) {
		t.Fatalf("want ErrInvokeRejected, got %v", err)
	}
}

// An expanded code_toolkit is a client pushing a credential under the name of
// a reference. Refused, not merged — merging would leave the caller in
// control of which token the provider clones with.
func TestAnExpandedCodeToolkitIsRefused(t *testing.T) {
	minter := &recordingMinter{}
	body := `{"configuration":{"parameters":{"code_toolkit":` +
		`{"github_configuration":{"base_url":"https://api.github.com","access_token":"ghp_attacker"}}}}}`

	_, _, err := rewriter(t, minter).Rewrite(
		context.Background(), strings.NewReader(body), 7, 11)
	if !errors.Is(err, deepwiki.ErrInvokeRejected) {
		t.Fatalf("an expanded code_toolkit was accepted: %v", err)
	}
	// The error must not echo the value: the thing a client wrongly puts in
	// code_toolkit is exactly the thing most likely to be a secret.
	if strings.Contains(err.Error(), "ghp_attacker") {
		t.Fatalf("the error echoed the pushed credential: %v", err)
	}
	if minted, _ := minter.snapshot(); len(minted) != 0 {
		t.Fatalf("a token was minted for a refused body: %v", minted)
	}
}

// Resolution refuses before minting, so a refused invocation leaves no live
// credential behind at all.
func TestNoTokenIsMintedForARefusedResolution(t *testing.T) {
	minter := &recordingMinter{}
	built, err := deepwiki.NewInvokeRewriter(
		func() *deepwiki.CredentialResolver {
			resolver, _ := testResolver(t, map[int32]configurationapp.CurrentConfiguration{
				42: githubToolkit(42, 7, "https://ghe.attacker.example"),
			}, "github.com")
			return resolver
		}(), nil, minter, "https://elitea.test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = built.Rewrite(context.Background(),
		strings.NewReader(`{"configuration":{"parameters":{"code_toolkit":42}}}`), 7, 11)
	if !errors.Is(err, deepwiki.ErrEgressRefused) {
		t.Fatalf("%v", err)
	}
	if minted, _ := minter.snapshot(); len(minted) != 0 {
		t.Fatalf("a callback token was minted for a refused host: %v", minted)
	}
}

func TestAMissingCodeToolkitIsRejectedNotDefaulted(t *testing.T) {
	minter := &recordingMinter{}
	for _, body := range []string{
		`{}`,
		`{"configuration":{}}`,
		`{"configuration":{"parameters":{}}}`,
		`{"configuration":{"parameters":{"code_toolkit":null}}}`,
		`{"configuration":{"parameters":{"code_toolkit":0}}}`,
		`{"configuration":{"parameters":{"code_toolkit":"42"}}}`,
	} {
		if _, _, err := rewriter(t, minter).Rewrite(
			context.Background(), strings.NewReader(body), 7, 11); !errors.Is(
			err, deepwiki.ErrInvokeRejected) {
			t.Fatalf("body %s was accepted: %v", body, err)
		}
	}
	if minted, _ := minter.snapshot(); len(minted) != 0 {
		t.Fatalf("tokens were minted for unusable bodies: %v", minted)
	}
}

// ---------------------------------------------------------------------------
// through the route
// ---------------------------------------------------------------------------

func routeWith(
	t *testing.T,
	cfg deepwiki.Config,
	credentials *deepwiki.CredentialResolver,
	minter deepwiki.CallbackMinter,
) *deepwiki.Route {
	t.Helper()
	built, err := deepwiki.NewRoute(cfg, authConfig(),
		resolver(deepwiki.ReadPermission, deepwiki.GeneratePermission),
		credentials, nil, minter, nil)
	if err != nil {
		t.Fatal(err)
	}
	return built
}

// The provider must receive the rewritten body, not the client's — the whole
// chain, over the real mTLS hop.
func TestTheProviderReceivesTheRewrittenBody(t *testing.T) {
	log, cfg := provider(t)
	minter := &recordingMinter{}
	handler := routeWith(t, cfg, testCredentialResolver(t), minter)

	response := call(t, handler, http.MethodPost,
		"/api/v2/deepwiki/tools/7/Wikis/generate_wiki/invoke",
		`{"configuration":{"parameters":{"code_toolkit":42,"repository":"octocat/hello"}}}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}

	arrived := (*log)[0].body
	if strings.Contains(arrived, `"code_toolkit":42`) {
		t.Fatalf("the provider received the unexpanded reference: %s", arrived)
	}
	if !strings.Contains(arrived, "decrypted:GH_TOKEN") {
		t.Fatalf("the provider received no credential: %s", arrived)
	}
	if !strings.Contains(arrived, "bearer-for-token-a") {
		t.Fatalf("the provider received no callback bearer: %s", arrived)
	}
}

// A provider that refuses the invocation leaves a live bearer behind for work
// that will never run. It expires either way; revoking closes the window for
// the case that fails fastest.
func TestARefusedInvocationRevokesItsCallbackToken(t *testing.T) {
	_, cfg := provider(t)
	// A port nothing listens on: the hop fails, the proxy answers 503.
	cfg.BaseURL = "https://127.0.0.1:1"
	minter := &recordingMinter{}
	handler := routeWith(t, cfg, testCredentialResolver(t), minter)

	response := call(t, handler, http.MethodPost,
		"/api/v2/deepwiki/tools/7/Wikis/generate_wiki/invoke",
		`{"configuration":{"parameters":{"code_toolkit":42}}}`)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", response.Code)
	}

	minted, revoked := minter.snapshot()
	if len(minted) != 1 {
		t.Fatalf("minted %v", minted)
	}
	if len(revoked) != 1 || revoked[0] != minted[0] {
		t.Fatalf("minted %v but revoked %v", minted, revoked)
	}
}

// An accepted invocation keeps its token: the work is running and the provider
// needs the credential to hand back what it produces.
func TestAnAcceptedInvocationKeepsItsCallbackToken(t *testing.T) {
	_, cfg := provider(t)
	minter := &recordingMinter{}
	handler := routeWith(t, cfg, testCredentialResolver(t), minter)

	response := call(t, handler, http.MethodPost,
		"/api/v2/deepwiki/tools/7/Wikis/generate_wiki/invoke",
		`{"configuration":{"parameters":{"code_toolkit":42}}}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	if minted, revoked := minter.snapshot(); len(minted) != 1 || len(revoked) != 0 {
		t.Fatalf("minted %v revoked %v", minted, revoked)
	}
}

// A refused host reaches the caller as 403 with a message they can act on,
// and nothing crosses the hop.
func TestARefusedHostAnswers403WithoutReachingTheProvider(t *testing.T) {
	log, cfg := provider(t)
	credentials, unsecreter := testResolver(t, map[int32]configurationapp.CurrentConfiguration{
		42: githubToolkit(42, 7, "https://ghe.attacker.example"),
	}, "github.com")
	minter := &recordingMinter{}
	handler := routeWith(t, cfg, credentials, minter)

	response := call(t, handler, http.MethodPost,
		"/api/v2/deepwiki/tools/7/Wikis/generate_wiki/invoke",
		`{"configuration":{"parameters":{"code_toolkit":42}}}`)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	if len(*log) != 0 {
		t.Fatal("a refused invocation reached the provider")
	}
	if unsecreter.opens != 0 {
		t.Fatal("a refused invocation opened the vault")
	}
	// The refusal must not disclose the allowlist's contents; that is the
	// operator's configuration, not the caller's business.
	if strings.Contains(response.Body.String(), "github.com") {
		t.Fatalf("the refusal disclosed the allowlist: %s", response.Body.String())
	}
}

// A project id above MaxInt32 must not alias a real project.
//
// The id is narrowed to int32 to read a configuration, and Go truncates
// silently: 4294967301 becomes 5. Without the bound, naming an out-of-range
// project resolves project 5's stored credentials and pushes them to the
// provider — an aliasing bug, not a lossy conversion. CodeQL found the
// conversion; this pins the behaviour.
func TestAnOutOfRangeProjectDoesNotAliasARealOne(t *testing.T) {
	log, cfg := provider(t)
	credentials, unsecreter := testResolver(t, map[int32]configurationapp.CurrentConfiguration{
		42: githubToolkit(42, 5, "https://api.github.com"), // project FIVE
	}, "api.github.com")
	minter := &recordingMinter{}
	handler := routeWith(t, cfg, credentials, minter)

	// 4294967301 truncates to 5 in int32.
	response := call(t, handler, http.MethodPost,
		"/api/v2/deepwiki/tools/4294967301/Wikis/generate_wiki/invoke",
		`{"configuration":{"parameters":{"code_toolkit":42}}}`)

	if response.Code == http.StatusOK {
		t.Fatalf("an out-of-range project was served: %d", response.Code)
	}
	if unsecreter.opens != 0 {
		t.Fatal("an out-of-range project opened project 5's vault")
	}
	if len(*log) != 0 {
		t.Fatal("an out-of-range project reached the provider")
	}
	if minted, _ := minter.snapshot(); len(minted) != 0 {
		t.Fatalf("a token was minted for an out-of-range project: %v", minted)
	}
}

// The rewriter is exported and must not assume its caller bounded the id.
//
// TestAnOutOfRangeProjectDoesNotAliasARealOne covers the route; this covers
// the seam a different caller would come through, which is the reason
// repos.narrowRowID exists in the same shape.
func TestTheRewriterRefusesAnOutOfRangeProjectOnItsOwn(t *testing.T) {
	// The configuration lives in project FIVE, and 4294967301 truncates to 5.
	//
	// That placement is the whole test. With the toolkit in any OTHER project
	// this passes whether or not the bound exists — the resolve simply misses
	// — and the assertion would be measuring nothing. Here, a missing bound
	// RESOLVES: it reads project 5's credentials and mints a token for them.
	credentials, unsecreter := testResolver(t, map[int32]configurationapp.CurrentConfiguration{
		42: githubToolkit(42, 5, "https://api.github.com"),
	}, "api.github.com")
	minter := &recordingMinter{}
	built, err := deepwiki.NewInvokeRewriter(
		credentials, nil, minter, "https://elitea.test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = built.Rewrite(context.Background(),
		strings.NewReader(`{"configuration":{"parameters":{"code_toolkit":42}}}`),
		4294967301, 11)
	if !errors.Is(err, deepwiki.ErrToolkitNotResolvable) {
		t.Fatalf("an out-of-range project was accepted: %v", err)
	}
	if unsecreter.opens != 0 {
		t.Fatal("project 5's vault was opened for project 4294967301")
	}
	if minted, _ := minter.snapshot(); len(minted) != 0 {
		t.Fatalf("a token was minted: %v", minted)
	}
}

// Only the invoke path is rewritten. Poll and cancel carry no body worth
// touching, and a rewrite there would mint a token per poll.
func TestPollingMintsNothing(t *testing.T) {
	_, cfg := provider(t)
	minter := &recordingMinter{}
	handler := routeWith(t, cfg, testCredentialResolver(t), minter)

	call(t, handler, http.MethodGet, "/api/v2/deepwiki/slots/7", "")
	call(t, handler, http.MethodGet,
		"/api/v2/deepwiki/invocations/7/Wikis/generate_wiki/abc", "")
	call(t, handler, http.MethodDelete,
		"/api/v2/deepwiki/invocations/7/Wikis/generate_wiki/abc", "")

	if minted, _ := minter.snapshot(); len(minted) != 0 {
		t.Fatalf("reads minted callback tokens: %v", minted)
	}
}
