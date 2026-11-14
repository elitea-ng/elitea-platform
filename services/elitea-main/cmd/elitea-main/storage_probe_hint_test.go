package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

func TestObjectStoreProbeErrorNamesWorkloadIdentityOnlyForKeylessGCS(t *testing.T) {
	cause := errors.New("could not find default credentials")
	keyless := objectStoreProbeError(storage.Config{Backend: "gcs"}, cause)
	if !strings.Contains(keyless.Error(), "workloadIdentityUser") || !errors.Is(keyless, cause) {
		t.Fatalf("keyless GCS error = %v, want the Workload Identity hint wrapping the cause", keyless)
	}
	for _, cfg := range []storage.Config{
		{Backend: "s3"},
		{Backend: "gcs", GCS: storage.GCSConfig{Endpoint: "http://fake:4443"}},
		{Backend: "gcs", GCS: storage.GCSConfig{CredentialsFile: "/k.json"}},
	} {
		if strings.Contains(objectStoreProbeError(cfg, cause).Error(), "workloadIdentityUser") {
			t.Fatalf("%+v: hint must not appear", cfg)
		}
	}
}
