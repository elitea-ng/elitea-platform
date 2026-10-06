package storage

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
)

type codeSecretAuthorizer struct {
	actor   int64
	project int64
	err     error
}

func (a *codeSecretAuthorizer) AuthorizeAgentRuntimeContext(context.Context, ContentClaim) (RuntimeContextAuthorization, error) {
	return RuntimeContextAuthorization{ResourceProjectID: a.project, ActorID: strconv.FormatInt(a.actor, 10)}, a.err
}

type codeSecretPermissions struct {
	deny bool
	err  error
}

func (p codeSecretPermissions) ResolvePermissions(_ context.Context, u auth.User, mode, project string) (auth.PermissionResolution, error) {
	if mode != auth.PermissionModeDefault || project != "9" {
		return auth.PermissionResolution{}, errors.New("wrong permission scope")
	}
	actor, _ := strconv.ParseInt(u.UserID, 10, 64)
	permissions := []string{}
	if !p.deny {
		permissions = []string{"configuration.secrets.secret.unsecret"}
	}
	return auth.PermissionResolution{UserID: actor, Permissions: permissions}, p.err
}

type codeSecretScope struct{ err error }

func (s codeSecretScope) PersonalProjectID(_ context.Context, actor int32) (int32, error) {
	return actor + 100, s.err
}

type codeSecretVault struct {
	SecretVault
	value   string
	shared  bool
	missing bool
	lookups int
}

func (v *codeSecretVault) AllowsExternalAccess(string) bool { return v.shared }
func (v *codeSecretVault) Lookup(string) (centrysecrets.Secret, error) {
	v.lookups++
	if v.missing {
		return centrysecrets.Secret{}, centrysecrets.ErrSecretNotFound
	}
	return centrysecrets.Secret{Value: v.value}, nil
}

type codeSecretVaults struct {
	projects map[int64]*codeSecretVault
	reads    []int64
	admin    int
	err      error
}

func (v *codeSecretVaults) LoadProjectVault(_ context.Context, project int64) (SecretVault, error) {
	v.reads = append(v.reads, project)
	if v.err != nil {
		return nil, v.err
	}
	vault := v.projects[project]
	if vault == nil {
		return nil, errors.New("vault unavailable")
	}
	return vault, nil
}
func (v *codeSecretVaults) LoadAdminVault(context.Context) (SecretVault, error) {
	v.admin++
	return nil, errors.New("admin fallback forbidden")
}

type codeSecretPolicy bool

func (p codeSecretPolicy) AllowCodeSecret(context.Context, string) (bool, error) { return bool(p), nil }

func TestCodePersonalSecretSelectsExecutingUsersWithoutSameNameFallback(t *testing.T) {
	a := &codeSecretAuthorizer{actor: 1, project: 9}
	vaults := &codeSecretVaults{projects: map[int64]*codeSecretVault{101: {value: "user-one", shared: true}, 102: {value: "user-two", shared: true}, 9: {value: "shared-project", shared: true}}}
	service, err := NewRuntimeCodeSecretService(a, codeSecretPermissions{}, codeSecretScope{}, vaults, codeSecretPolicy(true))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"user-one", "user-two"} {
		value, err := service.Read(t.Context(), ContentClaim{}, "personal", "same")
		if err != nil || value != expected {
			t.Fatalf("value=%q err=%v", value, err)
		}
		a.actor++
	}
	if len(vaults.reads) != 2 || vaults.reads[0] != 101 || vaults.reads[1] != 102 || vaults.admin != 0 {
		t.Fatalf("unexpected scopes %#v", vaults.reads)
	}
}
func TestCodePersonalSecretChecksSharingBeforeExistence(t *testing.T) {
	for _, missing := range []bool{false, true} {
		vault := &codeSecretVault{value: "private", missing: missing}
		vaults := &codeSecretVaults{projects: map[int64]*codeSecretVault{101: vault}}
		service, _ := NewRuntimeCodeSecretService(&codeSecretAuthorizer{1, 9, nil}, codeSecretPermissions{}, codeSecretScope{}, vaults, codeSecretPolicy(true))
		if _, err := service.Read(t.Context(), ContentClaim{}, "personal", "same"); !errors.Is(err, ErrCodeSecretSharingDenied) {
			t.Fatalf("err=%v", err)
		}
		if vault.lookups != 0 {
			t.Fatal("looked up an unshared secret")
		}
	}
}
func TestCodeSecretOnlyAuthorizedVaultMissIsNotFound(t *testing.T) {
	missing := &codeSecretVault{shared: true, missing: true}
	vaults := &codeSecretVaults{projects: map[int64]*codeSecretVault{101: missing}}
	service, _ := NewRuntimeCodeSecretService(&codeSecretAuthorizer{1, 9, nil}, codeSecretPermissions{}, codeSecretScope{}, vaults, codeSecretPolicy(true))
	if _, err := service.Read(t.Context(), ContentClaim{}, "personal", "same"); !errors.Is(err, ErrCodeSecretNotFound) {
		t.Fatalf("err=%v", err)
	}
	vaults.err = errors.New("database unavailable")
	if _, err := service.Read(t.Context(), ContentClaim{}, "personal", "same"); err == nil || errors.Is(err, ErrCodeSecretNotFound) {
		t.Fatalf("dependency became defaultable: %v", err)
	}
}
func TestCodeSecretDenialsAndMalformedNamesDoNotReadVault(t *testing.T) {
	for _, name := range []string{"same\n", "../same", "same-name", ""} {
		vaults := &codeSecretVaults{}
		service, _ := NewRuntimeCodeSecretService(&codeSecretAuthorizer{1, 9, nil}, codeSecretPermissions{}, codeSecretScope{}, vaults, codeSecretPolicy(true))
		if _, err := service.Read(t.Context(), ContentClaim{}, "personal", name); err == nil {
			t.Fatal("accepted malformed name")
		}
		if len(vaults.reads) != 0 {
			t.Fatal("read vault before validation")
		}
	}
	for _, denied := range []bool{true, false} {
		vaults := &codeSecretVaults{}
		service, _ := NewRuntimeCodeSecretService(&codeSecretAuthorizer{1, 9, nil}, codeSecretPermissions{deny: denied}, codeSecretScope{}, vaults, codeSecretPolicy(denied))
		if _, err := service.Read(t.Context(), ContentClaim{}, "personal", "same"); err == nil {
			t.Fatal("accepted denial")
		}
		if len(vaults.reads) != 0 {
			t.Fatal("read vault before authorization")
		}
	}
}

func TestCodeCommittedSecretAuthorizationRechecksSharingWithoutLookup(t *testing.T) {
	vault := &codeSecretVault{value: "original", shared: true}
	vaults := &codeSecretVaults{projects: map[int64]*codeSecretVault{101: vault}}
	service, _ := NewRuntimeCodeSecretService(&codeSecretAuthorizer{1, 9, nil}, codeSecretPermissions{}, codeSecretScope{}, vaults, codeSecretPolicy(true))
	if _, err := service.Read(t.Context(), ContentClaim{}, "personal", "same"); err != nil {
		t.Fatal(err)
	}
	if err := service.Authorize(t.Context(), ContentClaim{}, "personal", "same"); err != nil {
		t.Fatal(err)
	}
	vault.shared = false
	if err := service.Authorize(t.Context(), ContentClaim{}, "personal", "same"); !errors.Is(err, ErrCodeSecretSharingDenied) {
		t.Fatal("revoked sharing released old value")
	}
	if vault.lookups != 1 || len(vaults.reads) != 3 || vaults.admin != 0 {
		t.Fatal("authorization reread a secret or used a fallback")
	}
}
