package identityproviders

// The PostgreSQL store behind `elitea_auth.identity_providers` (shared
// migration 0095).
//
// The store holds no policy. It reads and writes typed provider revisions, and
// Validate is the only thing that decides whether a document may exist. Putting
// a second opinion here would let the admin write path and a future importer
// disagree about what a valid provider is.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// Store reads and writes the platform's identity provider definitions.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const providerColumns = `provider_key, kind, display_name, enabled, revision, config, secret_ref, updated_at`

// List returns every definition, ordered by display name.
//
// Disabled rows are included. An admin listing that hid them would hide the
// definition an operator prepared for a cutover, and the enabled/disabled state
// is the first thing the page shows.
func (s *Store) List(ctx context.Context) ([]Provider, error) {
	if s == nil || s.pool == nil {
		return nil, ErrNoPool
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+providerColumns+`
		   FROM elitea_auth.identity_providers
		  ORDER BY lower(display_name), provider_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	providers := make([]Provider, 0)
	for rows.Next() {
		provider, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		providers = append(providers, provider)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return providers, nil
}

// Lookup resolves one definition by key, enabled or not.
func (s *Store) Lookup(ctx context.Context, key string) (Provider, error) {
	if s == nil || s.pool == nil {
		return Provider{}, ErrNoPool
	}
	normalized := NormalizeKey(key)
	if normalized == "" {
		return Provider{}, ErrNotFound
	}
	row := s.pool.QueryRow(ctx,
		`SELECT `+providerColumns+`
		   FROM elitea_auth.identity_providers
		  WHERE provider_key = $1`, normalized)
	provider, err := scanProvider(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Provider{}, ErrNotFound
	}
	if err != nil {
		return Provider{}, err
	}
	return provider, nil
}

// Enabled resolves the one live definition for a protocol.
//
// This is the read the LOGIN PATH makes, and it is the reason ErrNotFound and a
// read failure must stay distinguishable. "No provider is configured" is a
// deployment that federates no logins and should say so; "the table could not
// be read" is an outage, and answering it as the first would present a
// perfectly configured deployment as an unconfigured one.
func (s *Store) Enabled(ctx context.Context, kind Kind) (Provider, error) {
	if s == nil || s.pool == nil {
		return Provider{}, ErrNoPool
	}
	row := s.pool.QueryRow(ctx,
		`SELECT `+providerColumns+`
		   FROM elitea_auth.identity_providers
		  WHERE kind = $1 AND enabled`, string(kind))
	provider, err := scanProvider(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Provider{}, ErrNotFound
	}
	if err != nil {
		return Provider{}, err
	}
	return provider, nil
}

// Upsert writes one definition and returns it as stored.
//
// The write REPLACES the document. A save from the admin page means "this is
// the definition now", not "merge this into whatever was there" — a merge would
// make removing a scope impossible.
//
// REVISION IS BUMPED BY THE DATABASE, not by the caller. Two operators saving
// at once would otherwise both compute the same next number, and the resolver's
// cache key would stop changing when the document did.
//
// ENABLING IS EXCLUSIVE. A definition being enabled disables every other of its
// kind in the SAME transaction, so the partial unique index never has to refuse
// the save. Leaving the index to reject it would make the operator disable the
// old provider first, and between those two saves the deployment federates no
// logins at all.
//
// SecretRef is written as given. Sealing the plaintext into the vault belongs
// to the admin write path, which owns the vault; this store must not acquire a
// second way to touch secrets.
func (s *Store) Upsert(ctx context.Context, provider Provider) (Provider, error) {
	return s.upsert(ctx, provider, nil)
}

// UpsertAtRevision is Upsert that writes only while the stored row is still
// at expected, the revision the caller read (0: no row yet). Otherwise it
// writes nothing and returns ErrRevisionConflict (issue 2558). The admin
// write path decides whether a save is a super_admin's trust change by
// comparing it with the row it read; without this check a save that matched a
// stale read would silently revert a trust change made in between.
func (s *Store) UpsertAtRevision(ctx context.Context, provider Provider, expected int) (Provider, error) {
	return s.upsert(ctx, provider, &expected)
}

func (s *Store) upsert(ctx context.Context, provider Provider, expected *int) (Provider, error) {
	if s == nil || s.pool == nil {
		return Provider{}, ErrNoPool
	}
	validated, err := Validate(provider)
	if err != nil {
		return Provider{}, err
	}
	document, err := encodeDocument(validated)
	if err != nil {
		return Provider{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Provider{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if validated.Enabled {
		if _, err := tx.Exec(ctx,
			`UPDATE elitea_auth.identity_providers
			    SET enabled = false, updated_at = now()
			  WHERE kind = $1 AND provider_key <> $2 AND enabled`,
			string(validated.Kind), validated.Key,
		); err != nil {
			return Provider{}, err
		}
	}

	row := tx.QueryRow(ctx,
		`INSERT INTO elitea_auth.identity_providers
		     (provider_key, kind, display_name, enabled, revision, config, secret_ref)
		 VALUES ($1, $2, $3, $4, 1, $5, $6)
		 ON CONFLICT (provider_key) DO UPDATE
		     SET kind         = EXCLUDED.kind,
		         display_name = EXCLUDED.display_name,
		         enabled      = EXCLUDED.enabled,
		         config       = EXCLUDED.config,
		         secret_ref   = EXCLUDED.secret_ref,
		         revision     = elitea_auth.identity_providers.revision + 1,
		         updated_at   = now()
		   WHERE $7::integer IS NULL OR elitea_auth.identity_providers.revision = $7::integer
		 RETURNING `+providerColumns,
		validated.Key, string(validated.Kind), validated.DisplayName,
		validated.Enabled, document, validated.SecretRef, expected)
	stored, err := scanProvider(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Provider{}, ErrRevisionConflict
	}
	if err != nil {
		return Provider{}, err
	}
	// The row the caller read was deleted in between, and this write created
	// it again: a create is not the edit the caller was allowed to make.
	if expected != nil && *expected > 0 && stored.Revision == 1 {
		return Provider{}, ErrRevisionConflict
	}
	// The security audit event of the route, in this transaction (ADR-0059
	// R1.5): a failed append rolls the write back.
	if err := securityaudit.RunChangeHook(ctx, tx, stored.Key); err != nil {
		return Provider{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Provider{}, err
	}
	return stored, nil
}

// Delete removes one definition and returns the vault reference it held.
//
// The reference is RETURNED rather than acted on, for the same reason Upsert
// does not seal: the vault belongs to the admin write path. A caller that
// ignores the returned reference leaves an entry nothing reads, which is inert.
func (s *Store) Delete(ctx context.Context, key string) (string, error) {
	if s == nil || s.pool == nil {
		return "", ErrNoPool
	}
	normalized := NormalizeKey(key)
	if normalized == "" {
		return "", ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var reference string
	err = tx.QueryRow(ctx,
		`DELETE FROM elitea_auth.identity_providers
		  WHERE provider_key = $1
		 RETURNING secret_ref`, normalized).Scan(&reference)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	// The security audit event of the route, in this transaction (ADR-0059
	// R1.5).
	if err := securityaudit.RunChangeHook(ctx, tx, normalized); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return reference, nil
}

// IsSchemaMissing reports that the provider table itself is absent.
//
// It exists for ONE caller: the composition root, which reads the table at boot
// to decide whether to mount the browser-auth routes. A deployment whose
// migrations have not been applied yet — a pod that starts before the migration
// job finishes, or a rollback that leaves the binary ahead of the schema — must
// not be turned into a total outage by a federation feature it may not even use.
//
// It is deliberately NOT used on the request path. There, an unreadable table
// stays fatal: refusing one login is right, and falling back to a provider the
// operator replaced is not.
func IsSchemaMissing(err error) bool {
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) {
		return false
	}
	// 42P01 undefined_table, 3F000 invalid_schema_name. Both are what an
	// unapplied migration looks like; neither is what a broken database looks
	// like.
	return pgError.Code == "42P01" || pgError.Code == "3F000"
}

// rowScanner covers both pgx.Row and pgx.Rows, so one scan function serves the
// single-row and the listing paths. Two would drift.
type rowScanner interface {
	Scan(destination ...any) error
}

func scanProvider(row rowScanner) (Provider, error) {
	var (
		provider Provider
		kind     string
		document []byte
		updated  time.Time
	)
	if err := row.Scan(
		&provider.Key, &kind, &provider.DisplayName, &provider.Enabled,
		&provider.Revision, &document, &provider.SecretRef, &updated,
	); err != nil {
		return Provider{}, err
	}
	parsed, err := ParseKind(kind)
	if err != nil {
		return Provider{}, err
	}
	provider.Kind = parsed
	provider.UpdatedAt = updated
	if err := decodeDocument(&provider, document); err != nil {
		return Provider{}, err
	}
	return provider, nil
}

func encodeDocument(provider Provider) ([]byte, error) {
	switch provider.Kind {
	case KindOIDC:
		return json.Marshal(provider.OIDC)
	case KindSAML:
		return json.Marshal(provider.SAML)
	default:
		return nil, ErrUnknownKind
	}
}

// decodeDocument opens the stored document against its row's kind.
//
// A document that will not decode is an ERROR, not an empty one. A zero-valued
// OIDC document has no issuer and no client id, and returning it would present
// a corrupt row to the login path as a provider that simply is not configured.
func decodeDocument(provider *Provider, document []byte) error {
	if len(document) == 0 {
		document = []byte("{}")
	}
	switch provider.Kind {
	case KindOIDC:
		var oidcDocument OIDCDocument
		if err := json.Unmarshal(document, &oidcDocument); err != nil {
			return err
		}
		provider.OIDC = &oidcDocument
	case KindSAML:
		var samlDocument SAMLDocument
		if err := json.Unmarshal(document, &samlDocument); err != nil {
			return err
		}
		provider.SAML = &samlDocument
	default:
		return ErrUnknownKind
	}
	return nil
}
