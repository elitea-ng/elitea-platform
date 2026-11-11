package heartbeat

// The platform-scheduler job that runs one heartbeat pass a minute. It is a
// schedulingapp.Handler, registered as pipeline.schedule.scan.v1 is: the
// framework owns the clock, the occurrence lease, the claim fence and the
// catch-up budget. The mode is durable admission: the ACK means "the due
// agents were triaged and their runs were admitted", not "the runs ended".
// A pass suppressed by maintenance is a success, not a retry.

import (
	"context"
	"errors"
	"time"

	schedulingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/scheduling"
)

// The job's registered identity.
const (
	ScanJobID       = "agent.heartbeat.scan.v1"
	ScanJobRevision = "agent-heartbeat-scan-r1"
	ScanJobCadence  = "* * * * *"
	// ScanJobTimeout is shorter than the framework's two-minute lease.
	ScanJobTimeout = 90 * time.Second
)

// ScanJob is the handler the platform scheduler registers.
type ScanJob struct {
	service *Service
}

// NewScanJob wraps the service as a scheduled job.
func NewScanJob(service *Service) (*ScanJob, error) {
	if service == nil {
		return nil, errors.New("heartbeat: a service is required for the scan job")
	}
	return &ScanJob{service: service}, nil
}

// Name reports the registered job id.
func (*ScanJob) Name() string { return ScanJobID }

// Execute runs one pass. An occurrence that is not this registration's is
// refused.
func (job *ScanJob) Execute(ctx context.Context, occurrence schedulingapp.Occurrence) (schedulingapp.Outcome, error) {
	if job == nil || job.service == nil || ctx == nil ||
		occurrence.InvocationID == "" ||
		occurrence.JobID != ScanJobID ||
		occurrence.ScheduleRevision != ScanJobRevision ||
		occurrence.DueAt.IsZero() ||
		occurrence.LeaseEpoch <= 0 ||
		occurrence.ClaimFence == "" {
		return "", errors.New("heartbeat: invalid scheduled occurrence")
	}
	result, err := job.service.Scan(ctx)
	if errors.Is(err, ErrPassIncomplete) {
		// Not a failure: the agents that did not wake stay due and the next
		// minute's pass takes them. A retry would only repeat this pass.
		job.service.cfg.Logger.Warn("heartbeat: the pass stopped before every due agent woke",
			"due", result.Due, "woken", result.Woken)
		return schedulingapp.OutcomeDurablyAdmitted, nil
	}
	if err != nil {
		return "", err
	}
	return schedulingapp.OutcomeDurablyAdmitted, nil
}

var _ schedulingapp.Handler = (*ScanJob)(nil)
