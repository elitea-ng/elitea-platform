package repos

// The mandate attachments of unattended runs (tenant 0158, issue #1122):
// pipeline_schedules.mandate_id, pipeline_triggers.mandate_id and
// index_schedule_mandates, and the security audit events of attaching,
// detaching and refusing. Built only with credential governance on.
//
// Every attach and detach appends a securityaudit.TypeDecision event in the
// same transaction as the write; an append error rolls the write back. A
// refused fire appends its event in a transaction of its own: nothing else
// is written for a run that did not start.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// ErrAttachmentTargetNotFound is a schedule, live trigger or index that the
// project does not hold.
var ErrAttachmentTargetNotFound = errors.New("the unattended run target does not exist")

// MandateAttachment names one unattended run target. VersionID names a
// pipeline schedule or trigger; ToolkitID and IndexName an index schedule.
type MandateAttachment struct {
	Origin    runmandate.Origin
	ProjectID int64
	VersionID int64
	ToolkitID int64
	IndexName string
	// MandateID is the attached mandate (Attach only).
	MandateID string
	// ActorID is the user who attaches or detaches.
	ActorID int64
}

// UnattendedRunMandates is the PostgreSQL store of the attachments and the
// runmandate.RefusalRecorder.
type UnattendedRunMandates struct {
	pool  *pgxpool.Pool
	audit securityaudit.Appender
}

// NewUnattendedRunMandates builds the store. A nil pool or audit makes every
// call fail rather than write unaudited.
func NewUnattendedRunMandates(pool *pgxpool.Pool, audit securityaudit.Appender) *UnattendedRunMandates {
	return &UnattendedRunMandates{pool: pool, audit: audit}
}

var _ runmandate.RefusalRecorder = (*UnattendedRunMandates)(nil)

func (s *UnattendedRunMandates) ready() error {
	if s == nil || s.pool == nil || s.audit == nil {
		return errors.New("unattended run mandates: no database pool or security audit appender")
	}
	return nil
}

// Target reads what the attachment names: the schedule's cron and input,
// the trigger, or the index. ResourceID is the row's own id (a schedule or
// trigger id), or "<toolkit>:<index>".
func (s *UnattendedRunMandates) Target(ctx context.Context, a MandateAttachment) (runmandate.Target, error) {
	if err := s.ready(); err != nil {
		return runmandate.Target{}, err
	}
	schema, err := tenantschema.Quote(strconv.FormatInt(a.ProjectID, 10))
	if err != nil {
		return runmandate.Target{}, ErrAttachmentTargetNotFound
	}
	target := runmandate.Target{Origin: a.Origin, TenantProjectID: a.ProjectID, ApplicationVersionID: a.VersionID}
	var id int64
	switch a.Origin {
	case runmandate.OriginPipelineSchedule:
		var input string
		err = s.pool.QueryRow(ctx, fmt.Sprintf(
			`SELECT id, cron, user_input FROM %s.pipeline_schedules WHERE version_id = $1`, schema), a.VersionID).
			Scan(&id, &target.Cron, &input)
		target.Input = &input
	case runmandate.OriginPipelineTrigger:
		err = s.pool.QueryRow(ctx, fmt.Sprintf(
			`SELECT id FROM %s.pipeline_triggers WHERE version_id = $1 AND revoked_at IS NULL`, schema), a.VersionID).
			Scan(&id)
	case runmandate.OriginIndexSchedule:
		target.ApplicationVersionID = 0
		err = s.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT id FROM %s.elitea_tools
WHERE id = $1 AND jsonb_typeof(meta::jsonb -> 'indexes_meta' -> $2::text) = 'object'`, schema),
			a.ToolkitID, a.IndexName).Scan(&id)
		target.ResourceID = strconv.FormatInt(a.ToolkitID, 10) + ":" + a.IndexName
	default:
		return runmandate.Target{}, ErrAttachmentTargetNotFound
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return runmandate.Target{}, ErrAttachmentTargetNotFound
	}
	if err != nil {
		return runmandate.Target{}, fmt.Errorf("read the unattended run target: %w", err)
	}
	if target.ResourceID == "" {
		target.ResourceID = strconv.FormatInt(id, 10)
	}
	return target, nil
}

// Attach stores a.MandateID on the target and appends its audit event.
func (s *UnattendedRunMandates) Attach(ctx context.Context, a MandateAttachment, target runmandate.Target) error {
	return s.write(ctx, a, target, a.MandateID)
}

// Detach clears the target's mandate and appends its audit event. A
// governed target with no mandate runs nothing.
func (s *UnattendedRunMandates) Detach(ctx context.Context, a MandateAttachment, target runmandate.Target) error {
	return s.write(ctx, a, target, "")
}

func (s *UnattendedRunMandates) write(ctx context.Context, a MandateAttachment, target runmandate.Target, mandateID string) error {
	if err := s.ready(); err != nil {
		return err
	}
	schema, err := tenantschema.Quote(strconv.FormatInt(a.ProjectID, 10))
	if err != nil {
		return ErrAttachmentTargetNotFound
	}
	var value *string
	if mandateID != "" {
		value = &mandateID
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin mandate attachment: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var affected int64
	switch a.Origin {
	case runmandate.OriginPipelineSchedule, runmandate.OriginPipelineTrigger:
		table := "pipeline_schedules"
		live := ""
		if a.Origin == runmandate.OriginPipelineTrigger {
			table, live = "pipeline_triggers", " AND revoked_at IS NULL"
		}
		tag, execErr := tx.Exec(ctx, fmt.Sprintf(
			`UPDATE %s.%s SET mandate_id = $2 WHERE version_id = $1%s`, schema, table, live), a.VersionID, value)
		err, affected = execErr, tag.RowsAffected()
	case runmandate.OriginIndexSchedule:
		if value == nil {
			tag, execErr := tx.Exec(ctx, fmt.Sprintf(
				`DELETE FROM %s.index_schedule_mandates WHERE toolkit_id = $1 AND index_name = $2`, schema),
				a.ToolkitID, a.IndexName)
			// Detaching an index that has no attachment changes nothing and
			// is not an error: the index exists (Target read it).
			err, affected = execErr, max(tag.RowsAffected(), 1)
		} else {
			tag, execErr := tx.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.index_schedule_mandates (toolkit_id, index_name, mandate_id, attached_by)
VALUES ($1, $2, $3, $4)
ON CONFLICT (toolkit_id, index_name) DO UPDATE SET
    mandate_id = EXCLUDED.mandate_id, attached_by = EXCLUDED.attached_by, attached_at = now()`, schema),
				a.ToolkitID, a.IndexName, mandateID, a.ActorID)
			err, affected = execErr, tag.RowsAffected()
		}
	default:
		return ErrAttachmentTargetNotFound
	}
	if err != nil {
		return fmt.Errorf("write the mandate attachment: %w", err)
	}
	if affected == 0 {
		return ErrAttachmentTargetNotFound
	}
	action := "unattended_run.mandate.attach"
	if mandateID == "" {
		action = "unattended_run.mandate.detach"
	}
	event := securityaudit.SecurityEvent{
		Type:            securityaudit.TypeDecision,
		TenantProjectID: a.ProjectID,
		Action:          action,
		Effect:          securityaudit.EffectAllow,
		Reasons:         []string{"origin." + string(a.Origin)},
		PrincipalRef:    auditUserRef(strconv.FormatInt(a.ActorID, 10)),
		MandateRef:      mandateID,
		ResourceType:    string(a.Origin),
		ResourceID:      target.ResourceID,
	}
	if err := s.audit.Append(ctx, tx, event); err != nil {
		return fmt.Errorf("audit mandate attachment: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit mandate attachment: %w", err)
	}
	return nil
}

// IndexScheduleMandate reads an index schedule's attached mandate; empty
// when there is none.
func (s *UnattendedRunMandates) IndexScheduleMandate(ctx context.Context, projectID, toolkitID int64, indexName string) (string, error) {
	if s == nil || s.pool == nil {
		return "", errors.New("unattended run mandates: no database pool")
	}
	schema, err := tenantschema.Quote(strconv.FormatInt(projectID, 10))
	if err != nil {
		return "", err
	}
	var mandateID string
	err = s.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT mandate_id FROM %s.index_schedule_mandates WHERE toolkit_id = $1 AND index_name = $2`, schema),
		toolkitID, indexName).Scan(&mandateID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the index schedule mandate: %w", err)
	}
	return mandateID, nil
}

// RecordRunRefusal appends the deny decision of one refused unattended run.
func (s *UnattendedRunMandates) RecordRunRefusal(ctx context.Context, refusal runmandate.Refusal) error {
	if err := s.ready(); err != nil {
		return err
	}
	event := securityaudit.SecurityEvent{
		Type:            securityaudit.TypeDecision,
		Time:            refusal.At,
		TenantProjectID: refusal.Target.TenantProjectID,
		Action:          "unattended_run.admit",
		Effect:          securityaudit.EffectDeny,
		Reasons:         []string{refusalReason(refusal.Code), "origin." + string(refusal.Target.Origin)},
		MandateRef:      refusal.MandateID,
		ResourceType:    string(refusal.Target.Origin),
		ResourceID:      refusal.Target.ResourceID,
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return s.audit.Append(ctx, tx, event)
	})
}

// refusalReason is a result code as a reason code: lower case
// (MANDATE_REQUIRED is mandate_required).
func refusalReason(code string) string { return strings.ToLower(code) }
