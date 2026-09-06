package admin

// The admin GLOBAL invite — `POST /admin/user_invite/administration`.
//
// This is the platform-level counterpart of the project invite that already
// exists (`POST /admin/users/{mode}/{projectID}`,
// internal/api/v2/eliteacore/users_write.go): it creates a platform user record
// for an address that has never logged in, with no project and no role, so the
// admin console can then place them.
//
// Legacy: legacy/plugins/admin/api/v2/user_invite.py. Its handler does four
// things, of which this service can do one and a half:
//
//  1. `auth_cirro_invite(email, name)` — asks the deployment's EXTERNAL identity
//     service to create an invitation and mail the address a token. That is an
//     IdP integration, not a platform table; nothing in this service speaks it,
//     and AGENTS.md does not carry it forward.
//  2. create the `auth_core__user` row if the address has none. Done here.
//  3. `auth_add_user_group(user_id, 1)` — link the new user to the root group.
//     SKIPPED, deliberately: this service's RBAC never reads
//     auth_core__group/…__user_group (internal/infra/legacyrbac/postgres.go
//     resolves from user_role and project_user_role only), the tables are not
//     part of its bootstrap schema, and the already-shipped project invite path
//     does not write them either. Recorded here rather than done silently.
//  4. `auth_add_user_provider(user_id, "cirro:invite:token:<token>")` — bind the
//     invitation token as a provider identity. There is no token without (1),
//     and writing a provider_ref the IdP has not asserted is what the project
//     invite path already refuses to do (see UsersCreate's doc comment): the
//     OIDC provisioning path resolves an existing row BY EMAIL and links the
//     real provider on first login
//     (internal/db/queries/auth_provisioning.sql).
//
// So the invited user can log in and land on their record — that is what makes
// the row worth creating — but no invitation is DELIVERED by this service. The
// response says so in a field rather than answering a bare `{"ok": true}`,
// because "invite sent" is precisely the success a console would report from
// that, and nothing was sent.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"strings"

	"github.com/jackc/pgx/v5"

	appmailer "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/mailer"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// inviteRecordExists is the half of an undelivered-invite explanation that is
// about the RECORD rather than about the relay.
//
// It is a separate constant because the relay half is no longer one sentence:
// since gap G7 the mail configuration is resolvable at runtime, so the reason
// names the field that is missing and where to set it
// (`emailsettings.Resolution.Reason`, assembled in email.go). This part stays
// constant, because it is true of every one of those cases.
const inviteRecordExists = "The platform user record exists, so the address can be granted roles " +
	"now and is resolved by email on first login."

// UserInvite serves `POST /admin/user_invite/administration`.
func (h *Handler) UserInvite(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "database unavailable"})
		return
	}

	var body struct {
		UserName  string `json:"user_name"`
		UserEmail string `json:"user_email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request body"})
		return
	}

	name := strings.TrimSpace(body.UserName)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Name not set"})
		return
	}
	email := normalizeInviteEmail(body.UserEmail)
	if email == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Email not set"})
		return
	}
	if !validInviteEmail(email) {
		writeJSON(w, http.StatusBadRequest,
			map[string]any{"ok": false, "error": "Invalid email: " + strings.TrimSpace(body.UserEmail)})
		return
	}

	ctx := r.Context()
	var userID int
	created := false
	err := h.pool.QueryRow(ctx,
		`SELECT id FROM public.auth_core__user WHERE lower(email) = $1`, email).Scan(&userID)
	switch {
	case err == nil:
	case errors.Is(err, pgx.ErrNoRows):
		// The name is only applied at CREATION. Overwriting an existing user's
		// name from an invite form would let this endpoint rename accounts,
		// which is not what "invite" means and is not what pylon does.
		if err := h.pool.QueryRow(ctx,
			`INSERT INTO public.auth_core__user (email, name) VALUES ($1, $2) RETURNING id`,
			email, name).Scan(&userID); err != nil {
			writeJSON(w, http.StatusInternalServerError,
				map[string]any{"ok": false, "error": "failed to create the user record"})
			return
		}
		created = true
	default:
		writeJSON(w, http.StatusInternalServerError,
			map[string]any{"ok": false, "error": "failed to look the address up"})
		return
	}

	// Delivery (ADR-0024 WP7). The record exists either way; what the
	// response says about the e-mail is what actually happened to it.
	delivered, delivery := h.deliverInvitation(r, appmailer.Invitation{
		Email: email, Name: name, InvitedBy: inviterName(r),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                   true,
		"id":                   userID,
		"email":                email,
		"name":                 name,
		"created":              created,
		"invitation_delivered": delivered,
		"invitation_delivery":  delivery,
	})
}

// deliverInvitation sends the invitation when a mailer is wired and reports
// what happened in the two response fields. A relay failure is reported, not
// swallowed and not fatal: the record exists and the operator can resend.
func (h *Handler) deliverInvitation(r *http.Request, invitation appmailer.Invitation) (bool, string) {
	if h.mailer == nil || !h.mailer.Configured(r.Context()) {
		return false, h.inviteDeliveryUnavailableReason(r)
	}
	if err := h.mailer.SendInvitation(r.Context(), invitation); err != nil {
		return false, "the invitation could not be sent: " + err.Error()
	}
	return true, "an invitation e-mail was sent to " + invitation.Email
}

// inviterName is the caller's e-mail (the name field is never populated on
// this platform's principals), or empty.
func inviterName(r *http.Request) string {
	principal, ok := auth.UserFromContext(r.Context())
	if !ok {
		return ""
	}
	return principal.Email
}

func normalizeInviteEmail(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// validInviteEmail is the same backstop the project invite applies (the
// unexported `validEmail` in internal/api/v2/eliteacore/users_write.go, copied
// rather than exported to keep that package's surface unchanged): net/mail
// accepts display-name forms like `A <a@b.c>` that are not what this endpoint
// means by an address, so the parse result must round-trip to the input.
func validInviteEmail(address string) bool {
	parsed, err := mail.ParseAddress(address)
	if err != nil || parsed.Address != address {
		return false
	}
	at := strings.LastIndex(address, "@")
	return at > 0 && strings.Contains(address[at+1:], ".")
}
