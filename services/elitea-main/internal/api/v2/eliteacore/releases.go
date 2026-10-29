package eliteacore

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// ReleaseRecorder records agent releases (ADR-0040 decision 2) inside the
// caller's transaction. Publish records one release for the version it
// created, in the publish transaction; Unpublish withdraws the releases of
// the versions it reverted, in the unpublish transaction. Either both
// commit or neither does, so a publish never answers 200 without its
// release and an unpublish never leaves a reverted version's release
// published. *repos.AgentReleases satisfies it.
type ReleaseRecorder interface {
	RecordPublishedTx(ctx context.Context, tx pgx.Tx, projectID, applicationID, versionID, sourceVersionID int64, name string, actorID int64) (int64, error)
	WithdrawVersionsTx(ctx context.Context, tx pgx.Tx, projectID int64, versionIDs []int64, actorID int64) error
}

// WithReleases wires the release recorder. Unwired, Publish and Unpublish
// record no release.
func WithReleases(recorder ReleaseRecorder) Option {
	return func(handler *Handler) {
		// A typed nil boxed into the interface is absent.
		if recorder == nil {
			return
		}
		if value := reflect.ValueOf(recorder); value.Kind() == reflect.Ptr && value.IsNil() {
			return
		}
		handler.releases = recorder
	}
}

// releaseActorID is the numeric id of the caller, zero when none.
func releaseActorID(r *http.Request) int64 {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		return 0
	}
	id, ok := user.OwningUserID()
	if !ok || id <= 0 {
		return 0
	}
	return id
}

// recordPublishedRelease records the release of the version a publish
// created, in the publish transaction. It answers 0 when the deployment
// records no releases.
func (h *Handler) recordPublishedRelease(ctx context.Context, tx pgx.Tx, r *http.Request, projectID string, applicationID, versionID int, sourceVersionID, name string) (int64, error) {
	if h.releases == nil {
		return 0, nil
	}
	project, err := strconv.ParseInt(projectID, 10, 64)
	if err != nil || project <= 0 {
		return 0, fmt.Errorf("project id %q is not a number", projectID)
	}
	source, _ := strconv.ParseInt(sourceVersionID, 10, 64)
	return h.releases.RecordPublishedTx(ctx, tx, project, int64(applicationID), int64(versionID), source, name, releaseActorID(r))
}

// withdrawReleases withdraws the releases of the versions an unpublish
// reverted, in the unpublish transaction.
func (h *Handler) withdrawReleases(ctx context.Context, tx pgx.Tx, r *http.Request, projectID string, versionIDs []int) error {
	if h.releases == nil || len(versionIDs) == 0 {
		return nil
	}
	project, err := strconv.ParseInt(projectID, 10, 64)
	if err != nil || project <= 0 {
		return fmt.Errorf("project id %q is not a number", projectID)
	}
	ids := make([]int64, 0, len(versionIDs))
	for _, id := range versionIDs {
		ids = append(ids, int64(id))
	}
	return h.releases.WithdrawVersionsTx(ctx, tx, project, ids, releaseActorID(r))
}
