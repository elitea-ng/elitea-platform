package provideroauth

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

var providerPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

var (
	// ErrInvalid refuses a request before any statement runs.
	ErrInvalid = errors.New("provideroauth: invalid request")
	// ErrConsentRequired means no usable grant exists for the key: the
	// user has not consented, or the provider refused the grant and the
	// user must consent again.
	ErrConsentRequired = errors.New("provideroauth: the user must consent to the provider")
	// ErrGrantConflict means the grant changed while it was written (a
	// concurrent consent for the same key).
	ErrGrantConflict = errors.New("provideroauth: the grant changed concurrently")
)

// Grant states.
const (
	StateActive  = "active"
	StateInvalid = "invalid"
	StateRevoked = "revoked"
)

// GrantKey names one grant. Every field is required.
type GrantKey struct {
	TenantProjectID int64
	Provider        string
	TargetBindingID string
	SubjectUserID   int64
	AgentBindingID  string
}

func (k GrantKey) valid() bool {
	return k.TenantProjectID > 0 && providerPattern.MatchString(k.Provider) && validUUID(k.TargetBindingID) &&
		k.SubjectUserID > 0 && validUUID(k.AgentBindingID)
}

func (k GrantKey) aad(grantID string) GrantAAD {
	return GrantAAD{
		TenantProjectID: k.TenantProjectID, GrantID: grantID, Provider: k.Provider,
		TargetBindingID: k.TargetBindingID, SubjectUserID: k.SubjectUserID, AgentBindingID: k.AgentBindingID,
	}
}

func validUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id.String() == value
}

// Grants is the PostgreSQL custody store of provider grants.
type Grants struct {
	pool   *pgxpool.Pool
	sealer *sealer
	audit  securityaudit.Appender
	now    func() time.Time
}

// NewGrants builds the store. Every consent and every invalidation appends
// a security audit event in its transaction; a nil audit refuses
// construction.
func NewGrants(pool *pgxpool.Pool, masterKey []byte, audit securityaudit.Appender) (*Grants, error) {
	if pool == nil || audit == nil {
		return nil, errors.New("provideroauth: a database pool and a security audit appender are required")
	}
	s, err := newSealer(masterKey)
	if err != nil {
		return nil, err
	}
	return &Grants{pool: pool, sealer: s, audit: audit, now: time.Now}, nil
}

// consented is one consent's tokens and metadata.
type consented struct {
	Key         GrantKey
	Tokens      TokenResponse
	RedirectURI string
}

// save seals the consented tokens into the key's grant, creating it or
// replacing its tokens, and appends the audit event in the same
// transaction. It answers the grant id.
func (g *Grants) save(ctx context.Context, c consented) (string, error) {
	if !c.Key.valid() || c.Tokens.AccessToken == "" || c.RedirectURI == "" {
		return "", ErrInvalid
	}
	now := g.now().UTC()
	var grantID string
	err := pgx.BeginFunc(ctx, g.pool, func(tx pgx.Tx) error {
		k := c.Key
		err := tx.QueryRow(ctx, `
SELECT id::text FROM elitea_identity.provider_oauth_grants
 WHERE tenant_project_id = $1 AND provider_type = $2 AND target_binding_id = $3::uuid
   AND subject_user_id = $4 AND agent_binding_id = $5::uuid
 FOR UPDATE`, k.TenantProjectID, k.Provider, k.TargetBindingID, k.SubjectUserID, k.AgentBindingID).Scan(&grantID)
		insert := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !insert {
			return fmt.Errorf("provideroauth: read the grant: %w", err)
		}
		if insert {
			id, err := uuid.NewV7()
			if err != nil {
				return fmt.Errorf("provideroauth: grant id: %w", err)
			}
			grantID = id.String()
		}
		aad := k.aad(grantID)
		access := g.sealer.seal(c.Tokens.AccessToken, aad.with(purposeAccess))
		var refresh []byte
		if c.Tokens.RefreshToken != "" {
			refresh = g.sealer.seal(c.Tokens.RefreshToken, aad.with(purposeRefresh))
		}
		expires := nullableTime(c.Tokens.ExpiresAt)
		scopes := c.Tokens.Scopes
		if scopes == nil {
			scopes = []string{}
		}
		if insert {
			_, err = tx.Exec(ctx, `
INSERT INTO elitea_identity.provider_oauth_grants
    (id, tenant_project_id, provider_type, target_binding_id, subject_user_id, agent_binding_id,
     refresh_token_ciphertext, access_token_ciphertext, access_token_expires_at, granted_scopes,
     consented_at, consent_redirect_uri, key_label)
VALUES ($1::uuid, $2, $3, $4::uuid, $5, $6::uuid, $7, $8, $9, $10, $11, $12, $13)`,
				grantID, k.TenantProjectID, k.Provider, k.TargetBindingID, k.SubjectUserID, k.AgentBindingID,
				refresh, access, expires, scopes, now, c.RedirectURI, KeyLabel)
		} else {
			_, err = tx.Exec(ctx, `
UPDATE elitea_identity.provider_oauth_grants
   SET state = 'active', generation = generation + 1,
       refresh_token_ciphertext = $2, access_token_ciphertext = $3, access_token_expires_at = $4,
       granted_scopes = $5, consented_at = $6, consent_redirect_uri = $7, key_label = $8,
       updated_at = clock_timestamp()
 WHERE id = $1::uuid`, grantID, refresh, access, expires, scopes, now, c.RedirectURI, KeyLabel)
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrGrantConflict
		}
		if err != nil {
			return fmt.Errorf("provideroauth: store the grant: %w", err)
		}
		return g.audit.Append(ctx, tx, grantEvent(k, grantID, now, "provider_oauth_grant.consent", "state.active"))
	})
	if err != nil {
		return "", err
	}
	return grantID, nil
}

func grantEvent(k GrantKey, grantID string, at time.Time, action, reason string) securityaudit.SecurityEvent {
	return securityaudit.SecurityEvent{
		Type: securityaudit.TypeGrantChanged, Time: at, TenantProjectID: k.TenantProjectID,
		Action: action, Effect: securityaudit.EffectAllow, Reasons: []string{reason, "provider." + k.Provider},
		PrincipalRef: "user:" + strconv.FormatInt(k.SubjectUserID, 10), AgentBindingID: k.AgentBindingID,
		ResourceType: "provider_oauth_grant", ResourceID: grantID,
	}
}

func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC().Truncate(time.Microsecond)
	return &u
}

// snapshot is one read of a grant. The refresh token stays sealed until a
// refresh needs it.
type snapshot struct {
	ID          string
	Key         GrantKey
	State       string
	Generation  int64
	AccessToken string
	ExpiresAt   time.Time
	refreshSeal []byte
}

// load reads the key's grant and opens its access token. No grant is
// ErrConsentRequired.
func (g *Grants) load(ctx context.Context, k GrantKey) (snapshot, error) {
	if !k.valid() {
		return snapshot{}, ErrInvalid
	}
	s := snapshot{Key: k}
	var access []byte
	var expires *time.Time
	err := g.pool.QueryRow(ctx, `
SELECT id::text, state, generation, access_token_ciphertext, access_token_expires_at, refresh_token_ciphertext
  FROM elitea_identity.provider_oauth_grants
 WHERE tenant_project_id = $1 AND provider_type = $2 AND target_binding_id = $3::uuid
   AND subject_user_id = $4 AND agent_binding_id = $5::uuid`,
		k.TenantProjectID, k.Provider, k.TargetBindingID, k.SubjectUserID, k.AgentBindingID).
		Scan(&s.ID, &s.State, &s.Generation, &access, &expires, &s.refreshSeal)
	if errors.Is(err, pgx.ErrNoRows) {
		return snapshot{}, ErrConsentRequired
	}
	if err != nil {
		return snapshot{}, fmt.Errorf("provideroauth: read the grant: %w", err)
	}
	if expires != nil {
		s.ExpiresAt = expires.UTC()
	}
	if s.State == StateActive && len(access) > 0 {
		token, err := g.sealer.open(access, k.aad(s.ID).with(purposeAccess))
		if err != nil {
			return snapshot{}, err
		}
		s.AccessToken = token
	}
	return s, nil
}

// openRefresh opens the snapshot's refresh token; empty when it has none.
func (g *Grants) openRefresh(s snapshot) (string, error) {
	if len(s.refreshSeal) == 0 {
		return "", nil
	}
	return g.sealer.open(s.refreshSeal, s.Key.aad(s.ID).with(purposeRefresh))
}

// writeRefreshed writes the refreshed access token and, when the provider
// rotated it, the new refresh token, inside the lease's fenced
// transaction. A grant whose generation moved is ErrGrantConflict.
func (g *Grants) writeRefreshed(ctx context.Context, tx pgx.Tx, s snapshot, tokens TokenResponse) (int64, error) {
	aad := s.Key.aad(s.ID)
	access := g.sealer.seal(tokens.AccessToken, aad.with(purposeAccess))
	refresh := s.refreshSeal
	if tokens.RefreshToken != "" {
		refresh = g.sealer.seal(tokens.RefreshToken, aad.with(purposeRefresh))
	}
	var generation int64
	err := tx.QueryRow(ctx, `
UPDATE elitea_identity.provider_oauth_grants
   SET access_token_ciphertext = $3, access_token_expires_at = $4, refresh_token_ciphertext = $5,
       generation = generation + 1, refreshed_at = clock_timestamp(), updated_at = clock_timestamp()
 WHERE id = $1::uuid AND generation = $2 AND state = 'active'
RETURNING generation`, s.ID, s.Generation, access, nullableTime(tokens.ExpiresAt), refresh).Scan(&generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrGrantConflict
	}
	if err != nil {
		return 0, fmt.Errorf("provideroauth: write the refreshed grant: %w", err)
	}
	return generation, nil
}

// markInvalid wipes the grant's tokens after the provider refused its
// refresh token, bumps its revocation epoch and audits, inside the lease's
// fenced transaction.
func (g *Grants) markInvalid(ctx context.Context, tx pgx.Tx, s snapshot) error {
	tag, err := tx.Exec(ctx, `
UPDATE elitea_identity.provider_oauth_grants
   SET state = 'invalid', access_token_ciphertext = NULL, refresh_token_ciphertext = NULL,
       access_token_expires_at = NULL, generation = generation + 1,
       revocation_epoch = revocation_epoch + 1, updated_at = clock_timestamp()
 WHERE id = $1::uuid AND generation = $2 AND state = 'active'`, s.ID, s.Generation)
	if err != nil {
		return fmt.Errorf("provideroauth: invalidate the grant: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrGrantConflict
	}
	return g.audit.Append(ctx, tx, grantEvent(s.Key, s.ID, g.now().UTC(), "provider_oauth_grant.invalidate", "provider.invalid_grant"))
}
