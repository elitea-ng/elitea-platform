package agentexecution

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/proto"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
)

// GovernedAgentBindings answers the usable agent binding of an application
// version: the same one-active-binding rule the admission transaction binds
// the security context with. None, or more than one, is found=false.
type GovernedAgentBindings interface {
	UsableBindingForVersion(ctx context.Context, tenantProjectID, applicationVersionID int64) (bindingID string, definitionDigest [32]byte, found bool, err error)
}

// GovernedDefinitions computes the canonical definition digest of an
// application version (agentidentity.VersionDefinitionDigest over its
// stored definition, #1462) and records an admission that bound no agent
// identity because a binding pins another digest. found is false when the
// tenant project holds no such version.
type GovernedDefinitions interface {
	DefinitionDigest(ctx context.Context, tenantProjectID, applicationID, applicationVersionID int64) (digest [32]byte, found bool, err error)
	RecordDefinitionMismatch(ctx context.Context, tenantProjectID int64, bindingID string, applicationID, applicationVersionID int64) error
}

// GovernedToolkitSource reads a toolkit's STORED row (type and settings, with
// configuration references unexpanded) from the run's project.
type GovernedToolkitSource interface {
	StoredToolkit(ctx context.Context, projectID, toolkitID int64) (toolkitType string, settings map[string]any, found bool, err error)
}

// AgentAdmissionReplays reads a committed admission by its idempotency
// scope and key, with the request digest it was admitted under.
// found=false when there is none.
type AgentAdmissionReplays interface {
	AgentAdmissionReplay(ctx context.Context, scope, key string) (outcome executionapp.AdmissionOutcome, digest runtimedomain.Digest, found bool, err error)
}

// GovernedCredentialFreeze is what admission resolved for a governed run of
// an application version (#1119). The admission transaction writes it into
// the run's security context as the governed-credential-references entry,
// after it checks the binding it binds is AgentBindingID (empty: none).
type GovernedCredentialFreeze struct {
	AgentBindingID       string
	ApplicationID        int64
	ApplicationVersionID int64
	// DefinitionChecked is true when Freeze computed DefinitionDigest, the
	// canonical digest of the version the run executes (#1462; zero when
	// the version could not be read). The admission transaction then binds
	// only a binding that pins exactly this digest.
	DefinitionChecked bool
	DefinitionDigest  [32]byte
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
	resolver    configurationapp.GovernedResolver
	bindings    GovernedAgentBindings
	toolkits    GovernedToolkitSource
	replays     AgentAdmissionReplays
	definitions GovernedDefinitions
}

// NewGovernedCredentials builds the admission step.
func NewGovernedCredentials(resolver configurationapp.GovernedResolver, bindings GovernedAgentBindings, toolkits GovernedToolkitSource, replays AgentAdmissionReplays, definitions GovernedDefinitions) (*GovernedCredentials, error) {
	if resolver == nil || bindings == nil || toolkits == nil || replays == nil || definitions == nil {
		return nil, errors.New("governed credentials: resolver, binding reader, toolkit source, replay reader and definition digests are required")
	}
	return &GovernedCredentials{resolver: resolver, bindings: bindings, toolkits: toolkits, replays: replays, definitions: definitions}, nil
}

// replay answers the committed admission a retry names (found=true), so a
// retry is never re-resolved: the freeze is the original admission's. A
// different request under the same key is an idempotency conflict.
func (g *GovernedCredentials) replay(ctx context.Context, scope, key string, requestDigest runtimedomain.Digest) (executionapp.AdmissionOutcome, bool, error) {
	outcome, digest, found, err := g.replays.AgentAdmissionReplay(ctx, scope, key)
	switch {
	case err != nil:
		return executionapp.AdmissionOutcome{}, false, fmt.Errorf("governed credentials: read the idempotent admission: %w", err)
	case !found:
		return executionapp.AdmissionOutcome{}, false, nil
	case digest != requestDigest:
		return executionapp.AdmissionOutcome{}, false, executionapp.ErrIdempotencyConflict
	}
	outcome.Created = false
	return outcome, true, nil
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
// to bind one that appeared in between. A binding whose definition digest
// is not the canonical digest of the version (#1462) is treated as no
// binding: the mismatch is audited and the run gets no agent identity, so
// it is denied, not refused. For a bound
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
	// A release run binds the binding of the release's own version row
	// (release.go); the admission transaction reads the same one.
	versionID = bindingVersionID(request.Release, versionID)
	bindingID, boundDigest, found, err := g.bindings.UsableBindingForVersion(ctx, tenant, versionID)
	if err != nil {
		return nil, nil, fmt.Errorf("governed credentials: read the agent binding: %w", err)
	}
	freeze := &GovernedCredentialFreeze{ApplicationID: applicationID, ApplicationVersionID: versionID}
	var application map[string]json.RawMessage
	_, mandated := securitycontext.RunAuthorityFrom(ctx)
	if found || mandated {
		if err := json.Unmarshal(request.Input.GetApplication(), &application); err != nil {
			return nil, nil, fmt.Errorf("%w: the frozen application is not readable", ErrInvalidAgentAdmission)
		}
		// The canonical digest of the version the run executes (#1462). An
		// unattended run's mandate names its binding, which the admission
		// transaction compares with this digest as well.
		if request.Release != nil {
			// A release run (ADR-0040 decision 2) executes the release's
			// definition, which the start resolver read from the immutable
			// release and checked against its toolkit pins. Its digest is
			// the release's, whatever the version row holds now.
			freeze.DefinitionChecked = true
			freeze.DefinitionDigest = request.Release.DefinitionDigest
		} else if err := g.checkDefinition(ctx, tenant, application, freeze); err != nil {
			return nil, nil, err
		}
	}
	if !found {
		// The admission transaction checks it binds no binding either.
		return request.Input, freeze, nil
	}
	if freeze.DefinitionDigest != boundDigest {
		// The binding pins another definition than the one this run
		// executes: bind no agent identity, so every governed credential
		// use of the run is denied. An operator re-pins the binding to the
		// computed digest the admin API shows.
		if err := g.definitions.RecordDefinitionMismatch(ctx, tenant, bindingID, applicationID, versionID); err != nil {
			return nil, nil, fmt.Errorf("governed credentials: audit the definition digest mismatch: %w", err)
		}
		return request.Input, freeze, nil
	}
	freeze.AgentBindingID = bindingID
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
		// A toolkit frozen at its digest alone is not rewritten.
		rewrote := ref.CredentialReferenceID != ""
		if slices.ContainsFunc(freeze.References, func(r GovernedCredentialReference) bool { return r.ToolkitID == ref.ToolkitID }) {
			// The same toolkit twice resolves the same way; freeze it once.
			if !slices.Contains(freeze.References, *ref) {
				return nil, nil, &GovernedCredentialAdmissionError{Code: "credential_reference_ambiguous", ToolkitID: ref.ToolkitID,
					err: configurationapp.ErrGovernedReferenceAmbiguous}
			}
		} else {
			freeze.References = append(freeze.References, *ref)
		}
		if rewrote {
			tools[index], changed = rewritten, true
		}
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

// checkDefinition computes the canonical digest of the run's version into
// freeze. A version the tenant project does not hold digests to zero, which
// no binding pins. When the run carries a captured root source (its exact
// admitted bytes), that source must have the same digest: a version edited
// between the start and this read, or a continuation of an admission whose
// definition was since edited, then digests to zero as well.
func (g *GovernedCredentials) checkDefinition(ctx context.Context, tenant int64, application map[string]json.RawMessage, freeze *GovernedCredentialFreeze) error {
	digest, found, err := g.definitions.DefinitionDigest(ctx, tenant, freeze.ApplicationID, freeze.ApplicationVersionID)
	if err != nil {
		return fmt.Errorf("governed credentials: compute the definition digest: %w", err)
	}
	freeze.DefinitionChecked = true
	if !found {
		return nil
	}
	if raw, ok := application["source_definition"]; ok && string(raw) != "null" {
		var source struct {
			SourceDefinitionSHA256 string `json:"source_definition_sha256"`
		}
		if json.Unmarshal(raw, &source) != nil || source.SourceDefinitionSHA256 != hex.EncodeToString(digest[:]) {
			return nil
		}
	}
	freeze.DefinitionDigest = digest
	return nil
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
