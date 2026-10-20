package storage

import "testing"

func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

func TestArtifactConfigFromEnvRejectsInvalidInput(t *testing.T) {
	cases := map[string]map[string]string{
		"unset backend": {
			"STORAGE_CONTAINER": "bucket",
		},
		"backend filesystem": {
			"STORAGE_BACKEND":   "filesystem",
			"STORAGE_CONTAINER": "bucket",
		},
		"backend nonsense": {
			"STORAGE_BACKEND":   "nonsense",
			"STORAGE_CONTAINER": "bucket",
		},
		"s3 with no container": {
			"STORAGE_BACKEND": "s3",
		},
		"azure with no account": {
			"STORAGE_BACKEND":        "azure",
			"STORAGE_CONTAINER":      "bucket",
			"AZURE_STORAGE_KEY":      "key",
			"AZURE_STORAGE_ENDPOINT": "http://azurite:10000",
		},
		"s3 with a presign switch that is not a boolean": {
			"STORAGE_BACKEND":   "s3",
			"STORAGE_CONTAINER": "bucket",
			"S3_PRESIGN":        "no",
		},
		"gcs with both endpoint and credentials file": {
			"STORAGE_BACKEND":                "gcs",
			"STORAGE_CONTAINER":              "bucket",
			"GCS_ENDPOINT":                   "http://fake-gcs:4443/storage/v1/",
			"GOOGLE_APPLICATION_CREDENTIALS": "/tmp/creds.json",
		},
	}

	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ConfigFromEnv(lookupFrom(env)); err == nil {
				t.Fatalf("ConfigFromEnv(%v) = nil error, want an error", env)
			}
		})
	}
}

func TestArtifactConfigFromEnvRejectsNilLookup(t *testing.T) {
	if _, err := ConfigFromEnv(nil); err == nil {
		t.Fatal("ConfigFromEnv(nil) = nil error, want an error")
	}
}

func TestArtifactConfigFromEnvAcceptsValidS3(t *testing.T) {
	cfg, err := ConfigFromEnv(lookupFrom(map[string]string{
		"STORAGE_BACKEND":     "s3",
		"STORAGE_CONTAINER":   "elitea-artifacts",
		"STORAGE_KEY_PREFIX":  "prefix",
		"S3_ENDPOINT_URL":     "http://rustfs:9000",
		"S3_ACCESS_KEY":       "ak",
		"S3_SECRET_KEY":       "sk",
		"S3_FORCE_PATH_STYLE": "true",
	}))
	if err != nil {
		t.Fatalf("ConfigFromEnv returned %v, want nil", err)
	}
	if cfg.Backend != "s3" {
		t.Errorf("Backend = %q, want s3", cfg.Backend)
	}
	if cfg.S3.Bucket != "elitea-artifacts" {
		t.Errorf("S3.Bucket = %q, want elitea-artifacts", cfg.S3.Bucket)
	}
	if cfg.S3.KeyPrefix != "prefix" {
		t.Errorf("S3.KeyPrefix = %q, want prefix", cfg.S3.KeyPrefix)
	}
	if !cfg.S3.ForcePathStyle {
		t.Errorf("S3.ForcePathStyle = false, want true")
	}
	if cfg.S3.Region != "us-east-1" {
		t.Errorf("S3.Region = %q, want default us-east-1", cfg.S3.Region)
	}
	if cfg.S3.NoPresign {
		t.Errorf("S3.NoPresign = true with S3_PRESIGN unset, want presigning on by default")
	}
}

func TestArtifactConfigFromEnvS3PresignSwitch(t *testing.T) {
	for value, want := range map[string]bool{"true": false, "false": true} {
		cfg, err := ConfigFromEnv(lookupFrom(map[string]string{
			"STORAGE_BACKEND":   "s3",
			"STORAGE_CONTAINER": "bucket",
			"S3_PRESIGN":        value,
		}))
		if err != nil {
			t.Fatalf("S3_PRESIGN=%s: ConfigFromEnv returned %v", value, err)
		}
		if cfg.S3.NoPresign != want {
			t.Errorf("S3_PRESIGN=%s: NoPresign = %v, want %v", value, cfg.S3.NoPresign, want)
		}
	}
}

func TestArtifactConfigFromEnvAcceptsValidAzure(t *testing.T) {
	cfg, err := ConfigFromEnv(lookupFrom(map[string]string{
		"STORAGE_BACKEND":        "azure",
		"STORAGE_CONTAINER":      "elitea-artifacts",
		"AZURE_STORAGE_ACCOUNT":  "devstoreaccount1",
		"AZURE_STORAGE_KEY":      "key",
		"AZURE_STORAGE_ENDPOINT": "http://azurite:10000/devstoreaccount1",
	}))
	if err != nil {
		t.Fatalf("ConfigFromEnv returned %v, want nil", err)
	}
	if cfg.Azure.ContainerName != "elitea-artifacts" {
		t.Errorf("Azure.ContainerName = %q, want elitea-artifacts", cfg.Azure.ContainerName)
	}
	if cfg.Azure.Account != "devstoreaccount1" {
		t.Errorf("Azure.Account = %q, want devstoreaccount1", cfg.Azure.Account)
	}
}

func TestArtifactConfigFromEnvAcceptsValidGCS(t *testing.T) {
	cfg, err := ConfigFromEnv(lookupFrom(map[string]string{
		"STORAGE_BACKEND":   "gcs",
		"STORAGE_CONTAINER": "elitea-artifacts",
		"GCS_ENDPOINT":      "http://fake-gcs:4443/storage/v1/",
	}))
	if err != nil {
		t.Fatalf("ConfigFromEnv returned %v, want nil", err)
	}
	if cfg.GCS.Bucket != "elitea-artifacts" {
		t.Errorf("GCS.Bucket = %q, want elitea-artifacts", cfg.GCS.Bucket)
	}
	if cfg.GCS.Endpoint != "http://fake-gcs:4443/storage/v1/" {
		t.Errorf("GCS.Endpoint = %q, want the configured endpoint", cfg.GCS.Endpoint)
	}
}
