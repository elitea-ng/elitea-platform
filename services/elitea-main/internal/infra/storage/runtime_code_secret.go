package storage

import (
	"context"
	"errors"
	"math"
	"regexp"
	"strconv"
	"unicode/utf8"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
)

var (
	ErrCodeSecretSharingDenied = errors.New("code personal secret sharing denied")
	ErrCodeSecretNotFound      = errors.New("code authorized secret not found")
	ErrCodeSecretPolicyDenied  = errors.New("code protected secret denied")
	codeSecretName             = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
)

// CodePersonalScope is implemented by CurrentExpansionScopeRepository.
// It selects the executing actor's personal project without shared-project fallback.
type CodePersonalScope interface {
	PersonalProjectID(context.Context, int32) (int32, error)
}

// CodeSecretPolicy reads the current default-secret policy.
// A nil policy cannot authorize a read.
type CodeSecretPolicy interface {
	AllowCodeSecret(context.Context, string) (bool, error)
}

type codeSecretSharing interface{ AllowsExternalAccess(string) bool }

// RuntimeCodeSecretService uses native claim and project permission checks.
// It does not mint a bearer token or load an admin vault.
type RuntimeCodeSecretService struct {
	authorizer  AgentRuntimeContextAuthorizer
	permissions auth.PermissionResolver
	personal    CodePersonalScope
	vaults      SecretVaultLoader
	policy      CodeSecretPolicy
}

func NewRuntimeCodeSecretService(authorizer AgentRuntimeContextAuthorizer, permissions auth.PermissionResolver, personal CodePersonalScope, vaults SecretVaultLoader, policy CodeSecretPolicy) (*RuntimeCodeSecretService, error) {
	if authorizer == nil || permissions == nil || personal == nil || vaults == nil || policy == nil {
		return nil, errors.New("code secret dependencies are required")
	}
	return &RuntimeCodeSecretService{authorizer: authorizer, permissions: permissions, personal: personal, vaults: vaults, policy: policy}, nil
}

// Read permits only project or personal scope and an exact ASCII secret name.
// Only an opened, authorized vault miss returns ErrCodeSecretNotFound.
func (s *RuntimeCodeSecretService) authorizedVault(ctx context.Context, claim ContentClaim, scope, name string) (SecretVault, error) {
	if s == nil || ctx == nil || len(name) == 0 || len(name) > 256 || !codeSecretName.MatchString(name) || (scope != "project" && scope != "personal") {
		return nil, ErrContentRejected
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	principal, err := s.authorizer.AuthorizeAgentRuntimeContext(ctx, claim)
	if err != nil {
		return nil, err
	}
	actor, err := strconv.ParseInt(principal.ActorID, 10, 32)
	if err != nil || actor <= 0 || strconv.FormatInt(actor, 10) != principal.ActorID || principal.ResourceProjectID <= 0 || principal.ResourceProjectID > math.MaxInt32 {
		return nil, ErrContentUnauthorized
	}
	permission, err := s.permissions.ResolvePermissions(ctx, auth.User{ID: principal.ActorID, UserID: principal.ActorID}, auth.PermissionModeDefault, strconv.FormatInt(principal.ResourceProjectID, 10))
	if err != nil {
		return nil, err
	}
	allowed := false
	for _, value := range permission.Permissions {
		if value == "configuration.secrets.secret.unsecret" {
			allowed = true
			break
		}
	}
	if permission.UserID != actor || !allowed {
		return nil, ErrContentUnauthorized
	}
	permitted, err := s.policy.AllowCodeSecret(ctx, name)
	if err != nil {
		return nil, err
	}
	if !permitted {
		return nil, ErrCodeSecretPolicyDenied
	}
	project := principal.ResourceProjectID
	if scope == "personal" {
		personal, err := s.personal.PersonalProjectID(ctx, int32(actor))
		if err != nil {
			return nil, err
		}
		if personal <= 0 {
			return nil, ErrContentUnauthorized
		}
		project = int64(personal)
	}
	vault, err := s.vaults.LoadProjectVault(ctx, project)
	if err != nil {
		return nil, err
	}
	if scope == "personal" {
		sharing, ok := vault.(codeSecretSharing)
		if !ok {
			return nil, runtimeContextUnavailable("code_secret_sharing")
		}
		// Check sharing before existence. A missing flag has the same denial.
		if !sharing.AllowsExternalAccess(name) {
			return nil, ErrCodeSecretSharingDenied
		}
	}
	return vault, nil
}

// Authorize checks current permission, personal scope and sharing without reading
// a secret value or turning a dependency/sharing failure into an authorized miss.
func (s *RuntimeCodeSecretService) Authorize(ctx context.Context, claim ContentClaim, scope, name string) error {
	_, err := s.authorizedVault(ctx, claim, scope, name)
	return err
}
func (s *RuntimeCodeSecretService) Read(ctx context.Context, claim ContentClaim, scope, name string) (string, error) {
	vault, err := s.authorizedVault(ctx, claim, scope, name)
	if err != nil {
		return "", err
	}
	secret, err := vault.Lookup(name)
	if errors.Is(err, centrysecrets.ErrSecretNotFound) {
		return "", ErrCodeSecretNotFound
	}
	if err != nil {
		return "", err
	}
	if !utf8.ValidString(secret.Value) || len(secret.Value) > 256*1024 {
		return "", ErrContentRejected
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return secret.Value, nil
}
