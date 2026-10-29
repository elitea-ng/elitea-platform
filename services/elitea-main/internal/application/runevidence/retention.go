package runevidence

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/elitea-ng/elitea-platform/libs/go/routines"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/retention"
)

// The retention routines (libs/go/routines registry rows), one per data
// class, both in elitea-main because only elitea-main holds the object store
// (the reason conversationretention gives).
const (
	ContentRoutineID = "main.step_content_retention"
	RecordRoutineID  = "main.step_record_retention"
)

// Retention defaults bounding one pass.
const (
	DefaultRetentionBatch      = 200
	DefaultRetentionMaxPerPass = 5000
	DefaultPurgeBatch          = 200
	DefaultAnchorBatch         = 500
	defaultPassTimeout         = 30 * time.Minute
)

// PolicyConversationDeleted is the content expiry policy of a step whose
// conversation was deleted.
const PolicyConversationDeleted = "conversation_deleted"

// ExpiryPolicy names a retention expiry: "retention.<class>:<days>d".
func ExpiryPolicy(class retention.DataClass, days int) string {
	return "retention." + string(class) + ":" + strconv.Itoa(days) + "d"
}

// Purge is one step whose expired content objects are still stored.
type Purge struct {
	ID   int64
	Keys []string
}

// Policies is where a pass reads the policy (repos.RetentionPolicyRepo).
type Policies interface {
	PlatformPolicy(ctx context.Context, class retention.DataClass) (retention.Policy, error)
	ProjectOverrides(ctx context.Context, class retention.DataClass) (map[int64]retention.Policy, error)
}

// RetentionStore is the run_steps side of the sweeps (repos.RunSteps).
type RetentionStore interface {
	// EvidenceProjects lists the projects that have runs, in id order.
	EvidenceProjects(ctx context.Context) ([]int64, error)
	// ExpireContent marks up to limit of the project's steps recorded
	// before cutoff whose content is stored and that no active legal hold
	// covers as expired under policy (row first). It returns how many.
	ExpireContent(ctx context.Context, projectID int64, cutoff time.Time, policy string, limit int) (int, error)
	// HeldContent counts the project's steps a hold keeps past cutoff.
	HeldContent(ctx context.Context, projectID int64, cutoff time.Time) (int64, error)
	// ClaimPurges returns up to limit steps with expired content whose
	// objects are still stored.
	ClaimPurges(ctx context.Context, limit int) ([]Purge, error)
	// CompletePurges clears the object keys of purged steps.
	CompletePurges(ctx context.Context, ids []int64) error
	// ExpiredRuns returns up to limit runs of the project whose last step is
	// before cutoff, that no active legal hold covers and that every audit
	// export sink has passed.
	ExpiredRuns(ctx context.Context, projectID int64, cutoff time.Time, limit int) ([]string, error)
	// HeldRuns counts the project's runs a hold keeps past cutoff.
	HeldRuns(ctx context.Context, projectID int64, cutoff time.Time) (int64, error)
	// ExpireRunContent marks every stored content side of the run expired
	// under policy (row first, before the run's rows are deleted).
	ExpireRunContent(ctx context.Context, runID, policy string) error
	// DeleteRun deletes the run's head and steps when it is still expired,
	// unheld and holds no stored content object. deleted is false otherwise.
	DeleteRun(ctx context.Context, runID string, cutoff time.Time) (deleted bool, err error)
	// AnchorPending anchors up to limit settled runs whose head moved since
	// the last anchor (the backstop for a settlement whose anchor failed).
	AnchorPending(ctx context.Context, limit int) (int, error)
}

// Gate reports whether a maintenance window is open. An error stops the pass.
type Gate func(ctx context.Context) (bool, error)

// RetentionConfig bounds one pass. Zero fields take the defaults.
type RetentionConfig struct {
	BatchSize  int
	MaxPerPass int
	PurgeBatch int
	Meter      metric.Meter
}

// RetentionStats is what one pass did.
type RetentionStats struct {
	Suppressed  bool
	Projects    int
	Expired     int64
	Purged      int64
	DeletedRuns int64
	SkippedHold int64
	Anchored    int64
	Errors      int64
	Truncated   bool
}

// Sweeper enforces step_content (Content) and step_record (Records).
type Sweeper struct {
	policies Policies
	store    RetentionStore
	content  ContentStore
	gate     Gate
	cfg      RetentionConfig
	logger   *slog.Logger
	now      func() time.Time

	expired     metric.Int64Counter
	purged      metric.Int64Counter
	deletedRuns metric.Int64Counter
	skippedHold metric.Int64Counter
	errors      metric.Int64Counter
}

// NewSweeper builds both sweeps. content may be nil when no object store is
// composed: nothing is stored, so a purge only clears keys.
func NewSweeper(policies Policies, store RetentionStore, content ContentStore, gate Gate, cfg RetentionConfig, logger *slog.Logger) (*Sweeper, error) {
	if policies == nil || store == nil || gate == nil {
		return nil, errors.New("run evidence retention: policies, store and gate are required")
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultRetentionBatch
	}
	if cfg.MaxPerPass <= 0 {
		cfg.MaxPerPass = DefaultRetentionMaxPerPass
	}
	if cfg.PurgeBatch <= 0 {
		cfg.PurgeBatch = DefaultPurgeBatch
	}
	if cfg.Meter == nil {
		cfg.Meter = otel.Meter("elitea-main")
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Sweeper{policies: policies, store: store, content: content, gate: gate, cfg: cfg, logger: logger, now: time.Now}
	var err error
	for _, instrument := range []struct {
		target *metric.Int64Counter
		name   string
		help   string
	}{
		{&s.expired, "retention.step_content.expired", "Step content sides marked expired by the retention sweeps, by class."},
		{&s.purged, "retention.step_content.purged", "Steps whose expired content objects were deleted."},
		{&s.deletedRuns, "retention.step_record.deleted_runs", "Runs whose step records the retention sweep deleted."},
		{&s.skippedHold, "retention.steps.skipped_hold", "Expired steps or runs kept because a legal hold covers them, by class."},
		{&s.errors, "retention.steps.errors", "Step retention failures, by class and stage."},
	} {
		if *instrument.target, err = cfg.Meter.Int64Counter(instrument.name, metric.WithDescription(instrument.help)); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// ContentPass is the step_content routine's pass.
func (s *Sweeper) ContentPass(ctx context.Context) (string, error) {
	return s.pass(ctx, retention.ClassStepContent, s.SweepContent)
}

// RecordPass is the step_record routine's pass.
func (s *Sweeper) RecordPass(ctx context.Context) (string, error) {
	return s.pass(ctx, retention.ClassStepRecord, s.SweepRecords)
}

func (s *Sweeper) pass(ctx context.Context, class retention.DataClass, sweep func(context.Context) (RetentionStats, error)) (string, error) {
	passCtx, cancel := context.WithTimeout(ctx, defaultPassTimeout)
	defer cancel()
	stats, err := sweep(passCtx)
	if err != nil {
		s.errors.Add(ctx, 1, metric.WithAttributes(attribute.String("class", string(class)), attribute.String("stage", "pass")))
		s.logger.Error("step retention: pass failed", "class", class, "err", err)
		return routines.OutcomeError, err
	}
	if stats.Suppressed {
		s.logger.Info("step retention: maintenance mode is active; not sweeping", "class", class)
		return routines.OutcomeMaintenance, nil
	}
	s.logger.Info("step retention: pass complete", "class", class, "projects", stats.Projects,
		"expired", stats.Expired, "purged", stats.Purged, "deleted_runs", stats.DeletedRuns,
		"skipped_hold", stats.SkippedHold, "anchored", stats.Anchored, "errors", stats.Errors, "truncated", stats.Truncated)
	return routines.OutcomeOK, nil
}

// projectMaxima resolves each project's effective maximum for class; nil
// when nothing can expire.
func (s *Sweeper) projectMaxima(ctx context.Context, class retention.DataClass) (map[int64]int, error) {
	platform, err := s.policies.PlatformPolicy(ctx, class)
	if err != nil {
		return nil, fmt.Errorf("read %s policy: %w", class, err)
	}
	var overrides map[int64]retention.Policy
	if platform.AllowProjectOverride {
		if overrides, err = s.policies.ProjectOverrides(ctx, class); err != nil {
			return nil, fmt.Errorf("read %s overrides: %w", class, err)
		}
	}
	var projects []int64
	if platform.MaxAgeDays == nil {
		for id := range overrides {
			projects = append(projects, id)
		}
		slices.Sort(projects)
	} else if projects, err = s.store.EvidenceProjects(ctx); err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	maxima := make(map[int64]int, len(projects))
	for _, id := range projects {
		var override *retention.Policy
		if o, ok := overrides[id]; ok {
			override = &o
		}
		if days := retention.Effective(platform, override); days != nil {
			maxima[id] = *days
		}
	}
	return maxima, nil
}

func (s *Sweeper) paused(ctx context.Context) (bool, error) {
	paused, err := s.gate(ctx)
	if err != nil {
		return false, fmt.Errorf("maintenance switch unreadable, not deleting: %w", err)
	}
	return paused, nil
}

// SweepContent expires stored content past the step_content maximum (rows
// first), then deletes the objects of every expired side, including those a
// conversation deletion or the step_record sweep expired.
func (s *Sweeper) SweepContent(ctx context.Context) (RetentionStats, error) {
	var stats RetentionStats
	if paused, err := s.paused(ctx); err != nil || paused {
		stats.Suppressed = paused
		return stats, err
	}
	maxima, err := s.projectMaxima(ctx, retention.ClassStepContent)
	if err != nil {
		return stats, err
	}
	class := attribute.String("class", string(retention.ClassStepContent))
	now := s.now().UTC()
	budget := s.cfg.MaxPerPass
	for _, projectID := range sortedKeys(maxima) {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		days := maxima[projectID]
		stats.Projects++
		cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
		if held, err := s.store.HeldContent(ctx, projectID, cutoff); err == nil {
			stats.SkippedHold += held
			s.skippedHold.Add(ctx, held, metric.WithAttributes(class))
		} else {
			s.fail(ctx, &stats, class, "select", projectID, err)
		}
		for budget > 0 {
			limit := min(s.cfg.BatchSize, budget)
			n, err := s.store.ExpireContent(ctx, projectID, cutoff, ExpiryPolicy(retention.ClassStepContent, days), limit)
			if err != nil {
				s.fail(ctx, &stats, class, "expire", projectID, err)
				break
			}
			budget -= n
			stats.Expired += int64(n)
			s.expired.Add(ctx, int64(n), metric.WithAttributes(class))
			if n < limit {
				break
			}
		}
		if budget <= 0 {
			stats.Truncated = true
			break
		}
	}
	return stats, s.drain(ctx, &stats)
}

// SweepRecords anchors pending runs, then deletes whole runs past the
// step_record maximum: their content first (expired, then purged), then the
// rows, so no object outlives its row.
func (s *Sweeper) SweepRecords(ctx context.Context) (RetentionStats, error) {
	var stats RetentionStats
	if paused, err := s.paused(ctx); err != nil || paused {
		stats.Suppressed = paused
		return stats, err
	}
	class := attribute.String("class", string(retention.ClassStepRecord))
	if anchored, err := s.store.AnchorPending(ctx, DefaultAnchorBatch); err != nil {
		s.fail(ctx, &stats, class, "anchor", 0, err)
	} else {
		stats.Anchored = int64(anchored)
	}
	maxima, err := s.projectMaxima(ctx, retention.ClassStepRecord)
	if err != nil {
		return stats, err
	}
	now := s.now().UTC()
	budget := s.cfg.MaxPerPass
	for _, projectID := range sortedKeys(maxima) {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		days := maxima[projectID]
		stats.Projects++
		cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
		if held, err := s.store.HeldRuns(ctx, projectID, cutoff); err == nil {
			stats.SkippedHold += held
			s.skippedHold.Add(ctx, held, metric.WithAttributes(class))
		} else {
			s.fail(ctx, &stats, class, "select", projectID, err)
		}
		for budget > 0 {
			limit := min(s.cfg.BatchSize, budget)
			runs, err := s.store.ExpiredRuns(ctx, projectID, cutoff, limit)
			if err != nil {
				s.fail(ctx, &stats, class, "select", projectID, err)
				break
			}
			budget -= len(runs)
			expired := make([]string, 0, len(runs))
			for _, runID := range runs {
				if err := s.store.ExpireRunContent(ctx, runID, ExpiryPolicy(retention.ClassStepRecord, days)); err != nil {
					s.fail(ctx, &stats, class, "expire", projectID, err)
					continue
				}
				expired = append(expired, runID)
			}
			if err := s.drain(ctx, &stats); err != nil {
				s.fail(ctx, &stats, class, "purge", projectID, err)
				break
			}
			progressed := false
			for _, runID := range expired {
				deleted, err := s.store.DeleteRun(ctx, runID, cutoff)
				if err != nil {
					s.fail(ctx, &stats, class, "delete", projectID, err)
					continue
				}
				if deleted {
					progressed = true
					stats.DeletedRuns++
					s.deletedRuns.Add(ctx, 1)
				}
			}
			if len(runs) < limit || !progressed {
				break
			}
		}
		if budget <= 0 {
			stats.Truncated = true
			break
		}
	}
	return stats, nil
}

// drain deletes the objects of expired content and clears their keys. A
// failed object delete leaves the key, and the next pass retries.
func (s *Sweeper) drain(ctx context.Context, stats *RetentionStats) error {
	for {
		claimed, err := s.store.ClaimPurges(ctx, s.cfg.PurgeBatch)
		if err != nil {
			return fmt.Errorf("claim content purges: %w", err)
		}
		if len(claimed) == 0 {
			return nil
		}
		var done []int64
		for _, purge := range claimed {
			if s.content != nil && len(purge.Keys) > 0 {
				if err := s.content.Delete(ctx, purge.Keys); err != nil {
					stats.Errors++
					s.errors.Add(ctx, 1, metric.WithAttributes(attribute.String("stage", "purge")))
					s.logger.Error("step retention: content delete failed", "step_row", purge.ID, "err", err)
					continue
				}
			}
			done = append(done, purge.ID)
		}
		if err := s.store.CompletePurges(ctx, done); err != nil {
			return fmt.Errorf("complete content purges: %w", err)
		}
		stats.Purged += int64(len(done))
		s.purged.Add(ctx, int64(len(done)))
		if len(done) < len(claimed) || len(claimed) < s.cfg.PurgeBatch {
			// A failure stays for the next pass rather than spinning here.
			return nil
		}
	}
}

func (s *Sweeper) fail(ctx context.Context, stats *RetentionStats, class attribute.KeyValue, stage string, projectID int64, err error) {
	stats.Errors++
	s.errors.Add(ctx, 1, metric.WithAttributes(class, attribute.String("stage", stage)))
	s.logger.Error("step retention: "+stage+" failed", "class", class.Value.AsString(), "project_id", projectID, "err", err)
}

func sortedKeys(m map[int64]int) []int64 {
	keys := make([]int64, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
