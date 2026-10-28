package credentialgrant

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"
)

// The digest is sha256 over the domain, the type, a NUL and the settings'
// sorted compact JSON: operators can compute it from the stored row.
func TestToolkitDigestIsTheDocumentedSha256(t *testing.T) {
	settings := map[string]any{"z": 1.0, "jira_configuration": map[string]any{"private": false, "elitea_title": "jira-prod"}}
	got, err := ToolkitDigest("jira", settings)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte("elitea.credential-governance.toolkit-digest.v1\x00jira\x00" +
		`{"jira_configuration":{"elitea_title":"jira-prod","private":false},"z":1}`))
	if got != want {
		t.Fatalf("digest = %x, want %x", got, want)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(`{"jira_configuration":{"elitea_title":"jira-prod","private":false},"z":1}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if again, _ := ToolkitDigest("jira", decoded); again != got {
		t.Fatal("a re-read row digests differently")
	}
	if other, _ := ToolkitDigest("confluence", settings); other == got {
		t.Fatal("the type is not covered")
	}
	if _, err := ToolkitDigest("", settings); !errors.Is(err, ErrInvalidToolkitDefinition) {
		t.Fatalf("no type: %v", err)
	}
}
