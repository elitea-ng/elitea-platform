package runtime

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

const SnapshotDescriptorLimit = 16 * 1024
const SnapshotExecutableLimit = 32 * 1024 * 1024

var ErrSnapshotInvalid = errors.New("compiled snapshot contract is invalid")

// RustSnapshotBinding preserves the runner's exact field order and JSON names.
// Scope comes from verified execution authority. Profiles come from trusted release metadata.
type RustSnapshotBinding struct {
	Revision                  uint8  `json:"revision"`
	ReusePolicy               string `json:"reuse_policy"`
	TenantID                  string `json:"tenant_id"`
	ProjectID                 int32  `json:"project_id"`
	BasePreparedRequestSHA256 string `json:"base_prepared_request_sha256"`
	SourceSHA256              string `json:"source_sha256"`
	CompilationImageDigest    string `json:"compilation_image_digest"`
	ExecutionImageDigest      string `json:"execution_image_digest"`
	Platform                  string `json:"platform"`
	Target                    string `json:"target"`
	PolicyRevision            string `json:"policy_revision"`
	CargoManifestSHA256       string `json:"cargo_manifest_sha256"`
	CargoLockSHA256           string `json:"cargo_lock_sha256"`
	CargoConfigSHA256         string `json:"cargo_config_sha256"`
	VendorSHA256              string `json:"vendor_sha256"`
	ToolchainSHA256           string `json:"toolchain_sha256"`
	AdapterSHA256             string `json:"adapter_sha256"`
	WrapperSHA256             string `json:"wrapper_sha256"`
	CompilerFlagsSHA256       string `json:"compiler_flags_sha256"`
}

func SnapshotDigest(v string) bool {
	if len(v) != 64 {
		return false
	}
	for _, b := range []byte(v) {
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}
func snapshotIdentity(v string, policy bool) bool {
	if len(v) == 0 || len(v) > 128 {
		return false
	}
	for _, b := range []byte(v) {
		if (b < 'a' || b > 'z') && (b < 'A' || b > 'Z') && (b < '0' || b > '9') && b != '_' && b != '-' && (!policy || b != '.') {
			return false
		}
	}
	return true
}
func (b RustSnapshotBinding) Validate() error {
	if b.Revision != 1 || b.ReusePolicy != "snapshot_v1" || !snapshotIdentity(b.TenantID, false) || b.ProjectID <= 0 || !snapshotIdentity(b.PolicyRevision, true) || b.CompilationImageDigest != b.ExecutionImageDigest || !strings.HasPrefix(b.CompilationImageDigest, "sha256:") || !SnapshotDigest(strings.TrimPrefix(b.CompilationImageDigest, "sha256:")) {
		return ErrSnapshotInvalid
	}
	if (b.Platform != "linux/arm64/gnu" || b.Target != "aarch64-unknown-linux-gnu") && (b.Platform != "linux/amd64/gnu" || b.Target != "x86_64-unknown-linux-gnu") {
		return ErrSnapshotInvalid
	}
	for _, v := range []string{b.BasePreparedRequestSHA256, b.SourceSHA256, b.CargoManifestSHA256, b.CargoLockSHA256, b.CargoConfigSHA256, b.VendorSHA256, b.ToolchainSHA256, b.AdapterSHA256, b.WrapperSHA256, b.CompilerFlagsSHA256} {
		if !SnapshotDigest(v) {
			return ErrSnapshotInvalid
		}
	}
	return nil
}
func SnapshotJSON(v any) ([]byte, error) {
	var out bytes.Buffer
	e := json.NewEncoder(&out)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte{'\n'}), nil
}
func SnapshotHash(domain string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(domain))
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(body)))
	h.Write(size[:])
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}
func SnapshotContentSHA256(body []byte) string {
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}
func (b RustSnapshotBinding) Key() (string, error) {
	if err := b.Validate(); err != nil {
		return "", err
	}
	body, err := SnapshotJSON(b)
	if err != nil {
		return "", err
	}
	return SnapshotHash("elitea.rust.compiled-snapshot-key.v1\x00", body), nil
}
func snapshotDecode(body []byte, out any) error {
	if len(body) == 0 || len(body) > SnapshotDescriptorLimit {
		return ErrSnapshotInvalid
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		return ErrSnapshotInvalid
	}
	canonical, err := SnapshotJSON(out)
	if err != nil || !bytes.Equal(body, canonical) {
		return ErrSnapshotInvalid
	}
	return nil
}
func ParseRustSnapshotBinding(body []byte) (RustSnapshotBinding, error) {
	var b RustSnapshotBinding
	if snapshotDecode(body, &b) != nil || b.Validate() != nil {
		return b, ErrSnapshotInvalid
	}
	return b, nil
}

type RustSnapshotDescriptor struct {
	Revision          uint8               `json:"revision"`
	Binding           RustSnapshotBinding `json:"binding"`
	SnapshotKeySHA256 string              `json:"snapshot_key_sha256"`
	ExecutableSHA256  string              `json:"executable_sha256"`
	ExecutableBytes   int64               `json:"executable_bytes"`
}

func ParseRustSnapshotDescriptor(body []byte, root string) (RustSnapshotDescriptor, error) {
	var d RustSnapshotDescriptor
	if snapshotDecode(body, &d) != nil || !SnapshotDigest(root) || SnapshotContentSHA256(body) != root || d.Revision != 1 || !SnapshotDigest(d.ExecutableSHA256) || d.ExecutableBytes < 1 || d.ExecutableBytes > SnapshotExecutableLimit {
		return d, ErrSnapshotInvalid
	}
	key, err := d.Binding.Key()
	if err != nil || key != d.SnapshotKeySHA256 {
		return d, ErrSnapshotInvalid
	}
	return d, nil
}

// MatchPrepared binds exact original request bytes, including input and timeout.
// It never normalizes source text or replaces the prepared request fingerprint.
func (b RustSnapshotBinding) MatchPrepared(body []byte) (string, error) {
	if len(body) == 0 || len(body) > 1024*1024 || b.Validate() != nil || SnapshotHash("elitea.sandbox.prepared-job.v1\x00", body) != b.BasePreparedRequestSHA256 {
		return "", ErrSnapshotInvalid
	}
	var request struct {
		Revision uint8                      `json:"revision"`
		Language string                     `json:"language"`
		Source   string                     `json:"source"`
		Input    map[string]json.RawMessage `json:"input"`
		Image    string                     `json:"image_digest"`
		Policy   string                     `json:"policy_revision"`
		Timeout  uint64                     `json:"timeout_seconds"`
		Bundle   string                     `json:"dependency_bundle_sha256"`
	}
	if json.Unmarshal(body, &request) != nil || (request.Revision != 1 && request.Revision != 3) || request.Language != "rust" || request.Input == nil || len(request.Source) > 256*1024 || request.Timeout < 1 || request.Timeout > 3600 || request.Image != b.ExecutionImageDigest || request.Policy != b.PolicyRevision || SnapshotContentSHA256([]byte(request.Source)) != b.SourceSHA256 {
		return "", ErrSnapshotInvalid
	}
	if request.Revision == 1 && request.Bundle != "" || request.Revision == 3 && !SnapshotDigest(request.Bundle) {
		return "", ErrSnapshotInvalid
	}
	return request.Bundle, nil
}

// SnapshotJobDigest separates compile and execute intent from the base Code request.
// Publication/read use the same exact selected execution intent and descriptor root.
func SnapshotJobDigest(purpose string, b RustSnapshotBinding, root string) (string, error) {
	key, err := b.Key()
	if err != nil {
		return "", err
	}
	if purpose != "compile" && purpose != "execute" {
		return "", ErrSnapshotInvalid
	}
	if purpose == "compile" && root != "" || purpose == "execute" && !SnapshotDigest(root) {
		return "", ErrSnapshotInvalid
	}
	body, err := SnapshotJSON(struct {
		Revision uint8  `json:"revision"`
		Purpose  string `json:"purpose"`
		Base     string `json:"base_prepared_request_sha256"`
		Key      string `json:"snapshot_key_sha256"`
		Root     string `json:"descriptor_sha256,omitempty"`
	}{1, purpose, b.BasePreparedRequestSHA256, key, root})
	if err != nil {
		return "", err
	}
	return SnapshotHash("elitea.sandbox.compiled-job.v1\x00", body), nil
}
func SnapshotActivationKey(execution, activation string) string {
	h := sha256.New()
	h.Write([]byte("elitea.sandbox.activation.v1\x00"))
	var size [8]byte
	for _, v := range []string{execution, activation} {
		binary.BigEndian.PutUint64(size[:], uint64(len(v)))
		h.Write(size[:])
		h.Write([]byte(v))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// RustSnapshotProfile is trusted release metadata. Requests cannot add a profile.
type RustSnapshotProfile struct {
	Binding                RustSnapshotBinding
	DependencyBundleSHA256 string
}
type RustSnapshotProfiles struct{ profiles []RustSnapshotProfile }

func NewRustSnapshotProfiles(profiles []RustSnapshotProfile) (*RustSnapshotProfiles, error) {
	if len(profiles) == 0 || len(profiles) > 64 {
		return nil, ErrSnapshotInvalid
	}
	copyProfiles := append([]RustSnapshotProfile(nil), profiles...)
	for _, p := range copyProfiles {
		if p.Binding.Validate() != nil || p.DependencyBundleSHA256 != "" && !SnapshotDigest(p.DependencyBundleSHA256) {
			return nil, ErrSnapshotInvalid
		}
	}
	return &RustSnapshotProfiles{copyProfiles}, nil
}
func (p *RustSnapshotProfiles) Validate(b RustSnapshotBinding, bundle string) error {
	if p == nil || b.Validate() != nil {
		return ErrSnapshotInvalid
	}
	for _, profile := range p.profiles {
		expected := profile.Binding
		expected.TenantID = b.TenantID
		expected.ProjectID = b.ProjectID
		expected.BasePreparedRequestSHA256 = b.BasePreparedRequestSHA256
		expected.SourceSHA256 = b.SourceSHA256
		if expected == b && profile.DependencyBundleSHA256 == bundle {
			return nil
		}
	}
	return ErrSnapshotInvalid
}
