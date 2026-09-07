package deepwiki_test

// The facade side of "a wiki from an artifact folder".
//
// The rewrite is what these assert, because the rewrite is where the decision
// is: a body that names a folder must reach the provider with a VALIDATED
// source and no vault opened, a body that names two sources must be refused,
// and a body that names a repository toolkit must behave exactly as it did.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/deepwiki"
)

// forwarded is the toolkit-level configuration the provider would receive.
func forwarded(t *testing.T, rewritten []byte) map[string]any {
	t.Helper()
	var out struct {
		Configuration struct {
			Parameters map[string]any `json:"parameters"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal(rewritten, &out); err != nil {
		t.Fatal(err)
	}
	return out.Configuration.Parameters
}

func TestAFolderSourceReachesTheProviderWithoutOpeningTheVault(t *testing.T) {
	minter := &recordingMinter{}
	resolver := testCredentialResolver(t)
	body := `{"configuration":{"parameters":{` +
		`"artifact_configuration":{"bucket":"Handbook-Bucket","prefix":"/docs/product/"},` +
		`"llm_model":"gpt-4o","active_branch":"v3"}},` +
		`"parameters":{"query":"Document the handbook"}}`

	built, err := deepwiki.NewInvokeRewriter(resolver, nil, minter, "https://elitea.test/", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rewritten, grant, err := built.Rewrite(context.Background(), strings.NewReader(body), 7, 11)
	if err != nil {
		t.Fatal(err)
	}
	parameters := forwarded(t, rewritten)

	// The block is written back CANONICAL: the client's spelling is not what
	// the provider reads.
	block, ok := parameters["artifact_configuration"].(map[string]any)
	if !ok {
		t.Fatalf("artifact_configuration is %v", parameters["artifact_configuration"])
	}
	if block["bucket"] != "handbook-bucket" || block["prefix"] != "docs/product" {
		t.Fatalf("the source was not normalised: %v", block)
	}
	// And the repository is DERIVED, which is what the host's extractor and
	// the engine's materialiser both read the source out of.
	if parameters["repository"] != "artifact://handbook-bucket/docs/product" {
		t.Fatalf("repository %v", parameters["repository"])
	}
	if _, ok := parameters["code_toolkit"]; ok {
		t.Fatalf("a folder source left a code_toolkit behind: %v", parameters)
	}

	// The callback is still minted — it is how the provider reads the folder.
	settings, ok := parameters["llm_settings"].(map[string]any)
	if !ok {
		t.Fatalf("no llm_settings: %v", parameters)
	}
	if settings["api_key"] != "bearer-for-"+grant.UUID {
		t.Fatalf("api_key is not the minted bearer: %v", settings["api_key"])
	}
	// The project comes from the PATH, and it is the only project whose
	// buckets the provider can then read. That is the folder source's whole
	// authorization story.
	if settings["organization"] != "7" {
		t.Fatalf("organization %v", settings["organization"])
	}
}

func TestAFolderSourceOpensNoConfigurationRow(t *testing.T) {
	configurations := &fakeConfigurations{rows: nil}
	unsecreter := &countingUnsecreter{}
	resolver, err := deepwiki.NewCredentialResolver(
		configurations, unsecreter, deepwiki.ParseGitEgressPolicy(""))
	if err != nil {
		t.Fatal(err)
	}
	built, err := deepwiki.NewInvokeRewriter(
		resolver, nil, &recordingMinter{}, "https://elitea.test/", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"configuration":{"parameters":{"artifact_configuration":{"bucket":"handbook"}}}}`
	if _, _, err := built.Rewrite(context.Background(), strings.NewReader(body), 7, 11); err != nil {
		t.Fatal(err)
	}
	// An EMPTY allowlist refuses every git host, and this generation passed:
	// there is no host, so there is nothing for it to refuse.
	if configurations.calls != 0 {
		t.Fatalf("a folder source read %d configuration rows", configurations.calls)
	}
	if unsecreter.opens != 0 {
		t.Fatalf("a folder source opened the vault %d times", unsecreter.opens)
	}
}

func TestABodyNamingTwoSourcesIsRefused(t *testing.T) {
	minter := &recordingMinter{}
	body := `{"configuration":{"parameters":{` +
		`"code_toolkit":42,"artifact_configuration":{"bucket":"handbook"}}}}`
	_, _, err := rewriter(t, minter).Rewrite(
		context.Background(), strings.NewReader(body), 7, 11)
	if !errors.Is(err, deepwiki.ErrToolkitNotResolvable) {
		t.Fatalf("err = %v, want a refusal the caller can fix", err)
	}
	// The refusal happens before the mint: a token issued for work that never
	// happened is a live bearer nobody revokes.
	if minted, _ := minter.snapshot(); len(minted) != 0 {
		t.Fatalf("a refused request minted %v", minted)
	}
}

func TestAMalformedFolderSourceIsRefusedWithItsReason(t *testing.T) {
	for name, block := range map[string]string{
		"a bucket name no project can hold": `{"bucket":"Handbook_Bucket"}`,
		"a bucket that is too short":        `{"bucket":"a"}`,
		"no bucket at all":                  `{"prefix":"docs"}`,
		"a folder that climbs out":          `{"bucket":"handbook","prefix":"../../etc"}`,
		"a folder with an empty segment":    `{"bucket":"handbook","prefix":"a//b"}`,
		"a source that is not an object":    `"handbook"`,
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"configuration":{"parameters":{"artifact_configuration":` + block + `}}}`
			_, _, err := rewriter(t, &recordingMinter{}).Rewrite(
				context.Background(), strings.NewReader(body), 7, 11)
			if !errors.Is(err, deepwiki.ErrToolkitNotResolvable) {
				t.Fatalf("err = %v, want a refusal", err)
			}
		})
	}
}

func TestAnAbsentFolderSourceStillNeedsACodeToolkit(t *testing.T) {
	// The branch must not become a way past the reference requirement: a
	// null or missing block is a git generation, and those still resolve.
	for _, body := range []string{
		`{"configuration":{"parameters":{}}}`,
		`{"configuration":{"parameters":{"artifact_configuration":null}}}`,
	} {
		_, _, err := rewriter(t, &recordingMinter{}).Rewrite(
			context.Background(), strings.NewReader(body), 7, 11)
		if err == nil {
			t.Fatalf("%s was accepted without a source", body)
		}
	}
}

func TestAWikiQueryCannotNameAFolderSource(t *testing.T) {
	// Only the `Wikis` toolkit reads a source. A wiki query reads the wiki
	// bucket, and letting it name a folder would be a second, unowned way to
	// point a toolkit at a bucket.
	minter := &recordingMinter{}
	built := rewriter(t, minter)
	body := `{"configuration":{"parameters":{"artifact_configuration":{"bucket":"handbook"}}}}`
	rewritten, _, err := built.For("wiki_query", "")(
		context.Background(), strings.NewReader(body), 7, 11)
	if err != nil {
		t.Fatal(err)
	}
	parameters := forwarded(t, rewritten)
	if _, ok := parameters["repository"]; ok {
		t.Fatalf("a wiki query derived a repository from a folder: %v", parameters)
	}
}
