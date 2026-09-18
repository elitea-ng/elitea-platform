package indexschedule

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
)

const (
	scheduleCredentialFailureReason = "toolkit credentials resolving issue"
	scheduleSystemPATFailureReason  = "missing valid user token"
)

type ScheduledInputResolver interface {
	ResolveScheduled(
		context.Context,
		indexingapp.StartRequest,
		indexingapp.CurrentToolkitSnapshot,
	) (indexingapp.AuthoritativeInputs, error)
}

type ScheduledIndex struct {
	State         string
	Configuration any
}

// ScheduledIndexInspector performs one exact type=index_meta and collection
// lookup. Implementations must reject duplicate exact rows and must not scan
// unrelated metadata records.
type ScheduledIndexInspector interface {
	InspectScheduledIndex(
		context.Context,
		Candidate,
		int64,
		indexingapp.CurrentToolkitSnapshot,
	) (ScheduledIndex, bool, error)
}

type ScheduledIndexStarter interface {
	StartScheduledIndexData(
		context.Context,
		indexingapp.ScheduledStartRequest,
	) (indexingapp.StartOutcome, error)
}

// ProjectSystemIdentityPreflight verifies that the project already owns a
// usable system "api" PAT. It must not return or retain bearer material.
type ProjectSystemIdentityPreflight interface {
	CheckProjectSystemIdentity(context.Context, int64) error
}

// CurrentExecutor preserves the current index scheduling preparation order:
// load the exact toolkit, apply the schedule credential rule, verify the
// project-system identity, fully unsecret the complete settings as a transient
// preflight, inspect the exact existing index, freeze sealed inputs, then enter
// the same durable admission path as an interactive index start.
type CurrentExecutor struct {
	toolkits       indexingapp.CurrentToolkitReader
	settings       indexingapp.CurrentToolkitSettingsValidator
	systemIdentity ProjectSystemIdentityPreflight
	indexMeta      ScheduledIndexInspector
	inputs         ScheduledInputResolver
	start          ScheduledIndexStarter
}

func NewCurrentExecutor(
	toolkits indexingapp.CurrentToolkitReader,
	settings indexingapp.CurrentToolkitSettingsValidator,
	systemIdentity ProjectSystemIdentityPreflight,
	indexMeta ScheduledIndexInspector,
	inputs ScheduledInputResolver,
	start ScheduledIndexStarter,
) (*CurrentExecutor, error) {
	if toolkits == nil || settings == nil || systemIdentity == nil ||
		indexMeta == nil || inputs == nil || start == nil {
		return nil, errors.New("current index schedule executor dependencies are required")
	}
	return &CurrentExecutor{
		toolkits: toolkits, settings: settings,
		systemIdentity: systemIdentity, indexMeta: indexMeta,
		inputs: inputs, start: start,
	}, nil
}

func (executor *CurrentExecutor) ExecuteScheduled(
	ctx context.Context,
	candidate Candidate,
	_ time.Time,
	idempotencyKey string,
) (ExecutionOutcome, error) {
	if executor == nil || ctx == nil || !validCandidate(candidate) ||
		candidate.ProjectID > math.MaxInt32 ||
		candidate.ToolkitID > math.MaxInt32 ||
		candidate.Schedule.CreatedBy > math.MaxInt32 ||
		candidate.ScheduleUserID > math.MaxInt32 ||
		idempotencyKey == "" ||
		len(idempotencyKey) > 200 {
		return ExecutionOutcome{}, ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return ExecutionOutcome{}, err
	}

	accessUserID := candidate.ScheduleUserID
	if accessUserID == -1 {
		accessUserID = candidate.Schedule.CreatedBy
	}
	toolkit, found, err := executor.toolkits.GetCurrentToolkit(
		ctx,
		int32(candidate.ProjectID),
		int32(accessUserID),
		int32(candidate.ToolkitID),
	)
	if err != nil {
		return ExecutionOutcome{}, dependencyError(ctx, err)
	}
	if !found {
		return ExecutionOutcome{
			Disposition: ExecutionSkippedUnavailable,
		}, nil
	}
	if toolkit.ID != int32(candidate.ToolkitID) ||
		toolkit.Type != candidate.ToolkitType ||
		toolkit.Settings == nil {
		return ExecutionOutcome{}, ErrScheduleDependency
	}

	settings, credentialOK := scheduledToolkitSettings(candidate, toolkit.Settings)
	if !credentialOK {
		return scheduleInitializationFailure(
			scheduleCredentialFailureReason,
		), nil
	}
	if err := executor.systemIdentity.CheckProjectSystemIdentity(
		ctx,
		candidate.ProjectID,
	); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return ExecutionOutcome{}, contextErr
		}
		return scheduleInitializationFailure(
			scheduleSystemPATFailureReason,
		), nil
	}

	// The returned map may contain plaintext and is deliberately discarded.
	// Durable inputs are produced separately in ReferenceMode below.
	preflight, err := executor.settings.Resolve(
		ctx,
		configurationapp.CurrentToolkitSettingsRequest{
			ToolkitType: toolkit.Type,
			Settings:    cloneScheduleSettings(settings),
			ProjectID:   int32(candidate.ProjectID),
			UserID:      int32(accessUserID),
			Mode:        configurationapp.CurrentToolkitSettingsClaimMode,
		},
	)
	if err != nil || preflight == nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return ExecutionOutcome{}, contextErr
		}
		return scheduleInitializationFailure(
			scheduleCredentialFailureReason,
		), nil
	}
	clear(preflight)

	// Use the same saved toolkit snapshot for the exact PgVector lookup and
	// frozen durable inputs. Re-reading the toolkit here could mix a new
	// PgVector target with the earlier credential/settings snapshot.
	toolkit.Settings = settings
	index, found, err := executor.indexMeta.InspectScheduledIndex(
		ctx,
		candidate,
		accessUserID,
		toolkit,
	)
	if err != nil {
		return ExecutionOutcome{}, dependencyError(ctx, err)
	}
	if !found || index.State == "" {
		return ExecutionOutcome{
			Disposition: ExecutionSkippedUnavailable,
		}, nil
	}
	if strings.EqualFold(index.State, "in_progress") {
		return ExecutionOutcome{
			Disposition: ExecutionSkippedActive,
		}, nil
	}
	parameters, err := scheduledIndexParameters(index)
	if err != nil {
		return ExecutionOutcome{}, err
	}

	inputs, err := executor.inputs.ResolveScheduled(
		ctx,
		indexingapp.StartRequest{
			ProjectID:            candidate.ProjectID,
			ActorUserID:          accessUserID,
			ToolkitID:            candidate.ToolkitID,
			ToolParameters:       parameters,
			RequestedLLMSettings: json.RawMessage(`{}`),
		},
		toolkit,
	)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return ExecutionOutcome{}, contextErr
		}
		if errors.Is(
			err,
			indexingapp.ErrCurrentToolkitSettingsResolutionUnavailable,
		) || errors.Is(
			err,
			indexingapp.ErrInvalidAuthoritativeIndexInput,
		) {
			return scheduleInitializationFailure(
				scheduleCredentialFailureReason,
			), nil
		}
		return ExecutionOutcome{}, dependencyError(ctx, err)
	}
	outcome, err := executor.start.StartScheduledIndexData(
		ctx,
		indexingapp.ScheduledStartRequest{
			ProjectID:              candidate.ProjectID,
			AttributionActorUserID: candidate.Schedule.CreatedBy,
			ToolkitID:              candidate.ToolkitID,
			Inputs:                 inputs,
			IdempotencyKey:         idempotencyKey,
			CorrelationID:          idempotencyKey,
		},
	)
	if err != nil {
		var conflict *indexingapp.ActiveIndexConflictError
		if errors.As(err, &conflict) {
			return ExecutionOutcome{
				Disposition: ExecutionSkippedActive,
			}, nil
		}
		return ExecutionOutcome{}, dependencyError(ctx, err)
	}
	if outcome.TaskID == "" {
		return ExecutionOutcome{}, ErrScheduleDependency
	}
	if outcome.Created {
		return ExecutionOutcome{Disposition: ExecutionAdmitted}, nil
	}
	return ExecutionOutcome{Disposition: ExecutionIdempotent}, nil
}

func scheduledToolkitSettings(
	candidate Candidate,
	stored map[string]any,
) (map[string]any, bool) {
	settings := cloneScheduleSettings(stored)
	configurationKey := candidate.ToolkitType + "_configuration"
	_, keyPresent := settings[configurationKey]
	if !keyPresent {
		// Some toolkit types (ado_wiki, ado_repos, artifact, application, …)
		// store their credential under a shared *group* key instead of
		// "{type}_configuration" — the naive derivation above never matches
		// for those, so this settings snapshot has no field this function
		// can override. When the schedule author picked no explicit
		// credential there is genuinely nothing to override (fall through
		// unchanged, as before). But when they DID pick one, silently
		// proceeding would run with the toolkit's own saved configuration
		// instead of the credential they chose — with no failure, no log
		// above DEBUG, and no way to tell the two apart later. Refuse
		// instead of masking that misconfiguration as success (#6527).
		if candidate.Schedule.Credentials != nil {
			return nil, false
		}
		return settings, true
	}
	if candidate.ScheduleUserID == -1 &&
		candidate.Schedule.Credentials == nil {
		return settings, true
	}
	credentials := candidate.Schedule.Credentials
	if credentials == nil ||
		credentials.EliteaTitle == "" {
		return nil, false
	}
	settings[configurationKey] = map[string]any{
		"elitea_title": credentials.EliteaTitle,
		"private":      nullableSchedulePrivate(credentials.Private),
	}
	return settings, true
}

func nullableSchedulePrivate(value *bool) any {
	if value == nil {
		return nil
	}
	return *value
}

func scheduledIndexParameters(index ScheduledIndex) (json.RawMessage, error) {
	if index.Configuration == nil {
		return nil, ErrScheduleDependency
	}
	var encoded []byte
	var err error
	switch configuration := index.Configuration.(type) {
	case string:
		encoded = []byte(configuration)
	case json.RawMessage:
		encoded = bytes.Clone(configuration)
	default:
		encoded, err = json.Marshal(configuration)
	}
	if err != nil ||
		len(encoded) == 0 ||
		len(encoded) > indexingapp.MaxToolParametersBytes ||
		!json.Valid(encoded) {
		return nil, ErrScheduleDependency
	}
	trimmed := bytes.TrimSpace(encoded)
	if len(trimmed) < 2 || trimmed[0] != '{' ||
		trimmed[len(trimmed)-1] != '}' {
		return nil, ErrScheduleDependency
	}
	return encoded, nil
}

func cloneScheduleSettings(value map[string]any) map[string]any {
	cloned := make(map[string]any, len(value))
	for key, item := range value {
		cloned[key] = item
	}
	return cloned
}

func scheduleInitializationFailure(reason string) ExecutionOutcome {
	return ExecutionOutcome{
		Disposition: ExecutionInitializationFailed,
		SafeReason:  reason,
	}
}

var _ Executor = (*CurrentExecutor)(nil)
