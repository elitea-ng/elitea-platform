package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
)

func TestRunValidatesResolvedFormSnapshotWithoutOutput(t *testing.T) {
	path := writePrivateFormSnapshot(t, `{"users":[{"login":"admin","password":"TEST_ONLY_PASSWORD","email":"admin@example.test","attributes":{"name":"Admin"}}]}`)
	var stderr bytes.Buffer

	if code := run([]string{"-form-users-file", path}, &stderr); code != exitValid || stderr.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func TestRunRejectsInvalidSnapshotsWithoutLeakingInputs(t *testing.T) {
	const secretCanary = "TEST_ONLY_SECRET_CANARY"
	tests := map[string]string{
		"unknown field":    `{"users":[{"login":"admin","password":"` + secretCanary + `","unsupported":"value"}]}`,
		"duplicate member": `{"users":[{"login":"admin","password":"first","password":"` + secretCanary + `"}]}`,
		"duplicate login":  `{"users":[{"login":"admin","password":"first"},{"login":"admin","password":"` + secretCanary + `"}]}`,
		"malformed":        `{"users":[{"login":"admin","password":"` + secretCanary + `"}`,
		"invalid claim":    `{"users":[{"login":"admin","password":"` + secretCanary + `","attributes":{"email":42}}]}`,
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			path := writePrivateFormSnapshot(t, contents)
			var stderr bytes.Buffer
			if code := run([]string{"--form-users-file=" + path}, &stderr); code != exitInvalid {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
			if stderr.String() != invalidConfigText || strings.Contains(stderr.String(), secretCanary) ||
				strings.Contains(stderr.String(), path) {
				t.Fatalf("unsafe stderr=%q", stderr.String())
			}
		})
	}
}

func TestRunRejectsUnsafeFileAndArgumentsWithGenericErrors(t *testing.T) {
	t.Run("permissions", func(t *testing.T) {
		path := writePrivateFormSnapshot(t, `{"users":[]}`)
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		if code := run([]string{"-form-users-file", path}, &stderr); code != exitInvalid || stderr.String() != invalidConfigText {
			t.Fatalf("code=%d stderr=%q", code, stderr.String())
		}
	})

	t.Run("symlink", func(t *testing.T) {
		path := writePrivateFormSnapshot(t, `{"users":[]}`)
		link := filepath.Join(filepath.Dir(path), "form-users-link.json")
		if err := os.Symlink(path, link); err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		if code := run([]string{"-form-users-file", link}, &stderr); code != exitInvalid || stderr.String() != invalidConfigText {
			t.Fatalf("code=%d stderr=%q", code, stderr.String())
		}
	})

	t.Run("missing", func(t *testing.T) {
		root := canonicalTempDir(t)
		path := filepath.Join(root, "TEST_ONLY_SECRET_MISSING.json")
		var stderr bytes.Buffer
		if code := run([]string{"-form-users-file", path}, &stderr); code != exitInvalid ||
			stderr.String() != invalidConfigText || strings.Contains(stderr.String(), path) {
			t.Fatalf("code=%d stderr=%q", code, stderr.String())
		}
	})

	t.Run("invalid UTF-8", func(t *testing.T) {
		path := writePrivateFormBytes(t, []byte{'{', 0xff, '}'})
		var stderr bytes.Buffer
		if code := run([]string{"-form-users-file", path}, &stderr); code != exitInvalid || stderr.String() != invalidConfigText {
			t.Fatalf("code=%d stderr=%q", code, stderr.String())
		}
	})

	t.Run("oversized", func(t *testing.T) {
		path := writePrivateFormBytes(t, bytes.Repeat([]byte{'x'}, browserapp.MaxFormConfigurationBytes+1))
		var stderr bytes.Buffer
		if code := run([]string{"-form-users-file", path}, &stderr); code != exitInvalid || stderr.String() != invalidConfigText {
			t.Fatalf("code=%d stderr=%q", code, stderr.String())
		}
	})

	for name, arguments := range map[string][]string{
		"missing":   nil,
		"empty":     {"-form-users-file", ""},
		"extra":     {"-form-users-file", "/private/example", "TEST_ONLY_SECRET_ARGUMENT"},
		"unknown":   {"-unknown=TEST_ONLY_SECRET_ARGUMENT"},
		"duplicate": {"-form-users-file", "/first", "--form-users-file=/second"},
	} {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			if code := run(arguments, &stderr); code != exitInvalidUsage || stderr.String() != invalidUsageText ||
				strings.Contains(stderr.String(), "TEST_ONLY_SECRET") {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
		})
	}
}

func writePrivateFormSnapshot(t *testing.T, contents string) string {
	t.Helper()
	return writePrivateFormBytes(t, []byte(contents))
}

func writePrivateFormBytes(t *testing.T, contents []byte) string {
	t.Helper()
	root := canonicalTempDir(t)
	path := filepath.Join(root, "form-users.json")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// A user with no usable address is a configuration error that the service
// refuses at sign-in. The validator keeps the file VALID (exit 0) but warns,
// with a count and never a login.
func TestRunWarnsAboutFormUsersWithoutEmail(t *testing.T) {
	path := writePrivateFormSnapshot(t, `{"users":[
		{"login":"TEST_ONLY_LOGIN_A","password":"TEST_ONLY_PASSWORD"},
		{"login":"TEST_ONLY_LOGIN_B","password":"TEST_ONLY_PASSWORD","email":"b@centry.user"},
		{"login":"ok","password":"TEST_ONLY_PASSWORD","email":"ok@example.test"}
	]}`)
	var stderr bytes.Buffer
	if code := run([]string{"-form-users-file", path}, &stderr); code != exitValid {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "warning: 2 Form user(s) have no usable email address") {
		t.Fatalf("stderr=%q, want the missing-email warning", stderr.String())
	}
	if strings.Contains(stderr.String(), "TEST_ONLY_LOGIN") || strings.Contains(stderr.String(), "TEST_ONLY_PASSWORD") {
		t.Fatalf("the warning leaked a configured value: %q", stderr.String())
	}
}
