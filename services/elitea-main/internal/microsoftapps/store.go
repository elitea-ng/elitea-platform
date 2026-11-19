package microsoftapps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// Auditor writes a security audit event in the transaction of the change it
// describes (securityaudit.PostgresAppender).
type Auditor interface {
	Append(ctx context.Context, tx pgx.Tx, e securityaudit.SecurityEvent) error
}

// Actor is who made a change, for updated_by and the audit event.
type Actor struct {
	// UserID is the admin's user id; 0 for the platform itself (the Helm seed).
	UserID int
	// Ref is the audit principal reference, "user:<id>" or "helm-seed".
	Ref string
}

func (a Actor) ref() string {
	if a.Ref != "" {
		return a.Ref
	}
	if a.UserID > 0 {
		return "user:" + strconv.Itoa(a.UserID)
	}
	return "platform"
}

func (a Actor) id() *int {
	if a.UserID > 0 {
		id := a.UserID
		return &id
	}
	return nil
}

// Audit actions, written as ai.elitea.security.v1.config.changed events whose
// ResourceType is "microsoft_oauth_app" and ResourceID the app id. The reasons
// carry the revision and the fields that changed, never a value.
const (
	ActionCreated       = "config.microsoft_oauth_app.created"
	ActionUpdated       = "config.microsoft_oauth_app.updated"
	ActionSecretSet     = "config.microsoft_oauth_app.secret_set"
	ActionSecretCleared = "config.microsoft_oauth_app.secret_cleared"
	ActionDisabled      = "config.microsoft_oauth_app.disabled"
	ActionEnabled       = "config.microsoft_oauth_app.enabled"
	ActionAttention     = "config.microsoft_oauth_app.needs_attention"
	ActionSettings      = "config.microsoft_apps.settings_changed"
	auditResourceType   = "microsoft_oauth_app"
)

// Platform setting `microsoft.custom_apps` (ADR-0057 decision 11): whether a
// credential may use Custom app mode. Default on. It lives in the platform
// settings store like every other deployment setting.
const (
	SettingsSection  = "microsoft"
	KeyCustomApps    = "custom_apps"
	settingsResource = "microsoft_settings"
)

// Store reads and writes the registry. It is the only code that seals or opens a
// client secret.
type Store struct {
	pool   *pgxpool.Pool
	sealer *sealer
	audit  Auditor
	now    func() time.Time
}

// NewStore builds the store. masterKey is the deployment's decoded 32-byte vault
// master key; the store derives its own custody key from it and keeps no copy of
// the master key. A nil audit refuses every write: no change is made unaudited.
func NewStore(pool *pgxpool.Pool, masterKey []byte, audit Auditor) (*Store, error) {
	if pool == nil {
		return nil, ErrNoPool
	}
	if audit == nil {
		return nil, errors.New("microsoftapps: the security audit appender is required")
	}
	sealer, err := newSealer(masterKey)
	if err != nil {
		return nil, err
	}
	return &Store{pool: pool, sealer: sealer, audit: audit, now: time.Now}, nil
}

const appColumns = `id::text, coalesce(seed_key, ''), name, description, client_id, authority,
       client_secret_ciphertext IS NOT NULL, secret_expires_at, credential_types, scope_ceiling,
       auto_refresh_token, project_scope, state, state_reason, created_by, updated_by,
       created_at, updated_at, revision`

type scanner interface{ Scan(dest ...any) error }

func scanApp(row scanner) (App, error) {
	var (
		app     App
		ceiling []byte
		scope   string
		state   string
	)
	err := row.Scan(&app.ID, &app.SeedKey, &app.Name, &app.Description, &app.ClientID, &app.Authority,
		&app.SecretSet, &app.SecretExpiresAt, &app.CredentialTypes, &ceiling,
		&app.AutoRefreshToken, &scope, &state, &app.StateReason, &app.CreatedBy, &app.UpdatedBy,
		&app.CreatedAt, &app.UpdatedAt, &app.Revision)
	if err != nil {
		return App{}, err
	}
	app.ProjectScope, app.State = ProjectScope(scope), State(state)
	app.ScopeCeiling = map[string][]string{}
	if len(ceiling) > 0 {
		if err := json.Unmarshal(ceiling, &app.ScopeCeiling); err != nil {
			return App{}, fmt.Errorf("microsoftapps: decode the scope ceiling: %w", err)
		}
	}
	return app, nil
}

type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func loadProjects(ctx context.Context, q queryer, apps []App) error {
	if len(apps) == 0 {
		return nil
	}
	ids := make([]string, 0, len(apps))
	index := map[string]int{}
	for i, app := range apps {
		ids = append(ids, app.ID)
		index[app.ID] = i
	}
	rows, err := q.Query(ctx, `
SELECT app_id::text, tenant_project_id FROM elitea_identity.microsoft_oauth_app_projects
 WHERE app_id = ANY($1::uuid[]) ORDER BY app_id, tenant_project_id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var project int64
		if err := rows.Scan(&id, &project); err != nil {
			return err
		}
		apps[index[id]].ProjectIDs = append(apps[index[id]].ProjectIDs, project)
	}
	return rows.Err()
}

// List returns every app, disabled ones included, newest name order.
func (s *Store) List(ctx context.Context) ([]App, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+appColumns+` FROM elitea_identity.microsoft_oauth_apps ORDER BY lower(name), id`)
	if err != nil {
		return nil, err
	}
	apps := []App{}
	for rows.Next() {
		app, err := scanApp(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		apps = append(apps, app)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := loadProjects(ctx, s.pool, apps); err != nil {
		return nil, err
	}
	return apps, nil
}

// Get returns one app.
func (s *Store) Get(ctx context.Context, id string) (App, error) {
	if _, err := uuid.Parse(id); err != nil {
		return App{}, ErrNotFound
	}
	row := s.pool.QueryRow(ctx, `SELECT `+appColumns+` FROM elitea_identity.microsoft_oauth_apps WHERE id = $1::uuid`, id)
	app, err := scanApp(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return App{}, ErrNotFound
	}
	if err != nil {
		return App{}, err
	}
	apps := []App{app}
	if err := loadProjects(ctx, s.pool, apps); err != nil {
		return App{}, err
	}
	return apps[0], nil
}

// openSecret opens the sealed secret of an app under its row's AAD. It is the one
// place a registry secret is decrypted.
func (s *Store) openSecret(ctx context.Context, app App) (Secret, error) {
	var ciphertext []byte
	var version int
	err := s.pool.QueryRow(ctx, `
SELECT client_secret_ciphertext, coalesce(key_version, 0)
  FROM elitea_identity.microsoft_oauth_apps WHERE id = $1::uuid`, app.ID).Scan(&ciphertext, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return Secret{}, ErrNotFound
	}
	if err != nil {
		return Secret{}, err
	}
	if len(ciphertext) == 0 {
		return Secret{}, ErrCustodyIntegrity
	}
	return s.sealer.open(ciphertext, secretAAD{AppID: app.ID, ClientID: app.ClientID, KeyVersion: version})
}

// Create registers an app. A secret in the input makes it active; without one it
// is a draft.
func (s *Store) Create(ctx context.Context, in Input, actor Actor) (App, error) {
	return s.create(ctx, "", in, actor)
}

func (s *Store) create(ctx context.Context, seedKey string, in Input, actor Actor) (App, error) {
	in, err := in.Normalize()
	if err != nil {
		return App{}, err
	}
	id := uuid.NewString()
	var ciphertext []byte
	var keyLabel *string
	var keyVersion *int
	state := StateDraft
	if in.Secret != nil && *in.Secret != "" {
		ciphertext = s.sealer.seal(*in.Secret, secretAAD{AppID: id, ClientID: in.ClientID, KeyVersion: CurrentKeyVersion})
		label, version := KeyLabel, CurrentKeyVersion
		keyLabel, keyVersion = &label, &version
		state = StateActive
	}
	ceiling, _ := json.Marshal(in.ScopeCeiling)
	var seed *string
	if seedKey != "" {
		seed = &seedKey
	}
	var created App
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
INSERT INTO elitea_identity.microsoft_oauth_apps
  (id, seed_key, name, description, client_id, client_secret_ciphertext, key_label, key_version,
   secret_expires_at, authority, credential_types, scope_ceiling, auto_refresh_token, project_scope,
   state, created_by, updated_by)
VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::jsonb, $13, $14, $15, $16, $16)
RETURNING `+appColumns,
			id, seed, in.Name, in.Description, in.ClientID, ciphertext, keyLabel, keyVersion,
			in.SecretExpiresAt, in.Authority, in.CredentialTypes, string(ceiling), in.AutoRefreshToken,
			string(in.ProjectScope), string(state), actor.id())
		var err error
		if created, err = scanApp(row); err != nil {
			return err
		}
		if err := replaceProjects(ctx, tx, id, in.ProjectIDs); err != nil {
			return err
		}
		created.ProjectIDs = in.ProjectIDs
		return s.audit.Append(ctx, tx, auditEvent(ActionCreated, actor, id, created.Revision, "state:"+string(state)))
	})
	if isUniqueViolation(err) {
		return App{}, ErrConflict
	}
	if err != nil {
		return App{}, err
	}
	return created, nil
}

func replaceProjects(ctx context.Context, tx pgx.Tx, id string, projects []int64) error {
	if _, err := tx.Exec(ctx, `DELETE FROM elitea_identity.microsoft_oauth_app_projects WHERE app_id = $1::uuid`, id); err != nil {
		return err
	}
	for _, project := range projects {
		if _, err := tx.Exec(ctx, `
INSERT INTO elitea_identity.microsoft_oauth_app_projects (app_id, tenant_project_id) VALUES ($1::uuid, $2)`, id, project); err != nil {
			return err
		}
	}
	return nil
}

// Update replaces an app's definition. A nil Secret keeps the sealed one. The
// secret belongs to its client: changing the client id without a new secret is a
// validation error, because the old secret would not be a secret of the new
// client.
func (s *Store) Update(ctx context.Context, id string, in Input, actor Actor) (App, error) {
	in, err := in.Normalize()
	if err != nil {
		return App{}, err
	}
	if _, err := uuid.Parse(id); err != nil {
		return App{}, ErrNotFound
	}
	var updated App
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+appColumns+` FROM elitea_identity.microsoft_oauth_apps WHERE id = $1::uuid FOR UPDATE`, id)
		current, err := scanApp(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		clientChanged := current.ClientID != in.ClientID
		secretProvided := in.Secret != nil && *in.Secret != ""
		secretCleared := in.Secret != nil && *in.Secret == ""
		if clientChanged && current.SecretSet && !secretProvided && !secretCleared {
			return invalid("secret", "the client id changed: enter the client secret of the new client")
		}

		hasSecret := current.SecretSet
		var ciphertext []byte
		var keyLabel *string
		var keyVersion *int
		replaceSecret := false
		switch {
		case secretProvided:
			ciphertext = s.sealer.seal(*in.Secret, secretAAD{AppID: id, ClientID: in.ClientID, KeyVersion: CurrentKeyVersion})
			label, version := KeyLabel, CurrentKeyVersion
			keyLabel, keyVersion = &label, &version
			hasSecret, replaceSecret = true, true
		case secretCleared:
			hasSecret, replaceSecret = false, true
		}
		state, reason := nextState(current.State, current.StateReason, hasSecret, secretProvided)
		ceiling, _ := json.Marshal(in.ScopeCeiling)
		revision := current.Revision + 1
		if replaceSecret {
			_, err = tx.Exec(ctx, `
UPDATE elitea_identity.microsoft_oauth_apps SET
  name = $2, description = $3, client_id = $4, authority = $5, credential_types = $6,
  scope_ceiling = $7::jsonb, auto_refresh_token = $8, project_scope = $9, secret_expires_at = $10,
  client_secret_ciphertext = $11, key_label = $12, key_version = $13,
  state = $14, state_reason = $15, updated_by = $16, updated_at = now(), revision = $17
WHERE id = $1::uuid`,
				id, in.Name, in.Description, in.ClientID, in.Authority, in.CredentialTypes,
				string(ceiling), in.AutoRefreshToken, string(in.ProjectScope), in.SecretExpiresAt,
				ciphertext, keyLabel, keyVersion, string(state), reason, actor.id(), revision)
		} else {
			_, err = tx.Exec(ctx, `
UPDATE elitea_identity.microsoft_oauth_apps SET
  name = $2, description = $3, client_id = $4, authority = $5, credential_types = $6,
  scope_ceiling = $7::jsonb, auto_refresh_token = $8, project_scope = $9, secret_expires_at = $10,
  state = $11, state_reason = $12, updated_by = $13, updated_at = now(), revision = $14
WHERE id = $1::uuid`,
				id, in.Name, in.Description, in.ClientID, in.Authority, in.CredentialTypes,
				string(ceiling), in.AutoRefreshToken, string(in.ProjectScope), in.SecretExpiresAt,
				string(state), reason, actor.id(), revision)
		}
		if err != nil {
			return err
		}
		if err := replaceProjects(ctx, tx, id, in.ProjectIDs); err != nil {
			return err
		}
		if updated, err = scanApp(tx.QueryRow(ctx, `SELECT `+appColumns+` FROM elitea_identity.microsoft_oauth_apps WHERE id = $1::uuid`, id)); err != nil {
			return err
		}
		updated.ProjectIDs = in.ProjectIDs
		reasons := changedFields(current, in)
		if clientChanged {
			reasons = append(reasons, "client_changed")
		}
		if err := s.audit.Append(ctx, tx, auditEvent(ActionUpdated, actor, id, revision, reasons...)); err != nil {
			return err
		}
		switch {
		case secretProvided:
			return s.audit.Append(ctx, tx, auditEvent(ActionSecretSet, actor, id, revision))
		case secretCleared && current.SecretSet:
			return s.audit.Append(ctx, tx, auditEvent(ActionSecretCleared, actor, id, revision))
		}
		return nil
	})
	if isUniqueViolation(err) {
		return App{}, ErrConflict
	}
	if err != nil {
		return App{}, err
	}
	return updated, nil
}

// nextState is the state after an edit. An administrator's disable survives an
// edit; a platform-set needs_attention clears only when the secret was replaced.
func nextState(current State, reason string, hasSecret, secretReplaced bool) (State, string) {
	switch {
	case current == StateDisabled:
		return StateDisabled, reason
	case !hasSecret:
		return StateDraft, ""
	case current == StateNeedsAttention && !secretReplaced:
		return StateNeedsAttention, reason
	}
	return StateActive, ""
}

func changedFields(current App, in Input) []string {
	var changed []string
	add := func(name string, differs bool) {
		if differs {
			changed = append(changed, "field:"+name)
		}
	}
	add("name", current.Name != in.Name)
	add("description", current.Description != in.Description)
	add("client_id", current.ClientID != in.ClientID)
	add("authority", current.Authority != in.Authority)
	add("credential_types", strings.Join(current.CredentialTypes, ",") != strings.Join(in.CredentialTypes, ","))
	ceilingNow, _ := json.Marshal(current.ScopeCeiling)
	ceilingNew, _ := json.Marshal(in.ScopeCeiling)
	add("scope_ceiling", string(ceilingNow) != string(ceilingNew))
	add("auto_refresh_token", current.AutoRefreshToken != in.AutoRefreshToken)
	add("project_scope", current.ProjectScope != in.ProjectScope)
	nowProjects := append([]int64(nil), current.ProjectIDs...)
	sort.Slice(nowProjects, func(i, j int) bool { return nowProjects[i] < nowProjects[j] })
	add("project_ids", fmt.Sprint(nowProjects) != fmt.Sprint(in.ProjectIDs))
	add("secret_expires_at", !sameTime(current.SecretExpiresAt, in.SecretExpiresAt))
	return changed
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// SetDisabled switches an app off or back on. Enabling a draft keeps it a draft.
func (s *Store) SetDisabled(ctx context.Context, id string, disabled bool, actor Actor) (App, error) {
	if _, err := uuid.Parse(id); err != nil {
		return App{}, ErrNotFound
	}
	var out App
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		current, err := scanApp(tx.QueryRow(ctx, `SELECT `+appColumns+` FROM elitea_identity.microsoft_oauth_apps WHERE id = $1::uuid FOR UPDATE`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		state, reason, action := StateDisabled, "", ActionDisabled
		if !disabled {
			action = ActionEnabled
			state = StateDraft
			if current.SecretSet {
				state = StateActive
			}
		}
		if current.State == state {
			out = current
			return nil
		}
		if _, err := tx.Exec(ctx, `
UPDATE elitea_identity.microsoft_oauth_apps SET state = $2, state_reason = $3, updated_by = $4,
  updated_at = now(), revision = revision + 1 WHERE id = $1::uuid`, id, string(state), reason, actor.id()); err != nil {
			return err
		}
		if out, err = scanApp(tx.QueryRow(ctx, `SELECT `+appColumns+` FROM elitea_identity.microsoft_oauth_apps WHERE id = $1::uuid`, id)); err != nil {
			return err
		}
		return s.audit.Append(ctx, tx, auditEvent(action, actor, id, out.Revision))
	})
	if err != nil {
		return App{}, err
	}
	apps := []App{out}
	if err := loadProjects(ctx, s.pool, apps); err != nil {
		return App{}, err
	}
	return apps[0], nil
}

// MarkNeedsAttention records that the platform found the app unusable (a secret
// that does not open, a failed Test). It never overrides an administrator's
// disable, and a draft has nothing to attend to.
func (s *Store) MarkNeedsAttention(ctx context.Context, id, reason string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
UPDATE elitea_identity.microsoft_oauth_apps SET state = 'needs_attention', state_reason = $2,
  updated_at = now(), revision = revision + 1
 WHERE id = $1::uuid AND state = 'active'`, id, reason)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return s.audit.Append(ctx, tx, auditEvent(ActionAttention, Actor{Ref: "platform"}, id, 0, "reason:"+reason))
	})
}

// CustomAppsAllowed reads `microsoft.custom_apps`: whether a credential may use
// Custom app mode. Absent means allowed.
func (s *Store) CustomAppsAllowed(ctx context.Context) (bool, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT value FROM centry.platform_config WHERE section = $1 AND key = $2`,
		SettingsSection, KeyCustomApps).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return true, err
	}
	var allowed bool
	if json.Unmarshal(raw, &allowed) != nil {
		return true, nil
	}
	return allowed, nil
}

// SetCustomAppsAllowed writes `microsoft.custom_apps` and its audit event.
func (s *Store) SetCustomAppsAllowed(ctx context.Context, allowed bool, actor Actor) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
INSERT INTO centry.platform_config (section, key, value, updated_at, updated_by)
VALUES ($1, $2, $3::jsonb, now() AT TIME ZONE 'UTC', $4)
ON CONFLICT (section, key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at, updated_by = EXCLUDED.updated_by`,
			SettingsSection, KeyCustomApps, strconv.FormatBool(allowed), actor.ref()); err != nil {
			return err
		}
		event := auditEvent(ActionSettings, actor, "custom_apps", 0, "custom_apps:"+strconv.FormatBool(allowed))
		event.ResourceType = settingsResource
		return s.audit.Append(ctx, tx, event)
	})
}

func auditEvent(action string, actor Actor, id string, revision int, reasons ...string) securityaudit.SecurityEvent {
	if revision > 0 {
		reasons = append([]string{"revision:" + strconv.Itoa(revision)}, reasons...)
	}
	return securityaudit.SecurityEvent{
		Type:         securityaudit.TypeConfigChanged,
		Action:       action,
		Effect:       securityaudit.EffectAllow,
		Reasons:      reasons,
		PrincipalRef: actor.ref(),
		ResourceType: auditResourceType,
		ResourceID:   id,
	}
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
