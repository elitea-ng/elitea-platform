package admin

// The admin surface of OUTBOUND E-MAIL — the real editor behind the
// Configuration page's "E-mail" section (gap G7).
//
// # Why this is its own surface and not that section made writable
//
// It is the identity provider surface's argument, applied to the same shape of
// problem:
//
//   - the SMTP password is a credential, and `config_values.go`'s
//     `rejectCredentialField` refuses a credential into a
//     `centry.platform_config` row — correctly, because every holder of
//     `runtime.plugins` can read those rows. So the section declares no
//     password field, the generic value endpoints cannot serve this document,
//     and the plaintext is sealed into the GLOBAL vault's hidden bucket
//     instead;
//   - the document has invariants a flat list of field values cannot express.
//     A user name without a password is a session the relay refuses at AUTH,
//     and a host without a sender address is a message that cannot be
//     submitted. The generic form would save both and report success.
//
// # The permission is the one the page it replaces already used
//
// `runtime.plugins`, resolved in administration mode, gates every route here —
// the same string the MCP catalogue and the identity provider surfaces carry,
// and the same one the Configuration page already required. No new permission
// name arrives, so no new grant is needed and the grant gate in
// `internal/api/router_permission_grant_gate_test.go` stays untripped.
//
// # What a caller can and cannot see
//
// A read returns the stored document, the EFFECTIVE document (the stored rows
// laid over the environment defaults), and a per-field tag saying which layer
// decided each value. It never returns the password. The plaintext is written
// once and read back only by the resolver, on its way into one SMTP session;
// the read reports `password_set` instead.
//
// # A save takes effect on the next message
//
// `internal/emailsettings`'s resolver runs per send, so there is nothing to
// invalidate here and no restart to ask for. That is the whole point of the
// surface: the reference's answer to a wrong relay host was a redeploy.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/emailsettings"
)

// EmailSettingsStore is the write seam.
//
// It is an INTERFACE rather than the concrete `*emailsettings.Store` for the
// reason `IdentityProviderStore` states: this handler's contract is the ORDER
// of its refusals — which ones happen before the database is read and before
// the vault is written — and a concrete store over a nil pool can only prove
// the refusals that come first.
type EmailSettingsStore interface {
	// Ready reports whether both the database and the vault are wired.
	Ready() bool
	// Save writes the stored layer. `password` is tri-state: nil leaves the
	// sealed value alone, an empty string clears it, a value re-seals it.
	Save(ctx context.Context, settings emailsettings.Settings, password *string, author string) error
}

// EmailSettingsResolver is the read seam: the merged view of the stored rows
// over the environment defaults, and whether the result can actually send.
type EmailSettingsResolver interface {
	Resolve(ctx context.Context) emailsettings.Resolution
}

// WithEmailSettings supplies the store and the resolver behind
// `/admin/email/administration`.
func WithEmailSettings(store EmailSettingsStore, resolver EmailSettingsResolver) Option {
	return func(h *Handler) {
		// A nil interface is not stored, so an unwired composition root leaves
		// the field nil rather than boxing one. WithIdentityProviders states
		// the same rule and why a typed nil would get past this check.
		if store == nil || resolver == nil {
			return
		}
		h.emailSettings = store
		h.emailResolver = resolver
	}
}

// emailSettingsReady reports whether both dependencies are wired, and answers
// 503 when they are not.
//
// Fail CLOSED, and say what is missing rather than answering an empty
// document: an empty form on a deployment that cannot store what the operator
// types is a save that reports success into a void.
func (h *Handler) emailSettingsReady(w http.ResponseWriter) bool {
	if h.emailSettings != nil && h.emailResolver != nil && h.emailSettings.Ready() {
		return true
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"error": "outbound e-mail settings are not available on this deployment: it has no database or no platform vault",
	})
	return false
}

// emailSettingsBody is the wire shape of a save.
//
// Password is a POINTER so three cases stay distinct: absent (leave the sealed
// password alone), an empty string (clear it), and a value (re-seal it).
// Collapsing absent and empty is how a save from a form that cannot echo a
// credential erases it — see `emailsettings.Store.Save`.
type emailSettingsBody struct {
	Host          string  `json:"host"`
	Port          int     `json:"port"`
	Username      string  `json:"username"`
	Password      *string `json:"password"`
	TLS           string  `json:"tls"`
	From          string  `json:"from"`
	ReplyTo       string  `json:"reply_to"`
	PublicBaseURL string  `json:"public_base_url"`
}

// emailSettingsView is what both verbs answer with. It never carries a
// password.
type emailSettingsView struct {
	// Settings is the STORED layer — what this deployment's administrator has
	// typed. A blank field here is a field the environment may still supply.
	Settings emailsettings.Settings `json:"settings"`
	// Effective is the merged document the next message will actually use.
	Effective emailsettings.Settings `json:"effective"`
	// Sources says, per field, which layer decided the effective value:
	// `database`, `environment` or `unset`.
	Sources map[string]string `json:"sources"`
	// PasswordSet reports that a password is in force. The value is never
	// returned by any route here.
	PasswordSet bool `json:"password_set"`
	// PasswordSource is the layer that supplied it.
	PasswordSource string `json:"password_source"`
	// Configured reports whether a message can be submitted right now — the
	// same fact `invitation_delivered` depends on.
	Configured bool `json:"configured"`
	// Reason says why not, when Configured is false. It names the field to
	// set, because the operator reading it is the one who can set it.
	Reason string `json:"reason,omitempty"`
	Saved  bool   `json:"saved,omitempty"`
}

// EmailSettingsRead serves `GET /admin/email/administration`.
func (h *Handler) EmailSettingsRead(w http.ResponseWriter, r *http.Request) {
	if !h.emailSettingsReady(w) {
		return
	}
	writeJSON(w, http.StatusOK, h.emailSettingsView(r, false))
}

// EmailSettingsSave serves `PUT /admin/email/administration`.
//
// It RE-READS after the write instead of echoing the request. A write that
// silently failed must look different from one that landed, and the response
// is also where the operator learns whether the document is now complete
// enough to send — which the request body cannot say, because completeness is
// decided by the merge with the environment.
func (h *Handler) EmailSettingsSave(w http.ResponseWriter, r *http.Request) {
	if !h.emailSettingsReady(w) {
		return
	}
	var body emailSettingsBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}

	settings := emailsettings.Settings{
		Host:          body.Host,
		Port:          body.Port,
		Username:      body.Username,
		TLS:           body.TLS,
		From:          body.From,
		ReplyTo:       body.ReplyTo,
		PublicBaseURL: body.PublicBaseURL,
	}

	principal, _ := auth.UserFromContext(r.Context())
	author := principal.Email
	if author == "" {
		author = principal.ID
	}

	if err := h.emailSettings.Save(r.Context(), settings, body.Password, author); err != nil {
		writeEmailSettingsError(w, err)
		return
	}
	view := h.emailSettingsView(r, true)
	writeJSON(w, http.StatusOK, view)
}

// emailSettingsView resolves the current state for a response.
func (h *Handler) emailSettingsView(r *http.Request, saved bool) emailSettingsView {
	resolution := h.emailResolver.Resolve(r.Context())
	return emailSettingsView{
		Settings:       resolution.Stored,
		Effective:      resolution.Effective,
		Sources:        emailsettings.Sources(resolution.Stored, resolution.Effective),
		PasswordSet:    resolution.PasswordSet,
		PasswordSource: resolution.PasswordSource,
		Configured:     resolution.Configured,
		Reason:         resolution.Reason,
		Saved:          saved,
	}
}

// writeEmailSettingsError answers 400 and NAMES the field when the refusal was
// about a value, and 503 when the store itself would not answer.
//
// A refusal that does not say which value was wrong sends the operator back to
// a form of eight controls with nothing to go on — the rule
// `writeIdentityProviderValidationError` states for the same reason.
func writeEmailSettingsError(w http.ResponseWriter, err error) {
	var field emailsettings.FieldError
	if errors.As(err, &field) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": field.Reason, "field": field.Field})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"error": "the outbound e-mail settings could not be written",
	})
}

// EmailTestSend serves `POST /admin/email/test/administration`.
//
// It is the SAME send as the Branding page's test button, on this page's own
// permission. Two routes rather than one shared route because the permissions
// differ — `configuration.branding` there, `runtime.plugins` here — and an
// operator who may configure the relay must be able to test the relay without
// also holding the grant that lets them re-brand the product.
func (h *Handler) EmailTestSend(w http.ResponseWriter, r *http.Request) {
	h.sendTestEmail(w, r)
}

// sendTestEmail is the body of both test routes.
func (h *Handler) sendTestEmail(w http.ResponseWriter, r *http.Request) {
	if reason := h.mailUnavailableReason(r); reason != "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": reason})
		return
	}
	var body brandingTestEmailBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	to := strings.TrimSpace(body.To)
	if reason := validateEmailAddress("to", to); reason != "" || to == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "\"to\" must be a plain e-mail address"})
		return
	}
	if err := h.mailer.SendTest(r.Context(), to); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "the mail relay refused the message: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": true, "to": to})
}

// mailUnavailableReason is the empty string when a message can be sent, and
// otherwise the sentence to show the operator.
//
// It prefers the RESOLVER's reason, which names the field that is missing
// ("a sender address is required…"), over the generic one. "Outbound e-mail is
// not configured" is true of every one of those cases and actionable in none
// of them, and the operator reading it is the person who can fix it.
func (h *Handler) mailUnavailableReason(r *http.Request) string {
	if h.mailer == nil || !h.mailer.Configured(r.Context()) {
		if h.emailResolver != nil {
			if reason := h.emailResolver.Resolve(r.Context()).Reason; reason != "" {
				return reason
			}
		}
		return emailsettings.NotConfiguredReason
	}
	return ""
}

// inviteDeliveryUnavailableReason is what an invite response carries in
// `invitation_delivery` when nothing was sent. Same reasoning as above, plus
// the sentence about the record that WAS created, because that is the part of
// the outcome the console is otherwise about to misreport.
func (h *Handler) inviteDeliveryUnavailableReason(r *http.Request) string {
	return "no invitation was delivered: " + h.mailUnavailableReason(r) + ". " + inviteRecordExists
}
