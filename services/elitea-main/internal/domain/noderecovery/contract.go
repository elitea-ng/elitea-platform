package noderecovery

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

const Schema = "elitea.pipeline.node-recovery-required.v1"

var ErrInvalid = errors.New("invalid node recovery contract")

type ReplaySafety struct {
	Kind      string `json:"kind"`
	EffectID  string `json:"effect_id,omitempty"`
	ReceiptID string `json:"receipt_id,omitempty"`
}

type Receipt struct {
	Schema          string       `json:"schema"`
	ActivationID    string       `json:"activation_id"`
	JournalRevision uint64       `json:"journal_revision"`
	NodeID          string       `json:"node_id"`
	GraphThread     string       `json:"graph_thread"`
	Step            uint64       `json:"step"`
	Attempt         uint16       `json:"attempt"`
	FailureClass    string       `json:"failure_class"`
	StopReason      string       `json:"stop_reason"`
	ReplaySafety    ReplaySafety `json:"replay_safety"`
	AllowedActions  []string     `json:"allowed_actions"`
}

type Request struct {
	RequestID        string `json:"request_id"`
	ExecutionID      string `json:"execution_id"`
	Generation       uint64 `json:"generation"`
	ActivationID     string `json:"activation_id"`
	ExpectedRevision uint64 `json:"expected_revision"`
	Action           string `json:"action"`
}

func ValidID(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	raw, err := hex.DecodeString(value)
	return err == nil && !bytes.Equal(raw, make([]byte, 32))
}

// ValidExecutionID matches Main currentRuntimeID: hex.EncodeToString of 16 bytes.
// It is an opaque execution selector, never UUID or authority.
func ValidExecutionID(value string) bool {
	if len(value) != 32 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// ValidResponseMessageID matches the existing chat response UUID selector.
func ValidResponseMessageID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	raw := strings.ReplaceAll(value, "-", "")
	if len(raw) != 32 || strings.ToLower(raw) != raw {
		return false
	}
	_, err := hex.DecodeString(raw)
	return err == nil && raw != strings.Repeat("0", 32)
}

func (r Request) Validate() error {
	if !ValidID(r.RequestID) || !ValidExecutionID(r.ExecutionID) || !ValidID(r.ActivationID) ||
		r.Generation == 0 || r.Generation > math.MaxInt64 || r.ExpectedRevision == 0 || r.ExpectedRevision > math.MaxInt64 ||
		(r.Action != "retry" && r.Action != "reconcile" && r.Action != "resume_result") {
		return ErrInvalid
	}
	return nil
}

func (r Receipt) Validate() error {
	if r.Schema != Schema || !ValidID(r.ActivationID) || r.JournalRevision == 0 || r.JournalRevision > math.MaxInt64 ||
		r.Step > math.MaxInt64 || r.Attempt == 0 || r.Attempt > 16 || !safeThread(r.GraphThread) || !safeNode(r.NodeID) || len(r.AllowedActions) != 1 {
		return ErrInvalid
	}
	transient := false
	switch r.FailureClass {
	case "dependency_unavailable", "rate_limited", "attempt_timeout", "worker_interrupted":
		transient = true
	case "invalid_configuration", "invalid_input", "invalid_result", "model_output_incomplete":
	default:
		return ErrInvalid
	}
	action := ""
	switch r.ReplaySafety.Kind {
	case "no_external_effect":
		if r.ReplaySafety.EffectID != "" || r.ReplaySafety.ReceiptID != "" {
			return ErrInvalid
		}
		action = "retry"
	case "idempotent_effect_not_committed":
		if !ValidID(r.ReplaySafety.EffectID) || r.ReplaySafety.ReceiptID != "" {
			return ErrInvalid
		}
		action = "retry"
	case "unknown_external_effect":
		if !ValidID(r.ReplaySafety.EffectID) || r.ReplaySafety.ReceiptID != "" {
			return ErrInvalid
		}
		action = "reconcile"
	case "completed_external_effect":
		if !ValidID(r.ReplaySafety.ReceiptID) || r.ReplaySafety.EffectID != "" {
			return ErrInvalid
		}
		action = "resume_result"
	case "unclassified":
		if r.ReplaySafety.EffectID != "" || r.ReplaySafety.ReceiptID != "" {
			return ErrInvalid
		}
		action = "reconcile"
	default:
		return ErrInvalid
	}
	if r.AllowedActions[0] != action {
		return ErrInvalid
	}
	if action == "retry" {
		if !transient || r.StopReason != "operator_approval_required" {
			return ErrInvalid
		}
	} else if r.StopReason != "effect_reconciliation_required" {
		return ErrInvalid
	}
	return nil
}

func DecodeReceipt(raw []byte) (Receipt, error) {
	var r Receipt
	if strictDecode(raw, &r, []string{"schema", "activation_id", "journal_revision", "node_id", "graph_thread", "step", "attempt", "failure_class", "stop_reason", "replay_safety", "allowed_actions"}) != nil || r.Validate() != nil {
		return Receipt{}, ErrInvalid
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	var replay map[string]json.RawMessage
	_ = json.Unmarshal(fields["replay_safety"], &replay)
	wanted := 1
	if r.ReplaySafety.EffectID != "" || r.ReplaySafety.ReceiptID != "" {
		wanted = 2
	}
	if len(replay) != wanted {
		return Receipt{}, ErrInvalid
	}
	return r, nil
}

func DecodeRequest(raw []byte) (Request, error) {
	var r Request
	if strictDecode(raw, &r, []string{"request_id", "execution_id", "generation", "activation_id", "expected_revision", "action"}) != nil || r.Validate() != nil {
		return Request{}, ErrInvalid
	}
	return r, nil
}

// DecodeStrictObject rejects duplicate, unknown, missing, and null fields.
func DecodeStrictObject(raw []byte, target any, required []string) error {
	return strictDecode(raw, target, required)
}

// CanonicalReceipt uses sorted object keys and preserves exact integers.
func CanonicalReceipt(raw []byte) ([]byte, error) {
	if _, err := DecodeReceipt(raw); err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	if d.Decode(&value) != nil {
		return nil, ErrInvalid
	}
	var out bytes.Buffer
	e := json.NewEncoder(&out)
	e.SetEscapeHTML(false)
	if e.Encode(value) != nil {
		return nil, ErrInvalid
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

func strictDecode(raw []byte, target any, required []string) error {
	if len(raw) == 0 || len(raw) > 8192 || !utf8.Valid(raw) {
		return ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if walk(d, 0) != nil {
		return ErrInvalid
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrInvalid
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != len(required) {
		return ErrInvalid
	}
	for _, key := range required {
		if value, ok := fields[key]; !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return ErrInvalid
		}
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(target) != nil {
		return ErrInvalid
	}
	return nil
}

func walk(d *json.Decoder, depth int) error {
	if depth > 8 {
		return ErrInvalid
	}
	t, err := d.Token()
	if err != nil || t == nil {
		return ErrInvalid
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			t, err := d.Token()
			key, ok := t.(string)
			if err != nil || !ok || seen[key] {
				return ErrInvalid
			}
			seen[key] = true
			if walk(d, depth+1) != nil {
				return ErrInvalid
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return ErrInvalid
		}
	case '[':
		for d.More() {
			if walk(d, depth+1) != nil {
				return ErrInvalid
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func safeThread(value string) bool {
	return len(value) > 0 && len(value) <= 512 && utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) < 0 && !strings.ContainsAny(value, "\u2028\u2029")
}

func safeNode(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_.:-", c)) {
			return false
		}
	}
	return true
}
