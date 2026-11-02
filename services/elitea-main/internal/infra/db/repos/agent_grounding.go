package repos

import (
	"encoding/json"
)

// The worker reports a grounding indicator on the final message of an agent
// that must cite (ADR-0043 decision 5): how many of the run's sources the
// answer cites, and which claim spans have no citation. Main keeps it on the
// message group's meta (`meta.grounding`) so a reloaded conversation shows
// the same indicator as the live stream.
//
// The indicator is display data, not authority. It is rebuilt into a closed
// shape with bounded numbers. A malformed indicator is dropped; it never
// fails the answer it rides on. It has no score field, and Main never adds
// one.

const (
	groundingVersion = 1
	// The worker's limit (agent-runtime grounding.rs MAX_UNCITED_SPANS).
	maxGroundingUncitedSpans = 64
	// A bound far above any real answer: a 4 MiB answer has fewer UTF-16
	// code units than this, and fewer sources and spans.
	maxGroundingCount = 1 << 24
)

type groundingSpan struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type groundingIndicator struct {
	Version          int             `json:"version"`
	SourcesAvailable int             `json:"sources_available"`
	SourcesCited     int             `json:"sources_cited"`
	ClaimSpans       int             `json:"claim_spans"`
	UncitedSpans     int             `json:"uncited_spans"`
	Uncited          []groundingSpan `json:"uncited"`
	InvalidMarkers   int             `json:"invalid_markers"`
}

func groundingCount(value int) bool {
	return value >= 0 && value <= maxGroundingCount
}

func (indicator groundingIndicator) valid() bool {
	if indicator.Version != groundingVersion || indicator.SourcesAvailable < 1 ||
		!groundingCount(indicator.SourcesAvailable) || !groundingCount(indicator.SourcesCited) ||
		!groundingCount(indicator.ClaimSpans) || !groundingCount(indicator.UncitedSpans) ||
		!groundingCount(indicator.InvalidMarkers) ||
		indicator.SourcesCited > indicator.SourcesAvailable || indicator.UncitedSpans > indicator.ClaimSpans ||
		len(indicator.Uncited) > maxGroundingUncitedSpans || len(indicator.Uncited) > indicator.UncitedSpans {
		return false
	}
	previousEnd := 0
	for _, span := range indicator.Uncited {
		if span.Start < previousEnd || span.End <= span.Start || span.End > maxGroundingCount {
			return false
		}
		previousEnd = span.End
	}
	return true
}

// groundingFromMetadata reads and sanitizes the indicator from a node
// event's response metadata; nil when absent or invalid.
func groundingFromMetadata(responseMetadata json.RawMessage) json.RawMessage {
	var metadata struct {
		Grounding json.RawMessage `json:"grounding"`
	}
	if json.Unmarshal(responseMetadata, &metadata) != nil {
		return nil
	}
	return sanitizeGrounding(metadata.Grounding)
}

// sanitizeGrounding returns the canonical JSON object to persist, or nil.
func sanitizeGrounding(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var indicator groundingIndicator
	if json.Unmarshal(raw, &indicator) != nil || !indicator.valid() {
		return nil
	}
	if indicator.Uncited == nil {
		indicator.Uncited = []groundingSpan{}
	}
	encoded, err := json.Marshal(indicator)
	if err != nil {
		return nil
	}
	return encoded
}
