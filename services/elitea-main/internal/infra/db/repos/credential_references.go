package repos

// Credential reference administration (shared 0184 + 0193, issue #1124):
// the lifecycle of elitea_identity.credential_references, the attachment of
// a reference to a toolkit's stored configuration reference, and the agent
// toolkit bindings that pin a toolkit's digest for an agent binding.
//
// Built only with credential governance on. Every write runs in one
// transaction with its securityaudit.TypeDecision event; an append error
// rolls the write back.
//
//   - Staging (migrate-references) writes `staged` only. Activation is the
//     admin API's, by a user who is not the stager (two-person rule, in code
//     and in the 0193 CHECK).
//   - Retire moves any non-retired reference to `retired`. The revocation
//     epoch reader treats a reference outside active/retiring as revoked at
//     any epoch, so the next RedeemCredential of a run that froze it is
//     denied. `generation` is the secret VERSION the broker reads and never
//     moves on a state change.
//   - Attach writes `credential_reference_id` into the one non-private
//     configuration reference of a toolkit's stored settings. That edits the
//     toolkit, so its digest changes: a toolkit binding pinned before the
//     attach no longer matches until it is pinned again.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/epoch"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// Typed refusals of the credential reference store. Each message is safe to
// return to a caller.
var (
	ErrCredentialReferenceTransition = errors.New("credential reference: the transition is not allowed from the current state")
	ErrCredentialReferenceSelfActive = errors.New("credential reference: the activator must differ from the operator who staged it")
	ErrCredentialReferenceNoStager   = errors.New("credential reference: the reference records no stager, so the two-person rule cannot be checked")
	ErrCredentialReferenceRetired    = errors.New("credential reference: the reference is retired")
	ErrCredentialReferenceIncomplete = errors.New("credential reference: the reference's secret copy has not completed; rerun migrate-references --copy-to vault")
	ErrCredentialReferenceActor      = errors.New("credential references: the actor is not an auth_core user id")
	ErrCredentialReferenceTenant     = errors.New("credential reference: the reference belongs to another project")
	ErrCredentialReferenceProvider   = errors.New("credential reference: the toolkit type is not the reference's provider")
	ErrToolkitNotFound               = errors.New("toolkit: no such toolkit in this project")
	ErrToolkitNoConfigurationRef     = errors.New("toolkit: the toolkit holds no configuration reference; it is governed by its digest and a grant alone")
	ErrToolkitConfigurationAmbiguous = errors.New("toolkit: the toolkit holds more than one configuration reference")
	ErrToolkitPrivateConfiguration   = errors.New("toolkit: a private configuration reference is not governed")
	ErrToolkitAttachedElsewhere      = errors.New("toolkit: the toolkit's configuration reference names another credential reference; detach it first")
	ErrAgentBindingNotUsable         = errors.New("agent binding: no such binding in this project, or it is revoked")
)

// CredentialReferenceRecord is one reference with its lifecycle actors and
// its target.
type CredentialReferenceRecord struct {
	credentialgrant.CredentialReference
	TargetIdentity   string
	StagedBy         string
	ActivatedBy      string
	LastTransitionBy string
	MigratedFrom     string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// CredentialReferenceFilter selects a page of one tenant project's
// references.
type CredentialReferenceFilter struct {
	TenantProjectID int64
	State           credentialgrant.ReferenceState
	Limit, Offset   int
}

// CredentialReferenceAction is an admin transition.
type CredentialReferenceAction string

const (
	CredentialReferenceActivate CredentialReferenceAction = "activate"
	CredentialReferenceRetire   CredentialReferenceAction = "retire"
)

// ToolkitAttachment names one toolkit of a tenant project.
type ToolkitAttachment struct {
	ProjectID int64
	ToolkitID int64
}

// ToolkitAttachmentResult is what an attach or a detach changed.
type ToolkitAttachmentResult struct {
	ProjectID             int64
	ToolkitID             int64
	Field                 string
	CredentialReferenceID string
	// ToolkitDigest is the toolkit's digest after the write.
	ToolkitDigest [32]byte
}

// AgentToolkitPin is one written agent toolkit binding.
type AgentToolkitPin struct {
	AgentBindingID  string
	TenantProjectID int64
	ToolkitID       int64
	ToolkitDigest   [32]byte
}

// CredentialReferences is the PostgreSQL store.
type CredentialReferences struct {
	pool     *pgxpool.Pool
	audit    securityaudit.Appender
	notifier epoch.Notifier
	now      func() time.Time
}

// NewCredentialReferences builds the store. A nil audit makes every write
// fail rather than write unaudited.
func NewCredentialReferences(pool *pgxpool.Pool, audit securityaudit.Appender) *CredentialReferences {
	return &CredentialReferences{pool: pool, audit: audit, now: time.Now}
}

// WithNotifier signals n after a committed retire (#1121).
func (s *CredentialReferences) WithNotifier(n epoch.Notifier) *CredentialReferences {
	s.notifier = n
	return s
}

func (s *CredentialReferences) readReady() error {
	if s == nil || s.pool == nil {
		return errors.New("credential references: no database pool")
	}
	return nil
}

func (s *CredentialReferences) writeReady() error {
	if err := s.readReady(); err != nil {
		return err
	}
	if s.audit == nil {
		return errors.New("credential references: no security audit appender")
	}
	return nil
}

const credentialReferenceColumns = `r.id::text, r.tenant_project_id, r.class, r.backend, r.locator, r.provider_type,
       r.target_binding_id::text, r.generation, r.state, t.target_identity,
       COALESCE(r.staged_by::text, ''), COALESCE(r.activated_by::text, ''), COALESCE(r.last_transition_by::text, ''),
       COALESCE(r.migrated_from, ''), r.created_at, r.updated_at`

const credentialReferenceFrom = `elitea_identity.credential_references AS r
JOIN elitea_identity.target_bindings AS t
  ON t.id = r.target_binding_id AND t.tenant_project_id = r.tenant_project_id`

func scanCredentialReference(row pgx.Row) (CredentialReferenceRecord, error) {
	var (
		rec          CredentialReferenceRecord
		class, state string
	)
	err := row.Scan(&rec.ID, &rec.TenantProjectID, &class, &rec.Backend, &rec.Locator, &rec.ProviderType,
		&rec.TargetBindingID, &rec.Generation, &state, &rec.TargetIdentity,
		&rec.StagedBy, &rec.ActivatedBy, &rec.LastTransitionBy, &rec.MigratedFrom, &rec.CreatedAt, &rec.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CredentialReferenceRecord{}, credentialgrant.ErrReferenceNotFound
	}
	if err != nil {
		return CredentialReferenceRecord{}, fmt.Errorf("scan credential reference: %w", err)
	}
	rec.Class, rec.State = credentialgrant.CredentialClass(class), credentialgrant.ReferenceState(state)
	rec.CreatedAt, rec.UpdatedAt = rec.CreatedAt.UTC(), rec.UpdatedAt.UTC()
	return rec, nil
}

// Get reads one reference by id.
func (s *CredentialReferences) Get(ctx context.Context, id string) (CredentialReferenceRecord, error) {
	if err := s.readReady(); err != nil {
		return CredentialReferenceRecord{}, err
	}
	if !agentIdentityIsUUID(id) {
		return CredentialReferenceRecord{}, credentialgrant.ErrReferenceNotFound
	}
	return scanCredentialReference(s.pool.QueryRow(ctx,
		`SELECT `+credentialReferenceColumns+` FROM `+credentialReferenceFrom+` WHERE r.id = $1::uuid`, id))
}

// List reads a page of one tenant project's references, newest first.
func (s *CredentialReferences) List(ctx context.Context, filter CredentialReferenceFilter) ([]CredentialReferenceRecord, error) {
	if err := s.readReady(); err != nil {
		return nil, err
	}
	if filter.TenantProjectID <= 0 {
		return nil, errors.New("credential references: a tenant project is required")
	}
	limit, offset := agentIdentityListWindow(filter.Limit, filter.Offset)
	rows, err := s.pool.Query(ctx, `SELECT `+credentialReferenceColumns+` FROM `+credentialReferenceFrom+`
WHERE r.tenant_project_id = $1 AND ($2 = '' OR r.state = $2)
ORDER BY r.created_at DESC, r.id
LIMIT $3 OFFSET $4`, filter.TenantProjectID, string(filter.State), limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list credential references: %w", err)
	}
	defer rows.Close()
	out := []CredentialReferenceRecord{}
	for rows.Next() {
		rec, err := scanCredentialReference(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list credential references: %w", err)
	}
	return out, nil
}

// Transition applies an admin action under SELECT ... FOR UPDATE, so two
// concurrent activations give one success and one
// ErrCredentialReferenceTransition.
func (s *CredentialReferences) Transition(ctx context.Context, id string, action CredentialReferenceAction, actor string) (CredentialReferenceRecord, error) {
	if err := s.writeReady(); err != nil {
		return CredentialReferenceRecord{}, err
	}
	actorID, actor, err := CanonicalUserID(actor)
	if err != nil {
		return CredentialReferenceRecord{}, err
	}
	if !agentIdentityIsUUID(id) {
		return CredentialReferenceRecord{}, credentialgrant.ErrReferenceNotFound
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return CredentialReferenceRecord{}, fmt.Errorf("begin credential reference transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, err := scanCredentialReference(tx.QueryRow(ctx,
		`SELECT `+credentialReferenceColumns+` FROM `+credentialReferenceFrom+` WHERE r.id = $1::uuid FOR UPDATE OF r`, id))
	if err != nil {
		return CredentialReferenceRecord{}, err
	}
	var next credentialgrant.ReferenceState
	var activatedBy *int64
	if current.ActivatedBy != "" {
		previous, _, err := CanonicalUserID(current.ActivatedBy)
		if err != nil {
			return CredentialReferenceRecord{}, err
		}
		activatedBy = &previous
	}
	switch action {
	case CredentialReferenceActivate:
		if current.State != credentialgrant.ReferenceStaged {
			return CredentialReferenceRecord{}, ErrCredentialReferenceTransition
		}
		if current.StagedBy == "" {
			return CredentialReferenceRecord{}, ErrCredentialReferenceNoStager
		}
		// Both sides are canonical decimal ids (bigint columns, CanonicalUserID).
		if current.StagedBy == actor {
			return CredentialReferenceRecord{}, ErrCredentialReferenceSelfActive
		}
		if current.Class == credentialgrant.ClassStaticSecret && current.Generation == 0 {
			// A --copy-to vault reference is staged at generation 0 and
			// gets its version once the copy is written; until then there
			// is no version for the broker to read.
			return CredentialReferenceRecord{}, ErrCredentialReferenceIncomplete
		}
		next, activatedBy = credentialgrant.ReferenceActive, &actorID
	case CredentialReferenceRetire:
		if current.State == credentialgrant.ReferenceRetired {
			return CredentialReferenceRecord{}, ErrCredentialReferenceTransition
		}
		next = credentialgrant.ReferenceRetired
	default:
		return CredentialReferenceRecord{}, ErrCredentialReferenceTransition
	}
	tag, err := tx.Exec(ctx, `
UPDATE elitea_identity.credential_references
SET state = $2, activated_by = $3, last_transition_by = $4, updated_at = $5
WHERE id = $1::uuid AND state = $6`,
		id, string(next), activatedBy, actorID, s.now().UTC(), string(current.State))
	if err != nil {
		return CredentialReferenceRecord{}, mapCredentialReferenceWriteError(err)
	}
	if tag.RowsAffected() != 1 {
		return CredentialReferenceRecord{}, ErrCredentialReferenceTransition
	}
	written, err := scanCredentialReference(tx.QueryRow(ctx,
		`SELECT `+credentialReferenceColumns+` FROM `+credentialReferenceFrom+` WHERE r.id = $1::uuid`, id))
	if err != nil {
		return CredentialReferenceRecord{}, err
	}
	event := credentialReferenceEvent(written, "credential_reference."+string(action), actor,
		"state."+string(written.State), "from."+string(current.State))
	if err := s.audit.Append(ctx, tx, event); err != nil {
		return CredentialReferenceRecord{}, fmt.Errorf("audit credential reference transition: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CredentialReferenceRecord{}, mapCredentialReferenceWriteError(fmt.Errorf("commit credential reference transition: %w", err))
	}
	if s.notifier != nil && written.State == credentialgrant.ReferenceRetired {
		s.notifier.Notify(ctx, epoch.Change{
			Epoch:     epoch.SecurityEpoch{Kind: epoch.KindCredentialRef, ID: written.ID, Value: written.Generation},
			ChangedAt: written.UpdatedAt,
		})
	}
	return written, nil
}

// CanonicalUserID parses an auth_core user id (a positive decimal int64)
// and renders it canonically, so "007" and "7" are the same person to the
// two-person rule. Anything else is ErrCredentialReferenceActor.
func CanonicalUserID(value string) (int64, string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "+") {
		return 0, "", ErrCredentialReferenceActor
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, "", ErrCredentialReferenceActor
	}
	return id, strconv.FormatInt(id, 10), nil
}

func mapCredentialReferenceWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == "credential_references_activator_not_stager" {
		return ErrCredentialReferenceSelfActive
	}
	return err
}

func credentialReferenceEvent(rec CredentialReferenceRecord, action, actor string, reasons ...string) securityaudit.SecurityEvent {
	return securityaudit.SecurityEvent{
		Type:            securityaudit.TypeDecision,
		Time:            rec.UpdatedAt,
		TenantProjectID: rec.TenantProjectID,
		Action:          action,
		Effect:          securityaudit.EffectAllow,
		Reasons:         reasons,
		PrincipalRef:    auditUserRef(actor),
		ResourceType:    "credential_reference",
		ResourceID:      rec.ID,
		RevocationEpoch: rec.Generation,
	}
}

// toolkitReferenceField finds the one configuration reference of stored
// toolkit settings. The shape is configurations.ToolkitCredentialRefs's, the
// one the governed resolver reads, so attach and resolve cannot disagree on
// the field; only the field's name is added here.
func toolkitReferenceField(settings map[string]any) (string, configurationapp.ToolkitCredentialRef, error) {
	fields := configurationapp.ToolkitCredentialRefFields(settings)
	refs := configurationapp.ToolkitCredentialRefs(settings)
	switch {
	case len(refs) == 0:
		return "", configurationapp.ToolkitCredentialRef{}, ErrToolkitNoConfigurationRef
	case len(refs) > 1:
		return "", configurationapp.ToolkitCredentialRef{}, ErrToolkitConfigurationAmbiguous
	}
	return fields[0], refs[0], nil
}

// lockedToolkit reads a tenant toolkit row FOR UPDATE inside tx and decodes
// its settings the way the admission's toolkit reader does, so a digest
// computed here equals the digest admission computes.
func lockedToolkit(ctx context.Context, tx pgx.Tx, projectID, toolkitID int64) (string, string, map[string]any, error) {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil || toolkitID <= 0 {
		return "", "", nil, ErrToolkitNotFound
	}
	var (
		toolkitType string
		raw         []byte
	)
	err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT type, settings::text FROM %s.elitea_tools WHERE id = $1 FOR UPDATE`, schema),
		toolkitID).Scan(&toolkitType, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil, ErrToolkitNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
		// No tenant schema: the project does not exist.
		return "", "", nil, ErrToolkitNotFound
	}
	if err != nil {
		return "", "", nil, fmt.Errorf("read the toolkit: %w", err)
	}
	decoded, err := decodeCurrentToolkitJSON(raw, "settings")
	if err != nil {
		return "", "", nil, err
	}
	settings, ok := decoded.(map[string]any)
	if !ok || settings == nil {
		return "", "", nil, ErrInvalidCurrentToolkitRow
	}
	return schema, toolkitType, settings, nil
}

// Attach writes the reference id into the toolkit's one non-private
// configuration reference. Attaching the same reference again changes
// nothing and succeeds.
func (s *CredentialReferences) Attach(ctx context.Context, referenceID string, target ToolkitAttachment, actor string) (ToolkitAttachmentResult, error) {
	return s.writeAttachment(ctx, referenceID, target, actor, true)
}

// Detach removes the reference id from the toolkit's configuration
// reference. Detaching a toolkit that names no reference succeeds.
func (s *CredentialReferences) Detach(ctx context.Context, referenceID string, target ToolkitAttachment, actor string) (ToolkitAttachmentResult, error) {
	return s.writeAttachment(ctx, referenceID, target, actor, false)
}

func (s *CredentialReferences) writeAttachment(ctx context.Context, referenceID string, target ToolkitAttachment, actor string, attach bool) (ToolkitAttachmentResult, error) {
	if err := s.writeReady(); err != nil {
		return ToolkitAttachmentResult{}, err
	}
	if strings.TrimSpace(actor) == "" {
		return ToolkitAttachmentResult{}, errors.New("credential references: the actor is unknown")
	}
	if !agentIdentityIsUUID(referenceID) {
		return ToolkitAttachmentResult{}, credentialgrant.ErrReferenceNotFound
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ToolkitAttachmentResult{}, fmt.Errorf("begin toolkit attachment: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Lock order: the reference, then the toolkit, then the audit head.
	ref, err := scanCredentialReference(tx.QueryRow(ctx,
		`SELECT `+credentialReferenceColumns+` FROM `+credentialReferenceFrom+` WHERE r.id = $1::uuid FOR SHARE OF r`, referenceID))
	if err != nil {
		return ToolkitAttachmentResult{}, err
	}
	if ref.TenantProjectID != target.ProjectID {
		return ToolkitAttachmentResult{}, ErrCredentialReferenceTenant
	}
	if attach && ref.State == credentialgrant.ReferenceRetired {
		return ToolkitAttachmentResult{}, ErrCredentialReferenceRetired
	}
	schema, toolkitType, settings, err := lockedToolkit(ctx, tx, target.ProjectID, target.ToolkitID)
	if err != nil {
		return ToolkitAttachmentResult{}, err
	}
	if toolkitType != ref.ProviderType {
		return ToolkitAttachmentResult{}, ErrCredentialReferenceProvider
	}
	field, configRef, err := toolkitReferenceField(settings)
	if err != nil {
		return ToolkitAttachmentResult{}, err
	}
	if configRef.Private && attach {
		return ToolkitAttachmentResult{}, ErrToolkitPrivateConfiguration
	}
	object, _ := settings[field].(map[string]any)
	existing := configRef.CredentialReferenceID
	if existing != "" && existing != referenceID {
		return ToolkitAttachmentResult{}, ErrToolkitAttachedElsewhere
	}
	result := ToolkitAttachmentResult{ProjectID: target.ProjectID, ToolkitID: target.ToolkitID, Field: field}
	switch {
	case attach && existing != referenceID:
		object[configurationapp.CredentialReferenceIDField] = referenceID
		_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.elitea_tools
SET settings = jsonb_set(settings::jsonb, ARRAY[$2::text, $3::text], to_jsonb($4::text)) WHERE id = $1`, schema),
			target.ToolkitID, field, configurationapp.CredentialReferenceIDField, referenceID)
	case !attach && existing == referenceID:
		delete(object, configurationapp.CredentialReferenceIDField)
		_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.elitea_tools
SET settings = settings::jsonb #- ARRAY[$2::text, $3::text] WHERE id = $1`, schema),
			target.ToolkitID, field, configurationapp.CredentialReferenceIDField)
	}
	if err != nil {
		return ToolkitAttachmentResult{}, fmt.Errorf("write the toolkit attachment: %w", err)
	}
	if attach {
		result.CredentialReferenceID = referenceID
	}
	if result.ToolkitDigest, err = credentialgrant.ToolkitDigest(toolkitType, settings); err != nil {
		return ToolkitAttachmentResult{}, err
	}
	action := "credential_reference.attach"
	if !attach {
		action = "credential_reference.detach"
	}
	event := credentialReferenceEvent(ref.withoutTime(), action, actor, "toolkit."+strconv.FormatInt(target.ToolkitID, 10))
	if err := s.audit.Append(ctx, tx, event); err != nil {
		return ToolkitAttachmentResult{}, fmt.Errorf("audit toolkit attachment: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ToolkitAttachmentResult{}, fmt.Errorf("commit toolkit attachment: %w", err)
	}
	return result, nil
}

// withoutTime leaves the event time to the appender's clock.
func (r CredentialReferenceRecord) withoutTime() CredentialReferenceRecord {
	r.UpdatedAt = time.Time{}
	return r
}

// PinToolkit writes (or re-pins) the agent toolkit binding of a toolkit at
// its CURRENT stored digest. The agent binding must be in the project and
// not revoked.
func (s *CredentialReferences) PinToolkit(ctx context.Context, agentBindingID string, target ToolkitAttachment, actor string) (AgentToolkitPin, error) {
	if err := s.writeReady(); err != nil {
		return AgentToolkitPin{}, err
	}
	if strings.TrimSpace(actor) == "" {
		return AgentToolkitPin{}, errors.New("credential references: the actor is unknown")
	}
	if !agentIdentityIsUUID(agentBindingID) {
		return AgentToolkitPin{}, ErrAgentBindingNotUsable
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AgentToolkitPin{}, fmt.Errorf("begin toolkit pin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var state string
	err = tx.QueryRow(ctx, `SELECT state FROM elitea_identity.agent_identity_bindings
WHERE id = $1::uuid AND tenant_project_id = $2 FOR SHARE`, agentBindingID, target.ProjectID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && state == "revoked") {
		return AgentToolkitPin{}, ErrAgentBindingNotUsable
	}
	if err != nil {
		return AgentToolkitPin{}, fmt.Errorf("read the agent binding: %w", err)
	}
	_, toolkitType, settings, err := lockedToolkit(ctx, tx, target.ProjectID, target.ToolkitID)
	if err != nil {
		return AgentToolkitPin{}, err
	}
	digest, err := credentialgrant.ToolkitDigest(toolkitType, settings)
	if err != nil {
		return AgentToolkitPin{}, err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO elitea_identity.agent_toolkit_bindings (agent_binding_id, toolkit_id, tenant_project_id, toolkit_digest)
VALUES ($1::uuid, $2, $3, $4)
ON CONFLICT (agent_binding_id, toolkit_id) DO UPDATE SET toolkit_digest = EXCLUDED.toolkit_digest`,
		agentBindingID, target.ToolkitID, target.ProjectID, digest[:]); err != nil {
		return AgentToolkitPin{}, fmt.Errorf("write the agent toolkit binding: %w", err)
	}
	event := securityaudit.SecurityEvent{
		Type:            securityaudit.TypeDecision,
		TenantProjectID: target.ProjectID,
		Action:          "agent_toolkit_binding.pin",
		Effect:          securityaudit.EffectAllow,
		Reasons:         []string{"toolkit." + strconv.FormatInt(target.ToolkitID, 10)},
		PrincipalRef:    auditUserRef(actor),
		AgentBindingID:  agentBindingID,
		ResourceType:    "agent_toolkit_binding",
		ResourceID:      strconv.FormatInt(target.ToolkitID, 10),
	}
	if err := s.audit.Append(ctx, tx, event); err != nil {
		return AgentToolkitPin{}, fmt.Errorf("audit toolkit pin: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentToolkitPin{}, fmt.Errorf("commit toolkit pin: %w", err)
	}
	return AgentToolkitPin{AgentBindingID: agentBindingID, TenantProjectID: target.ProjectID,
		ToolkitID: target.ToolkitID, ToolkitDigest: digest}, nil
}

// UnpinToolkit removes an agent toolkit binding. A governed run of the
// binding then no longer governs, and so no longer redeems for, the toolkit.
func (s *CredentialReferences) UnpinToolkit(ctx context.Context, agentBindingID string, target ToolkitAttachment, actor string) error {
	if err := s.writeReady(); err != nil {
		return err
	}
	if strings.TrimSpace(actor) == "" {
		return errors.New("credential references: the actor is unknown")
	}
	if !agentIdentityIsUUID(agentBindingID) {
		return ErrAgentBindingNotUsable
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM elitea_identity.agent_toolkit_bindings
WHERE agent_binding_id = $1::uuid AND toolkit_id = $2 AND tenant_project_id = $3`,
			agentBindingID, target.ToolkitID, target.ProjectID)
		if err != nil {
			return fmt.Errorf("delete the agent toolkit binding: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrToolkitNotFound
		}
		return s.audit.Append(ctx, tx, securityaudit.SecurityEvent{
			Type:            securityaudit.TypeDecision,
			TenantProjectID: target.ProjectID,
			Action:          "agent_toolkit_binding.unpin",
			Effect:          securityaudit.EffectAllow,
			Reasons:         []string{"toolkit." + strconv.FormatInt(target.ToolkitID, 10)},
			PrincipalRef:    auditUserRef(actor),
			AgentBindingID:  agentBindingID,
			ResourceType:    "agent_toolkit_binding",
			ResourceID:      strconv.FormatInt(target.ToolkitID, 10),
		})
	})
}

// StagedReference is one reference migrate-references stages.
type StagedReference struct {
	TenantProjectID int64
	ProviderType    string
	TargetIdentity  string
	Class           credentialgrant.CredentialClass
	Backend         string
	Locator         string
	Generation      int64
	MigratedFrom    string
	// StagedBy is the operator's auth_core user id (CanonicalUserID).
	StagedBy string
}

// StageResult says what StageReference found or wrote.
type StageResult struct {
	Record  CredentialReferenceRecord
	Created bool
}

// FindTarget reads the target binding of a canonical identity; empty when
// there is none.
func (s *CredentialReferences) FindTarget(ctx context.Context, tenantProjectID int64, providerType, identity string) (string, error) {
	if err := s.readReady(); err != nil {
		return "", err
	}
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id::text FROM elitea_identity.target_bindings
WHERE tenant_project_id = $1 AND provider_type = $2 AND target_identity = $3 AND audience = ''`,
		tenantProjectID, providerType, identity).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// liveMigratedFrom selects the one reference of a migration source that is
// not retired: the partial unique index of 0193 covers exactly these rows,
// so a retired reference never blocks staging its replacement.
const liveMigratedFrom = `r.tenant_project_id = $1 AND r.migrated_from = $2 AND r.state <> 'retired'`

// FindMigrated reads the live (not retired) reference a migration source
// staged.
func (s *CredentialReferences) FindMigrated(ctx context.Context, tenantProjectID int64, migratedFrom string) (CredentialReferenceRecord, bool, error) {
	if err := s.readReady(); err != nil {
		return CredentialReferenceRecord{}, false, err
	}
	rec, err := scanCredentialReference(s.pool.QueryRow(ctx, `SELECT `+credentialReferenceColumns+` FROM `+credentialReferenceFrom+`
WHERE `+liveMigratedFrom, tenantProjectID, migratedFrom))
	if errors.Is(err, credentialgrant.ErrReferenceNotFound) {
		return CredentialReferenceRecord{}, false, nil
	}
	return rec, err == nil, err
}

// StageReference upserts the target binding and inserts the reference in
// `staged`, with its audit event, in one transaction. A live reference
// already staged from the same source is returned unchanged (Created false);
// a retired one is not, so a rerun after a retire stages its replacement.
func (s *CredentialReferences) StageReference(ctx context.Context, staged StagedReference, newID func() string) (StageResult, error) {
	if err := s.writeReady(); err != nil {
		return StageResult{}, err
	}
	if staged.TenantProjectID <= 0 || staged.MigratedFrom == "" || !staged.Class.Valid() {
		return StageResult{}, errors.New("credential references: a staged reference needs a tenant, a class, a source and a stager")
	}
	stagerID, stager, err := CanonicalUserID(staged.StagedBy)
	if err != nil {
		return StageResult{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return StageResult{}, fmt.Errorf("begin staging: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// DO UPDATE (a no-op write of a key column), not DO NOTHING: under
	// concurrency DO NOTHING returns no row and a SELECT in the same
	// statement cannot see the row another transaction just committed, so
	// the upsert came back empty. DO UPDATE waits for that row and returns
	// its id.
	var targetID string
	err = tx.QueryRow(ctx, `
INSERT INTO elitea_identity.target_bindings (id, tenant_project_id, provider_type, target_identity, audience)
VALUES ($1::uuid, $2, $3, $4, '')
ON CONFLICT (tenant_project_id, provider_type, target_identity, audience)
DO UPDATE SET target_identity = EXCLUDED.target_identity
RETURNING id::text`, newID(), staged.TenantProjectID, staged.ProviderType, staged.TargetIdentity).Scan(&targetID)
	if err != nil {
		return StageResult{}, fmt.Errorf("stage the target binding: %w", err)
	}
	id := newID()
	tag, err := tx.Exec(ctx, `
INSERT INTO elitea_identity.credential_references
    (id, tenant_project_id, class, backend, locator, provider_type, target_binding_id, generation, state,
     staged_by, last_transition_by, migrated_from)
VALUES ($1::uuid, $2, $3, $4, $5, $6, $7::uuid, $8, 'staged', $9, $9, $10)
ON CONFLICT (tenant_project_id, migrated_from) WHERE migrated_from IS NOT NULL AND state <> 'retired' DO NOTHING`,
		id, staged.TenantProjectID, string(staged.Class), staged.Backend, staged.Locator, staged.ProviderType,
		targetID, staged.Generation, stagerID, staged.MigratedFrom)
	if err != nil {
		return StageResult{}, fmt.Errorf("stage the credential reference: %w", err)
	}
	created := tag.RowsAffected() == 1
	rec, err := scanCredentialReference(tx.QueryRow(ctx, `SELECT `+credentialReferenceColumns+` FROM `+credentialReferenceFrom+`
WHERE `+liveMigratedFrom, staged.TenantProjectID, staged.MigratedFrom))
	if err != nil {
		return StageResult{}, err
	}
	if !created {
		// Nothing was written; nothing to audit.
		return StageResult{Record: rec}, nil
	}
	event := credentialReferenceEvent(rec.withoutTime(), "credential_reference.stage", stager,
		"state.staged", "class."+string(rec.Class))
	if err := s.audit.Append(ctx, tx, event); err != nil {
		return StageResult{}, fmt.Errorf("audit staging: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return StageResult{}, fmt.Errorf("commit staging: %w", err)
	}
	return StageResult{Record: rec, Created: true}, nil
}

// SetCopiedGeneration records the version a migration write created (a
// --copy-to vault copy, or a composite key map's pgvault version 1) on the
// staged reference that was staged for it at generation 0,
// with its audit event. A reference already at that generation succeeds
// unchanged (a concurrent run finished first); any other state or
// generation is ErrCredentialReferenceTransition.
func (s *CredentialReferences) SetCopiedGeneration(ctx context.Context, id string, generation int64, actor string) (CredentialReferenceRecord, error) {
	if err := s.writeReady(); err != nil {
		return CredentialReferenceRecord{}, err
	}
	_, actor, err := CanonicalUserID(actor)
	if err != nil {
		return CredentialReferenceRecord{}, err
	}
	if !agentIdentityIsUUID(id) || generation <= 0 {
		return CredentialReferenceRecord{}, credentialgrant.ErrReferenceNotFound
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return CredentialReferenceRecord{}, fmt.Errorf("begin generation write: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, err := scanCredentialReference(tx.QueryRow(ctx,
		`SELECT `+credentialReferenceColumns+` FROM `+credentialReferenceFrom+` WHERE r.id = $1::uuid FOR UPDATE OF r`, id))
	if err != nil {
		return CredentialReferenceRecord{}, err
	}
	if current.Generation == generation && current.State == credentialgrant.ReferenceStaged {
		return current, nil
	}
	// A pgvault secret has one version: a composite key map written to a
	// new pgvault name (#1481) is version 1.
	writable := current.Backend == "vault" || (current.Backend == "pgvault" && generation == 1)
	if current.Generation != 0 || current.State != credentialgrant.ReferenceStaged || !writable {
		return CredentialReferenceRecord{}, ErrCredentialReferenceTransition
	}
	if _, err := tx.Exec(ctx, `UPDATE elitea_identity.credential_references SET generation = $2, updated_at = $3
WHERE id = $1::uuid`, id, generation, s.now().UTC()); err != nil {
		return CredentialReferenceRecord{}, fmt.Errorf("write the copied generation: %w", err)
	}
	written, err := scanCredentialReference(tx.QueryRow(ctx,
		`SELECT `+credentialReferenceColumns+` FROM `+credentialReferenceFrom+` WHERE r.id = $1::uuid`, id))
	if err != nil {
		return CredentialReferenceRecord{}, err
	}
	event := credentialReferenceEvent(written, "credential_reference.copy_committed", actor,
		"generation."+strconv.FormatInt(generation, 10))
	if err := s.audit.Append(ctx, tx, event); err != nil {
		return CredentialReferenceRecord{}, fmt.Errorf("audit the copied generation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CredentialReferenceRecord{}, fmt.Errorf("commit the copied generation: %w", err)
	}
	return written, nil
}

// RecordSecretCopy appends the audit decision that names a migration's
// secret read (decisionID is the StoreContext.DecisionID of the read):
// `credential_reference.copy_to_vault` for a --copy-to vault copy, and
// `credential_reference.copy_to_pgvault` for a composite key map written to
// a new pgvault name (#1481).
func (s *CredentialReferences) RecordSecretCopy(ctx context.Context, tenantProjectID int64, referenceID, decisionID, actor, backend string) error {
	if err := s.writeReady(); err != nil {
		return err
	}
	if backend != "vault" && backend != "pgvault" {
		return fmt.Errorf("credential references: no secret copy to backend %q", backend)
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return s.audit.Append(ctx, tx, securityaudit.SecurityEvent{
			Type:            securityaudit.TypeDecision,
			TenantProjectID: tenantProjectID,
			Action:          "credential_reference.copy_to_" + backend,
			Effect:          securityaudit.EffectAllow,
			Reasons:         []string{"operator.migrate_references"},
			PrincipalRef:    auditUserRef(actor),
			DecisionID:      decisionID,
			ResourceType:    "credential_reference",
			ResourceID:      referenceID,
		})
	})
}
