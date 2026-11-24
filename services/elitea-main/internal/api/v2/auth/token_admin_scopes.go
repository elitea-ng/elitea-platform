package auth

// Administration scopes on personal access tokens (ADR-0059 decision 7,
// "Token scopes" and "Who can set scopes"; story R1.3a, issue 2507).
//
// A token carries an administration access (shared 0265): `none` (the
// default), `scoped` (only the names in admin_scopes, intersected with the
// owner's CURRENT set at every request, R1.2a) or `inherit` (a backfill value
// that ends on the sunset date). This file is the write side: the two
// requests that set scopes, and the read that tells the token screen which
// scopes it may offer.
//
//   - POST /auth/token/ takes `admin_access` (`none` or `scoped`, never
//     `inherit`) and `admin_scopes`.
//   - PUT /auth/token/{uuid}/admin_scopes replaces the access of an owned
//     token. `none` drops every scope, `scoped` sets exactly the names asked
//     for. `inherit` is never accepted: it exists only as a backfill value.
//   - GET /auth/token/admin_scopes answers the scopes THIS credential may set:
//     its own administration set, intersected with the registry's token
//     scopes. The token screen offers these and nothing else (R1.3b); hiding
//     is not enforcement, and the two requests check again.
//
// # Who can set scopes
//
// Setting `scoped` needs, in this order (each refusal is a 403 with a stable
// code and an admin audit row, except the sign-in one, which is a 401):
//
//  1. a browser session of the kind `session` or `form` (CodeScopesBrowserSessionRequired;
//     a personal access token, a native session, a run token and a principal
//     with no kind are refused), never `sealed` (CodeScopesSealedSession);
//  2. no elevation (CodeScopesElevated): the resolver's elevation indicator,
//     PermissionResolution.ElevationID, which the elevation union of R2.1
//     sets, so the rule is live the day it merges;
//  3. every name is a token scope in the registry (CodeScopeNotAllowed:
//     `token: never`, or no registry row at all);
//  4. every name is inside the CALLING credential's administration set
//     (CodeScopeNotHeld): what the resolver answers for this request, never
//     the owner's full set;
//  5. the person authenticated within the configured time
//     (apimw.RecentAuthentication): 401 `reauthentication_required`, with the
//     limit and the sign-in method in headers, so the web client sends the
//     person through the right re-entry and back (R1.3b).
//
// The scope checks run before the sign-in one, so nobody signs in again only
// to be refused. Dropping every scope (`none` on the PUT) needs rules 1 and 2
// only: narrowing a credential needs no recent sign-in.
//
// Every change of a token's access appends one admin.access.changed event
// (kind token_scopes, R1.5) in the transaction of the change.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit/accessevent"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// The refusal codes of the scope rule. Clients key their message on them.
const (
	// CodeScopesBrowserSessionRequired: the calling credential is not a
	// browser session of the kind `session` or `form`.
	CodeScopesBrowserSessionRequired = "token_scopes_browser_session_required"
	// CodeScopesSealedSession: a sealed break-glass session (R2.7).
	CodeScopesSealedSession = "token_scopes_sealed_session"
	// CodeScopesElevated: the caller's set includes an elevation (R2.1).
	CodeScopesElevated = "token_scopes_elevated"
	// CodeScopeNotAllowed: a name the registry marks `token: never`, or no
	// administration permission at all. The body's `scopes` lists them.
	CodeScopeNotAllowed = "token_scope_not_allowed"
	// CodeScopeNotHeld: a name outside the calling credential's own
	// administration set. The body's `scopes` lists them.
	CodeScopeNotHeld = "token_scope_not_held"
	// CodeScopesCallerUnresolved: the caller's administration set could not
	// be read (no resolver, or the resolver refused the principal).
	CodeScopesCallerUnresolved = "token_scopes_caller_unresolved"
)

// TokenAdminScopesRefusedAuditAction is the admin audit action of a refused
// scope change on PUT /auth/token/{uuid}/admin_scopes. A refused create keeps
// TokenMintRefusedAuditAction.
const TokenAdminScopesRefusedAuditAction = "token.admin_scopes_refused"

// accessEventSource is the CloudEvents source of this package's
// admin.access.changed events.
const accessEventSource = "elitea-main/auth"

const (
	adminAccessNone    = adminaccess.TokenAccessNone
	adminAccessScoped  = adminaccess.TokenAccessScoped
	adminAccessInherit = adminaccess.TokenAccessInherit
)

// tokenAdminAccess is a token's administration access.
type tokenAdminAccess struct {
	Access string
	Scopes []string
}

// granted reports an access that carries administration rights to write:
// scoped. none, the zero value and inherit (never written here) are not.
func (a tokenAdminAccess) granted() bool { return a.Access == adminAccessScoped }

// normalized answers the access as the responses state it: none for the zero
// value, and never a nil scope list.
func (a tokenAdminAccess) normalized() tokenAdminAccess {
	if a.Access == "" {
		a.Access = adminAccessNone
	}
	if a.Scopes == nil {
		a.Scopes = []string{}
	}
	return a
}

// tokenAdminAccessChange is one change of a token's access, for its event.
type tokenAdminAccessChange struct {
	TokenID int64
	Before  tokenAdminAccess
	After   tokenAdminAccess
	Created bool
}

// tokenAdminAccessUpdate is the input of SetAdminAccess.
type tokenAdminAccessUpdate struct {
	OwnerID  int64
	UUID     string
	Admin    tokenAdminAccess
	OnChange tokenAdminAccessHook
}

var (
	errNoAdminAccessHook = errors.New("token administration access: no change hook")
	errNoAccessAppender  = errors.New("token administration access: no security audit appender")
)

// tokenAdminAccessInputError is a 400 of the administration fields.
type tokenAdminAccessInputError struct {
	code    string
	message string
}

// maxTokenAdminScopes bounds admin_scopes: no more than every registry name.
var maxTokenAdminScopes = len(adminaccess.Names())

// parseTokenAdminAccess reads `admin_access` and `admin_scopes`. Absent
// access with no scopes is `none`. `inherit` is refused: the dialog never
// offers it and no request sets it. Scopes are sorted and de-duplicated.
func parseTokenAdminAccess(access *string, scopes *[]string) (tokenAdminAccess, *tokenAdminAccessInputError) {
	var names []string
	if scopes != nil {
		names = *scopes
	}
	value := adminAccessNone
	if access != nil {
		value = *access
	}
	switch value {
	case adminAccessNone:
		if len(names) > 0 {
			return tokenAdminAccess{}, &tokenAdminAccessInputError{"invalid_admin_scopes",
				"admin_scopes must be empty unless admin_access is scoped"}
		}
		return tokenAdminAccess{Access: adminAccessNone, Scopes: []string{}}, nil
	case adminAccessScoped:
	default:
		return tokenAdminAccess{}, &tokenAdminAccessInputError{"invalid_admin_access",
			"admin_access must be none or scoped"}
	}
	if len(names) == 0 || len(names) > maxTokenAdminScopes {
		return tokenAdminAccess{}, &tokenAdminAccessInputError{"invalid_admin_scopes",
			fmt.Sprintf("a scoped token needs between 1 and %d admin_scopes", maxTokenAdminScopes)}
	}
	for _, name := range names {
		if name == "" || len(name) > adminaccess.MaxNameLength {
			return tokenAdminAccess{}, &tokenAdminAccessInputError{"invalid_admin_scopes",
				"every admin_scopes entry must be a permission name"}
		}
	}
	sorted := slices.Clone(names)
	slices.Sort(sorted)
	return tokenAdminAccess{Access: adminAccessScoped, Scopes: slices.Compact(sorted)}, nil
}

// tokenScopeRefusal is a 403 of the scope rule; Scopes names the refused
// names, when the refusal is about names.
type tokenScopeRefusal struct {
	tokenMintRefusal
	Scopes []string
}

// adminScopeCaller is what the scope rule learned about the calling
// credential: its refusal, or its administration set.
type adminScopeCaller struct {
	refusal *tokenScopeRefusal
	set     []string
}

// scopeKindRefusal applies rule 1: the credential kinds that may set scopes.
func scopeKindRefusal(kind identity.CredentialKind) *tokenScopeRefusal {
	refuse := func(code, message string) *tokenScopeRefusal {
		return &tokenScopeRefusal{tokenMintRefusal: tokenMintRefusal{
			Code: code, Message: message, CredentialKind: mintCredential(kind),
		}}
	}
	switch kind {
	case identity.CredentialKindSession, identity.CredentialKindForm:
		return nil
	case identity.CredentialKindSealed:
		return refuse(CodeScopesSealedSession,
			"a break-glass session cannot give a token administration scopes; use your own sign-in")
	default:
		return refuse(CodeScopesBrowserSessionRequired,
			"administration scopes can be set only from a browser sign-in, in the web app")
	}
}

// adminScopeCallerOf applies rules 1 and 2. err is a dependency fault (500).
func (h *Handler) adminScopeCallerOf(r *http.Request, user identity.User) (adminScopeCaller, error) {
	kind := mintCredential(user.CredentialKind)
	refuse := func(code, message string) (adminScopeCaller, error) {
		return adminScopeCaller{refusal: &tokenScopeRefusal{
			tokenMintRefusal: tokenMintRefusal{Code: code, Message: message, CredentialKind: kind},
		}}, nil
	}
	if refusal := scopeKindRefusal(user.CredentialKind); refusal != nil {
		return adminScopeCaller{refusal: refusal}, nil
	}
	if h.permissions == nil {
		return refuse(CodeScopesCallerUnresolved, "the server could not read your administration permissions")
	}
	resolution, err := h.permissions.ResolvePermissions(r.Context(), user, identity.PermissionModeAdministration, "")
	if errors.Is(err, identity.ErrPermissionDenied) {
		return refuse(CodeScopesCallerUnresolved, "the server could not read your administration permissions")
	}
	if err != nil {
		return adminScopeCaller{}, err
	}
	if resolution.ElevationID != "" || audit.ElevationIDFromContext(r.Context()) != "" {
		return refuse(CodeScopesElevated,
			"administration scopes cannot be set under an elevation; end it and use your standing sign-in")
	}
	return adminScopeCaller{set: resolution.Permissions}, nil
}

// scopeRefusalOf applies rules 3 and 4 to names, against the caller's set.
func scopeRefusalOf(kind mintCredential, set, names []string) *tokenScopeRefusal {
	var notAllowed, notHeld []string
	for _, name := range names {
		row, ok := adminaccess.Lookup(name)
		switch {
		case !ok || !row.Token:
			notAllowed = append(notAllowed, name)
		case !slices.Contains(set, name):
			notHeld = append(notHeld, name)
		}
	}
	if len(notAllowed) > 0 {
		return &tokenScopeRefusal{tokenMintRefusal: tokenMintRefusal{Code: CodeScopeNotAllowed, CredentialKind: kind,
			Message: "these permissions can never be a token scope"}, Scopes: notAllowed}
	}
	if len(notHeld) > 0 {
		return &tokenScopeRefusal{tokenMintRefusal: tokenMintRefusal{Code: CodeScopeNotHeld, CredentialKind: kind,
			Message: "a token can carry only administration permissions you hold yourself"}, Scopes: notHeld}
	}
	return nil
}

// authorizeAdminScopes applies the whole rule to a request that sets the
// scopes names. It writes the refusal and answers false, or answers true.
// action is the audit action of a refusal.
func (h *Handler) authorizeAdminScopes(w http.ResponseWriter, r *http.Request, user identity.User, names []string, action string) bool {
	caller, err := h.adminScopeCallerOf(r, user)
	if err != nil {
		writeTokenMintFault(w, r, mintCredential(user.CredentialKind), err)
		return false
	}
	refusal := caller.refusal
	if refusal == nil {
		refusal = scopeRefusalOf(mintCredential(user.CredentialKind), caller.set, names)
	}
	if refusal != nil {
		writeTokenScopeRefusal(w, r, action, refusal)
		return false
	}
	if _, ok := h.recentAuthentication.Check(r); !ok {
		h.recentAuthentication.Refuse(w, r)
		return false
	}
	return true
}

// tokenScopeJSONError is the nested envelope of a scope refusal.
type tokenScopeJSONError struct {
	Error struct {
		tokenJSONErrorFields
		Scopes []string `json:"scopes,omitempty"`
	} `json:"error"`
}

func writeTokenScopeRefusal(w http.ResponseWriter, r *http.Request, action string, refusal *tokenScopeRefusal) {
	if len(refusal.Scopes) == 0 {
		writeTokenRefusal(w, r, action, &refusal.tokenMintRefusal)
		return
	}
	audit.Annotate(r.Context(), audit.Annotation{Action: action, EntityType: "token", EntityName: refusal.Code})
	var body tokenScopeJSONError
	body.Error.tokenJSONErrorFields = tokenJSONErrorFields{Message: refusal.Message, Type: "permission_error", Code: refusal.Code}
	body.Error.Scopes = refusal.Scopes
	writeJSON(w, http.StatusForbidden, body)
}

// tokenAdminAccessEvent is the hook that appends the admin.access.changed
// event of a change, in its transaction. ownerID is the actor: only the owner
// changes a token's access.
func (h *Handler) tokenAdminAccessEvent(r *http.Request, ownerID int64) tokenAdminAccessHook {
	requestContext := r.Context()
	return func(ctx context.Context, tx pgx.Tx, change tokenAdminAccessChange) error {
		if h.accessAudit == nil {
			return errNoAccessAppender
		}
		fields := []string{}
		switch {
		case change.Created:
			fields = append(fields, "created")
		case change.Before.normalized().Access != change.After.normalized().Access:
			fields = append(fields, "admin_access")
		}
		event := accessevent.FromRequest(requestContext, securityaudit.AdminAccessChanged{
			ActorUserID: ownerID,
			SubjectType: "token",
			SubjectID:   strconv.FormatInt(change.TokenID, 10),
			Change: securityaudit.AccessChange{
				Kind:   securityaudit.AccessTokenScopes,
				Before: change.Before.normalized().Scopes,
				After:  change.After.normalized().Scopes,
				Fields: fields,
			},
		})
		return h.accessAudit.Append(ctx, tx, securityaudit.NewAdminAccessChanged(event))
	}
}

// tokenScopeOffer is the answer of GET /auth/token/admin_scopes.
type tokenScopeOffer struct {
	// Settable is false when this credential may set no scope at all; Code
	// then says why (one of the refusal codes).
	Settable bool               `json:"settable"`
	Code     string             `json:"code,omitempty"`
	Scopes   []tokenScopeOption `json:"scopes"`
}

type tokenScopeOption struct {
	Name  string `json:"name"`
	Area  string `json:"area"`
	Class string `json:"class"`
}

// TokenAdminScopeOffer serves GET /auth/token/admin_scopes: the scopes the
// calling credential may give a token. Empty for a caller who holds no
// administration permission, and for every credential the rule refuses.
func (h *Handler) TokenAdminScopeOffer(w http.ResponseWriter, r *http.Request) {
	user, ok := identity.UserFromContext(r.Context())
	if !ok {
		apierr.Write(w, apierr.Unauthorized("not authenticated"))
		return
	}
	caller, err := h.adminScopeCallerOf(r, user)
	if err != nil {
		apierr.Write(w, apierr.Internal("failed to resolve caller permissions"))
		return
	}
	offer := tokenScopeOffer{Scopes: []tokenScopeOption{}}
	if caller.refusal != nil {
		offer.Code = caller.refusal.Code
		writeJSON(w, http.StatusOK, offer)
		return
	}
	offer.Settable = true
	for _, name := range caller.set {
		if row, ok := adminaccess.Lookup(name); ok && row.Token {
			offer.Scopes = append(offer.Scopes, tokenScopeOption{Name: row.Name, Area: string(row.Area), Class: string(row.Class)})
		}
	}
	slices.SortFunc(offer.Scopes, func(a, b tokenScopeOption) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	writeJSON(w, http.StatusOK, offer)
}

// tokenAdminScopesRequest is the body of the PUT.
type tokenAdminScopesRequest struct {
	AdminAccess *string   `json:"admin_access"`
	AdminScopes *[]string `json:"admin_scopes"`
}

// TokenSetAdminScopes serves PUT /auth/token/{tokenUUID}/admin_scopes.
func (h *Handler) TokenSetAdminScopes(w http.ResponseWriter, r *http.Request) {
	user, ok := identity.UserFromContext(r.Context())
	if !ok {
		apierr.Write(w, apierr.Unauthorized("not authenticated"))
		return
	}
	ownerID, ok := user.OwningUserID()
	if !ok {
		apierr.Write(w, apierr.Unauthorized("not authenticated"))
		return
	}
	// Rules 1 and 2 first, before the service check and the body: a refused
	// credential learns nothing about the deployment.
	caller, err := h.adminScopeCallerOf(r, user)
	if err != nil {
		writeTokenMintFault(w, r, mintCredential(user.CredentialKind), err)
		return
	}
	if caller.refusal != nil {
		writeTokenScopeRefusal(w, r, TokenAdminScopesRefusedAuditAction, caller.refusal)
		return
	}
	if !h.tokenServiceAvailable(w) {
		return
	}
	tokenUUID := chi.URLParam(r, "tokenUUID")
	if !validTokenUUID(tokenUUID) {
		apierr.Write(w, apierr.BadRequest("invalid token UUID"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTokenRequestBodyBytes)
	var request tokenAdminScopesRequest
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&request); err != nil || requireJSONDocumentEOF(decoder) != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}
	if request.AdminAccess == nil {
		writeTokenJSONError(w, http.StatusBadRequest, "invalid_request_error", "invalid_admin_access",
			"admin_access is required: none or scoped")
		return
	}
	admin, inputErr := parseTokenAdminAccess(request.AdminAccess, request.AdminScopes)
	if inputErr != nil {
		writeTokenJSONError(w, http.StatusBadRequest, "invalid_request_error", inputErr.code, inputErr.message)
		return
	}
	if admin.granted() && !h.authorizeAdminScopes(w, r, user, admin.Scopes, TokenAdminScopesRefusedAuditAction) {
		return
	}
	record, err := h.tokens.SetAdminAccess(r.Context(), tokenAdminAccessUpdate{
		OwnerID: ownerID, UUID: tokenUUID, Admin: admin, OnChange: h.tokenAdminAccessEvent(r, ownerID),
	})
	switch {
	case errors.Is(err, errTokenNotFound), errors.Is(err, errTokenForbidden):
		// The same answer as TokenGet: a token of another user is not
		// distinguishable from no token.
		apierr.Write(w, apierr.BadRequest("token not found"))
		return
	case err != nil:
		apierr.Write(w, apierr.Internal("failed to change the token's administration scopes"))
		return
	}
	token, err := h.presentToken(record, false)
	if err != nil {
		apierr.Write(w, apierr.Internal("failed to encode token metadata"))
		return
	}
	writeJSON(w, http.StatusOK, token)
}

// readTokenAdminAccess reads the administration access of the token rows ids.
// A row that is absent reads as the zero value, which presents as none.
func readTokenAdminAccess(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, ids []int32) (map[int32]tokenAdminAccess, error) {
	out := make(map[int32]tokenAdminAccess, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
SELECT token.id, token.admin_access, token.admin_scopes
FROM public.auth_core__token AS token
WHERE token.id = ANY($1::integer[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("read token administration access: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int32
		var access tokenAdminAccess
		if err := rows.Scan(&id, &access.Access, &access.Scopes); err != nil {
			return nil, fmt.Errorf("scan token administration access: %w", err)
		}
		out[id] = access
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read token administration access: %w", err)
	}
	return out, nil
}

// writeTokenAdminAccess stores admin on the token row id, in tx.
func writeTokenAdminAccess(ctx context.Context, tx pgx.Tx, id int32, admin tokenAdminAccess) error {
	admin = admin.normalized()
	tag, err := tx.Exec(ctx, `
UPDATE public.auth_core__token
SET admin_access = $2, admin_scopes = $3::text[]
WHERE id = $1`, id, admin.Access, admin.Scopes)
	if err != nil {
		return fmt.Errorf("write token administration access: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("write token administration access: affected %d rows, want 1", tag.RowsAffected())
	}
	return nil
}

// SetAdminAccess replaces the administration access of an owned personal
// access token. The row is locked, its access read, written and its event
// appended in one transaction; an unchanged access writes nothing and appends
// no event.
func (r *postgresTokenRepository) SetAdminAccess(ctx context.Context, update tokenAdminAccessUpdate) (tokenRecord, error) {
	databaseUserID, err := patDatabaseID(update.OwnerID)
	if err != nil {
		return tokenRecord{}, err
	}
	if update.OnChange == nil {
		return tokenRecord{}, errNoAdminAccessHook
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return tokenRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := r.queries.WithTx(tx)
	locked, err := queries.LockPATByUUID(ctx, update.UUID)
	if errors.Is(err, pgx.ErrNoRows) {
		return tokenRecord{}, errTokenNotFound
	}
	if err != nil {
		return tokenRecord{}, err
	}
	if locked.UserID != databaseUserID {
		return tokenRecord{}, errTokenForbidden
	}
	current, err := readTokenAdminAccess(ctx, tx, []int32{locked.ID})
	if err != nil {
		return tokenRecord{}, err
	}
	before := current[locked.ID].normalized()
	after := update.Admin.normalized()
	if before.Access != after.Access || !slices.Equal(before.Scopes, after.Scopes) {
		if err := writeTokenAdminAccess(ctx, tx, locked.ID, after); err != nil {
			return tokenRecord{}, err
		}
		if err := update.OnChange(ctx, tx, tokenAdminAccessChange{
			TokenID: int64(locked.ID), Before: before, After: after,
		}); err != nil {
			return tokenRecord{}, err
		}
	}
	row, err := queries.GetOwnedPAT(ctx, sqlcgen.GetOwnedPATParams{Uuid: update.UUID, UserID: databaseUserID})
	if err != nil {
		return tokenRecord{}, err
	}
	record, err := patRecord(row.ID, row.Uuid, row.Expires, row.UserID, row.Name, row.ProjectID)
	if err != nil {
		return tokenRecord{}, err
	}
	record.Admin = after
	if err := tx.Commit(ctx); err != nil {
		return tokenRecord{}, err
	}
	return record, nil
}

// WithRecentAuthentication replaces the recent-authentication check of the
// scope rule. Production keeps the zero value, the deployment's limit; a test
// moves the clock.
func WithRecentAuthentication(check apimw.RecentAuthentication) Option {
	return func(handler *Handler) {
		handler.recentAuthentication = check
	}
}

// WithAccessAuditAppender replaces the appender of the admin.access.changed
// events. Production keeps the PostgreSQL appender NewHandler composes.
func WithAccessAuditAppender(appender securityaudit.Appender) Option {
	return func(handler *Handler) {
		if appender != nil {
			handler.accessAudit = appender
		}
	}
}
