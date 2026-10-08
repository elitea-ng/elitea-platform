package executioninterrupt

import (
	"encoding/json"
	"slices"
)

// maxRawCardBytes bounds parse work on a non-canonical raise before the
// canonical card is measured against MaxCardBytes.
const maxRawCardBytes = 2 * MaxCardBytes

// Card holds the fields of a raised card that the ledger indexes. The full
// card is kept only as its canonical bytes; display text is never decoded into
// a Go value that could reach a log.
type Card struct {
	InterruptKey     string
	InterruptID      string
	Kind             Kind
	AvailableActions []Action
	PayloadSHA256    string
}

type cardWire struct {
	InterruptKey     string     `json:"interrupt_key"`
	InterruptID      string     `json:"interrupt_id"`
	Kind             Kind       `json:"kind"`
	AvailableActions []Action   `json:"available_actions"`
	PayloadSHA256    string     `json:"payload_sha256"`
	ParentAgentPath  []tierWire `json:"parent_agent_path"`
	FanoutV1         *struct {
		Ordinal int `json:"ordinal"`
	} `json:"fanout_v1"`
}

type tierWire struct {
	SiblingOrdinal *int `json:"sibling_ordinal"`
}

// ParseCard validates a raised card against the contract schema
// (fanout-interrupt-card.v1) and the rules the schema cannot express, and
// returns it with its canonical bytes (at most MaxCardBytes).
func ParseCard(raw []byte) (Card, []byte, error) {
	schemas, err := loadSchemas()
	if err != nil {
		return Card{}, nil, err
	}
	value, err := decodeStrict(raw, maxRawCardBytes)
	if err != nil {
		return Card{}, nil, ErrInvalidCard
	}
	if schemas.card.Validate(value) != nil {
		return Card{}, nil, ErrInvalidCard
	}
	canonical, err := canonicalOf(value)
	if err != nil || len(canonical) > MaxCardBytes {
		return Card{}, nil, ErrInvalidCard
	}
	var wire cardWire
	if err := json.Unmarshal(canonical, &wire); err != nil {
		return Card{}, nil, ErrInvalidCard
	}
	// Contract §3.3: the member tier's sibling_ordinal equals fanout_v1.ordinal.
	if wire.FanoutV1 != nil && !slices.ContainsFunc(wire.ParentAgentPath, func(tier tierWire) bool {
		return tier.SiblingOrdinal != nil && *tier.SiblingOrdinal == wire.FanoutV1.Ordinal
	}) {
		return Card{}, nil, ErrInvalidCard
	}
	return Card{
		InterruptKey:     wire.InterruptKey,
		InterruptID:      wire.InterruptID,
		Kind:             wire.Kind,
		AvailableActions: wire.AvailableActions,
		PayloadSHA256:    wire.PayloadSHA256,
	}, canonical, nil
}

// Offers reports whether the card lists the action.
func (c Card) Offers(action Action) bool { return slices.Contains(c.AvailableActions, action) }
