package adminaccess

// The sealed credential list for the console (ADR-0059 decision 2, the row
// `access.breakglass.manage`, and decision 5; story R2.17, issue 2610):
//
//	GET /api/v2/admin/access/sealed_credentials
//
// METADATA ONLY. A row is the credential's id, label, level, the person it
// is bound to (or the reserved break-glass account), its times, its state and
// whether it is split. Never the selector, the verifier, the digest or
// anything derived from them: the projection is registry.ListSealedCredentials,
// the one the operator command `break-glass list` reads, whose SELECT list
// never names the digest, and the wire shape below has no field for the
// selector.
//
// The list names the people with emergency access, so it follows the rule of
// decision 6: only a holder of `access.breakglass.manage` reads it (the route
// gate), an elevation never does and a token never does (the action table:
// `elevation: deny`, `token: never`, applied by the middlewares of
// internal/api). Every read writes the decision event the command writes
// (registry.SealedCredentialListAction), with the source `console`, in the
// transaction of the read.
//
// Issue and revoke are NOT here, and never will be: they stay process
// commands.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	registry "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// SealedCredentialsPath is the route, relative to the /api/v2/admin group.
const SealedCredentialsPath = "/access/sealed_credentials"

// BoundAccount is the account a sealed credential signs in as.
type BoundAccount struct {
	ID    int64
	Name  string
	Email string
	// Reserved: the reserved break-glass account (an unbound credential).
	Reserved bool
}

// ListedSealedCredential is one credential of the list with its account.
type ListedSealedCredential struct {
	registry.SealedCredential
	// Account is the bound account, zero when its row no longer exists
	// (0271 keeps no foreign key). Unbound means "bound to the reserved
	// account", which the credential does not record on its own: a credential
	// whose reserved account row was deleted reads as bound, to the id it
	// names, with no name or e-mail.
	Account BoundAccount
}

// SealedCredentialLister reads the list and records the read.
// *PostgresSealedCredentials is the production one.
type SealedCredentialLister interface {
	ListSealedCredentials(ctx context.Context, actorUserID int64) ([]ListedSealedCredential, error)
}

// PostgresSealedCredentials lists the credentials of
// elitea_auth.sealed_credentials (shared 0271) through
// registry.ListSealedCredentials, in one transaction with its audit event.
type PostgresSealedCredentials struct {
	pool     *pgxpool.Pool
	appender securityaudit.Appender
}

// NewPostgresSealedCredentials returns the lister. A nil appender fails every
// read: the read is not served without its record.
func NewPostgresSealedCredentials(pool *pgxpool.Pool, appender securityaudit.Appender) *PostgresSealedCredentials {
	return &PostgresSealedCredentials{pool: pool, appender: appender}
}

// ListSealedCredentials implements SealedCredentialLister.
func (p *PostgresSealedCredentials) ListSealedCredentials(ctx context.Context, actorUserID int64) ([]ListedSealedCredential, error) {
	var listed []ListedSealedCredential
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		credentials, err := registry.ListSealedCredentials(ctx, tx, p.appender, registry.SealedList{
			Source:      securityaudit.ElevationSourceConsole,
			ActorUserID: actorUserID,
		})
		if err != nil {
			return err
		}
		accounts, err := boundAccounts(ctx, tx, credentials)
		if err != nil {
			return err
		}
		listed = make([]ListedSealedCredential, 0, len(credentials))
		for _, credential := range credentials {
			listed = append(listed, ListedSealedCredential{SealedCredential: credential, Account: accounts[credential.UserID]})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return listed, nil
}

// boundAccounts reads the accounts the credentials are bound to, by id.
func boundAccounts(ctx context.Context, tx pgx.Tx, credentials []registry.SealedCredential) (map[int64]BoundAccount, error) {
	ids := make([]int64, 0, len(credentials))
	for _, credential := range credentials {
		ids = append(ids, credential.UserID)
	}
	accounts := map[int64]BoundAccount{}
	if len(ids) == 0 {
		return accounts, nil
	}
	rows, err := tx.Query(ctx, `
SELECT id, COALESCE(name, ''), COALESCE(email, '')
FROM public.auth_core__user WHERE id = ANY($1::bigint[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("read the bound accounts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var account BoundAccount
		if err := rows.Scan(&account.ID, &account.Name, &account.Email); err != nil {
			return nil, fmt.Errorf("read the bound accounts: %w", err)
		}
		account.Reserved = registry.IsReservedBreakGlassEmail(account.Email)
		accounts[account.ID] = account
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the bound accounts: %w", err)
	}
	return accounts, nil
}

// SealedCredentialsHandler serves the route.
type SealedCredentialsHandler struct {
	lister SealedCredentialLister
	now    func() time.Time
}

// NewSealedCredentialsHandler returns the handler. A nil lister answers 503.
func NewSealedCredentialsHandler(lister SealedCredentialLister) *SealedCredentialsHandler {
	return &SealedCredentialsHandler{lister: lister, now: time.Now}
}

// WithClock replaces the clock the states are computed at (tests).
func (h *SealedCredentialsHandler) WithClock(now func() time.Time) *SealedCredentialsHandler {
	if now != nil {
		h.now = now
	}
	return h
}

// Register mounts the route on the /api/v2/admin router, behind middlewares
// (the route gate on access.breakglass.manage).
func (h *SealedCredentialsHandler) Register(r chi.Router, middlewares ...func(http.Handler) http.Handler) {
	r.With(middlewares...).Get(SealedCredentialsPath, h.List)
}

/* ── the wire shape (v2.yaml SealedCredentialList) ─────────────────────── */

type sealedCredentialsBody struct {
	Total int                    `json:"total"`
	Rows  []sealedCredentialBody `json:"rows"`
}

// sealedCredentialBody has NO field for the selector, the verifier or the
// digest: nothing derived from the verifier can be written by mistake.
type sealedCredentialBody struct {
	ID        string           `json:"id"`
	Label     string           `json:"label"`
	Level     string           `json:"level"`
	State     string           `json:"state"`
	Split     bool             `json:"split"`
	Unbound   bool             `json:"unbound"`
	BoundUser boundAccountBody `json:"bound_user"`
	IssuedAt  time.Time        `json:"issued_at"`
	ExpiresAt time.Time        `json:"expires_at"`
	UsedAt    *time.Time       `json:"used_at"`
	RevokedAt *time.Time       `json:"revoked_at"`
}

type boundAccountBody struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	value := t.UTC()
	return &value
}

// List answers the credentials, newest first.
func (h *SealedCredentialsHandler) List(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		apierr.WriteStatus(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if h.lister == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "the sealed credential list is not configured")
		return
	}
	// The read is recorded under the person who made it: a principal with no
	// owning account (none reaches here past the token rule, which refuses
	// every token) is refused rather than served under an empty record.
	actor, ok := user.OwningUserID()
	if !ok || actor <= 0 {
		apierr.WriteStatus(w, http.StatusUnauthorized, "authentication required")
		return
	}
	credentials, err := h.lister.ListSealedCredentials(r.Context(), actor)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled) {
			return
		}
		slog.ErrorContext(r.Context(), "admin access: the sealed credential list could not be read", "error", err)
		apierr.WriteStatus(w, http.StatusInternalServerError, "the sealed credential list could not be read")
		return
	}
	now := h.now()
	body := sealedCredentialsBody{Total: len(credentials), Rows: make([]sealedCredentialBody, 0, len(credentials))}
	for _, credential := range credentials {
		body.Rows = append(body.Rows, sealedCredentialBody{
			ID:      credential.ID.String(),
			Label:   credential.Label,
			Level:   credential.Level,
			State:   credential.State(now),
			Split:   credential.Split,
			Unbound: credential.Account.Reserved,
			BoundUser: boundAccountBody{
				ID: credential.UserID, Name: credential.Account.Name, Email: credential.Account.Email,
			},
			IssuedAt:  credential.IssuedAt.UTC(),
			ExpiresAt: credential.ExpiresAt.UTC(),
			UsedAt:    optionalTime(credential.UsedAt),
			RevokedAt: optionalTime(credential.RevokedAt),
		})
	}
	writeJSON(w, body)
}
