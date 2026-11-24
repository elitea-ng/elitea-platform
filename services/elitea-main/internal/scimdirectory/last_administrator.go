package scimdirectory

// The last-administrator rule on the SCIM deprovisioning path (ADR-0059
// decision 3, "A refused deprovisioning still ends the access"; story R0.11,
// issue 2500).
//
// A SCIM deactivation (PATCH active:false, PUT active:false, a create that
// adopts an account with active:false, DELETE) of the last administrator is
// refused: the deployment would have no administrator left, and only a
// database edit could repair it. But the organisation has said that the
// person left, so the refusal still ends the access. In one transaction:
//
//   - the account stays active and keeps its roles, so the deployment keeps an
//     administrator on record;
//   - its native device sessions and browser sessions are revoked
//     (revokeOnDeactivation) and its personal access tokens deleted, so its
//     session cookie and its token are refused at the next request;
//   - its sealed credentials are revoked (revokeSealedCredentials, a hook
//     until story R2.11, issue 2525, adds the table);
//   - one `critical` security audit event is written, when the refusal ended
//     live access (a retry that finds nothing left to end writes none).
//
// The client gets a SCIM error with status 409 that names the operator
// command for a successor.

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess/rolegrants"
)

// ErrLastAdministrator means the write would have deactivated the last
// administrator. The access was ended anyway; see LastAdministratorError.
var ErrLastAdministrator = errors.New("scimdirectory: the account is the last administrator")

// LastAdministratorError is a refused deprovisioning of the last
// administrator, after its access was ended.
type LastAdministratorError struct {
	UserID int
	// AuditEventID is the id of the critical security audit event; empty
	// when the refusal found no live access left to end (a retry).
	AuditEventID string
}

func (e *LastAdministratorError) Error() string {
	return "scimdirectory: account " + strconv.Itoa(e.UserID) +
		" is the last administrator; it stays active, its sessions and tokens are revoked"
}

// Is makes errors.Is(err, ErrLastAdministrator) true.
func (e *LastAdministratorError) Is(target error) bool { return target == ErrLastAdministrator }

// auditSource is the CloudEvents source of the refusal's event.
const auditSource = "elitea-main/scim"

// refuseLastAdministratorDeactivation runs before a write that deactivates
// `id`, as the first statement of its transaction (it takes LockRoleWrites).
// It returns nil when the write may go on. When the account is the last
// administrator it ends the account's access, COMMITS tx, and returns a
// *LastAdministratorError; the caller returns that error and writes nothing
// else.
func refuseLastAdministratorDeactivation(ctx context.Context, tx pgx.Tx, id int) error {
	guard, err := adminaccess.BeginLastAdministratorGuard(ctx, tx)
	if err != nil {
		return err
	}
	// A platform principal (system@centry.user on a legacy-backed database
	// holds the permission through `system`) is never managed by SCIM:
	// guardTarget refuses it next, and its sessions and tokens are not the
	// directory's to end.
	var email string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(email, '') FROM auth_core__user WHERE id = $1`, id).Scan(&email); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // guardTarget answers ErrNotFound
		}
		return err
	}
	if IsReservedAddress(email) {
		return nil
	}
	refusal, err := guard.CheckWithout(ctx, tx, int64(id))
	if err != nil {
		return err
	}
	if refusal == nil {
		return nil
	}
	// What is still live, before it is ended. An identity provider retries a
	// refused deprovisioning every cycle; a retry that finds nothing left to
	// end writes no second critical event, so the chain records each time
	// access was actually ended rather than each retry.
	var live int64
	if err := tx.QueryRow(ctx, `
SELECT (SELECT count(*) FROM elitea_auth.native_sessions WHERE user_id = $1 AND revoked_at IS NULL)
     + (SELECT count(*) FROM elitea_auth.browser_sessions WHERE user_id = $1 AND revoked_at IS NULL)
     + (SELECT count(*) FROM public.auth_core__token WHERE user_id = $1)`, id).Scan(&live); err != nil {
		return fmt.Errorf("scimdirectory: read the live access of the last administrator: %w", err)
	}
	if err := revokeOnDeactivation(ctx, tx, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM public.auth_core__token WHERE user_id = $1`, id); err != nil {
		return fmt.Errorf("scimdirectory: revoke the tokens of the last administrator: %w", err)
	}
	if err := revokeSealedCredentials(ctx, tx, id); err != nil {
		return err
	}
	refused := &LastAdministratorError{UserID: id}
	if live == 0 {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return refused
	}
	record, err := adminaccess.AppendAccessEvent(ctx, tx, auditSource, adminaccess.AccessEvent{
		Action:        adminaccess.AccessActionDeprovisionRefused,
		Source:        rolegrants.SourceSCIM,
		PrincipalRef:  "scim",
		SubjectUserID: int64(id),
		Severity:      adminaccess.SeverityCritical,
		Reasons: []string{
			"code:" + adminaccess.CodeLastAdministrator,
			"sessions:revoked", "tokens:revoked", "account:kept_active",
		},
	})
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	refused.AuditEventID = record.EventID
	return refused
}

// revokeSealedCredentials is the hook for the sealed credentials of story
// R2.6 (issue 2520): story R2.11 (issue 2525) revokes the person's sealed
// credentials here, in the same transaction, once the table exists.
func revokeSealedCredentials(context.Context, pgx.Tx, int) error { return nil }
