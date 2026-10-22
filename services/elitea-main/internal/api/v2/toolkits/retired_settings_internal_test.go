package toolkits

import "testing"

// A toolkit saved before the pgvector retirement may still store a
// `pgvector_configuration`. The API never serves it back.
func TestRedactSettingsDropsTheRetiredPgvectorConfiguration(t *testing.T) {
	out, ok := redactSettings(map[string]any{
		"pgvector_configuration": map[string]any{"elitea_title": "elitea-pgvector", "private": false},
		"space":                  "DOCS",
	}).(map[string]any)
	if !ok {
		t.Fatal("redactSettings did not return an object")
	}
	if _, present := out["pgvector_configuration"]; present {
		t.Fatalf("the retired key was served: %#v", out)
	}
	if out["space"] != "DOCS" {
		t.Fatalf("an ordinary setting was lost: %#v", out)
	}
}

// A save that still carries the key, as an older client sends it (usually
// null), is accepted and the key is discarded; a body without settings, or
// with settings that do not hold it, is left as it was.
func TestDropRetiredToolkitSettingsDiscardsOnlyTheRetiredKey(t *testing.T) {
	settings := map[string]any{"pgvector_configuration": nil, "space": "DOCS"}
	body := map[string]any{"type": "confluence", "settings": settings}
	dropRetiredToolkitSettings(body)
	saved := body["settings"].(map[string]any)
	if _, present := saved["pgvector_configuration"]; present || saved["space"] != "DOCS" {
		t.Fatalf("settings = %#v, want only space", saved)
	}
	if _, present := settings["pgvector_configuration"]; !present {
		t.Fatal("the caller's map was written to")
	}

	rename := map[string]any{"name": "renamed"}
	dropRetiredToolkitSettings(rename)
	if _, present := rename["settings"]; present {
		t.Fatalf("a body without settings gained some: %#v", rename)
	}
}
