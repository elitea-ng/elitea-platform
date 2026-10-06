package codesandbox

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	recovery "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
)

const ResultAudience = "elitea.runtime.code-sandbox-whole-result.v1"
const IntentDomain = "elitea.sandbox.original-code-intent.ed25519.v1\x00"
const RecoveryDomain = "elitea.sandbox.node-code-recovery-grant.ed25519.v1\x00"
const MaxReceiptBytes = 1024 * 1024

var ErrRejected = errors.New("original Code authority denied")

type OriginalVisitRef struct {
	VisitID      string `json:"visit_id"`
	Revision     uint8  `json:"revision"`
	DigestSHA256 string `json:"digest_sha256"`
}

func (r OriginalVisitRef) Validate() error {
	if r.Revision != 1 || !NonzeroDigest(r.VisitID) || !NonzeroDigest(r.DigestSHA256) {
		return ErrRejected
	}
	return nil
}

type VisitRequest struct {
	Schema                        string          `json:"schema"`
	ActivationID                  string          `json:"activation_id"`
	NodeID                        string          `json:"node_id"`
	GraphThread                   string          `json:"graph_thread"`
	Step                          uint64          `json:"step"`
	Attempt                       uint16          `json:"attempt"`
	NodeDigest                    string          `json:"node_digest"`
	OwningYAMLSHA256              string          `json:"owning_yaml_sha256"`
	ConfigurationBase64URL        string          `json:"exact_configuration_json_base64url"`
	PreWorkspacePreparedBase64URL string          `json:"pre_workspace_prepared_job_json_base64url"`
	SavedChildScope               json.RawMessage `json:"saved_child_scope"`
}
type VisitResponse struct {
	Schema                     string           `json:"schema"`
	OriginalVisit              OriginalVisitRef `json:"original_visit"`
	ExecutionID                string           `json:"execution_id"`
	OriginalGeneration         uint64           `json:"original_generation"`
	ActivationID               string           `json:"activation_id"`
	Attempt                    uint16           `json:"attempt"`
	NodeDigest                 string           `json:"node_digest"`
	PreWorkspacePreparedSHA256 string           `json:"pre_workspace_prepared_sha256"`
}
type IntentRequest struct {
	Schema                   string           `json:"schema"`
	OriginalVisit            OriginalVisitRef `json:"original_visit"`
	DispatchActivation       string           `json:"dispatch_activation"`
	RequestDigest            string           `json:"request_digest"`
	SupervisorAudience       string           `json:"supervisor_audience"`
	PreparedBase64URL        string           `json:"prepared_job_json_base64url"`
	CompiledBindingBase64URL *string          `json:"compiled_binding_json_base64url"`
	SelectedDescriptorSHA256 *string          `json:"selected_descriptor_sha256"`
}

type SignedGrant struct {
	Schema             string `json:"schema"`
	KeyID              string `json:"key_id"`
	ClaimsBase64URL    string `json:"claims_base64url"`
	SignatureBase64URL string `json:"signature_base64url"`
}

type IntentResponse struct {
	Schema string      `json:"schema"`
	Intent SignedGrant `json:"intent"`
}

type Binding struct {
	Schema             string `json:"schema"`
	Purpose            string `json:"purpose"`
	ExecutionID        string `json:"execution_id"`
	OriginalGeneration uint64 `json:"original_generation"`
	ActivationID       string `json:"activation_id"`
	NodeID             string `json:"node_id"`
	GraphThread        string `json:"graph_thread"`
	Step               uint64 `json:"step"`
	Attempt            uint16 `json:"attempt"`
	DispatchActivation string `json:"dispatch_activation"`
	JobKey             string `json:"job_key"`
	RequestDigest      string `json:"request_digest"`
	SupervisorAudience string `json:"supervisor_audience"`
	NodeDigest         string `json:"node_digest"`
	Language           string `json:"language"`
	PreparedSHA256     string `json:"prepared_job_sha256"`
	SourceSHA256       string `json:"source_sha256"`
	InputSHA256        string `json:"input_sha256"`
}

type Visit struct {
	ActivationID     string `json:"activation_id"`
	NodeID           string `json:"node_id"`
	GraphThread      string `json:"graph_thread"`
	Step             uint64 `json:"step"`
	Attempt          uint16 `json:"attempt"`
	ExpectedRevision uint64 `json:"expected_revision"`
	ReceiptSHA256    string `json:"receipt_sha256"`
}

type Receipt struct {
	Schema          string  `json:"schema"`
	Kind            string  `json:"kind"`
	Binding         Binding `json:"binding"`
	Visit           Visit   `json:"visit"`
	ResultBase64URL string  `json:"result_json_base64url,omitempty"`
	ResultSHA256    string  `json:"result_sha256,omitempty"`
	SealID          string  `json:"seal_id,omitempty"`
}

type Response struct {
	Schema  string          `json:"schema"`
	State   string          `json:"state"`
	Receipt json.RawMessage `json:"receipt"`
}

type OwnerRequest struct {
	Schema string      `json:"schema"`
	Grant  SignedGrant `json:"grant"`
}

type GrantClaims struct {
	Schema                    string `json:"schema"`
	TenantID                  string `json:"tenant_id"`
	ProjectID                 int64  `json:"project_id"`
	ExecutionID               string `json:"execution_id"`
	OriginalGeneration        uint64 `json:"original_generation"`
	ClaimID                   string `json:"claim_id"`
	ClaimAttempt              uint64 `json:"claim_attempt"`
	LeaseEpoch                uint64 `json:"lease_epoch"`
	FenceSHA256               string `json:"fence_sha256"`
	ActivationID              string `json:"activation_id"`
	NodeID                    string `json:"node_id"`
	GraphThread               string `json:"graph_thread"`
	Step                      uint64 `json:"step"`
	Attempt                   uint16 `json:"attempt"`
	ExpectedRevision          uint64 `json:"expected_revision"`
	ReceiptSHA256             string `json:"receipt_sha256"`
	DispatchActivation        string `json:"dispatch_activation"`
	JobKey                    string `json:"job_key"`
	RequestDigest             string `json:"request_digest"`
	BindingSHA256             string `json:"binding_sha256"`
	SupervisorAudience        string `json:"supervisor_audience"`
	RequesterWorkloadIdentity string `json:"requester_workload_identity"`
	Operation                 string `json:"operation"`
	IssuedAtMillis            int64  `json:"issued_at_unix_millis"`
	ExpiresAtMillis           int64  `json:"expires_at_unix_millis"`
}

type IntentClaims struct {
	Schema                    string `json:"schema"`
	Purpose                   string `json:"purpose"`
	TenantID                  string `json:"tenant_id"`
	ProjectID                 int64  `json:"project_id"`
	ExecutionID               string `json:"execution_id"`
	OriginalGeneration        uint64 `json:"original_generation"`
	ClaimID                   string `json:"claim_id"`
	ClaimAttempt              uint64 `json:"claim_attempt"`
	LeaseEpoch                uint64 `json:"lease_epoch"`
	FenceSHA256               string `json:"fence_sha256"`
	ActivationID              string `json:"activation_id"`
	NodeID                    string `json:"node_id"`
	GraphThread               string `json:"graph_thread"`
	Step                      uint64 `json:"step"`
	Attempt                   uint16 `json:"attempt"`
	NodeDigest                string `json:"node_digest"`
	DispatchActivation        string `json:"dispatch_activation"`
	JobKey                    string `json:"job_key"`
	RequestDigest             string `json:"request_digest"`
	SupervisorAudience        string `json:"supervisor_audience"`
	SubmitterWorkloadIdentity string `json:"submitter_workload_identity"`
	Language                  string `json:"language"`
	PreparedSHA256            string `json:"prepared_job_sha256"`
	SourceSHA256              string `json:"source_sha256"`
	InputSHA256               string `json:"input_sha256"`
	IssuedAtMillis            int64  `json:"issued_at_unix_millis"`
	ExpiresAtMillis           int64  `json:"expires_at_unix_millis"`
}

func Digest(raw []byte) string    { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func NonzeroDigest(v string) bool { return recovery.ValidID(v) && v != strings.Repeat("0", 64) }
func Identity(v string) bool {
	if len(v) == 0 || len(v) > 256 || !utf8.ValidString(v) {
		return false
	}
	for _, c := range v {
		if unicode.IsControl(c) || unicode.IsSpace(c) {
			return false
		}
	}
	return true
}
func JobKey(execution, activation string) string {
	h := sha256.New()
	h.Write([]byte("elitea.sandbox.activation.v1\x00"))
	for _, part := range []string{execution, activation} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}
func DispatchActivation(activation string, attempt uint16) string {
	raw, err := hex.DecodeString(activation)
	if err != nil || len(raw) != 32 || attempt == 0 {
		return ""
	}
	h := sha256.New()
	h.Write([]byte("elitea.pipeline.node-attempt-dispatch.v1\x00"))
	h.Write(raw)
	var n [2]byte
	binary.BigEndian.PutUint16(n[:], attempt)
	h.Write(n[:])
	return hex.EncodeToString(h.Sum(nil))
}
func (b Binding) Validate() error {
	if b.Schema != "elitea.sandbox.whole-code-binding.v1" || b.Purpose != "whole_code_execute" || !recovery.ValidExecutionID(b.ExecutionID) || b.OriginalGeneration == 0 || b.OriginalGeneration > math.MaxInt64 || !VisitBounds(b.ActivationID, b.NodeID, b.GraphThread, b.Step, b.Attempt) || !NonzeroDigest(b.DispatchActivation) || b.JobKey != JobKey(b.ExecutionID, b.DispatchActivation) || !NonzeroDigest(b.RequestDigest) || !Identity(b.SupervisorAudience) || !NonzeroDigest(b.NodeDigest) || !NonzeroDigest(b.PreparedSHA256) || !NonzeroDigest(b.SourceSHA256) || !NonzeroDigest(b.InputSHA256) {
		return ErrRejected
	}
	switch b.Language {
	case "python", "javascript", "typescript", "rust":
		return nil
	}
	return ErrRejected
}
func (v Visit) Matches(r recovery.Receipt, raw []byte) bool {
	return NonzeroDigest(v.ActivationID) && v.ActivationID == r.ActivationID && v.NodeID == r.NodeID && v.GraphThread == r.GraphThread && v.Step == r.Step && v.Attempt == r.Attempt && v.ExpectedRevision == r.JournalRevision && v.ReceiptSHA256 == Digest(raw)
}

// Canonical emits compact recursively sorted JSON. Protocol text excludes line separators.
func Canonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, ErrRejected
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	if d.Decode(&value) != nil {
		return nil, ErrRejected
	}
	var out bytes.Buffer
	e := json.NewEncoder(&out)
	e.SetEscapeHTML(false)
	if e.Encode(value) != nil {
		return nil, ErrRejected
	}
	return bytes.TrimSuffix(out.Bytes(), []byte{'\n'}), nil
}

// Decode checks duplicates at every depth before strict typed decoding.
func Decode(raw []byte, target any, max int) error {
	if len(raw) == 0 || len(raw) > max || !utf8.Valid(raw) {
		return ErrRejected
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	count := 0
	if walk(d, 0, &count) != nil {
		return ErrRejected
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrRejected
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	d.DisallowUnknownFields()
	if d.Decode(target) != nil {
		return ErrRejected
	}
	return nil
}
func walk(d *json.Decoder, depth int, count *int) error {
	*count++
	if depth > 32 || *count > 100000 {
		return ErrRejected
	}
	t, err := d.Token()
	if err != nil {
		return ErrRejected
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, err := d.Token()
			key, ok := k.(string)
			if err != nil || !ok || seen[key] {
				return ErrRejected
			}
			seen[key] = true
			if walk(d, depth+1, count) != nil {
				return ErrRejected
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return ErrRejected
		}
	case '[':
		for d.More() {
			if walk(d, depth+1, count) != nil {
				return ErrRejected
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return ErrRejected
		}
	default:
		return ErrRejected
	}
	return nil
}
func DecodeBase64(v string, max int) ([]byte, error) {
	if len(v) == 0 || len(v) > base64.RawURLEncoding.EncodedLen(max) {
		return nil, ErrRejected
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(v)
	if err != nil || len(raw) > max || base64.RawURLEncoding.EncodeToString(raw) != v {
		return nil, ErrRejected
	}
	return raw, nil
}
func DecodeReceipt(raw []byte, binding Binding, visit Visit) (Receipt, error) {
	var r Receipt
	if Decode(raw, &r, MaxReceiptBytes) != nil || r.Schema != "elitea.sandbox.whole-code-recovery-receipt.v1" || r.Binding != binding || r.Visit != visit || binding.Validate() != nil || binding.ActivationID != visit.ActivationID || binding.NodeID != visit.NodeID || binding.GraphThread != visit.GraphThread || binding.Step != visit.Step || binding.Attempt != visit.Attempt {
		return Receipt{}, ErrRejected
	}
	canonical, err := Canonical(r)
	if err != nil || !bytes.Equal(canonical, raw) {
		return Receipt{}, ErrRejected
	}
	switch r.Kind {
	case "committed_result":
		data, err := DecodeBase64(r.ResultBase64URL, 512*1024)
		if err != nil || len(data) == 0 || !json.Valid(data) || Digest(data) != r.ResultSHA256 || r.SealID != "" {
			return Receipt{}, ErrRejected
		}
	case "verified_no_effect":
		if r.ResultBase64URL != "" || r.ResultSHA256 != "" || !NonzeroDigest(r.SealID) {
			return Receipt{}, ErrRejected
		}
		exact, err := Canonical([]any{binding, visit})
		if err != nil {
			return Receipt{}, ErrRejected
		}
		h := sha256.New()
		h.Write([]byte("elitea.sandbox.whole-code-no-effect-seal.v1\x00"))
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(exact)))
		h.Write(n[:])
		h.Write(exact)
		if r.SealID != hex.EncodeToString(h.Sum(nil)) {
			return Receipt{}, ErrRejected
		}
	default:
		return Receipt{}, ErrRejected
	}
	return r, nil
}

// RequiredFields rejects omitted fields and explicit null except declared nullable selectors.
func RequiredFields(raw []byte, keys []string, nullable ...string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != len(keys) {
		return false
	}
	allowed := map[string]bool{}
	for _, k := range nullable {
		allowed[k] = true
	}
	for _, k := range keys {
		v, ok := fields[k]
		if !ok || !allowed[k] && bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return false
		}
	}
	return true
}

// VisitBounds validates identifiers only; it grants no journal/checkpoint authority.
func VisitBounds(activation, node, thread string, step uint64, attempt uint16) bool {
	if !NonzeroDigest(activation) || len(node) == 0 || len(node) > 128 || len(thread) == 0 || len(thread) > 512 || !utf8.ValidString(thread) || step > math.MaxInt64 || attempt < 1 || attempt > 16 {
		return false
	}
	for _, c := range node {
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') && !strings.ContainsRune("_.:-", c) {
			return false
		}
	}
	for _, c := range thread {
		if unicode.IsControl(c) || c == '\u2028' || c == '\u2029' {
			return false
		}
	}
	return true
}
