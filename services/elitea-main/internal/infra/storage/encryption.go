package storage

import (
	"context"
	"errors"
)

// ErrEncryptionUnreportable: the backend cannot say whether its bucket
// encrypts at rest (the API is not implemented, or the store has no such
// check). It is not a refusal: the operator may attest the encryption
// (ELITEA_CLOUD_EXECUTOR_STORAGE_ENCRYPTION_ATTESTED). A backend that answers
// "no default encryption" answers (false, nil), and no attestation overrides
// that.
var ErrEncryptionUnreportable = errors.New("storage: the backend cannot report default encryption at rest")

// DefaultEncryptionVerifier is implemented by a backend that can say whether
// its physical bucket encrypts objects at rest by default. The cloud executor
// stores plain git bundles and relies on bucket-side encryption (ADR-0034
// decision 4), so Main checks it at startup and keeps the executor off when it
// cannot confirm it.
type DefaultEncryptionVerifier interface {
	DefaultEncryptionConfirmed(ctx context.Context) (bool, error)
}

// DefaultEncryptionConfirmed asks store. A store without the check answers
// ErrEncryptionUnreportable.
func DefaultEncryptionConfirmed(ctx context.Context, store ObjectStore) (bool, error) {
	verifier, ok := store.(DefaultEncryptionVerifier)
	if !ok {
		return false, ErrEncryptionUnreportable
	}
	return verifier.DefaultEncryptionConfirmed(ctx)
}

// DefaultEncryptionConfirmed forwards to the wrapped backend.
func (s *instrumentedStore) DefaultEncryptionConfirmed(ctx context.Context) (bool, error) {
	return DefaultEncryptionConfirmed(ctx, s.inner)
}

// RetentionLifecycleConfigurer is implemented by a backend that installs its
// own incomplete-multipart lifecycle rule at startup (s3.Backend,
// gcs.Backend).
type RetentionLifecycleConfigurer interface {
	ConfigureRetentionLifecycle(ctx context.Context) error
}

// ConfigureRetentionLifecycle forwards to the wrapped backend. Without it the
// startup type assertion in cmd/elitea-main never saw the backend's method
// through the wrapper, and the lifecycle rule was never installed.
func (s *instrumentedStore) ConfigureRetentionLifecycle(ctx context.Context) error {
	configurer, ok := s.inner.(RetentionLifecycleConfigurer)
	if !ok {
		return nil
	}
	return configurer.ConfigureRetentionLifecycle(ctx)
}
