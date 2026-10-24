// Package pricesync implements the LLM model price-catalog sync worker
// (design §8.8). It runs as a cold, low-frequency batch job inside
// elitea-scheduler (which already has a pgxpool and a routine loop, so it is off
// the /llm hot path) and keeps gateway.gateway_models — the per-1M-token price
// catalog — refreshed from one or more ordered PriceSource adapters.
//
// THE LOAD-BEARING INVARIANT (§7.1/§8.8): gateway.gateway_models stores prices
// PER 1,000,000 BILLED UNITS. Upstream feeds (LiteLLM, getbifrost.ai/datasheet)
// publish prices PER SINGLE UNIT. The Normalizer below is the SINGLE conversion
// point that multiplies per-unit → per-1M by ×1,000,000. Feeding a per-unit
// value straight into the per-1M schema is a silent 1,000,000× undercharge;
// skipping or duplicating this multiply anywhere else re-opens the bug. Every
// source declares its own Denomination and the Normalizer applies exactly one
// convert.
//
// The billed unit is a TOKEN for a chat model, a SECOND of audio for
// speech-to-text, and a CHARACTER for text-to-speech. All three ride the same
// conversion, because the denomination is a COUNT and the count scale is the
// same for all of them. They are stored in separate columns, because a price
// per second must never be multiplied by a token count.
package pricesync

import (
	"context"
	"fmt"
)

// Denomination is the BILLED-UNIT COUNT a PriceSource publishes prices against.
// It is not specific to tokens. A model bills against one of three units — a
// token (chat), a second of audio (speech-to-text), or a character of text
// (text-to-speech) — and every upstream feed publishes ALL THREE at the same
// count per price. LiteLLM, for example, publishes input_cost_per_token,
// input_cost_per_second and input_cost_per_character all as the cost of ONE
// unit, so one PerToken declaration scales all three correctly.
//
// Read the names below as "per single unit", "per 1,000 units", "per 1,000,000
// units". A reader who reads them as tokens-only will think the audio columns
// need a second, separate conversion; they do not, and adding one would be a
// 1,000,000x overcharge on every audio row.
//
// The zero value is deliberately invalid so a source that forgets to declare one
// fails loudly rather than defaulting to a wrong (and silently mis-scaled) unit.
type Denomination int

const (
	// DenominationUnknown is the invalid zero value.
	DenominationUnknown Denomination = iota
	// PerToken — cost per SINGLE billed unit: one token, one second of audio, or
	// one character (LiteLLM, bifrost-datasheet).
	PerToken
	// Per1K — cost per 1,000 billed units.
	Per1K
	// Per1M — cost per 1,000,000 billed units (the canonical gateway_models schema).
	Per1M
)

// String renders the denomination for logs.
func (d Denomination) String() string {
	switch d {
	case PerToken:
		return "per-token"
	case Per1K:
		return "per-1k"
	case Per1M:
		return "per-1m"
	default:
		return "unknown"
	}
}

// factorTo1M returns the multiplier that converts a value in this denomination
// to the canonical per-1M denomination. The factor is a count ratio, so it is
// the same for token, second and character prices. It errors on an unknown
// denomination so a mis-configured source cannot silently produce zero- or
// wrongly-scaled prices.
func (d Denomination) factorTo1M() (float64, error) {
	switch d {
	case PerToken:
		return 1_000_000, nil
	case Per1K:
		return 1_000, nil
	case Per1M:
		return 1, nil
	default:
		return 0, fmt.Errorf("pricesync: unknown denomination %d", int(d))
	}
}

// RawModelPrice is a single model's pricing exactly as a source published it, in
// that source's own Denomination. Cost fields are pointers so an absent field
// (nil) is distinct from a genuine zero price.
type RawModelPrice struct {
	Provider          string
	ModelName         string
	InputCost         *float64
	OutputCost        *float64
	CacheCreationCost *float64
	CacheReadCost     *float64
	// InputCostAbove128k is the tiered above-128k-context input price when the
	// source publishes one (LiteLLM's *_above_128k_tokens field).
	InputCostAbove128k *float64
	// The audio prices below bill against a unit that is NOT a token, which is
	// why they are separate fields and not folded into InputCost/OutputCost. A
	// speech-to-text model bills the AUDIO DURATION it consumed, and a
	// text-to-speech model bills the CHARACTER COUNT it read. Mixing either into
	// the token fields would price seconds at the token rate.
	//
	// InputCostPerSecond is the audio-duration input price (speech-to-text).
	InputCostPerSecond *float64
	// OutputCostPerSecond is the audio-duration output price (a model that
	// returns audio).
	OutputCostPerSecond *float64
	// InputCostPerCharacter is the per-character input price.
	InputCostPerCharacter *float64
	// OutputCostPerCharacter is the per-character output price (text-to-speech).
	OutputCostPerCharacter *float64
}

// NormalizedModelPrice is a RawModelPrice converted to the canonical per-1M
// denomination and tagged with the source that produced it (provenance, §7.2).
// Every non-nil cost has been multiplied through the single Normalizer point.
type NormalizedModelPrice struct {
	Provider           string
	ModelName          string
	InputCostPer1M     *float64
	OutputCostPer1M    *float64
	CacheCreationPer1M *float64
	CacheReadPer1M     *float64
	InputCostAbove128k *float64
	// The four audio prices are per 1,000,000 SECONDS and per 1,000,000
	// CHARACTERS respectively — the same count scale as the token columns, a
	// different billed unit. The names carry the unit so a later reader cannot
	// divide a seconds price by a token count.
	InputCostPer1MSeconds     *float64
	OutputCostPer1MSeconds    *float64
	InputCostPer1MCharacters  *float64
	OutputCostPer1MCharacters *float64
	Source                    string
}

// key is the natural key mirroring the gateway_models UNIQUE(provider, model_name).
func (n NormalizedModelPrice) key() string { return n.Provider + "\x00" + n.ModelName }

// PriceSource is one upstream price feed. Sources are composed in precedence
// order by the Syncer; each declares the denomination its upstream uses so the
// Normalizer can convert it (design §8.8).
type PriceSource interface {
	// Name identifies the source for provenance and logs ("litellm", "seed", ...).
	Name() string
	// Fetch returns the source's raw prices in its own denomination.
	Fetch(ctx context.Context) ([]RawModelPrice, error)
	// Denomination declares the unit Fetch's costs are expressed in.
	Denomination() Denomination
}

// Normalizer is the single point that converts a source's RawModelPrice from its
// declared denomination to the canonical per-1M gateway_models schema.
type Normalizer struct{}

// mul returns v*factor when v is non-nil, else nil — preserving the
// absent-vs-zero distinction through the conversion.
func mul(v *float64, factor float64) *float64 {
	if v == nil {
		return nil
	}
	out := *v * factor
	return &out
}

// Normalize converts one raw price to per-1M using the source's denomination.
// It returns an error only when the denomination is invalid; a valid source
// with all-nil costs normalizes to an all-nil (but still keyed) row.
func (Normalizer) Normalize(raw RawModelPrice, denom Denomination, source string) (NormalizedModelPrice, error) {
	factor, err := denom.factorTo1M()
	if err != nil {
		return NormalizedModelPrice{}, err
	}
	return NormalizedModelPrice{
		Provider:           raw.Provider,
		ModelName:          raw.ModelName,
		InputCostPer1M:     mul(raw.InputCost, factor),
		OutputCostPer1M:    mul(raw.OutputCost, factor),
		CacheCreationPer1M: mul(raw.CacheCreationCost, factor),
		CacheReadPer1M:     mul(raw.CacheReadCost, factor),
		InputCostAbove128k: mul(raw.InputCostAbove128k, factor),
		// The audio prices convert through the SAME factor as the token prices.
		// This is correct because the factor is a count ratio and the source
		// publishes seconds, characters and tokens at the same count per price
		// (see Denomination). Giving these their own factor here would break the
		// single-conversion-point invariant this whole package exists to hold.
		InputCostPer1MSeconds:     mul(raw.InputCostPerSecond, factor),
		OutputCostPer1MSeconds:    mul(raw.OutputCostPerSecond, factor),
		InputCostPer1MCharacters:  mul(raw.InputCostPerCharacter, factor),
		OutputCostPer1MCharacters: mul(raw.OutputCostPerCharacter, factor),
		Source:                    source,
	}, nil
}
