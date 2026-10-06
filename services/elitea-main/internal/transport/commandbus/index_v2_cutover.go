package commandbus

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/cutover"
)

// IndexV2CutoverReader reads the complete state of one command stream and its
// durable consumer: the "drained" gate. It does not decode a command
// version: a coordinated switch of the index route requires the stream, the
// consumer's outstanding deliveries and the per-delivery subjects to be
// completely empty first.
//
//	StreamEntries    = StreamInfo.State.Msgs
//	PendingEntries   = ConsumerInfo.NumAckPending + NumRedelivered + NumPending
//	DeliveryMappings = StreamInfo.State.NumSubjects (equal to Msgs by construction)
type IndexV2CutoverReader struct {
	js       jetstream.JetStream
	stream   string
	consumer string
}

func NewIndexV2CutoverReader(js jetstream.JetStream, stream string) (*IndexV2CutoverReader, error) {
	if js == nil {
		return nil, errors.New("index v2 cutover JetStream context is required")
	}
	consumer, ok := KnownStreams[stream]
	if !ok {
		return nil, ValidateRoute(stream, "")
	}
	return &IndexV2CutoverReader{js: js, stream: stream, consumer: consumer}, nil
}

func (r *IndexV2CutoverReader) ReadIndexControlState(ctx context.Context) (cutover.IndexControlState, error) {
	if ctx == nil {
		return cutover.IndexControlState{}, errors.New("index v2 cutover context is required")
	}
	if err := ctx.Err(); err != nil {
		return cutover.IndexControlState{}, err
	}
	stream, err := r.js.Stream(ctx, r.stream)
	if err != nil {
		return cutover.IndexControlState{}, cutoverError(ctx, "read index stream state", err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		return cutover.IndexControlState{}, cutoverError(ctx, "read index stream state", err)
	}
	consumer, err := stream.Consumer(ctx, r.consumer)
	if err != nil {
		if errors.Is(err, jetstream.ErrConsumerNotFound) {
			return cutover.IndexControlState{}, errors.New("read index consumer state: the required durable consumer is absent")
		}
		return cutover.IndexControlState{}, cutoverError(ctx, "read index consumer state", err)
	}
	cinfo := consumer.CachedInfo()
	return cutover.IndexControlState{
		StreamEntries:    int64(info.State.Msgs),
		PendingEntries:   int64(cinfo.NumAckPending) + int64(cinfo.NumRedelivered) + int64(cinfo.NumPending),
		DeliveryMappings: int64(info.State.NumSubjects),
	}, nil
}

func cutoverError(ctx context.Context, operation string, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		return fmt.Errorf("%s: the stream is absent", operation)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

var _ cutover.IndexControlStateReader = (*IndexV2CutoverReader)(nil)
