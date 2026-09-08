package run_test

// The `context_paths` rules, read out of the SAME golden fixture the Python
// implementation's tests read (conformance/provider/fixtures/deepwiki/
// context/context_paths.json). Writing the expectations inline in each
// language would let the two drift with both suites green, which is the one
// failure mode a rule about "what a client may make the server read" cannot
// afford.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki/run"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/artifacts"
)

type contextSpec struct {
	Budget struct {
		TotalChars       int `json:"total_chars"`
		PerDocumentChars int `json:"per_document_chars"`
	} `json:"budget"`
	Wiki struct {
		WikiID         string            `json:"wiki_id"`
		RepoConfig     map[string]any    `json:"repo_config"`
		WikiVersionID  string            `json:"wiki_version_id"`
		ManifestKey    string            `json:"manifest_key"`
		Manifest       map[string]any    `json:"manifest"`
		Pages          map[string]string `json:"pages"`
		GeneratedPages map[string]struct {
			Fill  string `json:"fill"`
			Chars int    `json:"chars"`
		} `json:"generated_pages"`
	} `json:"wiki"`
	Cases map[string]struct {
		ContextPaths     []string `json:"context_paths"`
		ContextPathsRaw  *string  `json:"context_paths_raw"`
		Version          *string  `json:"context_wiki_version_id"`
		Question         string   `json:"question"`
		EnhancedQuestion *string  `json:"enhanced_question"`
		RefusalContains  []string `json:"refusal_contains"`
		ReadsExpected    *int     `json:"reads_expected"`
		Expect           struct {
			Sections              *int     `json:"sections"`
			Contains              []string `json:"contains"`
			BodyCharsBeforeMarker *int     `json:"body_chars_before_marker"`
		} `json:"expect"`
	} `json:"cases"`
}

func loadContextSpec(t *testing.T) contextSpec {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtures, "context", "context_paths.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec contextSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	return spec
}

// recordingBucket is the wiki's bucket plus a record of every read. The read
// COUNT is load-bearing: several cases assert that a refused identifier
// never reaches the transport at all, and a resolver that fetched first and
// complained afterwards would still pass a message assertion.
type recordingBucket struct {
	objects map[string][]byte
	reads   []string
}

func newRecordingBucket(spec contextSpec) *recordingBucket {
	bucket := &recordingBucket{objects: map[string][]byte{}}
	manifest, _ := json.Marshal(spec.Wiki.Manifest)
	bucket.objects[spec.Wiki.ManifestKey] = manifest
	for page, body := range spec.Wiki.Pages {
		bucket.objects[spec.Wiki.WikiID+"/"+page] = []byte(body)
	}
	for page, shape := range spec.Wiki.GeneratedPages {
		if strings.HasPrefix(page, "_") {
			continue
		}
		bucket.objects[spec.Wiki.WikiID+"/"+page] = []byte(strings.Repeat(shape.Fill, shape.Chars))
	}
	return bucket
}

func (b *recordingBucket) Upload(_ context.Context, _, _ string, _ []byte) error { return nil }

func (b *recordingBucket) Download(_ context.Context, bucket, key string) ([]byte, error) {
	b.reads = append(b.reads, key)
	body, ok := b.objects[key]
	if !ok {
		return nil, fmt.Errorf("%s/%s does not exist", bucket, key)
	}
	return body, nil
}

func (b *recordingBucket) List(context.Context, string, string) ([]artifacts.Object, error) {
	return nil, nil
}

func (b *recordingBucket) DeleteBatch(context.Context, string, []string) ([]string, []artifacts.Failure, error) {
	return nil, nil, nil
}

func contextParams(spec contextSpec, name string) run.Params {
	c := spec.Cases[name]
	params := run.Params{
		"question":     c.Question,
		"llm_settings": map[string]any{"api_base": "https://elitea.example/llm/v1", "api_key": "k"},
		// The wiki id is DERIVED, from the same toolkit settings the rest of
		// the invocation derives it from — never sent by the client. That is
		// what stops a selection naming another project's wiki, so the test
		// has to go through the real derivation rather than assert one.
		"code_toolkit": map[string]any{
			"github_configuration": map[string]any{},
			"repository":           spec.Wiki.RepoConfig["repository"],
			"active_branch":        spec.Wiki.RepoConfig["branch"],
		},
	}
	if c.ContextPathsRaw != nil {
		params[run.ContextPathsParam] = *c.ContextPathsRaw
	} else {
		selection := make([]any, 0, len(c.ContextPaths))
		for _, page := range c.ContextPaths {
			selection = append(selection, page)
		}
		params[run.ContextPathsParam] = selection
	}
	if c.Version != nil {
		params[run.ContextVersionParam] = *c.Version
	}
	return params
}

func TestContextPathsMatchTheGoldenFixture(t *testing.T) {
	spec := loadContextSpec(t)
	if run.TotalBudgetChars != spec.Budget.TotalChars || run.PerDocumentBudgetChars != spec.Budget.PerDocumentChars {
		t.Fatalf("budgets drifted from the fixture: %d/%d", run.TotalBudgetChars, run.PerDocumentBudgetChars)
	}

	names := make([]string, 0, len(spec.Cases))
	for name := range spec.Cases {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		c := spec.Cases[name]
		t.Run(name, func(t *testing.T) {
			bucket := newRecordingBucket(spec)
			transport := func(map[string]any) (run.ArtifactClient, error) { return bucket, nil }

			resolved, err := run.ApplyContextPaths(context.Background(), "ask", contextParams(spec, name), transport)

			if len(c.RefusalContains) > 0 {
				if err == nil {
					t.Fatalf("expected a refusal, got %v", resolved["question"])
				}
				for _, needle := range c.RefusalContains {
					if !strings.Contains(err.Error(), needle) {
						t.Fatalf("refusal %q does not contain %q", err, needle)
					}
				}
				// THE SSRF ASSERTION. An id that is not a wiki page of this
				// wiki must be refused BEFORE the transport is touched.
				if c.ReadsExpected != nil && len(bucket.reads) != *c.ReadsExpected {
					t.Fatalf("expected %d reads, got %v", *c.ReadsExpected, bucket.reads)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}

			question, _ := resolved["question"].(string)
			if c.EnhancedQuestion != nil && question != *c.EnhancedQuestion {
				t.Fatalf("prepend shape differs\n got: %q\nwant: %q", question, *c.EnhancedQuestion)
			}
			if c.Expect.Sections != nil && strings.Count(question, "--- source: ") != *c.Expect.Sections {
				t.Fatalf("expected %d sections, got %d", *c.Expect.Sections, strings.Count(question, "--- source: "))
			}
			for _, needle := range c.Expect.Contains {
				if !strings.Contains(question, needle) {
					t.Fatalf("answer does not contain %q:\n%.400s", needle, question)
				}
			}
			if c.Expect.BodyCharsBeforeMarker != nil {
				header := "--- source: wiki_pages/bulk/p1.md ---\n"
				body := strings.SplitN(strings.SplitN(question, header, 2)[1], "[…", 2)[0]
				// The marker's own two leading newlines are not budget.
				if len([]rune(body))-2 != *c.Expect.BodyCharsBeforeMarker {
					t.Fatalf("truncated body is %d characters, want %d", len([]rune(body))-2, *c.Expect.BodyCharsBeforeMarker)
				}
			}
			// Whichever resolver runs first SPENDS the keys, which is the
			// only reason a second one downstream is safe.
			if _, left := resolved[run.ContextPathsParam]; left {
				t.Fatal("context_paths survived resolution; a downstream resolver would prepend twice")
			}
			if _, left := resolved[run.ContextVersionParam]; left {
				t.Fatal("context_wiki_version_id survived resolution")
			}
		})
	}
}

func TestASelectionSentToAToolThatCannotHonourItIsRefused(t *testing.T) {
	// Silently ignoring it would answer ungrounded and look like the
	// feature working.
	spec := loadContextSpec(t)
	bucket := newRecordingBucket(spec)
	transport := func(map[string]any) (run.ArtifactClient, error) { return bucket, nil }
	_, err := run.ApplyContextPaths(context.Background(), "generate_wiki",
		contextParams(spec, "two_pages_are_prepended_in_selection_order"), transport)
	if err == nil || !strings.Contains(err.Error(), "is not supported by generate_wiki") {
		t.Fatalf("got %v", err)
	}
	if len(bucket.reads) != 0 {
		t.Fatalf("reads attempted: %v", bucket.reads)
	}
}

func TestNoTransportIsRefusedRatherThanAnsweredWithoutContext(t *testing.T) {
	spec := loadContextSpec(t)
	_, err := run.ApplyContextPaths(context.Background(), "ask",
		contextParams(spec, "two_pages_are_prepended_in_selection_order"), nil)
	if err == nil || !strings.Contains(err.Error(), "artifacts base_url not configured") {
		t.Fatalf("got %v", err)
	}
}
