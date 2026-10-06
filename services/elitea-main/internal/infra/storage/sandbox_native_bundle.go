package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

type nativePlatform struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
	ABI  string `json:"abi"`
}
type nativeRecord struct {
	Revision    uint8           `json:"revision"`
	Kind        string          `json:"kind"`
	Language    string          `json:"language"`
	Platform    nativePlatform  `json:"platform"`
	Preparation string          `json:"preparation_sha256"`
	Source      string          `json:"source_sha256"`
	Image       string          `json:"execution_image_digest"`
	Policy      string          `json:"execution_policy_revision"`
	Payload     json.RawMessage `json:"payload"`
	Digest      string          `json:"digest"`
}
type NativeSandboxBundle struct {
	record nativeRecord
	files  []sandboxBundleFile
	native []byte
}

func (b *NativeSandboxBundle) Digest() string                     { return b.record.Digest }
func (b *NativeSandboxBundle) nativeBytes() []byte                { return b.native }
func (b *NativeSandboxBundle) metadataName() string               { return "elitea-native-bundle-v2.json" }
func (b *NativeSandboxBundle) storagePrefix() string              { return "native-v2/" }
func (b *NativeSandboxBundle) recordedFiles() []sandboxBundleFile { return b.files }
func (b *NativeSandboxBundle) file(name string) (sandboxBundleFile, error) {
	for _, f := range b.files {
		if f.Name == name {
			return f, nil
		}
	}
	return sandboxBundleFile{}, ErrContentRejected
}
func strictNative(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		return ErrContentRejected
	}
	return nil
}
func canonicalNative(raw []byte, removeDigest bool) ([]byte, error) {
	var v map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if d.Decode(&v) != nil || v == nil {
		return nil, ErrContentRejected
	}
	if removeDigest {
		delete(v, "digest")
	}
	return sandboxJSON(v)
}
func nativePolicy(v string) bool {
	if len(v) == 0 || len(v) > 128 {
		return false
	}
	for _, b := range []byte(v) {
		if (b < 'a' || b > 'z') && (b < 'A' || b > 'Z') && (b < '0' || b > '9') && !strings.ContainsRune("._-", rune(b)) {
			return false
		}
	}
	return true
}
func nativePath(v string) bool {
	if len(v) == 0 || len(v) > 256 || len(strings.Split(v, "/")) > 24 {
		return false
	}
	for _, p := range strings.Split(v, "/") {
		if p == "" || p == "." || p == ".." {
			return false
		}
		for _, b := range []byte(p) {
			if (b < 'a' || b > 'z') && (b < 'A' || b > 'Z') && (b < '0' || b > '9') && !strings.ContainsRune("_.+-@", rune(b)) {
				return false
			}
		}
	}
	return true
}
func nativeDenoPath(v string) bool {
	return v == "elitea-javascript-lock.json" || v == "elitea-javascript-dependencies.mjs" || strings.HasPrefix(v, "deno-cache/npm/registry.npmjs.org/") || strings.HasPrefix(v, "deno-cache/remote/https/jsr.io/") && sandboxDigest(strings.TrimPrefix(v, "deno-cache/remote/https/jsr.io/"))
}
func ParseNativeSandboxBundle(raw []byte, root string) (*NativeSandboxBundle, error) {
	if len(raw) > sandboxBundleMetadataLimit || !utf8.Valid(raw) || !sandboxDigest(root) {
		return nil, ErrContentRejected
	}
	var r nativeRecord
	if strictNative(raw, &r) != nil {
		return nil, ErrContentRejected
	}
	if r.Revision != 2 || r.Digest != root || r.Platform.OS != "linux" || (r.Platform.Arch != "amd64" && r.Platform.Arch != "arm64") || r.Platform.ABI != "gnu" || !sandboxDigest(r.Preparation) || !sandboxDigest(r.Source) || !strings.HasPrefix(r.Image, "sha256:") || !sandboxDigest(strings.TrimPrefix(r.Image, "sha256:")) || !nativePolicy(r.Policy) {
		return nil, ErrContentRejected
	}
	canonical, err := canonicalNative(raw, false)
	if err != nil || !bytes.Equal(canonical, raw) {
		return nil, ErrContentRejected
	}
	content, err := canonicalNative(raw, true)
	if err != nil {
		return nil, ErrContentRejected
	}
	hash := sha256.Sum256(content)
	if hex.EncodeToString(hash[:]) != root {
		return nil, ErrContentRejected
	}
	var files []sandboxBundleFile
	switch r.Kind {
	case "deno":
		if r.Language != "javascript" && r.Language != "typescript" {
			return nil, ErrContentRejected
		}
		files, err = nativeDenoFiles(r.Payload)
	case "cargo":
		if r.Language != "rust" {
			return nil, ErrContentRejected
		}
		files, err = nativeCargoFiles(r)
	default:
		return nil, ErrContentRejected
	}
	if err != nil {
		return nil, err
	}
	return &NativeSandboxBundle{r, files, canonical}, nil
}
func nativeDenoFiles(raw []byte) ([]sandboxBundleFile, error) {
	var r sandboxBundleRecord
	if strictNative(raw, &r) != nil || r.Revision != 1 || r.Runtime != "deno-2.5.4" || !sandboxDigest(r.Digest) || r.Requirements == nil || len(r.Requirements) > 128 || len(r.Files) < 2 || len(r.Files) > 512 {
		return nil, ErrContentRejected
	}
	for _, v := range r.Requirements {
		if len(v) == 0 || len(v) > 256 || !sandboxASCII(v) || (!strings.HasPrefix(v, "npm:") && !strings.HasPrefix(v, "jsr:")) {
			return nil, ErrContentRejected
		}
	}
	var total int64
	lock, module := false, false
	files := make([]sandboxBundleFile, len(r.Files))
	for i, f := range r.Files {
		if !nativePath(f.Name) || !nativeDenoPath(f.Name) || !sandboxDigest(f.SHA256) || f.Bytes < 0 || f.Bytes > sandboxBundleFileLimit || i > 0 && f.Name <= r.Files[i-1].Name {
			return nil, ErrContentRejected
		}
		if f.Name == "elitea-javascript-lock.json" {
			lock = true
			if f.Bytes > 1024*1024 {
				return nil, ErrContentRejected
			}
		}
		if f.Name == "elitea-javascript-dependencies.mjs" {
			module = true
			if f.Bytes > 40*1024 {
				return nil, ErrContentRejected
			}
		}
		total += f.Bytes
		files[i] = sandboxBundleFile{f.SHA256 + ".blob", f.Bytes, f.SHA256}
	}
	if !lock || !module || total > sandboxBundleContentLimit {
		return nil, ErrContentRejected
	}
	content, err := sandboxJSON(r.sandboxBundleContent)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(content)
	if hex.EncodeToString(hash[:]) != r.Digest {
		return nil, ErrContentRejected
	}
	return files, nil
}
func nativeCargoFiles(r nativeRecord) ([]sandboxBundleFile, error) {
	type profile struct {
		Preparation string `json:"preparation_image"`
		Execution   string `json:"execution_image"`
		Rust        string `json:"rust_revision"`
		OS          string `json:"os"`
		Arch        string `json:"arch"`
		Target      string `json:"target"`
		Policy      string `json:"policy_revision"`
	}
	type object struct {
		Role   string `json:"role"`
		Name   string `json:"name"`
		Bytes  int64  `json:"bytes"`
		SHA256 string `json:"sha256"`
	}
	var p struct {
		Revision    uint8    `json:"revision"`
		Preparation string   `json:"preparation_sha256"`
		Declaration string   `json:"declaration_sha256"`
		Profile     profile  `json:"profile"`
		Objects     []object `json:"objects"`
	}
	if strictNative(r.Payload, &p) != nil {
		return nil, ErrContentRejected
	}
	target := "x86_64-unknown-linux-gnu"
	if r.Platform.Arch == "arm64" {
		target = "aarch64-unknown-linux-gnu"
	}
	if p.Revision != 1 || p.Preparation != r.Preparation || p.Declaration != r.Source || p.Profile.Execution != r.Image || p.Profile.Policy != r.Policy || p.Profile.OS != r.Platform.OS || p.Profile.Arch != r.Platform.Arch || p.Profile.Target != target || p.Profile.Rust != "1.97.1" || !strings.HasPrefix(p.Profile.Preparation, "sha256:") || !sandboxDigest(strings.TrimPrefix(p.Profile.Preparation, "sha256:")) || len(p.Objects) != 2 {
		return nil, ErrContentRejected
	}
	files := make([]sandboxBundleFile, 2)
	for i, f := range p.Objects {
		role, limit := "record", int64(8*1024*1024)
		if i == 1 {
			role, limit = "archive", 128*1024*1024
		}
		if f.Role != role || !sandboxDigest(f.SHA256) || f.Name != f.SHA256+".blob" || f.Bytes <= 0 || f.Bytes > limit {
			return nil, ErrContentRejected
		}
		files[i] = sandboxBundleFile{f.Name, f.Bytes, f.SHA256}
	}
	if files[0].Name == files[1].Name {
		return nil, ErrContentRejected
	}
	return files, nil
}
func (store *SandboxBundleStore) OpenNativeBundle(ctx context.Context, scope SandboxBundleScope, root string) (bundle *NativeSandboxBundle, result error) {
	if !sandboxDigest(root) || scope.key == "" {
		return nil, ErrContentUnauthorized
	}
	release, err := store.admit(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	ref, err := NewPlatformObjectRef(sandboxBundleBucket, scope.key+"/native-v2/"+root+"/elitea-native-bundle-v2.json")
	if err != nil {
		return nil, err
	}
	body, info, err := store.store.Get(ctx, ref, nil)
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, body.Close()) }()
	if info.Size < 0 || info.Size > sandboxBundleMetadataLimit {
		return nil, ErrContentRejected
	}
	raw, err := io.ReadAll(io.LimitReader(sandboxContextReader{ctx, body}, sandboxBundleMetadataLimit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) != info.Size {
		return nil, ErrContentRejected
	}
	return ParseNativeSandboxBundle(raw, root)
}
