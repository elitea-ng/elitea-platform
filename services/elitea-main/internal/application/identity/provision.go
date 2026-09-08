// Package identity coordinates authenticated identity provisioning after an
// identity provider has verified its assertion.
package identity

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

const (
	fallbackEmailDomain = "centry.user"
	projectViewerRole   = "viewer"

	// InitialAdministrationMode / InitialAdministrationRole name the single
	// grant `initial_global_admins` confers. They are exported because a
	// SECOND provisioning plane needs them: internal/api/v2/auth is the
	// browser plane that is actually mounted when single sign-on is
	// configured (internal/api/production_router.go resolves /forward-auth to
	// one owner), and it must confer the same grant, not a lookalike.
	InitialAdministrationMode = "administration"
	InitialAdministrationRole = "super_admin"

	// These conservative bounds keep IdP-controlled writes finite. They must be
	// revalidated against sanitized identity and IdP-claim maxima before mount.
	MaxProviderBytes          = 64
	MaxProviderReferenceBytes = 768
	MaxEmailBytes             = 1024
	MaxNameClaimBytes         = 2048
)

var (
	ErrInvalidAssertion          = errors.New("invalid verified identity assertion")
	ErrIdentitySuspended         = errors.New("authenticated identity is suspended")
	ErrProvisioningFailed        = errors.New("authenticated identity provisioning failed")
	ErrInvalidProvisioningResult = errors.New("identity repository returned an invalid provisioning result")
)

// VerifiedAssertion contains only typed claims from a successfully verified
// provider callback. ProviderReference remains the raw current-baseline value;
// it is intentionally not prefixed or otherwise namespaced in this slice.
type VerifiedAssertion struct {
	Provider          string
	ProviderReference string
	Email             string
	GivenName         string
	FamilyName        string
	Name              string
}

type ProvisionRequest struct {
	Assertion VerifiedAssertion
}

// ProjectEnrollmentPolicy is the current configuration consumed by the
// new_ai_user behavior on every successful browser login. AllowedDomains keeps
// the current comma-separated syntax; AdditionalGlobalAdminRoles are project
// role names, not platform roles.
type ProjectEnrollmentPolicy struct {
	ProjectID                  int32
	AllowedDomains             string
	AdditionalGlobalAdminRoles []string
}

// ProvisioningPolicy is trusted operator configuration captured at service
// construction. Provider callbacks cannot supply or override these values.
type ProvisioningPolicy struct {
	InitialGlobalAdmins []string
	ProjectEnrollment   ProjectEnrollmentPolicy
}

// ProjectEnrollmentDecision is the identity-derived part of project
// reconciliation. Global administration roles remain authoritative in the
// repository transaction and are therefore evaluated there.
type ProjectEnrollmentDecision struct {
	ProjectID                  int32
	Eligible                   bool
	AdditionalGlobalAdminRoles []string
}

// ProvisionCommand is the complete intent for one atomic repository call.
// Empty initial-administration fields mean the exact provider reference was
// not configured as an initial global administrator.
type ProvisionCommand struct {
	Provider                  string
	ProviderReference         string
	Email                     string
	Name                      string
	InitialAdministrationMode string
	InitialAdministrationRole string
	ProjectEnrollment         ProjectEnrollmentDecision
}

type ProvisionResult struct {
	UserID    int64
	Suspended bool
}

// Repository owns the transaction that resolves or creates the user, links
// the provider, applies current-baseline group/profile/initial-admin rules,
// applies project reconciliation, and issues the actor personal access token
// the agent runtime signs the person's calls with — all in that same
// transaction. A successful result is returned only after all those effects
// commit. A suspended result must be returned without committing provisioning
// or reconciliation effects.
//
// The actor token belongs in the SAME list as the initial-admin grant because
// the other browser plane (internal/api/v2/auth, mounted when single sign-on
// is configured) already leaves both rows behind. When only one plane left the
// token behind, which login route an operator chose decided whether a fresh
// install could complete a chat turn.
type Repository interface {
	Provision(ctx context.Context, command ProvisionCommand) (ProvisionResult, error)
}

type ProvisionService struct {
	repository Repository
	policy     ProvisioningPolicy
}

func NewProvisionService(repository Repository, policy ProvisioningPolicy) (*ProvisionService, error) {
	if repository == nil {
		return nil, errors.New("identity repository is required")
	}
	policy.InitialGlobalAdmins = append([]string(nil), policy.InitialGlobalAdmins...)
	policy.ProjectEnrollment.AdditionalGlobalAdminRoles = append(
		[]string(nil),
		policy.ProjectEnrollment.AdditionalGlobalAdminRoles...,
	)
	return &ProvisionService{repository: repository, policy: policy}, nil
}

func (s *ProvisionService) Provision(ctx context.Context, request ProvisionRequest) (ProvisionResult, error) {
	if err := request.Assertion.validate(); err != nil {
		return ProvisionResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ProvisionResult{}, err
	}

	command := deriveCommand(request.Assertion, s.policy)
	result, err := s.repository.Provision(ctx, command)
	if err != nil {
		return ProvisionResult{}, sanitizedRepositoryError(err)
	}
	if result.Suspended {
		return ProvisionResult{}, ErrIdentitySuspended
	}
	if result.UserID <= 0 {
		return ProvisionResult{}, ErrInvalidProvisioningResult
	}
	return result, nil
}

func (a VerifiedAssertion) validate() error {
	if !validRequiredText(a.Provider, MaxProviderBytes) ||
		!validRequiredText(a.ProviderReference, MaxProviderReferenceBytes) {
		return ErrInvalidAssertion
	}
	// The current baseline does not require RFC email syntax. Preserve that
	// contract while rejecting whitespace-bearing input as a strict malformed-
	// claim correction at the new typed boundary.
	if !validOptionalText(a.Email, MaxEmailBytes) || strings.ContainsFunc(a.Email, unicode.IsSpace) {
		return ErrInvalidAssertion
	}
	if !validOptionalText(a.GivenName, MaxNameClaimBytes) ||
		!validOptionalText(a.FamilyName, MaxNameClaimBytes) ||
		!validOptionalText(a.Name, MaxNameClaimBytes) {
		return ErrInvalidAssertion
	}
	return nil
}

func validRequiredText(value string, maxBytes int) bool {
	return len(value) <= maxBytes && validText(value) && strings.TrimSpace(value) != ""
}

func validOptionalText(value string, maxBytes int) bool {
	return value == "" || validRequiredText(value, maxBytes)
}

func validText(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsFunc(value, unicode.IsControl)
}

func deriveCommand(assertion VerifiedAssertion, policy ProvisioningPolicy) ProvisionCommand {
	email := assertion.Email
	if email == "" {
		email = assertion.ProviderReference + "@" + fallbackEmailDomain
	}
	email = lowerLikePython(email)

	name := email
	if assertion.GivenName != "" && assertion.FamilyName != "" {
		name = assertion.GivenName + " " + assertion.FamilyName
	} else if assertion.Name != "" {
		name = assertion.Name
	}

	command := ProvisionCommand{
		Provider:          assertion.Provider,
		ProviderReference: assertion.ProviderReference,
		Email:             email,
		Name:              name,
		ProjectEnrollment: deriveProjectEnrollment(email, policy.ProjectEnrollment),
	}
	if IsInitialGlobalAdmin(policy.InitialGlobalAdmins, assertion.ProviderReference) {
		command.InitialAdministrationMode = InitialAdministrationMode
		command.InitialAdministrationRole = InitialAdministrationRole
	}
	return command
}

// InitialGlobalAdminEmailPrefix marks an `initial_global_admins` entry that
// names a VERIFIED e-mail address instead of a provider reference.
//
// WHY THE LIST NEEDED A SECOND SHAPE. A provider reference is the only exact
// name for a login, but with Azure AD or Okta the OIDC subject is an opaque
// identifier that nobody can know before the person signs in once. The list is
// read at boot, so "deploy, sign in, read the subject out of the database, edit
// the chart, restart" was the only way to make the first administrator of a
// single-sign-on deployment. An address the operator already knows removes that
// round trip.
//
// THE PREFIX IS MANDATORY AND THE NAMESPACE IS CLOSED. An `email:` entry is
// compared against a verified address ONLY, and never against a provider
// reference. Without that rule an identity provider that chooses its own
// subject could assert `sub = "email:victim@corp.com"` and collect a grant
// meant for the address.
const InitialGlobalAdminEmailPrefix = "email:"

// IsInitialGlobalAdmin reports whether the provider reference alone names a
// configured initial global administrator. It is MatchesInitialGlobalAdmin with
// no verified address. A plane that carries no verified-address signal calls
// this one.
func IsInitialGlobalAdmin(admins []string, providerReference string) bool {
	return MatchesInitialGlobalAdmin(admins, providerReference, "")
}

// MatchesInitialGlobalAdmin is the ONE definition of "this login is a
// configured initial global administrator". Both provisioning planes ask it, so
// the list cannot come to mean two different things on the two of them.
//
// AN ENTRY MATCHES IN EXACTLY ONE OF TWO WAYS.
//
//  1. It equals the provider reference, EXACTLY. This is the original rule and
//     it does not change.
//  2. It is `email:<address>` and this login carried that address AS VERIFIED.
//     The comparison is case-insensitive, through the same Unicode lowering
//     the provisioned account's own address gets.
//
// `verifiedEmail` IS A PROMISE, NOT A CLAIM. The caller passes an address here
// only when the identity provider STATED that the address is verified — for
// OIDC, an explicit `"email_verified": true`. A caller with no such statement
// passes "", and then no `email:` entry can match. That keeps the takeover
// class internal/api/v2/auth/oidc.go refuses by construction out of this path:
// an identity provider that merely ASSERTS an address still cannot assert its
// way into the administration role.
//
// The reference passed here must be the BARE one — the raw subject, with no
// `oidc:` / `saml:` namespace prefix. The v2/auth plane stores prefixed
// references and strips the prefix before asking; see
// internal/api/v2/auth/first_login.go for why both spellings are accepted in
// the configuration file but only one is compared here.
func MatchesInitialGlobalAdmin(admins []string, providerReference, verifiedEmail string) bool {
	address := NormalizeInitialGlobalAdminEmail(verifiedEmail)
	for _, admin := range admins {
		candidate, isEmailEntry := strings.CutPrefix(admin, InitialGlobalAdminEmailPrefix)
		if isEmailEntry {
			if address != "" && NormalizeInitialGlobalAdminEmail(candidate) == address {
				return true
			}
			continue
		}
		if providerReference != "" && admin == providerReference {
			return true
		}
	}
	return false
}

// NormalizeInitialGlobalAdminEmail folds an address to the form the comparison
// uses. It answers "" for text that holds no address.
func NormalizeInitialGlobalAdminEmail(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	return lowerLikePython(trimmed)
}

// InitialGlobalAdminShapes counts what an operator actually configured.
//
// It exists for a startup log line. `initial_global_admins` is applied at a
// first login that can happen days later, so a misspelled entry is otherwise
// found as "the administrator grant did nothing", with no way to tell a wrong
// VALUE from a wrong SHAPE.
type InitialGlobalAdminShapes struct {
	// References counts the entries compared against a provider reference:
	// `oidc:<sub>`, `saml:<nameid>`, or a bare subject.
	References int

	// Emails counts the well-formed `email:<address>` entries.
	Emails int

	// Malformed lists the entries that are neither. They are reported, never
	// fatal: a boot that refuses to start over one bad list entry takes out a
	// running deployment for a value that only changes a first login.
	Malformed []string
}

// ClassifyInitialGlobalAdmins sorts the configured entries by shape.
//
// A BARE ADDRESS IS NOT MALFORMED. `alice@corp.com` with no prefix is a valid
// reference entry: it names the pylon-era bare provider reference, and it
// matches `saml:alice@corp.com` through the prefix strip in
// internal/api/v2/auth/first_login.go. To report it would warn about a working
// configuration.
func ClassifyInitialGlobalAdmins(admins []string) InitialGlobalAdminShapes {
	var shapes InitialGlobalAdminShapes
	for _, admin := range admins {
		candidate, isEmailEntry := strings.CutPrefix(admin, InitialGlobalAdminEmailPrefix)
		switch {
		case isEmailEntry && plausibleEmailAddress(candidate):
			shapes.Emails++
		case isEmailEntry:
			shapes.Malformed = append(shapes.Malformed, admin)
		case strings.TrimSpace(admin) != "":
			shapes.References++
		default:
			shapes.Malformed = append(shapes.Malformed, admin)
		}
	}
	return shapes
}

// plausibleEmailAddress is the shape test for the text after `email:`. It is
// coarse on purpose: it refuses what can never equal an address claim, and it
// leaves the exact grammar to the identity provider that issues the claim.
func plausibleEmailAddress(value string) bool {
	if value == "" || value != strings.TrimSpace(value) {
		return false
	}
	local, domain, found := strings.Cut(value, "@")
	if !found || local == "" || domain == "" {
		return false
	}
	if strings.Contains(domain, "@") || !strings.Contains(domain, ".") {
		return false
	}
	return utf8.ValidString(value) &&
		!strings.ContainsFunc(value, unicode.IsSpace) &&
		!strings.ContainsFunc(value, unicode.IsControl)
}

func lowerLikePython(value string) string {
	// Python str.lower and Go strings.ToLower differ for Unicode special-casing
	// and contextual mappings. The pinned x/text Unicode tables are therefore a
	// cross-language readiness input and must stay covered by parity fixtures.
	return cases.Lower(language.Und).String(value)
}

func deriveProjectEnrollment(email string, policy ProjectEnrollmentPolicy) ProjectEnrollmentDecision {
	decision := ProjectEnrollmentDecision{ProjectID: policy.ProjectID}
	allowedDomains := make(map[string]struct{})
	for _, value := range strings.Split(policy.AllowedDomains, ",") {
		allowedDomains[strings.Trim(strings.TrimSpace(value), "@")] = struct{}{}
	}
	domain := email
	if separator := strings.LastIndexByte(email, '@'); separator >= 0 {
		domain = email[separator+1:]
	}
	_, wildcard := allowedDomains["*"]
	_, domainAllowed := allowedDomains[domain]
	decision.Eligible = wildcard || domainAllowed
	if !decision.Eligible {
		return decision
	}

	seen := map[string]struct{}{projectViewerRole: {}}
	decision.AdditionalGlobalAdminRoles = make([]string, 0, len(policy.AdditionalGlobalAdminRoles))
	for _, role := range policy.AdditionalGlobalAdminRoles {
		if _, duplicate := seen[role]; duplicate {
			continue
		}
		seen[role] = struct{}{}
		decision.AdditionalGlobalAdminRoles = append(decision.AdditionalGlobalAdminRoles, role)
	}
	return decision
}

func sanitizedRepositoryError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return ErrProvisioningFailed
	}
}
