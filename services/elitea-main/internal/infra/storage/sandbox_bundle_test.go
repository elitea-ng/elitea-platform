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
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Native Deno output. The range contains '<', which Go normally HTML-escapes.
const nativeSandboxBundle = `{"revision":1,"runtime":"pyodide-0.29.0","requirements":["humanize>=4.13,<4.14"],"files":[{"name":"elitea-python-lock.json","bytes":914,"sha256":"52b1abe470e0f4b4581071104797ba3a0b13a5ff1e9058f227ce6497f694724e"},{"name":"humanize-4.13.0-py3-none-any.whl","bytes":128869,"sha256":"b810820b31891813b1673e8fec7f1ed3312061eab2f26e3fa192c393d11ed25f"},{"name":"micropip-0.11.0-py3-none-any.whl","bytes":115409,"sha256":"d98e6100df5e4145c1b11d559a8620e92b98d994cc1b52381a8815cebf3e7128"},{"name":"packaging-24.2-py3-none-any.whl","bytes":71930,"sha256":"6b006054a076c37c192c1e3faf3154037817f9bd245d47035326a8a9414bfdc3"}],"digest":"71d60b3a4bcbef14d4007fe9339ec6f7cff9c4a4c1a320f0e9c07ca26cd5d625"}`

func sandboxTestRecord(t *testing.T, record sandboxBundleRecord) ([]byte, string) {
	t.Helper()
	content, err := sandboxJSON(record.sandboxBundleContent)
	require.NoError(t, err)
	digest := sha256.Sum256(content)
	record.Digest = hex.EncodeToString(digest[:])
	native, err := sandboxJSON(record)
	require.NoError(t, err)
	return native, record.Digest
}

func TestSandboxBundleNativeDigest(t *testing.T) {
	var record sandboxBundleRecord
	require.NoError(t, json.Unmarshal([]byte(nativeSandboxBundle), &record))
	bundle, err := ParsePythonSandboxBundle([]byte(nativeSandboxBundle), record.Digest)
	require.NoError(t, err)
	require.Equal(t, nativeSandboxBundle, string(bundle.native))
	// Public accessors must not mutate the admitted file set.
	names := bundle.FileNames()
	names[0] = "other.whl"
	require.Equal(t, sandboxBundleLock, bundle.FileNames()[0])
	_, err = ParsePythonSandboxBundle([]byte(nativeSandboxBundle), strings.Repeat("0", 64))
	require.ErrorIs(t, err, ErrContentRejected)
}

func TestSandboxBundleMetadataAdmission(t *testing.T) {
	for name, change := range map[string]func(*sandboxBundleRecord){
		"revision":            func(r *sandboxBundleRecord) { r.Revision = 2 },
		"runtime":             func(r *sandboxBundleRecord) { r.Runtime = "other" },
		"requirements_null":   func(r *sandboxBundleRecord) { r.Requirements = nil },
		"requirements_many":   func(r *sandboxBundleRecord) { r.Requirements = make([]string, 129) },
		"requirement_long":    func(r *sandboxBundleRecord) { r.Requirements = []string{strings.Repeat("x", 257)} },
		"requirement_empty":   func(r *sandboxBundleRecord) { r.Requirements = []string{""} },
		"requirement_unicode": func(r *sandboxBundleRecord) { r.Requirements = []string{"pkg\u2028"} },
		"requirement_control": func(r *sandboxBundleRecord) { r.Requirements = []string{"pkg\n"} },
		"file_path":           func(r *sandboxBundleRecord) { r.Files[1].Name = "../other.whl" },
		"file_newline":        func(r *sandboxBundleRecord) { r.Files[1].Name += "\n" },
		"file_digest":         func(r *sandboxBundleRecord) { r.Files[1].SHA256 = strings.Repeat("A", 64) },
		"file_negative":       func(r *sandboxBundleRecord) { r.Files[1].Bytes = -1 },
		"lock_large":          func(r *sandboxBundleRecord) { r.Files[0].Bytes = 1024*1024 + 1 },
		"wheel_large":         func(r *sandboxBundleRecord) { r.Files[1].Bytes = sandboxBundleFileLimit + 1 },
		"file_duplicate":      func(r *sandboxBundleRecord) { r.Files[1] = r.Files[0] },
		"file_order":          func(r *sandboxBundleRecord) { r.Files[1], r.Files[2] = r.Files[2], r.Files[1] },
		"lock_missing":        func(r *sandboxBundleRecord) { r.Files = r.Files[1:] },
		"files_many": func(r *sandboxBundleRecord) {
			for i := 0; i < 258; i++ {
				r.Files = append(r.Files, sandboxBundleFile{Name: fmt.Sprintf("z%03d.whl", i), SHA256: strings.Repeat("0", 64)})
			}
		},
		"total_large": func(r *sandboxBundleRecord) {
			r.Files = []sandboxBundleFile{{Name: sandboxBundleLock, Bytes: 1, SHA256: strings.Repeat("0", 64)}}
			for i := 0; i < 4; i++ {
				r.Files = append(r.Files, sandboxBundleFile{Name: fmt.Sprintf("z%d.whl", i), Bytes: sandboxBundleFileLimit, SHA256: strings.Repeat("0", 64)})
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			var record sandboxBundleRecord
			require.NoError(t, json.Unmarshal([]byte(nativeSandboxBundle), &record))
			change(&record)
			native, root := sandboxTestRecord(t, record)
			_, err := ParsePythonSandboxBundle(native, root)
			require.ErrorIs(t, err, ErrContentRejected)
		})
	}
	var record sandboxBundleRecord
	require.NoError(t, json.Unmarshal([]byte(nativeSandboxBundle), &record))
	for _, native := range []string{
		strings.Repeat("x", sandboxBundleMetadataLimit+1),
		"prose " + nativeSandboxBundle,
		nativeSandboxBundle + " {}",
		strings.Replace(nativeSandboxBundle, `"revision":1`, `"extra":true,"revision":1`, 1),
		string([]byte{0xff}),
	} {
		_, err := ParsePythonSandboxBundle([]byte(native), record.Digest)
		require.ErrorIs(t, err, ErrContentRejected)
	}
}

type sandboxObjectFake struct {
	ObjectStore
	objects map[ObjectRef][]byte
	puts    []ObjectRef
	get     func(ObjectRef) (io.ReadCloser, ObjectInfo, error)
	put     func(ObjectRef, io.Reader) error
}

func (store *sandboxObjectFake) Put(_ context.Context, ref ObjectRef, body io.Reader, options PutOptions) (ObjectInfo, error) {
	if store.put != nil {
		if err := store.put(ref, body); err != nil {
			return ObjectInfo{}, err
		}
	}
	content, err := io.ReadAll(body)
	if err != nil {
		return ObjectInfo{}, err
	}
	if int64(len(content)) != options.ContentLength {
		return ObjectInfo{}, errors.New("unexpected content length")
	}
	store.objects[ref] = content
	store.puts = append(store.puts, ref)
	return ObjectInfo{Size: int64(len(content))}, nil
}

func (store *sandboxObjectFake) Get(_ context.Context, ref ObjectRef, _ *ByteRange) (io.ReadCloser, ObjectInfo, error) {
	if store.get != nil {
		return store.get(ref)
	}
	content, ok := store.objects[ref]
	if !ok {
		return nil, ObjectInfo{}, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(content)), ObjectInfo{Size: int64(len(content))}, nil
}

func sandboxTestStore(t *testing.T) (*SandboxBundleStore, *sandboxObjectFake, SandboxBundleScope, *PythonSandboxBundle, map[string][]byte) {
	t.Helper()
	content := map[string][]byte{sandboxBundleLock: []byte(`{"packages":{}}`), "z-test.whl": []byte("fixture wheel bytes")}
	record := sandboxBundleRecord{sandboxBundleContent: sandboxBundleContent{Revision: 1, Runtime: "pyodide-0.29.0", Requirements: []string{}}}
	for _, name := range []string{sandboxBundleLock, "z-test.whl"} {
		digest := sha256.Sum256(content[name])
		record.Files = append(record.Files, sandboxBundleFile{Name: name, Bytes: int64(len(content[name])), SHA256: hex.EncodeToString(digest[:])})
	}
	native, root := sandboxTestRecord(t, record)
	bundle, err := ParsePythonSandboxBundle(native, root)
	require.NoError(t, err)
	backend := &sandboxObjectFake{objects: make(map[ObjectRef][]byte)}
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	store, err := NewSandboxBundleStore(backend, dir, 1)
	require.NoError(t, err)
	scope, err := NewSandboxBundleScope("tenant-a", 2)
	require.NoError(t, err)
	return store, backend, scope, bundle, content
}

func sandboxTestUpload(t *testing.T, store *SandboxBundleStore, scope SandboxBundleScope, bundle *PythonSandboxBundle, content map[string][]byte) {
	t.Helper()
	for _, name := range bundle.FileNames() {
		require.NoError(t, store.PutFile(context.Background(), scope, bundle, name, bytes.NewReader(content[name])))
	}
}

func TestSandboxBundlePublicationAndReplacement(t *testing.T) {
	ctx := context.Background()
	store, backend, scope, bundle, content := sandboxTestStore(t)
	require.ErrorIs(t, store.Publish(ctx, scope, bundle), ErrNotFound)
	_, err := store.OpenBundle(ctx, scope, bundle.Digest())
	require.ErrorIs(t, err, ErrNotFound)
	sandboxTestUpload(t, store, scope, bundle, content)
	require.NoError(t, store.Publish(ctx, scope, bundle))
	require.Equal(t, "elitea-python-bundle.json", filepath.Base(backend.puts[len(backend.puts)-1].Key()))
	// Remove the original staging directory. A replacement needs only shared data.
	require.NoError(t, os.Remove(store.spoolDir))
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	replacement, err := NewSandboxBundleStore(backend, dir, 1)
	require.NoError(t, err)
	loaded, err := replacement.OpenBundle(ctx, scope, bundle.Digest())
	require.NoError(t, err)
	for _, name := range loaded.FileNames() {
		var output bytes.Buffer
		require.NoError(t, replacement.FetchFile(ctx, scope, loaded, name, &output))
		require.Equal(t, content[name], output.Bytes())
	}
	for _, other := range []SandboxBundleScope{mustSandboxScope(t, "tenant-b", 2), mustSandboxScope(t, "tenant-a", 3)} {
		_, err := replacement.OpenBundle(ctx, other, bundle.Digest())
		require.ErrorIs(t, err, ErrNotFound)
	}
	for ref := range backend.objects {
		require.Equal(t, PlatformScopeID, ref.ProjectID())
		public, err := NewObjectRef("2", ref.Bucket(), ref.Key())
		require.NoError(t, err)
		require.NotEqual(t, ref.StorageKey(""), public.StorageKey(""))
	}
}

func mustSandboxScope(t *testing.T, tenant string, project int32) SandboxBundleScope {
	t.Helper()
	scope, err := NewSandboxBundleScope(tenant, project)
	require.NoError(t, err)
	return scope
}

func TestSandboxBundleCorruptUploadCannotReplaceContent(t *testing.T) {
	store, backend, scope, bundle, content := sandboxTestStore(t)
	sandboxTestUpload(t, store, scope, bundle, content)
	for _, bad := range [][]byte{[]byte("altered wheel bytes"), []byte("short"), append(append([]byte(nil), content["z-test.whl"]...), 'x')} {
		require.ErrorIs(t, store.PutFile(context.Background(), scope, bundle, "z-test.whl", bytes.NewReader(bad)), ErrContentRejected)
		require.Len(t, backend.puts, 2)
	}
	entries, err := os.ReadDir(store.spoolDir)
	require.NoError(t, err)
	require.Empty(t, entries)
	require.NoError(t, store.Publish(context.Background(), scope, bundle))
}

type sandboxTrackedBody struct {
	io.Reader
	closed bool
}

func (body *sandboxTrackedBody) Close() error { body.closed = true; return nil }

func TestSandboxBundleCorruptSharedContentBlocksPublication(t *testing.T) {
	for _, mode := range []string{"hash", "short", "long", "size"} {
		t.Run(mode, func(t *testing.T) {
			store, backend, scope, bundle, content := sandboxTestStore(t)
			sandboxTestUpload(t, store, scope, bundle, content)
			bad := append([]byte(nil), content["z-test.whl"]...)
			size := int64(len(bad))
			switch mode {
			case "hash":
				bad[0] ^= 1
			case "short":
				bad = bad[:len(bad)-1]
			case "long":
				bad = append(bad, 'x')
			case "size":
				size++
			}
			body := &sandboxTrackedBody{Reader: bytes.NewReader(bad)}
			backend.get = func(ref ObjectRef) (io.ReadCloser, ObjectInfo, error) {
				if strings.HasSuffix(ref.Key(), "/z-test.whl") {
					return body, ObjectInfo{Size: size}, nil
				}
				value := backend.objects[ref]
				return io.NopCloser(bytes.NewReader(value)), ObjectInfo{Size: int64(len(value))}, nil
			}
			require.ErrorIs(t, store.Publish(context.Background(), scope, bundle), ErrContentRejected)
			require.True(t, body.closed)
			require.Len(t, backend.puts, 2)
		})
	}
}

func TestSandboxBundleBoundedAdmissionAndCleanup(t *testing.T) {
	store, backend, scope, bundle, content := sandboxTestStore(t)
	ctx := context.Background()
	release, err := store.admit(ctx)
	require.NoError(t, err)
	require.ErrorIs(t, store.PutFile(ctx, scope, bundle, sandboxBundleLock, bytes.NewReader(content[sandboxBundleLock])), ErrContentUnavailable)
	release()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, store.PutFile(canceled, scope, bundle, sandboxBundleLock, bytes.NewReader(content[sandboxBundleLock])), context.Canceled)
	backend.put = func(_ ObjectRef, reader io.Reader) error {
		file, ok := reader.(*os.File)
		require.True(t, ok)
		stat, err := file.Stat()
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), stat.Mode().Perm())
		return ErrAccessDenied
	}
	require.ErrorIs(t, store.PutFile(ctx, scope, bundle, sandboxBundleLock, bytes.NewReader(content[sandboxBundleLock])), ErrAccessDenied)
	entries, err := os.ReadDir(store.spoolDir)
	require.NoError(t, err)
	require.Empty(t, entries)
	backend.put = nil
	require.NoError(t, store.PutFile(ctx, scope, bundle, sandboxBundleLock, bytes.NewReader(content[sandboxBundleLock])))
}

func TestSandboxBundleRejectsInvalidScopeAndZeroMetadata(t *testing.T) {
	store, _, scope, bundle, _ := sandboxTestStore(t)
	for _, invalid := range []struct {
		tenant  string
		project int32
	}{{"", 2}, {"tenant", 0}, {"tenant\x00", 2}, {string([]byte{0xff}), 2}} {
		_, err := NewSandboxBundleScope(invalid.tenant, invalid.project)
		require.ErrorIs(t, err, ErrContentUnauthorized)
	}
	require.ErrorIs(t, store.Publish(context.Background(), SandboxBundleScope{}, bundle), ErrContentUnauthorized)
	require.ErrorIs(t, store.Publish(context.Background(), scope, nil), ErrContentUnauthorized)
	require.ErrorIs(t, store.Publish(context.Background(), scope, &PythonSandboxBundle{}), ErrContentRejected)
	require.ErrorIs(t, store.PutFile(context.Background(), scope, bundle, "../z-test.whl", strings.NewReader("")), ErrContentRejected)
	_, err := NewSandboxBundleStore(store.store, store.spoolDir, 0)
	require.Error(t, err)
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(store.spoolDir, link))
	_, err = NewSandboxBundleStore(store.store, link, 1)
	require.Error(t, err)
}

func TestSandboxBundleMetadataReadBoundsAndClosure(t *testing.T) {
	for _, mode := range []string{"size_large", "size_negative", "long", "short", "changed", "read_error"} {
		t.Run(mode, func(t *testing.T) {
			store, backend, scope, bundle, _ := sandboxTestStore(t)
			native := append([]byte(nil), bundle.native...)
			size := int64(len(native))
			switch mode {
			case "size_large":
				size = sandboxBundleMetadataLimit + 1
			case "size_negative":
				size = -1
			case "long":
				native = append(native, ' ')
			case "short":
				native = native[:len(native)-1]
			case "changed":
				native[0] = '['
			}
			body := &sandboxTrackedBody{Reader: bytes.NewReader(native)}
			if mode == "read_error" {
				body.Reader = sandboxErrorReader{}
			}
			backend.get = func(ObjectRef) (io.ReadCloser, ObjectInfo, error) { return body, ObjectInfo{Size: size}, nil }
			_, err := store.OpenBundle(context.Background(), scope, bundle.Digest())
			require.Error(t, err)
			require.True(t, body.closed)
		})
	}
}

type sandboxErrorReader struct{}

func (sandboxErrorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
