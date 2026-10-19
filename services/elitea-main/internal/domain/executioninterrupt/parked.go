package executioninterrupt

import (
	"encoding/json"
	"errors"
)

// ParkedSchema names response_metadata.fanout_parked_v1 (contract §8).
const ParkedSchema = "elitea.pipeline.fanout-parked.v1"

// maxRawParkedBytes bounds the parked payload before parsing: 16 entries of
// a 64-byte key and a 512-byte id stay far below it.
const maxRawParkedBytes = 16 * 1024

// ErrInvalidParked is a parked terminal payload that does not satisfy
// fanout-parked.schema.json. The frame carrying it is refused, never stored
// as a silent park.
var ErrInvalidParked = errors.New("execution interrupt parked payload is invalid")

// ParkedCard is one open card the parked execution still waits on.
type ParkedCard struct {
	InterruptKey string
	InterruptID  string
}

// Parked is the strictly decoded fanout_parked_v1 payload. Canonical is the
// contract canonical form of the payload, the only bytes Main stores.
type Parked struct {
	DecisionRevisionSeen int64
	Pending              []ParkedCard
	Canonical            []byte
}

// ParseParked validates raw against fanout-parked.schema.json (closed
// objects, 1-16 unique entries, bounded strings) and returns it decoded.
func ParseParked(raw []byte) (Parked, error) {
	schemas, err := loadSchemas()
	if err != nil {
		return Parked{}, err
	}
	value, err := decodeStrict(raw, maxRawParkedBytes)
	if err != nil || schemas.parked.Validate(value) != nil {
		return Parked{}, ErrInvalidParked
	}
	canonical, err := canonicalOf(value)
	if err != nil {
		return Parked{}, ErrInvalidParked
	}
	var wire struct {
		DecisionRevisionSeen int64 `json:"decision_revision_seen"`
		Pending              []struct {
			InterruptKey string `json:"interrupt_key"`
			InterruptID  string `json:"interrupt_id"`
		} `json:"pending"`
	}
	if json.Unmarshal(canonical, &wire) != nil {
		return Parked{}, ErrInvalidParked
	}
	parked := Parked{DecisionRevisionSeen: wire.DecisionRevisionSeen, Canonical: canonical,
		Pending: make([]ParkedCard, 0, len(wire.Pending))}
	seen := make(map[string]struct{}, len(wire.Pending))
	for _, entry := range wire.Pending {
		// uniqueItems compares whole entries; one key is one card.
		if _, duplicate := seen[entry.InterruptKey]; duplicate {
			return Parked{}, ErrInvalidParked
		}
		seen[entry.InterruptKey] = struct{}{}
		parked.Pending = append(parked.Pending, ParkedCard{InterruptKey: entry.InterruptKey, InterruptID: entry.InterruptID})
	}
	return parked, nil
}
