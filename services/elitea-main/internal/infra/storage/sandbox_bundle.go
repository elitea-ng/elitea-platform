package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	sandboxBundleMetadataLimit = 128 * 1024
	sandboxBundleContentLimit  = 128 * 1024 * 1024
	sandboxBundleFileLimit     = 32 * 1024 * 1024
	sandboxBundleBucket        = "sandbox-dependencies"
	sandboxBundleLock          = "elitea-python-lock.json"
)

// SandboxBundleScope comes from verified sandbox authority, never request headers.
// This storage layer validates identity shape. It does not authorize a caller.
type SandboxBundleScope struct {
	key string
}

func NewSandboxBundleScope(tenant string, project int32) (SandboxBundleScope, error) {
	if tenant == "" || len(tenant) > 256 || strings.ContainsRune(tenant, 0) || !utf8.ValidString(tenant) || project <= 0 {
		return SandboxBundleScope{}, ErrContentUnauthorized
	}
	digest := sha256.Sum256([]byte(tenant))
	return SandboxBundleScope{key: hex.EncodeToString(digest[:]) + "/" + strconv.FormatInt(int64(project), 10)}, nil
}

type sandboxBundleFile struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type sandboxBundleContent struct {
	Revision     uint32              `json:"revision"`
	Runtime      string              `json:"runtime"`
	Requirements []string            `json:"requirements"`
	Files        []sandboxBundleFile `json:"files"`
}

type sandboxBundleRecord struct {
	sandboxBundleContent
	Digest string `json:"digest"`
}

// PythonSandboxBundle is immutable metadata validated against an external root.
// The native preparer owns package resolution and the lockfile format.
type PythonSandboxBundle struct {
	record sandboxBundleRecord
	native []byte
}

func sandboxDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, value := range []byte(value) {
		if (value < '0' || value > '9') && (value < 'a' || value > 'f') {
			return false
		}
	}
	return true
}

func sandboxASCII(value string) bool {
	for _, value := range []byte(value) {
		if value < 0x20 || value > 0x7e {
			return false
		}
	}
	return true
}

func sandboxFileName(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, value := range []byte(value) {
		if (value < '0' || value > '9') && (value < 'a' || value > 'z') && (value < 'A' || value > 'Z') && !strings.ContainsRune("_.+-", rune(value)) {
			return false
		}
	}
	return value == sandboxBundleLock || strings.HasSuffix(value, ".whl")
}

// sandboxJSON matches the preparer's JSON.stringify for this ASCII record.
func sandboxJSON(value any) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(output.Bytes(), []byte{'\n'}), nil
}

func ParsePythonSandboxBundle(native []byte, expectedRoot string) (*PythonSandboxBundle, error) {
	if len(native) > sandboxBundleMetadataLimit || !utf8.Valid(native) || !sandboxDigest(expectedRoot) {
		return nil, ErrContentRejected
	}
	var record sandboxBundleRecord
	decoder := json.NewDecoder(bytes.NewReader(native))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return nil, ErrContentRejected
	}
	if decoder.Decode(new(any)) != io.EOF || record.Revision != 1 || record.Runtime != "pyodide-0.29.0" || record.Digest != expectedRoot || record.Requirements == nil || len(record.Requirements) > 128 || len(record.Files) == 0 || len(record.Files) > 257 {
		return nil, ErrContentRejected
	}
	for _, requirement := range record.Requirements {
		if requirement == "" || len(requirement) > 256 || !sandboxASCII(requirement) {
			return nil, ErrContentRejected
		}
	}
	var total int64
	lockFound := false
	for index, file := range record.Files {
		limit := int64(sandboxBundleFileLimit)
		if file.Name == sandboxBundleLock {
			lockFound = true
			limit = 1024 * 1024
		}
		if !sandboxFileName(file.Name) || !sandboxDigest(file.SHA256) || file.Bytes < 0 || file.Bytes > limit || index > 0 && file.Name <= record.Files[index-1].Name {
			return nil, ErrContentRejected
		}
		total += file.Bytes
	}
	if !lockFound || total > sandboxBundleContentLimit {
		return nil, ErrContentRejected
	}
	content, err := sandboxJSON(record.sandboxBundleContent)
	if err != nil {
		return nil, ErrContentRejected
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != expectedRoot {
		return nil, ErrContentRejected
	}
	canonical, err := sandboxJSON(record)
	if err != nil || len(canonical) > sandboxBundleMetadataLimit {
		return nil, ErrContentRejected
	}
	return &PythonSandboxBundle{record: record, native: canonical}, nil
}

func (bundle *PythonSandboxBundle) Digest() string { return bundle.record.Digest }

func (bundle *PythonSandboxBundle) FileNames() []string {
	names := make([]string, len(bundle.record.Files))
	for index, file := range bundle.record.Files {
		names[index] = file.Name
	}
	return names
}

func (bundle *PythonSandboxBundle) file(name string) (sandboxBundleFile, error) {
	for _, file := range bundle.record.Files {
		if file.Name == name {
			return file, nil
		}
	}
	return sandboxBundleFile{}, ErrContentRejected
}

// SandboxBundleStore uses shared object storage. Private local files are staging only.
// Callers must authorize scope and root before invoking any operation.
type SandboxBundleStore struct {
	store    ObjectStore
	spoolDir string
	slots    chan struct{}
}

func NewSandboxBundleStore(store ObjectStore, spoolDir string, maxRequests int) (*SandboxBundleStore, error) {
	stat, err := os.Lstat(spoolDir)
	if store == nil || err != nil || !stat.IsDir() || stat.Mode().Perm()&0o077 != 0 || maxRequests < 1 || maxRequests > 32 {
		return nil, errors.New("sandbox bundle storage requires a private staging directory and bounded capacity")
	}
	return &SandboxBundleStore{store: store, spoolDir: spoolDir, slots: make(chan struct{}, maxRequests)}, nil
}

func (store *SandboxBundleStore) admit(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case store.slots <- struct{}{}:
		return func() { <-store.slots }, nil
	default:
		return nil, ErrContentUnavailable
	}
}

func sandboxRef(scope SandboxBundleScope, bundle *PythonSandboxBundle, name string) (ObjectRef, error) {
	if scope.key == "" || bundle == nil {
		return ObjectRef{}, ErrContentUnauthorized
	}
	if !sandboxDigest(bundle.Digest()) || len(bundle.native) == 0 || name != "elitea-python-bundle.json" && !sandboxFileName(name) {
		return ObjectRef{}, ErrContentRejected
	}
	return NewPlatformObjectRef(sandboxBundleBucket, scope.key+"/"+bundle.Digest()+"/"+name)
}

type sandboxContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader sandboxContextReader) Read(bytes []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(bytes)
}

func verifySandboxFile(ctx context.Context, body io.Reader, output io.Writer, file sandboxBundleFile) error {
	hash := sha256.New()
	count, err := io.Copy(io.MultiWriter(output, hash), io.LimitReader(sandboxContextReader{ctx, body}, file.Bytes+1))
	if err != nil {
		return err
	}
	if count != file.Bytes || hex.EncodeToString(hash.Sum(nil)) != file.SHA256 {
		return ErrContentRejected
	}
	return ctx.Err()
}

// PutFile verifies the complete bounded body before any object-store write.
// The caller owns body cancellation. Failed staging never replaces cached content.
func (store *SandboxBundleStore) PutFile(ctx context.Context, scope SandboxBundleScope, bundle *PythonSandboxBundle, name string, body io.Reader) (result error) {
	ref, err := sandboxRef(scope, bundle, name)
	if err != nil {
		return err
	}
	file, err := bundle.file(name)
	if err != nil || body == nil {
		return ErrContentRejected
	}
	release, err := store.admit(ctx)
	if err != nil {
		return err
	}
	defer release()
	staged, err := os.CreateTemp(store.spoolDir, "dependency-*")
	if err != nil {
		return fmt.Errorf("stage sandbox dependency: %w", err)
	}
	defer func() {
		result = errors.Join(result, staged.Close(), os.Remove(staged.Name()))
	}()
	if err := verifySandboxFile(ctx, body, staged, file); err != nil {
		return err
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err = store.store.Put(ctx, ref, staged, PutOptions{ContentType: "application/octet-stream", ContentLength: file.Bytes})
	return err
}

// FetchFile writes only to caller-owned staging. Verify before publishing or executing it.
func (store *SandboxBundleStore) FetchFile(ctx context.Context, scope SandboxBundleScope, bundle *PythonSandboxBundle, name string, output io.Writer) (result error) {
	release, err := store.admit(ctx)
	if err != nil {
		return err
	}
	defer release()
	return store.fetchFile(ctx, scope, bundle, name, output)
}

func (store *SandboxBundleStore) fetchFile(ctx context.Context, scope SandboxBundleScope, bundle *PythonSandboxBundle, name string, output io.Writer) (result error) {
	ref, err := sandboxRef(scope, bundle, name)
	if err != nil {
		return err
	}
	file, err := bundle.file(name)
	if err != nil || output == nil {
		return ErrContentRejected
	}
	body, info, err := store.store.Get(ctx, ref, nil)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, body.Close()) }()
	if info.Size != file.Bytes {
		return ErrContentRejected
	}
	return verifySandboxFile(ctx, body, output, file)
}

// Publish verifies all shared files before it publishes the immutable bundle record.
// Persist the preparation receipt only after this operation succeeds.
func (store *SandboxBundleStore) Publish(ctx context.Context, scope SandboxBundleScope, bundle *PythonSandboxBundle) error {
	ref, err := sandboxRef(scope, bundle, "elitea-python-bundle.json")
	if err != nil {
		return err
	}
	release, err := store.admit(ctx)
	if err != nil {
		return err
	}
	defer release()
	for _, file := range bundle.record.Files {
		if err := store.fetchFile(ctx, scope, bundle, file.Name, io.Discard); err != nil {
			return err
		}
	}
	_, err = store.store.Put(ctx, ref, bytes.NewReader(bundle.native), PutOptions{ContentType: "application/json", ContentLength: int64(len(bundle.native))})
	return err
}

func (store *SandboxBundleStore) OpenBundle(ctx context.Context, scope SandboxBundleScope, root string) (bundle *PythonSandboxBundle, result error) {
	if !sandboxDigest(root) || scope.key == "" {
		return nil, ErrContentUnauthorized
	}
	release, err := store.admit(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	ref, err := NewPlatformObjectRef(sandboxBundleBucket, scope.key+"/"+root+"/elitea-python-bundle.json")
	if err != nil {
		return nil, err
	}
	body, info, err := store.store.Get(ctx, ref, nil)
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, body.Close()) }()
	if info.Size > sandboxBundleMetadataLimit || info.Size < 0 {
		return nil, ErrContentRejected
	}
	native, err := io.ReadAll(io.LimitReader(sandboxContextReader{ctx, body}, sandboxBundleMetadataLimit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(native)) != info.Size {
		return nil, ErrContentRejected
	}
	return ParsePythonSandboxBundle(native, root)
}
