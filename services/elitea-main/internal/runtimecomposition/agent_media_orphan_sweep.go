package runtimecomposition

// The agent-media orphan sweep (ADR-0050 decision 6, #1855).
//
// The avatar pack routes write straight to each project's reserved
// `agent-media` bucket with no artifacts row (agentmediaapi), so neither
// the artifact retention sweep nor bucket deletion ever sees those objects.
// Every "Generate stills" stores four candidates whether or not one is
// kept, and a pack edit leaves its old files behind: they accumulated for
// good.
//
// This job deletes a file that NOTHING names any more, once it is older
// than a grace period. "Names" is global and wide on purpose
// (repos.AgentMediaReferencesRepository): a published or mirrored copy
// points other projects at the source project's URLs, and a revision
// snapshot or a release definition can bring an old file back. The grace
// period covers drafts: a generated still is stored before the author
// saves the version that names it.
//
// Fail closed: when the reference set cannot be read in full, the tick
// deletes nothing. A file of unknown age (a backend that reports no
// modification time) is kept.
//
// NOT HERE, pending owner decisions (#1855): counting agent-media against
// the project storage quota, and purging the bucket when a project is
// deleted (which would break the avatars of catalogue copies, which point
// at the source project's files, unless publishing copies them too).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/agentmediaapi"
	schedulingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/scheduling"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

const (
	agentMediaOrphanSweepCapability = "agent.media.orphan.sweep.v1"
	agentMediaOrphanSweepRevision   = "agent-media-orphan-sweep-r1"
	// Daily, off the hour. Nothing about an orphan is urgent, and each tick
	// reads every tenant's versions.
	agentMediaOrphanSweepCadence        = "23 3 * * *"
	// Under the scheduler's two-minute lease, which every job's timeout must
	// be; the per-tick delete bound keeps a tick well inside it.
	agentMediaOrphanSweepHandlerTimeout = 90 * time.Second
	// agentMediaOrphanGrace is how long an unnamed file is kept. Seven days
	// is a placeholder for the owner's retention decision (#1855): long
	// enough for an author to come back to unsaved stills, short enough
	// that rejected candidates do not pile up.
	agentMediaOrphanGrace = 7 * 24 * time.Hour
	// agentMediaOrphanMaxDeletesPerTick bounds one tick's deletes; a
	// backlog drains over the following days.
	agentMediaOrphanMaxDeletesPerTick = 1000
	agentMediaOrphanListPage          = int32(1000)
)

var errAgentMediaOrphanSweepInvalidOccurrence = errors.New("invalid agent media orphan sweep occurrence")

// agentMediaReferences is the slice of repos.AgentMediaReferencesRepository
// the sweep needs.
type agentMediaReferences interface {
	TenantProjects(ctx context.Context) ([]int64, error)
	Referenced(ctx context.Context) (map[string]struct{}, error)
}

type agentMediaOrphanSweep struct {
	references agentMediaReferences
	store      storage.ObjectStore
	now        func() time.Time
}

func newAgentMediaOrphanSweep(references agentMediaReferences, store storage.ObjectStore) (*agentMediaOrphanSweep, error) {
	if references == nil || store == nil {
		return nil, errors.New("agent media orphan sweep dependencies are required")
	}
	return &agentMediaOrphanSweep{references: references, store: store, now: time.Now}, nil
}

func (*agentMediaOrphanSweep) Name() string {
	return agentMediaOrphanSweepCapability
}

func (s *agentMediaOrphanSweep) Execute(
	ctx context.Context,
	occurrence schedulingapp.Occurrence,
) (schedulingapp.Outcome, error) {
	if s == nil || s.references == nil || s.store == nil || ctx == nil ||
		occurrence.InvocationID == "" ||
		occurrence.JobID != agentMediaOrphanSweepCapability ||
		occurrence.ScheduleRevision != agentMediaOrphanSweepRevision ||
		occurrence.DueAt.IsZero() ||
		occurrence.LeaseEpoch <= 0 ||
		occurrence.ClaimFence == "" {
		return "", errAgentMediaOrphanSweepInvalidOccurrence
	}
	if _, err := s.sweep(ctx); err != nil {
		return "", err
	}
	return schedulingapp.OutcomeLocalCompleted, nil
}

// sweep deletes the unnamed files past the grace period and answers how
// many it deleted.
func (s *agentMediaOrphanSweep) sweep(ctx context.Context) (int, error) {
	// The reference set first and in full: without it nothing is known to
	// be an orphan.
	referenced, err := s.references.Referenced(ctx)
	if err != nil {
		return 0, fmt.Errorf("agent media orphan sweep: read references: %w", err)
	}
	projects, err := s.references.TenantProjects(ctx)
	if err != nil {
		return 0, fmt.Errorf("agent media orphan sweep: list projects: %w", err)
	}
	cutoff := s.now().Add(-agentMediaOrphanGrace)
	deleted := 0
	var failures []error
	for _, project := range projects {
		if deleted >= agentMediaOrphanMaxDeletesPerTick {
			break
		}
		count, err := s.sweepProject(ctx, project, referenced, cutoff, agentMediaOrphanMaxDeletesPerTick-deleted)
		deleted += count
		if err != nil {
			// One project's backend error does not stop the others.
			failures = append(failures, fmt.Errorf("project %d: %w", project, err))
		}
	}
	if deleted > 0 {
		slog.InfoContext(ctx, "agent media orphan sweep: deleted unnamed files", "deleted", deleted)
	}
	return deleted, errors.Join(failures...)
}

func (s *agentMediaOrphanSweep) sweepProject(ctx context.Context, project int64, referenced map[string]struct{}, cutoff time.Time, budget int) (int, error) {
	projectID := strconv.FormatInt(project, 10)
	bucket, err := storage.NewBucketRef(projectID, agentmediaapi.Bucket)
	if err != nil {
		return 0, err
	}
	deleted := 0
	token := ""
	for {
		page, err := s.store.List(ctx, storage.ListQuery{Bucket: bucket, MaxKeys: agentMediaOrphanListPage, ContinuationToken: token})
		if errors.Is(err, storage.ErrNotFound) {
			return deleted, nil // no bucket: nothing was ever stored
		}
		if err != nil {
			return deleted, err
		}
		var orphans []storage.ObjectRef
		for _, object := range page.Objects {
			if len(orphans) >= budget-deleted {
				break
			}
			if object.LastModified.IsZero() || !object.LastModified.Before(cutoff) {
				continue
			}
			if _, named := referenced[repos.AgentMediaReferenceKey(project, object.Key)]; named {
				continue
			}
			ref, err := storage.NewObjectRef(projectID, agentmediaapi.Bucket, object.Key)
			if err != nil {
				continue
			}
			orphans = append(orphans, ref)
		}
		if len(orphans) > 0 {
			result, err := s.store.DeleteBatch(ctx, orphans)
			deleted += len(result.Deleted)
			if err != nil {
				return deleted, err
			}
			for _, failure := range result.Failed {
				slog.WarnContext(ctx, "agent media orphan sweep: delete failed", "project_id", project, "key", failure.Key, "error", failure.Err)
			}
		}
		if deleted >= budget || !page.IsTruncated || page.NextContinuationToken == "" {
			return deleted, nil
		}
		token = page.NextContinuationToken
	}
}

var _ schedulingapp.Handler = (*agentMediaOrphanSweep)(nil)
