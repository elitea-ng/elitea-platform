package main

import (
	"context"
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage/azure"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage/gcs"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage/s3"
)

// newObjectStore dispatches to the concrete backend constructor named by
// cfg.Backend. It lives in package main, not internal/infra/storage: s3,
// azure, and gcs each import internal/infra/storage for ObjectRef,
// ObjectInfo, and the rest of S1's types (required by their own
// var _ storage.ObjectStore assertions), so a factory inside package storage
// that constructed s3.Backend/azure.Backend/gcs.Backend would need storage
// to import them back — a direct import cycle, not a style choice.
//
// The returned store is always storage.Instrument-wrapped (S18) — every
// call site that obtains an ObjectStore through this constructor gets the
// OTel instruments and delete-audit logging for free, with no separate
// wiring step to remember at each of them.
func newObjectStore(ctx context.Context, cfg storage.Config) (storage.ObjectStore, error) {
	var (
		store storage.ObjectStore
		err   error
	)
	switch cfg.Backend {
	case "s3":
		store, err = s3.New(ctx, s3.Config{
			Endpoint:       cfg.S3.Endpoint,
			AccessKey:      cfg.S3.AccessKey,
			SecretKey:      cfg.S3.SecretKey,
			Region:         cfg.S3.Region,
			ForcePathStyle: cfg.S3.ForcePathStyle,
			Bucket:         cfg.S3.Bucket,
			KeyPrefix:      cfg.S3.KeyPrefix,
			NoPresign:      cfg.S3.NoPresign,
		})
	case "azure":
		store, err = azure.New(ctx, azure.Config{
			Account:       cfg.Azure.Account,
			Key:           cfg.Azure.Key,
			ContainerName: cfg.Azure.ContainerName,
			Endpoint:      cfg.Azure.Endpoint,
			KeyPrefix:     cfg.Azure.KeyPrefix,
		})
	case "gcs":
		store, err = gcs.New(ctx, gcs.Config{
			Bucket:          cfg.GCS.Bucket,
			CredentialsFile: cfg.GCS.CredentialsFile,
			Endpoint:        cfg.GCS.Endpoint,
			KeyPrefix:       cfg.GCS.KeyPrefix,
		})
	default:
		return nil, fmt.Errorf("storage: unrecognised backend %q", cfg.Backend)
	}
	if err != nil {
		return nil, err
	}
	return storage.Instrument(store, cfg.Backend), nil
}
