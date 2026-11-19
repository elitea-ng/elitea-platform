package microsoftapps

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Resolution error codes (ADR-0057 decision 5). A credential that names an app
// that cannot be used fails with one of these stable codes, and never falls back
// to another client.
const (
	CodeAppNotFound       = "app_not_found"
	CodeAppDraft          = "app_draft"
	CodeAppDisabled       = "app_disabled"
	CodeAppNeedsAttention = "app_needs_attention"
	CodeTypeNotServed     = "app_type_not_served"
	CodeProjectNotServed  = "app_project_not_served"
	CodeTenantMismatch    = "app_tenant_mismatch"
	CodeSecretUnavailable = "app_secret_unavailable"
	CodeTenantRequired    = "app_tenant_required"
)

// ResolveError is a refusal to use an app, with the stable code the sign-in, the
// refresh and the credential check report. Its message never names the app's
// client id or secret.
type ResolveError struct {
	Code    string
	Message string
}

func (e *ResolveError) Error() string { return e.Message }

// Is lets errors.Is match on the code.
func (e *ResolveError) Is(target error) bool {
	other, ok := target.(*ResolveError)
	return ok && other.Code == e.Code
}

// ErrAppNotFound is the sentinel for errors.Is(err, microsoftapps.ErrAppNotFound).
var ErrAppNotFound = &ResolveError{Code: CodeAppNotFound}

func refuse(code, message string) *ResolveError { return &ResolveError{Code: code, Message: message} }

// Reader is what the resolver needs from the store (the Store implements it;
// tests supply a fake).
type Reader interface {
	Get(ctx context.Context, id string) (App, error)
	List(ctx context.Context) ([]App, error)
	openSecret(ctx context.Context, app App) (Secret, error)
	MarkNeedsAttention(ctx context.Context, id, reason string) error
	CustomAppsAllowed(ctx context.Context) (bool, error)
}

// Resolver is the one door to a registry app for sign-in, refresh and the
// credential check. The availability rule (a serving type, a serving project, an
// active state) is applied at EVERY use, and both ids come from the client, so
// neither is trusted.
type Resolver struct {
	store Reader
	now   func() time.Time
}

// NewResolver builds the resolver over a store.
func NewResolver(store *Store) *Resolver { return &Resolver{store: store, now: time.Now} }

// Request names the use: the app a credential references, the credential's type
// and project, and for Dataverse the credential's tenant_id.
type Request struct {
	AppID          string
	CredentialType string
	ProjectID      int64
	// TenantID is the credential's tenant_id (Dataverse only).
	TenantID string
}

// Resolved is a usable app for one credential. Secret is write-only to this
// process: it redacts in every format, log and JSON encoding.
type Resolved struct {
	AppID    string
	ClientID string
	// Authority is the authority the sign-in and the refresh use. For Dataverse
	// it is built from the credential's tenant when the app's authority is a
	// multi-tenant alias.
	Authority        string
	Secret           Secret
	AutoRefreshToken bool
	// Ceiling is the Graph permissions the app may ask for on this type; nil for
	// Dataverse, whose scope comes from the credential's org URL.
	Ceiling []string
	// Revision changes with every edit of the app, so a caller can tell that the
	// app moved under a cached value.
	Revision int
}

// Resolve checks that the app may be used for the credential and returns the
// client. It reads the secret only after every other check passed.
func (r *Resolver) Resolve(ctx context.Context, req Request) (Resolved, error) {
	var lastStale error
	// An edit between the read that described the app and the read of its secret
	// is not a fault: describe it again. Two attempts are enough, because a third
	// edit inside one request is not a race, it is a loop.
	for attempt := 0; attempt < 2; attempt++ {
		resolved, err := r.resolveOnce(ctx, req)
		if !errors.Is(err, errStale) {
			return resolved, err
		}
		lastStale = err
	}
	return Resolved{}, lastStale
}

func (r *Resolver) resolveOnce(ctx context.Context, req Request) (Resolved, error) {
	app, err := r.usable(ctx, req)
	if err != nil {
		return Resolved{}, err
	}
	authority, err := authorityFor(app, req)
	if err != nil {
		return Resolved{}, err
	}
	secret, err := r.store.openSecret(ctx, app)
	if errors.Is(err, errStale) {
		return Resolved{}, err
	}
	if err != nil {
		if errors.Is(err, ErrCustodyIntegrity) {
			// A secret that does not open is a platform fault the admin must
			// see; the app stops being offered until it is fixed.
			_ = r.store.MarkNeedsAttention(ctx, app.ID, ReasonSecretUnreadable)
		}
		return Resolved{}, refuse(CodeSecretUnavailable, "The Default app's client secret is not available. Ask an administrator to check the app.")
	}
	resolved := Resolved{
		AppID: app.ID, ClientID: app.ClientID, Authority: authority, Secret: secret,
		AutoRefreshToken: app.AutoRefreshToken, Revision: app.Revision,
	}
	if req.CredentialType != TypeDataverse {
		resolved.Ceiling = app.Ceiling(req.CredentialType)
	}
	return resolved, nil
}

// Describe is Resolve without the secret: the app's public facts for a check that
// needs only the client id, authority and ceiling.
func (r *Resolver) Describe(ctx context.Context, req Request) (Resolved, error) {
	app, err := r.usable(ctx, req)
	if err != nil {
		return Resolved{}, err
	}
	authority, err := authorityFor(app, req)
	if err != nil {
		return Resolved{}, err
	}
	resolved := Resolved{
		AppID: app.ID, ClientID: app.ClientID, Authority: authority,
		AutoRefreshToken: app.AutoRefreshToken, Revision: app.Revision,
	}
	if req.CredentialType != TypeDataverse {
		resolved.Ceiling = app.Ceiling(req.CredentialType)
	}
	return resolved, nil
}

func (r *Resolver) usable(ctx context.Context, req Request) (App, error) {
	if !ValidType(req.CredentialType) {
		return App{}, refuse(CodeTypeNotServed, "This credential type does not use Default apps.")
	}
	app, err := r.store.Get(ctx, strings.TrimSpace(req.AppID))
	if errors.Is(err, ErrNotFound) {
		return App{}, refuse(CodeAppNotFound, "The Default app this credential names does not exist.")
	}
	if err != nil {
		return App{}, err
	}
	switch app.State {
	case StateActive:
	case StateDraft:
		return App{}, refuse(CodeAppDraft, "The Default app is not set up yet. Ask an administrator to finish it.")
	case StateDisabled:
		return App{}, refuse(CodeAppDisabled, "The Default app was disabled by an administrator.")
	default:
		return App{}, refuse(CodeAppNeedsAttention, "The Default app needs an administrator's attention.")
	}
	if !app.Serves(req.CredentialType) {
		return App{}, refuse(CodeTypeNotServed, "The Default app does not serve this credential type.")
	}
	if !app.ServesProject(req.ProjectID) {
		return App{}, refuse(CodeProjectNotServed, "The Default app is not available to this project.")
	}
	return app, nil
}

// authorityFor applies the tenant rule of ADR-0057 decision 2. Dataverse builds
// its authority from the credential's tenant: the app's authority must be that
// tenant, or a multi-tenant alias (organizations, common), in which case the
// credential's tenant is used. Every other type uses the app's own authority.
func authorityFor(app App, req Request) (string, error) {
	if req.CredentialType != TypeDataverse {
		return app.Authority, nil
	}
	appAuthority, err := ParseAuthority(app.Authority)
	if err != nil {
		return "", refuse(CodeAppNeedsAttention, "The Default app needs an administrator's attention.")
	}
	tenant := strings.ToLower(strings.TrimSpace(req.TenantID))
	if tenant == "" {
		return "", refuse(CodeTenantRequired, "A Dataverse credential names its tenant ID.")
	}
	credential, err := ParseAuthority("https://" + EntraLoginHost + "/" + tenant)
	if err != nil || credential.MultiTenant() {
		return "", refuse(CodeTenantMismatch, "The Dataverse credential's tenant ID is not a tenant ID or a verified domain.")
	}
	if appAuthority.MultiTenant() || appAuthority.Tenant == credential.Tenant {
		return credential.URL, nil
	}
	return "", refuse(CodeTenantMismatch, "The Default app belongs to a different Microsoft tenant than this Dataverse credential.")
}

// Offer is what a credential form shows of an app: no client id, no secret.
type Offer struct {
	ID               string
	Name             string
	Description      string
	AutoRefreshToken bool
	// Ceiling is the permissions the app may ask for on the type; nil for
	// Dataverse.
	Ceiling []string
}

// Offers lists the apps a credential form may offer for a type and project: active,
// serving the type, serving the project. An empty registry offers nothing, and the
// form shows only Custom app.
func (r *Resolver) Offers(ctx context.Context, credentialType string, projectID int64) ([]Offer, error) {
	if !ValidType(credentialType) {
		return nil, refuse(CodeTypeNotServed, "This credential type does not use Default apps.")
	}
	apps, err := r.store.List(ctx)
	if err != nil {
		return nil, err
	}
	offers := []Offer{}
	for _, app := range apps {
		if app.State != StateActive || !app.Serves(credentialType) || !app.ServesProject(projectID) {
			continue
		}
		offer := Offer{ID: app.ID, Name: app.Name, Description: app.Description, AutoRefreshToken: app.AutoRefreshToken}
		if credentialType != TypeDataverse {
			offer.Ceiling = app.Ceiling(credentialType)
		}
		offers = append(offers, offer)
	}
	return offers, nil
}

// CustomAppsAllowed is `microsoft.custom_apps`: a credential form shows Custom app
// only when true (the default). A read failure allows it, because a database
// hiccup must not silently remove the one mode that works with no registry.
func (r *Resolver) CustomAppsAllowed(ctx context.Context) bool {
	allowed, err := r.store.CustomAppsAllowed(ctx)
	if err != nil {
		return true
	}
	return allowed
}
