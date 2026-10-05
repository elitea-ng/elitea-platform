package runtimecomposition

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/security/securefile"
)

func TestDisabledCodeBrokerDoesNotRequireMaterial(t *testing.T) {
	config := CodePlatformConfig{ContentKeysFile: "/missing/unreadable"}
	files, err := config.MaterialFiles()
	if err != nil || len(files) != 0 {
		t.Fatal("disabled broker required material", err)
	}
	keys, err := config.LoadContentKeys()
	if err != nil || keys != nil {
		t.Fatal("disabled broker loaded material", err)
	}
}
func TestEnabledCodeBrokerRequiresPrivateMaterial(t *testing.T) {
	if _, err := (CodePlatformConfig{Enabled: true}).LoadContentKeys(); err == nil {
		t.Fatal("missing file accepted")
	}
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "content-keys.json")
	if err := os.WriteFile(path, []byte(`{"revision":1,"current_key_id":"a","keys":[{"id":"a","key_base64url":"AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	config := CodePlatformConfig{Enabled: true, ContentKeysFile: path}
	files, err := config.MaterialFiles()
	if err != nil || len(files) != 1 || files[0].Permissions != securefile.PrivateMaterial {
		t.Fatal("wrong material profile", err)
	}
	if _, err = config.LoadContentKeys(); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = config.LoadContentKeys(); err == nil {
		t.Fatal("public material accepted as private content keys")
	}
}
