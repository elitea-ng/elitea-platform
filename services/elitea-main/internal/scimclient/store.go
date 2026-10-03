package scimclient

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Client is one row of `elitea_auth.scim_clients`, without the hash.
type Client struct {
	ID            int64
	Name          string
	AuthMethod    string
	ClientID      string
	SecretHint    string
	CreatedBy     *int
	CreatedByName string
	CreatedAt     time.Time
	LastUsedAt    *time.Time
	RotatedAt     *time.Time
	RevokedAt     *time.Time
	ExpiresAt     *time.Time
}

// Active reports whether the client can authenticate at now.
func (c Client) Active(now time.Time) bool {
	if c.RevokedAt != nil {
		return false
	}
	return c.ExpiresAt == nil || c.ExpiresAt.After(now)
}

// Issued is a client together with the secret that was minted for it. The
// secret exists only in this value: the store keeps its hash.
type Issued struct {
	Client Client
	Secret string
}

// AccessToken is a minted access token and its lifetime.
type AccessToken struct {
	Token     string
	ExpiresIn time.Duration
}

// lastUsedThrottle is the shortest interval between two `last_used_at` writes
// for one client. An identity provider sync sends many requests a second; the
// admin screen needs "when was this last used", not a write per request.
const lastUsedThrottle = time.Minute

// maxLiveTokensPerClient is how many unexpired access tokens one client may
// hold. Issuing one more deletes the oldest.
const maxLiveTokensPerClient = 20

// Store reads and writes the two SCIM credential tables.
type Store struct {
	pool *pgxpool.Pool
	ttl  time.Duration
	now  func() time.Time

	mu       sync.Mutex
	lastSeen map[int64]time.Time
}

// NewStore builds a store. A ttl of zero selects DefaultAccessTokenTTL.
func NewStore(pool *pgxpool.Pool, ttl time.Duration) *Store {
	if ttl <= 0 {
		ttl = DefaultAccessTokenTTL
	}
	return &Store{pool: pool, ttl: ttl, now: time.Now, lastSeen: map[int64]time.Time{}}
}

// AccessTokenTTL is the lifetime of an access token the store issues.
func (s *Store) AccessTokenTTL() time.Duration { return s.ttl }

const clientColumns = `id, name, auth_method, COALESCE(client_id, ''), secret_hint, created_by,
	created_at, last_used_at, rotated_at, revoked_at, expires_at`

func scanClient(row pgx.Row) (Client, error) {
	var client Client
	err := row.Scan(&client.ID, &client.Name, &client.AuthMethod, &client.ClientID, &client.SecretHint,
		&client.CreatedBy, &client.CreatedAt, &client.LastUsedAt, &client.RotatedAt, &client.RevokedAt,
		&client.ExpiresAt)
	return client, err
}

// Create makes a client and returns its secret once. For client_credentials
// the client identifier is minted too.
//
// expiresAt is optional. When it is set it must be in the future and at most
// MaxClientLifetime away; after it the client stops authenticating, as if it
// were revoked.
func (s *Store) Create(ctx context.Context, name, method string, expiresAt *time.Time, createdBy *int) (Issued, error) {
	name, err := normalizeName(name)
	if err != nil {
		return Issued{}, err
	}
	if expiresAt != nil {
		now := s.now()
		if !expiresAt.After(now) || expiresAt.Sub(now) > MaxClientLifetime {
			return Issued{}, ErrInvalidExpiry
		}
	}
	var secret, clientID string
	switch method {
	case MethodBearer:
		secret, err = randomToken(PrefixBearerSecret, secretBytes)
	case MethodClientCredentials:
		secret, err = randomToken(PrefixClientSecret, secretBytes)
		if err == nil {
			clientID, err = randomToken(PrefixClientID, clientIDBytes)
		}
	default:
		return Issued{}, ErrInvalidMethod
	}
	if err != nil {
		return Issued{}, err
	}
	var clientIDValue any
	if clientID != "" {
		clientIDValue = clientID
	}
	client, err := scanClient(s.pool.QueryRow(ctx, `
		INSERT INTO elitea_auth.scim_clients (name, auth_method, client_id, secret_hash, secret_hint, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+clientColumns,
		name, method, clientIDValue, HashSecret(secret), secretHint(secret), createdBy, expiresAt))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "scim_clients_name" {
			return Issued{}, ErrDuplicateName
		}
		return Issued{}, fmt.Errorf("insert scim client: %w", err)
	}
	return Issued{Client: client, Secret: secret}, nil
}

// List returns every client, newest first. CreatedByName is filled when the
// account table is readable; a deployment without it still lists the rows.
func (s *Store) List(ctx context.Context) ([]Client, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+clientColumns+`
		FROM elitea_auth.scim_clients ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list scim clients: %w", err)
	}
	clients, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Client, error) {
		return scanClient(row)
	})
	if err != nil {
		return nil, fmt.Errorf("scan scim clients: %w", err)
	}
	s.fillCreatorNames(ctx, clients)
	return clients, nil
}

// fillCreatorNames is best effort. The account table belongs to another
// migration corpus, so its absence must not fail the listing.
func (s *Store) fillCreatorNames(ctx context.Context, clients []Client) {
	ids := make([]int32, 0, len(clients))
	for _, client := range clients {
		if client.CreatedBy != nil {
			ids = append(ids, int32(*client.CreatedBy))
		}
	}
	if len(ids) == 0 {
		return
	}
	var present *string
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass('public.auth_core__user')::text`).Scan(&present); err != nil || present == nil {
		return
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, COALESCE(NULLIF(name, ''), email, '') FROM auth_core__user WHERE id = ANY($1)`, ids)
	if err != nil {
		return
	}
	defer rows.Close()
	names := map[int]string{}
	for rows.Next() {
		var id int
		var name string
		if rows.Scan(&id, &name) == nil {
			names[id] = name
		}
	}
	for i := range clients {
		if clients[i].CreatedBy != nil {
			clients[i].CreatedByName = names[*clients[i].CreatedBy]
		}
	}
}

// Get returns one client.
func (s *Store) Get(ctx context.Context, id int64) (Client, error) {
	client, err := scanClient(s.pool.QueryRow(ctx,
		`SELECT `+clientColumns+` FROM elitea_auth.scim_clients WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Client{}, ErrNotFound
	}
	if err != nil {
		return Client{}, fmt.Errorf("read scim client: %w", err)
	}
	return client, nil
}

// Rotate replaces the secret of an active client and returns the new one once.
// The old secret stops working when the transaction commits. For a
// client_credentials client every outstanding access token is deleted too: an
// operator rotates because a secret may have leaked, and a token minted with it
// would otherwise outlive the rotation by up to one TTL.
func (s *Store) Rotate(ctx context.Context, id int64) (Issued, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Issued{}, fmt.Errorf("begin rotate: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	current, err := scanClient(tx.QueryRow(ctx,
		`SELECT `+clientColumns+` FROM elitea_auth.scim_clients WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Issued{}, ErrNotFound
	}
	if err != nil {
		return Issued{}, fmt.Errorf("lock scim client: %w", err)
	}
	if current.RevokedAt != nil {
		return Issued{}, ErrRevoked
	}
	prefix := PrefixBearerSecret
	if current.AuthMethod == MethodClientCredentials {
		prefix = PrefixClientSecret
	}
	secret, err := randomToken(prefix, secretBytes)
	if err != nil {
		return Issued{}, err
	}
	client, err := scanClient(tx.QueryRow(ctx, `
		UPDATE elitea_auth.scim_clients
		SET secret_hash = $2, secret_hint = $3, rotated_at = now()
		WHERE id = $1
		RETURNING `+clientColumns, id, HashSecret(secret), secretHint(secret)))
	if err != nil {
		return Issued{}, fmt.Errorf("rotate scim client: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM elitea_auth.scim_access_tokens WHERE client_id = $1`, id); err != nil {
		return Issued{}, fmt.Errorf("revoke scim access tokens: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Issued{}, fmt.Errorf("commit rotate: %w", err)
	}
	client.CreatedByName = current.CreatedByName
	return Issued{Client: client, Secret: secret}, nil
}

// Revoke stops a client from authenticating and deletes its access tokens. The
// row stays, so the admin screen and the audit trail can still name it.
// Revoking a revoked client is not an error and keeps the first revocation time.
func (s *Store) Revoke(ctx context.Context, id int64) (Client, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Client{}, fmt.Errorf("begin revoke: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	client, err := scanClient(tx.QueryRow(ctx, `
		UPDATE elitea_auth.scim_clients
		SET revoked_at = COALESCE(revoked_at, now())
		WHERE id = $1
		RETURNING `+clientColumns, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Client{}, ErrNotFound
	}
	if err != nil {
		return Client{}, fmt.Errorf("revoke scim client: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM elitea_auth.scim_access_tokens WHERE client_id = $1`, id); err != nil {
		return Client{}, fmt.Errorf("revoke scim access tokens: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Client{}, fmt.Errorf("commit revoke: %w", err)
	}
	return client, nil
}

// Delete removes a client. Its access tokens go with it (ON DELETE CASCADE).
func (s *Store) Delete(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM elitea_auth.scim_clients WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete scim client: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Authenticate resolves a SCIM bearer token. The token is either the secret of
// a bearer client or a live access token from the token endpoint. Anything
// else, a personal access token included, is ErrRejected.
func (s *Store) Authenticate(ctx context.Context, token string) (Principal, error) {
	var (
		principal Principal
		stored    string
		err       error
	)
	hash := HashSecret(token)
	switch {
	case wellFormed(token, PrefixBearerSecret, secretBytes):
		err = s.pool.QueryRow(ctx, `
			SELECT id, name, auth_method, secret_hash FROM elitea_auth.scim_clients
			WHERE secret_hash = $1 AND auth_method = 'bearer'
			  AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())`,
			hash).Scan(&principal.ID, &principal.Name, &principal.Method, &stored)
	case wellFormed(token, PrefixAccessToken, secretBytes):
		err = s.pool.QueryRow(ctx, `
			SELECT c.id, c.name, c.auth_method, t.token_hash
			FROM elitea_auth.scim_access_tokens t
			JOIN elitea_auth.scim_clients c ON c.id = t.client_id
			WHERE t.token_hash = $1 AND t.expires_at > now()
			  -- A token minted with a secret that a rotation replaced never
			  -- authenticates, whatever order the two transactions committed in.
			  AND t.client_secret_hash = c.secret_hash
			  AND c.auth_method = 'client_credentials'
			  AND c.revoked_at IS NULL AND (c.expires_at IS NULL OR c.expires_at > now())`,
			hash).Scan(&principal.ID, &principal.Name, &principal.Method, &stored)
	default:
		return Principal{}, ErrRejected
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrRejected
	}
	if err != nil {
		return Principal{}, fmt.Errorf("resolve scim credential: %w", err)
	}
	// The row was found BY the hash, so this compare is defence in depth: it
	// keeps the decision in constant time even if the lookup changes later.
	if !hashesEqual(stored, hash) {
		return Principal{}, ErrRejected
	}
	return principal, nil
}

// dummyHash is compared against when a client identifier is unknown, so an
// unknown client and a wrong secret take the same path.
var dummyHash = HashSecret("scimcs_unknown-client")

// IssueAccessToken authenticates a client_credentials client and mints an
// access token. Every failure is ErrRejected.
func (s *Store) IssueAccessToken(ctx context.Context, clientID, clientSecret string) (AccessToken, Principal, error) {
	if !wellFormed(clientID, PrefixClientID, clientIDBytes) {
		hashesEqual(dummyHash, HashSecret(clientSecret))
		return AccessToken{}, Principal{}, ErrRejected
	}
	var (
		principal Principal
		stored    string
		active    bool
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, name, auth_method, secret_hash,
		       revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())
		FROM elitea_auth.scim_clients
		WHERE client_id = $1 AND auth_method = 'client_credentials'`,
		clientID).Scan(&principal.ID, &principal.Name, &principal.Method, &stored, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		hashesEqual(dummyHash, HashSecret(clientSecret))
		return AccessToken{}, Principal{}, ErrRejected
	}
	if err != nil {
		return AccessToken{}, Principal{}, fmt.Errorf("resolve scim client: %w", err)
	}
	if !hashesEqual(stored, HashSecret(clientSecret)) || !active {
		return AccessToken{}, Principal{}, ErrRejected
	}

	token, err := randomToken(PrefixAccessToken, secretBytes)
	if err != nil {
		return AccessToken{}, Principal{}, err
	}
	expiresAt := s.now().Add(s.ttl)
	// ONE statement that inserts only while the client still holds the secret
	// that was just verified and is still active. A rotation or a revocation
	// that committed after the read above leaves zero rows here; one that
	// commits after this INSERT is caught by Authenticate's secret-hash join.
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO elitea_auth.scim_access_tokens (token_hash, client_id, client_secret_hash, expires_at)
		SELECT $1, id, secret_hash, $3
		FROM elitea_auth.scim_clients
		WHERE id = $2 AND secret_hash = $4 AND auth_method = 'client_credentials'
		  AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())`,
		HashSecret(token), principal.ID, expiresAt, stored)
	if err != nil {
		return AccessToken{}, Principal{}, fmt.Errorf("insert scim access token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return AccessToken{}, Principal{}, ErrRejected
	}
	// Bound the live tokens per client: an identity provider needs one, and a
	// client that requests a token on every call must not grow the table
	// without limit. The newest maxLiveTokensPerClient survive. Expired rows
	// of every client go too, on the path that creates rows, so no separate
	// reaper is needed. A failure here only delays the cleanup.
	_, _ = s.pool.Exec(ctx, `
		DELETE FROM elitea_auth.scim_access_tokens
		WHERE client_id = $1 AND token_hash NOT IN (
			SELECT token_hash FROM elitea_auth.scim_access_tokens
			WHERE client_id = $1 ORDER BY issued_at DESC, token_hash LIMIT $2)`,
		principal.ID, maxLiveTokensPerClient)
	_, _ = s.pool.Exec(ctx, `DELETE FROM elitea_auth.scim_access_tokens WHERE expires_at < now()`)
	return AccessToken{Token: token, ExpiresIn: s.ttl}, principal, nil
}

// TouchLastUsed records that a client authenticated. At most one write per
// client per lastUsedThrottle and per replica; the SQL guard keeps several
// replicas from writing more often than that between them as well.
func (s *Store) TouchLastUsed(ctx context.Context, id int64) {
	now := s.now()
	s.mu.Lock()
	if last, ok := s.lastSeen[id]; ok && now.Sub(last) < lastUsedThrottle {
		s.mu.Unlock()
		return
	}
	s.lastSeen[id] = now
	// The map holds one entry per client that authenticated. Clients are
	// created by hand, so it stays small; it is reset if it ever is not.
	if len(s.lastSeen) > 10_000 {
		s.lastSeen = map[int64]time.Time{id: now}
	}
	s.mu.Unlock()
	_, _ = s.pool.Exec(ctx, `
		UPDATE elitea_auth.scim_clients SET last_used_at = now()
		WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')`, id)
}
