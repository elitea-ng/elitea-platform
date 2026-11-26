package eliteacore

// The admin.access.changed events of the central project-member routes
// (ADR-0059 R1.5 follow-up, issue 2994). A membership written through the
// administration routes of /admin/users/administration/{projectID} (invite,
// role change, removal) is a project_membership event in the write's own
// transaction, with the facts R1.6 rates it by: a failed append fails the
// write. The project-gated `{mode}` routes are a member's own project
// administration, not a central route, and append none.

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit/accessevent"
)

const accessEventSource = "elitea-main/eliteacore"

// WithAccessAuditAppender replaces the appender of the central member
// routes' events. Production keeps the PostgreSQL appender; a test passes a
// failing one to show that the write rolls back.
func WithAccessAuditAppender(appender securityaudit.Appender) Option {
	return func(handler *Handler) {
		if appender != nil {
			handler.accessAudit = appender
		}
	}
}

func (h *Handler) accessAppender() securityaudit.Appender {
	if h.accessAudit != nil {
		return h.accessAudit
	}
	return accessevent.DefaultAppender(accessEventSource)
}

// errMembershipEvent marks a write that failed on its security audit event.
var errMembershipEvent = errors.New("eliteacore: the membership security audit event could not be appended")

// centralMembership records the memberships one central write touches: the
// roles each person held in the project before the write, read in its
// transaction, so the event after it names the change. A `{mode}` write
// carries none and appends nothing.
type centralMembership struct {
	central   bool
	projectID int64
	bulk      bool
	before    map[int64][]string
	// given are the project roles each person was given provisionally under
	// the caller's elevation (ADR-0059 R2.12b): their event has the source
	// `provisional`.
	given map[int64][]int
	// caller is the elevated caller of a provisional write.
	caller adminaccess.Caller
}

// gave records the roles caller gave a person provisionally.
func (m *centralMembership) gave(caller adminaccess.Caller, userID int, roleIDs []int) {
	if len(roleIDs) == 0 {
		return
	}
	m.caller = caller
	if m.given == nil {
		m.given = map[int64][]int{}
	}
	m.given[int64(userID)] = append(m.given[int64(userID)], roleIDs...)
}

// beginCentralMembership reads, in tx, the roles of userIDs in the project
// before a write through mode. bulk is whether the request writes more than
// one membership.
func beginCentralMembership(
	ctx context.Context, tx pgx.Tx, mode string, projectID int, userIDs []int, bulk bool,
) (*centralMembership, error) {
	m := &centralMembership{central: mode == userWriteModeAdministration, projectID: int64(projectID), bulk: bulk}
	if !m.central {
		return m, nil
	}
	m.before = make(map[int64][]string, len(userIDs))
	for _, userID := range userIDs {
		roles, err := accessevent.ProjectMemberRoles(ctx, tx, m.projectID, int64(userID))
		if err != nil {
			return nil, err
		}
		m.before[int64(userID)] = roles
	}
	return m, nil
}

// appendChanges appends one project_membership event per person whose roles
// in the project the write changed, in request order, in tx.
func (h *Handler) appendChanges(ctx context.Context, tx pgx.Tx, r *http.Request, m *centralMembership, userIDs []int) error {
	if m == nil || !m.central {
		return nil
	}
	for _, userID := range userIDs {
		if err := accessevent.AppendMembershipChange(ctx, tx, h.accessAppender(), r.Context(),
			m.projectID, int64(userID), m.before[int64(userID)], m.bulk); err != nil {
			return errors.Join(errMembershipEvent, err)
		}
		if err := accessevent.AppendProvisionalMembership(ctx, tx, h.accessAppender(), r.Context(),
			m.caller, m.projectID, int64(userID), m.given[int64(userID)], m.bulk); err != nil {
			return errors.Join(errMembershipEvent, err)
		}
	}
	return nil
}
