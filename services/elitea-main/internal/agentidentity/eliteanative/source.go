package eliteanative

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
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
	// ReadFile reads a mounted Secret. Nil uses ReadMountedKey.
	ReadFile func(path string) ([]byte, error)
}

func (s KeySources) load(ctx context.Context, spec KeySpec) (secretstore.RuntimeSecret, error) {
	switch {
	case spec.File != "":
		read := s.ReadFile
		if read == nil {
			read = ReadMountedKey
		}
		raw, err := read(spec.File)
		if err != nil {
			// ReadMountedKey's errors are fixed phrases; none holds material.
			return secretstore.RuntimeSecret{}, fmt.Errorf("elitea-native agent identity: key %q: %w", spec.KeyID, err)
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

// ReadMountedKey reads a private key file as a Kubernetes Secret volume
// presents it: a plain file, or the kubelet's atomic-writer layout
// (<dir>/<key> -> ..data/<key>, ..data -> ..<timestamp>/), whose files are
// owned by root and readable through the volume's defaultMode. It follows
// the egress token key's established mount (a whole-directory Secret volume,
// defaultMode 0444, scoped to this pod's mount namespace) rather than
// securefile's owner-only rule, which a non-root reader of a Secret volume
// cannot meet without a copy step.
//
// The path must be absolute and clean and resolve to a regular file inside
// its own directory: a symlink that leaves the mount is refused. The file
// must not be executable or writable by group or other, and the directory
// that holds it must not be writable by group or other.
func ReadMountedKey(path string) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\r\n\x00") {
		return nil, errors.New("the key path must be absolute and clean")
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, errors.New("the key directory cannot be resolved")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, errors.New("the key file cannot be resolved")
	}
	rel, err := filepath.Rel(dir, resolved)
	sep := string(filepath.Separator)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+sep) || strings.Count(rel, sep) > 1 {
		return nil, errors.New("the key file resolves outside its mount directory")
	}
	parent, err := os.Stat(filepath.Dir(resolved))
	if err != nil || parent.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("the key directory is writable by group or other")
	}
	info, err := os.Lstat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxKeyFileBytes {
		return nil, errors.New("the key file is not a bounded regular file")
	}
	if info.Mode().Perm()&0o133 != 0 {
		return nil, errors.New("the key file is executable or writable by group or other")
	}
	file, err := os.Open(resolved)
	if err != nil {
		return nil, errors.New("the key file is not readable")
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("the key file changed during open")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxKeyFileBytes+1))
	if err != nil || len(raw) == 0 || len(raw) > maxKeyFileBytes {
		clear(raw)
		return nil, errors.New("the key file exceeds its bound")
	}
	return raw, nil
}
