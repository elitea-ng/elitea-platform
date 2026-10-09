package executioninterrupt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Decision is the public decision body (fanout-interrupt-decision-request.v1).
// CredentialRef is an opaque token-store reference, never a token.
type Decision struct {
	RequestID        string  `json:"request_id"`
	ExpectedRevision int64   `json:"expected_revision"`
	Action           Action  `json:"action"`
	Value            string  `json:"value"`
	CredentialRef    *string `json:"credential_ref,omitempty"`
}

// ParseDecisionRequest validates a decision body of at most
// MaxDecisionBodyBytes against the contract schema and returns it with its
// canonical bytes. The canonical bytes are what the ledger stores and what a
// replay must equal.
func ParseDecisionRequest(raw []byte) (Decision, []byte, error) {
	schemas, err := loadSchemas()
	if err != nil {
		return Decision{}, nil, err
	}
	value, err := decodeStrict(raw, MaxDecisionBodyBytes)
	if err != nil {
		return Decision{}, nil, ErrInvalidDecision
	}
	if schemas.decision.Validate(value) != nil {
		return Decision{}, nil, ErrInvalidDecision
	}
	canonical, err := canonicalOf(value)
	if err != nil || len(canonical) > MaxDecisionBodyBytes {
		return Decision{}, nil, ErrInvalidDecision
	}
	var decision Decision
	if err := json.Unmarshal(canonical, &decision); err != nil {
		return Decision{}, nil, ErrInvalidDecision
	}
	return decision, canonical, nil
}

// DecisionSHA256 is the contract §7 digest that binds a fetched decision to
// its ACK: SHA-256 over the canonical {action, credential_ref, interrupt_key,
// request_id, revision, value}, credential_ref null when absent.
func DecisionSHA256(interruptKey, requestID string, revision int64, action Action, value string, credentialRef *string) (string, error) {
	var ref any
	if credentialRef != nil {
		ref = *credentialRef
	}
	encoded, err := canonicalOf(map[string]any{
		"action":         string(action),
		"credential_ref": ref,
		"interrupt_key":  interruptKey,
		"request_id":     requestID,
		"revision":       revision,
		"value":          value,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// FetchedDecision is one entry of fanout-interrupt-fetch.v1. It never carries
// decided_by or the private frontier.
type FetchedDecision struct {
	InterruptKey   string  `json:"interrupt_key"`
	InterruptID    string  `json:"interrupt_id"`
	Revision       int64   `json:"revision"`
	RequestID      string  `json:"request_id"`
	Action         Action  `json:"action"`
	Value          string  `json:"value"`
	CredentialRef  *string `json:"credential_ref"`
	DecisionSHA256 string  `json:"decision_sha256"`
}

func (d FetchedDecision) canonicalValue() map[string]any {
	var ref any
	if d.CredentialRef != nil {
		ref = *d.CredentialRef
	}
	return map[string]any{
		"action":          string(d.Action),
		"credential_ref":  ref,
		"decision_sha256": d.DecisionSHA256,
		"interrupt_id":    d.InterruptID,
		"interrupt_key":   d.InterruptKey,
		"request_id":      d.RequestID,
		"revision":        d.Revision,
		"value":           d.Value,
	}
}

// CanonicalSize is the entry's canonical byte length (bounded by
// MaxFetchEntryBytes).
func (d FetchedDecision) CanonicalSize() (int, error) {
	encoded, err := canonicalOf(d.canonicalValue())
	return len(encoded), err
}

// FetchSchema names fanout-interrupt-fetch.v1.
const FetchSchema = "elitea.pipeline.fanout-interrupt-fetch.v1"

// Fetch is the private claim-fenced fetch response.
type Fetch struct {
	Schema           string            `json:"schema"`
	ExecutionID      string            `json:"execution_id"`
	Generation       int64             `json:"generation"`
	DecisionRevision int64             `json:"decision_revision"`
	Decisions        []FetchedDecision `json:"decisions"`
}

// CanonicalJSON writes the fetch response in the contract canonical form.
func (f Fetch) CanonicalJSON() ([]byte, error) {
	decisions := make([]any, 0, len(f.Decisions))
	for _, decision := range f.Decisions {
		decisions = append(decisions, decision.canonicalValue())
	}
	return canonicalOf(map[string]any{
		"decision_revision": f.DecisionRevision,
		"decisions":         decisions,
		"execution_id":      f.ExecutionID,
		"generation":        f.Generation,
		"schema":            f.Schema,
	})
}
