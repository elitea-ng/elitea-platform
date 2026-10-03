// Package artifactbootstrap builds a project's system buckets ("reports",
// "tasks") and tears down every bucket of a deleted project — the Go-service
// equivalent of what legacy creates on project creation and removes on
// project deletion. See
// docs/plans/storage-migration-plan.md S13 for why this package is not
// called from anywhere yet.
package artifactbootstrap

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

const (
	bucketReports    = "reports"
	bucketTasks      = "tasks"
	systemBucketType = "system"
)

// Repository is the S6 bucket-metadata dependency this package needs — the
// union of ArtifactBucketsRepository and ArtifactObjectsRepository methods
// actually called here. A struct embedding both (they share no method
// names) satisfies this via promotion, the same pattern
// internal/api/router.go's artifactRepoAdapter uses.
type Repository interface {
	GetBucket(ctx context.Context, projectID int64, name string) (repos.BucketRow, error)
	ListBuckets(ctx context.Context, projectID int64) ([]repos.BucketRow, error)
	CreateBucket(ctx context.Context, input repos.NewBucketInput) (repos.BucketRow, error)
	SoftDeleteBucket(ctx context.Context, id int64) error
	DeleteObjects(ctx context.Context, bucketID int64, keys []string) error
}

// Bootstrapper creates and removes a project's system buckets. See
// BootstrapProjectBuckets/TeardownProjectBuckets for the still-open question
// of what should call it in a running system.
//
// It is a constructor + methods, not the plan's illustrative bare
// package-level functions — matching how every other internal/application/*
// package in this codebase is actually structured (e.g. indexmeta's
// NewDeleteService(...).Delete(...)), not the plan's simplified signature.
type Bootstrapper struct {
	repo  Repository
	store storage.ObjectStore
}

func NewBootstrapper(repo Repository, store storage.ObjectStore) *Bootstrapper {
	return &Bootstrapper{repo: repo, store: store}
}

// BootstrapProjectBuckets creates the project's "reports" and "tasks"
// buckets, both bucket_type=system, idempotently — a second call for the
// same project is a no-op, not an error.
//
// It is not called from anywhere in this service today (S13), and neither
// is TeardownProjectBuckets. There is no project-creation or
// project-deletion path to hook: no CreateProject/DeleteProject handler
// exists, no internal/application/projects-shaped package exists, and the
// projects table itself has no CREATE TABLE under this service — it is
// owned and written by something outside elitea-main entirely. Open
// question for an owner: what should actually call these two methods in a
// running system — a legacy webhook, an event this service should
// subscribe to, a database trigger, or a mechanism not yet designed? Until
// that's answered, every project created after this migration ships has no
// system buckets, same as today.
func (b *Bootstrapper) BootstrapProjectBuckets(ctx context.Context, projectID string) error {
	id, err := parseProjectID(projectID)
	if err != nil {
		return err
	}
	for _, name := range []string{bucketReports, bucketTasks} {
		_, err := b.repo.CreateBucket(ctx, repos.NewBucketInput{
			ProjectID:   id,
			Name:        name,
			DisplayName: name,
			BucketType:  systemBucketType,
		})
		if err != nil && !errors.Is(err, storage.ErrAlreadyExists) {
			return fmt.Errorf("bootstrap %s bucket: %w", name, err)
		}
	}
	return nil
}

// TeardownProjectBuckets soft-deletes EVERY live bucket of the project, the
// system pair and every bucket a user created, and purges every object in
// them: the physical bytes (ObjectStore.DeleteBatch) and their metadata rows
// (Repository.DeleteObjects). It is idempotent: a project with no live bucket
// is a no-op.
//
// It used to tear down "reports" and "tasks" only. A deleted project then
// kept every user bucket row live and every object in it under p/<id>/ (C1).
//
// It tries every bucket even after one fails, so one unreachable object does
// not strand the rest, and returns the failures joined.
func (b *Bootstrapper) TeardownProjectBuckets(ctx context.Context, projectID string) error {
	id, err := parseProjectID(projectID)
	if err != nil {
		return err
	}
	buckets, err := b.repo.ListBuckets(ctx, id)
	if err != nil {
		return fmt.Errorf("list project buckets: %w", err)
	}
	var failures []error
	for _, row := range buckets {
		if err := b.teardownBucket(ctx, projectID, row); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (b *Bootstrapper) teardownBucket(ctx context.Context, projectID string, row repos.BucketRow) error {
	bucketRef, err := storage.NewBucketRef(projectID, row.Name)
	if err != nil {
		return fmt.Errorf("build bucket ref for %s: %w", row.Name, err)
	}
	if err := b.purgeObjects(ctx, bucketRef, row.ID); err != nil {
		return fmt.Errorf("purge objects in %s: %w", row.Name, err)
	}
	if err := b.repo.SoftDeleteBucket(ctx, row.ID); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("delete %s bucket: %w", row.Name, err)
	}
	return nil
}

// purgeObjects removes every physical object under bucketRef and its
// metadata row, paginating List until exhausted. Mirrors
// internal/api/v2/artifacts/handler.go's deleteAllObjects (S8), plus the
// metadata cleanup that function is documented as deliberately deferring
// (see S12's note on the same gap) — closing it here since this is new
// code with no existing behavior to preserve.
func (b *Bootstrapper) purgeObjects(ctx context.Context, bucketRef storage.ObjectRef, bucketID int64) error {
	var token string
	for {
		page, err := b.store.List(ctx, storage.ListQuery{Bucket: bucketRef, ContinuationToken: token})
		if err != nil {
			return fmt.Errorf("list objects: %w", err)
		}

		if len(page.Objects) > 0 {
			refs := make([]storage.ObjectRef, 0, len(page.Objects))
			for _, obj := range page.Objects {
				ref, err := storage.NewObjectRef(bucketRef.ProjectID(), bucketRef.Bucket(), obj.Key)
				if err != nil {
					return fmt.Errorf("build object ref for %q: %w", obj.Key, err)
				}
				refs = append(refs, ref)
			}
			result, batchErr := b.store.DeleteBatch(ctx, refs)

			// Clean up metadata for whatever DeleteBatch actually reports
			// as deleted before handling batchErr/Failed below — a
			// partial failure must not orphan metadata rows for objects
			// whose physical bytes are already gone (that row would
			// count toward the project's quota forever, and a retry can
			// never rediscover a key List no longer returns).
			if len(result.Deleted) > 0 {
				if err := b.repo.DeleteObjects(ctx, bucketID, result.Deleted); err != nil {
					return fmt.Errorf("delete object metadata: %w", err)
				}
			}
			if batchErr != nil {
				return fmt.Errorf("delete batch: %w", batchErr)
			}
			if len(result.Failed) > 0 {
				first := result.Failed[0]
				return fmt.Errorf("delete batch: %d object(s) failed (first: %s: %v)", len(result.Failed), first.Key, first.Err)
			}
		}

		if !page.IsTruncated {
			return nil
		}
		token = page.NextContinuationToken
	}
}

// parseProjectID validates projectID against the same rule storage.NewBucketRef
// enforces (delegated, not duplicated, so the two can never drift apart) and
// returns its int64 form for the repository layer. Without this, a value
// strconv.ParseInt alone would accept — a leading zero, a "+" sign, or a
// 19-digit int64 — lets BootstrapProjectBuckets create real bucket rows
// under a projectID string that TeardownProjectBuckets's storage.NewBucketRef
// call then permanently rejects, since it always uses the original string,
// never a canonicalized one.
func parseProjectID(projectID string) (int64, error) {
	if _, err := storage.NewBucketRef(projectID, bucketReports); err != nil {
		return 0, fmt.Errorf("invalid project id %q: %w", projectID, err)
	}
	id, err := strconv.ParseInt(projectID, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid project id %q: %w", projectID, err)
	}
	return id, nil
}
