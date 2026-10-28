package agentexecution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/proto"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
)

// GovernedAgentBindings answers the usable agent binding of an application
// version: the same one-active-binding rule the admission transaction binds
// the security context with. None, or more than one, is found=false.
type GovernedAgentBindings interface {
	UsableBindingForVersion(ctx context.Context, tenantProjectID, applicationVersionID int64) (bindingID string, found bool, err error)
}

// GovernedToolkitSource reads a toolkit's STORED row (type and settings, with
// configuration references unexpanded) from the run's project.
type GovernedToolkitSource interface {
	StoredToolkit(ctx context.Context, projectID, toolkitID int64) (toolkitType string, settings map[string]any, found bool, err error)
}

// GovernedCredentialFreeze is what admission resolved for a governed run of
// an application version (#1119). The admission transaction writes it into
// the run's security context as the governed-credential-references entry,
// after it checks the binding it binds is AgentBindingID (empty: none).
type GovernedCredentialFreeze struct {
	AgentBindingID       string
	ApplicationID        int64
	ApplicationVersionID int64
	// References holds one entry per credentialed toolkit, toolkit ids
	// ascending.
	References []GovernedCredentialReference
}

// GovernedCredentialReference is one toolkit's frozen reference.
type GovernedCredentialReference struct {
	ToolkitID             int64
	CredentialReferenceID string
	TargetBindingID       string
	Generation            int64
	ToolkitDigest         [32]byte
}

// GovernedCredentialAdmissionError refuses a governed admission with a safe
// code (configurations.GovernedResolutionCode). It never names a title, a
// project or a credential.
type GovernedCredentialAdmissionError struct {
	Code      string
	ToolkitID int64
	err       error
}

func (e *GovernedCredentialAdmissionError) Error() string {
	return "governed credential admission refused: " + e.Code
}

func (e *GovernedCredentialAdmissionError) Unwrap() error { return e.err }

// GovernedCredentials resolves and freezes the exact credential reference of
// every credentialed toolkit of a bound agent at admission (#1119). It is
// built only with credential governance on, when every fresh admission is a
// governed pin.
type GovernedCredentials struct {
	resolver configurationapp.GovernedResolver
	bindings GovernedAgentBindings
	toolkits GovernedToolkitSource
}

// NewGovernedCredentials builds the admission step.
func NewGovernedCredentials(resolver configurationapp.GovernedResolver, bindings GovernedAgentBindings, toolkits GovernedToolkitSource) (*GovernedCredentials, error) {
	if resolver == nil || bindings == nil || toolkits == nil {
		return nil, errors.New("governed credentials: resolver, binding reader and toolkit source are required")
	}
	return &GovernedCredentials{resolver: resolver, bindings: bindings, toolkits: toolkits}, nil
}

// WithGovernedCredentials makes admission resolve and freeze governed
// credential references (#1119). Without it admission is unchanged.
func (s *AdmissionService) WithGovernedCredentials(credentials *GovernedCredentials) *AdmissionService {
	if s != nil && credentials != nil {
		s.governed = credentials
	}
	return s
}

// governedApplication is the application and version an admission runs; zero
// for an ad-hoc run.
func governedApplication(request SubmitRequest) (int64, int64) {
	switch {
	case request.CurrentTurn != nil:
		return request.CurrentTurn.ApplicationID, request.CurrentTurn.ApplicationVersionID
	case request.CurrentRegenerateTurn != nil:
		return request.CurrentRegenerateTurn.ApplicationID, request.CurrentRegenerateTurn.ApplicationVersionID
	case request.CurrentContinueTurn != nil:
		return request.CurrentContinueTurn.ApplicationID, request.CurrentContinueTurn.ApplicationVersionID
	}
	return 0, 0
}

// Freeze resolves the run's toolkit credentials and returns the input with
// each resolved reference written into its toolkit's frozen settings, top
// level, as `credential_reference_id` (where the worker reads it; the bundle
// digest covers it), and the freeze for the security context.
//
// A run of no application version, or of a version with no usable binding,
// is not a governed agent: nothing is frozen and the input is returned as
// is (the broker denies every governed use of such a run). The freeze of an
// unbound version names no binding, so the admission transaction refuses
// to bind one that appeared in between. For a bound
// agent every top-level toolkit whose stored settings hold a configuration
// reference must resolve: the first failure refuses the admission with a
// typed error. A toolkit with no configuration reference is not credentialed
// and is not frozen. The resolution runs under configurations.WithGovernedRun,
// so the legacy title expansion refuses to run inside it.
func (g *GovernedCredentials) Freeze(ctx context.Context, request SubmitRequest) (*runtimev1.AgentExecutionInputV1, *GovernedCredentialFreeze, error) {
	applicationID, versionID := governedApplication(request)
	if request.CapabilityID != executiondomain.AgentApplicationCapability || versionID <= 0 || request.Input == nil {
		return request.Input, nil, nil
	}
	tenant, err := parsePositive(request.Identity.ResourceProjectID)
	if err != nil {
		return nil, nil, ErrInvalidAgentAdmission
	}
	ctx = configurationapp.WithGovernedRun(ctx)
	bindingID, found, err := g.bindings.UsableBindingForVersion(ctx, tenant, versionID)
	if err != nil {
		return nil, nil, fmt.Errorf("governed credentials: read the agent binding: %w", err)
	}
	freeze := &GovernedCredentialFreeze{ApplicationID: applicationID, ApplicationVersionID: versionID}
	if !found {
		// The admission transaction checks it binds no binding either.
		return request.Input, freeze, nil
	}
	freeze.AgentBindingID = bindingID

	var application map[string]json.RawMessage
	if err := json.Unmarshal(request.Input.GetApplication(), &application); err != nil {
		return nil, nil, fmt.Errorf("%w: the frozen application is not readable", ErrInvalidAgentAdmission)
	}
	var details map[string]json.RawMessage
	if raw, ok := application["version_details"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &details); err != nil {
			return nil, nil, fmt.Errorf("%w: the frozen version is not readable", ErrInvalidAgentAdmission)
		}
	}
	var tools []json.RawMessage
	if raw, ok := details["tools"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &tools); err != nil {
			return nil, nil, fmt.Errorf("%w: the frozen toolkit set is not a list", ErrInvalidAgentAdmission)
		}
	}
	changed := false
	for index, entry := range tools {
		rewritten, ref, err := g.freezeToolkit(ctx, tenant, bindingID, entry)
		if err != nil {
			return nil, nil, err
		}
		if ref == nil {
			continue
		}
		if slices.ContainsFunc(freeze.References, func(r GovernedCredentialReference) bool { return r.ToolkitID == ref.ToolkitID }) {
			// The same toolkit twice resolves the same way; freeze it once.
			if !slices.Contains(freeze.References, *ref) {
				return nil, nil, &GovernedCredentialAdmissionError{Code: "credential_reference_ambiguous", ToolkitID: ref.ToolkitID,
					err: configurationapp.ErrGovernedReferenceAmbiguous}
			}
		} else {
			freeze.References = append(freeze.References, *ref)
		}
		tools[index], changed = rewritten, true
	}
	slices.SortFunc(freeze.References, func(a, b GovernedCredentialReference) int {
		switch {
		case a.ToolkitID < b.ToolkitID:
			return -1
		case a.ToolkitID > b.ToolkitID:
			return 1
		}
		return 0
	})
	if !changed {
		return request.Input, freeze, nil
	}
	if details["tools"], err = json.Marshal(tools); err != nil {
		return nil, nil, err
	}
	if application["version_details"], err = json.Marshal(details); err != nil {
		return nil, nil, err
	}
	encoded, err := json.Marshal(application)
	if err != nil {
		return nil, nil, err
	}
	input := proto.Clone(request.Input).(*runtimev1.AgentExecutionInputV1)
	input.Application = encoded
	return input, freeze, nil
}

// freezeToolkit resolves one frozen toolkit entry. It answers a nil
// reference for an entry that is not a credentialed toolkit.
func (g *GovernedCredentials) freezeToolkit(ctx context.Context, tenant int64, bindingID string, entry json.RawMessage) (json.RawMessage, *GovernedCredentialReference, error) {
	var tool map[string]json.RawMessage
	if json.Unmarshal(entry, &tool) != nil {
		return entry, nil, nil
	}
	toolkitID, ok := positiveJSONInt(tool["id"])
	if !ok {
		return entry, nil, nil
	}
	toolkitType, stored, found, err := g.toolkits.StoredToolkit(ctx, tenant, toolkitID)
	if err != nil {
		return nil, nil, fmt.Errorf("governed credentials: read a stored toolkit: %w", err)
	}
	if !found {
		return entry, nil, nil
	}
	refuse := func(err error) error {
		code, ok := configurationapp.GovernedResolutionCode(err)
		if !ok {
			return fmt.Errorf("governed credentials: resolve a toolkit credential: %w", err)
		}
		return &GovernedCredentialAdmissionError{Code: code, ToolkitID: toolkitID, err: err}
	}
	digest, err := credentialgrant.ToolkitDigest(toolkitType, stored)
	if err != nil {
		return nil, nil, fmt.Errorf("governed credentials: toolkit digest: %w", err)
	}
	refs := configurationapp.ToolkitCredentialRefs(stored)
	switch {
	case len(refs) == 0:
		// No configuration reference: a toolkit the agent binding binds is
		// frozen at its digest alone (the broker resolves its reference from
		// the one usable grant); an unbound one is not governed.
		bound, err := g.resolver.BindToolkit(ctx, tenant, bindingID, toolkitID, digest)
		if err != nil {
			return nil, nil, refuse(err)
		}
		if !bound {
			return entry, nil, nil
		}
		return entry, &GovernedCredentialReference{ToolkitID: toolkitID, ToolkitDigest: digest}, nil
	case len(refs) > 1:
		return nil, nil, refuse(configurationapp.ErrGovernedReferenceAmbiguous)
	}
	resolution, err := g.resolver.Resolve(ctx, tenant, bindingID, toolkitID, digest, refs[0])
	if err != nil {
		return nil, nil, refuse(err)
	}
	var settings map[string]json.RawMessage
	if raw, ok := tool["settings"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &settings); err != nil {
			return nil, nil, fmt.Errorf("%w: a frozen toolkit's settings are not an object", ErrInvalidAgentAdmission)
		}
	}
	if settings == nil {
		settings = map[string]json.RawMessage{}
	}
	if settings[configurationapp.CredentialReferenceIDField], err = json.Marshal(resolution.CredentialReferenceID); err != nil {
		return nil, nil, err
	}
	if tool["settings"], err = json.Marshal(settings); err != nil {
		return nil, nil, err
	}
	rewritten, err := json.Marshal(tool)
	if err != nil {
		return nil, nil, err
	}
	return rewritten, &GovernedCredentialReference{
		ToolkitID: toolkitID, CredentialReferenceID: resolution.CredentialReferenceID,
		TargetBindingID: resolution.TargetBindingID, Generation: resolution.Generation, ToolkitDigest: digest,
	}, nil
}

func parsePositive(value string) (int64, error) {
	id, ok := positiveJSONInt(json.RawMessage(value))
	if !ok {
		return 0, errors.New("not a positive integer")
	}
	return id, nil
}
