package run_test

// The engine's index is scoped by project (engine migration 0005), and the
// project the engine scopes it by is the one THIS host stamps from
// authenticated context: the verified identity of the hop, else (a host
// with no identity secret) the facade-written llm_settings.organization. A
// caller's own _elitea_project_id never reaches the engine.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki/run"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

const identitySecret = "test-identity-secret"

// askResult is a sidecar stream that answers one ask.
func askResult() []string {
	encoded, _ := json.Marshal(map[string]any{"result": map[string]any{"success": true, "answer": "42"}})
	return []string{string(encoded)}
}

// sentProject is the _elitea_project_id of the sidecar's only request.
func sentProject(t *testing.T, f *fakeSidecar) (any, map[string]any) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 1 {
		t.Fatalf("the engine got %d requests, not one", len(f.requests))
	}
	arguments, _ := f.requests[0]["arguments"].(map[string]any)
	value, present := arguments[run.ProjectArgument]
	if !present {
		return nil, arguments
	}
	return value, arguments
}

// forgedAsk is an ask that carries a caller-supplied project in every place
// a request can carry a key, and a repo_identifier_override. The host must
// replace the project with the authenticated one.
func forgedAsk(organization string) map[string]any {
	return map[string]any{
		"configuration": map[string]any{"parameters": map[string]any{
			"llm_settings":      map[string]any{"api_base": "http://elitea-main:8080/llm/v1", "api_key": "minted", "organization": organization},
			run.ProjectArgument: "999",
		}},
		"parameters": map[string]any{
			"question":                 "What does it do?",
			"repo_identifier_override": "acme/widgets:main",
			run.ProjectArgument:        "999",
		},
	}
}

// A request through the whole SPI server: the gate strips the caller's
// identity headers, verifies the facade's signature, and the engine gets
// the SIGNED project — not the body's, not llm_settings'.
func TestTheHostStampsTheVerifiedProjectOverACallersOwn(t *testing.T) {
	sidecar := newFakeSidecar(t, askResult(), 0)
	settings, err := spi.SettingsFromEnv("ELITEA_DEEPWIKI_", func(key string) (string, bool) {
		switch key {
		case "ELITEA_DEEPWIKI_ENGINE_SOCKET":
			return sidecar.socket, true
		case "ELITEA_DEEPWIKI_IDENTITY_SECRET":
			return identitySecret, true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := run.NewEngineRunner(settings)
	if !runner.VerifiedIdentity {
		t.Fatal("a host with an identity secret must read the project from the verified identity")
	}
	server, err := spi.NewServer(settings, deepwiki.App(runner), nil)
	if err != nil {
		t.Fatal(err)
	}
	server.Start(t.Context())
	t.Cleanup(server.Stop)

	encoded, _ := json.Marshal(forgedAsk("90200"))
	request := httptest.NewRequest(http.MethodPost, "/tools/Wikis/ask/invoke", bytes.NewReader(encoded))
	spi.SignHeaders(request.Header, spi.Identity{ProjectID: "17", UserID: "5"}, []byte(identitySecret))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	var accepted map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &accepted)
	id, _ := accepted["invocation_id"].(string)
	if id == "" {
		t.Fatalf("not accepted: %d %s", recorder.Code, recorder.Body.String())
	}
	body := pollUntilTerminal(t, server, "/tools/Wikis/ask/invocations/"+id)
	if body["status"] != "Completed" {
		t.Fatalf("%v", body)
	}
	project, arguments := sentProject(t, sidecar)
	if project != "17" {
		t.Fatalf("the engine was told project %v, not the signed 17: %v", project, arguments)
	}
	// The override still travels; it can only name a wiki of project 17.
	if arguments["repo_identifier_override"] != "acme/widgets:main" {
		t.Fatalf("override %v", arguments["repo_identifier_override"])
	}

	// A forged, UNSIGNED project header is stripped, and with no valid
	// signature on a verifying host the index tool is refused before the
	// engine is called.
	forged := httptest.NewRequest(http.MethodPost, "/tools/Wikis/ask/invoke", bytes.NewReader(encoded))
	forged.Header.Set(spi.HeaderProjectID, "999")
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, forged)
	_ = json.Unmarshal(recorder.Body.Bytes(), &accepted)
	id, _ = accepted["invocation_id"].(string)
	body = pollUntilTerminal(t, server, "/tools/Wikis/ask/invocations/"+id)
	if body["status"] != "Error" || body["error_category"] != "invalid_input" {
		t.Fatalf("an unsigned hop reached the index: %v", body)
	}
	sidecar.mu.Lock()
	calls := len(sidecar.requests)
	sidecar.mu.Unlock()
	if calls != 1 {
		t.Fatalf("the unsigned hop reached the engine (%d calls)", calls)
	}
}

func pollUntilTerminal(t *testing.T, server *spi.Server, path string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		raw, _ := io.ReadAll(recorder.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if status, _ := body["status"].(string); status == "Completed" || status == "Error" {
			return body
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never terminated", path)
	return nil
}

// A host with no identity secret has no verified identity; the project is
// then the facade-written llm_settings.organization, and a caller's own
// _elitea_project_id is still overwritten.
func TestWithoutAnIdentitySecretTheFacadesOrganizationIsTheProject(t *testing.T) {
	sidecar := newFakeSidecar(t, askResult(), 0)
	runner := engineRunner(sidecar, &fakeArtifactClient{})
	if runner.VerifiedIdentity {
		t.Fatal("no identity secret is configured")
	}
	if _, err := invokeFamily(t, runner, spi.Family{Name: "main"}, "ask", forgedAsk("90200")); err != nil {
		t.Fatal(err)
	}
	if project, arguments := sentProject(t, sidecar); project != "90200" {
		t.Fatalf("the engine was told project %v, not the facade's 90200: %v", project, arguments)
	}
}

// Every index tool is refused, before the engine is called, when no
// project can be trusted.
func TestAnIndexToolWithoutATrustedProjectNeverReachesTheEngine(t *testing.T) {
	for _, tool := range []string{"generate_wiki", "ask", "deep_research"} {
		for name, request := range map[string]map[string]any{
			"no organization":        forgedAsk(""),
			"a malformed one":        forgedAsk("1 OR 1=1"),
			"a non-positive one":     forgedAsk("0"),
			"beyond a 32-bit id":     forgedAsk("4294967296"),
			"no llm_settings at all": fixtureRequest("GO", nil),
		} {
			sidecar := newFakeSidecar(t, askResult(), 0)
			_, err := invokeFamily(t, engineRunner(sidecar, &fakeArtifactClient{}), spi.Family{Name: "main"}, tool, request)
			if err == nil || !strings.Contains(err.Error(), "project") {
				t.Fatalf("%s, %s: %v", tool, name, err)
			}
			sidecar.mu.Lock()
			calls := len(sidecar.requests)
			sidecar.mu.Unlock()
			if calls != 0 {
				t.Fatalf("%s, %s: the engine was called", tool, name)
			}
		}
	}
}

func TestTrustedProject(t *testing.T) {
	withOrganization := func(value any) run.Params {
		return run.Params{"llm_settings": map[string]any{"organization": value}}
	}
	for _, c := range []struct {
		name     string
		identity spi.Identity
		verified bool
		params   run.Params
		want     string
	}{
		{"the verified identity wins", spi.Identity{ProjectID: "17"}, true, withOrganization("90200"), "17"},
		{"an identity is used on an unverifying host too", spi.Identity{ProjectID: "17"}, false, withOrganization("90200"), "17"},
		{"a verifying host never falls back", spi.Identity{}, true, withOrganization("90200"), ""},
		{"an unverifying host reads the facade's organization", spi.Identity{}, false, withOrganization("90200"), "90200"},
		{"a JSON number organization", spi.Identity{}, false, withOrganization(float64(7)), "7"},
		{"a malformed identity", spi.Identity{ProjectID: "x"}, true, nil, ""},
		{"a zero organization", spi.Identity{}, false, withOrganization("0"), ""},
		{"leading zeros are normalised", spi.Identity{ProjectID: "007"}, true, nil, "7"},
	} {
		got, err := run.TrustedProject(c.identity, c.verified, c.params)
		if got != c.want || (c.want == "") != (err != nil) {
			t.Fatalf("%s: %q %v", c.name, got, err)
		}
	}
}
