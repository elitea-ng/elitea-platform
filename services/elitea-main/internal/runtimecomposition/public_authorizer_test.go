package runtimecomposition

import (
	"context"
	"errors"
	"testing"

	configurationapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	executionapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/executions"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

type authorizationRow struct {
	active       bool
	capabilityID string
	err          error
}

type authorizationStore struct {
	row             authorizationRow
	admissionParams sqlcgen.AuthorizeRuntimeValidationProjectParams
	outputParams    sqlcgen.ResolveRuntimeExecutionEventCapabilityParams
	calls           int
}

func (s *authorizationStore) AuthorizeRuntimeValidationProject(
	_ context.Context,
	params sqlcgen.AuthorizeRuntimeValidationProjectParams,
) (bool, error) {
	s.calls++
	s.admissionParams = params
	return s.row.active, s.row.err
}

func (s *authorizationStore) ResolveRuntimeExecutionEventCapability(
	_ context.Context,
	params sqlcgen.ResolveRuntimeExecutionEventCapabilityParams,
) (string, error) {
	s.calls++
	s.outputParams = params
	return s.row.capabilityID, s.row.err
}

// allowObservers admits every observer, so the tests that pin the permission
// branches are not also about execution ownership.
type allowObservers struct{}

func (allowObservers) MayObserveAgentExecution(context.Context, int64, string, auth.User) (bool, error) {
	return true, nil
}

// recordingObservers answers a fixed verdict and records what it was asked.
type recordingObservers struct {
	allowed     bool
	err         error
	calls       int
	project     int64
	executionID string
	principal   auth.User
}

func (o *recordingObservers) MayObserveAgentExecution(_ context.Context, project int64, executionID string, principal auth.User) (bool, error) {
	o.calls++
	o.project, o.executionID, o.principal = project, executionID, principal
	return o.allowed, o.err
}

type authorizationPermissionResolver struct {
	resolution auth.PermissionResolution
	err        error
	calls      int
	principal  auth.User
	mode       string
	projectID  string
}

func (r *authorizationPermissionResolver) ResolvePermissions(
	_ context.Context,
	principal auth.User,
	mode string,
	projectID string,
) (auth.PermissionResolution, error) {
	r.calls++
	r.principal = principal
	r.mode = mode
	r.projectID = projectID
	return r.resolution, r.err
}

func indexEventPermissions() *authorizationPermissionResolver {
	return &authorizationPermissionResolver{
		resolution: auth.PermissionResolution{
			UserID:      17,
			Permissions: []string{"models.applications.tool.patch"},
		},
	}
}

func TestPublicAuthorizerDerivesPhaseOneIdentityFromVerifiedMembership(t *testing.T) {
	store := &authorizationStore{row: authorizationRow{active: true}}
	authorizer, err := newPostgresPublicAuthorizer(
		store,
		&authorizationStore{row: authorizationRow{capabilityID: "index.ingest.v1"}},
		indexEventPermissions(),
		allowObservers{},
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := auth.ContextWithAuthenticatedUser(context.Background(), auth.User{ID: "17"}, auth.AuthenticationSourceToken)
	identity, err := authorizer.AuthorizeValidation(ctx, "42", "revision-1")
	if err != nil {
		t.Fatal(err)
	}
	if identity.TenantID != "42" || identity.ResourceProjectID != "42" || identity.ProjectionProjectID != "42" || identity.ActorID != "17" {
		t.Fatalf("unexpected admission identity: %+v", identity)
	}
	if store.calls != 1 || store.admissionParams.ProjectID != 42 || store.admissionParams.UserID != 17 {
		t.Fatalf("authorization query params = %+v calls=%d", store.admissionParams, store.calls)
	}
}

func TestPublicAuthorizerAcceptsVerifiedForwardedMembership(t *testing.T) {
	store := &authorizationStore{row: authorizationRow{active: true}}
	authorizer, err := newPostgresPublicAuthorizer(
		store,
		&authorizationStore{row: authorizationRow{capabilityID: "index.ingest.v1"}},
		indexEventPermissions(),
		allowObservers{},
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := auth.ContextWithAuthenticatedUser(context.Background(), auth.User{ID: "17"}, auth.AuthenticationSourceForwarded)
	identity, err := authorizer.AuthorizeValidation(ctx, "42", "revision-1")
	if err != nil {
		t.Fatal(err)
	}
	if identity.ActorID != "17" || identity.ResourceProjectID != "42" || store.calls != 1 {
		t.Fatalf("identity=%+v membership queries=%d", identity, store.calls)
	}
}

func TestPublicAuthorizerRejectsDevelopmentAndMissingMembership(t *testing.T) {
	developmentStore := &authorizationStore{row: authorizationRow{active: true}}
	authorizer, err := newPostgresPublicAuthorizer(
		developmentStore,
		&authorizationStore{row: authorizationRow{capabilityID: "index.ingest.v1"}},
		indexEventPermissions(),
		allowObservers{},
	)
	if err != nil {
		t.Fatal(err)
	}
	development := auth.ContextWithAuthenticatedUser(context.Background(), auth.User{ID: "17"}, auth.AuthenticationSourceDevelopment)
	if _, err := authorizer.AuthorizeValidation(development, "42", "revision-1"); !errors.Is(err, configurationapi.ErrValidationForbidden) {
		t.Fatalf("development source error = %v", err)
	}
	if developmentStore.calls != 0 {
		t.Fatalf("development source reached database %d times", developmentStore.calls)
	}

	store := &authorizationStore{row: authorizationRow{active: false}}
	authorizer, err = newPostgresPublicAuthorizer(
		store,
		&authorizationStore{row: authorizationRow{capabilityID: "index.ingest.v1"}},
		indexEventPermissions(),
		allowObservers{},
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := auth.ContextWithAuthenticatedUser(context.Background(), auth.User{ID: "17"}, auth.AuthenticationSourceSession)
	if _, err := authorizer.AuthorizeValidation(ctx, "42", "revision-1"); !errors.Is(err, configurationapi.ErrValidationForbidden) {
		t.Fatalf("missing membership error = %v", err)
	}
}

func TestPublicAuthorizerBindsAllowlistedExecutionEventsToProjectionProject(t *testing.T) {
	store := &authorizationStore{row: authorizationRow{capabilityID: "index.ingest.v1"}}
	permissions := indexEventPermissions()
	authorizer, err := newPostgresPublicAuthorizer(
		&authorizationStore{row: authorizationRow{active: true}},
		store,
		permissions,
		allowObservers{},
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := auth.ContextWithAuthenticatedUser(context.Background(), auth.User{ID: "17"}, auth.AuthenticationSourceAPIKey)
	if err := authorizer.AuthorizeExecutionEvents(ctx, "42", "execution-1"); err != nil {
		t.Fatal(err)
	}
	if store.calls != 1 || store.outputParams.ExecutionID != "execution-1" || store.outputParams.ProjectID != 42 {
		t.Fatalf("execution authorization params = %+v calls=%d", store.outputParams, store.calls)
	}
	if permissions.calls != 1 ||
		permissions.principal.ID != "17" ||
		permissions.mode != auth.PermissionModeDefault ||
		permissions.projectID != "42" {
		t.Fatalf("permission resolution mismatch: %+v", permissions)
	}

	store = &authorizationStore{row: authorizationRow{}}
	authorizer, _ = newPostgresPublicAuthorizer(
		&authorizationStore{row: authorizationRow{active: true}},
		store,
		indexEventPermissions(),
		allowObservers{},
	)
	if err := authorizer.AuthorizeExecutionEvents(ctx, "42", "execution-1"); !errors.Is(err, executionapi.ErrExecutionEventsForbidden) {
		t.Fatalf("unauthorized event error = %v", err)
	}
}

func TestPublicAuthorizerRequiresIndexObservationPermission(t *testing.T) {
	store := &authorizationStore{row: authorizationRow{capabilityID: "index.ingest.v1"}}
	permissions := &authorizationPermissionResolver{
		resolution: auth.PermissionResolution{
			UserID:      17,
			Permissions: []string{"models.applications.tool.get"},
		},
	}
	authorizer, err := newPostgresPublicAuthorizer(
		&authorizationStore{row: authorizationRow{active: true}},
		store,
		permissions,
		allowObservers{},
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := auth.ContextWithAuthenticatedUser(context.Background(), auth.User{ID: "17"}, auth.AuthenticationSourceSession)
	if err := authorizer.AuthorizeExecutionEvents(ctx, "42", "execution-1"); !errors.Is(err, executionapi.ErrExecutionEventsForbidden) {
		t.Fatalf("missing patch permission error = %v", err)
	}

	permissions.err = errors.New("inactive principal")
	if err := authorizer.AuthorizeExecutionEvents(ctx, "42", "execution-1"); !errors.Is(err, executionapi.ErrExecutionEventsForbidden) {
		t.Fatalf("resolver failure error = %v", err)
	}
}

func TestPublicAuthorizerRequiresChatPermissionForBothAgentCapabilities(t *testing.T) {
	ctx := auth.ContextWithAuthenticatedUser(
		context.Background(),
		auth.User{ID: "17"},
		auth.AuthenticationSourceSession,
	)
	for _, capabilityID := range []string{
		executiondomain.AgentApplicationCapability,
		executiondomain.AgentAdhocCapability,
	} {
		store := &authorizationStore{row: authorizationRow{capabilityID: capabilityID}}
		permissions := &authorizationPermissionResolver{resolution: auth.PermissionResolution{
			UserID: 17, Permissions: []string{"models.chat.messages.create"},
		}}
		authorizer, err := newPostgresPublicAuthorizer(
			&authorizationStore{row: authorizationRow{active: true}},
			store,
			permissions,
			allowObservers{},
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := authorizer.AuthorizeExecutionEvents(ctx, "42", "execution-1"); err != nil {
			t.Fatalf("capability %s permission rejected: %v", capabilityID, err)
		}
		permissions.resolution.Permissions = []string{"models.chat.messages.get"}
		if err := authorizer.AuthorizeExecutionEvents(ctx, "42", "execution-1"); !errors.Is(err, executionapi.ErrExecutionEventsForbidden) {
			t.Fatalf("capability %s missing permission error = %v", capabilityID, err)
		}
	}
}

// The chat permission says who may chat in the project, not whose execution
// this is. An agent execution's stream is bound to the execution: the observer
// authority decides, and its refusal or failure is the route's answer.
func TestPublicAuthorizerBindsAgentEventsToTheExecutionObserver(t *testing.T) {
	ctx := auth.ContextWithAuthenticatedUser(
		context.Background(),
		auth.User{ID: "17", UserID: "17"},
		auth.AuthenticationSourceSession,
	)
	for _, capabilityID := range []string{
		executiondomain.AgentApplicationCapability,
		executiondomain.AgentAdhocCapability,
	} {
		observers := &recordingObservers{}
		authorizer, err := newPostgresPublicAuthorizer(
			&authorizationStore{row: authorizationRow{active: true}},
			&authorizationStore{row: authorizationRow{capabilityID: capabilityID}},
			&authorizationPermissionResolver{resolution: auth.PermissionResolution{
				UserID: 17, Permissions: []string{"models.chat.messages.create"},
			}},
			observers,
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := authorizer.AuthorizeExecutionEvents(ctx, "42", "execution-of-another-user"); !errors.Is(err, executionapi.ErrExecutionEventsForbidden) {
			t.Fatalf("capability %s: a member who may not observe the execution got %v", capabilityID, err)
		}
		if observers.calls != 1 || observers.project != 42 || observers.executionID != "execution-of-another-user" || observers.principal.ID != "17" {
			t.Fatalf("capability %s: observer asked %+v", capabilityID, observers)
		}
		observers.allowed = true
		if err := authorizer.AuthorizeExecutionEvents(ctx, "42", "execution-of-another-user"); err != nil {
			t.Fatalf("capability %s: an observer was refused: %v", capabilityID, err)
		}
		observers.err = errors.New("database unavailable")
		if err := authorizer.AuthorizeExecutionEvents(ctx, "42", "execution-of-another-user"); err == nil {
			t.Fatalf("capability %s: an observer lookup failure admitted the stream", capabilityID)
		}
	}

	// The binding is the agent branches' alone: index ingest keeps its own
	// named permission and never asks the observer authority.
	observers := &recordingObservers{}
	authorizer, err := newPostgresPublicAuthorizer(
		&authorizationStore{row: authorizationRow{active: true}},
		&authorizationStore{row: authorizationRow{capabilityID: executiondomain.IndexIngestCapability}},
		indexEventPermissions(),
		observers,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := authorizer.AuthorizeExecutionEvents(ctx, "42", "index-execution"); err != nil {
		t.Fatalf("index events: %v", err)
	}
	if observers.calls != 0 {
		t.Fatalf("index events asked the agent observer authority %d times", observers.calls)
	}
}

func TestPublicAuthorizerRequiresAnExecutionObserverAuthority(t *testing.T) {
	if _, err := newPostgresPublicAuthorizer(
		&authorizationStore{}, &authorizationStore{}, indexEventPermissions(), nil,
	); err == nil {
		t.Fatal("an authorizer with no observer authority was built")
	}
}

// TestPublicAuthorizerAuthorizesValidationEventsOnMembershipNotPermissionCount
// pins both directions that the replaced size check got wrong. A member with an
// empty default-mode permission set must keep the stream, and a non-member with
// a large set must lose it. Every case here holds the permission set constant
// against the membership answer, or the membership answer constant against the
// permission set, so a check that reads the set size fails at least one case.
func TestPublicAuthorizerAuthorizesValidationEventsOnMembershipNotPermissionCount(t *testing.T) {
	ctx := auth.ContextWithAuthenticatedUser(
		context.Background(),
		auth.User{ID: "17"},
		auth.AuthenticationSourceSession,
	)
	cases := []struct {
		name        string
		member      bool
		permissions []string
		admitted    bool
	}{
		{
			name:        "member holding no default-mode permission keeps the stream",
			member:      true,
			permissions: nil,
			admitted:    true,
		},
		{
			name:        "member holding one unrelated permission keeps the stream",
			member:      true,
			permissions: []string{"models.project_context.view"},
			admitted:    true,
		},
		{
			name:   "non-member holding many unrelated permissions loses the stream",
			member: false,
			permissions: []string{
				"models.project_context.view",
				"models.applications.tool.patch",
				"models.chat.messages.create",
				"configurations.configuration.details",
			},
			admitted: false,
		},
		{
			name:        "non-member holding no permission loses the stream",
			member:      false,
			permissions: nil,
			admitted:    false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			admission := &authorizationStore{row: authorizationRow{active: testCase.member}}
			output := &authorizationStore{row: authorizationRow{capabilityID: "configuration.validate.v1"}}
			permissions := &authorizationPermissionResolver{
				resolution: auth.PermissionResolution{
					UserID:      17,
					Permissions: testCase.permissions,
				},
			}
			authorizer, err := newPostgresPublicAuthorizer(admission, output, permissions, allowObservers{})
			if err != nil {
				t.Fatal(err)
			}
			err = authorizer.AuthorizeExecutionEvents(ctx, "42", "execution-1")
			if testCase.admitted && err != nil {
				t.Fatalf("member with %d permissions refused: %v", len(testCase.permissions), err)
			}
			if !testCase.admitted && !errors.Is(err, executionapi.ErrExecutionEventsForbidden) {
				t.Fatalf("non-member with %d permissions error = %v", len(testCase.permissions), err)
			}
			// The branch must reach the membership predicate, and it must ask
			// it about the projection project and the polling principal.
			if admission.calls != 1 {
				t.Fatalf("membership query calls = %d, want 1", admission.calls)
			}
			if admission.admissionParams.ProjectID != 42 || admission.admissionParams.UserID != 17 {
				t.Fatalf("membership query params = %+v", admission.admissionParams)
			}
		})
	}
}

// TestPublicAuthorizerRefusesValidationEventsForInactivePrincipal keeps the
// stream fail-closed when the resolver rejects the principal, which is what
// revalidates the active user and token on every poll.
func TestPublicAuthorizerRefusesValidationEventsForInactivePrincipal(t *testing.T) {
	admission := &authorizationStore{row: authorizationRow{active: true}}
	authorizer, err := newPostgresPublicAuthorizer(
		admission,
		&authorizationStore{row: authorizationRow{capabilityID: "configuration.validate.v1"}},
		&authorizationPermissionResolver{err: errors.New("inactive principal")},
		allowObservers{},
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := auth.ContextWithAuthenticatedUser(context.Background(), auth.User{ID: "17"}, auth.AuthenticationSourceSession)
	if err := authorizer.AuthorizeExecutionEvents(ctx, "42", "execution-1"); !errors.Is(err, executionapi.ErrExecutionEventsForbidden) {
		t.Fatalf("inactive principal error = %v", err)
	}
	if admission.calls != 0 {
		t.Fatalf("inactive principal reached the membership query %d times", admission.calls)
	}
}

// TestPublicAuthorizerRefusesValidationEventsWhenMembershipQueryFails keeps a
// database failure from admitting the stream.
func TestPublicAuthorizerRefusesValidationEventsWhenMembershipQueryFails(t *testing.T) {
	authorizer, err := newPostgresPublicAuthorizer(
		&authorizationStore{row: authorizationRow{active: true, err: errors.New("connection reset")}},
		&authorizationStore{row: authorizationRow{capabilityID: "configuration.validate.v1"}},
		&authorizationPermissionResolver{resolution: auth.PermissionResolution{UserID: 17}},
		allowObservers{},
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := auth.ContextWithAuthenticatedUser(context.Background(), auth.User{ID: "17"}, auth.AuthenticationSourceSession)
	if err := authorizer.AuthorizeExecutionEvents(ctx, "42", "execution-1"); err == nil {
		t.Fatal("membership query failure admitted the stream")
	}
}

// TestPublicAuthorizerKeepsNamedPermissionBranchesIndependentOfMembership
// proves the other two branches did not pick up a membership dependency. A
// member without the named permission must still be refused.
func TestPublicAuthorizerKeepsNamedPermissionBranchesIndependentOfMembership(t *testing.T) {
	ctx := auth.ContextWithAuthenticatedUser(context.Background(), auth.User{ID: "17"}, auth.AuthenticationSourceSession)
	cases := []struct {
		capabilityID string
		required     string
	}{
		{capabilityID: executiondomain.IndexIngestCapability, required: "models.applications.tool.patch"},
		{capabilityID: executiondomain.AgentApplicationCapability, required: "models.chat.messages.create"},
		{capabilityID: executiondomain.AgentAdhocCapability, required: "models.chat.messages.create"},
	}
	for _, testCase := range cases {
		t.Run(testCase.capabilityID, func(t *testing.T) {
			admission := &authorizationStore{row: authorizationRow{active: true}}
			permissions := &authorizationPermissionResolver{
				resolution: auth.PermissionResolution{
					UserID:      17,
					Permissions: []string{"models.project_context.view"},
				},
			}
			authorizer, err := newPostgresPublicAuthorizer(
				admission,
				&authorizationStore{row: authorizationRow{capabilityID: testCase.capabilityID}},
				permissions,
				allowObservers{},
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := authorizer.AuthorizeExecutionEvents(ctx, "42", "execution-1"); !errors.Is(err, executionapi.ErrExecutionEventsForbidden) {
				t.Fatalf("member without %s error = %v", testCase.required, err)
			}
			if admission.calls != 0 {
				t.Fatalf("named permission branch ran the membership query %d times", admission.calls)
			}
			permissions.resolution.Permissions = []string{testCase.required}
			if err := authorizer.AuthorizeExecutionEvents(ctx, "42", "execution-1"); err != nil {
				t.Fatalf("caller holding %s refused: %v", testCase.required, err)
			}
		})
	}
}

func TestPublicAuthorizerRejectsUnverifiedExecutionEventPrincipalBeforeDatabase(t *testing.T) {
	store := &authorizationStore{row: authorizationRow{capabilityID: "index.ingest.v1"}}
	authorizer, err := newPostgresPublicAuthorizer(
		&authorizationStore{row: authorizationRow{active: true}},
		store,
		indexEventPermissions(),
		allowObservers{},
	)
	if err != nil {
		t.Fatal(err)
	}
	contexts := []context.Context{
		auth.ContextWithUser(context.Background(), auth.User{ID: "17"}),
		auth.ContextWithAuthenticatedUser(context.Background(), auth.User{ID: "17"}, auth.AuthenticationSourceDevelopment),
	}
	for _, ctx := range contexts {
		if err := authorizer.AuthorizeExecutionEvents(ctx, "42", "execution-1"); !errors.Is(err, executionapi.ErrExecutionEventsForbidden) {
			t.Fatalf("unverified principal error = %v", err)
		}
	}
	if store.calls != 0 {
		t.Fatalf("unverified principal reached execution authorization database %d times", store.calls)
	}
}
