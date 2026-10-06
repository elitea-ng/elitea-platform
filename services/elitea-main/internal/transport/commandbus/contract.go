// Package commandbus is the runtime command bus: elitea-main's side of the
// durable, at-least-once channel that carries signed worker commands to the
// Rust and Python workers.
//
// The transport is NATS JetStream (ADR-0010 amendment "Transport binding:
// NATS JetStream"; docs/runtime-command-bus.md is the normative contract this
// file encodes). PostgreSQL stays the authority: the outbox owns dispatch
// intent and the claim owns execution, so the bus only has to provide
// at-least-once delivery to a shared consumer, a bounded live set with a
// synchronous "full" signal, de-duplication of PostgreSQL re-offers,
// redelivery of work whose consumer died, a heartbeat that keeps long work
// owned, and a readable "drained" state.
//
// This file is the contract every party shares: the stream, subject and
// consumer names, the header names, and the stream and consumer settings the
// bootstrap Job (deploy/helm/nats-bootstrap/files/bootstrap.sh) creates. The
// Rust worker (src/transport/nats_jetstream.rs) and the Python worker
// (elitea_worker/transport/nats_jetstream.py) restate the names; a test on
// each side pins them to docs/runtime-command-bus.md.
package commandbus

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SubjectRoot is the subject namespace of the command bus.
const SubjectRoot = "elitea.rt.v1"

// StreamPrefix prefixes every command stream name. The rest of the name is
// the route token, upper-case; the route's subjects use it lower-case.
const StreamPrefix = "ELITEA_RT_V1_"

// The three routes the permission table (deploy/helm/nats/values.yaml) and
// the bootstrap know. A deployment may point two dispatch routes at one
// stream (and one consumer); it cannot invent a fourth stream without a
// permission row and a bootstrap block for it.
const (
	StreamValidate = "ELITEA_RT_V1_VALIDATE"
	StreamAgent    = "ELITEA_RT_V1_AGENT"
	StreamIndex    = "ELITEA_RT_V1_INDEX"
)

// The durable pull consumers the bootstrap creates, one per stream. Every
// worker replica of a route binds to the same durable; a worker never creates
// or edits one.
const (
	ConsumerValidate = "elitea-configuration-worker-v1"
	ConsumerAgent    = "elitea-agent-worker-v1"
	ConsumerIndex    = "elitea-index-worker-v1"
)

// KnownStreams maps each stream to its durable consumer.
var KnownStreams = map[string]string{
	StreamValidate: ConsumerValidate,
	StreamAgent:    ConsumerAgent,
	StreamIndex:    ConsumerIndex,
}

// Headers. Neither is authority: only the signed envelope in the body is
// trusted, and the worker compares the subject's hash token with the signed
// command's idempotency key before it claims.
const (
	// HeaderMsgID is JetStream's de-duplication header. Its value is the
	// subject's hash token, so it is bounded and header-safe whatever bytes
	// the delivery ID holds.
	HeaderMsgID = "Nats-Msg-Id"
	// HeaderDeliveryID carries the raw delivery ID (the outbox ID, which is
	// also the command's idempotency key) for diagnostics.
	HeaderDeliveryID = "Elitea-Delivery-Id"
)

// DeadLetterBucket is the KV bucket a worker records a poison command in. Key
// "<route>.<hash token>", bucket TTL DeadLetterTTL.
const DeadLetterBucket = "ELITEA_RT_V1_DEADLETTER"

// ReplayWakeSubject is the core NATS subject of the advisory execution-replay
// wake-up (no JetStream; PostgreSQL polling is the fallback).
const ReplayWakeSubject = SubjectRoot + ".replay.wake"

// Consumer and redelivery timing.
const (
	// AckWait is the consumer's redelivery timer: twice the 30s claim lease,
	// the floor the Redis reclaim idle time had.
	AckWait = 60 * time.Second
	// InProgressInterval is how often a worker sends +WPI for every message
	// it owns. Well under AckWait, so two lost heartbeats do not redeliver.
	InProgressInterval = 5 * time.Second
	// RetryDelay is the NakWithDelay for a command the claim told the worker
	// to retry later; it matches the old reclaim idle time.
	RetryDelay = 60 * time.Second
	// PoisonDelay is the NakWithDelay for a command the worker cannot
	// verify or decode. The message is never terminated: a Term would free
	// the subject and PostgreSQL would re-offer the poison every 30s.
	PoisonDelay = 24 * time.Hour
	// DeadLetterTTL is the dead-letter bucket's TTL.
	DeadLetterTTL = 7 * 24 * time.Hour
	// DuplicateWindow is the stream's Nats-Msg-Id window. It only has to
	// cover an ambiguous publish retry; the per-subject limit is what makes
	// a PostgreSQL re-offer idempotent.
	DuplicateWindow = 2 * time.Minute
	// MaxAgeMargin is added to a route's longest execution deadline to give
	// the stream's MaxAge, a safety net that removes only commands whose
	// PostgreSQL deadline has already retired them.
	MaxAgeMargin = 2 * time.Hour
)

// Stream capacity bounds. MaxMsgs comes from the bootstrap's values and may
// be lower than MaxStreamMessages; the byte budget is fixed.
const (
	MaxStreamMessages = 1024
	MaxStreamBytes    = int64(64 << 20)
	// MaxMessageBytes is the stream's MaxMsgSize: payload plus headers. It is
	// the conformance limits' max_transport_message_bytes.
	MaxMessageBytes = 64 * 1024
)

// Consumer pull bounds.
const (
	MaxAckPending     = MaxStreamMessages
	MaxWaiting        = 512
	MaxRequestBatch   = 64
	MaxRequestExpires = 30 * time.Second
)

// ErrInvalidStream is returned for a stream name outside the contract.
var ErrInvalidStream = errors.New("runtime command stream is not an ELITEA_RT_V1_<ROUTE> name")

// RouteToken returns the subject token of a stream: ELITEA_RT_V1_AGENT ->
// "agent". The token is 1-32 characters of [A-Z0-9], starting with a letter.
func RouteToken(stream string) (string, error) {
	route, ok := strings.CutPrefix(stream, StreamPrefix)
	if !ok || route == "" || len(route) > 32 {
		return "", ErrInvalidStream
	}
	for i := 0; i < len(route); i++ {
		c := route[i]
		switch {
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return "", ErrInvalidStream
		}
	}
	return strings.ToLower(route), nil
}

// FilterSubject is the stream's subject filter: elitea.rt.v1.<route>.d.*.
func FilterSubject(stream string) (string, error) {
	route, err := RouteToken(stream)
	if err != nil {
		return "", err
	}
	return SubjectRoot + "." + route + ".d.*", nil
}

// DeliveryToken is the hash token that names one delivery:
// hex(sha256(deliveryID)). A hash, because a delivery ID may hold any byte
// but CR, LF and NUL, including '.', '*', '>' and spaces.
func DeliveryToken(deliveryID string) string {
	sum := sha256.Sum256([]byte(deliveryID))
	return hex.EncodeToString(sum[:])
}

// DeliverySubject is the one subject a delivery is published on. With the
// stream's MaxMsgsPerSubject=1 and DiscardNewPerSubject, a second live copy
// of the same delivery is refused by the server atomically.
func DeliverySubject(stream, deliveryID string) (string, error) {
	route, err := RouteToken(stream)
	if err != nil {
		return "", err
	}
	return SubjectRoot + "." + route + ".d." + DeliveryToken(deliveryID), nil
}

// DeadLetterKey is a delivery's key in DeadLetterBucket.
func DeadLetterKey(stream, deliveryID string) (string, error) {
	route, err := RouteToken(stream)
	if err != nil {
		return "", err
	}
	return route + "." + DeliveryToken(deliveryID), nil
}

// ValidateRoute checks a (stream, consumer) pair names a contract stream and
// its bootstrap-created durable.
func ValidateRoute(stream, consumer string) error {
	want, ok := KnownStreams[stream]
	if !ok {
		return fmt.Errorf("%w: %q is not one of %s, %s, %s", ErrInvalidStream, stream, StreamValidate, StreamAgent, StreamIndex)
	}
	if consumer != "" && consumer != want {
		return fmt.Errorf("runtime command stream %s is consumed by the durable %s, not %q", stream, want, consumer)
	}
	return nil
}
