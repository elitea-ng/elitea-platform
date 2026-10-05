package configurations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	MaxCurrentConfigurationTitleLength = 128
	CurrentConfigurationSourceUser     = "user"
)

var (
	ErrInvalidCurrentConfigurationMutation       = errors.New("invalid current configuration mutation")
	ErrUnknownCurrentConfigurationType           = errors.New("unknown current configuration type")
	ErrCurrentConfigurationNormalizationRequired = errors.New("current configuration normalization required")
	ErrImmutableCurrentConfigurationField        = errors.New("immutable current configuration field")
)

type CurrentConfigurationMutationErrorCode string

const (
	CurrentConfigurationMutationInvalid               CurrentConfigurationMutationErrorCode = "invalid"
	CurrentConfigurationMutationUnknownType           CurrentConfigurationMutationErrorCode = "unknown_type"
	CurrentConfigurationMutationNormalizationRequired CurrentConfigurationMutationErrorCode = "normalization_required"
	CurrentConfigurationMutationImmutable             CurrentConfigurationMutationErrorCode = "immutable"
)

// CurrentConfigurationMutationError is safe to return through the API. Field
// and Code are stable routing values; Error deliberately never includes the
// submitted value, schema, secret, or tenant payload.
type CurrentConfigurationMutationError struct {
	Code  CurrentConfigurationMutationErrorCode
	Field string
}

func (e *CurrentConfigurationMutationError) Error() string {
	if e == nil {
		return ErrInvalidCurrentConfigurationMutation.Error()
	}
	if e.Field == "" {
		return "configuration mutation: " + string(e.Code)
	}
	return fmt.Sprintf("configuration field %q: %s", e.Field, e.Code)
}

func (e *CurrentConfigurationMutationError) Unwrap() error {
	if e == nil {
		return ErrInvalidCurrentConfigurationMutation
	}
	switch e.Code {
	case CurrentConfigurationMutationUnknownType:
		return ErrUnknownCurrentConfigurationType
	case CurrentConfigurationMutationNormalizationRequired:
		return ErrCurrentConfigurationNormalizationRequired
	case CurrentConfigurationMutationImmutable:
		return ErrImmutableCurrentConfigurationField
	default:
		return ErrInvalidCurrentConfigurationMutation
	}
}

type CurrentConfigurationCreateRequest struct {
	ProjectID   int32
	AuthorID    int32
	EliteaTitle string
	Label       *string
	Type        string
	Shared      bool
	Data        map[string]any
}

// CurrentConfigurationUpdateRequest preserves PUT field presence explicitly.
// LabelSet permits the current nullable label contract; DataSet and MetaSet
// distinguish an omitted field from an invalid explicit null object.
type CurrentConfigurationUpdateRequest struct {
	ProjectID       int32
	ConfigurationID int32
	AuthorID        int32
	EliteaTitle     *string
	LabelSet        bool
	Label           *string
	DataSet         bool
	Data            map[string]any
	MetaSet         bool
	Meta            map[string]any
	Shared          *bool
}

type CurrentConfigurationDeleteRequest struct {
	ProjectID       int32
	ConfigurationID int32
	AuthorID        int32
}

type CurrentConfigurationNormalizationRequest struct {
	Operation  CurrentConfigurationNormalizationOperation
	ProjectID  int32
	AuthorID   int32
	Type       string
	DataSchema map[string]any
	Data       map[string]any
}

type CurrentConfigurationNormalizationOperation string

const (
	CurrentConfigurationNormalizationCreate CurrentConfigurationNormalizationOperation = "create"
	CurrentConfigurationNormalizationUpdate CurrentConfigurationNormalizationOperation = "update"
)

type CurrentConfigurationNormalizationResult struct {
	Data     map[string]any
	Complete bool
}

// CurrentConfigurationDataNormalizer is the explicit seam for Pydantic model
// validation, defaults, and coercion. ProjectID and AuthorID are trusted
// identities needed by the current SDK validator's configuration-expansion
// rules. Implementations must be bounded and context-aware. Update handling
// must be free of external effects because it runs while the row is locked;
// create handling may perform bounded reads/validation but no durable effects.
type CurrentConfigurationDataNormalizer interface {
	Normalize(context.Context, CurrentConfigurationNormalizationRequest) (CurrentConfigurationNormalizationResult, error)
}

// CurrentPoVDataNormalizer implements the complete current rules for the two
// built-in project settings needed by the first proof-of-value slice. For PUT,
// it also preserves the current shallow numeric coercion contract used for all
// other types. It does not claim create equivalence with arbitrary Pydantic or
// provider validators: those creates return Complete=false and therefore a
// typed normalization-required result from the mutation service.
type CurrentPoVDataNormalizer struct{}

func (CurrentPoVDataNormalizer) Normalize(
	ctx context.Context,
	request CurrentConfigurationNormalizationRequest,
) (CurrentConfigurationNormalizationResult, error) {
	if ctx == nil {
		return CurrentConfigurationNormalizationResult{}, ErrInvalidCurrentConfigurationMutation
	}
	if err := ctx.Err(); err != nil {
		return CurrentConfigurationNormalizationResult{}, err
	}
	switch request.Type {
	case "service_prompt":
		data, err := normalizeCurrentServicePrompt(request.Data, request.DataSchema)
		return CurrentConfigurationNormalizationResult{Data: data, Complete: err == nil}, err
	case "project_context":
		if request.Operation == CurrentConfigurationNormalizationCreate {
			data, err := normalizeCurrentProjectContext(request.Data, request.DataSchema)
			return CurrentConfigurationNormalizationResult{Data: data, Complete: err == nil}, err
		}
		return CurrentConfigurationNormalizationResult{
			Data:     normalizeCurrentShallowUpdateData(request.Data),
			Complete: true,
		}, nil
	default:
		if request.Operation == CurrentConfigurationNormalizationUpdate {
			return CurrentConfigurationNormalizationResult{
				Data:     normalizeCurrentShallowUpdateData(request.Data),
				Complete: true,
			}, nil
		}
		return CurrentConfigurationNormalizationResult{Complete: false}, nil
	}
}

type CurrentConfigurationLifecycleOperation string

const (
	CurrentConfigurationCreated CurrentConfigurationLifecycleOperation = "configuration_created"
	CurrentConfigurationUpdated CurrentConfigurationLifecycleOperation = "configuration_updated"
	CurrentConfigurationDeleted CurrentConfigurationLifecycleOperation = "configuration_deleted"
)

// CurrentConfigurationLifecycleSnapshot contains only the values required by
// lifecycle reconcilers. Data is either absent or already sanitized to hidden-
// secret references; arbitrary meta and status logs are intentionally excluded.
type CurrentConfigurationLifecycleSnapshot struct {
	ID          int32          `json:"id"`
	UUID        string         `json:"uuid"`
	ProjectID   int32          `json:"project_id"`
	EliteaTitle string         `json:"elitea_title"`
	Type        string         `json:"type"`
	Section     string         `json:"section"`
	Label       *string        `json:"label"`
	Shared      bool           `json:"shared"`
	StatusOK    bool           `json:"status_ok"`
	Source      string         `json:"source"`
	AuthorID    *int32         `json:"author_id"`
	Data        map[string]any `json:"data,omitempty"`
}

type CurrentConfigurationLifecycleIntent struct {
	ID        string                                 `json:"id"`
	Operation CurrentConfigurationLifecycleOperation `json:"operation"`
	ActorID   int32                                  `json:"actor_id"`
	Before    *CurrentConfigurationLifecycleSnapshot `json:"before,omitempty"`
	After     *CurrentConfigurationLifecycleSnapshot `json:"after,omitempty"`
}

// CurrentConfigurationMutationStore is scoped to the transaction created by
// CurrentConfigurationMutationRepository. GetForMutation must lock the row;
// hidden-secret changes and lifecycle intents use the same database transaction
// as the row mutation.
type CurrentConfigurationMutationStore interface {
	GetForMutation(context.Context, int32) (CurrentConfiguration, error)
	InsertConfiguration(context.Context, CurrentConfigurationCreate) (CurrentConfiguration, error)
	ReplaceConfiguration(context.Context, CurrentConfigurationReplace) (CurrentConfiguration, error)
	DeleteConfiguration(context.Context, int32) error
	PutHiddenSecrets(context.Context, []HiddenSecretMutation) error
	AppendLifecycleIntent(context.Context, CurrentConfigurationLifecycleIntent) error
}

// CurrentConfigurationMutationRepository owns the atomic row+vault+outbox
// boundary. It must roll back every store operation when operation returns an
// error and commit only after the callback returns nil. The store must not
// escape the callback.
type CurrentConfigurationMutationRepository interface {
	WithinCurrentConfigurationMutation(
		context.Context,
		int32,
		func(CurrentConfigurationMutationStore) error,
	) error
}

type CurrentConfigurationUUIDGenerator func() (string, error)

type CurrentConfigurationMutationService struct {
	repository  CurrentConfigurationMutationRepository
	catalog     *CurrentAvailableCatalog
	normalizer  CurrentConfigurationDataNormalizer
	newUUID     CurrentConfigurationUUIDGenerator
	newSecretID CurrentSecretIDGenerator
}

func NewCurrentConfigurationMutationService(
	repository CurrentConfigurationMutationRepository,
	catalog *CurrentAvailableCatalog,
	normalizer CurrentConfigurationDataNormalizer,
	newUUID CurrentConfigurationUUIDGenerator,
	newSecretID CurrentSecretIDGenerator,
) (*CurrentConfigurationMutationService, error) {
	if repository == nil || catalog == nil || !catalog.Complete() || newUUID == nil || newSecretID == nil {
		return nil, errors.New("current configuration mutation dependencies are required")
	}
	if normalizer == nil {
		normalizer = CurrentPoVDataNormalizer{}
	}
	return &CurrentConfigurationMutationService{
		repository:  repository,
		catalog:     catalog,
		normalizer:  normalizer,
		newUUID:     newUUID,
		newSecretID: newSecretID,
	}, nil
}

func (s *CurrentConfigurationMutationService) Create(
	ctx context.Context,
	request CurrentConfigurationCreateRequest,
) (CurrentConfiguration, error) {
	if err := validateCurrentMutationIdentity(ctx, request.ProjectID, request.AuthorID); err != nil {
		return CurrentConfiguration{}, err
	}
	title, err := normalizeCurrentConfigurationTitle(request.EliteaTitle)
	if err != nil {
		return CurrentConfiguration{}, err
	}
	entry, dataSchema, err := s.currentMutationContract(request.Type)
	if err != nil {
		return CurrentConfiguration{}, err
	}
	normalized, err := s.normalizeData(
		ctx,
		CurrentConfigurationNormalizationCreate,
		request.ProjectID,
		request.AuthorID,
		request.Type,
		dataSchema,
		request.Data,
	)
	if err != nil {
		return CurrentConfiguration{}, err
	}
	title, err = enforceCurrentConfigurationCreateIdentity(request.ProjectID, request.Type, title, normalized)
	if err != nil {
		return CurrentConfiguration{}, err
	}
	sanitized, secretMutations, err := s.extractSecrets(ctx, request.Type, dataSchema, normalized)
	if err != nil {
		return CurrentConfiguration{}, err
	}
	configurationUUID, err := s.allocateUUID("configuration")
	if err != nil {
		return CurrentConfiguration{}, err
	}
	lifecycleID, err := s.allocateUUID("configuration lifecycle intent")
	if err != nil {
		return CurrentConfiguration{}, err
	}

	authorID := request.AuthorID
	input := CurrentConfigurationCreate{
		UUID:        configurationUUID,
		ProjectID:   request.ProjectID,
		Label:       cloneCurrentString(request.Label),
		EliteaTitle: title,
		Type:        request.Type,
		Section:     entry.Section,
		Data:        sanitized,
		Meta:        map[string]any{},
		Shared:      request.Shared,
		StatusOK:    false,
		Source:      CurrentConfigurationSourceUser,
		AuthorID:    &authorID,
	}

	var created CurrentConfiguration
	err = s.repository.WithinCurrentConfigurationMutation(ctx, request.ProjectID, func(store CurrentConfigurationMutationStore) error {
		if store == nil {
			return errors.New("current configuration mutation repository returned a nil store")
		}
		stored, storeErr := store.InsertConfiguration(ctx, cloneCurrentConfigurationCreate(input))
		if storeErr != nil {
			return storeErr
		}
		if !currentCreatedConfigurationMatches(stored, input) {
			return errors.New("current configuration mutation store returned an invalid created row")
		}
		if storeErr = store.PutHiddenSecrets(ctx, cloneCurrentHiddenSecretMutations(secretMutations)); storeErr != nil {
			return storeErr
		}
		intent := CurrentConfigurationLifecycleIntent{
			ID:        lifecycleID,
			Operation: CurrentConfigurationCreated,
			ActorID:   request.AuthorID,
			After:     currentLifecycleSnapshot(stored, sanitized),
		}
		if storeErr = store.AppendLifecycleIntent(ctx, cloneCurrentLifecycleIntent(intent)); storeErr != nil {
			return storeErr
		}
		created = cloneCurrentConfiguration(stored)
		return nil
	})
	if err != nil {
		return CurrentConfiguration{}, fmt.Errorf("create current configuration atomically: %w", err)
	}
	return created, nil
}

func (s *CurrentConfigurationMutationService) Update(
	ctx context.Context,
	request CurrentConfigurationUpdateRequest,
) (CurrentConfiguration, error) {
	if err := validateCurrentMutationRowIdentity(ctx, request.ProjectID, request.ConfigurationID, request.AuthorID); err != nil {
		return CurrentConfiguration{}, err
	}
	if request.DataSet && request.Data == nil {
		return CurrentConfiguration{}, currentMutationFieldError(CurrentConfigurationMutationInvalid, "data")
	}
	if request.MetaSet && request.Meta == nil {
		return CurrentConfiguration{}, currentMutationFieldError(CurrentConfigurationMutationInvalid, "meta")
	}
	lifecycleID, err := s.allocateUUID("configuration lifecycle intent")
	if err != nil {
		return CurrentConfiguration{}, err
	}

	var updated CurrentConfiguration
	err = s.repository.WithinCurrentConfigurationMutation(ctx, request.ProjectID, func(store CurrentConfigurationMutationStore) error {
		if store == nil {
			return errors.New("current configuration mutation repository returned a nil store")
		}
		existing, storeErr := store.GetForMutation(ctx, request.ConfigurationID)
		if storeErr != nil {
			return storeErr
		}
		if !currentConfigurationHasIdentity(existing, request.ProjectID, request.ConfigurationID) {
			return errors.New("current configuration mutation store returned a row with invalid identity")
		}
		_, dataSchema, contractErr := s.currentMutationContract(existing.Type)
		if contractErr != nil {
			return contractErr
		}
		beforeData, sanitizeErr := s.sanitizeLifecycleData(ctx, existing, dataSchema)
		if sanitizeErr != nil {
			return sanitizeErr
		}
		replacement, sanitized, secretMutations, normalizeErr := s.buildReplacement(ctx, existing, request, dataSchema)
		if normalizeErr != nil {
			return normalizeErr
		}
		stored, storeErr := store.ReplaceConfiguration(ctx, cloneCurrentConfigurationReplace(replacement))
		if storeErr != nil {
			return storeErr
		}
		if !currentReplacedConfigurationMatches(stored, existing, replacement) {
			return errors.New("current configuration mutation store returned an invalid updated row")
		}
		if storeErr = store.PutHiddenSecrets(ctx, cloneCurrentHiddenSecretMutations(secretMutations)); storeErr != nil {
			return storeErr
		}
		intent := CurrentConfigurationLifecycleIntent{
			ID:        lifecycleID,
			Operation: CurrentConfigurationUpdated,
			ActorID:   request.AuthorID,
			Before:    currentLifecycleSnapshot(existing, beforeData),
			After:     currentLifecycleSnapshot(stored, sanitized),
		}
		if storeErr = store.AppendLifecycleIntent(ctx, cloneCurrentLifecycleIntent(intent)); storeErr != nil {
			return storeErr
		}
		updated = cloneCurrentConfiguration(stored)
		return nil
	})
	if err != nil {
		return CurrentConfiguration{}, fmt.Errorf("update current configuration atomically: %w", err)
	}
	return updated, nil
}

func (s *CurrentConfigurationMutationService) Delete(
	ctx context.Context,
	request CurrentConfigurationDeleteRequest,
) error {
	if err := validateCurrentMutationRowIdentity(ctx, request.ProjectID, request.ConfigurationID, request.AuthorID); err != nil {
		return err
	}
	lifecycleID, err := s.allocateUUID("configuration lifecycle intent")
	if err != nil {
		return err
	}
	err = s.repository.WithinCurrentConfigurationMutation(ctx, request.ProjectID, func(store CurrentConfigurationMutationStore) error {
		if store == nil {
			return errors.New("current configuration mutation repository returned a nil store")
		}
		existing, storeErr := store.GetForMutation(ctx, request.ConfigurationID)
		if storeErr != nil {
			return storeErr
		}
		if !currentConfigurationHasIdentity(existing, request.ProjectID, request.ConfigurationID) {
			return errors.New("current configuration mutation store returned a row with invalid identity")
		}
		sanitizedData, sanitizeErr := s.sanitizeDeleteLifecycleData(ctx, existing)
		if sanitizeErr != nil {
			return sanitizeErr
		}
		if storeErr = store.DeleteConfiguration(ctx, request.ConfigurationID); storeErr != nil {
			return storeErr
		}
		intent := CurrentConfigurationLifecycleIntent{
			ID:        lifecycleID,
			Operation: CurrentConfigurationDeleted,
			ActorID:   request.AuthorID,
			Before:    currentLifecycleSnapshot(existing, sanitizedData),
		}
		return store.AppendLifecycleIntent(ctx, cloneCurrentLifecycleIntent(intent))
	})
	if err != nil {
		return fmt.Errorf("delete current configuration atomically: %w", err)
	}
	return nil
}

func (s *CurrentConfigurationMutationService) currentMutationContract(
	typeName string,
) (CurrentAvailableConfigurationType, map[string]any, error) {
	entry, ok := s.catalog.EntryByType(typeName)
	if !ok {
		return CurrentAvailableConfigurationType{}, nil, currentMutationFieldError(CurrentConfigurationMutationUnknownType, "type")
	}
	dataSchema, ok := s.catalog.DataSchemaByType(typeName)
	if !ok {
		return CurrentAvailableConfigurationType{}, nil, currentMutationFieldError(CurrentConfigurationMutationNormalizationRequired, "data")
	}
	return entry, dataSchema, nil
}

func (s *CurrentConfigurationMutationService) normalizeData(
	ctx context.Context,
	operation CurrentConfigurationNormalizationOperation,
	projectID int32,
	authorID int32,
	typeName string,
	dataSchema map[string]any,
	data map[string]any,
) (map[string]any, error) {
	if data == nil {
		return nil, currentMutationFieldError(CurrentConfigurationMutationInvalid, "data")
	}
	result, err := s.normalizer.Normalize(ctx, CurrentConfigurationNormalizationRequest{
		Operation:  operation,
		ProjectID:  projectID,
		AuthorID:   authorID,
		Type:       typeName,
		DataSchema: cloneCurrentJSONObject(dataSchema),
		Data:       cloneCurrentJSONObject(data),
	})
	if err != nil {
		return nil, err
	}
	if !result.Complete || result.Data == nil {
		return nil, currentMutationFieldError(CurrentConfigurationMutationNormalizationRequired, "data")
	}
	normalized := cloneCurrentJSONObject(result.Data)
	// The description rule holds for a create AND an update. An update
	// stores the submitted object, so without this a PUT could store a
	// description the create refuses.
	if typeName == "llm_model" {
		if err := NormalizeLLMModelDescription(normalized); err != nil {
			return nil, currentMutationFieldError(CurrentConfigurationMutationInvalid, "data."+LLMModelDescriptionField)
		}
	}
	return normalized, nil
}

func (s *CurrentConfigurationMutationService) extractSecrets(
	ctx context.Context,
	typeName string,
	dataSchema map[string]any,
	data map[string]any,
) (map[string]any, []HiddenSecretMutation, error) {
	properties, ok := dataSchema["properties"].(map[string]any)
	if !ok {
		return nil, nil, currentMutationFieldError(CurrentConfigurationMutationNormalizationRequired, "data")
	}
	sanitized, mutations, err := ExtractCurrentConfigurationSecrets(ctx, data, properties, typeName, s.newSecretID)
	if err != nil {
		return nil, nil, currentMutationSecretError(err)
	}
	return sanitized, mutations, nil
}

func (s *CurrentConfigurationMutationService) buildReplacement(
	ctx context.Context,
	existing CurrentConfiguration,
	request CurrentConfigurationUpdateRequest,
	dataSchema map[string]any,
) (CurrentConfigurationReplace, map[string]any, []HiddenSecretMutation, error) {
	title := existing.EliteaTitle
	if request.EliteaTitle != nil {
		// ConfigurationUpdate has no elitea_title validator in the current
		// public contract. Preserve the submitted string exactly; create-time
		// identifier rules must not be silently added to PUT.
		title = *request.EliteaTitle
	}

	label := cloneCurrentString(existing.Label)
	if request.LabelSet {
		label = cloneCurrentString(request.Label)
	}
	meta := cloneCurrentJSONObject(existing.Meta)
	if request.MetaSet {
		meta = cloneCurrentJSONObject(request.Meta)
	}
	shared := existing.Shared
	if request.Shared != nil {
		shared = *request.Shared
	}

	data := cloneCurrentJSONObject(existing.Data)
	var lifecycleData map[string]any
	var secretMutations []HiddenSecretMutation
	if request.DataSet {
		candidate := cloneCurrentJSONObject(request.Data)
		if existing.Type == "service_prompt" {
			existingKey, err := currentServicePromptKey(existing)
			if err != nil {
				return CurrentConfigurationReplace{}, nil, nil, err
			}
			if _, present := candidate["key"]; !present {
				candidate["key"] = existingKey
			}
		}
		normalized, err := s.normalizeData(
			ctx,
			CurrentConfigurationNormalizationUpdate,
			request.ProjectID,
			request.AuthorID,
			existing.Type,
			dataSchema,
			candidate,
		)
		if err != nil {
			return CurrentConfigurationReplace{}, nil, nil, err
		}
		data, secretMutations, err = s.extractSecrets(ctx, existing.Type, dataSchema, normalized)
		if err != nil {
			return CurrentConfigurationReplace{}, nil, nil, err
		}
		if existing.Type == "service_prompt" {
			existingKey, err := currentServicePromptKey(existing)
			if err != nil {
				return CurrentConfigurationReplace{}, nil, nil, err
			}
			incomingKey, _ := data["key"].(string)
			if incomingKey != existingKey {
				return CurrentConfigurationReplace{}, nil, nil,
					currentMutationFieldError(CurrentConfigurationMutationImmutable, "data.key")
			}
			// Current update_configuration pins the title only while replacing
			// service-prompt data. A title-only PUT remains unchanged parity.
			title = existingKey
		}
		lifecycleData = cloneCurrentJSONObject(data)
	} else {
		// Preserve the current row byte-for-byte when data was omitted. Legacy
		// rows can contain a raw historical password; that must not make an
		// unrelated metadata update impossible and must never enter the outbox.
		var err error
		lifecycleData, err = s.sanitizeLifecycleData(ctx, existing, dataSchema)
		if err != nil {
			return CurrentConfigurationReplace{}, nil, nil, err
		}
	}

	return CurrentConfigurationReplace{
		ProjectID:       request.ProjectID,
		ConfigurationID: request.ConfigurationID,
		Label:           label,
		EliteaTitle:     title,
		Data:            data,
		Meta:            meta,
		Shared:          shared,
		StatusOK:        existing.StatusOK,
		StatusLogs:      cloneCurrentString(existing.StatusLogs),
	}, lifecycleData, secretMutations, nil
}

func (s *CurrentConfigurationMutationService) extractSecretsWithGenerator(
	ctx context.Context,
	typeName string,
	dataSchema map[string]any,
	data map[string]any,
	generator CurrentSecretIDGenerator,
) (map[string]any, []HiddenSecretMutation, error) {
	properties, ok := dataSchema["properties"].(map[string]any)
	if !ok {
		return nil, nil, currentMutationFieldError(CurrentConfigurationMutationNormalizationRequired, "data")
	}
	return ExtractCurrentConfigurationSecrets(ctx, data, properties, typeName, generator)
}

func (s *CurrentConfigurationMutationService) sanitizeDeleteLifecycleData(
	ctx context.Context,
	configuration CurrentConfiguration,
) (map[string]any, error) {
	_, dataSchema, err := s.currentMutationContract(configuration.Type)
	if err != nil {
		// A removed dynamic registry type remains deletable. Its structural
		// identity is sufficient for generic cleanup; data is explicitly absent.
		return nil, nil
	}
	return s.sanitizeLifecycleData(ctx, configuration, dataSchema)
}

func (s *CurrentConfigurationMutationService) sanitizeLifecycleData(
	ctx context.Context,
	configuration CurrentConfiguration,
	dataSchema map[string]any,
) (map[string]any, error) {
	sanitized, _, err := s.extractSecretsWithGenerator(ctx, configuration.Type, dataSchema, configuration.Data, nil)
	if err == nil {
		return sanitized, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	// Historical raw secret material must never enter the outbox. The mutation
	// still proceeds with an explicitly data-less structural snapshot.
	return nil, nil
}

func (s *CurrentConfigurationMutationService) allocateUUID(name string) (string, error) {
	value, err := s.newUUID()
	if err != nil {
		return "", fmt.Errorf("generate %s UUID: %w", name, err)
	}
	if !validCurrentConfigurationUUIDv4(value) {
		return "", fmt.Errorf("generate %s UUID: invalid generator result", name)
	}
	return value, nil
}

func normalizeCurrentServicePrompt(data, schema map[string]any) (map[string]any, error) {
	keyValue, ok := data["key"].(string)
	if !ok {
		return nil, currentMutationFieldError(CurrentConfigurationMutationInvalid, "data.key")
	}
	key, err := normalizeCurrentConfigurationTitle(keyValue)
	if err != nil {
		return nil, currentMutationFieldError(CurrentConfigurationMutationInvalid, "data.key")
	}
	prompt, ok := data["prompt"].(string)
	if !ok || prompt == "" {
		return nil, currentMutationFieldError(CurrentConfigurationMutationInvalid, "data.prompt")
	}
	properties, _ := schema["properties"].(map[string]any)
	keySchema, _ := properties["key"].(map[string]any)
	if !currentSchemaStringEnumContains(keySchema, key) {
		return nil, currentMutationFieldError(CurrentConfigurationMutationInvalid, "data.key")
	}
	return map[string]any{"key": key, "prompt": prompt}, nil
}

func normalizeCurrentProjectContext(data, schema map[string]any) (map[string]any, error) {
	properties, _ := schema["properties"].(map[string]any)
	content := ""
	if rawContent, present := data["content"]; present {
		value, ok := rawContent.(string)
		if !ok {
			return nil, currentMutationFieldError(CurrentConfigurationMutationInvalid, "data.content")
		}
		content = value
	}
	contentSchema, _ := properties["content"].(map[string]any)
	if limit, ok := currentSchemaInteger(contentSchema["maxLength"]); ok && len([]rune(content)) > limit {
		return nil, currentMutationFieldError(CurrentConfigurationMutationInvalid, "data.content")
	}

	enabled := true
	if rawEnabled, present := data["enabled"]; present {
		value, ok := coerceCurrentBoolean(rawEnabled)
		if !ok {
			return nil, currentMutationFieldError(CurrentConfigurationMutationInvalid, "data.enabled")
		}
		enabled = value
	}
	return map[string]any{"content": content, "enabled": enabled}, nil
}

func normalizeCurrentShallowUpdateData(data map[string]any) map[string]any {
	normalized := cloneCurrentJSONObject(data)
	for _, field := range [...]string{"max_output_tokens", "context_window"} {
		value, present := normalized[field]
		if !present {
			continue
		}
		integer, ok := coerceCurrentInteger(value)
		if !ok {
			integer = 0
		}
		normalized[field] = integer
	}
	return normalized
}

func coerceCurrentInteger(value any) (int64, bool) {
	switch value := value.(type) {
	case int:
		return int64(value), true
	case int8:
		return int64(value), true
	case int16:
		return int64(value), true
	case int32:
		return int64(value), true
	case int64:
		return value, true
	case uint:
		return int64(value), uint(int64(value)) == value
	case uint8:
		return int64(value), true
	case uint16:
		return int64(value), true
	case uint32:
		return int64(value), true
	case uint64:
		return int64(value), uint64(int64(value)) == value
	case float32:
		return int64(value), true
	case float64:
		return int64(value), true
	case string:
		integer, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		return integer, err == nil
	case json.Number:
		if integer, err := value.Int64(); err == nil {
			return integer, true
		}
		decimal, err := value.Float64()
		return int64(decimal), err == nil
	case bool:
		if value {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

func enforceCurrentConfigurationCreateIdentity(projectID int32, typeName, title string, data map[string]any) (string, error) {
	switch typeName {
	case "service_prompt":
		key, _ := data["key"].(string)
		return key, nil
	case "project_context":
		return "project_context_" + strconv.FormatInt(int64(projectID), 10), nil
	default:
		return title, nil
	}
}

func currentServicePromptKey(configuration CurrentConfiguration) (string, error) {
	key, _ := configuration.Data["key"].(string)
	if key == "" {
		key = configuration.EliteaTitle
	}
	normalized, err := normalizeCurrentConfigurationTitle(strings.TrimSpace(key))
	if err != nil {
		return "", currentMutationFieldError(CurrentConfigurationMutationInvalid, "data.key")
	}
	return normalized, nil
}

func normalizeCurrentConfigurationTitle(value string) (string, error) {
	if value == "" || len(value) > MaxCurrentConfigurationTitleLength {
		return "", currentMutationFieldError(CurrentConfigurationMutationInvalid, "elitea_title")
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '_' || character == '-' {
			continue
		}
		return "", currentMutationFieldError(CurrentConfigurationMutationInvalid, "elitea_title")
	}
	return strings.ToLower(value), nil
}

func validateCurrentMutationIdentity(ctx context.Context, projectID, authorID int32) error {
	if ctx == nil || projectID <= 0 || authorID <= 0 {
		return currentMutationFieldError(CurrentConfigurationMutationInvalid, "identity")
	}
	return ctx.Err()
}

func validateCurrentMutationRowIdentity(ctx context.Context, projectID, configurationID, authorID int32) error {
	if configurationID <= 0 {
		return currentMutationFieldError(CurrentConfigurationMutationInvalid, "configuration_id")
	}
	return validateCurrentMutationIdentity(ctx, projectID, authorID)
}

func currentMutationFieldError(code CurrentConfigurationMutationErrorCode, field string) error {
	return &CurrentConfigurationMutationError{Code: code, Field: field}
}

func currentMutationSecretError(err error) error {
	var fieldError *CurrentSecretFieldError
	if !errors.As(err, &fieldError) {
		return err
	}
	switch fieldError.reason {
	case "password value must be a string or null", "hidden-secret identifier is duplicated":
		return currentMutationFieldError(CurrentConfigurationMutationInvalid, currentMutationDataField(fieldError.Field))
	default:
		if strings.HasPrefix(fieldError.reason, "is not valid for configuration type") {
			return currentMutationFieldError(CurrentConfigurationMutationInvalid, currentMutationDataField(fieldError.Field))
		}
		return err
	}
}

func currentMutationDataField(field string) string {
	if field == "" || len(field) > MaxCurrentConfigurationTitleLength {
		return "data"
	}
	for _, character := range field {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '_' || character == '-' || character == '.' {
			continue
		}
		return "data"
	}
	return "data." + field
}

func currentSchemaStringEnumContains(schema map[string]any, value string) bool {
	values, ok := schema["enum"].([]any)
	if !ok || len(values) == 0 {
		return false
	}
	for _, candidate := range values {
		if candidate, ok := candidate.(string); ok && candidate == value {
			return true
		}
	}
	return false
}

func currentSchemaInteger(value any) (int, bool) {
	switch value := value.(type) {
	case int:
		return value, value >= 0
	case float64:
		return int(value), value >= 0 && value == float64(int(value))
	case interface{ Int64() (int64, error) }:
		integer, err := value.Int64()
		return int(integer), err == nil && integer >= 0 && int64(int(integer)) == integer
	default:
		return 0, false
	}
}

func coerceCurrentBoolean(value any) (bool, bool) {
	switch value := value.(type) {
	case bool:
		return value, true
	case string:
		switch strings.ToLower(value) {
		case "true", "1", "yes", "on":
			return true, true
		case "false", "0", "no", "off":
			return false, true
		}
	case float64:
		if value == 0 || value == 1 {
			return value == 1, true
		}
	case int:
		if value == 0 || value == 1 {
			return value == 1, true
		}
	}
	return false, false
}

func validCurrentConfigurationUUIDv4(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' || value[14] != '4' {
		return false
	}
	if value[19] != '8' && value[19] != '9' && value[19] != 'a' && value[19] != 'b' {
		return false
	}
	for index := 0; index < len(value); index++ {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		character := value[index]
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

func currentConfigurationHasIdentity(configuration CurrentConfiguration, projectID, configurationID int32) bool {
	return configuration.ID == configurationID && configuration.ProjectID == projectID && configuration.UUID != ""
}

func currentCreatedConfigurationMatches(configuration CurrentConfiguration, input CurrentConfigurationCreate) bool {
	return configuration.ID > 0 && configuration.UUID == input.UUID && configuration.ProjectID == input.ProjectID &&
		configuration.EliteaTitle == input.EliteaTitle && configuration.Type == input.Type && configuration.Section == input.Section &&
		configuration.Source == input.Source
}

func currentReplacedConfigurationMatches(
	configuration, existing CurrentConfiguration,
	input CurrentConfigurationReplace,
) bool {
	return configuration.ID == existing.ID && configuration.UUID == existing.UUID && configuration.ProjectID == input.ProjectID &&
		configuration.Type == existing.Type && configuration.Section == existing.Section && configuration.Source == existing.Source &&
		configuration.EliteaTitle == input.EliteaTitle
}

func currentLifecycleSnapshot(
	configuration CurrentConfiguration,
	sanitizedData map[string]any,
) *CurrentConfigurationLifecycleSnapshot {
	return &CurrentConfigurationLifecycleSnapshot{
		ID:          configuration.ID,
		UUID:        configuration.UUID,
		ProjectID:   configuration.ProjectID,
		EliteaTitle: configuration.EliteaTitle,
		Type:        configuration.Type,
		Section:     configuration.Section,
		Label:       cloneCurrentString(configuration.Label),
		Shared:      configuration.Shared,
		StatusOK:    configuration.StatusOK,
		Source:      configuration.Source,
		AuthorID:    cloneCurrentInt32(configuration.AuthorID),
		Data:        cloneCurrentJSONObject(sanitizedData),
	}
}

func cloneCurrentConfiguration(configuration CurrentConfiguration) CurrentConfiguration {
	configuration.Label = cloneCurrentString(configuration.Label)
	configuration.Data = cloneCurrentJSONObject(configuration.Data)
	configuration.Meta = cloneCurrentJSONObject(configuration.Meta)
	configuration.StatusLogs = cloneCurrentString(configuration.StatusLogs)
	configuration.AuthorID = cloneCurrentInt32(configuration.AuthorID)
	if configuration.Options != nil {
		options := cloneCurrentJSONObject(*configuration.Options)
		configuration.Options = &options
	}
	return configuration
}

func cloneCurrentHiddenSecretMutations(mutations []HiddenSecretMutation) []HiddenSecretMutation {
	cloned := make([]HiddenSecretMutation, len(mutations))
	for index, mutation := range mutations {
		cloned[index] = mutation
		cloned[index].Path = append([]string(nil), mutation.Path...)
	}
	return cloned
}

func cloneCurrentLifecycleIntent(intent CurrentConfigurationLifecycleIntent) CurrentConfigurationLifecycleIntent {
	intent.Before = cloneCurrentLifecycleSnapshot(intent.Before)
	intent.After = cloneCurrentLifecycleSnapshot(intent.After)
	return intent
}

func cloneCurrentLifecycleSnapshot(snapshot *CurrentConfigurationLifecycleSnapshot) *CurrentConfigurationLifecycleSnapshot {
	if snapshot == nil {
		return nil
	}
	cloned := *snapshot
	cloned.Label = cloneCurrentString(snapshot.Label)
	cloned.AuthorID = cloneCurrentInt32(snapshot.AuthorID)
	cloned.Data = cloneCurrentJSONObject(snapshot.Data)
	return &cloned
}

func cloneCurrentString(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneCurrentInt32(value *int32) *int32 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
