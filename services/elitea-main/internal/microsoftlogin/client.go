package microsoftlogin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftapps"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftauth/scopes"
)

// Credential is one saved Microsoft credential, as the sign-in reads it: its type
// and its data with every reference resolved, so a Custom app's client secret is
// the plaintext. It never leaves this process and is never logged.
type Credential struct {
	ID   string
	Type string
	Data map[string]any
}

// ErrCredentialNotFound is a credential that is not in the project.
var ErrCredentialNotFound = errors.New("microsoftlogin: no such credential in the project")

// Credentials reads a saved credential in a project (the Configurations store).
type Credentials interface {
	Load(ctx context.Context, projectID int64, credentialID string) (Credential, error)
}

// Apps is the Default app registry (microsoftapps.Resolver) and the platform
// setting that switches Custom app off.
type Apps interface {
	Resolve(ctx context.Context, req microsoftapps.Request) (microsoftapps.Resolved, error)
	CustomAppsAllowed(ctx context.Context) bool
}

// Stable refusal codes of a sign-in that cannot start. A Default app that cannot be
// used refuses with the registry's own code (app_disabled, app_draft, ...).
const (
	CodeNotMicrosoft        = "not_microsoft_credential"
	CodeNotDelegated        = "not_delegated"
	CodeCustomAppsDisabled  = "custom_apps_disabled"
	CodeAppsUnavailable     = "default_apps_unavailable"
	CodeClientInvalid       = "client_invalid"
	CodeAuthorityInvalid    = "authority_invalid"
	CodeScopesInvalid       = "scopes_invalid"
	CodeCredentialNotFound  = "credential_not_found"
	CodeCredentialChanged   = "credential_changed"
	CodeTooManySignIns      = "too_many_sign_ins"
	CodeClientKindInvalid   = "client_kind_invalid"
	CodeClientChallenge     = "client_challenge_invalid"
	CodeClientReturnInvalid = "client_return_invalid"
	CodeCredentialIDInvalid = "credential_id_invalid"
)

// Refusal is a sign-in refused for a reason the person (or an administrator) can
// act on. Its message names no client id, secret, token or host.
type Refusal struct {
	Code    string
	Message string
}

func (r *Refusal) Error() string { return r.Message }

func refuse(code, message string) *Refusal { return &Refusal{Code: code, Message: message} }

var guidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// client is the OAuth client a sign-in uses: who it is, where it signs in, what it
// asks for. A Custom app's secret is read from the credential every time it is
// needed and held only for the request.
type client struct {
	descriptor microsoftauth.Descriptor
	appRef     string
	clientID   string
	secret     microsoftapps.Secret
	authority  string
	// scopes is what the sign-in asks Microsoft for, OIDC scopes included.
	scopes []string
	// data is the credential's own fields, for the identity read (mailbox, org URL).
	data map[string]any
}

// fingerprint is the SHA-256 of the client id: what a sign-in records about the
// client it used (ADR-0057 decision 5, "a sign-in belongs to one client").
func fingerprint(clientID string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(clientID))))
	return hex.EncodeToString(sum[:])
}

// DefaultAppSignInScopes is the sign-in scope list of a credential under a Default
// app: what the credential selected, cut to the app's ceiling, and `offline_access`
// according to the APP's refresh setting (the credential's own switch does not
// apply). effective is what the credential is left with; removed names what the
// ceiling cut.
func DefaultAppSignInScopes(descriptor microsoftauth.Descriptor, app microsoftapps.Resolved, credentialType string, data map[string]any) (signIn, effective, removed []string, err error) {
	selected, _, err := descriptor.Scopes(data)
	if err != nil {
		return nil, nil, nil, err
	}
	effective, removed = app.EffectiveScopes(credentialType, selected)
	if descriptor.ScopeModel() == microsoftauth.ScopesAsStored {
		// SharePoint's sign-in scopes are the credential's own list, as ever.
		return effective, effective, removed, nil
	}
	refresh := app.AutoRefreshToken
	return scopes.SignIn(effective, &refresh), effective, removed, nil
}

// resolveClient decides which client signs this credential in, applying every rule
// of ADR-0057 decision 5 on every use: a Default app is resolved from the registry
// (active, serving the type and the project, the Dataverse tenant rule), a Custom
// app only while `microsoft.custom_apps` allows it, and the authority is always an
// Entra authority. The sign-in start and the callback both call it, so a credential
// that changed between the two is noticed.
func (s *Service) resolveClient(ctx context.Context, projectID int64, credential Credential) (client, error) {
	descriptor, known := microsoftauth.For(credential.Type)
	if !known {
		return client{}, refuse(CodeNotMicrosoft, "This credential type does not sign in with Microsoft.")
	}
	if !descriptor.IsDelegated(credential.Data) {
		return client{}, refuse(CodeNotDelegated, "This credential does not sign in per user.")
	}
	out := client{descriptor: descriptor, data: credential.Data}
	appID, _ := credential.Data[microsoftauth.FieldAppID].(string)
	appID = strings.TrimSpace(appID)
	var signIn []string
	if descriptor.DefaultAppSupported() && appID != "" {
		resolved, err := s.resolveDefault(ctx, projectID, credential, appID)
		if err != nil {
			return client{}, err
		}
		out.appRef, out.clientID, out.secret, out.authority = resolved.AppID, resolved.ClientID, resolved.Secret, resolved.Authority
		signIn, _, _, err = DefaultAppSignInScopes(descriptor, resolved, credential.Type, credential.Data)
		if err != nil {
			return client{}, refuse(CodeScopesInvalid, "Scopes: "+err.Error()+".")
		}
	} else {
		// A credential saved around the form's switch cannot sign in with its own
		// client while an administrator has Custom app off (a review finding of
		// S1b): the form hides Custom app, and this is the server-side half.
		if s.apps != nil && !s.apps.CustomAppsAllowed(ctx) {
			return client{}, refuse(CodeCustomAppsDisabled,
				"An administrator turned Custom app off. Use a Default app for this credential.")
		}
		var err error
		if out, err = s.customClient(out, credential); err != nil {
			return client{}, err
		}
		signIn, _, err = descriptor.SignInScopes(credential.Data)
		if err != nil {
			return client{}, refuse(CodeScopesInvalid, "Scopes: "+err.Error()+".")
		}
	}
	authority, err := microsoftauth.NormalizeEntraAuthority(out.authority)
	if err != nil {
		return client{}, refuse(CodeAuthorityInvalid, err.Error())
	}
	out.authority = authority
	out.scopes = withIdentityScopes(signIn)
	// The identity read must be buildable (a Dataverse org URL, an Outlook mailbox): a
	// credential that cannot be probed is refused here, not discovered at the first
	// tool call after a sign-in that looked successful.
	if _, err := descriptor.ProbeRequest(ctx, credential.Data, "probe"); err != nil {
		return client{}, refuse(CodeClientInvalid, "The credential's "+descriptor.Label()+" settings are not valid: "+err.Error()+".")
	}
	return out, nil
}

func (s *Service) resolveDefault(ctx context.Context, projectID int64, credential Credential, appID string) (microsoftapps.Resolved, error) {
	if s.apps == nil {
		// No registry is composed: an app reference resolves to nothing, and
		// falling back to another client would sign the person in as someone the
		// credential never named.
		return microsoftapps.Resolved{}, refuse(CodeAppsUnavailable,
			"Default apps are not available on this deployment yet. Use a Custom app.")
	}
	tenant, _ := credential.Data[microsoftauth.FieldTenantID].(string)
	resolved, err := s.apps.Resolve(ctx, microsoftapps.Request{
		AppID: appID, CredentialType: credential.Type, ProjectID: projectID, TenantID: tenant,
	})
	if err == nil {
		return resolved, nil
	}
	var refusal *microsoftapps.ResolveError
	if errors.As(err, &refusal) {
		return microsoftapps.Resolved{}, refuse(refusal.Code, refusal.Message)
	}
	s.logger.ErrorContext(ctx, "microsoft login: the Default app registry was not read", "type", credential.Type, "project_id", projectID)
	return microsoftapps.Resolved{}, err
}

func (s *Service) customClient(out client, credential Credential) (client, error) {
	clientID, _ := credential.Data["client_id"].(string)
	clientID = strings.TrimSpace(clientID)
	if !guidPattern.MatchString(clientID) {
		return client{}, refuse(CodeClientInvalid, "The credential's client ID is not an Entra application (client) ID.")
	}
	secret, _ := credential.Data["client_secret"].(string)
	authority, err := out.descriptor.Authority(credential.Data)
	if err != nil {
		return client{}, refuse(CodeAuthorityInvalid, err.Error())
	}
	out.appRef, out.clientID, out.authority = AppCustom, clientID, authority
	out.secret = microsoftapps.NewSecret(secret)
	return out, nil
}

// AppCustom is the app reference of a credential that signs in with its own client.
const AppCustom = "custom"

// withIdentityScopes adds the OIDC scopes a sign-in needs to learn who signed in:
// without openid and profile Entra returns no id token, and a login with no
// identity cannot be bound to a person.
func withIdentityScopes(signIn []string) []string {
	out := append([]string(nil), signIn...)
	have := map[string]bool{}
	for _, scope := range out {
		have[strings.ToLower(scope)] = true
	}
	for _, scope := range []string{"openid", "profile"} {
		if !have[scope] {
			out = append(out, scope)
		}
	}
	return out
}
