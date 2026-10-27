package commandbus

import (
	"context"
	"errors"
	"testing"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/proto"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
)

type governedAppenderStub struct {
	appenderStub
	governedCalls int
}

func (a *governedAppenderStub) AppendGoverned(_ context.Context, stream, deliveryID string, value []byte) (string, error) {
	a.governedCalls++
	a.stream, a.deliveryID, a.value = stream, deliveryID, append([]byte(nil), value...)
	return "1-0", nil
}

func governedReference(t *testing.T, executionID string) securitycontext.Reference {
	t.Helper()
	_, ref, err := securitycontext.Context{
		PolicyRevisionID: "2026-10-r1",
		PolicyDigest:     runtimedomain.SHA256([]byte("policy")),
		TenantProjectID:  42,
		IssuedAt:         time.UnixMilli(1_790_000_000_000).UTC(),
	}.Marshal(executionID)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func emittedCommand(t *testing.T, value []byte) *runtimev1.WorkerCommandV1 {
	t.Helper()
	var envelope runtimev1.SignedWorkerCommandEnvelopeV1
	if err := proto.Unmarshal(value, &envelope); err != nil {
		t.Fatal(err)
	}
	var command runtimev1.WorkerCommandV1
	if err := proto.Unmarshal(envelope.GetWorkerCommandBytes(), &command); err != nil {
		t.Fatal(err)
	}
	return &command
}

// A governed dispatch carries field 65 and goes out on the governed subject;
// a legacy one carries no field 65 and goes out as before (#1115).
func TestProducerRoutesGovernedCommandsToTheGovernedSubject(t *testing.T) {
	appender := &governedAppenderStub{}
	producer, err := NewProducer(validProducerConfig(), &signerStub{}, appender)
	if err != nil {
		t.Fatal(err)
	}
	legacy := validTransportDispatch()
	prepared, err := producer.PrepareValidation(context.Background(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.AppendPrepared(context.Background(), legacy.OutboxID, prepared); err != nil {
		t.Fatal(err)
	}
	if appender.calls != 1 || appender.governedCalls != 0 || emittedCommand(t, appender.value).GetSecurityContextRef() != nil {
		t.Fatalf("legacy dispatch: append=%d governed=%d", appender.calls, appender.governedCalls)
	}

	governed := validTransportDispatch()
	governed.SecurityContext = governedReference(t, governed.ExecutionID)
	prepared, err = producer.PrepareValidation(context.Background(), governed)
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.AppendPrepared(context.Background(), governed.OutboxID, prepared); err != nil {
		t.Fatal(err)
	}
	if appender.calls != 1 || appender.governedCalls != 1 {
		t.Fatalf("governed dispatch: append=%d governed=%d", appender.calls, appender.governedCalls)
	}
	ref, err := securitycontext.ReferenceFromProto(emittedCommand(t, appender.value).GetSecurityContextRef())
	if err != nil || ref != governed.SecurityContext {
		t.Fatalf("field 65 = %+v, %v; want %+v", ref, err, governed.SecurityContext)
	}
}

// An appender that cannot publish on the governed subject refuses a
// governed command rather than delivering it to a legacy consumer.
func TestProducerRefusesAGovernedCommandWithoutAGovernedAppender(t *testing.T) {
	appender := &appenderStub{}
	producer, err := NewProducer(validProducerConfig(), &signerStub{}, appender)
	if err != nil {
		t.Fatal(err)
	}
	governed := validTransportDispatch()
	governed.SecurityContext = governedReference(t, governed.ExecutionID)
	prepared, err := producer.PrepareValidation(context.Background(), governed)
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.AppendPrepared(context.Background(), governed.OutboxID, prepared); !errors.Is(err, executionapp.ErrInvalidPreparedEnvelope) || appender.calls != 0 {
		t.Fatalf("append = %v (calls %d), want a refusal before the bus", err, appender.calls)
	}
}
