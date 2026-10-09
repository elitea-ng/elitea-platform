package executioninterrupt

import "encoding/json"

type AckOutcome string

const (
	AckApplied AckOutcome = "applied"
	AckStale   AckOutcome = "stale"
)

// Ack is the private ACK body (fanout-interrupt-ack-request.v1). Revision and
// DecisionSHA256 must equal the fetched values.
type Ack struct {
	InterruptKey      string     `json:"interrupt_key"`
	RequestID         string     `json:"request_id"`
	Revision          int64      `json:"revision"`
	DecisionSHA256    string     `json:"decision_sha256"`
	Outcome           AckOutcome `json:"outcome"`
	ChildCheckpointID *string    `json:"child_checkpoint_id"`
}

// ParseAck validates an ACK body against the contract schema and returns it
// with the canonical bytes the ledger stores for replay comparison.
func ParseAck(raw []byte) (Ack, []byte, error) {
	schemas, err := loadSchemas()
	if err != nil {
		return Ack{}, nil, err
	}
	value, err := decodeStrict(raw, MaxAckBodyBytes)
	if err != nil {
		return Ack{}, nil, ErrInvalidAck
	}
	if schemas.ack.Validate(value) != nil {
		return Ack{}, nil, ErrInvalidAck
	}
	canonical, err := canonicalOf(value)
	if err != nil {
		return Ack{}, nil, ErrInvalidAck
	}
	var ack Ack
	if err := json.Unmarshal(canonical, &ack); err != nil {
		return Ack{}, nil, ErrInvalidAck
	}
	return ack, canonical, nil
}

// Frontier is the private position of a card: the frozen child thread, the
// fan-out node and the 1-based member ordinal. A coordinator card has the root
// thread, no node and ordinal 0. It is stored by Main and never returned to a
// client.
type Frontier struct {
	ChildThread string `json:"child_thread"`
	FanoutNode  string `json:"fanout_node"`
	Ordinal     int64  `json:"ordinal"`
}

// CanonicalJSON validates the frontier and writes it canonically.
func (f Frontier) CanonicalJSON() ([]byte, error) {
	member := f.FanoutNode != "" || f.Ordinal != 0
	if !safeText(f.ChildThread, MaxChildThreadBytes) || f.Ordinal < 0 || f.Ordinal > MaxFanoutOrdinal ||
		(member && (f.Ordinal == 0 || !safeToken(f.FanoutNode, MaxFanoutNodeBytes))) {
		return nil, ErrInvalidFrontier
	}
	encoded, err := canonicalOf(map[string]any{
		"child_thread": f.ChildThread,
		"fanout_node":  f.FanoutNode,
		"ordinal":      f.Ordinal,
	})
	if err != nil || len(encoded) > MaxFrontierBytes {
		return nil, ErrInvalidFrontier
	}
	return encoded, nil
}
