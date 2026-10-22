package indexmeta

import (
	"context"
	"errors"
	"fmt"
)

const MaxCurrentIndexMetaCollectionBytes = 4 << 10

var (
	ErrCurrentIndexMetaNotFound           = errors.New("current index metadata was not found")
	ErrCurrentIndexScheduleToolkitMissing = errors.New("current index metadata schedule toolkit was not found")
	ErrCurrentIndexScheduleUnavailable    = errors.New("current index metadata schedule cleanup failed")
)

// DeleteRequest contains identities from the already-authenticated and
// project-authorized current REST request. The caller cannot provide a
// storage target.
type DeleteRequest struct {
	ProjectID   int64
	ActorUserID int64
	ToolkitID   int64
	IndexMetaID string
}

// ScheduleCleaner owns the second, project-tenant transaction. It removes the
// complete indexes_meta[indexName] entry, including every user's schedule.
type ScheduleCleaner interface {
	DeleteCurrentIndexSchedule(
		context.Context,
		int32,
		int32,
		string,
	) error
}

// ScheduleToolkitMissingError preserves the current cleanup-race response
// without exposing an internal database error.
type ScheduleToolkitMissingError struct {
	ProjectID int32
	ToolkitID int32
	IndexName string
}

func (e *ScheduleToolkitMissingError) Error() string {
	if e == nil {
		return ErrCurrentIndexScheduleToolkitMissing.Error()
	}
	return fmt.Sprintf(
		"Toolkit %d not found (project_id=%d, index_name='%s')",
		e.ToolkitID,
		e.ProjectID,
		e.IndexName,
	)
}

func (e *ScheduleToolkitMissingError) Unwrap() error {
	return ErrCurrentIndexScheduleToolkitMissing
}

// ScheduleCleanupError intentionally diverges from the legacy Python response,
// which interpolated the raw database exception; this type retains the useful
// request context while returning a stable redacted error.
type ScheduleCleanupError struct {
	ProjectID int32
	ToolkitID int32
	IndexName string
}

func (e *ScheduleCleanupError) Error() string {
	if e == nil {
		return ErrCurrentIndexScheduleUnavailable.Error()
	}
	return fmt.Sprintf(
		"Error during index deletion (Toolkit %dproject_id=%d, index_name='%s') %s",
		e.ToolkitID,
		e.ProjectID,
		e.IndexName,
		ErrCurrentIndexScheduleUnavailable,
	)
}

func (e *ScheduleCleanupError) Unwrap() error {
	return ErrCurrentIndexScheduleUnavailable
}
