package indexregistry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"

	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	indexmetaapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexmeta"
)

// Store is the persistence the Service needs. The Postgres implementation is
// infra/db/repos.IndexRegistryRepository.
type Store interface {
	// List returns the live rows of one toolkit, oldest first.
	List(ctx context.Context, projectID, toolkitID int32) ([]Row, error)
	// FindByName returns the live row of one index name.
	FindByName(ctx context.Context, projectID, toolkitID int32, name string) (Row, bool, error)
	// SaveConfiguration replaces index_configuration and touches nothing else.
	// ErrNotFound when the index has no live row.
	SaveConfiguration(ctx context.Context, projectID, toolkitID int32, name string, configuration []byte) error
	// MarkDeleted tombstones the live row with this index id and returns it.
	// ErrNotFound when there is none; ErrActiveRun when its run is active (its
	// execution job is not terminal).
	MarkDeleted(ctx context.Context, projectID, toolkitID int32, indexID string) (Row, error)
	// PurgeDeleted removes a tombstone and its documents. It is only called
	// once the vector store has deleted the index's points.
	PurgeDeleted(ctx context.Context, indexID string) error
}

// Service is the rust-mode counterpart of indexmeta.Service,
// indexmeta.ExactService, indexmeta.DeleteService and
// indexmeta.ConfigurationService: the same request and response types, backed
// by the registry instead of a pgvector database. The saved toolkit is still
// resolved first, so a caller can only reach the indexes of a toolkit it can
// see; what it no longer needs is the toolkit's pgvector_configuration.
type Service struct {
	toolkits  indexingapp.CurrentToolkitReader
	store     Store
	schedules indexmetaapp.ScheduleCleaner
	vectors   indexingapp.IndexVectorDeleter
	report    func(error)
}

func NewService(
	toolkits indexingapp.CurrentToolkitReader,
	store Store,
	schedules indexmetaapp.ScheduleCleaner,
	vectors indexingapp.IndexVectorDeleter,
	report func(error),
) (*Service, error) {
	if toolkits == nil || store == nil || schedules == nil || vectors == nil || report == nil {
		return nil, errors.New("index registry service dependencies are required")
	}
	return &Service{
		toolkits: toolkits, store: store, schedules: schedules, vectors: vectors, report: report,
	}, nil
}

func validRequest(request indexmetaapp.Request) (projectID, actorID, toolkitID int32, ok bool) {
	if request.ProjectID <= 0 || request.ProjectID > math.MaxInt32 ||
		request.ActorUserID <= 0 || request.ActorUserID > math.MaxInt32 ||
		request.ToolkitID <= 0 || request.ToolkitID > math.MaxInt32 {
		return 0, 0, 0, false
	}
	return int32(request.ProjectID), int32(request.ActorUserID), int32(request.ToolkitID), true
}

// requireToolkit is the visibility check ResolveCurrentTarget makes before it
// looks for a pgvector reference. The registry needs no reference, but a
// toolkit the actor cannot see must still answer exactly as it does today.
func (s *Service) requireToolkit(ctx context.Context, projectID, actorID, toolkitID int32) error {
	toolkit, found, err := s.toolkits.GetCurrentToolkit(ctx, projectID, actorID, toolkitID)
	if err != nil {
		return dependencyError(ctx, indexmetaapp.ErrCurrentIndexMetaUnavailable, err)
	}
	if !found {
		return indexmetaapp.ErrCurrentIndexMetaToolkitMissing
	}
	return checkSnapshot(toolkit, toolkitID)
}

func checkSnapshot(toolkit indexingapp.CurrentToolkitSnapshot, toolkitID int32) error {
	if toolkit.ID != toolkitID || toolkit.ID <= 0 || toolkit.Type == "" ||
		strings.ContainsAny(toolkit.Type, "\x00\r\n") {
		return indexmetaapp.ErrCurrentIndexMetaTargetMissing
	}
	return nil
}

// List is the index list route: a non-nil slice, so an empty toolkit serializes
// as [] and never null.
func (s *Service) List(ctx context.Context, request indexmetaapp.Request) ([]indexmetaapp.Item, error) {
	projectID, actorID, toolkitID, ok := validRequest(request)
	if s == nil || ctx == nil || !ok {
		return nil, indexmetaapp.ErrInvalidCurrentIndexMetaRequest
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.requireToolkit(ctx, projectID, actorID, toolkitID); err != nil {
		return nil, err
	}
	rows, err := s.store.List(ctx, projectID, toolkitID)
	if err != nil {
		return nil, dependencyError(ctx, indexmetaapp.ErrCurrentIndexMetaUnavailable, err)
	}
	if len(rows) > indexmetaapp.MaxCurrentIndexMetaRows {
		return nil, indexmetaapp.ErrCurrentIndexMetaLimitExceeded
	}
	items := make([]indexmetaapp.Item, 0, len(rows))
	for _, row := range rows {
		items = append(items, item(row))
	}
	return items, nil
}

// item projects one row. A run is shown as `in_progress` exactly while its
// execution job is live; an `in_progress` row whose job ended is a dead run and
// is flagged Stale, which is how the list tells the user it stopped reporting.
// Nothing here depends on how long ago the row was last written.
func item(row Row) indexmetaapp.Item {
	return indexmetaapp.Item{ID: row.IndexID, Metadata: Metadata(row), Stale: row.Abandoned()}
}

// Find is the exact (single-index) read the schedule inspector uses.
func (s *Service) Find(ctx context.Context, request indexmetaapp.Request, collection string) (indexmetaapp.Item, bool, error) {
	projectID, actorID, toolkitID, ok := validRequest(request)
	if s == nil || ctx == nil || !ok || !validText(collection, indexmetaapp.MaxCurrentIndexMetaIDBytes) {
		return indexmetaapp.Item{}, false, indexmetaapp.ErrInvalidCurrentIndexMetaRequest
	}
	if err := s.requireToolkit(ctx, projectID, actorID, toolkitID); err != nil {
		return indexmetaapp.Item{}, false, err
	}
	return s.find(ctx, projectID, toolkitID, collection)
}

// FindSnapshot reads from a toolkit snapshot the caller already holds, so the
// scheduler's metadata preflight and its frozen execution inputs stay on one
// coherent view of the saved settings.
func (s *Service) FindSnapshot(
	ctx context.Context,
	request indexmetaapp.Request,
	collection string,
	toolkit indexingapp.CurrentToolkitSnapshot,
) (indexmetaapp.Item, bool, error) {
	projectID, _, toolkitID, ok := validRequest(request)
	if s == nil || ctx == nil || !ok || !validText(collection, indexmetaapp.MaxCurrentIndexMetaIDBytes) {
		return indexmetaapp.Item{}, false, indexmetaapp.ErrInvalidCurrentIndexMetaRequest
	}
	if err := checkSnapshot(toolkit, toolkitID); err != nil {
		return indexmetaapp.Item{}, false, err
	}
	return s.find(ctx, projectID, toolkitID, collection)
}

func (s *Service) find(ctx context.Context, projectID, toolkitID int32, collection string) (indexmetaapp.Item, bool, error) {
	row, found, err := s.store.FindByName(ctx, projectID, toolkitID, collection)
	if err != nil {
		return indexmetaapp.Item{}, false, dependencyError(ctx, indexmetaapp.ErrCurrentIndexMetaUnavailable, err)
	}
	if !found {
		return indexmetaapp.Item{}, false, nil
	}
	return indexmetaapp.Item{ID: row.IndexID, Metadata: Metadata(row)}, true, nil
}

var indexIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Delete removes an index: the registry row is tombstoned (and its name freed),
// the index's vectors are deleted through the vector store hook, and the
// schedule entry in the toolkit row is removed, in that order and as separate
// commits, like the Python path's two.
//
// An index whose run is still active is refused with
// indexing.ErrCurrentIndexMetaConflict: its worker is still writing vectors
// that nothing would then own. A run is active exactly while the execution job
// recorded on the row is not terminal; a run whose job ended or is missing is
// dead, and its index is deletable, which is how a dead run is cleared.
//
// The tombstone is purged only when the hook reports the vectors deleted. When
// that first attempt does not delete them (the hook defers because
// elitea-main has no elitea-vector client yet, or the call fails), the row
// stays as a tombstone and the user-visible delete still succeeds; the
// tombstone sweeper (TombstoneSweeper) repeats the idempotent deletion with a
// backoff until it succeeds.
func (s *Service) Delete(ctx context.Context, request indexmetaapp.DeleteRequest) error {
	if s == nil || ctx == nil || !validText(request.IndexMetaID, indexmetaapp.MaxCurrentIndexMetaIDBytes) ||
		request.ProjectID <= 0 || request.ProjectID > math.MaxInt32 ||
		request.ActorUserID <= 0 || request.ActorUserID > math.MaxInt32 ||
		request.ToolkitID <= 0 || request.ToolkitID > math.MaxInt32 {
		return indexmetaapp.ErrInvalidCurrentIndexMetaRequest
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	projectID, actorID, toolkitID := int32(request.ProjectID), int32(request.ActorUserID), int32(request.ToolkitID)
	if err := s.requireToolkit(ctx, projectID, actorID, toolkitID); err != nil {
		return err
	}
	// An id that is not a registry uuid names no row, exactly as an unknown id
	// does.
	if !indexIDPattern.MatchString(request.IndexMetaID) {
		return indexmetaapp.ErrCurrentIndexMetaNotFound
	}
	row, err := s.store.MarkDeleted(ctx, projectID, toolkitID, request.IndexMetaID)
	switch {
	case errors.Is(err, ErrNotFound):
		return indexmetaapp.ErrCurrentIndexMetaNotFound
	case errors.Is(err, ErrActiveRun):
		return indexingapp.ErrCurrentIndexMetaConflict
	case err != nil:
		return dependencyError(ctx, indexmetaapp.ErrCurrentIndexMetaUnavailable, err)
	}

	switch vectorErr := s.vectors.DeleteIndexVectors(ctx, indexingapp.IndexVectorNamespace{
		ProjectID: projectID, IndexID: row.IndexID,
	}); {
	case vectorErr == nil:
		if err := s.store.PurgeDeleted(ctx, row.IndexID); err != nil {
			// The tombstone stays; the sweeper repeats the (idempotent) deletion
			// of the vectors and the purge.
			s.report(err)
		}
	case errors.Is(vectorErr, indexingapp.ErrIndexVectorDeletionDeferred):
		// Pending, by design, until the vector store client exists; the
		// sweeper skips tombstones while the hook is deferred.
	default:
		s.report(vectorErr)
	}

	if err := s.schedules.DeleteCurrentIndexSchedule(ctx, projectID, toolkitID, row.Name); err != nil {
		if errors.Is(err, indexmetaapp.ErrCurrentIndexScheduleToolkitMissing) {
			return &indexmetaapp.ScheduleToolkitMissingError{ProjectID: projectID, ToolkitID: toolkitID, IndexName: row.Name}
		}
		safe := dependencyError(ctx, indexmetaapp.ErrCurrentIndexScheduleUnavailable, err)
		if !errors.Is(safe, indexmetaapp.ErrCurrentIndexScheduleUnavailable) {
			return safe
		}
		return &indexmetaapp.ScheduleCleanupError{ProjectID: projectID, ToolkitID: toolkitID, IndexName: row.Name}
	}
	return nil
}

// SaveConfiguration replaces one index's configuration without starting a run.
// The state, task id, history and counts belong to the run path and are left
// alone.
func (s *Service) SaveConfiguration(ctx context.Context, request indexmetaapp.ConfigurationRequest) error {
	if s == nil || ctx == nil || !validText(request.IndexName, indexmetaapp.MaxCurrentIndexMetaCollectionBytes) ||
		request.ProjectID <= 0 || request.ProjectID > math.MaxInt32 ||
		request.ActorUserID <= 0 || request.ActorUserID > math.MaxInt32 ||
		request.ToolkitID <= 0 || request.ToolkitID > math.MaxInt32 {
		return indexmetaapp.ErrInvalidCurrentIndexMetaRequest
	}
	if err := validConfiguration(request.Configuration); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	projectID, actorID, toolkitID := int32(request.ProjectID), int32(request.ActorUserID), int32(request.ToolkitID)
	if err := s.requireToolkit(ctx, projectID, actorID, toolkitID); err != nil {
		return err
	}
	err := s.store.SaveConfiguration(ctx, projectID, toolkitID, request.IndexName, request.Configuration)
	switch {
	case errors.Is(err, ErrNotFound):
		return indexmetaapp.ErrCurrentIndexMetaNotFound
	case err != nil:
		return dependencyError(ctx, indexmetaapp.ErrCurrentIndexConfigurationUnavailable, err)
	}
	return nil
}

// validConfiguration accepts exactly one bounded JSON object: the same rule as
// the Python path, so a configuration that could be saved can always be
// replayed by the next reindex.
func validConfiguration(configuration json.RawMessage) error {
	if len(configuration) == 0 || len(configuration) > indexmetaapp.MaxCurrentIndexConfigurationBytes ||
		!json.Valid(configuration) {
		return indexmetaapp.ErrCurrentIndexConfigurationInvalid
	}
	trimmed := bytes.TrimSpace(configuration)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return indexmetaapp.ErrCurrentIndexConfigurationInvalid
	}
	return nil
}

func validText(value string, limit int) bool {
	return value != "" && len(value) <= limit && utf8.ValidString(value) &&
		!strings.ContainsAny(value, "\x00\r\n")
}

func dependencyError(ctx context.Context, safe, cause error) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if errors.Is(cause, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return safe
}
