package storage

import (
	"context"
	"errors"
	"io"
	"time"

	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

const SnapshotExecutableName = "elitea-code-job"
const snapshotMetadataName = "descriptor.json"
const snapshotStoragePrefix = "compiled-snapshot-v1/"

type RustCompiledSnapshot struct {
	descriptor runtimedomain.RustSnapshotDescriptor
	native     []byte
	root       string
}

func ParseRustCompiledSnapshot(raw []byte, root string) (*RustCompiledSnapshot, error) {
	d, err := runtimedomain.ParseRustSnapshotDescriptor(raw, root)
	if err != nil {
		return nil, ErrContentRejected
	}
	return &RustCompiledSnapshot{d, append([]byte(nil), raw...), root}, nil
}
func (b *RustCompiledSnapshot) Digest() string        { return b.root }
func (b *RustCompiledSnapshot) nativeBytes() []byte   { return b.native }
func (b *RustCompiledSnapshot) metadataName() string  { return snapshotMetadataName }
func (b *RustCompiledSnapshot) storagePrefix() string { return snapshotStoragePrefix }
func (b *RustCompiledSnapshot) recordedFiles() []sandboxBundleFile {
	return []sandboxBundleFile{{Name: SnapshotExecutableName, Bytes: b.descriptor.ExecutableBytes, SHA256: b.descriptor.ExecutableSHA256}}
}
func (b *RustCompiledSnapshot) file(name string) (sandboxBundleFile, error) {
	if name != SnapshotExecutableName {
		return sandboxBundleFile{}, ErrContentRejected
	}
	return b.recordedFiles()[0], nil
}
func (store *SandboxBundleStore) OpenCompiledSnapshot(ctx context.Context, scope SandboxBundleScope, root string) (bundle *RustCompiledSnapshot, result error) {
	if !sandboxDigest(root) || scope.key == "" {
		return nil, ErrContentUnauthorized
	}
	release, err := store.admit(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	ref, err := NewPlatformObjectRef(sandboxBundleBucket, scope.key+"/"+snapshotStoragePrefix+root+"/"+snapshotMetadataName)
	if err != nil {
		return nil, err
	}
	body, info, err := store.store.Get(ctx, ref, nil)
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, body.Close()) }()
	if info.Size < 1 || info.Size > runtimedomain.SnapshotDescriptorLimit {
		return nil, ErrContentRejected
	}
	raw, err := io.ReadAll(io.LimitReader(sandboxContextReader{ctx, body}, runtimedomain.SnapshotDescriptorLimit+1))
	if err != nil || int64(len(raw)) != info.Size {
		return nil, ErrContentRejected
	}
	return ParseRustCompiledSnapshot(raw, root)
}

// SweepCompiledSnapshots deletes only the two fixed immutable objects. Expired
// reservations count against quota until deletion and its fenced receipt finish.
// A crash retries the same eviction epoch; no bucket listing or arbitrary paths.
func (store *SandboxBundleStore) SweepCompiledSnapshots(ctx context.Context, index runtimedomain.RustSnapshotEvictor, owner string, limit int) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if index == nil {
		return ErrContentRejected
	}
	rows, err := index.ClaimExpired(ctx, owner, limit, 30*time.Second)
	if err != nil {
		return err
	}
	var failures error
	for _, row := range rows {
		bundle, err := ParseRustCompiledSnapshot(row.Candidate.DescriptorJSON, row.Candidate.Root)
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		scope, err := NewSandboxBundleScope(row.Candidate.Scope.TenantID, row.Candidate.Scope.ProjectID)
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		err = index.CompleteEviction(ctx, row, func() error {
			var deletion error
			for _, name := range []string{SnapshotExecutableName, snapshotMetadataName} {
				ref, e := sandboxRef(scope, bundle, name)
				if e == nil {
					e = store.store.Delete(ctx, ref)
				}
				if e != nil && !errors.Is(e, ErrNotFound) {
					deletion = errors.Join(deletion, e)
				}
			}
			return deletion
		})
		failures = errors.Join(failures, err)
	}
	return failures
}
