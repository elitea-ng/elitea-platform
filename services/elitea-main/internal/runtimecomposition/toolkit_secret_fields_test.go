package runtimecomposition

import (
	"encoding/json"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
)

// TestPinnedToolkitSnapshotSecretFieldsAreClassifiedSecret sweeps the SDK
// toolkit schema snapshot for the audit #705 asks for.
//
// scripts/contract/sync_toolkit_schema_snapshot.py projects the SDK's `secret`
// annotation into current_toolkit_schema_snapshot.json (ANNOTATION_FIELDS).
// That annotation is the SDK's own statement that a settings field holds a
// credential, so it is the ORACLE for the name rule in
// internal/api/v2/toolkits/secret_settings_key.go.
//
// The snapshot is not an INPUT to that rule. At SDK revision b5113a1 it marks
// 11 fields across 52 toolkit types, and it names neither github's
// `access_token` nor jira's `password`; a catalogue-only redaction would leak
// both. The rule stays name-based, and this test fails when a regenerated
// snapshot adds a secret field the names miss.
func TestPinnedToolkitSnapshotSecretFieldsAreClassifiedSecret(t *testing.T) {
	t.Parallel()

	var document struct {
		Entries []struct {
			Type       string                    `json:"type"`
			Properties map[string]map[string]any `json:"properties"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(pinnedCurrentToolkitSchemaSnapshotJSON, &document); err != nil {
		t.Fatalf("decode pinned toolkit schema snapshot: %v", err)
	}
	if len(document.Entries) == 0 {
		t.Fatal("the pinned snapshot declares no toolkit types, so this sweep proves nothing")
	}

	declared := 0
	for _, entry := range document.Entries {
		for field, annotations := range entry.Properties {
			if secret, _ := annotations["secret"].(bool); !secret {
				continue
			}
			declared++
			if !toolkits.IsSecretSettingKey(field) {
				t.Errorf(
					"toolkit %q settings field %q is declared secret by the SDK, "+
						"but toolkits.IsSecretSettingKey says it is not — the "+
						"toolkit read would return its value",
					entry.Type, field,
				)
			}
		}
	}
	if declared == 0 {
		t.Fatal(
			"no settings field in the pinned snapshot carries the SDK `secret` " +
				"annotation — the sweep read the wrong key and would pass on any rule",
		)
	}
}

// TestPinnedToolkitSnapshotKeepsTokenBudgetsReadable is the other half of the
// sweep: it names the settings keys a client must be able to READ back, and
// fails if the rule classifies one of them as a secret.
//
// `max_tokens` and `toolkit_configuration_max_tokens` were measured stripped
// on a running stack (#705). The DeepWiki chat and generation paths read the
// second one and fall back to 4096 when it is absent, so a project configured
// for 32000 ran at 4096 with nothing on the screen to say so.
func TestPinnedToolkitSnapshotKeepsTokenBudgetsReadable(t *testing.T) {
	t.Parallel()

	for _, field := range []string{
		"max_tokens",
		"max_output_tokens",
		"max_total_tokens",
		"token_limit",
		"toolkit_configuration_max_tokens",
	} {
		if toolkits.IsSecretSettingKey(field) {
			t.Errorf("settings field %q is classified secret; it is a budget, not a credential", field)
		}
	}
}
