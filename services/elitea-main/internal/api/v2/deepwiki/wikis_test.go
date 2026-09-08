package deepwiki_test

// The per-toolkit rewrite (wikis.go): which rewrite an invoke gets, and what
// a wikis_toolkit reference expands to.
//
// The regression these hold: ONE rewrite requiring `code_toolkit` served all
// three toolkits, so a `wiki_query` invoke — whose configuration declares no
// toolkit reference at all, by the descriptor — was refused 400 for a body
// the caller had written exactly right.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/deepwiki"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// fakeToolkits is the application-toolkit reader.
type fakeToolkits struct {
	rows map[int32]repos.CurrentToolkit
	// asked records every (project, toolkit) read, so a test can prove the
	// row was read from the CALLER's project and not from anywhere else.
	asked [][2]int32
}

func (f *fakeToolkits) Get(_ context.Context, projectID, toolkitID int32) (repos.CurrentToolkit, error) {
	f.asked = append(f.asked, [2]int32{projectID, toolkitID})
	row, ok := f.rows[toolkitID]
	if !ok {
		return repos.CurrentToolkit{}, repos.ErrCurrentToolkitNotFound
	}
	return row, nil
}

// wikisToolkit is a stored Wikis toolkit row naming code toolkit 42.
func wikisToolkit(id int32, toolkitType string, settings map[string]any) repos.CurrentToolkit {
	return repos.CurrentToolkit{ID: id, Type: toolkitType, Settings: settings}
}

func queryRewriter(t *testing.T, toolkits deepwiki.ToolkitReader, minter deepwiki.CallbackMinter) *deepwiki.InvokeRewriter {
	t.Helper()
	built, err := deepwiki.NewInvokeRewriter(
		testCredentialResolver(t), toolkits, minter, "https://elitea.test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return built
}

func body(t *testing.T, parameters map[string]any, tool map[string]any) *bytes.Reader {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"configuration": map[string]any{"parameters": parameters},
		"parameters":    tool,
	})
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(encoded)
}

func decode(t *testing.T, rewritten []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rewritten, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func parametersOf(t *testing.T, rewritten []byte) map[string]any {
	t.Helper()
	configuration, _ := decode(t, rewritten)["configuration"].(map[string]any)
	parameters, ok := configuration["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("the rewritten body has no configuration.parameters: %s", rewritten)
	}
	return parameters
}

// ---------------------------------------------------------------------------
// which rewrite
// ---------------------------------------------------------------------------

func TestEachToolkitGetsItsOwnRewrite(t *testing.T) {
	rewriter := queryRewriter(t, &fakeToolkits{}, &recordingMinter{})
	for _, tc := range []struct {
		toolkit string
		// same reports whether this toolkit's rewrite is the Wikis default.
		same bool
	}{
		{"Wikis", true}, {"wikis", true}, {"DeepWikiToolkit", true},
		{"wikis_query", false}, {"deepwiki_query", false}, {"DeepwikiQuery", false}, {"deepwiki-query", false},
		{"wiki_query", false}, {"WikiQuery", false}, {"wiki-query", false},
		// An alias this facade does not know falls through to the default:
		// the PROVIDER refuses an unknown toolkit, with the legacy message
		// naming every accepted one.
		{"something-else", true},
	} {
		chosen := rewriter.For(tc.toolkit, "ask")
		if chosen == nil {
			t.Fatalf("%s: no rewrite", tc.toolkit)
		}
		// Compared by BEHAVIOUR, not by function pointer: an empty body
		// tells the three apart, because only the bucket rewrite accepts one.
		_, _, err := chosen(context.Background(), body(t, map[string]any{}, nil), 7, 3)
		refused := err != nil
		if tc.same && !refused {
			t.Errorf("%s got a rewrite that does not require code_toolkit", tc.toolkit)
		}
	}
}

// ---------------------------------------------------------------------------
// wiki_query
// ---------------------------------------------------------------------------

// TestAWikiQueryInvokeNeedsNoToolkitReference is the regression. Its
// configuration declares `llm_model` and `embedding_model` and nothing else
// (the descriptor), and what it reads is the project's artifact bucket.
func TestAWikiQueryInvokeNeedsNoToolkitReference(t *testing.T) {
	minter := &recordingMinter{}
	rewriter := queryRewriter(t, &fakeToolkits{}, minter)
	rewritten, grant, err := rewriter.For("wiki_query", "list_wikis")(
		context.Background(), body(t, map[string]any{"llm_model": "gpt-4o"}, map[string]any{}), 7, 3)
	if err != nil {
		t.Fatalf("a wiki_query invoke was refused: %v", err)
	}
	parameters := parametersOf(t, rewritten)
	settings, ok := parameters["llm_settings"].(map[string]any)
	if !ok {
		t.Fatalf("no llm_settings block: %v", parameters)
	}
	// The bucket is reached with THIS bearer: without it, list_wikis has no
	// artifact transport and answers "artifacts settings not configured".
	if settings["api_key"] != grant.Bearer {
		t.Errorf("the llm_settings block does not carry the minted bearer: %v", settings)
	}
	if settings["model_name"] != "gpt-4o" {
		t.Errorf("the toolkit's llm_model did not reach model_name: %v", settings)
	}
	if _, expanded := parameters["code_toolkit"]; expanded {
		t.Error("a wiki_query invoke was given a code_toolkit it did not ask for")
	}
	if len(minter.minted) != 1 {
		t.Errorf("minted %d grants", len(minter.minted))
	}
}

func TestAClientsOwnLLMSettingsNeverSurvivesAWikiQueryInvoke(t *testing.T) {
	rewriter := queryRewriter(t, &fakeToolkits{}, &recordingMinter{})
	rewritten, grant, err := rewriter.For("wiki_query", "delete_wiki")(context.Background(),
		body(t,
			map[string]any{"llm_settings": map[string]any{"api_key": "attacker", "api_base": "http://evil"}},
			map[string]any{"llm_settings": map[string]any{"api_key": "attacker-tool"}, "wiki_id": "x"}),
		7, 3)
	if err != nil {
		t.Fatal(err)
	}
	settings := parametersOf(t, rewritten)["llm_settings"].(map[string]any)
	if settings["api_key"] != grant.Bearer || strings.Contains(settings["api_base"].(string), "evil") {
		t.Fatalf("a client's own llm_settings reached the provider: %v", settings)
	}
	// And the tool half's block is lifted out too (#727): a provider reading
	// `parameters.llm_settings` would otherwise get the caller's.
	tool, _ := decode(t, rewritten)["parameters"].(map[string]any)
	if lifted, present := tool["llm_settings"].(map[string]any); present && lifted["api_key"] == "attacker-tool" {
		t.Fatalf("the tool's own llm_settings survived: %v", tool)
	}
}

// ---------------------------------------------------------------------------
// wikis_query
// ---------------------------------------------------------------------------

func TestAWikisQueryReferenceExpandsTheReferencedToolkitsOwnCodeToolkit(t *testing.T) {
	toolkits := &fakeToolkits{rows: map[int32]repos.CurrentToolkit{
		9001: wikisToolkit(9001, "wikis_Wikis", map[string]any{
			"code_toolkit": json.Number("42"), "repository": "octocat/hello", "active_branch": "trunk"}),
	}}
	rewriter := queryRewriter(t, toolkits, &recordingMinter{})
	rewritten, _, err := rewriter.For("wikis_query", "ask")(context.Background(),
		body(t, map[string]any{"wikis_toolkit": 9001}, map[string]any{"question": "?"}), 7, 3)
	if err != nil {
		t.Fatalf("a wikis_query invoke was refused: %v", err)
	}
	expanded, ok := parametersOf(t, rewritten)["wikis_toolkit"].(map[string]any)
	if !ok {
		t.Fatalf("wikis_toolkit did not expand: %v", parametersOf(t, rewritten))
	}
	// THE SHAPE THE PROVIDER MERGES. TransformQueryRequest merges this
	// object's keys into configuration.parameters, so `code_toolkit` is
	// what has to be in it; anything else and the provider clones nothing
	// and says nothing.
	code, ok := expanded["code_toolkit"].(map[string]any)
	if !ok {
		t.Fatalf("the expansion carries no code_toolkit: %v", expanded)
	}
	if _, ok := code["github_configuration"].(map[string]any); !ok {
		t.Fatalf("the code toolkit did not expand to material: %v", code)
	}
	// The repository and branch come from the ROW, not the body.
	if code["repository"] != "octocat/hello" || code["active_branch"] != "trunk" {
		t.Errorf("the referenced toolkit's own repository did not travel: %v", code)
	}
	// Read from the CALLER's project, and only the referenced toolkit.
	if len(toolkits.asked) != 1 || toolkits.asked[0] != [2]int32{7, 9001} {
		t.Errorf("read %v", toolkits.asked)
	}
}

// TestTheCodeToolkitComesFromTheRowNotTheBody is the property that stops a
// query toolkit being a way to expand ANY configuration in the project.
func TestTheCodeToolkitComesFromTheRowNotTheBody(t *testing.T) {
	toolkits := &fakeToolkits{rows: map[int32]repos.CurrentToolkit{
		9001: wikisToolkit(9001, "wikis_Wikis", map[string]any{"code_toolkit": json.Number("42")}),
	}}
	rewriter := queryRewriter(t, toolkits, &recordingMinter{})
	// The body names a DIFFERENT code toolkit, and a repository of its own.
	rewritten, _, err := rewriter.For("wikis_query", "ask")(context.Background(),
		body(t, map[string]any{
			"wikis_toolkit": 9001, "code_toolkit": 99, "repository": "attacker/repo",
		}, nil), 7, 3)
	if err != nil {
		t.Fatal(err)
	}
	parameters := parametersOf(t, rewritten)
	// The client's own code_toolkit integer is left exactly as it was —
	// un-expanded, so the provider sees a bare id and refuses it — while the
	// expansion went into wikis_toolkit.
	if parameters["code_toolkit"] != float64(99) {
		t.Errorf("the body's own code_toolkit was touched: %v", parameters["code_toolkit"])
	}
	code := parameters["wikis_toolkit"].(map[string]any)["code_toolkit"].(map[string]any)
	if repository, present := code["repository"]; present && repository == "attacker/repo" {
		t.Errorf("the caller re-pointed the wiki at their own repository: %v", code)
	}
}

func TestTheLegacyToolkitSpellingIsAcceptedAndThenRemoved(t *testing.T) {
	toolkits := &fakeToolkits{rows: map[int32]repos.CurrentToolkit{
		9001: wikisToolkit(9001, "deepwiki_Deepwiki", map[string]any{
			"toolkit_configuration_code_toolkit": json.Number("42")}),
	}}
	rewriter := queryRewriter(t, toolkits, &recordingMinter{})
	rewritten, _, err := rewriter.For("deepwiki_query", "ask")(context.Background(),
		body(t, map[string]any{"deepwiki_toolkit": 9001}, nil), 7, 3)
	if err != nil {
		t.Fatalf("the pre-rename spelling was refused: %v", err)
	}
	parameters := parametersOf(t, rewritten)
	if _, expanded := parameters["wikis_toolkit"].(map[string]any); !expanded {
		t.Fatalf("the expansion did not land under the modern name: %v", parameters)
	}
	// The bare id is REMOVED. Left behind, it is a reference the caller
	// still controls sitting beside material the facade chose.
	if _, survived := parameters["deepwiki_toolkit"]; survived {
		t.Errorf("the bare legacy id survived: %v", parameters["deepwiki_toolkit"])
	}
}

func TestAWikisQueryReferenceIsRefusedForEveryReasonACallerCanFix(t *testing.T) {
	toolkits := &fakeToolkits{rows: map[int32]repos.CurrentToolkit{
		9001: wikisToolkit(9001, "wikis_Wikis", map[string]any{"code_toolkit": json.Number("42")}),
		9002: wikisToolkit(9002, "github", map[string]any{"code_toolkit": json.Number("42")}),
		9003: wikisToolkit(9003, "wikis_Wikis", map[string]any{}),
		9004: wikisToolkit(9004, "wikis_Wikis", map[string]any{"code_toolkit": json.Number("77")}),
	}}
	minter := &recordingMinter{}
	rewriter := queryRewriter(t, toolkits, minter)
	for _, tc := range []struct {
		name       string
		parameters map[string]any
		is         error
	}{
		{"no reference", map[string]any{}, deepwiki.ErrInvokeRejected},
		{"an expanded object a client pushed", map[string]any{
			"wikis_toolkit": map[string]any{"code_toolkit": map[string]any{"github_configuration": map[string]any{"access_token": "theirs"}}},
		}, deepwiki.ErrInvokeRejected},
		{"a toolkit the project does not have", map[string]any{"wikis_toolkit": 4242}, deepwiki.ErrWikisToolkitNotResolvable},
		{"a toolkit that is not a Wikis toolkit", map[string]any{"wikis_toolkit": 9002}, deepwiki.ErrWikisToolkitNotResolvable},
		{"a Wikis toolkit naming no code toolkit", map[string]any{"wikis_toolkit": 9003}, deepwiki.ErrWikisToolkitNotResolvable},
		{"a code toolkit that is not a repository configuration", map[string]any{"wikis_toolkit": 9004}, deepwiki.ErrToolkitNotResolvable},
	} {
		_, _, err := rewriter.For("wikis_query", "ask")(context.Background(), body(t, tc.parameters, nil), 7, 3)
		if !errors.Is(err, tc.is) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.is)
		}
	}
	// NO TOKEN WAS MINTED for any of them. Resolution is the step that
	// refuses, and minting before it leaves a live, project-bound bearer
	// behind for work that will never run.
	if len(minter.minted) != 0 {
		t.Errorf("minted %d grants for refused invocations", len(minter.minted))
	}
}

func TestADeploymentThatCannotReadToolkitsSaysSoRatherThanFailingToBoot(t *testing.T) {
	rewriter := queryRewriter(t, nil, &recordingMinter{})
	// Wikis and wiki_query still work.
	if _, _, err := rewriter.For("wiki_query", "list_wikis")(
		context.Background(), body(t, map[string]any{}, nil), 7, 3); err != nil {
		t.Fatalf("wiki_query was refused without a toolkit reader: %v", err)
	}
	// wikis_query is refused, as unavailable rather than as the caller's fault.
	_, _, err := rewriter.For("wikis_query", "ask")(
		context.Background(), body(t, map[string]any{"wikis_toolkit": 9001}, nil), 7, 3)
	// Unavailable, not the caller's fault: invokeError maps that to 503,
	// which is what tells an operator to look at their own composition
	// rather than telling the user their request was wrong.
	if !errors.Is(err, deepwiki.ErrCredentialsUnavailable) {
		t.Fatalf("%v", err)
	}
}

// TestARefusedHostIsNeverDecryptedThroughAQueryToolkit carries the ordering
// claim across the second read: the wikis_query path reaches the vault
// through the SAME resolver, so the allowlist still runs before any decrypt.
func TestARefusedHostIsNeverDecryptedThroughAQueryToolkit(t *testing.T) {
	resolver, unsecreter := testResolver(t, map[int32]configurationapp.CurrentConfiguration{
		42: githubToolkit(42, 7, "https://ghe.attacker.example"),
	}, "github.com,api.github.com")
	built, err := deepwiki.NewInvokeRewriter(resolver, &fakeToolkits{
		rows: map[int32]repos.CurrentToolkit{
			9001: wikisToolkit(9001, "wikis_Wikis", map[string]any{"code_toolkit": json.Number("42")}),
		},
	}, &recordingMinter{}, "https://elitea.test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := built.For("wikis_query", "ask")(
		context.Background(), body(t, map[string]any{"wikis_toolkit": 9001}, nil), 7, 3,
	); !errors.Is(err, deepwiki.ErrEgressRefused) {
		t.Fatalf("a host off the allowlist was accepted: %v", err)
	}
	if unsecreter.opens != 0 {
		t.Fatalf("the vault was opened %d time(s) for a host that was then refused", unsecreter.opens)
	}
}
