package admin

// The admin.access.changed security audit events of the role write paths
// (ADR-0059 decision 8, "Security audit", story R1.5, issue 2509). Each write
// appends its event in its own transaction, through the handler's appender,
// so an append that fails rolls the change back: a change of who holds what
// does not happen without its evidence.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit/accessevent"
)

// accessEventSource is the CloudEvents source of the admin routes' events.
const accessEventSource = "elitea-main/admin"

// WithAccessAuditAppender replaces the appender of the admin.access.changed
// events. Production keeps the default PostgreSQL appender; a test passes a
// failing one to show that the change rolls back.
func WithAccessAuditAppender(appender securityaudit.Appender) Option {
	return func(h *Handler) {
		if appender != nil {
			h.accessAudit = appender
		}
	}
}

// defaultAccessAppender is the PostgreSQL appender every handler gets unless
// a test replaces it.
func defaultAccessAppender() securityaudit.Appender {
	return accessevent.DefaultAppender(accessEventSource)
}

// errNoAccessAppender refuses a role write on a handler built with no
// appender: it never happens through NewHandler.
var errNoAccessAppender = errors.New("admin: no security audit appender")

// appendAccessChange appends one admin.access.changed event in tx. The actor,
// the credential reference, the elevation and the trace come from the
// request; the caller states the subject and the change.
func (h *Handler) appendAccessChange(ctx context.Context, tx pgx.Tx, r *http.Request, change securityaudit.AdminAccessChanged) error {
	if h.accessAudit == nil {
		return errNoAccessAppender
	}
	return h.accessAudit.Append(ctx, tx, securityaudit.NewAdminAccessChanged(accessevent.FromRequest(r.Context(), change)))
}

// accessChangeHook is the change hook of a write whose transaction a store
// owns (the identity provider and SCIM client stores): the store runs it with
// the row it wrote, and the event commits with the row.
func (h *Handler) accessChangeHook(r *http.Request, build func(subjectID string) securityaudit.AdminAccessChanged) *http.Request {
	requestContext := r.Context()
	hook := func(ctx context.Context, tx pgx.Tx, subjectID string) error {
		if h.accessAudit == nil {
			return errNoAccessAppender
		}
		change := accessevent.FromRequest(requestContext, build(subjectID))
		return h.accessAudit.Append(ctx, tx, securityaudit.NewAdminAccessChanged(change))
	}
	return r.WithContext(securityaudit.ContextWithChangeHook(requestContext, hook))
}

// identityTrustChange is the identity_trust event of a trust change to
// subject (identity_provider/<key> or scim_client/<id>): fields names what
// changed, never a value.
func identityTrustChange(subjectType string, fields []string) func(string) securityaudit.AdminAccessChanged {
	return func(subjectID string) securityaudit.AdminAccessChanged {
		return securityaudit.AdminAccessChanged{
			SubjectType: subjectType,
			SubjectID:   subjectID,
			Change: securityaudit.AccessChange{
				Kind:   securityaudit.AccessIdentityTrust,
				Fields: sortedSet(fields),
			},
		}
	}
}

// requireChangeHookRan logs a store write that committed without running the
// change hook of its request: the change happened without its security audit
// event. It cannot be undone here; the DB integration tests of each route
// assert the event, so this is the production tripwire.
func requireChangeHookRan(r *http.Request, route string) {
	if !securityaudit.ChangeHookRan(r.Context()) {
		slog.ErrorContext(r.Context(), "admin: a trust change committed without its security audit event", "route", route)
	}
}

// appendRoleAssignment appends the role_assignment event of a change to the
// user's roles in mode, when the change changed anything: before is the set
// the write read under the role-writes lock, and the set after is read here,
// in the same transaction. source is the grant history source.
func (h *Handler) appendRoleAssignment(
	ctx context.Context, tx pgx.Tx, r *http.Request,
	actorUserID int64, userID int, mode string, before []string, source string,
) error {
	after, err := adminaccess.UserCentralRoles(ctx, tx, userID, mode)
	if err != nil {
		return err
	}
	before = sortedSet(before)
	after = sortedSet(after)
	if slices.Equal(before, after) {
		return nil
	}
	return h.appendAccessChange(ctx, tx, r, securityaudit.AdminAccessChanged{
		ActorUserID: actorUserID,
		SubjectType: "user",
		SubjectID:   strconv.Itoa(userID),
		Change: securityaudit.AccessChange{
			Kind: securityaudit.AccessRoleAssignment, Mode: mode, Source: source,
			Before: before, After: after,
		},
	})
}

// sortedSet is a sorted copy of names without duplicates or empty names,
// never nil.
func sortedSet(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if name != "" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// projectMemberRoles is the person's roles in the project, by name.
func projectMemberRoles(ctx context.Context, q adminaccess.Querier, projectID, userID int64) ([]string, error) {
	rows, err := q.Query(ctx, `
SELECT DISTINCT role.name
FROM public.auth_core__project_user_role assignment
JOIN public.auth_core__project_role role ON role.id = assignment.role_id
WHERE assignment.project_id = $1 AND assignment.user_id = $2
ORDER BY 1`, projectID, userID)
	if err != nil {
		return nil, fmt.Errorf("read the project roles: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the project roles: %w", err)
	}
	return names, nil
}

// appendMembershipChange appends the project_membership event of a
// membership written through a central route, with the three facts R1.6
// rates it by: whether the actor is a member of the project, whether the new
// member holds an administration role, and whether the write is a bulk
// invite. The partition is the project's.
func (h *Handler) appendMembershipChange(
	ctx context.Context, tx pgx.Tx, r *http.Request, projectID, userID int64, before []string, bulk bool,
) error {
	after, err := projectMemberRoles(ctx, tx, projectID, userID)
	if err != nil {
		return err
	}
	if slices.Equal(sortedSet(before), sortedSet(after)) {
		return nil
	}
	facts := securityaudit.MembershipFacts{Bulk: bulk}
	if principal, ok := auth.UserFromContext(r.Context()); ok {
		if actor, resolved := principal.OwningUserID(); resolved && actor > 0 {
			var member bool
			if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM public.auth_core__project_user_role
               WHERE project_id = $1 AND user_id = $2)`, projectID, actor).Scan(&member); err != nil {
				return fmt.Errorf("read the actor's membership: %w", err)
			}
			facts.ActorIsMember = member
		}
	}
	held, err := adminaccess.UserCentralRoles(ctx, tx, int(userID), adminaccess.ModeAdministration)
	if err != nil {
		return err
	}
	facts.MemberHoldsAdminRole = len(held) > 0
	return h.appendAccessChange(ctx, tx, r, securityaudit.AdminAccessChanged{
		SubjectType:     "user",
		SubjectID:       strconv.FormatInt(userID, 10),
		TenantProjectID: projectID,
		Change: securityaudit.AccessChange{
			Kind:       securityaudit.AccessProjectMembership,
			Before:     sortedSet(before),
			After:      sortedSet(after),
			Membership: &facts,
		},
	})
}
