package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

func TestStorageProbeHintOnlyForKeylessGCS(t *testing.T) {
	cause := errors.New("could not find default credentials")
	none := func(string) (string, bool) { return "", false }
	for _, step := range []string{"object store readiness probe", "configure object store retention lifecycle"} {
		keyless := objectStoreStartupErrorEnv(storage.Config{Backend: "gcs"}, step, cause, none)
		if !strings.HasPrefix(keyless.Error(), step+": ") || !strings.Contains(keyless.Error(), "workloadIdentityUser") || !errors.Is(keyless, cause) {
			t.Fatalf("%s: keyless GCS error = %v, want the step, the Workload Identity hint and the cause", step, keyless)
		}
	}
	env := func(name string) func(string) (string, bool) {
		return func(k string) (string, bool) { return "x", k == name }
	}
	cases := map[string]struct {
		cfg    storage.Config
		lookup func(string) (string, bool)
	}{
		"s3":               {storage.Config{Backend: "s3"}, none},
		"endpoint":         {storage.Config{Backend: "gcs", GCS: storage.GCSConfig{Endpoint: "http://fake:4443"}}, none},
		"key file":         {storage.Config{Backend: "gcs", GCS: storage.GCSConfig{CredentialsFile: "/k.json"}}, none},
		"ADC file env":     {storage.Config{Backend: "gcs"}, env("GOOGLE_APPLICATION_CREDENTIALS")},
		"storage emulator": {storage.Config{Backend: "gcs"}, env("STORAGE_EMULATOR_HOST")},
	}
	for name, c := range cases {
		got := objectStoreStartupErrorEnv(c.cfg, "object store readiness probe", cause, c.lookup)
		if strings.Contains(got.Error(), "workloadIdentityUser") {
			t.Fatalf("%s: hint must not appear: %v", name, got)
		}
	}
}
