package safety

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// SelfTestRoutineID is the routine registry row of the scheduled self-test
// (elitea_runtime.platform_routines). An operator changes its schedule or
// switches it off in the admin Routines screen.
const SelfTestRoutineID = "main.kill_switch_selftest"

// SelfTestDefaultSchedule is the routine's default schedule.
const SelfTestDefaultSchedule = "24h"

// DefaultStopSLO is how long a suspension may take to reach admission.
const DefaultStopSLO = 10 * time.Second

// SelfTestMetric is the gauge of the last successful pass, in Unix seconds.
// The Prometheus exporter names it
// elitea_kill_switch_selftest_last_success_timestamp.
const SelfTestMetric = "elitea.kill_switch.selftest.last_success_timestamp"

// SelfTestReason is the reason the synthetic suspension carries.
const SelfTestReason = "scheduled kill switch self-test"

// Routine outcome words (libs/go/routines).
const (
	selfTestOutcomeOK          = "ok"
	selfTestOutcomeError       = "error"
	selfTestOutcomeMaintenance = "maintenance"
)

// SelfTestResult is one pass, as the audit trail records it.
type SelfTestResult struct {
	SuspensionID int64
	// Refused: admission refused the synthetic agent while it was suspended.
	Refused bool
	// Resumed: admission admitted it again after the resume.
	Resumed bool
	// StopLatency is the time from the suspend call to the refusal.
	StopLatency time.Duration
	Duration    time.Duration
	Err         error
}

// SelfTestRecorder writes a result to the audit trail.
type SelfTestRecorder func(ctx context.Context, result SelfTestResult)

// SelfTestOptions configures a SelfTest. Maintenance and Record are optional.
type SelfTestOptions struct {
	// Maintenance reports whether maintenance mode is on; the pass does
	// nothing then. An error counts as off, as in the scheduler.
	Maintenance func(ctx context.Context) (bool, error)
	Record      SelfTestRecorder
	SLO         time.Duration
	Meter       metric.Meter
	Now         func() time.Time
}

// SelfTest proves the switch end to end on this replica: it suspends the
// synthetic agent (project 0, application 0, which no run has) through the
// same service an administrator uses, so the row is written and the cancel
// statement runs; it checks that the cached admission gate refuses the
// synthetic agent within the SLO; it resumes and checks that the gate admits
// it again.
//
// What it does NOT prove: that a worker stops a run in flight. Staging a real
// run every day would need a model, a worker and a project. The cancel of a
// run row is proven by the Postgres integration test of the repository, and
// the worker's honouring of a cancel by the worker's own fence tests.
type SelfTest struct {
	service     *Service
	gate        *Gate
	store       Store
	maintenance func(ctx context.Context) (bool, error)
	record      SelfTestRecorder
	slo         time.Duration
	now         func() time.Time
	gauge       metric.Float64Gauge
}

// NewSelfTest builds the self-test.
func NewSelfTest(service *Service, gate *Gate, opts SelfTestOptions) (*SelfTest, error) {
	if service == nil || gate == nil {
		return nil, errors.New("safety: the self-test needs the service and the gate")
	}
	meter := opts.Meter
	if meter == nil {
		meter = otel.Meter("elitea-main")
	}
	gauge, err := meter.Float64Gauge(SelfTestMetric,
		metric.WithDescription("Unix time of the last kill switch self-test that passed."))
	if err != nil {
		return nil, err
	}
	t := &SelfTest{
		service: service, gate: gate, store: service.store, maintenance: opts.Maintenance,
		record: opts.Record, slo: opts.SLO, now: opts.Now, gauge: gauge,
	}
	if t.slo <= 0 {
		t.slo = DefaultStopSLO
	}
	if t.now == nil {
		t.now = time.Now
	}
	return t, nil
}

var selfTestTarget = Target{Scope: ScopeAgent}

var selfTestActor = Actor{UserID: 0, PlatformAdmin: true}

// Pass runs one self-test. It returns a routines outcome word.
func (t *SelfTest) Pass(ctx context.Context) (string, error) {
	if t.maintenance != nil {
		if on, err := t.maintenance(ctx); err == nil && on {
			return selfTestOutcomeMaintenance, nil
		}
	}
	result := t.run(ctx)
	if t.record != nil {
		t.record(context.WithoutCancel(ctx), result)
	}
	if result.Err != nil {
		return selfTestOutcomeError, result.Err
	}
	t.gauge.Record(context.WithoutCancel(ctx), float64(t.now().Unix()))
	return selfTestOutcomeOK, nil
}

func (t *SelfTest) run(ctx context.Context) (result SelfTestResult) {
	started := t.now()
	defer func() { result.Duration = t.now().Sub(started) }()

	created, err := t.suspend(ctx)
	if err != nil {
		result.Err = fmt.Errorf("suspend the synthetic agent: %w", err)
		return result
	}
	result.SuspensionID = created.ID
	// Whatever happens below, the synthetic suspension does not outlive
	// the pass.
	resumed := false
	defer func() {
		if !resumed {
			_, _ = t.service.Resume(context.WithoutCancel(ctx), selfTestActor, ResumeRequest{ID: created.ID, Reason: "self-test cleanup"})
		}
	}()

	subject := Subject{}
	err = t.gate.Check(ctx, subject)
	result.StopLatency = t.now().Sub(started)
	refusal, ok := AsSuspended(err)
	switch {
	case !ok:
		result.Err = fmt.Errorf("admission admitted the suspended synthetic agent (%v)", err)
		return result
	case refusal.SuspensionID != created.ID:
		result.Err = fmt.Errorf("admission refused the synthetic agent for suspension %d, not %d", refusal.SuspensionID, created.ID)
		return result
	case result.StopLatency > t.slo:
		result.Err = fmt.Errorf("the refusal took %s, over the %s SLO", result.StopLatency, t.slo)
		return result
	}
	result.Refused = true

	if _, err := t.service.Resume(ctx, selfTestActor, ResumeRequest{ID: created.ID, Reason: "self-test resume"}); err != nil {
		result.Err = fmt.Errorf("resume the synthetic agent: %w", err)
		return result
	}
	resumed = true
	if refusal, ok := AsSuspended(t.gate.Check(ctx, subject)); ok && refusal.SuspensionID == created.ID {
		result.Err = errors.New("admission still refuses the synthetic agent after the resume")
		return result
	}
	result.Resumed = true
	return result
}

// suspend writes the synthetic suspension. A row left active by a pass that
// crashed is resumed first.
func (t *SelfTest) suspend(ctx context.Context) (Suspension, error) {
	request := CreateRequest{Target: selfTestTarget, Reason: SelfTestReason, Source: SourceSelfTest}
	created, err := t.service.Create(ctx, selfTestActor, request)
	var already *AlreadySuspendedError
	if !errors.As(err, &already) {
		return created, err
	}
	if _, err := t.service.Resume(ctx, selfTestActor, ResumeRequest{ID: already.Existing.ID, Reason: "self-test cleanup"}); err != nil {
		return Suspension{}, err
	}
	return t.service.Create(ctx, selfTestActor, request)
}
