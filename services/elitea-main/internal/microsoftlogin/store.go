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

// liveStates counts the sign-ins a person has open.
func (s *Service) liveStates(ctx context.Context, userID int64, now time.Time) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `
SELECT count(*) FROM elitea_identity.microsoft_login_states
 WHERE user_id = $1 AND consumed_at IS NULL AND expires_at > $2`, userID, now).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("microsoftlogin: count open sign-ins: %w", err)
	}
	return count, nil
}

// insertState stores a started sign-in. Dead rows are dead weight, so a start
// removes expired states and handoffs (an hour past, so a callback that just missed
// its window still finds its row to refuse it).
func (s *Service) insertState(ctx context.Context, digest [32]byte, b binding, shape clientShape,
	redirectURI string, sealed []byte, expires, now time.Time) error {
	horizon := now.Add(-time.Hour)
	if _, err := s.pool.Exec(ctx, `
DELETE FROM elitea_identity.microsoft_login_states WHERE expires_at < $1`, horizon); err != nil {
		return fmt.Errorf("microsoftlogin: prune sign-in states: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `
DELETE FROM elitea_identity.microsoft_login_handoffs WHERE expires_at < $1`, horizon); err != nil {
		return fmt.Errorf("microsoftlogin: prune handoffs: %w", err)
	}
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
	if _, err := s.pool.Exec(ctx, `
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

// takeHandoff deletes the row and returns it: the attempt itself is the single use.
func (s *Service) takeHandoff(ctx context.Context, digest [32]byte) (handoffRow, error) {
	row := handoffRow{}
	var state []byte
	var challenge *string
	err := s.pool.QueryRow(ctx, `
DELETE FROM elitea_identity.microsoft_login_handoffs
 WHERE handoff_digest = $1
RETURNING state_digest, user_id, tenant_project_id, credential_id::text, credential_type, app_ref, client_kind,
          client_challenge, client_id_fingerprint, authority, scope, entra_tid, entra_oid, account_name,
          access_token_ciphertext, access_expires_at, expires_at`, digest[:]).Scan(
		&state, &row.userID, &row.projectID, &row.credentialID, &row.credType, &row.appRef, &row.clientKind,
		&challenge, &row.fingerprint, &row.authority, &row.scope, &row.tid, &row.oid, &row.accountName,
		&row.ciphertext, &row.accessExpiresAt, &row.expiresAt)
	if errors.Is(err, errNoRow) {
		return handoffRow{}, ErrHandoffInvalid
	}
	if err != nil {
		return handoffRow{}, fmt.Errorf("microsoftlogin: take the handoff: %w", err)
	}
	copy(row.stateDigest[:], state)
	if challenge != nil {
		row.challenge = *challenge
	}
	row.expiresAt, row.accessExpiresAt = row.expiresAt.UTC(), row.accessExpiresAt.UTC()
	return row, nil
}
