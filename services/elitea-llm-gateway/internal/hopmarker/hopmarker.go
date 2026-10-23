// Package hopmarker implements the gateway's hop marker — the real
// anti-circular-routing mechanism (issue #164, follow-up to #12).
//
// # What it is
//
// The gateway sets one header on EVERY outbound provider request and
// recognises that same header on every inbound one. An inbound request that
// carries this deployment's own marker has already passed through this
// gateway, so it is a re-entry: the gateway refuses it instead of dispatching
// it a second time. The loop is therefore contained on the FIRST re-entry, at
// depth 1, whatever the traffic rate.
//
// # Why hop detection, and not the rate breaker
//
// internal/llmproxy/loopbreaker.go counts requests per (project_id, model).
// Issue #12 established that no rate threshold can separate a routing loop
// from legitimate traffic there, because both are bounded by the same
// per-replica provider worker pool: a threshold low enough to catch the
// canonical loop is low enough to trip ordinary bursts, and one high enough
// not to trip bursts can never fire on the loop. That layer is an
// amplification backstop. This is the loop detector.
//
// # Design constraints, and how each is met
//
//   - REPLICA-IDENTICAL. A loop can leave through replica A and re-enter on
//     replica B, so every replica must produce and recognise the same marker.
//     The marker is a pure function of operator-supplied key material, with no
//     per-process, per-request or clock input.
//
//   - DEDICATED KEY MATERIAL, never GATEWAY_IDENTITY_SECRET. The marker is
//     transmitted to every upstream, and a provider's api_base is
//     tenant-authored, so the marker is published to addresses a tenant
//     chooses. Deriving it from the key that signs the X-Elitea-* identity
//     headers would publish a MAC under that key to those addresses and would
//     tie marker rotation to identity rotation. Key material comes from
//     GATEWAY_HOP_SECRET alone.
//
//   - SECURITY DOES NOT REST ON SECRECY. Any upstream can harvest the marker
//     off a request the gateway sent it. The property that has to hold is
//     narrower, and it holds by construction: recognising a marker refuses
//     ONLY the request that carries it. Detection reads one header of one
//     request, compares it in constant time, and records NOTHING — no counter,
//     no per-tuple circuit, no shared state of any kind. A forged or harvested
//     marker therefore denies the forger's own request and cannot reach any
//     other request, project or tenant. This is the reason detection must stay
//     stateless: the moment a marker opened a circuit or incremented a counter,
//     a harvested marker would become a cross-tenant denial-of-service.
//
// # Value shape
//
// "v1=<hex>", where <hex> is HMAC-SHA256 of a FIXED domain-separation string
// under the hop secret. The message is fixed on purpose: the marker answers
// exactly one question ("did this deployment send this request?"), so binding
// it to a project or a request would add a dimension the answer does not need
// and would give an upstream a per-tenant value to correlate.
//
// The "v1=" prefix carries the scheme version so it can be rotated without
// ambiguity, in the idiom of the identity signature's "sha256=" prefix.
package hopmarker

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// Header is the hop-marker header name.
//
// It is in the X-Elitea-* namespace, which the browser edges otherwise DELETE
// in full (elitea-main's own strip, and the Gateway API HTTPRoute). This one name carries
// a narrow, documented exception, because the marker must survive the canonical
// loop path:
//
//	gateway → provider (= the platform's own /llm) → edge → elitea-main → gateway
//
// An edge that stripped it would disarm the guard silently. The exception is
// gated from both sides: services/elitea-main/tests/deployedge/edge_identity_strip_test.go
// fails if any browser edge deletes this name, and
// internal/hopmarker/path_pin_test.go fails if this name drifts away from
// the one that gate names.
//
// The exception is safe for the reason the package comment gives: this header
// is not an identity. It grants nothing, it names no project, and the only
// thing a client can achieve by sending it is the refusal of its own request.
const Header = "X-Elitea-Llm-Hop"

// version prefixes the marker value so the scheme can be rotated.
const version = "v1"

// domain is the fixed message the marker MACs. It is domain-separated so the
// value can never collide with any other MAC this deployment computes.
const domain = "elitea-llm-gateway/hop-marker/v1"

// Marker is one deployment's hop marker. A nil *Marker is the UNARMED marker:
// every method is safe on it, Value returns "" and Matches always reports
// false, so an unarmed gateway sets no header and refuses nothing.
type Marker struct {
	value string
}

// New builds the marker for secret. An empty secret returns nil — the unarmed
// marker. Callers must never invent key material of their own: a per-process
// random secret would break the replica-identical rule and would detect only
// the loops that happen to return to the replica that started them, while
// reading to an operator exactly like a working guard.
func New(secret []byte) *Marker {
	if len(secret) == 0 {
		return nil
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(domain))
	return &Marker{value: version + "=" + hex.EncodeToString(mac.Sum(nil))}
}

// Armed reports whether the marker carries key material.
func (m *Marker) Armed() bool { return m != nil && m.value != "" }

// Value is what the gateway sets on an outbound provider request, or "" when
// unarmed.
func (m *Marker) Value() string {
	if !m.Armed() {
		return ""
	}
	return m.value
}

// Matches reports whether got is THIS deployment's marker, comparing in
// constant time. An unarmed marker and an absent header both report false.
//
// A true answer means one thing only: the request now arriving was sent by
// this deployment's gateway. It says nothing about who sent it back, so the
// caller must refuse only this request and change no shared state.
func (m *Marker) Matches(got string) bool {
	if !m.Armed() || got == "" {
		return false
	}
	return hmac.Equal([]byte(got), []byte(m.value))
}
