package eliteacore

import (
	"context"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// ReleaseRecorder records agent releases (ADR-0040 decision 2). Publish
// records one release for the version it created; Unpublish withdraws the
// releases of the versions it reverted. *repos.AgentReleases satisfies it
// through repos.PublishReleaseRecorder.
type ReleaseRecorder interface {
	RecordPublished(ctx context.Context, projectID, applicationID, versionID, sourceVersionID int64, name string, actorID int64) (int64, error)
	WithdrawVersions(ctx context.Context, projectID int64, versionIDs []int64, actorID int64) error
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
// created. The publish has committed, so a failure here does not undo it:
// it is logged, and the response carries no release id.
func (h *Handler) recordPublishedRelease(r *http.Request, projectID string, applicationID, versionID int, sourceVersionID, name string) (int64, bool) {
	if h.releases == nil {
		return 0, false
	}
	project, err := strconv.ParseInt(projectID, 10, 64)
	if err != nil || project <= 0 {
		return 0, false
	}
	source, _ := strconv.ParseInt(sourceVersionID, 10, 64)
	id, err := h.releases.RecordPublished(r.Context(), project, int64(applicationID), int64(versionID), source, name, releaseActorID(r))
	if err != nil {
		slog.ErrorContext(r.Context(), "publish: record the agent release",
			"project_id", projectID, "application_id", applicationID, "version_id", versionID, "error", err)
		return 0, false
	}
	return id, true
}

// withdrawReleases withdraws the releases of the versions an unpublish
// reverted.
func (h *Handler) withdrawReleases(r *http.Request, projectID string, versionIDs []int) error {
	if h.releases == nil || len(versionIDs) == 0 {
		return nil
	}
	project, err := strconv.ParseInt(projectID, 10, 64)
	if err != nil || project <= 0 {
		return nil
	}
	ids := make([]int64, 0, len(versionIDs))
	for _, id := range versionIDs {
		ids = append(ids, int64(id))
	}
	return h.releases.WithdrawVersions(r.Context(), project, ids, releaseActorID(r))
}
