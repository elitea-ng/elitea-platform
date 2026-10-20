package storage

import (
	"errors"
	"fmt"
)

const (
	backendS3    = "s3"
	backendAzure = "azure"
	backendGCS   = "gcs"
)

// S3Config holds S3 backend configuration, sourced from STORAGE_CONTAINER,
// STORAGE_KEY_PREFIX, and the S3_* environment variables.
type S3Config struct {
	Endpoint       string
	AccessKey      string
	SecretKey      string
	Region         string
	ForcePathStyle bool
	Bucket         string
	KeyPrefix      string
	// NoPresign (S3_PRESIGN=false) hands clients Main's streaming facade
	// instead of presigned URLs, for a store clients cannot reach.
	NoPresign bool
}

// AzureConfig holds Azure Blob backend configuration, sourced from
// STORAGE_CONTAINER, STORAGE_KEY_PREFIX, and the AZURE_STORAGE_* environment
// variables.
type AzureConfig struct {
	Account       string
	Key           string
	Endpoint      string
	ContainerName string
	KeyPrefix     string
}

// GCSConfig holds GCS backend configuration, sourced from STORAGE_CONTAINER,
// STORAGE_KEY_PREFIX, GCS_ENDPOINT, and GOOGLE_APPLICATION_CREDENTIALS.
type GCSConfig struct {
	CredentialsFile string
	Endpoint        string
	Bucket          string
	KeyPrefix       string
}

// Config selects and configures exactly one ObjectStore backend.
type Config struct {
	Backend   string // "s3" | "azure" | "gcs" — required, env STORAGE_BACKEND
	Container string // physical bucket or container name — required, env STORAGE_CONTAINER
	KeyPrefix string // optional, empty by default, env STORAGE_KEY_PREFIX
	S3        S3Config
	Azure     AzureConfig
	GCS       GCSConfig
}

// ConfigFromEnv reads and validates storage configuration. It returns an
// error — never a silent default — when STORAGE_BACKEND is unset, empty, or
// not one of "s3"/"azure"/"gcs"; when STORAGE_CONTAINER is unset; when the
// selected backend's required credentials are absent; or when GCS has both
// an endpoint and a credentials file. There is no filesystem backend and no
// on-disk data directory setting — the libcloud path this replaces is not
// part of the target architecture (see ADR-0016 §8).
func ConfigFromEnv(lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		return Config{}, errors.New("storage: environment lookup is required")
	}

	backend, _ := lookup("STORAGE_BACKEND")
	if backend != backendS3 && backend != backendAzure && backend != backendGCS {
		return Config{}, fmt.Errorf(
			"storage: STORAGE_BACKEND must be one of %q, %q, %q (got %q)",
			backendS3, backendAzure, backendGCS, backend,
		)
	}

	container, present := lookup("STORAGE_CONTAINER")
	if !present || container == "" {
		return Config{}, errors.New("storage: STORAGE_CONTAINER is required")
	}

	keyPrefix, _ := lookup("STORAGE_KEY_PREFIX")

	cfg := Config{Backend: backend, Container: container, KeyPrefix: keyPrefix}

	switch backend {
	case backendS3:
		region, present := lookup("S3_REGION")
		if !present || region == "" {
			region = "us-east-1"
		}
		accessKey, _ := lookup("S3_ACCESS_KEY")
		secretKey, _ := lookup("S3_SECRET_KEY")
		endpoint, _ := lookup("S3_ENDPOINT_URL")
		forcePathStyle, _ := lookup("S3_FORCE_PATH_STYLE")
		presign, _ := lookup("S3_PRESIGN")
		if presign != "" && presign != "true" && presign != "false" {
			return Config{}, fmt.Errorf("storage: S3_PRESIGN must be \"true\" or \"false\" (got %q)", presign)
		}
		cfg.S3 = S3Config{
			Endpoint:       endpoint,
			AccessKey:      accessKey,
			SecretKey:      secretKey,
			Region:         region,
			ForcePathStyle: forcePathStyle == "true",
			Bucket:         container,
			KeyPrefix:      keyPrefix,
			NoPresign:      presign == "false",
		}

	case backendAzure:
		account, present := lookup("AZURE_STORAGE_ACCOUNT")
		if !present || account == "" {
			return Config{}, errors.New("storage: AZURE_STORAGE_ACCOUNT is required when STORAGE_BACKEND=azure")
		}
		key, _ := lookup("AZURE_STORAGE_KEY")
		endpoint, _ := lookup("AZURE_STORAGE_ENDPOINT")
		cfg.Azure = AzureConfig{
			Account:       account,
			Key:           key,
			Endpoint:      endpoint,
			ContainerName: container,
			KeyPrefix:     keyPrefix,
		}

	case backendGCS:
		credentialsFile, _ := lookup("GOOGLE_APPLICATION_CREDENTIALS")
		endpoint, _ := lookup("GCS_ENDPOINT")
		if credentialsFile != "" && endpoint != "" {
			return Config{}, errors.New(
				"storage: GOOGLE_APPLICATION_CREDENTIALS and GCS_ENDPOINT are mutually exclusive",
			)
		}
		cfg.GCS = GCSConfig{
			CredentialsFile: credentialsFile,
			Endpoint:        endpoint,
			Bucket:          container,
			KeyPrefix:       keyPrefix,
		}
	}

	return cfg, nil
}
