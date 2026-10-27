package delegationstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/obo"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const (
	maxAssertionBytes = 16 << 10
	maxScopes         = 64
	maxTextBytes      = 512
	// maxAssertionLifetime bounds what a sign-in may retain. Entra access
	// tokens live 60-90 minutes; CAE-capable ones up to 28 hours.
	maxAssertionLifetime = 30 * time.Hour
)

var (
	// ErrInvalid refuses a request before any statement runs.
	ErrInvalid = errors.New("delegationstore: invalid request")
	// ErrNoSignInAssertion means the session holds no usable sign-in
	// assertion for the audience. The user must sign in again.
	ErrNoSignInAssertion = errors.New("delegationstore: no usable sign-in assertion")
	// ErrDelegationNotFound is an unknown delegation, or one outside the
	// caller's tenant project and binding.
	ErrDelegationNotFound = errors.New("delegationstore: delegation not found")
	// ErrDelegationNotUsable is a revoked, suspended, expired or wiped
	// delegation.
	ErrDelegationNotUsable = errors.New("delegationstore: delegation not usable")

	// ErrBindingNotActive refuses a delegation for an agent binding that is
	// not active in the delegation's tenant project.
	ErrBindingNotActive = errors.New("delegationstore: agent binding is not active in the tenant project")
)

// Store is the PostgreSQL custody store.
type Store struct {
	pool   *pgxpool.Pool
	sealer *sealer
	audit  securityaudit.Appender
	now    func() time.Time
}

// New builds the store. Every change appends a security audit event in its
// transaction; a nil audit refuses construction.
func New(pool *pgxpool.Pool, masterKey []byte, audit securityaudit.Appender) (*Store, error) {
	if pool == nil || audit == nil {
		return nil, errors.New("delegationstore: a database pool and a security audit appender are required")
	}
	sealer, err := newSealer(masterKey)
	if err != nil {
		return nil, err
	}
	return &Store{pool: pool, sealer: sealer, audit: audit, now: time.Now}, nil
}

// SignIn is a verified middle-tier assertion retained at sign-in.
type SignIn struct {
	// SessionID is the browser session id the sign-in created. Only its
	// digest is stored.
	SessionID string
	UserID    int64
	Issuer    string
	TenantID  string
	ObjectID  string
	Audience  string
	Scopes    []string
	Token     string
	ExpiresAt time.Time
}

func (s SignIn) valid(now time.Time) bool {
	return s.SessionID != "" && len(s.SessionID) <= maxTextBytes && s.UserID > 0 &&
		validText(s.Issuer) && obo.ValidTenantID(s.TenantID) && obo.ValidTenantID(s.ObjectID) &&
		validText(s.Audience) && validScopes(s.Scopes) && s.Token != "" && len(s.Token) <= maxAssertionBytes &&
		!strings.ContainsFunc(s.Token, unicode.IsControl) &&
		s.ExpiresAt.After(now) && !s.ExpiresAt.After(now.Add(maxAssertionLifetime))
}

// RetainSignIn seals the assertion for the session and returns its id.
func (s *Store) RetainSignIn(ctx context.Context, signIn SignIn) (string, error) {
	now := s.now().UTC()
	if !signIn.valid(now) {
		return "", ErrInvalid
	}
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("delegationstore: assertion id: %w", err)
	}
	expires := signIn.ExpiresAt.UTC().Truncate(time.Microsecond)
	digest := SessionDigest(signIn.SessionID)
	ciphertext := s.sealer.seal(signIn.Token, SignInAAD{
		AssertionID: id.String(), SessionDigest: digest, UserID: signIn.UserID,
		TenantID: signIn.TenantID, ObjectID: signIn.ObjectID, Audience: signIn.Audience, ExpiresAt: expires,
	})
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
INSERT INTO elitea_identity.signin_assertions
    (id, browser_session_digest, user_id, issuer, tenant_id, subject_object_id, audience, scopes,
     assertion_ciphertext, assertion_expires_at, key_label)
VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			id.String(), digest[:], signIn.UserID, signIn.Issuer, signIn.TenantID, signIn.ObjectID,
			signIn.Audience, signIn.Scopes, ciphertext, expires, KeyLabel); err != nil {
			return fmt.Errorf("delegationstore: store the sign-in assertion: %w", err)
		}
		return s.audit.Append(ctx, tx, securityaudit.SecurityEvent{
			Type: securityaudit.TypeDelegationChanged, Time: now, Action: "signin_assertion.retain",
			Reasons:      []string{"state.active"},
			PrincipalRef: userRef(signIn.UserID), ResourceType: "signin_assertion", ResourceID: id.String(),
		})
	})
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// DelegateRequest names the run that needs interactive delegation.
type DelegateRequest struct {
	SessionID       string
	UserID          int64
	TenantProjectID int64
	AgentBindingID  string
	// Audience selects the sign-in assertion: the middle tier for classic
	// OBO, the blueprint for Agent ID OBO.
	Audience string
	Scopes   []string
}

// Delegation is the metadata of one delegation. It never holds the token.
type Delegation struct {
	ID              string
	TenantProjectID int64
	AgentBindingID  string
	TenantID        string
	ObjectID        string
	Audience        string
	Scopes          []string
	ExpiresAt       time.Time
	RevocationEpoch int64
}

// Delegate creates a delegation from the session's sign-in assertion.
func (s *Store) Delegate(ctx context.Context, r DelegateRequest) (Delegation, error) {
	if r.SessionID == "" || r.UserID <= 0 || r.TenantProjectID <= 0 || !isUUID(r.AgentBindingID) ||
		!validText(r.Audience) || !validScopes(r.Scopes) {
		return Delegation{}, ErrInvalid
	}
	now := s.now().UTC()
	digest := SessionDigest(r.SessionID)
	id, err := uuid.NewV7()
	if err != nil {
		return Delegation{}, fmt.Errorf("delegationstore: delegation id: %w", err)
	}
	var delegation Delegation
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// The binding must be live in this tenant project, held for the
		// transaction so a concurrent revocation orders after this row.
		var bindingActive bool
		err := tx.QueryRow(ctx, `
SELECT state = 'active'
FROM elitea_identity.agent_identity_bindings
WHERE id = $1::uuid AND tenant_project_id = $2
FOR SHARE`, r.AgentBindingID, r.TenantProjectID).Scan(&bindingActive)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !bindingActive) {
			return ErrBindingNotActive
		}
		if err != nil {
			return fmt.Errorf("delegationstore: read the agent binding: %w", err)
		}
		var (
			assertionID, issuer, tid, oid string
			ciphertext                    []byte
			expires, consented            time.Time
		)
		err = tx.QueryRow(ctx, `
SELECT id::text, issuer, tenant_id, subject_object_id, assertion_ciphertext, assertion_expires_at, created_at
FROM elitea_identity.signin_assertions
WHERE browser_session_digest = $1 AND user_id = $2 AND audience = $3
  AND state = 'active' AND assertion_expires_at > $4
ORDER BY created_at DESC LIMIT 1
FOR SHARE`, digest[:], r.UserID, r.Audience, now).
			Scan(&assertionID, &issuer, &tid, &oid, &ciphertext, &expires, &consented)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoSignInAssertion
		}
		if err != nil {
			return fmt.Errorf("delegationstore: read the sign-in assertion: %w", err)
		}
		expires = expires.UTC()
		token, err := s.sealer.open(ciphertext, SignInAAD{
			AssertionID: assertionID, SessionDigest: digest, UserID: r.UserID,
			TenantID: tid, ObjectID: oid, Audience: r.Audience, ExpiresAt: expires,
		})
		if err != nil {
			return err
		}
		delegation = Delegation{
			ID: id.String(), TenantProjectID: r.TenantProjectID, AgentBindingID: r.AgentBindingID,
			TenantID: tid, ObjectID: oid, Audience: r.Audience, Scopes: slices.Clone(r.Scopes), ExpiresAt: expires,
		}
		sealed := s.sealer.seal(token, delegation.aad())
		if _, err := tx.Exec(ctx, `
INSERT INTO elitea_identity.delegations
    (id, tenant_project_id, issuer, tenant_id, subject_object_id, agent_binding_id, audiences, scopes,
     consent_recorded_at, expires_at, state, assertion_ciphertext, assertion_expires_at,
     assertion_audience, key_label, browser_session_digest)
VALUES ($1::uuid, $2, $3, $4, $5, $6::uuid, ARRAY[$7::text], $8, $9, $10, 'active', $11, $10, $7, $12, $13)`,
			delegation.ID, r.TenantProjectID, issuer, tid, oid, r.AgentBindingID, r.Audience, r.Scopes,
			consented, expires, sealed, KeyLabel, digest[:]); err != nil {
			return fmt.Errorf("delegationstore: store the delegation: %w", err)
		}
		return s.audit.Append(ctx, tx, delegation.event(now, "delegation.create", "state.active", userRef(r.UserID)))
	})
	if err != nil {
		return Delegation{}, err
	}
	return delegation, nil
}

// LoadRequest scopes a Load to the caller's tenant project and binding.
type LoadRequest struct {
	DelegationID    string
	TenantProjectID int64
	AgentBindingID  string
}

// Load opens the assertion of an active delegation.
func (s *Store) Load(ctx context.Context, r LoadRequest) (obo.Assertion, Delegation, error) {
	if !isUUID(r.DelegationID) || r.TenantProjectID <= 0 || !isUUID(r.AgentBindingID) {
		return obo.Assertion{}, Delegation{}, ErrInvalid
	}
	var (
		d          Delegation
		state      string
		ciphertext []byte
		expires    *time.Time
		audience   *string
	)
	err := s.pool.QueryRow(ctx, `
SELECT id::text, tenant_project_id, agent_binding_id::text, tenant_id, subject_object_id, scopes,
       expires_at, revocation_epoch, state, assertion_ciphertext, assertion_expires_at, assertion_audience
FROM elitea_identity.delegations
WHERE id = $1::uuid AND tenant_project_id = $2 AND agent_binding_id = $3::uuid`,
		r.DelegationID, r.TenantProjectID, r.AgentBindingID).
		Scan(&d.ID, &d.TenantProjectID, &d.AgentBindingID, &d.TenantID, &d.ObjectID, &d.Scopes,
			&d.ExpiresAt, &d.RevocationEpoch, &state, &ciphertext, &expires, &audience)
	if errors.Is(err, pgx.ErrNoRows) {
		return obo.Assertion{}, Delegation{}, ErrDelegationNotFound
	}
	if err != nil {
		return obo.Assertion{}, Delegation{}, fmt.Errorf("delegationstore: read the delegation: %w", err)
	}
	now := s.now()
	if state != "active" || len(ciphertext) == 0 || expires == nil || audience == nil || !expires.After(now) {
		return obo.Assertion{}, Delegation{}, ErrDelegationNotUsable
	}
	d.Audience, d.ExpiresAt = *audience, expires.UTC()
	token, err := s.sealer.open(ciphertext, d.aad())
	if err != nil {
		return obo.Assertion{}, Delegation{}, err
	}
	return obo.Assertion{
		Token: token, TenantID: d.TenantID, ObjectID: d.ObjectID, Audience: d.Audience, ExpiresAt: d.ExpiresAt,
	}, d, nil
}

// Revoke revokes one delegation: state 'revoked', ciphertext cleared and
// epoch bumped in one transaction. Revoking a revoked delegation returns its
// epoch unchanged.
func (s *Store) Revoke(ctx context.Context, delegationID, actor string) (int64, error) {
	if !isUUID(delegationID) || !validText(actor) {
		return 0, ErrInvalid
	}
	var epoch int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		epoch, err = s.revokeLocked(ctx, tx, delegationID, actor, "revoke.operator")
		return err
	})
	return epoch, err
}

// RevokeSession revokes everything a browser session created: its sign-in
// assertions and its delegations. It is called at logout and returns how
// many delegations it revoked.
func (s *Store) RevokeSession(ctx context.Context, sessionID string) (int, error) {
	if sessionID == "" {
		return 0, nil
	}
	digest := SessionDigest(sessionID)
	now := s.now().UTC()
	revoked := 0
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
UPDATE elitea_identity.signin_assertions
SET state = 'revoked', assertion_ciphertext = NULL, revocation_epoch = revocation_epoch + 1,
    updated_at = clock_timestamp()
WHERE browser_session_digest = $1 AND state = 'active'
RETURNING id::text, user_id`, digest[:])
		if err != nil {
			return fmt.Errorf("delegationstore: revoke sign-in assertions: %w", err)
		}
		type retained struct {
			id   string
			user int64
		}
		var assertions []retained
		for rows.Next() {
			var a retained
			if err := rows.Scan(&a.id, &a.user); err != nil {
				rows.Close()
				return err
			}
			assertions = append(assertions, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, a := range assertions {
			if err := s.audit.Append(ctx, tx, securityaudit.SecurityEvent{
				Type: securityaudit.TypeDelegationChanged, Time: now, Action: "signin_assertion.revoke",
				Reasons: []string{"state.revoked", "revoke.logout"}, PrincipalRef: userRef(a.user),
				ResourceType: "signin_assertion", ResourceID: a.id,
			}); err != nil {
				return err
			}
		}
		idRows, err := tx.Query(ctx, `
SELECT id::text FROM elitea_identity.delegations
WHERE browser_session_digest = $1 AND state <> 'revoked'
ORDER BY id FOR UPDATE`, digest[:])
		if err != nil {
			return fmt.Errorf("delegationstore: read session delegations: %w", err)
		}
		ids, err := pgx.CollectRows(idRows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("delegationstore: read session delegations: %w", err)
		}
		for _, id := range ids {
			if _, err := s.revokeLocked(ctx, tx, id, "session", "revoke.logout"); err != nil {
				return err
			}
		}
		revoked = len(ids)
		return nil
	})
	return revoked, err
}

func (s *Store) revokeLocked(ctx context.Context, tx pgx.Tx, delegationID, actor, reason string) (int64, error) {
	var d Delegation
	var state string
	err := tx.QueryRow(ctx, `
SELECT id::text, tenant_project_id, agent_binding_id::text, tenant_id, subject_object_id, scopes, expires_at,
       revocation_epoch, state, COALESCE(assertion_audience, '')
FROM elitea_identity.delegations WHERE id = $1::uuid FOR UPDATE`, delegationID).
		Scan(&d.ID, &d.TenantProjectID, &d.AgentBindingID, &d.TenantID, &d.ObjectID, &d.Scopes, &d.ExpiresAt,
			&d.RevocationEpoch, &state, &d.Audience)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrDelegationNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("delegationstore: lock the delegation: %w", err)
	}
	if state == "revoked" {
		return d.RevocationEpoch, nil
	}
	if err := tx.QueryRow(ctx, `
UPDATE elitea_identity.delegations
SET state = 'revoked', assertion_ciphertext = NULL, revocation_epoch = revocation_epoch + 1,
    updated_at = clock_timestamp()
WHERE id = $1::uuid
RETURNING revocation_epoch`, delegationID).Scan(&d.RevocationEpoch); err != nil {
		return 0, fmt.Errorf("delegationstore: revoke the delegation: %w", err)
	}
	if err := s.audit.Append(ctx, tx, d.event(s.now().UTC(), "delegation.revoke", "state.revoked", actorRef(actor), reason)); err != nil {
		return 0, err
	}
	return d.RevocationEpoch, nil
}

func (d Delegation) aad() DelegationAAD {
	return DelegationAAD{
		TenantProjectID: d.TenantProjectID, DelegationID: d.ID, TenantID: d.TenantID, ObjectID: d.ObjectID,
		AgentBindingID: d.AgentBindingID, Audience: d.Audience, ExpiresAt: d.ExpiresAt,
	}
}

func (d Delegation) event(now time.Time, action, state, principal string, extra ...string) securityaudit.SecurityEvent {
	return securityaudit.SecurityEvent{
		Type: securityaudit.TypeDelegationChanged, Time: now, TenantProjectID: d.TenantProjectID,
		Action: action, Reasons: append([]string{state}, extra...), PrincipalRef: principal,
		AgentBindingID: d.AgentBindingID, DelegationRef: d.ID, ResourceType: "delegation", ResourceID: d.ID,
		RevocationEpoch: d.RevocationEpoch,
	}
}

func userRef(userID int64) string { return "user:" + strconv.FormatInt(userID, 10) }

func actorRef(actor string) string {
	if _, err := strconv.ParseInt(actor, 10, 64); err == nil {
		return "user:" + actor
	}
	return actor
}

func validText(value string) bool {
	return value != "" && len(value) <= maxTextBytes && strings.TrimSpace(value) == value &&
		!strings.ContainsFunc(value, unicode.IsControl)
}

func validScopes(scopes []string) bool {
	if len(scopes) > maxScopes {
		return false
	}
	for _, scope := range scopes {
		if !validText(scope) || strings.ContainsFunc(scope, unicode.IsSpace) {
			return false
		}
	}
	return true
}

func isUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value
}

// Delegation reads the metadata of one delegation for the operation
// decision (authzcompose.DelegationReader, #1118). It never opens the
// assertion. No row is mandate.ErrDelegationNotUsable, so an unknown id is
// a deny, not an error. A delegation whose assertion was wiped or has
// expired reads as expired: nothing can be exchanged for it.
func (s *Store) Delegation(ctx context.Context, id string) (mandate.Delegation, error) {
	if !isUUID(id) {
		return mandate.Delegation{}, mandate.ErrDelegationNotUsable
	}
	var (
		d                mandate.Delegation
		state            string
		assertionExpires *time.Time
	)
	err := s.pool.QueryRow(ctx, `
SELECT id::text, tenant_project_id, issuer, tenant_id, subject_object_id, agent_binding_id::text,
       audiences, scopes, consent_recorded_at, expires_at, state, revocation_epoch, assertion_expires_at
FROM elitea_identity.delegations
WHERE id = $1::uuid`, id).Scan(
		&d.ID, &d.TenantProjectID, &d.Issuer, &d.TenantID, &d.SubjectObjectID, &d.AgentBindingID,
		&d.Audiences, &d.Scopes, &d.ConsentRecordedAt, &d.ExpiresAt, &state, &d.RevocationEpoch, &assertionExpires)
	if errors.Is(err, pgx.ErrNoRows) {
		return mandate.Delegation{}, mandate.ErrDelegationNotUsable
	}
	if err != nil {
		return mandate.Delegation{}, fmt.Errorf("delegationstore: read the delegation: %w", err)
	}
	d.State, d.ExpiresAt = mandate.State(state), d.ExpiresAt.UTC()
	if d.State == mandate.StateActive && (assertionExpires == nil || !assertionExpires.After(s.now())) {
		d.State = mandate.StateExpired
	}
	return d, nil
}
