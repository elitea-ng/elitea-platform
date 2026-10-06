package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// This parser validates content. Original visit and grant owners authorize effects.
// Fingerprint uses the exact Rust request bytes, not a Go reserialization.
type CodePreparedRequest struct {
	Revision                uint8
	Language                string
	Source                  string
	Input                   json.RawMessage
	ImageDigest             string
	PolicyRevision          string
	TimeoutSeconds          uint32
	DependencyBundleSHA256  string
	Fingerprint             string
	PreparedSHA256          string
	PreWorkspaceFingerprint string
	PreWorkspaceSHA256      string
	PreWorkspaceBytes       []byte
	Workspace               *CodeWorkspaceBinding
	Broker                  *CodePreparedPlatformClient
}

type CodeWorkspaceBinding struct {
	Revision       uint32                 `json:"revision"`
	Selection      CodeWorkspaceSelection `json:"selection"`
	ManifestSHA256 string                 `json:"manifest_sha256"`
	PolicySHA256   string                 `json:"policy_sha256"`
}

type CodePreparedPlatformClient struct {
	Revision      uint8  `json:"revision"`
	PolicySHA256  string `json:"policy_sha256"`
	MaxCalls      uint16 `json:"max_calls"`
	MaxTotalBytes uint32 `json:"max_total_bytes"`
}

type codePreparedWire struct {
	Revision  uint8           `json:"revision"`
	Language  string          `json:"language"`
	Source    string          `json:"source"`
	Input     json.RawMessage `json:"input"`
	Image     string          `json:"image_digest"`
	Policy    string          `json:"policy_revision"`
	Timeout   uint32          `json:"timeout_seconds"`
	Bundle    json.RawMessage `json:"dependency_bundle_sha256,omitempty"`
	Native    json.RawMessage `json:"native_dependencies,omitempty"`
	Workspace json.RawMessage `json:"workspace,omitempty"`
	Broker    json.RawMessage `json:"platform_client,omitempty"`
}

func codePreparedDecode(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		return ErrCodeWorkspaceInvalid
	}
	return nil
}

// Token checks run before recursive maps, serialization, cloning, or hashing.
func codePreparedTokens(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	remaining := 120000
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 68 || remaining == 0 {
			return ErrCodeWorkspaceInvalid
		}
		remaining--
		token, err := decoder.Token()
		if err != nil {
			return ErrCodeWorkspaceInvalid
		}
		switch token {
		case json.Delim('{'):
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok || seen[name] {
					return ErrCodeWorkspaceInvalid
				}
				seen[name] = true
				if walk(depth+1) != nil {
					return ErrCodeWorkspaceInvalid
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return ErrCodeWorkspaceInvalid
			}
		case json.Delim('['):
			for decoder.More() {
				if walk(depth+1) != nil {
					return ErrCodeWorkspaceInvalid
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return ErrCodeWorkspaceInvalid
			}
		}
		return nil
	}
	if walk(0) != nil {
		return ErrCodeWorkspaceInvalid
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrCodeWorkspaceInvalid
	}
	return nil
}

func codePreparedPolicy(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, b := range []byte(value) {
		if (b < 'a' || b > 'z') && (b < 'A' || b > 'Z') && (b < '0' || b > '9') && !strings.ContainsRune("._-", rune(b)) {
			return false
		}
	}
	return true
}

func ParseCodePreparedRequest(raw []byte) (CodePreparedRequest, error) {
	empty := CodePreparedRequest{}
	if len(raw) == 0 || len(raw) > 1024*1024 || !utf8.Valid(raw) || codePreparedTokens(raw) != nil {
		return empty, ErrCodeWorkspaceInvalid
	}
	var wire codePreparedWire
	if codePreparedDecode(raw, &wire) != nil || strings.TrimSpace(wire.Source) == "" || len(wire.Source) > 256*1024 || strings.ContainsRune(wire.Source, '\x00') ||
		!strings.HasPrefix(wire.Image, "sha256:") || !workspaceHex(strings.TrimPrefix(wire.Image, "sha256:"), 64) || !codePreparedPolicy(wire.Policy) || wire.Timeout < 1 || wire.Timeout > 3600 {
		return empty, ErrCodeWorkspaceInvalid
	}
	switch wire.Language {
	case "python", "javascript", "typescript", "rust":
	default:
		return empty, ErrCodeWorkspaceInvalid
	}
	var input map[string]any
	if codePreparedDecode(wire.Input, &input) != nil || input == nil || len(input) > 256 {
		return empty, ErrCodeWorkspaceInvalid
	}
	remaining := 100000
	for _, value := range input {
		if codePreparedShape(value, 0, &remaining) != nil {
			return empty, ErrCodeWorkspaceInvalid
		}
	}
	baseRevision, err := codePreparedDependencyRevision(wire)
	if err != nil {
		return empty, err
	}
	var bundleSHA256 string
	if wire.Bundle != nil && codePreparedDecode(wire.Bundle, &bundleSHA256) != nil {
		return empty, ErrCodeWorkspaceInvalid
	}
	var workspace *CodeWorkspaceBinding
	if wire.Workspace != nil {
		workspace = new(CodeWorkspaceBinding)
		if codePreparedDecode(wire.Workspace, workspace) != nil || workspace.Revision != 1 || !workspaceHex(workspace.ManifestSHA256, 64) || !workspaceHex(workspace.PolicySHA256, 64) || workspace.Selection.Validate(codeWorkspaceCeilings()) != nil {
			return empty, ErrCodeWorkspaceInvalid
		}
		canonical, err := sandboxJSON(workspace)
		if err != nil || !bytes.Equal(canonical, wire.Workspace) {
			return empty, ErrCodeWorkspaceInvalid
		}
	}
	var broker *CodePreparedPlatformClient
	if wire.Broker != nil {
		broker = new(CodePreparedPlatformClient)
		if codePreparedDecode(wire.Broker, broker) != nil || broker.Revision != 1 || !workspaceHex(broker.PolicySHA256, 64) || broker.MaxCalls < 1 || broker.MaxCalls > 4096 || broker.MaxTotalBytes < 1 || broker.MaxTotalBytes > 64*1024*1024 {
			return empty, ErrCodeWorkspaceInvalid
		}
		canonical, err := sandboxJSON(broker)
		if err != nil || !bytes.Equal(canonical, wire.Broker) {
			return empty, ErrCodeWorkspaceInvalid
		}
	}
	expected := baseRevision
	if workspace != nil {
		expected = 4
	}
	if broker != nil {
		expected = 5
	}
	if wire.Revision != expected {
		return empty, ErrCodeWorkspaceInvalid
	}
	canonical, err := codePreparedBytes(wire)
	if err != nil || !bytes.Equal(canonical, raw) {
		return empty, ErrCodeWorkspaceInvalid
	}
	base := wire
	base.Workspace = nil
	base.Revision = baseRevision
	if broker != nil {
		base.Revision = 5
	}
	preWorkspace, err := codePreparedBytes(base)
	if err != nil {
		return empty, err
	}
	return CodePreparedRequest{Revision: wire.Revision, Language: wire.Language, Source: wire.Source, Input: bytes.Clone(wire.Input),
		ImageDigest: wire.Image, PolicyRevision: wire.Policy, TimeoutSeconds: wire.Timeout, DependencyBundleSHA256: bundleSHA256,
		Fingerprint: workspaceIdentity("elitea.sandbox.prepared-job.v1\x00", raw), PreparedSHA256: workspaceContentSHA256(raw),
		PreWorkspaceFingerprint: workspaceIdentity("elitea.sandbox.prepared-job.v1\x00", preWorkspace), PreWorkspaceSHA256: workspaceContentSHA256(preWorkspace),
		PreWorkspaceBytes: preWorkspace, Workspace: workspace, Broker: broker}, nil
}

func workspaceContentSHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func codeWorkspaceCeilings() CodeWorkspacePolicy {
	return CodeWorkspacePolicy{1, 4096, 4 << 20, 128 << 20, 4 << 20, 1024, 64, 128, 3600}
}

func codePreparedDependencyRevision(wire codePreparedWire) (uint8, error) {
	if wire.Bundle == nil && wire.Native == nil {
		return 1, nil
	}
	var root string
	if codePreparedDecode(wire.Bundle, &root) != nil || !workspaceHex(root, 64) {
		return 0, ErrCodeWorkspaceInvalid
	}
	if wire.Native == nil {
		if wire.Language == "python" {
			return 2, nil
		}
		return 0, ErrCodeWorkspaceInvalid
	}
	var native struct {
		Kind     string `json:"kind"`
		Platform struct {
			OS   string `json:"os"`
			Arch string `json:"arch"`
			ABI  string `json:"abi"`
		} `json:"platform"`
		Preparation  string          `json:"preparation_sha256"`
		Source       string          `json:"source_sha256"`
		Dependencies json.RawMessage `json:"dependencies_toml,omitempty"`
	}
	if codePreparedDecode(wire.Native, &native) != nil || native.Platform.OS != "linux" || (native.Platform.Arch != "amd64" && native.Platform.Arch != "arm64") || native.Platform.ABI != "gnu" || !workspaceHex(native.Preparation, 64) || !workspaceHex(native.Source, 64) {
		return 0, ErrCodeWorkspaceInvalid
	}
	acquisition := wire.Source
	switch {
	case (wire.Language == "javascript" || wire.Language == "typescript") && native.Kind == "deno" && native.Dependencies == nil:
	case wire.Language == "rust" && native.Kind == "cargo" && native.Dependencies != nil:
		if codePreparedDecode(native.Dependencies, &acquisition) != nil || acquisition == "" || len(acquisition) > 64*1024 || strings.ContainsRune(acquisition, '\x00') {
			return 0, ErrCodeWorkspaceInvalid
		}
	default:
		return 0, ErrCodeWorkspaceInvalid
	}
	if workspaceContentSHA256([]byte(acquisition)) != native.Source {
		return 0, ErrCodeWorkspaceInvalid
	}
	canonical, err := codePreparedOrderedJSON(native)
	if err != nil || !bytes.Equal(canonical, wire.Native) {
		return 0, ErrCodeWorkspaceInvalid
	}
	return 3, nil
}

func codePreparedShape(value any, depth int, remaining *int) error {
	if depth > 64 || *remaining == 0 {
		return ErrCodeWorkspaceInvalid
	}
	*remaining--
	switch v := value.(type) {
	case []any:
		for _, child := range v {
			if codePreparedShape(child, depth+1, remaining) != nil {
				return ErrCodeWorkspaceInvalid
			}
		}
	case map[string]any:
		for _, child := range v {
			if codePreparedShape(child, depth+1, remaining) != nil {
				return ErrCodeWorkspaceInvalid
			}
		}
	}
	return nil
}

// Preserve Rust serde field order and string escaping, including literal U+2028.
// json.RawMessage retains exact numbers and source bytes in the input record.
func codePreparedBytes(wire codePreparedWire) ([]byte, error) { return codePreparedOrderedJSON(wire) }
func codePreparedOrderedJSON(value any) ([]byte, error) {
	raw, err := sandboxJSON(value)
	if err != nil {
		return nil, ErrCodeWorkspaceInvalid
	}
	// Read struct field order from the encoder, then render each scalar with Rust rules.
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var out bytes.Buffer
	if codePreparedRender(d, &out, false, 0) != nil {
		return nil, ErrCodeWorkspaceInvalid
	}
	return out.Bytes(), nil
}
func codePreparedRender(d *json.Decoder, out *bytes.Buffer, sorted bool, depth int) error {
	if depth > 68 {
		return ErrCodeWorkspaceInvalid
	}
	token, err := d.Token()
	if err != nil {
		return ErrCodeWorkspaceInvalid
	}
	switch v := token.(type) {
	case json.Delim:
		switch v {
		case '{':
			type entry struct {
				key   string
				bytes []byte
			}
			var entries []entry
			for d.More() {
				key, err := d.Token()
				name, ok := key.(string)
				if err != nil || !ok {
					return ErrCodeWorkspaceInvalid
				}
				var child bytes.Buffer
				if codePreparedRender(d, &child, sorted || name == "input", depth+1) != nil {
					return ErrCodeWorkspaceInvalid
				}
				entries = append(entries, entry{name, child.Bytes()})
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return ErrCodeWorkspaceInvalid
			}
			if sorted {
				sort.Slice(entries, func(i, j int) bool { return entries[i].key < entries[j].key })
			}
			out.WriteByte('{')
			for i, e := range entries {
				if i > 0 {
					out.WriteByte(',')
				}
				codePreparedQuote(out, e.key)
				out.WriteByte(':')
				out.Write(e.bytes)
			}
			out.WriteByte('}')
		case '[':
			out.WriteByte('[')
			i := 0
			for d.More() {
				if i > 0 {
					out.WriteByte(',')
				}
				if codePreparedRender(d, out, sorted, depth+1) != nil {
					return ErrCodeWorkspaceInvalid
				}
				i++
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return ErrCodeWorkspaceInvalid
			}
			out.WriteByte(']')
		default:
			return ErrCodeWorkspaceInvalid
		}
	case string:
		codePreparedQuote(out, v)
	case json.Number:
		out.WriteString(v.String())
	case bool:
		out.WriteString(strconv.FormatBool(v))
	case nil:
		out.WriteString("null")
	default:
		return ErrCodeWorkspaceInvalid
	}
	return nil
}
func codePreparedQuote(out *bytes.Buffer, value string) {
	const digits = "0123456789abcdef"
	out.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"', '\\':
			out.WriteByte('\\')
			out.WriteRune(r)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if r < 32 {
				out.WriteString(`\u00`)
				out.WriteByte(digits[byte(r)>>4])
				out.WriteByte(digits[byte(r)&15])
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
}
