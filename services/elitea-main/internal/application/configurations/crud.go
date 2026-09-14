package configurations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	DefaultCurrentConfigurationListLimit = 20
	MaxCurrentConfigurationListLimit     = 200
	MaxCurrentConfigurationFilterValues  = 64
	MaxCurrentConfigurationFilterLength  = 128
	MaxCurrentConfigurationQueryLength   = 1024
)

var (
	ErrInvalidCurrentConfigurationRequest = errors.New("invalid current configuration request")
	ErrCurrentConfigurationNotFound       = errors.New("current configuration not found")
	ErrCurrentConfigurationConflict       = errors.New("current configuration conflicts with an existing row")
)

// CurrentConfiguration is the current p_N.configuration row plus the two
// read-only response enrichments supplied by the social and configuration
// registry integrations. It is a compatibility value, not a new parallel
// configuration domain model.
type CurrentConfiguration struct {
	ID          int32
	UUID        string
	ProjectID   int32
	Label       *string
	EliteaTitle string
	Type        string
	Section     string
	Data        map[string]any
	Meta        map[string]any
	Shared      bool
	StatusOK    bool
	StatusLogs  *string
	Source      string
	AuthorID    *int32
	CreatedAt   time.Time
	UpdatedAt   *time.Time
	IsPinned    bool
	Options     *map[string]any
}

type CurrentConfigurationCreate struct {
	UUID        string
	ProjectID   int32
	Label       *string
	EliteaTitle string
	Type        string
	Section     string
	Data        map[string]any
	Meta        map[string]any
	Shared      bool
	StatusOK    bool
	StatusLogs  *string
	Source      string
	AuthorID    *int32
}

// CurrentConfigurationReplace is a fully resolved replacement for the mutable
// columns of one current row. Partial-update parsing, registry validation,
// secret handling, and lifecycle events deliberately remain outside this
// unmounted row-parity seam.
type CurrentConfigurationReplace struct {
	ProjectID       int32
	ConfigurationID int32
	Label           *string
	EliteaTitle     string
	Data            map[string]any
	Meta            map[string]any
	Shared          bool
	StatusOK        bool
	StatusLogs      *string
}

type CurrentConfigurationListRequest struct {
	IDs             []int32
	ProjectID       int32
	PublicProjectID int32
	Types           []string
	Sections        []string
	Offset          int
	Limit           int
	IncludeShared   bool
	SharedOffset    int
	SharedLimit     int
	Query           string
	SortBy          string
	SortOrder       string
}

// CurrentConfigurationListFilter is a single tenant-schema query. SharedOnly
// means shared=true in the public project. Its false value does not mean
// shared=false: the current project page includes every row owned by that
// project, matching the current Python implementation.
type CurrentConfigurationListFilter struct {
	IDs        []int32
	ProjectID  int32
	Types      []string
	Sections   []string
	Offset     int
	Limit      int
	LabelQuery string
	SortBy     string
	SortOrder  string
	SharedOnly bool
}

type CurrentConfigurationPage struct {
	Items  []CurrentConfiguration
	Total  int64
	Offset int
	Limit  int
}

type CurrentConfigurationListResult struct {
	CurrentConfigurationPage
	Shared *CurrentConfigurationPage
}

// CurrentConfigurationRepository owns one authorized tenant transaction per
// method. ProjectID selects trusted tenant routing as well as the row predicate;
// callers must never derive it from an untrusted schema identifier.
type CurrentConfigurationRepository interface {
	Count(context.Context, CurrentConfigurationListFilter) (int64, error)
	List(context.Context, CurrentConfigurationListFilter) ([]CurrentConfiguration, error)
	Get(context.Context, int32, int32) (CurrentConfiguration, error)
	Create(context.Context, CurrentConfigurationCreate) (CurrentConfiguration, error)
	Replace(context.Context, CurrentConfigurationReplace) (CurrentConfiguration, error)
	Delete(context.Context, int32, int32) error
}

type CurrentCRUDService struct {
	repository CurrentConfigurationRepository
}

func NewCurrentCRUDService(repository CurrentConfigurationRepository) (*CurrentCRUDService, error) {
	if repository == nil {
		return nil, errors.New("current configuration repository is required")
	}
	return &CurrentCRUDService{repository: repository}, nil
}

func (s *CurrentCRUDService) List(ctx context.Context, request CurrentConfigurationListRequest) (CurrentConfigurationListResult, error) {
	if ctx == nil {
		return CurrentConfigurationListResult{}, ErrInvalidCurrentConfigurationRequest
	}
	if err := ctx.Err(); err != nil {
		return CurrentConfigurationListResult{}, err
	}

	request, err := normalizeCurrentConfigurationListRequest(request)
	if err != nil {
		return CurrentConfigurationListResult{}, err
	}

	current, err := s.listPage(ctx, CurrentConfigurationListFilter{
		ProjectID:  request.ProjectID,
		Types:      request.Types,
		IDs:        request.IDs,
		Sections:   request.Sections,
		Offset:     request.Offset,
		Limit:      request.Limit,
		LabelQuery: request.Query,
		SortBy:     request.SortBy,
		SortOrder:  request.SortOrder,
	})
	if err != nil {
		return CurrentConfigurationListResult{}, fmt.Errorf("list project configurations: %w", err)
	}

	result := CurrentConfigurationListResult{CurrentConfigurationPage: current}
	if !request.IncludeShared || request.ProjectID == request.PublicProjectID {
		return result, nil
	}

	shared, err := s.listPage(ctx, CurrentConfigurationListFilter{
		ProjectID:  request.PublicProjectID,
		Types:      request.Types,
		IDs:        request.IDs,
		Sections:   request.Sections,
		Offset:     request.SharedOffset,
		Limit:      request.SharedLimit,
		SortBy:     request.SortBy,
		SortOrder:  request.SortOrder,
		SharedOnly: true,
	})
	if err != nil {
		return CurrentConfigurationListResult{}, fmt.Errorf("list shared configurations: %w", err)
	}
	result.Shared = &shared
	return result, nil
}

func (s *CurrentCRUDService) listPage(ctx context.Context, filter CurrentConfigurationListFilter) (CurrentConfigurationPage, error) {
	total, err := s.repository.Count(ctx, cloneCurrentConfigurationListFilter(filter))
	if err != nil {
		return CurrentConfigurationPage{}, err
	}
	if total < 0 {
		return CurrentConfigurationPage{}, errors.New("current configuration repository returned a negative total")
	}

	// Current behavior resets only an out-of-range non-empty page. For an empty
	// result, the caller-provided offset is echoed unchanged.
	if total > 0 && int64(filter.Offset) >= total {
		filter.Offset = 0
	}
	items, err := s.repository.List(ctx, cloneCurrentConfigurationListFilter(filter))
	if err != nil {
		return CurrentConfigurationPage{}, err
	}
	if items == nil {
		items = []CurrentConfiguration{}
	}
	return CurrentConfigurationPage{
		Items:  items,
		Total:  total,
		Offset: filter.Offset,
		Limit:  filter.Limit,
	}, nil
}

func (s *CurrentCRUDService) Get(ctx context.Context, projectID, configurationID int32) (CurrentConfiguration, error) {
	if err := validateCurrentConfigurationIdentity(ctx, projectID, configurationID); err != nil {
		return CurrentConfiguration{}, err
	}
	configuration, err := s.repository.Get(ctx, projectID, configurationID)
	if err != nil {
		return CurrentConfiguration{}, fmt.Errorf("get current configuration: %w", err)
	}
	return configuration, nil
}

func (s *CurrentCRUDService) Create(ctx context.Context, input CurrentConfigurationCreate) (CurrentConfiguration, error) {
	if ctx == nil || input.ProjectID <= 0 {
		return CurrentConfiguration{}, ErrInvalidCurrentConfigurationRequest
	}
	if err := ctx.Err(); err != nil {
		return CurrentConfiguration{}, err
	}
	configuration, err := s.repository.Create(ctx, cloneCurrentConfigurationCreate(input))
	if err != nil {
		return CurrentConfiguration{}, fmt.Errorf("create current configuration: %w", err)
	}
	return configuration, nil
}

func (s *CurrentCRUDService) Replace(ctx context.Context, input CurrentConfigurationReplace) (CurrentConfiguration, error) {
	if err := validateCurrentConfigurationIdentity(ctx, input.ProjectID, input.ConfigurationID); err != nil {
		return CurrentConfiguration{}, err
	}
	configuration, err := s.repository.Replace(ctx, cloneCurrentConfigurationReplace(input))
	if err != nil {
		return CurrentConfiguration{}, fmt.Errorf("replace current configuration: %w", err)
	}
	return configuration, nil
}

func (s *CurrentCRUDService) Delete(ctx context.Context, projectID, configurationID int32) error {
	if err := validateCurrentConfigurationIdentity(ctx, projectID, configurationID); err != nil {
		return err
	}
	if err := s.repository.Delete(ctx, projectID, configurationID); err != nil {
		return fmt.Errorf("delete current configuration: %w", err)
	}
	return nil
}

func normalizeCurrentConfigurationListRequest(request CurrentConfigurationListRequest) (CurrentConfigurationListRequest, error) {
	if request.ProjectID <= 0 || (request.IncludeShared && request.PublicProjectID <= 0) {
		return CurrentConfigurationListRequest{}, ErrInvalidCurrentConfigurationRequest
	}
	if err := validateCurrentConfigurationFilters(request.Types); err != nil {
		return CurrentConfigurationListRequest{}, err
	}
	if err := validateCurrentConfigurationFilters(request.Sections); err != nil {
		return CurrentConfigurationListRequest{}, err
	}
	if len(request.Query) > MaxCurrentConfigurationQueryLength {
		return CurrentConfigurationListRequest{}, ErrInvalidCurrentConfigurationRequest
	}

	ids, err := NormalizeCurrentConfigurationIDs(request.IDs)
	if err != nil {
		return CurrentConfigurationListRequest{}, err
	}
	request.IDs = ids
	request.Types = append([]string(nil), request.Types...)
	request.Sections = append([]string(nil), request.Sections...)
	request.Offset = normalizeCurrentConfigurationOffset(request.Offset)
	request.Limit = normalizeCurrentConfigurationLimit(request.Limit)
	request.SharedOffset = normalizeCurrentConfigurationOffset(request.SharedOffset)
	request.SharedLimit = normalizeCurrentConfigurationLimit(request.SharedLimit)
	request.SortBy = normalizeCurrentConfigurationSortBy(request.SortBy)
	request.SortOrder = normalizeCurrentConfigurationSortOrder(request.SortOrder)
	return request, nil
}

func validateCurrentConfigurationFilters(values []string) error {
	if len(values) > MaxCurrentConfigurationFilterValues {
		return ErrInvalidCurrentConfigurationRequest
	}
	for _, value := range values {
		if len(value) > MaxCurrentConfigurationFilterLength {
			return ErrInvalidCurrentConfigurationRequest
		}
	}
	return nil
}

func normalizeCurrentConfigurationOffset(offset int) int {
	if offset < 0 {
		return 0
	}
	return offset
}

func normalizeCurrentConfigurationLimit(limit int) int {
	if limit <= 0 {
		return DefaultCurrentConfigurationListLimit
	}
	if limit > MaxCurrentConfigurationListLimit {
		return MaxCurrentConfigurationListLimit
	}
	return limit
}

func normalizeCurrentConfigurationSortBy(sortBy string) string {
	switch sortBy {
	case "id", "uuid", "project_id", "label", "elitea_title", "type", "section", "data", "meta", "shared", "status_ok", "status_logs", "source", "author_id", "created_at", "updated_at":
		return sortBy
	default:
		return "created_at"
	}
}

func normalizeCurrentConfigurationSortOrder(sortOrder string) string {
	if strings.EqualFold(sortOrder, "asc") {
		return "asc"
	}
	return "desc"
}

func validateCurrentConfigurationIdentity(ctx context.Context, projectID, configurationID int32) error {
	if ctx == nil || projectID <= 0 || configurationID <= 0 {
		return ErrInvalidCurrentConfigurationRequest
	}
	return ctx.Err()
}

func cloneCurrentConfigurationListFilter(filter CurrentConfigurationListFilter) CurrentConfigurationListFilter {
	filter.Types = append([]string(nil), filter.Types...)
	filter.Sections = append([]string(nil), filter.Sections...)
	return filter
}

func cloneCurrentConfigurationCreate(input CurrentConfigurationCreate) CurrentConfigurationCreate {
	input.Data = cloneCurrentJSONObject(input.Data)
	input.Meta = cloneCurrentJSONObject(input.Meta)
	return input
}

func cloneCurrentConfigurationReplace(input CurrentConfigurationReplace) CurrentConfigurationReplace {
	input.Data = cloneCurrentJSONObject(input.Data)
	input.Meta = cloneCurrentJSONObject(input.Meta)
	return input
}

func cloneCurrentJSONObject(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	copyValue := make(map[string]any, len(value))
	for key, item := range value {
		copyValue[key] = cloneCurrentJSONValue(item)
	}
	return copyValue
}

func cloneCurrentJSONValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return cloneCurrentJSONObject(value)
	case []any:
		copyValue := make([]any, len(value))
		for index, item := range value {
			copyValue[index] = cloneCurrentJSONValue(item)
		}
		return copyValue
	default:
		return value
	}
}
