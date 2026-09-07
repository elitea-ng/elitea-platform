package toolkits

// The toolkit list's redaction rule, compared with what the served schemas say
// is a secret.
//
// redactSettings drops a settings field by matching its NAME against six
// substrings. The SDK marks a secret field with `secret: true` or
// `format: "password"` in the schema it publishes. Those two lists are written
// by different people in different repositories and nothing joined them: a
// schema-declared secret whose name does not contain "secret", "token",
// "password", "credential", "api_key" or "apikey" is served in clear to every
// member of the project, in the paged list every toolkit screen loads first.
//
// This test is the join. It reads the pinned catalogue snapshot as DATA — it
// cannot import internal/runtimecomposition, which imports this package — so it
// stays in the internal test package where redactSettings is reachable.
//
// A new secret-bearing toolkit type in the SDK snapshot arrives here with no
// edit to this file. When one arrives whose field name the rule does not catch,
// this fails and names it.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

type catalogueSnapshotForRedaction struct {
	Entries []struct {
		Type     string `json:"type"`
		Settings struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"settings"`
	} `json:"entries"`
}

// secretDeclaringProperty is the subset of a property schema that says
// "secret". `anyOf` is read too: the SDK wraps an optional secret as
// `anyOf: [{type: string, secret: true}, {type: null}]`, and reading only the
// top level would miss every optional credential.
type secretDeclaringProperty struct {
	Secret bool                      `json:"secret"`
	Format string                    `json:"format"`
	AnyOf  []secretDeclaringProperty `json:"anyOf"`
}

func (p secretDeclaringProperty) declaresSecret() bool {
	if p.Secret || p.Format == "password" {
		return true
	}
	for _, branch := range p.AnyOf {
		if branch.declaresSecret() {
			return true
		}
	}
	return false
}

func TestEverySchemaDeclaredSecretIsRedactedFromTheToolkitList(t *testing.T) {
	t.Parallel()

	snapshot := readCatalogueSnapshotForRedaction(t)

	declared := 0
	var unredacted []string
	for _, entry := range snapshot.Entries {
		names := make([]string, 0, len(entry.Settings.Properties))
		for name := range entry.Settings.Properties {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			var property secretDeclaringProperty
			if err := json.Unmarshal(entry.Settings.Properties[name], &property); err != nil {
				// A property that is not an object cannot declare a secret.
				continue
			}
			if !property.declaresSecret() {
				continue
			}
			declared++
			redacted := redactSettings(map[string]any{name: "must-not-leak"}).(map[string]any)
			if _, present := redacted[name]; present {
				unredacted = append(unredacted, entry.Type+"."+name)
			}
		}
	}

	// The count is asserted so a snapshot that stopped carrying `secret` at all
	// — a regeneration that dropped the annotation — cannot pass this test by
	// having nothing to check. Eleven properties carry it at SDK b5113a1.
	if declared == 0 {
		t.Fatal("the pinned catalogue snapshot declares no secret property at all;" +
			" either the snapshot lost its `secret` annotations or this test is reading" +
			" the wrong file, and both make this gate measure nothing")
	}
	if len(unredacted) > 0 {
		t.Errorf("%d of %d schema-declared secret field(s) survive redactSettings"+
			" and are served in clear by the toolkit list: %v\n"+
			"Add the name to isSensitiveSettingKey, or record here why the schema's"+
			" own `secret` flag does not apply.", len(unredacted), declared, unredacted)
	}
}

func readCatalogueSnapshotForRedaction(t *testing.T) catalogueSnapshotForRedaction {
	t.Helper()
	path := filepath.Join(
		repositoryRootForRedactionTest(t), "services", "elitea-main", "internal",
		"runtimecomposition", "current_toolkit_catalogue_snapshot.json",
	)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the pinned toolkit catalogue snapshot: %v", err)
	}
	var snapshot catalogueSnapshotForRedaction
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatalf("decode the pinned toolkit catalogue snapshot: %v", err)
	}
	if len(snapshot.Entries) == 0 {
		t.Fatal("the pinned toolkit catalogue snapshot holds no entries")
	}
	return snapshot
}

func repositoryRootForRedactionTest(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for depth := 0; depth < 8; depth++ {
		if _, err := os.Stat(filepath.Join(directory, "go.work")); err == nil {
			return directory
		}
		directory = filepath.Dir(directory)
	}
	t.Fatal("repository root not found from the test working directory")
	return ""
}
