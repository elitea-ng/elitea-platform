package executioninterrupt

import (
	"encoding/json"
	"slices"
	"strings"
)

// maxRawCardBytes bounds parse work on a non-canonical raise before the
// canonical card is measured against MaxCardBytes. A producer may escape every
// character as \uXXXX (6 raw bytes for one canonical ASCII byte), so a raw card
// up to 6x the canonical bound can still be a valid card.
const maxRawCardBytes = 6 * MaxCardBytes

// Card holds the fields of a raised card that the ledger indexes. The full
// card is kept only as its canonical bytes; display text is never decoded into
// a Go value that could reach a log.
type Card struct {
	InterruptKey     string
	InterruptID      string
	Kind             Kind
	AvailableActions []Action
	PayloadSHA256    string
	// Member identity of a fan-out card; empty for a coordinator card.
	FanoutNode   string
	Ordinal      int64
	MemberCallID string
}

// MemberCallIDPrefix marks the fan-out member tier's call id (contract §3.3).
const MemberCallIDPrefix = "fo1_"

type cardWire struct {
	InterruptKey     string     `json:"interrupt_key"`
	InterruptID      string     `json:"interrupt_id"`
	Kind             Kind       `json:"kind"`
	AvailableActions []Action   `json:"available_actions"`
	PayloadSHA256    string     `json:"payload_sha256"`
	ParentAgentPath  []tierWire `json:"parent_agent_path"`
	FanoutV1         *struct {
		Node    string `json:"node"`
		Ordinal int    `json:"ordinal"`
	} `json:"fanout_v1"`
}

type tierWire struct {
	CallID         string `json:"call_id"`
	SiblingOrdinal *int   `json:"sibling_ordinal"`
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
	card := Card{
		InterruptKey:     wire.InterruptKey,
		InterruptID:      wire.InterruptID,
		Kind:             wire.Kind,
		AvailableActions: wire.AvailableActions,
		PayloadSHA256:    wire.PayloadSHA256,
	}
	if wire.FanoutV1 != nil {
		// v1.1 §4.1: exactly one member tier, carrying the member ordinal.
		members := 0
		for _, tier := range wire.ParentAgentPath {
			if !strings.HasPrefix(tier.CallID, MemberCallIDPrefix) {
				continue
			}
			members++
			if tier.SiblingOrdinal == nil || *tier.SiblingOrdinal != wire.FanoutV1.Ordinal || !validMemberCallID(tier.CallID) {
				return Card{}, nil, ErrInvalidCard
			}
			card.MemberCallID = tier.CallID
		}
		if members != 1 {
			return Card{}, nil, ErrInvalidCard
		}
		card.FanoutNode, card.Ordinal = wire.FanoutV1.Node, int64(wire.FanoutV1.Ordinal)
	}
	return card, canonical, nil
}

func validMemberCallID(value string) bool {
	digest, ok := strings.CutPrefix(value, MemberCallIDPrefix)
	return ok && ValidDigest(digest)
}

// MemberFrontier is the private frontier Main stores for a fan-out card
// (contract v1.1 §4.1): the member tier call id (a digest of the frozen child
// thread, which never leaves the Worker), the fan-out node and the ordinal. A
// coordinator card has none: until root pauses move to the ledger it is not
// raised through agent_interrupt_pending.
func (c Card) MemberFrontier() (Frontier, error) {
	if c.MemberCallID == "" || c.FanoutNode == "" || c.Ordinal < 1 {
		return Frontier{}, ErrInvalidCard
	}
	return Frontier{ChildThread: c.MemberCallID, FanoutNode: c.FanoutNode, Ordinal: c.Ordinal}, nil
}

// Offers reports whether the card lists the action.
func (c Card) Offers(action Action) bool { return slices.Contains(c.AvailableActions, action) }
