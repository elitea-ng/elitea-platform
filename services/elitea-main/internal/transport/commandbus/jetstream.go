package commandbus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
)

// jsErrStoreFailed is the JetStream error code (JSStreamStoreFailedF) the
// server answers a publish with when a stream limit refuses it. The server
// uses it both for "maximum messages per subject exceeded" (the delivery is
// already live) and for "maximum messages/bytes exceeded" (the stream is
// full); TestJetStreamAppender pins both against a real server. The two are
// told apart without reading the description: a live copy on the delivery's
// subject means the former.
const jsErrStoreFailed jetstream.ErrorCode = 10077

// publishTimeout bounds one publish when the caller's context has no
// deadline of its own.
const publishTimeout = 5 * time.Second

var ErrControlStreamSaturated = errors.New("CONTROL_STREAM_SATURATED")

// ControlStreamSaturatedError is a retryable backpressure result: the stream
// refused the publish with Discard=New, nothing was stored, and the command
// stays in PostgreSQL's outbox.
type ControlStreamSaturatedError struct {
	Stream          string
	CurrentMessages int64
	MaxMessages     int64
}

func (e *ControlStreamSaturatedError) Error() string {
	return fmt.Sprintf("%s: stream %s holds %d of %d live commands", ErrControlStreamSaturated, e.Stream, e.CurrentMessages, e.MaxMessages)
}

func (e *ControlStreamSaturatedError) Unwrap() error { return executionapp.ErrDispatchBackpressured }

func (e *ControlStreamSaturatedError) Is(target error) bool {
	return target == ErrControlStreamSaturated || target == executionapp.ErrDispatchBackpressured
}

// ErrControlDeliveryConflict means the delivery's subject already holds a live
// command whose bytes differ from the ones offered. The outbox's prepared
// envelope is immutable, so this is a fault, never a retry.
var ErrControlDeliveryConflict = errors.New("CONTROL_DELIVERY_CONFLICT")

// JetStreamAppender publishes signed envelopes on the command bus. It
// implements StreamAppender.
type JetStreamAppender struct {
	streams map[string]boundStream
	js      jetstream.JetStream
}

// boundStream is a stream handle plus the immutable facts the appender reads
// from it. The handle is shared by every concurrent Append; nats.go's
// Stream.Info rewrites the handle's cached info without a lock, so the
// appender never calls Info or CachedInfo on it after construction (a fresh
// handle serves the saturation report).
type boundStream struct {
	handle  jetstream.Stream
	name    string
	maxMsgs int64
}

// NewJetStreamAppender binds the appender to the streams it may publish
// into. Each stream must already exist with the contract's shape (Verify
// checks it); the appender holds its handle so the re-offer compare can use a
// direct get without a stream info round trip per publish.
func NewJetStreamAppender(js jetstream.JetStream, streams ...jetstream.Stream) (*JetStreamAppender, error) {
	if js == nil || len(streams) == 0 {
		return nil, errors.New("command bus JetStream context and at least one stream are required")
	}
	bound := make(map[string]boundStream, len(streams))
	for _, s := range streams {
		if s == nil {
			return nil, errors.New("command bus stream handle is nil")
		}
		info := s.CachedInfo()
		if _, err := RouteToken(info.Config.Name); err != nil {
			return nil, err
		}
		bound[info.Config.Name] = boundStream{handle: s, name: info.Config.Name, maxMsgs: info.Config.MaxMsgs}
	}
	return &JetStreamAppender{streams: bound, js: js}, nil
}

// Append publishes value on the delivery's subject. It returns
// "<stream>:<sequence>" for a stored (or already live, byte-identical)
// command.
func (a *JetStreamAppender) Append(ctx context.Context, stream, deliveryID string, value []byte) (string, error) {
	if !validDeliveryID(deliveryID) {
		return "", executionapp.ErrInvalidPreparedEnvelope
	}
	bound, ok := a.streams[stream]
	if !ok {
		return "", fmt.Errorf("command bus stream %q is not bound to this appender", stream)
	}
	subject, err := DeliverySubject(stream, deliveryID)
	if err != nil {
		return "", err
	}
	msg := NewCommandMessage(subject, deliveryID, value)
	if encodedTransportMessageBytes(value) > MaxMessageBytes {
		return "", ErrControlMessageLimitExceeded
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, publishTimeout)
		defer cancel()
	}
	ack, err := a.js.PublishMsg(ctx, msg, jetstream.WithExpectStream(stream))
	if err == nil {
		if ack.Duplicate {
			// The Nats-Msg-Id window saw this delivery inside the last two
			// minutes: an ambiguous retry. Compare with the live copy when
			// there is one, exactly as for a per-subject refusal.
			return a.compareLive(ctx, bound, subject, value, fmt.Sprintf("%s:%d", ack.Stream, ack.Sequence))
		}
		return fmt.Sprintf("%s:%d", ack.Stream, ack.Sequence), nil
	}
	var apiErr *jetstream.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode == jsErrStoreFailed {
		live, getErr := bound.handle.GetLastMsgForSubject(ctx, subject)
		switch {
		case getErr == nil:
			if !bytes.Equal(live.Data, value) {
				return "", ErrControlDeliveryConflict
			}
			return fmt.Sprintf("%s:%d", stream, live.Sequence), nil
		case errors.Is(getErr, jetstream.ErrMsgNotFound):
			return "", a.saturated(ctx, bound)
		default:
			return "", fmt.Errorf("read the live copy of a refused command: %w", getErr)
		}
	}
	return "", err
}

func (a *JetStreamAppender) compareLive(ctx context.Context, bound boundStream, subject string, value []byte, entryID string) (string, error) {
	live, err := bound.handle.GetLastMsgForSubject(ctx, subject)
	switch {
	case err == nil:
		if !bytes.Equal(live.Data, value) {
			return "", ErrControlDeliveryConflict
		}
		return fmt.Sprintf("%s:%d", bound.name, live.Sequence), nil
	case errors.Is(err, jetstream.ErrMsgNotFound):
		// Consumed since: a worker acked it after settlement.
		return entryID, nil
	default:
		return "", fmt.Errorf("read the live copy of a duplicate command: %w", err)
	}
}

func (a *JetStreamAppender) saturated(ctx context.Context, bound boundStream) error {
	result := &ControlStreamSaturatedError{Stream: bound.name, MaxMessages: bound.maxMsgs}
	// A fresh handle: its info round trip is the report's state, and it
	// never touches the shared handle's cache.
	if fresh, err := a.js.Stream(ctx, bound.name); err == nil {
		info := fresh.CachedInfo()
		result.CurrentMessages = int64(info.State.Msgs)
		result.MaxMessages = info.Config.MaxMsgs
	}
	return result
}

// NewCommandMessage builds the message the producer publishes, headers
// included. Exported for the integration tests of the other parties.
func NewCommandMessage(subject, deliveryID string, value []byte) *nats.Msg {
	msg := nats.NewMsg(subject)
	msg.Data = append([]byte(nil), value...)
	msg.Header.Set(HeaderMsgID, DeliveryToken(deliveryID))
	msg.Header.Set(HeaderDeliveryID, deliveryID)
	return msg
}

// transportHeaderReserve bounds the encoded headers of one command message:
// the NATS/1.0 status line, Nats-Msg-Id (64), Elitea-Delivery-Id (up to 256),
// Nats-Expected-Stream (up to 45) and their CRLF framing.
const transportHeaderReserve = 512

func encodedTransportMessageBytes(value []byte) int {
	return len(value) + transportHeaderReserve
}

// StreamRequirement is what elitea-main needs a command stream to be.
type StreamRequirement struct {
	Stream string
	// MinMaxAge is the longest execution deadline of every route that
	// publishes into the stream, plus MaxAgeMargin. A MaxAge of zero (no
	// age limit) also satisfies it.
	MinMaxAge time.Duration
	// MaxMessageBytes is the largest message the routes may publish; the
	// stream's MaxMsgSize must admit it.
	MaxMessageBytes int
}

// BindStream reads the stream's configuration and refuses one that is not
// the contract's shape, and the durable consumer the bootstrap creates for
// it. The bootstrap owns both; elitea-main only verifies. A drifted or absent
// asset fails startup with the bootstrap named, instead of a runtime plane
// that publishes into a stream that evicts commands or never delivers them.
func BindStream(ctx context.Context, js jetstream.JetStream, want StreamRequirement) (jetstream.Stream, error) {
	if js == nil {
		return nil, errors.New("command bus JetStream context is required")
	}
	durable, ok := KnownStreams[want.Stream]
	if !ok {
		return nil, ValidateRoute(want.Stream, "")
	}
	filter, err := FilterSubject(want.Stream)
	if err != nil {
		return nil, err
	}
	stream, err := js.Stream(ctx, want.Stream)
	if err != nil {
		return nil, fmt.Errorf("command bus stream %s is unavailable (the nats-bootstrap Job creates it): %w", want.Stream, err)
	}
	c := stream.CachedInfo().Config
	var problems []string
	if len(c.Subjects) != 1 || c.Subjects[0] != filter {
		problems = append(problems, fmt.Sprintf("subjects %v, want [%s]", c.Subjects, filter))
	}
	if c.Retention != jetstream.WorkQueuePolicy {
		problems = append(problems, "retention is not WorkQueue")
	}
	if c.Discard != jetstream.DiscardNew {
		problems = append(problems, "discard is not new")
	}
	if !c.DiscardNewPerSubject || c.MaxMsgsPerSubject != 1 {
		problems = append(problems, "one live message per delivery subject is not enforced (max_msgs_per_subject=1, discard_new_per_subject)")
	}
	if c.MaxMsgs < 1 || c.MaxMsgs > MaxStreamMessages {
		problems = append(problems, fmt.Sprintf("max_msgs %d is outside 1..%d", c.MaxMsgs, MaxStreamMessages))
	}
	if c.MaxBytes < 1 || c.MaxBytes > MaxStreamBytes {
		problems = append(problems, fmt.Sprintf("max_bytes %d is outside 1..%d", c.MaxBytes, MaxStreamBytes))
	}
	if c.MaxMsgSize < int32(want.MaxMessageBytes) || c.MaxMsgSize > MaxMessageBytes {
		problems = append(problems, fmt.Sprintf("max_msg_size %d does not admit %d-byte commands within the %d contract", c.MaxMsgSize, want.MaxMessageBytes, MaxMessageBytes))
	}
	if c.MaxAge != 0 && c.MaxAge < want.MinMaxAge {
		problems = append(problems, fmt.Sprintf("max_age %s would remove live commands; it must be at least %s", c.MaxAge, want.MinMaxAge))
	}
	if !c.AllowDirect {
		problems = append(problems, "allow_direct is off (the re-offer compare reads the live copy with a direct get)")
	}
	if !c.DenyDelete || !c.DenyPurge {
		problems = append(problems, "deny_delete and deny_purge must both be on")
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("command bus stream %s is not the contract's shape (re-run the nats-bootstrap Job): %s",
			want.Stream, strings.Join(problems, "; "))
	}
	consumer, err := stream.Consumer(ctx, durable)
	if err != nil {
		return nil, fmt.Errorf("command bus consumer %s/%s is unavailable (the nats-bootstrap Job creates it): %w", want.Stream, durable, err)
	}
	cc := consumer.CachedInfo().Config
	if cc.FilterSubject != filter || cc.AckPolicy != jetstream.AckExplicitPolicy || cc.AckWait != AckWait || cc.MaxDeliver != -1 {
		return nil, fmt.Errorf("command bus consumer %s/%s is not the contract's shape (filter %q, ack %v, ack_wait %s, max_deliver %d); re-run the nats-bootstrap Job",
			want.Stream, durable, cc.FilterSubject, cc.AckPolicy, cc.AckWait, cc.MaxDeliver)
	}
	return stream, nil
}
