package indexing

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
)

type startResolverStub struct {
	request StartRequest
	inputs  AuthoritativeInputs
	err     error
	calls   int
}

func (s *startResolverStub) Resolve(_ context.Context, request StartRequest) (AuthoritativeInputs, error) {
	s.calls++
	s.request = request.Clone()
	return s.inputs, s.err
}

type startAdmissionStub struct {
	request SubmitRequest
	outcome AdmissionOutcome
	err     error
	calls   int
}

func (s *startAdmissionStub) Submit(_ context.Context, request SubmitRequest) (AdmissionOutcome, error) {
	s.calls++
	s.request = request
	return s.outcome, s.err
}

func TestStartServiceResolvesThenAdmitsCurrentUserRequest(t *testing.T) {
	now := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	request := validStartServiceRequest()
	inputs := validStartServiceInputs()
	resolver := &startResolverStub{inputs: inputs}
	admissions := &startAdmissionStub{outcome: AdmissionOutcome{
		AdmissionOutcome: executionapp.AdmissionOutcome{
			ExecutionID: "execution-1",
			CommandID:   "command-1",
			Created:     true,
			AdmittedAt:  now,
			Deadline:    now.Add(time.Hour),
		},
		Generation:             1,
		IndexGeneration:        1,
		IndexMetaID:            "index-meta-1",
		IndexMetaCorrelationID: "message-1",
		IndexMetaInitializedAt: timePointer(now),
	}}
	generated := 0
	service, err := NewStartService(resolver, admissions, func() (string, error) {
		generated++
		return "unused", nil
	})
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := service.StartIndexData(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.TaskID != "execution-1" || !outcome.Created ||
		resolver.calls != 1 || admissions.calls != 1 || generated != 0 {
		t.Fatalf("outcome=%+v resolver=%d admission=%d generated=%d", outcome, resolver.calls, admissions.calls, generated)
	}
	if !reflect.DeepEqual(resolver.request, request) {
		t.Fatalf("resolver request=%+v want=%+v", resolver.request, request)
	}
	wantIdentity := executionapp.AdmissionIdentity{
		TenantID: "7", ResourceProjectID: "7", ProjectionProjectID: "7", ActorID: "11",
	}
	if admissions.request.Identity != wantIdentity || admissions.request.ToolkitID != 42 ||
		admissions.request.Initiator != executiondomain.IndexIngestInitiatorUser ||
		admissions.request.CorrelationID != request.MessageID ||
		admissions.request.ClientStreamID != request.StreamID ||
		admissions.request.ClientMessageID != request.MessageID ||
		admissions.request.SIOEvent != request.SIOEvent ||
		!reflect.DeepEqual(admissions.request.Inputs, inputs) {
		t.Fatalf("admission request=%+v", admissions.request)
	}
	if !strings.HasPrefix(admissions.request.IdempotencyKey, indexStartIdempotencyPrefix) || len(admissions.request.IdempotencyKey) != len(indexStartIdempotencyPrefix)+64 {
		t.Fatalf("idempotency key=%q", admissions.request.IdempotencyKey)
	}

	firstKey := admissions.request.IdempotencyKey
	if _, err := service.StartIndexData(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if admissions.request.IdempotencyKey != firstKey {
		t.Fatal("same current UI correlation produced a different idempotency key")
	}
}

func TestStartServiceUsesGeneratedCorrelationOnlyWhenCurrentIDsAreAbsent(t *testing.T) {
	request := validStartServiceRequest()
	request.StreamID = ""
	request.MessageID = ""
	resolver := &startResolverStub{inputs: validStartServiceInputs()}
	admissions := &startAdmissionStub{outcome: validStartAdmissionOutcome()}
	generated := 0
	service, err := NewStartService(resolver, admissions, func() (string, error) {
		generated++
		return "generated-correlation-" + string(rune('0'+generated)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.StartIndexData(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	first := admissions.request.IdempotencyKey
	if _, err := service.StartIndexData(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if generated != 2 || first == admissions.request.IdempotencyKey {
		t.Fatalf("generated=%d first=%q second=%q", generated, first, admissions.request.IdempotencyKey)
	}
	if admissions.request.CorrelationID != "generated-correlation-2" {
		t.Fatalf("generated correlation=%q", admissions.request.CorrelationID)
	}
}

func TestStartServiceAdmitsScheduleWithCreatedByAttributionAndNoBrowserStream(t *testing.T) {
	inputs := validStartServiceInputs()
	admissions := &startAdmissionStub{outcome: validStartAdmissionOutcome()}
	service, err := NewStartService(
		&startResolverStub{},
		admissions,
		func() (string, error) { return "unused", nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := service.StartScheduledIndexData(
		context.Background(),
		ScheduledStartRequest{
			ProjectID:              7,
			AttributionActorUserID: 23,
			ToolkitID:              42,
			Inputs:                 inputs,
			IdempotencyKey:         "index-schedule-v1:stable",
			CorrelationID:          "index-schedule-v1:stable",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.TaskID != "execution-1" || !outcome.Created ||
		admissions.calls != 1 {
		t.Fatalf("outcome=%+v admission calls=%d", outcome, admissions.calls)
	}
	if admissions.request.Identity.ActorID != "23" ||
		admissions.request.Identity.ResourceProjectID != "7" ||
		admissions.request.Initiator != executiondomain.IndexIngestInitiatorSchedule ||
		admissions.request.IdempotencyKey != "index-schedule-v1:stable" ||
		admissions.request.CorrelationID != "index-schedule-v1:stable" ||
		admissions.request.ClientStreamID != "" ||
		admissions.request.ClientMessageID != "" ||
		admissions.request.SIOEvent != "" ||
		!reflect.DeepEqual(admissions.request.Inputs, inputs) {
		t.Fatalf("scheduled admission=%+v", admissions.request)
	}

	admissions.outcome.Created = false
	replay, err := service.StartScheduledIndexData(
		context.Background(),
		ScheduledStartRequest{
			ProjectID:              7,
			AttributionActorUserID: 23,
			ToolkitID:              42,
			Inputs:                 inputs,
			IdempotencyKey:         "index-schedule-v1:stable",
			CorrelationID:          "index-schedule-v1:stable",
		},
	)
	if err != nil || replay.Created {
		t.Fatalf("idempotent scheduled replay=%+v error=%v", replay, err)
	}
}

func TestStartServiceRejectsInvalidScheduleBeforeAdmission(t *testing.T) {
	valid := ScheduledStartRequest{
		ProjectID:              7,
		AttributionActorUserID: 23,
		ToolkitID:              42,
		Inputs:                 validStartServiceInputs(),
		IdempotencyKey:         "index-schedule-v1:stable",
		CorrelationID:          "index-schedule-v1:stable",
	}
	tests := map[string]func(*ScheduledStartRequest){
		"project":     func(value *ScheduledStartRequest) { value.ProjectID = 0 },
		"actor":       func(value *ScheduledStartRequest) { value.AttributionActorUserID = 0 },
		"toolkit":     func(value *ScheduledStartRequest) { value.ToolkitID = math.MaxInt32 + 1 },
		"idempotency": func(value *ScheduledStartRequest) { value.IdempotencyKey = "" },
		"correlation": func(value *ScheduledStartRequest) { value.CorrelationID = "" },
		"inputs": func(value *ScheduledStartRequest) {
			value.Inputs.ToolParameters = json.RawMessage(`{}`)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			admissions := &startAdmissionStub{outcome: validStartAdmissionOutcome()}
			service, err := NewStartService(
				&startResolverStub{},
				admissions,
				func() (string, error) { return "unused", nil },
			)
			if err != nil {
				t.Fatal(err)
			}
			request := valid
			request.Inputs = valid.Inputs.Clone()
			mutate(&request)
			if _, err := service.StartScheduledIndexData(
				context.Background(),
				request,
			); !errors.Is(err, ErrInvalidIndexStart) {
				t.Fatalf("error=%v", err)
			}
			if admissions.calls != 0 {
				t.Fatal("invalid schedule crossed admission")
			}
		})
	}
}

func TestStartServiceRejectsInvalidInputBeforeDependencies(t *testing.T) {
	for name, mutate := range map[string]func(*StartRequest){
		"invalid request":                func(request *StartRequest) { request.ProjectID = 0 },
		"toolkit outside current schema": func(request *StartRequest) { request.ToolkitID = math.MaxInt32 + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			resolver := &startResolverStub{inputs: validStartServiceInputs()}
			admissions := &startAdmissionStub{outcome: validStartAdmissionOutcome()}
			service, err := NewStartService(resolver, admissions, func() (string, error) { return "id", nil })
			if err != nil {
				t.Fatal(err)
			}
			request := validStartServiceRequest()
			mutate(&request)
			if _, err := service.StartIndexData(context.Background(), request); !errors.Is(err, ErrInvalidIndexStart) {
				t.Fatalf("error=%v", err)
			}
			if resolver.calls != 0 || admissions.calls != 0 {
				t.Fatal("invalid request crossed a dependency boundary")
			}
		})
	}
}

func TestStartServicePreservesResolverAndAdmissionFailures(t *testing.T) {
	resolverFailure := errors.New("resolver failure")
	resolver := &startResolverStub{err: resolverFailure}
	admissions := &startAdmissionStub{outcome: validStartAdmissionOutcome()}
	service, err := NewStartService(resolver, admissions, func() (string, error) { return "id", nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.StartIndexData(context.Background(), validStartServiceRequest()); !errors.Is(err, resolverFailure) {
		t.Fatalf("resolver error=%v", err)
	}
	if admissions.calls != 0 {
		t.Fatal("resolver failure reached admission")
	}

	admissionFailure := errors.New("admission failure")
	resolver = &startResolverStub{inputs: validStartServiceInputs()}
	admissions = &startAdmissionStub{err: admissionFailure}
	service, err = NewStartService(resolver, admissions, func() (string, error) { return "id", nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.StartIndexData(context.Background(), validStartServiceRequest()); !errors.Is(err, admissionFailure) {
		t.Fatalf("admission error=%v", err)
	}
}

func TestStartServiceFailsClosedOnMissingCorrelationOrInvalidOutcome(t *testing.T) {
	request := validStartServiceRequest()
	request.StreamID = ""
	request.MessageID = ""
	resolver := &startResolverStub{inputs: validStartServiceInputs()}
	admissions := &startAdmissionStub{outcome: validStartAdmissionOutcome()}
	service, err := NewStartService(resolver, admissions, func() (string, error) { return "", nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.StartIndexData(context.Background(), request); err == nil || admissions.calls != 0 {
		t.Fatalf("empty generated correlation error=%v admission calls=%d", err, admissions.calls)
	}

	service, err = NewStartService(resolver, &startAdmissionStub{}, func() (string, error) { return "id", nil })
	if err != nil {
		t.Fatal(err)
	}
	request = validStartServiceRequest()
	if _, err := service.StartIndexData(context.Background(), request); err == nil {
		t.Fatal("invalid durable admission outcome was accepted")
	}

	uninitialized := validStartAdmissionOutcome()
	uninitialized.IndexMetaInitializedAt = nil
	service, err = NewStartService(
		resolver,
		&startAdmissionStub{outcome: uninitialized},
		func() (string, error) { return "id", nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.StartIndexData(context.Background(), request); err == nil {
		t.Fatal("HTTP start accepted an admission before PgVector metadata initialization")
	}
}

func TestNewStartServiceRequiresDependencies(t *testing.T) {
	resolver := &startResolverStub{}
	admissions := &startAdmissionStub{}
	for _, create := range []func() (*StartService, error){
		func() (*StartService, error) {
			return NewStartService(nil, admissions, func() (string, error) { return "id", nil })
		},
		func() (*StartService, error) {
			return NewStartService(resolver, nil, func() (string, error) { return "id", nil })
		},
		func() (*StartService, error) { return NewStartService(resolver, admissions, nil) },
	} {
		if _, err := create(); err == nil {
			t.Fatal("incomplete start service was accepted")
		}
	}
}

func validStartServiceRequest() StartRequest {
	model := "gpt-test"
	return StartRequest{
		ProjectID:            7,
		ActorUserID:          11,
		ToolkitID:            42,
		ToolParameters:       json.RawMessage(`{"index_name":"docs"}`),
		RequestedLLMModel:    &model,
		RequestedLLMSettings: json.RawMessage(`{"temperature":0.1}`),
		StreamID:             "stream-1",
		MessageID:            "message-1",
		SIOEvent:             CurrentIndexSIOEvent,
	}
}

func validStartServiceInputs() AuthoritativeInputs {
	model := "gpt-test"
	return AuthoritativeInputs{
		ToolkitConfiguration: json.RawMessage(`{"id":42,"type":"github"}`),
		ToolParameters:       json.RawMessage(`{"index_name":"docs"}`),
		LLMModel:             &model,
		LLMConfiguration:     json.RawMessage(`{"temperature":0.1}`),
	}
}

func validStartAdmissionOutcome() AdmissionOutcome {
	now := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	return AdmissionOutcome{
		AdmissionOutcome: executionapp.AdmissionOutcome{
			ExecutionID: "execution-1",
			CommandID:   "command-1",
			Created:     true,
			AdmittedAt:  now,
			Deadline:    now.Add(time.Hour),
		},
		Generation:             1,
		IndexGeneration:        1,
		IndexMetaID:            "index-meta-1",
		IndexMetaCorrelationID: "message-1",
		IndexMetaInitializedAt: timePointer(now),
	}
}

func timePointer(value time.Time) *time.Time { return &value }
