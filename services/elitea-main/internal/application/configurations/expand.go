package configurations

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
)

const (
	MaxCurrentExpansionDepth            = 32
	MaxCurrentExpansionNodes            = 16_384
	MaxCurrentExpansionStringBytes      = 4 << 20
	MaxCurrentExpansionIdentifierLength = 1_024
)

var (
	ErrInvalidCurrentExpansion    = errors.New("invalid current configuration expansion")
	ErrCurrentExpansionNotFound   = errors.New("current configuration expansion target not found")
	ErrCurrentExpansionRecursion  = errors.New("current configuration expansion recursion")
	ErrCurrentExpansionForbidden  = errors.New("current configuration expansion forbidden")
	ErrCurrentExpansionDependency = errors.New("current configuration expansion dependency failed")
)

type CurrentExpansionErrorCode string

const (
	CurrentExpansionInvalidCode    CurrentExpansionErrorCode = "invalid"
	CurrentExpansionNotFoundCode   CurrentExpansionErrorCode = "not_found"
	CurrentExpansionRecursionCode  CurrentExpansionErrorCode = "recursion"
	CurrentExpansionForbiddenCode  CurrentExpansionErrorCode = "forbidden"
	CurrentExpansionDependencyCode CurrentExpansionErrorCode = "dependency"
)

// CurrentExpansionError is safe to return across a transport boundary. It
// deliberately excludes configuration titles, project identities, values, and
// dependency error text.
type CurrentExpansionError struct {
	Code CurrentExpansionErrorCode
}

func (e *CurrentExpansionError) Error() string {
	switch e.Code {
	case CurrentExpansionNotFoundCode:
		return ErrCurrentExpansionNotFound.Error()
	case CurrentExpansionRecursionCode:
		return ErrCurrentExpansionRecursion.Error()
	case CurrentExpansionForbiddenCode:
		return ErrCurrentExpansionForbidden.Error()
	case CurrentExpansionDependencyCode:
		return ErrCurrentExpansionDependency.Error()
	default:
		return ErrInvalidCurrentExpansion.Error()
	}
}

func (e *CurrentExpansionError) Unwrap() error {
	switch e.Code {
	case CurrentExpansionNotFoundCode:
		return ErrCurrentExpansionNotFound
	case CurrentExpansionRecursionCode:
		return ErrCurrentExpansionRecursion
	case CurrentExpansionForbiddenCode:
		return ErrCurrentExpansionForbidden
	case CurrentExpansionDependencyCode:
		return ErrCurrentExpansionDependency
	default:
		return ErrInvalidCurrentExpansion
	}
}

// CurrentExpansionScope resolves project identities from already-authorized
// platform state. Implementations must not accept caller-selected schema names.
type CurrentExpansionScope interface {
	PublicProjectID(context.Context) (int32, error)
	PersonalProjectID(context.Context, int32) (int32, error)
}

// CurrentExpansionConfiguration is the minimum current configuration row used
// by expansion. Data may contain any JSON-compatible provider schema.
type CurrentExpansionConfiguration struct {
	UUID      string
	ProjectID int32
	Type      string
	Data      map[string]any
}

// CurrentExpansionFinder performs an exact elitea_title lookup in one
// authorized project. sharedOnly is true only for the public-project fallback.
type CurrentExpansionFinder interface {
	FindByEliteaTitle(
		context.Context,
		int32,
		string,
		bool,
	) (CurrentExpansionConfiguration, bool, error)
}

// CurrentExpansionUnsecreter redeems secret references at claim time using the
// owning configuration project's vault. It must not log or persist the result.
type CurrentExpansionUnsecreter interface {
	Unsecret(context.Context, int32, map[string]any) (map[string]any, error)
}

type CurrentExpansionRequest struct {
	Payload          map[string]any
	CurrentProjectID int32
	UserID           *int32
	Unsecret         bool
}

// CurrentExpansionService ports the current Configurations expansion behavior
// without giving providers, workers, or transports ownership of credentials.
type CurrentExpansionService struct {
	scope      CurrentExpansionScope
	finder     CurrentExpansionFinder
	unsecreter CurrentExpansionUnsecreter
}

func NewCurrentExpansionService(
	scope CurrentExpansionScope,
	finder CurrentExpansionFinder,
	unsecreter CurrentExpansionUnsecreter,
) (*CurrentExpansionService, error) {
	if scope == nil || finder == nil || unsecreter == nil {
		return nil, errors.New("current configuration expansion dependencies are required")
	}
	return &CurrentExpansionService{scope: scope, finder: finder, unsecreter: unsecreter}, nil
}

// Expand returns an owned expanded payload. It never mutates request.Payload or
// returns a partially expanded value when any lookup, policy, or secret
// redemption fails.
func (s *CurrentExpansionService) Expand(
	ctx context.Context,
	request CurrentExpansionRequest,
) (map[string]any, error) {
	if ctx == nil || request.Payload == nil || request.CurrentProjectID <= 0 {
		return nil, newCurrentExpansionError(CurrentExpansionInvalidCode)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// A governed run resolves credentials by exact reference only
	// (GovernedResolver, #1119); the title lookup below never runs for it.
	if IsGovernedRun(ctx) {
		return nil, ErrLegacyResolverInGovernedRun
	}

	walker := currentExpansionWalker{
		service: s,
		request: request,
		seen:    make(map[string]struct{}),
	}
	cloned, err := walker.cloneObject(request.Payload, 0)
	if err != nil {
		return nil, err
	}
	if err := walker.expandObject(ctx, cloned, 0); err != nil {
		return nil, err
	}
	return cloned, nil
}

type currentExpansionWalker struct {
	service *CurrentExpansionService
	request CurrentExpansionRequest
	seen    map[string]struct{}
	budget  currentExpansionBudget

	publicProjectID         int32
	publicProjectIDResolved bool
	personalProjectID       int32
	personalProjectResolved bool
}

func (w *currentExpansionWalker) expandObject(ctx context.Context, payload map[string]any, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > MaxCurrentExpansionDepth {
		return newCurrentExpansionError(CurrentExpansionInvalidCode)
	}

	title, hasTitle, err := currentExpansionTitle(payload)
	if err != nil {
		return err
	}
	if hasTitle {
		if _, repeated := w.seen[title]; repeated {
			return newCurrentExpansionError(CurrentExpansionRecursionCode)
		}
		if err := w.expandReference(ctx, payload, title, depth); err != nil {
			return err
		}
		// Current behavior rejects a title repeated anywhere later in the same
		// expansion, not only an ancestor cycle.
		w.seen[title] = struct{}{}
	}

	keys := make([]string, 0, len(payload))
	for key := range payload {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		child, isObject := payload[key].(map[string]any)
		if !isObject {
			// The current implementation intentionally does not expand objects
			// contained in arrays.
			continue
		}
		if err := w.expandObject(ctx, child, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (w *currentExpansionWalker) expandReference(
	ctx context.Context,
	payload map[string]any,
	title string,
	depth int,
) error {
	private, err := currentExpansionPrivate(payload)
	if err != nil {
		return err
	}
	publicProjectID, err := w.resolvePublicProjectID(ctx)
	if err != nil {
		return err
	}

	projectID := w.request.CurrentProjectID
	if private {
		if w.request.UserID == nil || *w.request.UserID <= 0 {
			return newCurrentExpansionError(CurrentExpansionInvalidCode)
		}
		projectID, err = w.resolvePersonalProjectID(ctx, *w.request.UserID)
		if err != nil {
			return err
		}
	}

	configuration, found, err := w.find(ctx, projectID, title, false)
	if err != nil {
		return err
	}
	if !found && projectID != publicProjectID {
		configuration, found, err = w.find(ctx, publicProjectID, title, true)
		if err != nil {
			return err
		}
	}
	// A row of a retired type (retired.go) is never expanded: it is kept in
	// the table but no runtime path reads it, so a reference to it resolves
	// to nothing, exactly as a missing row does.
	if !found || IsRetiredConfigurationType(configuration.Type) {
		return newCurrentExpansionError(CurrentExpansionNotFoundCode)
	}

	data, err := w.cloneObject(configuration.Data, depth)
	if err != nil {
		return err
	}
	if w.request.Unsecret {
		unsecreted, unsecretErr := w.service.unsecreter.Unsecret(ctx, configuration.ProjectID, data)
		if unsecretErr != nil {
			return currentExpansionDependencyError(ctx, unsecretErr)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err = w.cloneObject(unsecreted, depth)
		if err != nil {
			return err
		}
	}

	if err := w.budget.addMetadata(configuration.UUID, configuration.Type); err != nil {
		return err
	}
	for key, value := range data {
		payload[key] = value
	}
	payload["configuration_uuid"] = configuration.UUID
	payload["configuration_project_id"] = configuration.ProjectID
	payload["configuration_type"] = configuration.Type
	return nil
}

func (w *currentExpansionWalker) find(
	ctx context.Context,
	projectID int32,
	title string,
	sharedOnly bool,
) (CurrentExpansionConfiguration, bool, error) {
	configuration, found, err := w.service.finder.FindByEliteaTitle(ctx, projectID, title, sharedOnly)
	if err != nil {
		return CurrentExpansionConfiguration{}, false, currentExpansionDependencyError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return CurrentExpansionConfiguration{}, false, err
	}
	if !found {
		return CurrentExpansionConfiguration{}, false, nil
	}
	if configuration.ProjectID != projectID ||
		configuration.ProjectID <= 0 ||
		configuration.UUID == "" ||
		len(configuration.UUID) > MaxCurrentExpansionIdentifierLength ||
		configuration.Type == "" ||
		len(configuration.Type) > MaxCurrentExpansionIdentifierLength {
		return CurrentExpansionConfiguration{}, false, newCurrentExpansionError(CurrentExpansionDependencyCode)
	}
	return configuration, true, nil
}

func (w *currentExpansionWalker) resolvePublicProjectID(ctx context.Context) (int32, error) {
	if w.publicProjectIDResolved {
		return w.publicProjectID, nil
	}
	projectID, err := w.service.scope.PublicProjectID(ctx)
	if err != nil {
		return 0, currentExpansionDependencyError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if projectID <= 0 {
		return 0, newCurrentExpansionError(CurrentExpansionDependencyCode)
	}
	w.publicProjectID = projectID
	w.publicProjectIDResolved = true
	return projectID, nil
}

func (w *currentExpansionWalker) resolvePersonalProjectID(ctx context.Context, userID int32) (int32, error) {
	if w.personalProjectResolved {
		return w.personalProjectID, nil
	}
	projectID, err := w.service.scope.PersonalProjectID(ctx, userID)
	if err != nil {
		return 0, currentExpansionDependencyError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if projectID <= 0 {
		return 0, newCurrentExpansionError(CurrentExpansionDependencyCode)
	}
	w.personalProjectID = projectID
	w.personalProjectResolved = true
	return projectID, nil
}

func currentExpansionTitle(payload map[string]any) (string, bool, error) {
	for _, key := range []string{"elitea_title", "alita_title"} {
		value, exists := payload[key]
		if !exists || value == nil {
			continue
		}
		title, ok := value.(string)
		if !ok {
			return "", false, newCurrentExpansionError(CurrentExpansionInvalidCode)
		}
		if title == "" {
			continue
		}
		if len(title) > MaxCurrentExpansionIdentifierLength {
			return "", false, newCurrentExpansionError(CurrentExpansionInvalidCode)
		}
		return title, true, nil
	}
	return "", false, nil
}

func currentExpansionPrivate(payload map[string]any) (bool, error) {
	value, exists := payload["private"]
	if !exists || value == nil {
		return false, nil
	}
	private, ok := value.(bool)
	if !ok {
		return false, newCurrentExpansionError(CurrentExpansionInvalidCode)
	}
	return private, nil
}

func (w *currentExpansionWalker) cloneObject(value map[string]any, depth int) (map[string]any, error) {
	if value == nil {
		return map[string]any{}, nil
	}
	cloned, err := w.cloneJSON(value, depth)
	if err != nil {
		return nil, err
	}
	return cloned.(map[string]any), nil
}

func (w *currentExpansionWalker) cloneJSON(value any, depth int) (any, error) {
	if depth > MaxCurrentExpansionDepth {
		return nil, newCurrentExpansionError(CurrentExpansionInvalidCode)
	}

	switch value := value.(type) {
	case map[string]any:
		if err := w.budget.addContainer(len(value)); err != nil {
			return nil, err
		}
		cloned := make(map[string]any, len(value))
		for key, item := range value {
			if len(key) > MaxCurrentExpansionIdentifierLength {
				return nil, newCurrentExpansionError(CurrentExpansionInvalidCode)
			}
			if err := w.budget.addStringBytes(len(key)); err != nil {
				return nil, err
			}
			clonedItem, err := w.cloneJSON(item, depth+1)
			if err != nil {
				return nil, err
			}
			cloned[key] = clonedItem
		}
		return cloned, nil
	case []any:
		if err := w.budget.addContainer(len(value)); err != nil {
			return nil, err
		}
		cloned := make([]any, len(value))
		for index, item := range value {
			clonedItem, err := w.cloneJSON(item, depth+1)
			if err != nil {
				return nil, err
			}
			cloned[index] = clonedItem
		}
		return cloned, nil
	case string:
		if err := w.budget.addScalar(len(value)); err != nil {
			return nil, err
		}
		return value, nil
	case json.Number:
		if err := w.budget.addScalar(len(value)); err != nil {
			return nil, err
		}
		return value, nil
	case nil, bool,
		float32, float64,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		if err := w.budget.addScalar(0); err != nil {
			return nil, err
		}
		return value, nil
	default:
		return nil, newCurrentExpansionError(CurrentExpansionInvalidCode)
	}
}

type currentExpansionBudget struct {
	nodes       int
	stringBytes int
}

func (b *currentExpansionBudget) addContainer(entries int) error {
	if entries < 0 || entries > MaxCurrentExpansionNodes-b.nodes-1 {
		return newCurrentExpansionError(CurrentExpansionInvalidCode)
	}
	b.nodes++
	return nil
}

func (b *currentExpansionBudget) addScalar(stringBytes int) error {
	if b.nodes >= MaxCurrentExpansionNodes {
		return newCurrentExpansionError(CurrentExpansionInvalidCode)
	}
	b.nodes++
	return b.addStringBytes(stringBytes)
}

func (b *currentExpansionBudget) addStringBytes(count int) error {
	if count < 0 || count > MaxCurrentExpansionStringBytes-b.stringBytes {
		return newCurrentExpansionError(CurrentExpansionInvalidCode)
	}
	b.stringBytes += count
	return nil
}

func (b *currentExpansionBudget) addMetadata(uuid, configurationType string) error {
	if MaxCurrentExpansionNodes-b.nodes < 3 {
		return newCurrentExpansionError(CurrentExpansionInvalidCode)
	}
	b.nodes += 3
	return b.addStringBytes(
		len("configuration_uuid") + len(uuid) +
			len("configuration_project_id") +
			len("configuration_type") + len(configurationType),
	)
}

func newCurrentExpansionError(code CurrentExpansionErrorCode) error {
	return &CurrentExpansionError{Code: code}
}

func currentExpansionDependencyError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return newCurrentExpansionError(CurrentExpansionDependencyCode)
}
