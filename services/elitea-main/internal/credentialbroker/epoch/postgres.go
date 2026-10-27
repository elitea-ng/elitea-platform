package epoch

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Querier is the slice of a pgx pool the store uses.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// PostgresStore reads every epoch from its source table in ONE statement:
//
//	binding            agent_identity_bindings.revocation_epoch, state active
//	mandate            mandates.revocation_epoch, state active, inside not_before..expires_at
//	delegation         delegations.revocation_epoch, state active, unexpired
//	grant              credential_use_grants.epoch, state active, unexpired
//	credential_ref     credential_references.generation, state active or retiring
//	external_identity  external_agent_identities.revocation_epoch, state active
//
// Anything else answers Revoked.
type PostgresStore struct{ db Querier }

// NewPostgresStore builds the store.
func NewPostgresStore(db Querier) (*PostgresStore, error) {
	if db == nil {
		return nil, errors.New("epoch: a database is required")
	}
	return &PostgresStore{db: db}, nil
}

// MaxKeys bounds one read.
const MaxKeys = 32

// ErrInvalidKeys refuses a read with a malformed key or too many keys.
var ErrInvalidKeys = errors.New("epoch: invalid keys")

const readEpochsSQL = `
SELECT k.ord, COALESCE(CASE k.kind
    WHEN 'binding' THEN (SELECT b.revocation_epoch FROM elitea_identity.agent_identity_bindings b
                         WHERE b.id = k.id AND b.state = 'active')
    WHEN 'mandate' THEN (SELECT m.revocation_epoch FROM elitea_identity.mandates m
                         WHERE m.id = k.id AND m.state = 'active'
                           AND m.not_before <= clock_timestamp() AND m.expires_at > clock_timestamp())
    WHEN 'delegation' THEN (SELECT d.revocation_epoch FROM elitea_identity.delegations d
                            WHERE d.id = k.id AND d.state = 'active' AND d.expires_at > clock_timestamp())
    WHEN 'grant' THEN (SELECT g.epoch FROM elitea_identity.credential_use_grants g
                       WHERE g.id = k.id AND g.state = 'active'
                         AND (g.expires_at IS NULL OR g.expires_at > clock_timestamp()))
    WHEN 'credential_ref' THEN (SELECT c.generation FROM elitea_identity.credential_references c
                                WHERE c.id = k.id AND c.state IN ('active', 'retiring'))
    WHEN 'external_identity' THEN (SELECT e.revocation_epoch FROM elitea_identity.external_agent_identities e
                                   WHERE e.id = k.id AND e.state = 'active')
  END, -2)
FROM unnest($1::text[], $2::uuid[]) WITH ORDINALITY AS k(kind, id, ord)
ORDER BY k.ord`

// Read implements Reader. No key sends no statement.
func (s *PostgresStore) Read(ctx context.Context, keys []SecurityEpoch) ([]SecurityEpoch, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	if len(keys) > MaxKeys {
		return nil, ErrInvalidKeys
	}
	kinds := make([]string, len(keys))
	ids := make([]string, len(keys))
	for i, key := range keys {
		if !key.WellFormed() {
			return nil, ErrInvalidKeys
		}
		kinds[i], ids[i] = string(key.Kind), key.ID
	}
	rows, err := s.db.Query(ctx, readEpochsSQL, kinds, ids)
	if err != nil {
		return nil, fmt.Errorf("epoch: read: %w", err)
	}
	defer rows.Close()
	current := make([]SecurityEpoch, len(keys))
	seen := 0
	for rows.Next() {
		var ord, value int64
		if err := rows.Scan(&ord, &value); err != nil {
			return nil, fmt.Errorf("epoch: read: %w", err)
		}
		if ord < 1 || ord > int64(len(keys)) {
			return nil, errors.New("epoch: read answered an unknown key")
		}
		key := keys[ord-1]
		current[ord-1] = SecurityEpoch{Kind: key.Kind, ID: key.ID, Value: value}
		seen++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("epoch: read: %w", err)
	}
	if seen != len(keys) {
		return nil, errors.New("epoch: read answered fewer keys than asked")
	}
	return current, nil
}

// Current implements Checker.
func (s *PostgresStore) Current(ctx context.Context, keys []SecurityEpoch) ([]SecurityEpoch, error) {
	current, err := s.Read(ctx, keys)
	if err != nil {
		return nil, err
	}
	return Stale(keys, current), nil
}

var (
	_ Reader  = (*PostgresStore)(nil)
	_ Checker = (*PostgresStore)(nil)
)
