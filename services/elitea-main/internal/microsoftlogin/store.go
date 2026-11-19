package microsoftlogin

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// stateRow is a consumed sign-in state.
type stateRow struct {
	digest      [32]byte
	redirectURI string
	ciphertext  []byte
	expiresAt   time.Time
	port        int
	scheme      string
	b           binding
}

// insertState stores a started sign-in, enforcing the per-person cap atomically: a
// transaction takes an advisory lock keyed by the person, counts their open
// sign-ins and inserts, so parallel starts cannot all read the same count. It also
// removes what is dead: handoffs past their 60 seconds, and states an hour past
// their expiry (so a callback that just missed its window still finds its row to
// refuse it).
func (s *Service) insertState(ctx context.Context, digest [32]byte, b binding, shape clientShape,
	redirectURI string, sealed []byte, expires, now time.Time) error {
	var port *int
	if shape.port != 0 {
		port = &shape.port
	}
	var scheme, challenge *string
	if shape.scheme != "" {
		scheme = &shape.scheme
	}
	if shape.challenge != "" {
		challenge = &shape.challenge
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("microsoftlogin: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('elitea/microsoft-login/user/' || ($1::bigint)::text, 0))`, b.userID); err != nil {
		return fmt.Errorf("microsoftlogin: lock the person's sign-ins: %w", err)
	}
	var live int
	if err := tx.QueryRow(ctx, `
SELECT count(*) FROM elitea_identity.microsoft_login_states
 WHERE user_id = $1 AND consumed_at IS NULL AND expires_at > $2`, b.userID, now).Scan(&live); err != nil {
		return fmt.Errorf("microsoftlogin: count open sign-ins: %w", err)
	}
	if live >= MaxLiveSignIns {
		return refuse(CodeTooManySignIns, "Too many sign-ins are open. Finish or wait for them to expire, then try again.")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM elitea_identity.microsoft_login_states WHERE expires_at < $1`, now.Add(-time.Hour)); err != nil {
		return fmt.Errorf("microsoftlogin: prune sign-in states: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM elitea_identity.microsoft_login_handoffs WHERE expires_at < $1`, now); err != nil {
		return fmt.Errorf("microsoftlogin: prune handoffs: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO elitea_identity.microsoft_login_states
    (state_digest, user_id, tenant_project_id, credential_id, credential_type, app_ref, client_kind,
     client_challenge, return_port, return_scheme, client_id_fingerprint, redirect_uri, authority, scope,
     verifier_ciphertext, key_label, expires_at, created_at)
VALUES ($1, $2, $3, $4::uuid, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`,
		digest[:], b.userID, b.projectID, b.credentialID, b.credType, b.appRef, b.clientKind,
		challenge, port, scheme, b.fingerprint, redirectURI, b.authority, b.scope,
		sealed, KeyLabel, expires, now); err != nil {
		return fmt.Errorf("microsoftlogin: store the sign-in state: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("microsoftlogin: store the sign-in state: %w", err)
	}
	return nil
}

// consumeState marks the state used and returns it. A state is single use even when
// the rest of the callback fails.
func (s *Service) consumeState(ctx context.Context, digest [32]byte, now time.Time) (stateRow, error) {
	row := stateRow{digest: digest}
	var port *int
	var scheme, challenge *string
	err := s.pool.QueryRow(ctx, `
UPDATE elitea_identity.microsoft_login_states
   SET consumed_at = $2
 WHERE state_digest = $1 AND consumed_at IS NULL
RETURNING user_id, tenant_project_id, credential_id::text, credential_type, app_ref, client_kind,
          client_challenge, return_port, return_scheme, client_id_fingerprint, redirect_uri, authority, scope,
          verifier_ciphertext, expires_at`, digest[:], now).Scan(
		&row.b.userID, &row.b.projectID, &row.b.credentialID, &row.b.credType, &row.b.appRef, &row.b.clientKind,
		&challenge, &port, &scheme, &row.b.fingerprint, &row.redirectURI, &row.b.authority, &row.b.scope,
		&row.ciphertext, &row.expiresAt)
	if errors.Is(err, errNoRow) {
		return stateRow{}, ErrStateInvalid
	}
	if err != nil {
		return stateRow{}, fmt.Errorf("microsoftlogin: consume the sign-in state: %w", err)
	}
	if port != nil {
		row.port = *port
	}
	if scheme != nil {
		row.scheme = *scheme
	}
	if challenge != nil {
		row.b.challenge = *challenge
	}
	row.expiresAt = row.expiresAt.UTC()
	return row, nil
}

// handoffRow is a parked sign-in result.
type handoffRow struct {
	stateDigest     [32]byte
	userID          int64
	projectID       int64
	credentialID    string
	credType        string
	appRef          string
	clientKind      string
	challenge       string
	fingerprint     string
	authority       string
	scope           string
	tid, oid        string
	accountName     string
	ciphertext      []byte
	accessExpiresAt time.Time
	expiresAt       time.Time
}

func (h handoffRow) binding() binding {
	return binding{
		userID: h.userID, projectID: h.projectID, credentialID: h.credentialID, credType: h.credType,
		appRef: h.appRef, clientKind: h.clientKind, challenge: h.challenge, fingerprint: h.fingerprint,
		authority: h.authority, scope: h.scope,
	}
}

func (s *Service) insertHandoff(ctx context.Context, handoffDigest [32]byte, row handoffRow, now time.Time) error {
	var challenge *string
	if row.challenge != "" {
		challenge = &row.challenge
	}
	// A sealed token is useless a second after its handoff expires: a quiet deployment
	// must not keep it until someone else starts a sign-in.
	if _, err := s.pool.Exec(ctx, `DELETE FROM elitea_identity.microsoft_login_handoffs WHERE expires_at < $1`, now); err != nil {
		return fmt.Errorf("microsoftlogin: prune handoffs: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `
INSERT INTO elitea_identity.microsoft_login_handoffs
    (handoff_digest, state_digest, user_id, tenant_project_id, credential_id, credential_type, app_ref,
     client_kind, client_challenge, client_id_fingerprint, authority, scope, entra_tid, entra_oid, account_name,
     access_token_ciphertext, key_label, access_expires_at, expires_at, created_at)
VALUES ($1, $2, $3, $4, $5::uuid, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)`,
		handoffDigest[:], row.stateDigest[:], row.userID, row.projectID, row.credentialID, row.credType, row.appRef,
		row.clientKind, challenge, row.fingerprint, row.authority, row.scope, row.tid, row.oid, row.accountName,
		row.ciphertext, KeyLabel, row.accessExpiresAt, row.expiresAt, now); err != nil {
		return fmt.Errorf("microsoftlogin: park the handoff: %w", err)
	}
	return nil
}

// takeHandoff deletes the row and returns it, only when the caller is the starter in
// the project with the state and the verifier's challenge, and the row has not
// expired. The attempt that matches is the single use; one that does not match
// leaves the row alone.
func (s *Service) takeHandoff(ctx context.Context, digest, stateDigest [32]byte, userID, projectID int64, challenge string, now time.Time) (handoffRow, error) {
	row := handoffRow{}
	var state []byte
	var rowChallenge *string
	var want *string
	if challenge != "" {
		want = &challenge
	}
	err := s.pool.QueryRow(ctx, `
DELETE FROM elitea_identity.microsoft_login_handoffs
 WHERE handoff_digest = $1 AND state_digest = $2 AND user_id = $3 AND tenant_project_id = $4
   AND expires_at > $5 AND client_challenge IS NOT DISTINCT FROM $6
RETURNING state_digest, user_id, tenant_project_id, credential_id::text, credential_type, app_ref, client_kind,
          client_challenge, client_id_fingerprint, authority, scope, entra_tid, entra_oid, account_name,
          access_token_ciphertext, access_expires_at, expires_at`,
		digest[:], stateDigest[:], userID, projectID, now, want).Scan(
		&state, &row.userID, &row.projectID, &row.credentialID, &row.credType, &row.appRef, &row.clientKind,
		&rowChallenge, &row.fingerprint, &row.authority, &row.scope, &row.tid, &row.oid, &row.accountName,
		&row.ciphertext, &row.accessExpiresAt, &row.expiresAt)
	if errors.Is(err, errNoRow) {
		return handoffRow{}, ErrHandoffInvalid
	}
	if err != nil {
		return handoffRow{}, fmt.Errorf("microsoft login: take the handoff: %w", err)
	}
	copy(row.stateDigest[:], state)
	if rowChallenge != nil {
		row.challenge = *rowChallenge
	}
	row.expiresAt, row.accessExpiresAt = row.expiresAt.UTC(), row.accessExpiresAt.UTC()
	return row, nil
}
