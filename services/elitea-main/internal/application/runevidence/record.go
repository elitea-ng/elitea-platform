// Package runevidence is the step record of a run (ADR-0042 decisions 1 and
// 2, issue #1499): one row per model call, tool call, retrieval, approval,
// delegation, cap, error and output the worker reports, hash-chained per run,
// with the step's inputs and outputs in object storage and only their
// digests in the row.
//
// # The chain
//
// The worker generates each step's step_id (the idempotency key) and reports
// steps in the order they happened (ReportRunSteps, under the live claim).
// Main assigns seq in arrival order per run and the hash
//
//	hash = sha256(prev_hash || Canonical(record))
//
// with prev_hash the previous step's hash (32 zero bytes for seq 1), in ONE
// transaction that holds the run's head row (elitea_runtime.run_step_heads,
// shared 0205). Canonical covers every column of the row except the content's
// object keys and expiry: the content itself is covered by its SHA-256, so
// deleting it under the step_content policy leaves a chain that still
// verifies, and an auditor sees "content expired at T under policy P", not a
// gap. VerifyChain recomputes the chain from the rows and names the first seq
// that does not verify.
//
// # The anchor
//
// When the run settles, the head (its hash and step count) is appended to the
// tenant's security audit chain as ai.elitea.security.v1.run.steps.anchored,
// so a run whose rows were all rewritten, chain included, no longer matches
// the tenant chain. The appender and the 0183 outbox exist on every install,
// whatever the credential governance profile; see Store.Anchor.
package runevidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// Kind is a step kind (the CHECK of run_steps.kind).
type Kind string

// The step kinds.
const (
	KindModelCall  Kind = "model_call"
	KindToolCall   Kind = "tool_call"
	KindRetrieval  Kind = "retrieval"
	KindApproval   Kind = "approval"
	KindDelegation Kind = "delegation"
	KindCap        Kind = "cap"
	KindError      Kind = "error"
	KindOutput     Kind = "output"
)

// Valid reports whether k is one of the eight kinds.
func (k Kind) Valid() bool {
	switch k {
	case KindModelCall, KindToolCall, KindRetrieval, KindApproval, KindDelegation, KindCap, KindError, KindOutput:
		return true
	}
	return false
}

// Side names one content side of a step.
type Side string

// The content sides.
const (
	SideInputs  Side = "inputs"
	SideOutputs Side = "outputs"
)

// Media types a content side may carry.
const (
	MediaJSON = "application/json"
	MediaText = "text/plain"
)

// ContentMeta is what a step row keeps of one content side. Digest is nil
// when the side was empty or filtered to nothing; ObjectKey is empty when the
// bytes are not (or no longer) stored.
type ContentMeta struct {
	Digest       []byte
	Size         int64
	OriginalSize int64
	Truncated    bool
	Filtered     bool
	MediaType    string
	ObjectKey    string
}

// Record is one run_steps row.
type Record struct {
	ID               int64
	RunID            string
	Seq              int64
	StepID           string
	Generation       int64
	ProjectID        int64
	ApplicationID    *int64
	ReleaseID        *int64
	DefinitionDigest []byte
	Kind             Kind
	Actor            string
	ActorUserID      *int64
	ConversationUUID string
	NodeID           string
	Name             string
	ToolkitID        string
	StartedAt        time.Time
	EndedAt          time.Time
	// Summary is a JSON object; Retrieval a JSON object or nil.
	Summary          []byte
	Retrieval        []byte
	CitedStepIDs     []string
	CitedDocumentIDs []string
	Inputs           ContentMeta
	Outputs          ContentMeta
	PrevHash         []byte
	Hash             []byte

	// Not chained.
	ContentExpiredAt     *time.Time
	ContentExpiredPolicy string
	RecordedAt           time.Time
	ExportTxID           int64
}

// Content returns one side's metadata.
func (r Record) Content(side Side) ContentMeta {
	if side == SideInputs {
		return r.Inputs
	}
	return r.Outputs
}

// ContentStatus is what an evidence reader sees of a content side.
type ContentStatus string

// The content statuses.
const (
	ContentStored  ContentStatus = "stored"
	ContentExpired ContentStatus = "expired"
	ContentEmpty   ContentStatus = "empty"
	// ContentNotStored: a digest without bytes that never expired (the
	// deployment had no object store when the step was persisted).
	ContentNotStored ContentStatus = "not_stored"
)

// Status is the side's content status.
func (r Record) Status(side Side) ContentStatus {
	meta := r.Content(side)
	switch {
	case meta.Digest == nil:
		return ContentEmpty
	case meta.ObjectKey != "" && r.ContentExpiredAt == nil:
		return ContentStored
	case r.ContentExpiredAt != nil:
		return ContentExpired
	default:
		return ContentNotStored
	}
}

// ZeroHash is the prev_hash of a run's first step.
var ZeroHash = make([]byte, sha256.Size)

// Canonical is the bytes the chain hashes for a record: every chained column
// in a fixed order, JSON values (summary, retrieval) re-encoded with sorted
// keys and exact numbers, so the jsonb round trip (which reorders keys and
// rewrites 1e2 as 100) does not change it. The content is covered by its
// digest and sizes, never by its bytes or object key.
func Canonical(r Record) ([]byte, error) {
	summary, err := decodeJSON(r.Summary, true)
	if err != nil {
		return nil, fmt.Errorf("summary: %w", err)
	}
	var retrieval any
	if r.Retrieval != nil {
		if retrieval, err = decodeJSON(r.Retrieval, true); err != nil {
			return nil, fmt.Errorf("retrieval: %w", err)
		}
	}
	tree := []field{
		{"v", "1"},
		{"run_id", r.RunID},
		{"seq", r.Seq},
		{"step_id", r.StepID},
		{"generation", r.Generation},
		{"project_id", r.ProjectID},
		{"application_id", r.ApplicationID},
		{"release_id", r.ReleaseID},
		{"definition_digest", hexOrNil(r.DefinitionDigest)},
		{"kind", string(r.Kind)},
		{"actor", r.Actor},
		{"actor_user_id", r.ActorUserID},
		{"conversation_uuid", r.ConversationUUID},
		{"node_id", r.NodeID},
		{"name", r.Name},
		{"toolkit_id", r.ToolkitID},
		{"started_at_us", r.StartedAt.UnixMicro()},
		{"ended_at_us", r.EndedAt.UnixMicro()},
		{"summary", summary},
		{"retrieval", retrieval},
		{"cited_step_ids", stringList(r.CitedStepIDs)},
		{"cited_document_ids", stringList(r.CitedDocumentIDs)},
		{"inputs", contentFields(r.Inputs)},
		{"outputs", contentFields(r.Outputs)},
	}
	var out bytes.Buffer
	writeFields(&out, tree)
	return out.Bytes(), nil
}

// ChainHash is the record's hash after prev.
func ChainHash(prev []byte, r Record) ([]byte, error) {
	canonical, err := Canonical(r)
	if err != nil {
		return nil, err
	}
	var p securityaudit.Hash
	if len(prev) != len(p) {
		return nil, errors.New("previous hash has the wrong length")
	}
	copy(p[:], prev)
	h := securityaudit.ChainHash(p, canonical)
	return h[:], nil
}

// Break is the first step that does not verify.
type Break struct {
	Seq    int64  `json:"seq"`
	Reason string `json:"reason"`
}

// VerifyChain checks records (in seq order) from seq 1 and that the head
// row (headSeq, and headHash when not nil) names the last record. It returns
// the first break, or nil.
func VerifyChain(records []Record, headSeq int64, headHash []byte) *Break {
	var v Verifier
	for _, r := range records {
		if b := v.Next(r); b != nil {
			return b
		}
	}
	return v.Finish(headSeq, headHash)
}

// Verifier checks a run's chain one record at a time, so a long run is
// verified page by page. The zero value expects seq 1.
type Verifier struct {
	count int64
	prev  []byte
}

// Next checks the next record in seq order and returns the break it shows,
// or nil. After a break the verifier's state is undefined.
func (v *Verifier) Next(r Record) *Break {
	prev := v.prev
	if prev == nil {
		prev = ZeroHash
	}
	want := v.count + 1
	if r.Seq != want {
		return &Break{Seq: want, Reason: fmt.Sprintf("step missing: next stored step is seq %d", r.Seq)}
	}
	if !bytes.Equal(r.PrevHash, prev) {
		return &Break{Seq: r.Seq, Reason: "prev_hash does not match the previous step's hash"}
	}
	computed, err := ChainHash(prev, r)
	if err != nil {
		return &Break{Seq: r.Seq, Reason: "the step does not decode: " + err.Error()}
	}
	if !bytes.Equal(computed, r.Hash) {
		return &Break{Seq: r.Seq, Reason: "hash does not match the step's content"}
	}
	v.count, v.prev = want, r.Hash
	return nil
}

// Count is how many records verified.
func (v *Verifier) Count() int64 { return v.count }

// Finish checks the head row against the last verified record.
func (v *Verifier) Finish(headSeq int64, headHash []byte) *Break {
	last := v.count
	prev := v.prev
	if prev == nil {
		prev = ZeroHash
	}
	if headSeq != last {
		return &Break{Seq: min(headSeq, last) + 1, Reason: fmt.Sprintf("the run head names %d steps, %d are stored", headSeq, last)}
	}
	if headHash != nil && !bytes.Equal(headHash, prev) {
		return &Break{Seq: last, Reason: "the run head's hash does not match the last step"}
	}
	return nil
}

type field struct {
	name  string
	value any
}

func contentFields(m ContentMeta) []field {
	return []field{
		{"digest", hexOrNil(m.Digest)},
		{"size", m.Size},
		{"original_size", m.OriginalSize},
		{"truncated", m.Truncated},
		{"filtered", m.Filtered},
		{"media_type", m.MediaType},
	}
}

func hexOrNil(b []byte) any {
	if b == nil {
		return nil
	}
	return hex.EncodeToString(b)
}

func stringList(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

func writeFields(out *bytes.Buffer, fields []field) {
	out.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			out.WriteByte(',')
		}
		writeString(out, f.name)
		out.WriteByte(':')
		writeValue(out, f.value)
	}
	out.WriteByte('}')
}

// writeValue writes the canonical form. Numbers are exact rationals
// (json.Number through big.Rat), strings are JSON-quoted, so a number and a
// string never encode alike.
func writeValue(out *bytes.Buffer, value any) {
	switch v := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		out.WriteString(strconv.FormatBool(v))
	case string:
		writeString(out, v)
	case int64:
		out.WriteString(strconv.FormatInt(v, 10))
	case *int64:
		if v == nil {
			out.WriteString("null")
		} else {
			out.WriteString(strconv.FormatInt(*v, 10))
		}
	case json.Number:
		out.WriteString(canonicalNumber(v))
	case []field:
		writeFields(out, v)
	case []any:
		out.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				out.WriteByte(',')
			}
			writeValue(out, item)
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fields := make([]field, len(keys))
		for i, k := range keys {
			fields[i] = field{k, v[k]}
		}
		writeFields(out, fields)
	default:
		// Unreachable: decodeJSON yields only the types above.
		writeString(out, fmt.Sprintf("%v", v))
	}
}

func writeString(out *bytes.Buffer, s string) {
	encoded, _ := json.Marshal(s)
	out.Write(encoded)
}

// maxExponent bounds a JSON number's exponent before big.Rat parses it:
// 1e999999999 would allocate a huge integer.
const maxExponent = 400

func canonicalNumber(n json.Number) string {
	r, ok := new(big.Rat).SetString(string(n))
	if !ok {
		return "NaN"
	}
	return r.RatString()
}

// ErrInvalidJSON refuses a summary or filter that is not a bounded JSON object.
var ErrInvalidJSON = errors.New("not a JSON object")

// decodeJSON decodes one JSON value with exact numbers. object requires an
// object at the top.
func decodeJSON(raw []byte, object bool) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, ErrInvalidJSON
	}
	if decoder.More() {
		return nil, ErrInvalidJSON
	}
	if _, isObject := value.(map[string]any); object && !isObject {
		return nil, ErrInvalidJSON
	}
	if err := checkNumbers(value); err != nil {
		return nil, err
	}
	return value, nil
}

func checkNumbers(value any) error {
	switch v := value.(type) {
	case json.Number:
		text := strings.ToLower(string(v))
		if i := strings.IndexByte(text, 'e'); i >= 0 {
			exponent, err := strconv.Atoi(strings.TrimPrefix(text[i+1:], "+"))
			if err != nil || exponent > maxExponent || exponent < -maxExponent {
				return fmt.Errorf("%w: a number's exponent is out of range", ErrInvalidJSON)
			}
		}
	case []any:
		for _, item := range v {
			if err := checkNumbers(item); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, item := range v {
			if err := checkNumbers(item); err != nil {
				return err
			}
		}
	}
	return nil
}

// cleanString makes a worker string storable in a text or jsonb column:
// valid UTF-8, no NUL (PostgreSQL refuses both).
func cleanString(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	return strings.ReplaceAll(s, "\x00", "�")
}

// cleanJSONTree applies cleanString to every key and string of a decoded
// value.
func cleanJSONTree(value any) any {
	switch v := value.(type) {
	case string:
		return cleanString(v)
	case []any:
		for i := range v {
			v[i] = cleanJSONTree(v[i])
		}
		return v
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[cleanString(k)] = cleanJSONTree(item)
		}
		return out
	}
	return value
}
