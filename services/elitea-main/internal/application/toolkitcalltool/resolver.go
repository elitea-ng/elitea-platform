package toolkitcalltool

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/guardrails"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpoauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/toolkitnaming"
)

const maxToolkitIdentityBytes = 1024

var ErrToolkitSettingsResolutionUnavailable = errors.New(
	"toolkit settings resolution is unavailable",
)

// CurrentToolkitReader loads one saved toolkit row from the already authorized
// resource project. A row belonging to another project must come back
// found=false: this is the ONLY cross-project check on the tool-run path, and
// everything after it treats the row as visible.
//
// It is deliberately the same interface index admission uses, so both paths ask
// the same question of the same repository rather than each deciding
// visibility for itself.
type CurrentToolkitReader = indexingapp.CurrentToolkitReader

// CurrentToolkitSettingsValidator freezes the saved settings in REFERENCE mode:
// configuration titles are resolved to immutable references and the vault is
// never read. The worker redeems them later, under a claimed data-plane grant.
type CurrentToolkitSettingsValidator = indexingapp.CurrentToolkitSettingsValidator

// CurrentAuthoritativeInputResolver reloads the toolkit the caller named and
// freezes its settings. The caller supplies a toolkit ID and tool arguments and
// nothing else; settings and credentials come from the saved row.
type CurrentGuardrailResolver interface {
	ResolveCurrentAgentGuardrails(context.Context) (guardrails.Policy, error)
}

type MCPAuthorizationValidator interface {
	Validate(context.Context, string, mcpoauth.TokenBinding) (mcpoauth.TokenReference, error)
}
type ResolverOption func(*CurrentAuthoritativeInputResolver)

func WithMCPAuthorization(validator MCPAuthorizationValidator) ResolverOption {
	return func(resolver *CurrentAuthoritativeInputResolver) { resolver.tokens = validator }
}

type CurrentAuthoritativeInputResolver struct {
	tokens     MCPAuthorizationValidator
	guardrails CurrentGuardrailResolver
	toolkits   CurrentToolkitReader
	settings   CurrentToolkitSettingsValidator
}

func NewCurrentAuthoritativeInputResolver(
	toolkits CurrentToolkitReader,
	settings CurrentToolkitSettingsValidator,
	guardrails CurrentGuardrailResolver,
	options ...ResolverOption,
) (*CurrentAuthoritativeInputResolver, error) {
	if toolkits == nil || settings == nil || guardrails == nil {
		return nil, errors.New("tool-run input resolver dependencies are required")
	}
	resolver := &CurrentAuthoritativeInputResolver{toolkits: toolkits, settings: settings, guardrails: guardrails}
	for _, option := range options {
		if option != nil {
			option(resolver)
		}
	}
	return resolver, nil
}

func (r *CurrentAuthoritativeInputResolver) Resolve(
	ctx context.Context,
	request RunRequest,
) (AuthoritativeInputs, error) {
	if r == nil || ctx == nil || request.Validate() != nil {
		return AuthoritativeInputs{}, ErrInvalidToolRun
	}
	projectID, actorUserID, toolkitID, ok := resolverDatabaseIDs(request)
	if !ok {
		return AuthoritativeInputs{}, ErrInvalidToolRun
	}
	if err := ctx.Err(); err != nil {
		return AuthoritativeInputs{}, err
	}
	toolkit, found, err := r.toolkits.GetCurrentToolkit(ctx, projectID, actorUserID, toolkitID)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return AuthoritativeInputs{}, contextErr
		}
		return AuthoritativeInputs{}, err
	}
	if !found {
		return AuthoritativeInputs{}, ErrToolkitNotVisible
	}
	if toolkit.ID != toolkitID || toolkit.ID <= 0 || toolkit.Type == "" ||
		len(toolkit.Type) > maxToolkitIdentityBytes ||
		strings.ContainsAny(toolkit.Type, "\x00\r\n") ||
		len(toolkit.Name) > maxToolkitIdentityBytes ||
		strings.ContainsAny(toolkit.Name, "\x00\r\n") ||
		toolkit.Settings == nil {
		return AuthoritativeInputs{}, ErrInvalidAuthoritativeToolRunInput
	}

	expanded, err := r.settings.Resolve(ctx, configurationapp.CurrentToolkitSettingsRequest{
		ToolkitType: toolkit.Type,
		Settings:    cloneObject(toolkit.Settings),
		ProjectID:   projectID,
		UserID:      actorUserID,
		Mode:        configurationapp.CurrentToolkitSettingsReferenceMode,
	})
	if err != nil {
		return AuthoritativeInputs{}, settingsResolutionError(ctx, err)
	}
	if expanded == nil {
		return AuthoritativeInputs{}, ErrInvalidAuthoritativeToolRunInput
	}

	// The SAME `toolkit_config` shape index.ingest.v1 already sends. Both
	// capabilities reach `EliteAClient.test_toolkit_tool`, which reads `id`,
	// `type`, `toolkit_name` and `settings`; a second shape here would be a
	// second answer to what a toolkit configuration is.
	settings, err := json.Marshal(map[string]any{
		"id":           toolkit.ID,
		"type":         toolkit.Type,
		"toolkit_name": toolkitName(toolkit.Name, toolkit.Type),
		"settings":     cloneObject(expanded),
	})
	if err != nil || !validBoundedJSONObject(settings) {
		return AuthoritativeInputs{}, ErrInvalidAuthoritativeToolRunInput
	}
	policy, err := r.guardrails.ResolveCurrentAgentGuardrails(ctx)
	if err != nil {
		return AuthoritativeInputs{}, settingsResolutionError(ctx, err)
	}
	runtimePolicy := policy.Runtime()
	frozenContext := RuntimeContext{ToolkitSecurity: &runtimePolicy, LLMModel: request.LLMModel, LLMConfiguration: append(json.RawMessage(nil), request.LLMSettings...)}
	if request.MCPAuthorizationReference != "" {
		if r.tokens == nil {
			return AuthoritativeInputs{}, ErrToolkitSettingsResolutionUnavailable
		}
		resource, err := mcpoauth.ToolkitResource(toolkit.Type, expanded)
		if err != nil {
			return AuthoritativeInputs{}, ErrInvalidAuthoritativeToolRunInput
		}
		reference, err := r.tokens.Validate(ctx, request.MCPAuthorizationReference, mcpoauth.TokenBinding{ProjectID: projectID, ActorID: actorUserID, ToolkitID: int64(toolkitID), Resource: resource})
		if err != nil {
			return AuthoritativeInputs{}, settingsResolutionError(ctx, err)
		}
		if reference.Reference != request.MCPAuthorizationReference {
			return AuthoritativeInputs{}, ErrInvalidAuthoritativeToolRunInput
		}
		frozenContext.MCPTokenReference = &MCPTokenReference{Reference: reference.Reference, Revision: reference.Revision, ToolkitID: int64(toolkitID), Resource: resource}
	}
	runtimeContext, err := json.Marshal(frozenContext)
	if err != nil || !validRuntimeContext(runtimeContext) {
		return AuthoritativeInputs{}, ErrInvalidAuthoritativeToolRunInput
	}
	return AuthoritativeInputs{
		RuntimeContext: runtimeContext,
		ToolkitType:    toolkit.Type,
		ToolkitID:      int64(toolkit.ID),
		ToolName:       request.ToolName,
		Settings:       settings,
		Arguments:      append(json.RawMessage(nil), request.Arguments...),
	}, nil
}

func resolverDatabaseIDs(request RunRequest) (projectID, actorUserID, toolkitID int32, ok bool) {
	for _, pair := range []struct {
		source int64
		target *int32
	}{
		{request.ProjectID, &projectID},
		{request.ActorUserID, &actorUserID},
		{request.ToolkitID, &toolkitID},
	} {
		if pair.source <= 0 || pair.source > math.MaxInt32 {
			return 0, 0, 0, false
		}
		*pair.target = int32(pair.source)
	}
	return projectID, actorUserID, toolkitID, true
}

// toolkitName is the shared runtime rule — see internal/toolkitnaming. The
// tool-run path and index admission must answer the same identifier, and both
// now read it from one place instead of each holding a copy of the regexp.
func toolkitName(storedName, toolkitType string) string {
	return toolkitnaming.RuntimeName(storedName, toolkitType)
}

func cloneObject(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = cloneValue(value)
	}
	return cloned
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneObject(typed)
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = cloneValue(item)
		}
		return cloned
	default:
		return typed
	}
}

func settingsResolutionError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, configurationapp.ErrInvalidCurrentToolkitSettings) ||
		errors.Is(err, configurationapp.ErrCurrentToolkitSettingsValidation) {
		return ErrInvalidAuthoritativeToolRunInput
	}
	return ErrToolkitSettingsResolutionUnavailable
}

var _ AuthoritativeInputResolver = (*CurrentAuthoritativeInputResolver)(nil)
