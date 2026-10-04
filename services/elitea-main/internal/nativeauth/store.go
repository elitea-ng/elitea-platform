package nativeauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Revoke reasons (native_sessions.revoke_reason).
const (
	ReasonSignedOut       = "signed_out"
	ReasonUser            = "user"
	ReasonAdmin           = "admin"
	ReasonRefreshReuse    = "refresh_reuse"
	ReasonCodeReplay      = "code_replay"
	ReasonUserDeactivated = "user_deactivated"
	ReasonClientDisabled  = "client_disabled"
	ReasonClientRemoved   = "client_removed"
	ReasonExpired         = "expired"
)

// The platforms a client may claim.
var platforms = map[string]bool{
	"ios": true, "android": true, "macos": true, "windows": true, "linux": true, "other": true,
}

// ValidPlatform reports whether platform is one of the six accepted values.
func ValidPlatform(platform string) bool { return platforms[platform] }

// Store errors. None carries a database cause across a trust boundary.
var (
	// ErrNotFound: no authorization request matches the handle.
	ErrNotFound = errors.New("nativeauth: not found")
	// ErrNotPending: the authorization was already decided or has expired.
	ErrNotPending = errors.New("nativeauth: authorization is not pending")
	// ErrInvalidGrant: the code or refresh token is unknown, expired, used,
	// for another client or redirect, or fails PKCE (RFC 6749 §5.2).
	ErrInvalidGrant = errors.New("nativeauth: invalid grant")
	// ErrCodeReplay: a code that was already exchanged was presented again;
	// the family it produced is now revoked. It wraps ErrInvalidGrant.
	ErrCodeReplay = fmt.Errorf("%w: authorization code replayed", ErrInvalidGrant)
	// ErrFamilyRevoked: the refresh token's family is revoked, expired, or
	// belongs to a deactivated user or a removed client. The specific reasons
	// below wrap it.
	ErrFamilyRevoked = errors.New("nativeauth: device session revoked")
	// ErrRefreshReuse: a consumed refresh token was presented outside the
	// re-delivery window; the family was revoked in this request.
	ErrRefreshReuse = fmt.Errorf("%w: refresh token reused", ErrFamilyRevoked)
	// ErrClientInactive: the client is not registered or is disabled, as the
	// database says INSIDE the exchange transaction (the registry cache may
	// be stale on this replica).
	ErrClientInactive = errors.New("nativeauth: client not registered or disabled")
	// ErrUnavailable: the store did not answer.
	ErrUnavailable = errors.New("nativeauth: store unavailable")
)

// Store is the PostgreSQL side of the authorization server.
type Store struct {
	pool *pgxpool.Pool
	cfg  Config
	now  func() time.Time
}

// NewStore builds the store. cfg fields left zero take the defaults.
func NewStore(pool *pgxpool.Pool, cfg Config) *Store {
	defaults := DefaultConfig()
	if cfg.AccessTokenTTL <= 0 {
		cfg.AccessTokenTTL = defaults.AccessTokenTTL
	}
	if cfg.RefreshIdleTTL <= 0 {
		cfg.RefreshIdleTTL = defaults.RefreshIdleTTL
	}
	if cfg.RedeliveryWindow < 0 {
		cfg.RedeliveryWindow = 0
	}
	return &Store{pool: pool, cfg: cfg, now: time.Now}
}

// SetClock replaces the store's clock. Tests only.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Config is the lifetime policy the store issues under.
func (s *Store) Config() Config { return s.cfg }

func (s *Store) clock() time.Time { return s.now().UTC() }

func unavailable(operation string, err error) error {
	return fmt.Errorf("%w: %s: %v", ErrUnavailable, operation, err)
}

/* ── authorization requests ─────────────────────────────────────────────── */

// AuthorizationRequest is a validated /authorize request.
type AuthorizationRequest struct {
	ClientID      string
	RedirectURI   string
	CodeChallenge string
	State         string
	DeviceName    string
	Platform      string
	ClientVersion string
}

// Authorization is one stored authorization request.
type Authorization struct {
	ID             int64
	BinderHash     string
	ClientID       string
	RedirectURI    string
	State          string
	DeviceName     string
	Platform       string
	ClientVersion  string
	Status         string
	LoginRedirects int
	ExpiresAt      time.Time
}

// Expired reports whether the request outlived AuthorizationTTL.
func (a Authorization) Expired(now time.Time) bool { return !now.Before(a.ExpiresAt) }

// CreateAuthorization stores a pending request and returns the handle (query
// parameter) and the binder (cookie value). Neither is stored in plain text.
func (s *Store) CreateAuthorization(ctx context.Context, request AuthorizationRequest) (handle, binder string, err error) {
	if handle, err = NewOpaque(); err != nil {
		return "", "", err
	}
	if binder, err = NewOpaque(); err != nil {
		return "", "", err
	}
	now := s.clock()
	_, err = s.pool.Exec(ctx, `
		INSERT INTO elitea_auth.native_authorizations
		    (handle_hash, binder_hash, client_id, redirect_uri, code_challenge, state,
		     device_name, platform, client_version, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		HashSecret(handle), HashSecret(binder), request.ClientID, request.RedirectURI,
		request.CodeChallenge, request.State, request.DeviceName, request.Platform,
		request.ClientVersion, now, now.Add(AuthorizationTTL))
	if err != nil {
		return "", "", unavailable("insert authorization", err)
	}
	return handle, binder, nil
}

// CountPending counts unexpired pending requests: the global ceiling on
// /authorize.
func (s *Store) CountPending(ctx context.Context) (int64, error) {
	var count int64
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM elitea_auth.native_authorizations
		WHERE expires_at > $1 AND status = 'pending'`, s.clock()).Scan(&count)
	if err != nil {
		return 0, unavailable("count pending authorizations", err)
	}
	return count, nil
}

// AuthorizationByHandle loads a request by its handle.
func (s *Store) AuthorizationByHandle(ctx context.Context, handle string) (Authorization, error) {
	if !WellFormedOpaque(handle) {
		return Authorization{}, ErrNotFound
	}
	var a Authorization
	err := s.pool.QueryRow(ctx, `
		SELECT id, binder_hash, client_id, redirect_uri, state, device_name, platform,
		       client_version, status, login_redirects, expires_at
		FROM elitea_auth.native_authorizations
		WHERE handle_hash = $1`, HashSecret(handle)).Scan(
		&a.ID, &a.BinderHash, &a.ClientID, &a.RedirectURI, &a.State, &a.DeviceName, &a.Platform,
		&a.ClientVersion, &a.Status, &a.LoginRedirects, &a.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Authorization{}, ErrNotFound
	}
	if err != nil {
		return Authorization{}, unavailable("read authorization", err)
	}
	return a, nil
}

// BinderMatches compares a presented binder against the stored hash in
// constant time.
func (a Authorization) BinderMatches(binder string) bool {
	if !WellFormedOpaque(binder) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(HashSecret(binder)), []byte(a.BinderHash)) == 1
}

// BumpLoginRedirects counts one more sign-in bounce and returns the new count.
func (s *Store) BumpLoginRedirects(ctx context.Context, id int64) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `
		UPDATE elitea_auth.native_authorizations
		SET login_redirects = login_redirects + 1
		WHERE id = $1 AND status = 'pending'
		RETURNING login_redirects`, id).Scan(&count)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotPending
	}
	if err != nil {
		return 0, unavailable("count sign-in bounce", err)
	}
	return count, nil
}

// Decision is the result of a consent decision.
type Decision struct {
	RedirectURI string
	State       string
	// Code is set when the user allowed the request.
	Code string
}

// Decide records the user's answer atomically. Zero rows (double submit,
// expired request, already decided) is ErrNotPending.
func (s *Store) Decide(ctx context.Context, id int64, allow bool, userID int64) (Decision, error) {
	now := s.clock()
	status := "denied"
	var codeHash, code *string
	var codeExpires *time.Time
	if allow {
		generated, err := newSecret(PrefixCode)
		if err != nil {
			return Decision{}, err
		}
		hash := HashSecret(generated)
		expires := now.Add(CodeTTL)
		status, code, codeHash, codeExpires = "approved", &generated, &hash, &expires
	}
	var decision Decision
	err := s.pool.QueryRow(ctx, `
		UPDATE elitea_auth.native_authorizations
		SET status = $2, user_id = $3, decided_at = $4, code_hash = $5, code_expires_at = $6
		WHERE id = $1 AND status = 'pending' AND expires_at > $4
		RETURNING redirect_uri, state`,
		id, status, userID, now, codeHash, codeExpires).Scan(&decision.RedirectURI, &decision.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return Decision{}, ErrNotPending
	}
	if err != nil {
		return Decision{}, unavailable("record decision", err)
	}
	if code != nil {
		decision.Code = *code
	}
	return decision, nil
}

/* ── tokens ─────────────────────────────────────────────────────────────── */

// Grant is a successful token response's content.
type Grant struct {
	AccessToken      string
	RefreshToken     string
	AccessExpiresIn  time.Duration
	RefreshExpiresIn time.Duration
	DeviceID         string
	// The owner and device, for the audit row the handler writes.
	UserID     int64
	UserEmail  string
	ClientID   string
	DeviceName string
	// Redelivered is true when the grace window returned an existing pair.
	Redelivered bool
}

// ExchangeRequest is a validated authorization_code grant.
type ExchangeRequest struct {
	Code        string
	ClientID    string
	RedirectURI string
	Verifier    string
	// FileClientEnabled is the FILE layer's verdict on ClientID
	// (Registry.FileActive). The file layer is fixed for the process, so it
	// cannot be stale; it decides only when the DB layer has no row.
	FileClientEnabled bool
}

// ReplayInfo describes the family a replayed code revoked, for the audit row.
type ReplayInfo struct {
	UserID     int64
	ClientID   string
	DeviceName string
}

// ExchangeCode runs the code grant in one transaction. A replayed code revokes
// the family it produced and COMMITS that before answering ErrCodeReplay.
func (s *Store) ExchangeCode(ctx context.Context, request ExchangeRequest) (Grant, *ReplayInfo, error) {
	if !WellFormed(request.Code, PrefixCode) {
		return Grant{}, nil, ErrInvalidGrant
	}
	now := s.clock()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Grant{}, nil, unavailable("begin exchange", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		id                                       int64
		clientID, redirectURI, challenge, status string
		deviceName, platform, clientVersion      string
		userID                                   *int64
		codeExpires                              *time.Time
		sessionID                                *string
	)
	err = tx.QueryRow(ctx, `
		SELECT id, client_id, redirect_uri, code_challenge, status, device_name, platform,
		       client_version, user_id, code_expires_at, session_id::text
		FROM elitea_auth.native_authorizations
		WHERE code_hash = $1
		FOR UPDATE`, HashSecret(request.Code)).Scan(
		&id, &clientID, &redirectURI, &challenge, &status, &deviceName, &platform,
		&clientVersion, &userID, &codeExpires, &sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Grant{}, nil, ErrInvalidGrant
	}
	if err != nil {
		return Grant{}, nil, unavailable("read authorization code", err)
	}
	if status == "exchanged" {
		// RFC 6749 §4.1.2: a code used twice revokes what it produced.
		var info *ReplayInfo
		if sessionID != nil {
			revoked, revokeErr := revokeFamily(ctx, tx, *sessionID, ReasonCodeReplay, nil)
			if revokeErr != nil {
				return Grant{}, nil, unavailable("revoke replayed family", revokeErr)
			}
			if revoked && userID != nil {
				info = &ReplayInfo{UserID: *userID, ClientID: clientID, DeviceName: deviceName}
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return Grant{}, nil, unavailable("commit replay revocation", err)
		}
		return Grant{}, info, ErrCodeReplay
	}
	if status != "approved" || codeExpires == nil || !now.Before(*codeExpires) || userID == nil {
		return Grant{}, nil, ErrInvalidGrant
	}
	if clientID != request.ClientID || redirectURI != request.RedirectURI {
		return Grant{}, nil, ErrInvalidGrant
	}
	if !pkceMatches(request.Verifier, challenge) {
		return Grant{}, nil, ErrInvalidGrant
	}

	// The registry the caller checked is a per-replica cache: a client
	// disabled or deleted on another replica within its TTL still reads as
	// active there, and the disable revoked only the families that existed
	// when it committed. Re-check the client here, serialised against
	// UpsertClient/DeleteClient by the client's advisory lock, so a family is
	// either visible to the disable's revocation or never created.
	active, err := clientActiveTx(ctx, tx, clientID, request.FileClientEnabled)
	if err != nil {
		return Grant{}, nil, unavailable("re-check native client", err)
	}
	if !active {
		return Grant{}, nil, ErrClientInactive
	}

	grant, err := s.createFamily(ctx, tx, *userID, clientID, deviceName, platform, clientVersion, now)
	if err != nil {
		return Grant{}, nil, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE elitea_auth.native_authorizations
		SET status = 'exchanged', exchanged_at = $2, session_id = $3
		WHERE id = $1`, id, now, grant.DeviceID); err != nil {
		return Grant{}, nil, unavailable("mark code exchanged", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Grant{}, nil, unavailable("commit exchange", err)
	}
	return grant, nil, nil
}

func pkceMatches(verifier, challenge string) bool {
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}

func (s *Store) createFamily(
	ctx context.Context, tx pgx.Tx, userID int64, clientID, deviceName, platform, clientVersion string, now time.Time,
) (Grant, error) {
	var anchorID int64
	var email string
	err := tx.QueryRow(ctx, `
		INSERT INTO public.auth_core__token (uuid, expires, user_id, name)
		SELECT NULL, NULL, owner.id, 'native:' || $2::text
		FROM public.auth_core__user AS owner
		WHERE owner.id = $1 AND owner.suspended = false
		RETURNING id, (SELECT COALESCE(email, '') FROM public.auth_core__user WHERE id = $1)`,
		userID, clientID).Scan(&anchorID, &email)
	if errors.Is(err, pgx.ErrNoRows) {
		return Grant{}, ErrInvalidGrant
	}
	if err != nil {
		return Grant{}, unavailable("insert device anchor", err)
	}
	var expiresAt *time.Time
	if s.cfg.SessionMaxLifetime > 0 {
		cap := now.Add(s.cfg.SessionMaxLifetime)
		expiresAt = &cap
	}
	var sessionID string
	err = tx.QueryRow(ctx, `
		INSERT INTO elitea_auth.native_sessions
		    (user_id, token_id, client_id, device_name, platform, client_version,
		     created_at, last_seen_at, last_refreshed_at, idle_timeout_seconds, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7, $7, $8, $9)
		RETURNING id::text`,
		userID, anchorID, clientID, deviceName, platform, clientVersion, now,
		int64(s.cfg.RefreshIdleTTL/time.Second), expiresAt).Scan(&sessionID)
	if err != nil {
		return Grant{}, unavailable("insert device session", err)
	}
	pair, err := s.issuePair(ctx, tx, sessionID, 1, now)
	if err != nil {
		return Grant{}, err
	}
	return Grant{
		AccessToken:      pair.AccessToken,
		RefreshToken:     pair.RefreshToken,
		AccessExpiresIn:  s.cfg.AccessTokenTTL,
		RefreshExpiresIn: s.refreshExpiresIn(s.cfg.RefreshIdleTTL, expiresAt, now),
		DeviceID:         sessionID,
		UserID:           userID,
		UserEmail:        email,
		ClientID:         clientID,
		DeviceName:       deviceName,
	}, nil
}

// refreshExpiresIn is the idle TTL, shortened by the absolute cap.
func (s *Store) refreshExpiresIn(idle time.Duration, cap *time.Time, now time.Time) time.Duration {
	if cap != nil && cap.Sub(now) < idle {
		if remaining := cap.Sub(now); remaining > 0 {
			return remaining
		}
		return 0
	}
	return idle
}

func (s *Store) issuePair(ctx context.Context, tx pgx.Tx, sessionID string, generation int, now time.Time) (sealedPair, error) {
	access, err := newSecret(PrefixAccessToken)
	if err != nil {
		return sealedPair{}, err
	}
	refresh, err := newSecret(PrefixRefreshToken)
	if err != nil {
		return sealedPair{}, err
	}
	accessExpires := now.Add(s.cfg.AccessTokenTTL)
	if _, err := tx.Exec(ctx, `
		INSERT INTO elitea_auth.native_refresh_tokens (token_hash, session_id, generation, issued_at)
		VALUES ($1, $2, $3, $4)`, HashSecret(refresh), sessionID, generation, now); err != nil {
		return sealedPair{}, unavailable("insert refresh token", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO elitea_auth.native_access_tokens (token_hash, session_id, issued_at, expires_at)
		VALUES ($1, $2, $3, $4)`, HashSecret(access), sessionID, now, accessExpires); err != nil {
		return sealedPair{}, unavailable("insert access token", err)
	}
	return sealedPair{AccessToken: access, RefreshToken: refresh, AccessExpiresAt: accessExpires.Unix()}, nil
}

// RefreshRequest is a validated refresh_token grant.
type RefreshRequest struct {
	RefreshToken  string
	ClientID      string
	ClientVersion string
	// ClientActive is whether ClientID is registered and enabled NOW in the
	// effective registry. A family whose client disappeared from the file
	// layer is revoked at its next refresh (client_removed).
	ClientActive bool
}

// RefreshOutcome carries what the handler audits on a refusal.
type RefreshOutcome struct {
	UserID     int64
	UserEmail  string
	ClientID   string
	DeviceName string
	// Reason is the revoke reason this request applied, "" when none.
	Reason string
}

type familyRow struct {
	id              string
	userID          int64
	tokenID         *int64
	clientID        string
	deviceName      string
	lastRefreshedAt time.Time
	idleSeconds     int64
	expiresAt       *time.Time
	revokedAt       *time.Time
	ownerActive     bool
	ownerEmail      string
}

// Refresh rotates a refresh token. Every branch that revokes COMMITS before it
// returns: a handler that wrote the refusal and let the deferred rollback run
// would silently un-revoke the family.
//
// Concurrency: the family row is locked FIRST, and the presented token's row
// is read in a separate statement AFTER the lock is held, so a second request
// that waited on the lock sees the first one's consumed_at (READ COMMITTED
// takes a fresh snapshot per statement). Two concurrent refreshes with one
// token therefore serialise: inside the re-delivery window both receive the
// same successor pair; with a zero window the second revokes the family.
func (s *Store) Refresh(ctx context.Context, request RefreshRequest) (Grant, RefreshOutcome, error) {
	if !WellFormed(request.RefreshToken, PrefixRefreshToken) {
		return Grant{}, RefreshOutcome{}, ErrInvalidGrant
	}
	hash := HashSecret(request.RefreshToken)
	now := s.clock()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Grant{}, RefreshOutcome{}, unavailable("begin refresh", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var sessionID string
	err = tx.QueryRow(ctx, `
		SELECT session_id::text FROM elitea_auth.native_refresh_tokens WHERE token_hash = $1`, hash).Scan(&sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Grant{}, RefreshOutcome{}, ErrInvalidGrant
	}
	if err != nil {
		return Grant{}, RefreshOutcome{}, unavailable("read refresh token", err)
	}
	family, err := lockFamily(ctx, tx, sessionID)
	if err != nil {
		return Grant{}, RefreshOutcome{}, err
	}
	outcome := RefreshOutcome{
		UserID: family.userID, UserEmail: family.ownerEmail, ClientID: family.clientID, DeviceName: family.deviceName,
	}
	if family.clientID != request.ClientID {
		return Grant{}, RefreshOutcome{}, ErrInvalidGrant
	}
	if family.revokedAt != nil || family.tokenID == nil {
		return Grant{}, outcome, ErrFamilyRevoked
	}

	revokeAndCommit := func(reason string, cause error) (Grant, RefreshOutcome, error) {
		if _, err := revokeFamily(ctx, tx, sessionID, reason, nil); err != nil {
			return Grant{}, RefreshOutcome{}, unavailable("revoke family", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return Grant{}, RefreshOutcome{}, unavailable("commit revocation", err)
		}
		outcome.Reason = reason
		return Grant{}, outcome, cause
	}
	if !family.ownerActive {
		return revokeAndCommit(ReasonUserDeactivated, ErrFamilyRevoked)
	}
	if !request.ClientActive {
		return revokeAndCommit(ReasonClientRemoved, ErrFamilyRevoked)
	}

	var (
		generation int
		consumedAt *time.Time
		sealed     []byte
	)
	err = tx.QueryRow(ctx, `
		SELECT generation, consumed_at, successor_sealed
		FROM elitea_auth.native_refresh_tokens WHERE token_hash = $1`, hash).Scan(&generation, &consumedAt, &sealed)
	if err != nil {
		return Grant{}, RefreshOutcome{}, unavailable("re-read refresh token", err)
	}
	if consumedAt != nil {
		if pair, ok := s.redeliverable(request.RefreshToken, sessionID, generation, *consumedAt, sealed, now); ok {
			// The SAME successor pair, minted by the request whose response
			// was lost. Nothing is written: no new pair, no revocation.
			if err := tx.Commit(ctx); err != nil {
				return Grant{}, RefreshOutcome{}, unavailable("commit re-delivery", err)
			}
			accessLeft := pair.accessExpiresAt().Sub(now)
			if accessLeft < 0 {
				accessLeft = 0
			}
			return Grant{
				AccessToken:      pair.AccessToken,
				RefreshToken:     pair.RefreshToken,
				AccessExpiresIn:  accessLeft,
				RefreshExpiresIn: s.refreshExpiresIn(time.Duration(family.idleSeconds)*time.Second, family.expiresAt, now),
				DeviceID:         sessionID,
				UserID:           family.userID,
				UserEmail:        family.ownerEmail,
				ClientID:         family.clientID,
				DeviceName:       family.deviceName,
				Redelivered:      true,
			}, outcome, nil
		}
		return revokeAndCommit(ReasonRefreshReuse, ErrRefreshReuse)
	}
	idle := time.Duration(family.idleSeconds) * time.Second
	if !now.Before(family.lastRefreshedAt.Add(idle)) || (family.expiresAt != nil && !now.Before(*family.expiresAt)) {
		return revokeAndCommit(ReasonExpired, ErrFamilyRevoked)
	}

	pair, err := s.issuePair(ctx, tx, sessionID, generation+1, now)
	if err != nil {
		return Grant{}, RefreshOutcome{}, err
	}
	var box []byte
	if s.cfg.RedeliveryWindow > 0 {
		if box, err = sealSuccessor(request.RefreshToken, sessionID, generation, pair); err != nil {
			return Grant{}, RefreshOutcome{}, fmt.Errorf("nativeauth: seal successor: %w", err)
		}
	}
	tag, err := tx.Exec(ctx, `
		UPDATE elitea_auth.native_refresh_tokens
		SET consumed_at = $2, successor_sealed = $3
		WHERE token_hash = $1 AND consumed_at IS NULL`, hash, now, box)
	if err != nil {
		return Grant{}, RefreshOutcome{}, unavailable("consume refresh token", err)
	}
	if tag.RowsAffected() != 1 {
		return Grant{}, RefreshOutcome{}, unavailable("consume refresh token", errors.New("row changed under lock"))
	}
	// The previous generation's successor (this token) is now used, so its
	// sealed copy has no purpose left.
	if _, err := tx.Exec(ctx, `
		UPDATE elitea_auth.native_refresh_tokens
		SET successor_sealed = NULL
		WHERE session_id = $1 AND generation < $2 AND successor_sealed IS NOT NULL`, sessionID, generation); err != nil {
		return Grant{}, RefreshOutcome{}, unavailable("clear sealed successor", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE elitea_auth.native_sessions
		SET last_refreshed_at = $2, last_seen_at = $2,
		    client_version = COALESCE(NULLIF($3, ''), client_version)
		WHERE id = $1`, sessionID, now, request.ClientVersion); err != nil {
		return Grant{}, RefreshOutcome{}, unavailable("touch device session", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Grant{}, RefreshOutcome{}, unavailable("commit refresh", err)
	}
	return Grant{
		AccessToken:      pair.AccessToken,
		RefreshToken:     pair.RefreshToken,
		AccessExpiresIn:  s.cfg.AccessTokenTTL,
		RefreshExpiresIn: s.refreshExpiresIn(idle, family.expiresAt, now),
		DeviceID:         sessionID,
		UserID:           family.userID,
		UserEmail:        family.ownerEmail,
		ClientID:         family.clientID,
		DeviceName:       family.deviceName,
	}, outcome, nil
}

// redeliverable decides the grace window: the presented token was consumed no
// longer than RedeliveryWindow ago, and its successor is still unused (the
// sealed copy is cleared the moment the successor rotates).
func (s *Store) redeliverable(
	presented, sessionID string, generation int, consumedAt time.Time, sealed []byte, now time.Time,
) (sealedPair, bool) {
	if s.cfg.RedeliveryWindow <= 0 || len(sealed) == 0 || now.Sub(consumedAt) > s.cfg.RedeliveryWindow {
		return sealedPair{}, false
	}
	pair, err := openSuccessor(presented, sessionID, generation, sealed)
	if err != nil {
		return sealedPair{}, false
	}
	return pair, true
}

func lockFamily(ctx context.Context, tx pgx.Tx, sessionID string) (familyRow, error) {
	var family familyRow
	err := tx.QueryRow(ctx, `
		SELECT s.id::text, s.user_id, s.token_id, s.client_id, s.device_name, s.last_refreshed_at,
		       s.idle_timeout_seconds, s.expires_at, s.revoked_at,
		       (owner.id IS NOT NULL AND owner.suspended = false) AS owner_active,
		       COALESCE(owner.email, '')
		FROM elitea_auth.native_sessions AS s
		LEFT JOIN public.auth_core__user AS owner ON owner.id = s.user_id
		WHERE s.id = $1
		FOR UPDATE OF s`, sessionID).Scan(
		&family.id, &family.userID, &family.tokenID, &family.clientID, &family.deviceName,
		&family.lastRefreshedAt, &family.idleSeconds, &family.expiresAt, &family.revokedAt,
		&family.ownerActive, &family.ownerEmail)
	if errors.Is(err, pgx.ErrNoRows) {
		return familyRow{}, ErrInvalidGrant
	}
	if err != nil {
		return familyRow{}, unavailable("lock device session", err)
	}
	return family, nil
}

// revokeFamily revokes one family inside the caller's transaction: the row is
// marked revoked and its anchor cleared; the ADR-0018 binding and the anchor
// row are deleted. It reports whether a live family was revoked (false:
// already revoked or unknown).
//
// The family's ACCESS TOKENS ARE KEPT until they expire (the retention sweeper
// removes them): they can no longer authenticate — the validator reads the
// revoked family — but keeping the row is what lets the API answer the
// client's next call with `device_revoked` (wipe) instead of `token_rejected`
// (refresh once), which is the ADR-0025 decision 4 contract.
//
// The binding is deleted EXPLICITLY rather than through the guarded cascade
// (auth_pat.sql's DeleteTokenProjectBinding states why). Deleting the anchor is
// what cuts off an edge-validated native principal: the principal re-check
// reads the auth_core__token row by id and stops finding it.
func revokeFamily(ctx context.Context, tx pgx.Tx, sessionID, reason string, revokedBy *int64) (bool, error) {
	var anchor *int64
	err := tx.QueryRow(ctx, `
		WITH previous AS (
		    SELECT id, token_id FROM elitea_auth.native_sessions
		    WHERE id = $1 AND revoked_at IS NULL
		    FOR UPDATE
		)
		UPDATE elitea_auth.native_sessions AS s
		SET revoked_at = now(), revoke_reason = $2, revoked_by = $3, token_id = NULL
		FROM previous
		WHERE s.id = previous.id
		RETURNING previous.token_id`, sessionID, reason, revokedBy).Scan(&anchor)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if anchor != nil {
		if _, err := tx.Exec(ctx,
			`DELETE FROM elitea_identity.token_project_binding WHERE token_id = $1`, *anchor); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM public.auth_core__token WHERE id = $1`, *anchor); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE elitea_auth.native_refresh_tokens SET successor_sealed = NULL
		WHERE session_id = $1 AND successor_sealed IS NOT NULL`, sessionID); err != nil {
		return false, err
	}
	return true, nil
}

/* ── revocation by token (RFC 7009) ─────────────────────────────────────── */

// RevokedFamily describes a family /revoke revoked, for the audit row.
type RevokedFamily struct {
	DeviceID   string
	UserID     int64
	ClientID   string
	DeviceName string
}

// RevokeByToken revokes the whole family a refresh or access token belongs to
// (signed_out). An unknown token, an already-revoked family or a token for
// another client returns (nil, nil): RFC 7009 §2.2 answers 200 either way.
func (s *Store) RevokeByToken(ctx context.Context, token, clientID string) (*RevokedFamily, error) {
	var query string
	switch {
	case WellFormed(token, PrefixRefreshToken):
		query = `SELECT session_id::text FROM elitea_auth.native_refresh_tokens WHERE token_hash = $1`
	case WellFormed(token, PrefixAccessToken):
		query = `SELECT session_id::text FROM elitea_auth.native_access_tokens WHERE token_hash = $1`
	default:
		return nil, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, unavailable("begin revoke", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var sessionID string
	err = tx.QueryRow(ctx, query, HashSecret(token)).Scan(&sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, unavailable("resolve token family", err)
	}
	family, err := lockFamily(ctx, tx, sessionID)
	if errors.Is(err, ErrInvalidGrant) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if family.clientID != clientID {
		return nil, nil
	}
	revoked, err := revokeFamily(ctx, tx, sessionID, ReasonSignedOut, nil)
	if err != nil {
		return nil, unavailable("revoke family", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, unavailable("commit revoke", err)
	}
	if !revoked {
		return nil, nil
	}
	return &RevokedFamily{
		DeviceID: sessionID, UserID: family.userID, ClientID: family.clientID, DeviceName: family.deviceName,
	}, nil
}

/* ── client registry writes (DB layer) ──────────────────────────────────── */

// UpsertClient writes a DB-layer client. Disabling it revokes every live
// family of that client IN THE SAME TRANSACTION; turning it back on resurrects
// nothing. It returns how many families were revoked.
func (s *Store) UpsertClient(ctx context.Context, client Client, actor int64) (int64, error) {
	if err := ValidateClient(client); err != nil {
		return 0, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, unavailable("begin client save", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockClient(ctx, tx, client.ClientID, true); err != nil {
		return 0, unavailable("lock native client", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO elitea_auth.native_clients
		    (client_id, display_name, redirect_uris, enabled, min_client_version, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $6, $5, $5)
		ON CONFLICT (client_id) DO UPDATE
		SET display_name = EXCLUDED.display_name, redirect_uris = EXCLUDED.redirect_uris,
		    enabled = EXCLUDED.enabled, min_client_version = EXCLUDED.min_client_version,
		    updated_by = EXCLUDED.updated_by, updated_at = now()`,
		client.ClientID, client.DisplayName, client.RedirectURIs, client.Enabled, actor,
		client.MinClientVersion); err != nil {
		return 0, unavailable("save native client", err)
	}
	var revoked int64
	if !client.Enabled {
		if revoked, err = revokeClientFamilies(ctx, tx, client.ClientID, ReasonClientDisabled, actor); err != nil {
			return 0, unavailable("revoke client families", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, unavailable("commit client save", err)
	}
	return revoked, nil
}

// DeleteClient removes a DB-layer client and revokes every live family of it
// in the same transaction. A file entry with the same id applies again.
func (s *Store) DeleteClient(ctx context.Context, clientID string, actor int64) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, unavailable("begin client delete", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockClient(ctx, tx, clientID, true); err != nil {
		return 0, unavailable("lock native client", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM elitea_auth.native_clients WHERE client_id = $1`, clientID)
	if err != nil {
		return 0, unavailable("delete native client", err)
	}
	if tag.RowsAffected() == 0 {
		return 0, ErrClientNotFound
	}
	revoked, err := revokeClientFamilies(ctx, tx, clientID, ReasonClientRemoved, actor)
	if err != nil {
		return 0, unavailable("revoke client families", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, unavailable("commit client delete", err)
	}
	return revoked, nil
}

// clientLockClass is the first key of the per-client transaction advisory
// lock (the second is hashtext(client_id)). Writers of the DB layer take it
// exclusively; a code exchange takes it shared, so exchanges of one client
// run concurrently but never interleave with a save or delete of it.
const clientLockClass = 250025

func lockClient(ctx context.Context, tx pgx.Tx, clientID string, exclusive bool) error {
	query := `SELECT pg_advisory_xact_lock_shared($1, hashtext($2))`
	if exclusive {
		query = `SELECT pg_advisory_xact_lock($1, hashtext($2))`
	}
	_, err := tx.Exec(ctx, query, clientLockClass, clientID)
	return err
}

// clientActiveTx reports, inside tx and under the client's shared lock,
// whether clientID is registered and enabled: the DB row when there is one
// (it overrides the file entry), the file layer's verdict otherwise.
func clientActiveTx(ctx context.Context, tx pgx.Tx, clientID string, fileEnabled bool) (bool, error) {
	if err := lockClient(ctx, tx, clientID, false); err != nil {
		return false, err
	}
	var enabled bool
	err := tx.QueryRow(ctx,
		`SELECT enabled FROM elitea_auth.native_clients WHERE client_id = $1`, clientID).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return fileEnabled, nil
	}
	if err != nil {
		return false, err
	}
	return enabled, nil
}

func revokeClientFamilies(ctx context.Context, tx pgx.Tx, clientID, reason string, actor int64) (int64, error) {
	ids, err := collectIDs(ctx, tx, `
		SELECT id::text FROM elitea_auth.native_sessions
		WHERE client_id = $1 AND revoked_at IS NULL
		ORDER BY id
		FOR UPDATE`, clientID)
	if err != nil {
		return 0, err
	}
	var by *int64
	if actor > 0 {
		by = &actor
	}
	var count int64
	for _, id := range ids {
		revoked, err := revokeFamily(ctx, tx, id, reason, by)
		if err != nil {
			return 0, err
		}
		if revoked {
			count++
		}
	}
	return count, nil
}

func collectIDs(ctx context.Context, tx pgx.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ActiveDeviceCounts maps client_id to its live family count, for the admin
// list.
func (s *Store) ActiveDeviceCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT client_id, count(*) FROM elitea_auth.native_sessions
		WHERE revoked_at IS NULL GROUP BY client_id`)
	if err != nil {
		return nil, unavailable("count devices", err)
	}
	defer rows.Close()
	counts := map[string]int64{}
	for rows.Next() {
		var clientID string
		var count int64
		if err := rows.Scan(&clientID, &count); err != nil {
			return nil, unavailable("count devices", err)
		}
		counts[clientID] = count
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable("count devices", err)
	}
	return counts, nil
}

// HasDBClients reports whether the DB layer holds any client: the boot check
// that DEPLOYMENT_URL is set once a native client is registered.
func (s *Store) HasDBClients(ctx context.Context) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM elitea_auth.native_clients)`).Scan(&exists)
	if err != nil {
		return false, unavailable("probe native clients", err)
	}
	return exists, nil
}

// formatID is strconv.FormatInt for the principal fields.
func formatID(id int64) string { return strconv.FormatInt(id, 10) }
