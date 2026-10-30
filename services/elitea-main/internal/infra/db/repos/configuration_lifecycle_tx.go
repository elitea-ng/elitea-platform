package repos

// A configuration lifecycle event appended inside a caller's transaction,
// for the paths that write a project configuration row directly instead of
// through CurrentConfigurationMutationRepository: the connector approval and
// the permission trimming both rewrite the project's worker_egress row. Each
// such write must leave the same configuration_lifecycle_outbox event the
// Configuration route's AppendLifecycleIntent leaves, or the lifecycle
// consumers (cache invalidation, the configuration history) miss it.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

// readConfigurationLifecycleSnapshot reads one row of schema.configuration
// as a lifecycle snapshot. sanitize answers the snapshot's data from the
// row's data; it must drop every field that may hold secret material.
func readConfigurationLifecycleSnapshot(
	ctx context.Context,
	tx pgx.Tx,
	schema string,
	id int64,
	sanitize func(map[string]any) map[string]any,
) (*configurationapp.CurrentConfigurationLifecycleSnapshot, error) {
	var snapshot configurationapp.CurrentConfigurationLifecycleSnapshot
	var raw []byte
	err := tx.QueryRow(ctx, fmt.Sprintf(`
SELECT id::integer, uuid::text, project_id::integer, COALESCE(elitea_title, ''), COALESCE(type, ''),
       COALESCE(section, ''), label,
       COALESCE(shared, false), COALESCE(status_ok, false), COALESCE(source, ''), author_id::integer, data
FROM %s.configuration WHERE id = $1`, schema), id).Scan(
		&snapshot.ID, &snapshot.UUID, &snapshot.ProjectID, &snapshot.EliteaTitle, &snapshot.Type,
		&snapshot.Section, &snapshot.Label, &snapshot.Shared, &snapshot.StatusOK, &snapshot.Source,
		&snapshot.AuthorID, &raw)
	if err != nil {
		return nil, fmt.Errorf("configuration lifecycle: read the row: %w", err)
	}
	data := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, fmt.Errorf("configuration lifecycle: decode the row data: %w", err)
		}
	}
	snapshot.Data = sanitize(data)
	if snapshot.Data == nil {
		snapshot.Data = map[string]any{}
	}
	return &snapshot, nil
}

// workerEgressLifecycleData is the sanitized data of a worker_egress row:
// its schema has one field, `allowlist`, a list of public host entries with
// no secret material. Anything else in the row is left out.
func workerEgressLifecycleData(data map[string]any) map[string]any {
	out := map[string]any{}
	if list, ok := data["allowlist"]; ok {
		out["allowlist"] = list
	}
	return out
}

// appendConfigurationLifecycleEventTx appends the lifecycle event of a row
// written in tx: created (before nil), updated (both) or deleted (after nil).
// The revision is the configuration's latest plus one, the payload is the
// one the Configuration route encodes, and the digest authenticates it; the
// outbox's unique (project, uuid, revision) key fences a concurrent writer.
func appendConfigurationLifecycleEventTx(
	ctx context.Context,
	tx pgx.Tx,
	actorID int64,
	before, after *configurationapp.CurrentConfigurationLifecycleSnapshot,
) error {
	if actorID <= 0 || actorID > math.MaxInt32 {
		return errors.New("configuration lifecycle: the event needs the acting user")
	}
	if err := forgetRemovedProjectEgressSightings(ctx, tx, before, after); err != nil {
		return err
	}
	var operation configurationapp.CurrentConfigurationLifecycleOperation
	var identity *configurationapp.CurrentConfigurationLifecycleSnapshot
	switch {
	case before == nil && after != nil:
		operation, identity = configurationapp.CurrentConfigurationCreated, after
	case before != nil && after != nil:
		if before.UUID != after.UUID || before.ProjectID != after.ProjectID {
			return errors.New("configuration lifecycle: the snapshots name different rows")
		}
		operation, identity = configurationapp.CurrentConfigurationUpdated, after
	case before != nil:
		operation, identity = configurationapp.CurrentConfigurationDeleted, before
	default:
		return errors.New("configuration lifecycle: no snapshot")
	}
	if identity.ProjectID <= 0 || !validCurrentPersistenceUUID(identity.UUID, false) {
		return errors.New("configuration lifecycle: the row has no valid identity")
	}
	eventID, err := uuid.NewRandom()
	if err != nil {
		return err
	}
	snapshot, err := configurationapp.EncodeCurrentConfigurationLifecycleIntent(configurationapp.CurrentConfigurationLifecycleIntent{
		ID: eventID.String(), Operation: operation, ActorID: int32(actorID), Before: before, After: after,
	})
	if err != nil || len(snapshot) == 0 || len(snapshot) > maxCurrentLifecycleSnapshotBytes {
		return errors.New("configuration lifecycle: the snapshot does not encode")
	}
	digest := sha256.Sum256(snapshot)
	queries := sqlcgen.New(tx)
	revision, err := queries.GetLatestConfigurationLifecycleRevision(ctx, sqlcgen.GetLatestConfigurationLifecycleRevisionParams{
		ProjectID: identity.ProjectID, ConfigurationUuid: identity.UUID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		revision = 0
	} else if err != nil {
		return fmt.Errorf("configuration lifecycle: read the latest revision: %w", err)
	}
	if revision < 0 || revision == math.MaxInt64 {
		return errors.New("configuration lifecycle: the revision is exhausted")
	}
	if err := queries.InsertConfigurationLifecycleEvent(ctx, sqlcgen.InsertConfigurationLifecycleEventParams{
		EventID: eventID.String(), ProjectID: identity.ProjectID, ConfigurationUuid: identity.UUID,
		Revision: revision + 1, Operation: string(operation), ActorID: int32(actorID),
		SanitizedSnapshot: snapshot, SnapshotDigest: digest[:],
	}); err != nil {
		return fmt.Errorf("configuration lifecycle: append the event: %w", err)
	}
	return nil
}
