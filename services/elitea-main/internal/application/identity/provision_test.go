package identity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type repositoryStub struct {
	calls   int
	command ProvisionCommand
	result  ProvisionResult
	err     error
}

func (s *repositoryStub) Provision(_ context.Context, command ProvisionCommand) (ProvisionResult, error) {
	s.calls++
	s.command = command
	return s.result, s.err
}

func TestProvisionDerivesCurrentBaselineCommand(t *testing.T) {
	tests := []struct {
		name                 string
		assertion            VerifiedAssertion
		initialGlobalAdmins  []string
		wantEmail            string
		wantName             string
		wantInitialAdminMode string
		wantInitialAdminRole string
	}{
		{
			name: "asserted email and given plus family name",
			assertion: VerifiedAssertion{
				Provider:          "oidc",
				ProviderReference: "Admin",
				Email:             "USER@Example.COM",
				GivenName:         "Ada",
				FamilyName:        "Lovelace",
				Name:              "ignored display name",
			},
			initialGlobalAdmins:  []string{"Admin"},
			wantEmail:            "user@example.com",
			wantName:             "Ada Lovelace",
			wantInitialAdminMode: InitialAdministrationMode,
			wantInitialAdminRole: InitialAdministrationRole,
		},
		{
			name: "display name when the component names are incomplete",
			assertion: VerifiedAssertion{
				Provider:          "saml",
				ProviderReference: "Subject-42",
				Email:             "subject@example.test",
				GivenName:         "ignored without family",
				Name:              "Display Name",
			},
			initialGlobalAdmins: []string{"subject-42"},
			wantEmail:           "subject@example.test",
			wantName:            "Display Name",
		},
		{
			name: "email is the name when complete name is unavailable",
			assertion: VerifiedAssertion{
				Provider:          "form",
				ProviderReference: "LocalUser",
				Email:             "LocalUser@Example.test",
				FamilyName:        "ignored without given",
			},
			wantEmail: "localuser@example.test",
			wantName:  "localuser@example.test",
		},
		{
			name: "non RFC asserted email is only lowercased",
			assertion: VerifiedAssertion{
				Provider:          "oidc",
				ProviderReference: "subject",
				Email:             "LOCALLOGIN",
			},
			wantEmail: "locallogin",
			wantName:  "locallogin",
		},
		{
			name: "Python full lowercase expands dotted capital I",
			assertion: VerifiedAssertion{
				Provider:          "oidc",
				ProviderReference: "subject",
				Email:             "\u0130@EXAMPLE.COM",
			},
			wantEmail: "i\u0307@example.com",
			wantName:  "i\u0307@example.com",
		},
		{
			name: "Python contextual lowercase uses final sigma",
			assertion: VerifiedAssertion{
				Provider:          "oidc",
				ProviderReference: "subject",
				Email:             "\u039f\u03a3@EXAMPLE.COM",
			},
			wantEmail: "\u03bf\u03c2@example.com",
			wantName:  "\u03bf\u03c2@example.com",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := &repositoryStub{result: successfulResult(42)}
			service := mustProvisionService(t, repository, ProvisioningPolicy{
				InitialGlobalAdmins: test.initialGlobalAdmins,
			})

			result, err := service.Provision(context.Background(), ProvisionRequest{
				Assertion: test.assertion,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.UserID != 42 {
				t.Fatalf("user ID = %d, want 42", result.UserID)
			}
			if repository.calls != 1 {
				t.Fatalf("repository calls = %d, want 1", repository.calls)
			}

			command := repository.command
			if command.Provider != test.assertion.Provider || command.ProviderReference != test.assertion.ProviderReference {
				t.Fatalf("provider identity changed: %+v", command)
			}
			if command.Email != test.wantEmail || command.Name != test.wantName {
				t.Fatalf("derived identity = (%q, %q), want (%q, %q)", command.Email, command.Name, test.wantEmail, test.wantName)
			}
			if command.InitialAdministrationMode != test.wantInitialAdminMode || command.InitialAdministrationRole != test.wantInitialAdminRole {
				t.Fatalf("initial administration = (%q, %q), want (%q, %q)", command.InitialAdministrationMode, command.InitialAdministrationRole, test.wantInitialAdminMode, test.wantInitialAdminRole)
			}
		})
	}
}

func TestProvisionDerivesCurrentBaselineProjectEnrollment(t *testing.T) {
	tests := []struct {
		name           string
		email          string
		policy         ProjectEnrollmentPolicy
		wantEligible   bool
		wantAdminRoles []string
	}{
		{
			name:  "last at domain with whitespace and surrounding at signs",
			email: "alias@department@example.com",
			policy: ProjectEnrollmentPolicy{
				ProjectID:                  7,
				AllowedDomains:             " other.test, @@example.com@@ ",
				AdditionalGlobalAdminRoles: []string{"public_admin", "viewer", "public_admin", ""},
			},
			wantEligible:   true,
			wantAdminRoles: []string{"public_admin", ""},
		},
		{
			name:  "domain comparison remains case sensitive after email lowercase",
			email: "user@example.com",
			policy: ProjectEnrollmentPolicy{
				ProjectID:      7,
				AllowedDomains: "Example.COM",
			},
		},
		{
			name:  "wildcard",
			email: "local-login",
			policy: ProjectEnrollmentPolicy{
				ProjectID:      7,
				AllowedDomains: "*",
			},
			wantEligible: true,
		},
		{
			name:  "empty configured value matches empty trailing domain like Python split",
			email: "user@",
			policy: ProjectEnrollmentPolicy{
				ProjectID:      7,
				AllowedDomains: "",
			},
			wantEligible: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := deriveProjectEnrollment(test.email, test.policy)
			if decision.ProjectID != test.policy.ProjectID || decision.Eligible != test.wantEligible {
				t.Fatalf("decision = %+v, want project=%d eligible=%t", decision, test.policy.ProjectID, test.wantEligible)
			}
			if strings.Join(decision.AdditionalGlobalAdminRoles, "\x00") != strings.Join(test.wantAdminRoles, "\x00") {
				t.Fatalf("admin roles = %q, want %q", decision.AdditionalGlobalAdminRoles, test.wantAdminRoles)
			}
		})
	}
}

func TestProvisionInitialGlobalAdminMatchIsExactAndCaseSensitive(t *testing.T) {
	for _, admins := range [][]string{
		{"admin"},
		{" ADMIN "},
		{"other", "Admin"},
	} {
		repository := &repositoryStub{result: successfulResult(7)}
		service := mustProvisionService(t, repository, ProvisioningPolicy{
			InitialGlobalAdmins: admins,
		})
		_, err := service.Provision(context.Background(), ProvisionRequest{
			Assertion: VerifiedAssertion{
				Provider:          "oidc",
				ProviderReference: "ADMIN",
				Email:             "admin@example.test",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if repository.command.InitialAdministrationMode != "" || repository.command.InitialAdministrationRole != "" {
			t.Fatalf("non-exact config %q requested initial administration: %+v", admins, repository.command)
		}
	}
}

func TestProvisioningPolicyIsCopiedAtServiceConstruction(t *testing.T) {
	initialAdmins := []string{"ADMIN"}
	additionalRoles := []string{"public_admin"}
	repository := &repositoryStub{result: successfulResult(7)}
	service := mustProvisionService(t, repository, ProvisioningPolicy{
		InitialGlobalAdmins: initialAdmins,
		ProjectEnrollment: ProjectEnrollmentPolicy{
			ProjectID:                  7,
			AllowedDomains:             "example.com",
			AdditionalGlobalAdminRoles: additionalRoles,
		},
	})
	initialAdmins[0] = "changed"
	additionalRoles[0] = "changed"

	_, err := service.Provision(context.Background(), ProvisionRequest{Assertion: VerifiedAssertion{
		Provider:          "oidc",
		ProviderReference: "ADMIN",
		Email:             "user@example.com",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if repository.command.InitialAdministrationRole != InitialAdministrationRole {
		t.Fatalf("initial role = %q, policy was mutated through caller slice", repository.command.InitialAdministrationRole)
	}
	roles := repository.command.ProjectEnrollment.AdditionalGlobalAdminRoles
	if len(roles) != 1 || roles[0] != "public_admin" {
		t.Fatalf("additional roles = %q, policy was mutated through caller slice", roles)
	}
}

func TestProvisionRejectsInvalidAssertionWithoutRepositoryCall(t *testing.T) {
	invalidUTF8 := string([]byte{0xff})
	tests := []struct {
		name      string
		assertion VerifiedAssertion
	}{
		{name: "missing provider", assertion: validAssertion(func(a *VerifiedAssertion) { a.Provider = "" })},
		{name: "blank provider", assertion: validAssertion(func(a *VerifiedAssertion) { a.Provider = " \t " })},
		{name: "invalid provider encoding", assertion: validAssertion(func(a *VerifiedAssertion) { a.Provider = invalidUTF8 })},
		{name: "oversized provider", assertion: validAssertion(func(a *VerifiedAssertion) { a.Provider = strings.Repeat("p", MaxProviderBytes+1) })},
		{name: "missing provider reference", assertion: validAssertion(func(a *VerifiedAssertion) { a.ProviderReference = "" })},
		{name: "blank provider reference", assertion: validAssertion(func(a *VerifiedAssertion) { a.ProviderReference = " \t " })},
		{name: "provider reference control character", assertion: validAssertion(func(a *VerifiedAssertion) { a.ProviderReference = "subject\nother" })},
		{name: "oversized provider reference", assertion: validAssertion(func(a *VerifiedAssertion) { a.ProviderReference = strings.Repeat("s", MaxProviderReferenceBytes+1) })},
		{name: "blank asserted email", assertion: validAssertion(func(a *VerifiedAssertion) { a.Email = " \t " })},
		{name: "email whitespace", assertion: validAssertion(func(a *VerifiedAssertion) { a.Email = "user @example.com" })},
		{name: "invalid email encoding", assertion: validAssertion(func(a *VerifiedAssertion) { a.Email = invalidUTF8 })},
		{name: "oversized email", assertion: validAssertion(func(a *VerifiedAssertion) { a.Email = strings.Repeat("e", MaxEmailBytes+1) })},
		{name: "blank given name", assertion: validAssertion(func(a *VerifiedAssertion) { a.GivenName = " \t " })},
		{name: "oversized given name", assertion: validAssertion(func(a *VerifiedAssertion) { a.GivenName = strings.Repeat("g", MaxNameClaimBytes+1) })},
		{name: "invalid family name encoding", assertion: validAssertion(func(a *VerifiedAssertion) { a.FamilyName = invalidUTF8 })},
		{name: "oversized family name", assertion: validAssertion(func(a *VerifiedAssertion) { a.FamilyName = strings.Repeat("f", MaxNameClaimBytes+1) })},
		{name: "name control character", assertion: validAssertion(func(a *VerifiedAssertion) { a.Name = "first\rsecond" })},
		{name: "oversized display name", assertion: validAssertion(func(a *VerifiedAssertion) { a.Name = strings.Repeat("n", MaxNameClaimBytes+1) })},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := &repositoryStub{result: successfulResult(1)}
			service := mustProvisionService(t, repository)
			_, err := service.Provision(context.Background(), ProvisionRequest{Assertion: test.assertion})
			if !errors.Is(err, ErrInvalidAssertion) {
				t.Fatalf("error = %v, want %v", err, ErrInvalidAssertion)
			}
			if repository.calls != 0 {
				t.Fatalf("repository calls = %d, want 0", repository.calls)
			}
		})
	}
}

func TestProvisionDoesNotCallRepositoryForCanceledContext(t *testing.T) {
	for _, test := range []struct {
		name    string
		context func() context.Context
		want    error
	}{
		{
			name: "canceled",
			context: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			want: context.Canceled,
		},
		{
			name: "deadline exceeded",
			context: func() context.Context {
				ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
				defer cancel()
				return ctx
			},
			want: context.DeadlineExceeded,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := &repositoryStub{result: successfulResult(1)}
			service := mustProvisionService(t, repository)
			_, err := service.Provision(test.context(), ProvisionRequest{Assertion: validAssertion(nil)})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if repository.calls != 0 {
				t.Fatalf("repository calls = %d, want 0", repository.calls)
			}
		})
	}
}

func TestProvisionSanitizesRepositoryErrorsAndPreservesCancellation(t *testing.T) {
	sensitive := errors.New("database failure for user@example.com and provider-secret")
	tests := []struct {
		name    string
		repoErr error
		want    error
	}{
		{name: "dependency failure", repoErr: sensitive, want: ErrProvisioningFailed},
		{name: "canceled", repoErr: errors.Join(sensitive, context.Canceled), want: context.Canceled},
		{name: "deadline exceeded", repoErr: errors.Join(sensitive, context.DeadlineExceeded), want: context.DeadlineExceeded},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := &repositoryStub{err: test.repoErr}
			service := mustProvisionService(t, repository)
			_, err := service.Provision(context.Background(), ProvisionRequest{Assertion: validAssertion(nil)})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if strings.Contains(err.Error(), "user@example.com") || strings.Contains(err.Error(), "provider-secret") {
				t.Fatalf("error leaked repository detail: %v", err)
			}
		})
	}
}

func TestProvisionRejectsSuspendedAndInvalidRepositoryResults(t *testing.T) {
	tests := []struct {
		name   string
		result ProvisionResult
		want   error
	}{
		{
			name: "suspended",
			result: ProvisionResult{
				UserID:    42,
				Suspended: true,
			},
			want: ErrIdentitySuspended,
		},
		{
			name:   "missing user",
			result: ProvisionResult{},
			want:   ErrInvalidProvisioningResult,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := &repositoryStub{result: test.result}
			service := mustProvisionService(t, repository)
			_, err := service.Provision(context.Background(), ProvisionRequest{Assertion: validAssertion(nil)})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestNewProvisionServiceRequiresRepository(t *testing.T) {
	if _, err := NewProvisionService(nil, ProvisioningPolicy{}); err == nil {
		t.Fatal("expected missing repository error")
	}
}

func successfulResult(userID int64) ProvisionResult {
	return ProvisionResult{UserID: userID}
}

func mustProvisionService(t *testing.T, repository Repository, policies ...ProvisioningPolicy) *ProvisionService {
	t.Helper()
	policy := ProvisioningPolicy{}
	if len(policies) != 0 {
		policy = policies[0]
	}
	service, err := NewProvisionService(repository, policy)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func validAssertion(mutate func(*VerifiedAssertion)) VerifiedAssertion {
	assertion := VerifiedAssertion{
		Provider:          "oidc",
		ProviderReference: "admin",
		Email:             "user@example.com",
		GivenName:         "Given",
		FamilyName:        "Family",
		Name:              "Display",
	}
	if mutate != nil {
		mutate(&assertion)
	}
	return assertion
}

// No address is ever synthesized. The fallback that used to turn a missing
// email into `<provider reference>@centry.user` put people into the
// system-identity domain; an assertion with no address — or with an address in
// that domain — is refused before the repository is called.
func TestProvisionRefusesAnAssertionWithoutAUsableEmail(t *testing.T) {
	for _, email := range []string{"", "localuser@centry.user", "LocalUser@CENTRY.USER"} {
		repository := &repositoryStub{result: successfulResult(42)}
		service := mustProvisionService(t, repository, ProvisioningPolicy{})
		_, err := service.Provision(context.Background(), ProvisionRequest{
			Assertion: VerifiedAssertion{Provider: "form", ProviderReference: "LocalUser", Email: email},
		})
		if !errors.Is(err, ErrMissingEmail) {
			t.Fatalf("email %q: error = %v, want %v", email, err, ErrMissingEmail)
		}
		if repository.calls != 0 {
			t.Fatalf("email %q: repository called %d times; nothing may be provisioned", email, repository.calls)
		}
	}
}
