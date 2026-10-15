package storage

import "context"

// DefaultEncryptionVerifier is implemented by a backend that can say whether
// its physical bucket encrypts objects at rest by default. The cloud executor
// stores plain git bundles and relies on bucket-side encryption (ADR-0034
// decision 4), so Main checks it at startup and keeps the executor off when it
// cannot confirm it.
type DefaultEncryptionVerifier interface {
	DefaultEncryptionConfirmed(ctx context.Context) (bool, error)
}

// DefaultEncryptionConfirmed asks store; a backend that cannot answer is not
// confirmed.
func DefaultEncryptionConfirmed(ctx context.Context, store ObjectStore) (bool, error) {
	verifier, ok := store.(DefaultEncryptionVerifier)
	if !ok {
		return false, nil
	}
	return verifier.DefaultEncryptionConfirmed(ctx)
}

// DefaultEncryptionConfirmed forwards to the wrapped backend.
func (s *instrumentedStore) DefaultEncryptionConfirmed(ctx context.Context) (bool, error) {
	return DefaultEncryptionConfirmed(ctx, s.inner)
}
