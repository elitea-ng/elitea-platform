package eliteanative

import (
	"context"
	"errors"
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/security/securefile"
)

// maxKeyFileBytes bounds one PEM private key (RSA 4096 is about 3.3 KiB).
const maxKeyFileBytes = 16 * 1024

// KeySources reads the private key material of a KeySpec. Each source is
// optional; a spec whose source is unset refuses boot.
type KeySources struct {
	// Vault reads a KV v2 secret. Context is the platform StoreContext the
	// ring's reads carry (WorkloadScope selects the Vault role).
	Vault        secretstore.SecretStore
	VaultContext secretstore.StoreContext
	// ReadFile reads a mounted Secret. Nil uses securefile with private
	// material rules (absolute path, no symlink, owner-only permissions).
	ReadFile func(path string) ([]byte, error)
}

func (s KeySources) load(ctx context.Context, spec KeySpec) (secretstore.RuntimeSecret, error) {
	switch {
	case spec.File != "":
		read := s.ReadFile
		if read == nil {
			read = func(path string) ([]byte, error) {
				return securefile.Read(path, maxKeyFileBytes, securefile.PrivateMaterial)
			}
		}
		raw, err := read(spec.File)
		if err != nil {
			return secretstore.RuntimeSecret{}, fmt.Errorf("elitea-native agent identity: key %q: the key file is not readable", spec.KeyID)
		}
		defer clear(raw)
		return secretstore.NewRuntimeSecret(raw), nil
	case spec.Vault != nil:
		if s.Vault == nil {
			return secretstore.RuntimeSecret{}, fmt.Errorf("elitea-native agent identity: key %q names Vault, which is not configured", spec.KeyID)
		}
		version, err := s.Vault.Read(ctx, s.VaultContext, secretstore.SecretRef{
			Backend: secretstore.BackendVault, Mount: spec.Vault.Mount, Path: spec.Vault.Path,
		}, spec.Vault.Version)
		if err != nil {
			// The store's error names a reason class, never a path or a value.
			return secretstore.RuntimeSecret{}, fmt.Errorf("elitea-native agent identity: key %q: %w", spec.KeyID, err)
		}
		return version.Value, nil
	}
	return secretstore.RuntimeSecret{}, fmt.Errorf("elitea-native agent identity: key %q has no source", spec.KeyID)
}

// LoadKeyRing loads every configured key and builds the ring. The material
// is parsed into a LocalSigner and destroyed; nothing else keeps it. Extra
// signers (a KMS implementation of KeySigner) join the configured keys.
func LoadKeyRing(ctx context.Context, cfg Config, sources KeySources, foreign []ForeignKey, extra ...RingKey) (*KeyRing, error) {
	keys := make([]RingKey, 0, len(cfg.Keys)+len(extra))
	for _, spec := range cfg.Keys {
		material, err := sources.load(ctx, spec)
		if err != nil {
			return nil, err
		}
		signer, err := NewLocalSigner(spec.KeyID, spec.Algorithm, material)
		if err != nil {
			return nil, err
		}
		keys = append(keys, RingKey{Signer: signer, NotBefore: spec.NotBefore})
	}
	keys = append(keys, extra...)
	if len(keys) == 0 {
		return nil, errors.New("elitea-native agent identity: the key ring is empty")
	}
	return NewKeyRing(keys, cfg.RotationOverlap, cfg.retention(), foreign)
}
