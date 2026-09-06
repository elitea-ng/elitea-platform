package run_test

// The host's half of "a wiki from an artifact folder": the fifth branch in
// the repo_config extractor, the egress decision that has nothing to decide,
// and the wiki id a folder is named with.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki/run"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

func TestAnArtifactFolderIsTheFifthProvider(t *testing.T) {
	config := run.ExtractRepoConfig(map[string]any{
		"code_toolkit": map[string]any{
			"artifact_configuration": map[string]any{
				"bucket": "Handbook-Bucket", "prefix": "/docs/product/",
			},
			"active_branch": "v3",
		},
	})
	want := map[string]any{
		"provider_type":   "artifact",
		"provider_config": map[string]any{"bucket": "handbook-bucket", "prefix": "docs/product"},
		"repository":      "artifact://handbook-bucket/docs/product",
		"branch":          "v3",
		"project":         nil,
		"is_cloud":        nil,
	}
	if !reflect.DeepEqual(config.Map(), want) {
		t.Fatalf("\n got %v\nwant %v", config.Map(), want)
	}
}

func TestTheArtifactBranchReadsEverySpellingTheSourceArrivesIn(t *testing.T) {
	for name, params := range map[string]map[string]any{
		"the prefixed configuration key": {
			"code_toolkit": map[string]any{
				"toolkit_configuration_artifact_configuration": map[string]any{"bucket": "handbook"},
			},
		},
		"a bare artifact repository string": {
			"code_toolkit": map[string]any{"repository": "artifact://handbook"},
		},
		"the source at the top level": {
			"artifact_configuration": map[string]any{"bucket": "handbook"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			config := run.ExtractRepoConfig(params)
			if config.ProviderType != run.ArtifactProviderType {
				t.Fatalf("provider_type %q", config.ProviderType)
			}
			if config.RepositoryString() != "artifact://handbook" {
				t.Fatalf("repository %q", config.RepositoryString())
			}
			if config.BranchString() != "main" {
				t.Fatalf("branch %q", config.BranchString())
			}
		})
	}
}

func TestAMalformedArtifactSourceFallsBackToTheLegacyChain(t *testing.T) {
	// The branch must not swallow a payload it cannot use: an unusable
	// bucket name is not an artifact source, and the four legacy providers
	// then run exactly as they always did.
	config := run.ExtractRepoConfig(map[string]any{
		"code_toolkit": map[string]any{
			"artifact_configuration": map[string]any{"bucket": "Not_A_Bucket"},
			"github_configuration":   map[string]any{"base_url": "https://api.github.com"},
			"repository":             "acme/notes",
		},
	})
	if config.ProviderType != "github" || config.RepositoryString() != "acme/notes" {
		t.Fatalf("%+v", config)
	}
}

func TestAnArtifactFolderIsNotCheckedAgainstTheGitAllowlist(t *testing.T) {
	// The allowlist is FAIL-CLOSED: unset refuses every clone. A folder
	// passes it because there is no clone — the read goes to elitea-main
	// over this invocation's own callback grant, which carries the project.
	params := map[string]any{
		"code_toolkit": map[string]any{
			"artifact_configuration": map[string]any{"bucket": "handbook"},
		},
	}
	host, err := run.CheckEgress(spi.ParseEgressPolicy(""), params)
	if err != nil {
		t.Fatalf("a folder source was refused by the git allowlist: %v", err)
	}
	if host != "" {
		t.Fatalf("a folder source named a host: %q", host)
	}
	// And the guard is not a blanket pass: a git source with the same empty
	// allowlist is still refused.
	if _, err := run.CheckEgress(spi.ParseEgressPolicy(""), map[string]any{
		"code_toolkit": map[string]any{"github_configuration": map[string]any{"base_url": "https://api.github.com"}},
	}); spi.Classify(err) != spi.CategoryInvalidInput {
		t.Fatalf("the allowlist stopped refusing git sources: %v", err)
	}
}

func TestAFolderIsNamedWithoutItsScheme(t *testing.T) {
	// The wiki id is an object-key prefix and the string the browser matches
	// a manifest on. Left as `artifact://…` the `//` becomes four dashes.
	folder := map[string]any{"repository": "artifact://handbook-bucket/docs/product"}
	if got := run.WikiIDFor(folder, "v3"); got != "handbook-bucket--docs--product--v3" {
		t.Fatalf("wiki id %q", got)
	}
	if got := run.DisplayRepositoryFor(folder); got != "handbook-bucket/docs/product" {
		t.Fatalf("display repository %q", got)
	}
	// A bucket with no folder inside it.
	if got := run.WikiIDFor(map[string]any{"repository": "artifact://handbook"}, ""); got != "handbook--main" {
		t.Fatalf("wiki id %q", got)
	}
	// And a git source is named exactly as it was.
	if got := run.WikiIDFor(map[string]any{"repository": "acme/notes"}, "main"); got != "acme--notes--main" {
		t.Fatalf("wiki id %q", got)
	}
}

func TestTheFixtureGenerationNamesAFolderSourceCorrectly(t *testing.T) {
	// Through the runner, so the egress check, the argument build and the
	// upload all run — the fixture engine is the path DWIKI-018 proves.
	client := &fakeArtifactClient{}
	body, err := invoke(t, fixtureRunner(client, 0), "generate_wiki", map[string]any{
		"configuration": map[string]any{"parameters": map[string]any{
			"artifact_configuration": map[string]any{"bucket": "handbook-bucket", "prefix": "docs"},
			"repository":             "artifact://handbook-bucket/docs",
			"llm_settings":           transport,
		}},
		"parameters": map[string]any{"query": "Document the handbook"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if body["status"] != "Completed" {
		t.Fatalf("%v", body)
	}
	manifest := uploadedManifest(t, client, "handbook-bucket--docs--main")
	if manifest["wiki_id"] != "handbook-bucket--docs--main" {
		t.Fatalf("wiki_id %v", manifest["wiki_id"])
	}
	if manifest["repository"] != "handbook-bucket/docs" {
		t.Fatalf("manifest repository %v", manifest["repository"])
	}
	if manifest["provider_type"] != "artifact" {
		t.Fatalf("manifest provider_type %v", manifest["provider_type"])
	}
	if manifest["wiki_title"] != "docs wiki" {
		t.Fatalf("wiki_title %v", manifest["wiki_title"])
	}
}

func TestTheFixtureGenerationIsUnchangedForAGitSource(t *testing.T) {
	client := &fakeArtifactClient{}
	if _, err := invoke(t, fixtureRunner(client, 0), "generate_wiki",
		fixtureRequest("Document the notes service", transport)); err != nil {
		t.Fatal(err)
	}
	manifest := uploadedManifest(t, client, "acme--e2e-service--main")
	if manifest["repository"] != "acme/e2e-service" || manifest["provider_type"] != "github" {
		t.Fatalf("%v", manifest)
	}
}

// uploadedManifest reads the manifest the generation uploaded under one wiki
// id, failing when nothing landed there.
func uploadedManifest(t *testing.T, client *fakeArtifactClient, wikiID string) map[string]any {
	t.Helper()
	client.mu.Lock()
	defer client.mu.Unlock()
	for _, object := range client.uploads {
		if !strings.HasPrefix(object.name, wikiID+"/wiki_manifest_") {
			continue
		}
		manifest := map[string]any{}
		if err := json.Unmarshal([]byte(object.data), &manifest); err != nil {
			t.Fatal(err)
		}
		return manifest
	}
	t.Fatalf("no manifest was uploaded under %s", wikiID)
	return nil
}
