package authcomposition

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authsvc"
)

func TestMaterializeLoadsExactPurposeSeparatedSnapshot(t *testing.T) {
	config := writeMaterialFixture(t)
	material, err := materialize(config)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(material.attemptKey, bytes.Repeat([]byte{0xa5}, minAttemptKeyBytes)) ||
		string(material.patSigningKey) != "päss-signing-key\n" || material.formProvider == nil {
		t.Fatalf("unexpected materialized snapshot: attempt=%x PAT=%q provider=%v",
			material.attemptKey,
			material.patSigningKey,
			material.formProvider,
		)
	}
	files, err := config.MaterialFiles()
	if err != nil || len(files) != 3 {
		t.Fatalf("material files = %d, %v; want the attempt key, the PAT key and the Form users", len(files), err)
	}

	attemptKey := material.attemptKey
	patKey := material.patSigningKey
	material.destroy()
	if material.attemptKey != nil || material.patSigningKey != nil ||
		material.formProvider != nil || !allZero(attemptKey) || !allZero(patKey) {
		t.Fatalf("destroy did not clear material: %+v", material)
	}
}

func TestMaterializeRejectsAliasedOrReusedPurposes(t *testing.T) {
	t.Run("hard-linked identity", func(t *testing.T) {
		config := writeMaterialFixture(t)
		if err := os.Remove(config.Credentials.PATSigningKeyFile); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(config.Credentials.AttemptKeyFile, config.Credentials.PATSigningKeyFile); err != nil {
			t.Fatal(err)
		}
		assertInvalidMaterial(t, config)
	})

	t.Run("equal raw content", func(t *testing.T) {
		config := writeMaterialFixture(t)
		attemptKey, err := os.ReadFile(config.Credentials.AttemptKeyFile)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(config.Credentials.PATSigningKeyFile, attemptKey, 0o600); err != nil {
			t.Fatal(err)
		}
		assertInvalidMaterial(t, config)
	})
}

func TestMaterializeRejectsInvalidFilesWithoutLeakingContents(t *testing.T) {
	tests := map[string]func(Config) error{
		"short attempt key": func(config Config) error {
			return os.WriteFile(config.Credentials.AttemptKeyFile, bytes.Repeat([]byte("a"), minAttemptKeyBytes-1), 0o600)
		},
		"long attempt key": func(config Config) error {
			return os.WriteFile(config.Credentials.AttemptKeyFile, bytes.Repeat([]byte("a"), maxAttemptKeyBytes+1), 0o600)
		},
		"PAT NUL": func(config Config) error {
			return os.WriteFile(config.Credentials.PATSigningKeyFile, []byte("do-not-leak\x00key"), 0o600)
		},
		"invalid Form JSON": func(config Config) error {
			return os.WriteFile(config.Provider.Form.UsersJSONFile, []byte(`{"users":[{"login":"admin","password":"do-not-leak"}],"unknown":true}`), 0o600)
		},
		"broad private permissions": func(config Config) error {
			return os.Chmod(config.Credentials.AttemptKeyFile, 0o640)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			config := writeMaterialFixture(t)
			if err := mutate(config); err != nil {
				t.Fatal(err)
			}
			_, err := materialize(config)
			if !errors.Is(err, ErrInvalidMaterial) {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(err.Error(), "do-not-leak") {
				t.Fatalf("file contents escaped into error: %v", err)
			}
		})
	}
}

func TestMaterializeRejectsInvalidConfigBeforeReading(t *testing.T) {
	config := parsedValidConfig(t)
	config.SchemaVersion = "invalid"
	if _, err := materialize(config); !errors.Is(err, ErrInvalidMaterial) {
		t.Fatalf("error = %v", err)
	}
}

func TestMaterializeDoesNotExposeDeploymentPath(t *testing.T) {
	config := parsedValidConfig(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config.Credentials.AttemptKeyFile = filepath.Join(root, "sensitive-deployment-path")
	_, err = materialize(config)
	if !errors.Is(err, ErrInvalidMaterial) {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), "sensitive-deployment-path") {
		t.Fatalf("deployment path escaped into error: %v", err)
	}
}

func TestMaterializedPATKeyVerifiesPythonHS512BytesWithoutTrimming(t *testing.T) {
	const (
		secret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		// PyJWT 2.12.1, current-baseline payload and HS512.
		encoded = "eyJhbGciOiJIUzUxMiIsInR5cCI6IkpXVCJ9.eyJ1dWlkIjoiOGNlNGJlNDktMGQxMC00ZjA1LWE2M2YtZDZkNDZmOTlhM2YwIiwiZXhwaXJlcyI6bnVsbH0.mrvHMef_5BEBKHViTDuciP_FOzJrW8VitsbdJmDy6kjwup5JAhCLEYJO4XW7GgwzDL3Nij5d7Lv2gXSIcfKkvA"
	)
	config := writeMaterialFixture(t)
	if err := os.WriteFile(config.Credentials.PATSigningKeyFile, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	material, err := materialize(config)
	if err != nil {
		t.Fatal(err)
	}
	_, err = authsvc.NewLocalValidatorBytes(nil, material.patSigningKey).ValidateToken(context.Background(), encoded)
	if !errors.Is(err, authsvc.ErrTokenValidationUnavailable) {
		t.Fatalf("exact Python-issued token was not signature-compatible: %v", err)
	}
	material.destroy()

	if err := os.WriteFile(config.Credentials.PATSigningKeyFile, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	material, err = materialize(config)
	if err != nil {
		t.Fatal(err)
	}
	defer material.destroy()
	_, err = authsvc.NewLocalValidatorBytes(nil, material.patSigningKey).ValidateToken(context.Background(), encoded)
	if !errors.Is(err, authsvc.ErrTokenRejected) {
		t.Fatalf("terminal newline was silently trimmed from PAT key: %v", err)
	}
}

func writeMaterialFixture(t *testing.T) Config {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := parsedValidConfig(t)
	config.Credentials.AttemptKeyFile = filepath.Join(root, "attempt-key")
	config.Credentials.PATSigningKeyFile = filepath.Join(root, "pat-key")
	config.Provider.Form.UsersJSONFile = filepath.Join(root, "form-users.json")

	files := []struct {
		path string
		raw  []byte
		mode os.FileMode
	}{
		{config.Credentials.AttemptKeyFile, bytes.Repeat([]byte{0xa5}, minAttemptKeyBytes), 0o600},
		{config.Credentials.PATSigningKeyFile, []byte("päss-signing-key\n"), 0o600},
		{config.Provider.Form.UsersJSONFile, []byte(`{"users":[{"login":"admin","password":"correct horse battery staple","attributes":{"email":"admin@example.test"}}]}`), 0o600},
	}
	for _, file := range files {
		if err := os.WriteFile(file.path, file.raw, file.mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(file.path, file.mode); err != nil {
			t.Fatal(err)
		}
	}
	return config
}

func assertInvalidMaterial(t *testing.T, config Config) {
	t.Helper()
	if _, err := materialize(config); !errors.Is(err, ErrInvalidMaterial) {
		t.Fatalf("error = %v", err)
	}
}

func allZero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}
