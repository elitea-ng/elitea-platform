package spi_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// fixture reads one of the frozen SPI fixtures under conformance/provider.
func fixture(t *testing.T, parts ...string) map[string]any {
	t.Helper()
	path := filepath.Join(append([]string{"..", "..", "..", "..", "conformance", "provider", "fixtures", "deepwiki"}, parts...)...)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("fixture %s: %v", path, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("fixture %s: %v", path, err)
	}
	return out
}

// The canonical string and the HMAC are the Python shell's, vector for
// vector: these two signatures were produced by security/identity.py over
// the same secret and identity (see the commit that added this test).
func TestIdentitySignaturesMatchThePythonShell(t *testing.T) {
	secret := []byte("shared-with-the-provider")
	v1 := spi.Identity{ProjectID: "1", UserID: "7", TenantID: "acme"}
	if got := v1.Sign(secret); got != "sha256=8e4646aaf0e0b8abeb65ea7c2a18cbfffb5a7bb340241da728e14e71ddede178" {
		t.Fatalf("v1 signature %s", got)
	}
	v2 := spi.Identity{ProjectID: "1", UserID: "7", TenantID: "acme", ExecutionID: "exec-9"}
	if got := v2.Sign(secret); got != "sha256=077da6cbd68f78662269f3a5de8422bd63ffbd0515a54f486dfc559b06c58a9f" {
		t.Fatalf("v2 signature %s", got)
	}
	h := http.Header{}
	spi.SignHeaders(h, v2, secret)
	if !spi.VerifySignature(h, secret) {
		t.Fatal("a signature the signer produced did not verify")
	}
	// An execution id appended AFTER a v1 signature is not accepted.
	h1 := http.Header{}
	spi.SignHeaders(h1, v1, secret)
	h1.Set(spi.HeaderExecutionID, "exec-9")
	if spi.VerifySignature(h1, secret) {
		t.Fatal("a v1 signature verified with an execution id added afterwards")
	}
	h1.Del(spi.HeaderExecutionID)
	if !spi.VerifySignature(h1, secret) {
		t.Fatal("a v1 signature without an execution id was refused")
	}
	h1.Set(spi.HeaderProjectID, "2")
	if spi.VerifySignature(h1, secret) {
		t.Fatal("a changed project id verified")
	}
	if !spi.VerifySignature(http.Header{}, nil) {
		t.Fatal("no secret must mean no verification, not a refusal")
	}
}

// The classifier's precedence is the recorded one: message words first for
// the categories the legacy classifier keyed on text, then the kind.
func TestClassifierMatchesTheRecordedPrecedence(t *testing.T) {
	recorded := fixture(t, "spi", "errors.json")["recorded"].(map[string]any)
	cases := map[string]error{
		"resource_not_found": spi.Failf(spi.KindNotFound, "Wiki not found for repository"),
		"service_busy":       spi.Failf(spi.KindRuntime, "[SERVICE_BUSY] DeepWiki service is busy"),
		"artifact_error":     spi.Failf(spi.KindRuntime, "Failed to download artifact"),
		"out_of_memory":      spi.Failf(spi.KindMemory, "out of memory while embedding"),
		"timeout_error":      spi.Failf(spi.KindRuntime, "Clone timeout after 300s"),
		"inference_failed":   spi.Failf(spi.KindRuntime, "LLM generation failed"),
		"runtime_error":      spi.Failf(spi.KindRuntime, "worker exited with code 1"),
		"invalid_input":      spi.Failf(spi.KindValue, "query must not be empty"),
		"unknown_error":      spi.Failf(spi.KindKey, "llm_settings"),
	}
	if len(cases) != len(recorded) {
		t.Fatalf("the fixture records %d categories, this test covers %d", len(recorded), len(cases))
	}
	for category, err := range cases {
		entry := recorded[category].(map[string]any)
		if got := spi.Classify(err); got != entry["error_category"] {
			t.Errorf("%s: classified as %s", category, got)
		}
		if got := string(spi.KindOf(err)); got != entry["error_type"] {
			t.Errorf("%s: error_type %s, recorded %s", category, got, entry["error_type"])
		}
	}
	// A plain Go error is an unknown_error of the generic kind, and a
	// not-found text wins over any kind — the legacy rule.
	if spi.Classify(errors.New("boom")) != spi.CategoryUnknown {
		t.Error("a plain error was not unknown_error")
	}
	if spi.Classify(spi.Failf(spi.KindValue, "index not found")) != spi.CategoryResourceNotFound {
		t.Error("'not found' in the text did not win over ValueError")
	}
	if spi.Classify(spi.Failf(spi.KindRuntime, "training crashed")) != spi.CategoryTrainingFailed {
		t.Error("a RuntimeError mentioning training was not training_failed")
	}
}

func TestToolErrorCarriesTheContract(t *testing.T) {
	body := spi.ToolError(nil, "invocation_1", "generate_wiki", spi.Failf(spi.KindNotFound, "Wiki not found for repository"))
	if body["status"] != "Error" || body["error_category"] != "resource_not_found" || body["error_type"] != "FileNotFoundError" || body["result_type"] != "String" {
		t.Fatalf("envelope %v", body)
	}
	var objects []map[string]any
	if err := json.Unmarshal([]byte(body["result"].(string)), &objects); err != nil || len(objects) != 1 {
		t.Fatalf("result %v: %v", body["result"], err)
	}
	if objects[0]["object_type"] != "message" || objects[0]["result_target"] != "response" ||
		objects[0]["data"] != "Generate_wiki failed: Wiki not found for repository" {
		t.Fatalf("message object %v", objects[0])
	}
}

func env(pairs map[string]string) spi.Lookup {
	return func(key string) (string, bool) { v, ok := pairs[key]; return v, ok }
}

func TestSettingsAreStrictAndPrefixed(t *testing.T) {
	s, err := spi.SettingsFromEnv("ELITEA_DEEPWIKI_", env(map[string]string{
		"ELITEA_DEEPWIKI_MAX_PARALLEL_WORKERS": "3",
		"DEEPWIKI_JOBS_ENABLED":                "true", // the legacy alias
		"ELITEA_DEEPWIKI_TLS_CERTFILE":         "/c", "ELITEA_DEEPWIKI_TLS_KEYFILE": "/k", "ELITEA_DEEPWIKI_TLS_CA_FILE": "/ca",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if s.MaxParallelWorkers != 3 || !s.JobsEnabled || !s.TerminatesMTLS() || !s.MTLSRequired() || s.InvocationRetentionSeconds != 3600 {
		t.Fatalf("settings %+v", s)
	}
	for name, pairs := range map[string]map[string]string{
		"a non-integer":             {"ELITEA_DEEPWIKI_MAX_CONCURRENT_JOBS": "many"},
		"below the minimum":         {"ELITEA_DEEPWIKI_MAX_PARALLEL_WORKERS": "0"},
		"a non-boolean":             {"ELITEA_DEEPWIKI_SLOTS_MODE": "maybe"},
		"a certificate with no key": {"ELITEA_DEEPWIKI_TLS_CERTFILE": "/c"},
		"a CA with no server cert":  {"ELITEA_DEEPWIKI_TLS_CA_FILE": "/ca"},
	} {
		if _, err := spi.SettingsFromEnv("ELITEA_DEEPWIKI_", env(pairs)); !errors.Is(err, spi.ErrConfig) {
			t.Errorf("%s was accepted: %v", name, err)
		}
	}
	if _, err := spi.SettingsFromEnv("", env(nil)); !errors.Is(err, spi.ErrConfig) {
		t.Error("an empty prefix was accepted")
	}
}

// The callback hop's trust is its own setting once the hop goes through
// platform-edge (ADR-0027): the runtime CA there, the provider CA on the
// listener. Unset, it stays the listener's CA, as before the split.
func TestTheCallbackCAIsItsOwnSettingWithTheOldFallback(t *testing.T) {
	mtls := map[string]string{
		"ELITEA_DEEPWIKI_TLS_CERTFILE": "/c", "ELITEA_DEEPWIKI_TLS_KEYFILE": "/k", "ELITEA_DEEPWIKI_TLS_CA_FILE": "/provider-ca",
	}
	s, err := spi.SettingsFromEnv("ELITEA_DEEPWIKI_", env(mtls))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.CallbackCA(); got != "/provider-ca" {
		t.Fatalf("callback CA without its own setting = %q, want the listener's", got)
	}
	mtls["ELITEA_DEEPWIKI_CALLBACK_CA_FILE"] = "/runtime-ca"
	s, err = spi.SettingsFromEnv("ELITEA_DEEPWIKI_", env(mtls))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.CallbackCA(); got != "/runtime-ca" || s.TLSCAFile != "/provider-ca" {
		t.Fatalf("callback CA = %q, listener CA = %q", got, s.TLSCAFile)
	}
}

func TestToolkitAdmissionRaisesTheTwoLegacyRefusals(t *testing.T) {
	table := spi.Toolkits{
		Families: []spi.Family{
			{Name: "main", Aliases: []string{"Wikis", "wikis"}, Tools: []string{"generate_wiki"}},
			{Name: "query", Aliases: []string{"wikis_query"}, Tools: []string{"ask"}, UnknownToolIsInvalidInput: true, Label: "deepwiki_query"},
		},
		Advertised: []string{"Wikis", "wikis_query"},
	}
	if err := table.Validate(); err != nil {
		t.Fatal(err)
	}
	_, err := table.Resolve("NotAToolkit")
	if spi.Classify(err) != spi.CategoryResourceNotFound || err.Error() != "Unknown toolkit: NotAToolkit. Expected: one of ['Wikis', 'wikis', 'wikis_query']" {
		t.Fatalf("unknown toolkit: %v", err)
	}
	main, _ := table.Resolve("wikis")
	if err := table.Admit(main, "list_wikis"); spi.Classify(err) != spi.CategoryResourceNotFound || err.Error() != "Unknown tool: list_wikis" {
		t.Fatalf("unknown main tool: %v", err)
	}
	query, _ := table.Resolve("wikis_query")
	if err := table.Admit(query, "generate_wiki"); spi.Classify(err) != spi.CategoryInvalidInput ||
		err.Error() != "Tool 'generate_wiki' not available in deepwiki_query toolkit. Available: ask" {
		t.Fatalf("unknown query tool: %v", err)
	}
	for name, broken := range map[string]spi.Toolkits{
		"no families":        {},
		"an alias twice":     {Families: []spi.Family{{Name: "a", Aliases: []string{"x"}, Tools: []string{"t"}}, {Name: "b", Aliases: []string{"x"}, Tools: []string{"t"}}}},
		"advertised unknown": {Families: []spi.Family{{Name: "a", Aliases: []string{"x"}, Tools: []string{"t"}}}, Advertised: []string{"y"}},
		"label missing":      {Families: []spi.Family{{Name: "a", Aliases: []string{"x"}, Tools: []string{"t"}, UnknownToolIsInvalidInput: true}}},
	} {
		if err := broken.Validate(); !errors.Is(err, spi.ErrConfig) {
			t.Errorf("%s was accepted: %v", name, err)
		}
	}
}

func TestEgressPolicyRulesAreTheSharedOnes(t *testing.T) {
	policy := spi.ParseEgressPolicy("github.com, *.github.com GITLAB.example")
	for host, want := range map[string]bool{
		"github.com": true, "api.github.com": true, "a.b.github.com": false, "github.com:443": true,
		"gitlab.example": true, "evil.com": false, "": false, "[::1]": false,
	} {
		if got := policy.Permits(host); got != want {
			t.Errorf("%q: permitted %v, want %v", host, got, want)
		}
	}
	if !spi.ParseEgressPolicy("*").AllowsEverything() || spi.ParseEgressPolicy("*").Check("anything", "clone destination") != nil {
		t.Error("'*' did not opt out")
	}
	if err := spi.ParseEgressPolicy("").Check("github.com", "clone destination"); spi.Classify(err) != spi.CategoryInvalidInput {
		t.Errorf("an empty policy did not refuse as invalid_input: %v", err)
	}
	if spi.HostOf("https://github.com/acme/x.git") != "github.com" || spi.HostOf("git@gitlab.com:acme/x.git") != "gitlab.com" || spi.HostOf("Bitbucket.org") != "bitbucket.org" {
		t.Error("HostOf")
	}
}
