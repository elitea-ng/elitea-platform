// Package conversationretention deletes conversations older than the
// retention policy's maximum (ADR-0042 decision 4, issue #1495).
//
// # Why it runs in elitea-main and not in elitea-scheduler
//
// The other retention sweepers live in elitea-scheduler because each deletes
// rows of one table with one statement. A conversation is a graph (participant
// mappings, message groups and items, attachments, pins, the memory taint
// trigger, sync tombstones) and its stored attachment bytes are in the object
// store, which only elitea-main holds. The delete path that knows all of that
// is elitea-main's (repos.ConversationsRepo, the same sequence a user's delete
// runs). A scheduler copy would be a second deleter that drifts from the
// first; calling elitea-main from the scheduler would be a new internal hop
// for a loop that needs no second process. So the sweep runs here, on the
// shared routine registry (libs/go/routines, routine id
// main.conversation_retention): one replica at a time, schedule and on/off
// editable on the admin Schedules page, like the scheduler's sweepers.
//
// # Age
//
// A conversation's age is its LAST MESSAGE time (its creation when it has
// none), not its creation and not `updated_at`: a conversation in daily use
// for two years is not two years stale, and `updated_at` also moves on a
// rename.
//
// # What a pass does
//
//  1. Maintenance: a pass in a maintenance window deletes nothing. An
//     unreadable switch also deletes nothing (fail closed: this is the one
//     loop whose mistake cannot be undone).
//  2. Policy: the platform row of `conversation` and every project override.
//     With the seeded default (keep forever, no overrides) the pass reads no
//     tenant and deletes nothing.
//  3. Per project with a maximum: candidates older than the cutoff that no
//     active legal hold covers, oldest first, in bounded batches; each is
//     deleted in its own transaction which re-checks age and hold under the
//     row lock and journals the stored attachments.
//  4. Drain: journalled attachment objects are deleted with backoff kept in
//     the journal row.
package conversationretention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/elitea-ng/elitea-platform/libs/go/routines"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/retention"
)

// RoutineID is the routine registry row of the sweep.
const RoutineID = "main.conversation_retention"

// Defaults bounding one pass.
const (
	DefaultInterval       = time.Hour
	DefaultBatchSize      = 100
	DefaultMaxPerPass     = 2000
	DefaultDrainBatch     = 200
	DefaultDrainLease     = 10 * time.Minute
	DefaultRetryBackoff   = 5 * time.Minute
	MaxRetryBackoff       = 24 * time.Hour
	defaultPassTimeoutCap = 30 * time.Minute
)

// Candidate is an expired conversation the store selected.
type Candidate struct {
	ID           int64
	LastActivity time.Time
}

// Outcome is what deleting one candidate decided.
type Outcome string

// The outcomes. Only Deleted removed anything.
const (
	Deleted Outcome = "deleted"
	Held    Outcome = "held"
	Active  Outcome = "active"
	Gone    Outcome = "gone"
)

// AttachmentRef is one stored attachment object.
type AttachmentRef struct {
	Bucket string
	Name   string
}

// Cleanup is one claimed attachment journal row.
type Cleanup struct {
	ID             int64
	ProjectID      int64
	ConversationID int64
	Attachment     AttachmentRef
	Attempts       int
}

// Policies is where the pass reads the policy.
type Policies interface {
	PlatformPolicy(ctx context.Context, class retention.DataClass) (retention.Policy, error)
	ProjectOverrides(ctx context.Context, class retention.DataClass) (map[int64]retention.Policy, error)
}

// Store is the conversation side.
type Store interface {
	RetentionTenantProjects(ctx context.Context) ([]int64, error)
	ExpiredConversations(ctx context.Context, projectID int64, cutoff time.Time, limit int) ([]Candidate, error)
	// HeldExpiredConversations counts the expired conversations a hold keeps.
	// A full-tenant count, so the sweep asks once per project per pass.
	HeldExpiredConversations(ctx context.Context, projectID int64, cutoff time.Time) (int64, error)
	DeleteExpiredConversation(ctx context.Context, projectID, conversationID int64, cutoff time.Time) (Outcome, int, error)
	ClaimAttachmentCleanup(ctx context.Context, limit int, lease time.Duration) ([]Cleanup, error)
	CompleteAttachmentCleanup(ctx context.Context, ids []int64) error
	FailAttachmentCleanup(ctx context.Context, id int64, cause string, retryAt time.Time) error
}

// Purger deletes stored attachment bytes and their object records
// (conversations.DeleteStoredAttachments in production).
type Purger func(ctx context.Context, projectID int64, refs []AttachmentRef) error

// Gate reports whether a maintenance window is open. An error stops the pass.
type Gate func(ctx context.Context) (bool, error)

// Config bounds one pass. Zero fields take the defaults.
type Config struct {
	BatchSize  int
	MaxPerPass int
	DrainBatch int
	DrainLease time.Duration
	Meter      metric.Meter
}

// Stats is what one pass did.
type Stats struct {
	Suppressed  bool
	Projects    int
	Deleted     int64
	SkippedHold int64
	Journalled  int64
	Purged      int64
	Errors      int64
	Truncated   bool
	// LagSeconds is how far past its cutoff the oldest expired, unheld
	// conversation the pass saw was; 0 when none was seen.
	LagSeconds float64
}

// Sweeper is the conversation retention sweep.
type Sweeper struct {
	policies Policies
	store    Store
	purge    Purger
	gate     Gate
	cfg      Config
	logger   *slog.Logger
	now      func() time.Time

	deleted     metric.Int64Counter
	skippedHold metric.Int64Counter
	errors      metric.Int64Counter
	lag         metric.Float64Gauge
}

// New builds the sweep. Every dependency is required: a sweep composed
// without a gate would delete through a maintenance window, and one without a
// purger would leave attachment bytes behind forever.
func New(policies Policies, store Store, purge Purger, gate Gate, cfg Config, logger *slog.Logger) (*Sweeper, error) {
	if policies == nil || store == nil || purge == nil || gate == nil {
		return nil, errors.New("conversation retention: policies, store, purger and gate are required")
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultBatchSize
	}
	if cfg.MaxPerPass <= 0 {
		cfg.MaxPerPass = DefaultMaxPerPass
	}
	if cfg.DrainBatch <= 0 {
		cfg.DrainBatch = DefaultDrainBatch
	}
	if cfg.DrainLease <= 0 {
		cfg.DrainLease = DefaultDrainLease
	}
	if cfg.Meter == nil {
		cfg.Meter = otel.Meter("elitea-main")
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Sweeper{policies: policies, store: store, purge: purge, gate: gate, cfg: cfg, logger: logger, now: time.Now}
	var err error
	if s.deleted, err = cfg.Meter.Int64Counter("retention.conversation.deleted",
		metric.WithDescription("Conversations deleted by the retention sweep.")); err != nil {
		return nil, err
	}
	if s.skippedHold, err = cfg.Meter.Int64Counter("retention.conversation.skipped_hold",
		metric.WithDescription("Expired conversations the sweep kept because a legal hold covers them.")); err != nil {
		return nil, err
	}
	if s.errors, err = cfg.Meter.Int64Counter("retention.conversation.errors",
		metric.WithDescription("Conversation retention failures, by stage.")); err != nil {
		return nil, err
	}
	if s.lag, err = cfg.Meter.Float64Gauge("retention.conversation.lag_seconds",
		metric.WithDescription("How far past its cutoff the oldest expired, unheld conversation is.")); err != nil {
		return nil, err
	}
	return s, nil
}

// Pass runs one pass for the routine registry loop.
func (s *Sweeper) Pass(ctx context.Context) (string, error) {
	passCtx, cancel := context.WithTimeout(ctx, defaultPassTimeoutCap)
	defer cancel()
	stats, err := s.Sweep(passCtx)
	if err != nil {
		s.errors.Add(ctx, 1, metric.WithAttributes(attribute.String("stage", "pass")))
		s.logger.Error("conversation retention: pass failed", "err", err,
			"deleted_before_failure", stats.Deleted)
		return routines.OutcomeError, err
	}
	if stats.Suppressed {
		s.logger.Info("conversation retention: maintenance mode is active; not sweeping")
		return routines.OutcomeMaintenance, nil
	}
	s.logger.Info("conversation retention: pass complete",
		"projects", stats.Projects, "deleted", stats.Deleted, "skipped_hold", stats.SkippedHold,
		"attachments_journalled", stats.Journalled, "attachments_purged", stats.Purged,
		"errors", stats.Errors, "truncated", stats.Truncated, "lag_seconds", stats.LagSeconds)
	return routines.OutcomeOK, nil
}

// Sweep runs one pass and reports it. Exported for tests.
func (s *Sweeper) Sweep(ctx context.Context) (Stats, error) {
	var stats Stats
	paused, err := s.gate(ctx)
	if err != nil {
		return stats, fmt.Errorf("maintenance switch unreadable, not deleting: %w", err)
	}
	if paused {
		stats.Suppressed = true
		return stats, nil
	}

	platform, err := s.policies.PlatformPolicy(ctx, retention.ClassConversation)
	if err != nil {
		return stats, fmt.Errorf("read conversation policy: %w", err)
	}
	var overrides map[int64]retention.Policy
	if platform.AllowProjectOverride {
		if overrides, err = s.policies.ProjectOverrides(ctx, retention.ClassConversation); err != nil {
			return stats, fmt.Errorf("read conversation overrides: %w", err)
		}
	}

	// Which projects can have anything to delete. With keep-forever on the
	// platform only overridden projects can; otherwise every tenant.
	var projects []int64
	if platform.MaxAgeDays == nil {
		for id, override := range overrides {
			if retention.Effective(platform, &override) != nil {
				projects = append(projects, id)
			}
		}
		slices.Sort(projects)
	} else if projects, err = s.store.RetentionTenantProjects(ctx); err != nil {
		return stats, fmt.Errorf("list tenants: %w", err)
	}

	now := s.now().UTC()
	budget := s.cfg.MaxPerPass
	var oldestLag time.Duration
	for _, projectID := range projects {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		var override *retention.Policy
		if o, ok := overrides[projectID]; ok {
			override = &o
		}
		maxDays := retention.Effective(platform, override)
		if maxDays == nil {
			continue
		}
		stats.Projects++
		cutoff := now.Add(-time.Duration(*maxDays) * 24 * time.Hour)
		// Counted once per project per pass, before the batches: the held
		// set does not shrink as they delete unheld rows, and the count
		// reads the whole tenant.
		if held, err := s.store.HeldExpiredConversations(ctx, projectID, cutoff); err != nil {
			stats.Errors++
			s.errors.Add(ctx, 1, metric.WithAttributes(attribute.String("stage", "select")))
			s.logger.Error("conversation retention: held count failed", "project_id", projectID, "err", err)
		} else {
			stats.SkippedHold += held
			s.skippedHold.Add(ctx, held)
		}
		for budget > 0 {
			limit := min(s.cfg.BatchSize, budget)
			candidates, err := s.store.ExpiredConversations(ctx, projectID, cutoff, limit)
			if err != nil {
				stats.Errors++
				s.errors.Add(ctx, 1, metric.WithAttributes(attribute.String("stage", "select")))
				s.logger.Error("conversation retention: candidate read failed", "project_id", projectID, "err", err)
				break
			}
			if len(candidates) > 0 {
				if lag := cutoff.Sub(candidates[0].LastActivity.UTC()); lag > oldestLag {
					oldestLag = lag
				}
			}
			progressed := false
			for _, candidate := range candidates {
				budget--
				outcome, journalled, err := s.store.DeleteExpiredConversation(ctx, projectID, candidate.ID, cutoff)
				if err != nil {
					stats.Errors++
					s.errors.Add(ctx, 1, metric.WithAttributes(attribute.String("stage", "delete")))
					s.logger.Error("conversation retention: delete failed",
						"project_id", projectID, "conversation_id", candidate.ID, "err", err)
					continue
				}
				switch outcome {
				case Deleted:
					progressed = true
					stats.Deleted++
					stats.Journalled += int64(journalled)
					s.deleted.Add(ctx, 1)
				case Held:
					stats.SkippedHold++
					s.skippedHold.Add(ctx, 1)
				}
			}
			if len(candidates) < limit || !progressed {
				// Drained, or every candidate was skipped: asking again would
				// return the same rows.
				break
			}
		}
		if budget <= 0 {
			stats.Truncated = true
			break
		}
	}
	stats.LagSeconds = oldestLag.Seconds()
	s.lag.Record(ctx, stats.LagSeconds)

	if err := s.drain(ctx, &stats); err != nil {
		return stats, err
	}
	return stats, nil
}

// drain deletes journalled attachment objects. A failure is recorded in the
// journal row with backoff and does not fail the pass.
func (s *Sweeper) drain(ctx context.Context, stats *Stats) error {
	claimed, err := s.store.ClaimAttachmentCleanup(ctx, s.cfg.DrainBatch, s.cfg.DrainLease)
	if err != nil {
		s.errors.Add(ctx, 1, metric.WithAttributes(attribute.String("stage", "drain")))
		return fmt.Errorf("claim attachment journal: %w", err)
	}
	var done []int64
	for _, row := range claimed {
		if err := s.purge(ctx, row.ProjectID, []AttachmentRef{row.Attachment}); err != nil {
			stats.Errors++
			s.errors.Add(ctx, 1, metric.WithAttributes(attribute.String("stage", "purge")))
			retryAt := s.now().Add(backoff(row.Attempts))
			if failErr := s.store.FailAttachmentCleanup(ctx, row.ID, err.Error(), retryAt); failErr != nil {
				s.logger.Error("conversation retention: journal update failed", "id", row.ID, "err", failErr)
			}
			continue
		}
		done = append(done, row.ID)
	}
	if err := s.store.CompleteAttachmentCleanup(ctx, done); err != nil {
		return fmt.Errorf("complete attachment journal: %w", err)
	}
	stats.Purged += int64(len(done))
	return nil
}

func backoff(attempts int) time.Duration {
	d := DefaultRetryBackoff
	for i := 1; i < attempts && d < MaxRetryBackoff; i++ {
		d *= 2
	}
	return min(d, MaxRetryBackoff)
}
