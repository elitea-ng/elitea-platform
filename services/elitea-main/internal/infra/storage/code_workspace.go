package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// These limits are operator policy. Model input cannot change them.
type CodeWorkspacePolicy struct {
	Revision              uint32 `json:"revision"`
	MaxFiles              uint32 `json:"max_files"`
	MaxFileBytes          uint64 `json:"max_file_bytes"`
	MaxTotalBytes         uint64 `json:"max_total_bytes"`
	MaxManifestBytes      uint32 `json:"max_manifest_bytes"`
	MaxPathBytes          uint32 `json:"max_path_bytes"`
	MaxDepth              uint32 `json:"max_depth"`
	MaxProjections        uint32 `json:"max_projections"`
	MaxAcquisitionSeconds uint32 `json:"max_acquisition_seconds"`
}

func DefaultCodeWorkspacePolicy() CodeWorkspacePolicy {
	return CodeWorkspacePolicy{1, 1024, 1 << 20, 16 << 20, 512 << 10, 255, 16, 64, 300}
}

func (p CodeWorkspacePolicy) Validate() error {
	if p.Revision != 1 || p.MaxFiles == 0 || p.MaxFiles > 4096 ||
		p.MaxFileBytes == 0 || p.MaxFileBytes > 4<<20 ||
		p.MaxTotalBytes < p.MaxFileBytes || p.MaxTotalBytes > 128<<20 ||
		p.MaxManifestBytes < 1024 || p.MaxManifestBytes > 4<<20 ||
		p.MaxPathBytes == 0 || p.MaxPathBytes > 1024 ||
		p.MaxDepth == 0 || p.MaxDepth > 64 ||
		p.MaxProjections == 0 || p.MaxProjections > 128 || p.MaxAcquisitionSeconds == 0 || p.MaxAcquisitionSeconds > 3600 {
		return ErrCodeWorkspaceInvalid
	}
	return nil
}

var (
	ErrCodeWorkspaceInvalid             = errors.New("Code workspace identity or content is invalid")
	ErrCodeWorkspaceUnavailable         = errors.New("Code workspace content is unavailable")
	ErrCodeWorkspaceCapability          = errors.New("the saved toolkit has no supported repository read capability")
	ErrCodeWorkspaceWritableUnavailable = errors.New("Code repository readwrite requires durable quota-backed workspace storage; use read with bounded scratch")
)

// Bounds errors contain safe operator limits. They contain no repository data.
type CodeWorkspaceBoundsError struct {
	Bound string
	Limit uint64
}

func (e *CodeWorkspaceBoundsError) Error() string {
	return fmt.Sprintf("Code workspace exceeds operator policy %s=%d; select fewer files or change operator policy", e.Bound, e.Limit)
}

type CodeWorkspaceMode string

const (
	CodeWorkspaceRead      CodeWorkspaceMode = "read"
	CodeWorkspaceReadwrite CodeWorkspaceMode = "readwrite"
)

// Selection has no project, URL, branch, or credential fields.
type CodeWorkspaceSelection struct {
	ToolkitID              int32             `json:"toolkit_id"`
	ToolkitReferenceSHA256 string            `json:"toolkit_reference_sha256"`
	RepositoryID           string            `json:"repository_id"`
	Commit                 string            `json:"commit"`
	Mode                   CodeWorkspaceMode `json:"mode"`
	Include                []string          `json:"include"`
}

func workspaceHex(value string, lengths ...int) bool {
	valid := false
	for _, length := range lengths {
		valid = valid || len(value) == length
	}
	if !valid {
		return false
	}
	for _, b := range []byte(value) {
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}

// Conservative ASCII paths prevent platform separators and ambiguous names.
func workspacePath(value string, p CodeWorkspacePolicy) error {
	if value == "" || value[0] == '/' || strings.HasSuffix(value, "/") {
		return ErrCodeWorkspaceInvalid
	}
	if uint64(len(value)) > uint64(p.MaxPathBytes) {
		return &CodeWorkspaceBoundsError{"max_path_bytes", uint64(p.MaxPathBytes)}
	}
	parts := strings.Split(value, "/")
	if uint64(len(parts)) > uint64(p.MaxDepth) {
		return &CodeWorkspaceBoundsError{"max_depth", uint64(p.MaxDepth)}
	}
	for _, part := range parts {
		lower := strings.ToLower(part)
		if part == "" || part == "." || part == ".." || lower == ".git" || strings.HasPrefix(lower, ".elitea") {
			return ErrCodeWorkspaceInvalid
		}
		for _, b := range []byte(part) {
			if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.ContainsRune("._-+@", rune(b))) {
				return ErrCodeWorkspaceInvalid
			}
		}
	}
	return nil
}

func (s CodeWorkspaceSelection) Validate(p CodeWorkspacePolicy) error {
	if p.Validate() != nil || s.ToolkitID <= 0 || !workspaceHex(s.ToolkitReferenceSHA256, 64) ||
		!workspaceHex(s.Commit, 40, 64) || s.Mode != CodeWorkspaceRead && s.Mode != CodeWorkspaceReadwrite ||
		s.RepositoryID == "" || len(s.RepositoryID) > 128 || !sandboxASCII(s.RepositoryID) ||
		strings.ContainsAny(s.RepositoryID, "/\\\x00\r\n ") || s.Include == nil || len(s.Include) == 0 {
		return ErrCodeWorkspaceInvalid
	}
	if uint64(len(s.Include)) > uint64(p.MaxProjections) {
		return &CodeWorkspaceBoundsError{"max_projections", uint64(p.MaxProjections)}
	}
	for index, path := range s.Include {
		if err := workspacePath(path, p); err != nil {
			return err
		}
		if index > 0 && path <= s.Include[index-1] {
			return ErrCodeWorkspaceInvalid
		}
		for _, previous := range s.Include[:index] {
			if strings.HasPrefix(path, previous+"/") {
				return ErrCodeWorkspaceInvalid
			}
		}
	}
	return nil
}

func (s CodeWorkspaceSelection) contains(path string) bool {
	for _, include := range s.Include {
		if path == include || strings.HasPrefix(path, include+"/") {
			return true
		}
	}
	return false
}

func workspaceIdentity(domain string, canonical []byte) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(domain))
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(canonical)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write(canonical)
	return hex.EncodeToString(hash.Sum(nil))
}

func (p CodeWorkspacePolicy) SHA256() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	canonical, err := sandboxJSON(p)
	if err != nil {
		return "", ErrCodeWorkspaceInvalid
	}
	return workspaceIdentity("elitea.code.workspace-policy.v1\x00", canonical), nil
}

type CodeWorkspaceFile struct {
	Path       string `json:"path"`
	Bytes      uint64 `json:"bytes"`
	SHA256     string `json:"sha256"`
	Executable bool   `json:"executable"`
}

type workspaceContent struct {
	Revision  uint32                 `json:"revision"`
	Selection CodeWorkspaceSelection `json:"selection"`
	Policy    CodeWorkspacePolicy    `json:"policy"`
	Files     []CodeWorkspaceFile    `json:"files"`
}
type workspaceRecord struct {
	workspaceContent
	Root string `json:"root"`
}

// Manifest is immutable and contains no credentials. It grants no authority.
type CodeWorkspaceManifest struct {
	record workspaceRecord
	native []byte
}

func (m *CodeWorkspaceManifest) Policy() CodeWorkspacePolicy {
	return m.record.Policy
}

func NewCodeWorkspaceManifest(selection CodeWorkspaceSelection, policy CodeWorkspacePolicy, files []CodeWorkspaceFile) (*CodeWorkspaceManifest, error) {
	if err := selection.Validate(policy); err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, ErrCodeWorkspaceInvalid
	}
	if uint64(len(files)) > uint64(policy.MaxFiles) {
		return nil, &CodeWorkspaceBoundsError{"max_files", uint64(policy.MaxFiles)}
	}
	selection.Include = append([]string(nil), selection.Include...)
	files = append([]CodeWorkspaceFile(nil), files...)
	var total uint64
	blobSizes := map[string]uint64{}
	for index, file := range files {
		if err := workspacePath(file.Path, policy); err != nil {
			return nil, err
		}
		if !selection.contains(file.Path) || !workspaceHex(file.SHA256, 64) || index > 0 && file.Path <= files[index-1].Path {
			return nil, ErrCodeWorkspaceInvalid
		}
		if file.Bytes > policy.MaxFileBytes {
			return nil, &CodeWorkspaceBoundsError{"max_file_bytes", policy.MaxFileBytes}
		}
		if file.Bytes > policy.MaxTotalBytes-total {
			return nil, &CodeWorkspaceBoundsError{"max_total_bytes", policy.MaxTotalBytes}
		}
		total += file.Bytes
		if size, exists := blobSizes[file.SHA256]; exists && size != file.Bytes {
			return nil, ErrCodeWorkspaceInvalid
		}
		blobSizes[file.SHA256] = file.Bytes
		for _, previous := range files[:index] {
			if strings.HasPrefix(file.Path, previous.Path+"/") {
				return nil, ErrCodeWorkspaceInvalid
			}
		}
	}
	content := workspaceContent{1, selection, policy, files}
	canonical, err := sandboxJSON(content)
	if err != nil {
		return nil, ErrCodeWorkspaceInvalid
	}
	root := workspaceIdentity("elitea.code.workspace-manifest.v1\x00", canonical)
	record := workspaceRecord{content, root}
	native, err := sandboxJSON(record)
	if err != nil {
		return nil, ErrCodeWorkspaceInvalid
	}
	if uint64(len(native)) > uint64(policy.MaxManifestBytes) {
		return nil, &CodeWorkspaceBoundsError{"max_manifest_bytes", uint64(policy.MaxManifestBytes)}
	}
	return &CodeWorkspaceManifest{record, native}, nil
}

func ParseCodeWorkspaceManifest(native []byte, expectedRoot string, policy CodeWorkspacePolicy) (*CodeWorkspaceManifest, error) {
	if policy.Validate() != nil || len(native) > int(policy.MaxManifestBytes) || !workspaceHex(expectedRoot, 64) {
		return nil, ErrCodeWorkspaceInvalid
	}
	var record workspaceRecord
	decoder := json.NewDecoder(bytes.NewReader(native))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || decoder.Decode(new(any)) != io.EOF || record.Revision != 1 || record.Policy != policy {
		return nil, ErrCodeWorkspaceInvalid
	}
	manifest, err := NewCodeWorkspaceManifest(record.Selection, policy, record.Files)
	if err != nil {
		return nil, err
	}
	// Exact canonical bytes also reject duplicate JSON members and alternate roots.
	if manifest.Digest() != expectedRoot || record.Root != expectedRoot || !bytes.Equal(native, manifest.native) {
		return nil, ErrCodeWorkspaceInvalid
	}
	return manifest, nil
}

func (m *CodeWorkspaceManifest) Digest() string { return m.record.Root }
func (m *CodeWorkspaceManifest) Bytes() []byte  { return append([]byte(nil), m.native...) }
func (m *CodeWorkspaceManifest) Selection() CodeWorkspaceSelection {
	s := m.record.Selection
	s.Include = append([]string(nil), s.Include...)
	return s
}
func (m *CodeWorkspaceManifest) Files() []CodeWorkspaceFile {
	return append([]CodeWorkspaceFile(nil), m.record.Files...)
}
func (m *CodeWorkspaceManifest) nativeBytes() []byte   { return m.native }
func (m *CodeWorkspaceManifest) metadataName() string  { return "elitea-code-workspace.json" }
func (m *CodeWorkspaceManifest) storagePrefix() string { return "workspaces/" }
func (m *CodeWorkspaceManifest) recordedFiles() []sandboxBundleFile {
	unique := map[string]sandboxBundleFile{}
	for _, file := range m.record.Files {
		unique[file.SHA256] = sandboxBundleFile{"data/" + file.SHA256, int64(file.Bytes), file.SHA256}
	}
	files := make([]sandboxBundleFile, 0, len(unique))
	for _, file := range unique {
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files
}
func (m *CodeWorkspaceManifest) file(name string) (sandboxBundleFile, error) {
	for _, file := range m.recordedFiles() {
		if file.Name == name {
			return file, nil
		}
	}
	return sandboxBundleFile{}, ErrContentRejected
}
