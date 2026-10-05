package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	scope "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/executionchildscope"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
)

// Internal Main producer port, called after freeze/hash and before credentials.
// There is deliberately no HTTP route accepting definition bytes from Worker.
func (repo *ExecutionChildScopesRepository) CaptureFrozenSavedChildVersion(ctx context.Context, claim storage.ContentClaim, application, version uint64, expected string, raw, sourceDefinition json.RawMessage) error {
	if repo == nil || application == 0 || application > 2147483647 || version == 0 || version > 2147483647 || !scope.ValidDigest(expected) || len(raw) == 0 || len(raw) > scope.MaxDefinitionBytes || !json.Valid(raw) || len(sourceDefinition) == 0 || len(sourceDefinition) > scope.MaxDefinitionBytes || !json.Valid(sourceDefinition) {
		return scope.ErrDenied
	}
	return repo.store.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx sqlExecutor) error {
		original, err := readSavedChildClaim(ctx, tx, claim)
		if err != nil || scope.FrozenDefinitionDigest(original.ProjectID, application, version, raw) != expected {
			return scope.ErrDenied
		}
		var running bool
		if tx.QueryRow(ctx, `SELECT state='RUNNING' FROM elitea_runtime.execution_jobs WHERE execution_id=$1 AND generation=$2 FOR UPDATE`, claim.ExecutionID, claim.Generation).Scan(&running) != nil || !running {
			return scope.ErrDenied
		}
		var existing, sourceWire, existingSource []byte
		err = tx.QueryRow(ctx, `SELECT d.definition_bytes,r.canonical_wire,s.definition_bytes FROM elitea_runtime.execution_saved_child_captures c JOIN elitea_runtime.execution_captured_definitions d USING(resource_project_id,application_id,version_id,definition_sha256) JOIN elitea_runtime.execution_definition_source_refs r ON r.source_id=c.source_id AND r.digest_sha256=c.source_digest_sha256 JOIN elitea_runtime.execution_captured_definitions s ON s.resource_project_id=r.resource_project_id AND s.application_id=r.application_id AND s.version_id=r.version_id AND s.definition_sha256=r.definition_sha256 WHERE c.execution_id=$1 AND c.generation=$2 AND c.application_id=$3 AND c.version_id=$4 AND c.definition_sha256=$5 AND c.root_input_sha256=$6 AND c.resource_project_id=$7 AND c.actor_id=$8 FOR SHARE OF c,d,r,s`, claim.ExecutionID, claim.Generation, application, version, expected, scope.Digest(original.Payload), original.ProjectID, original.ActorID).Scan(&existing, &sourceWire, &existingSource)
		if err == nil {
			if !bytes.Equal(existing, raw) || !bytes.Equal(existingSource, sourceDefinition) {
				return scope.ErrDenied
			}
			var wire scope.SourceWire
			if json.Unmarshal(sourceWire, &wire) != nil {
				return scope.ErrDenied
			}
			_, err = scope.DecodeSourceWire(sourceWire, existingSource, wire.Reference(), original.ProjectID, original.ActorID)
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return scope.ErrUnavailable
		}
		var count, used int64
		if tx.QueryRow(ctx, `SELECT count(*),COALESCE(sum(d.byte_length+s.byte_length),0) FROM elitea_runtime.execution_saved_child_captures c JOIN elitea_runtime.execution_captured_definitions d USING(resource_project_id,application_id,version_id,definition_sha256) JOIN elitea_runtime.execution_definition_source_refs r ON r.source_id=c.source_id AND r.digest_sha256=c.source_digest_sha256 JOIN elitea_runtime.execution_captured_definitions s ON s.resource_project_id=r.resource_project_id AND s.application_id=r.application_id AND s.version_id=r.version_id AND s.definition_sha256=r.definition_sha256 WHERE c.execution_id=$1 AND c.generation=$2`, claim.ExecutionID, claim.Generation).Scan(&count, &used) != nil || count >= 128 || used+int64(len(raw)+len(sourceDefinition)) > scope.MaxCatalogBytes {
			return scope.ErrDenied
		}
		sourceRef, err := captureMainSourceReference(ctx, tx, original.ProjectID, original.ActorID, application, version, sourceDefinition)
		if err != nil {
			return err
		}
		if err = captureMainDefinition(ctx, tx, original.ProjectID, application, version, expected, raw); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO elitea_runtime.execution_saved_child_captures(execution_id,generation,application_id,version_id,definition_sha256,root_input_sha256,resource_project_id,actor_id,source_id,source_digest_sha256) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(execution_id,generation,application_id,version_id,definition_sha256) DO NOTHING`, claim.ExecutionID, claim.Generation, application, version, expected, scope.Digest(original.Payload), original.ProjectID, original.ActorID, sourceRef.SourceID, sourceRef.DigestSHA256)
		if err != nil || tag.RowsAffected() != 1 {
			return scope.ErrUnavailable
		}
		return nil
	})
}
func (repo *ExecutionChildScopesRepository) ReadFrozenSavedChildVersion(ctx context.Context, claim storage.ContentClaim, application, version uint64, expected string) (json.RawMessage, error) {
	if repo == nil || application == 0 || version == 0 || !scope.ValidDigest(expected) {
		return nil, scope.ErrDenied
	}
	var raw []byte
	err := repo.store.WithinTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadWrite}, func(tx sqlExecutor) error {
		original, err := readSavedChildClaim(ctx, tx, claim)
		if err != nil {
			return err
		}
		if tx.QueryRow(ctx, `SELECT d.definition_bytes FROM elitea_runtime.execution_saved_child_captures c JOIN elitea_runtime.execution_captured_definitions d USING(resource_project_id,application_id,version_id,definition_sha256) WHERE c.execution_id=$1 AND c.generation=$2 AND c.application_id=$3 AND c.version_id=$4 AND c.definition_sha256=$5 AND c.root_input_sha256=$6 AND c.resource_project_id=$7 AND c.actor_id=$8 FOR SHARE OF c,d`, claim.ExecutionID, claim.Generation, application, version, expected, scope.Digest(original.Payload), original.ProjectID, original.ActorID).Scan(&raw) != nil || len(raw) == 0 || len(raw) > scope.MaxDefinitionBytes || scope.FrozenDefinitionDigest(original.ProjectID, application, version, raw) != expected {
			return scope.ErrDenied
		}
		return nil
	})
	return bytes.Clone(raw), err
}

// One immutable bytes/frame owner serves root and descendant relations. Only
// Main producer methods can write it; no Worker definition upload exists.
func captureMainDefinition(ctx context.Context, tx sqlExecutor, project int64, application, version uint64, full string, raw []byte) error {
	if tx == nil || project <= 0 || len(raw) == 0 || len(raw) > scope.MaxDefinitionBytes || !json.Valid(raw) || scope.FrozenDefinitionDigest(project, application, version, raw) != full {
		return scope.ErrDenied
	}
	if _, err := tx.Exec(ctx, `INSERT INTO elitea_runtime.execution_captured_definitions(resource_project_id,application_id,version_id,definition_sha256,definition_bytes,byte_length) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(resource_project_id,application_id,version_id,definition_sha256) DO NOTHING`, project, application, version, full, raw, len(raw)); err != nil {
		return scope.ErrUnavailable
	}
	var existing []byte
	if tx.QueryRow(ctx, `SELECT definition_bytes FROM elitea_runtime.execution_captured_definitions WHERE resource_project_id=$1 AND application_id=$2 AND version_id=$3 AND definition_sha256=$4 FOR SHARE`, project, application, version, full).Scan(&existing) != nil || !bytes.Equal(existing, raw) {
		return scope.ErrDenied
	}
	return nil
}

// The saved-runtime fingerprint and original source fingerprint can differ.
// This original execution relation, minted by Main before redemption, links
// both exact common-store captures without rehashing a redeemed runtime.
func readCapturedSavedChildSource(ctx context.Context, tx sqlExecutor, original storage.HTTPActionInput, claim storage.ContentClaim, application, version uint64, frozen string) (scope.SourceDefinition, error) {
	var raw, definition []byte
	if tx.QueryRow(ctx, `SELECT r.canonical_wire,d.definition_bytes FROM elitea_runtime.execution_saved_child_captures c JOIN elitea_runtime.execution_definition_source_refs r ON r.source_id=c.source_id AND r.digest_sha256=c.source_digest_sha256 JOIN elitea_runtime.execution_captured_definitions d ON d.resource_project_id=r.resource_project_id AND d.application_id=r.application_id AND d.version_id=r.version_id AND d.definition_sha256=r.definition_sha256 WHERE c.execution_id=$1 AND c.generation=$2 AND c.application_id=$3 AND c.version_id=$4 AND c.definition_sha256=$5 AND c.root_input_sha256=$6 AND c.resource_project_id=$7 AND c.actor_id=$8 AND r.resource_project_id=c.resource_project_id AND r.actor_id=c.actor_id AND r.application_id=c.application_id AND r.version_id=c.version_id FOR SHARE OF c,r,d`, claim.ExecutionID, claim.Generation, application, version, frozen, scope.Digest(original.Payload), original.ProjectID, original.ActorID).Scan(&raw, &definition) != nil {
		return scope.SourceDefinition{}, scope.ErrDenied
	}
	var wire scope.SourceWire
	if json.Unmarshal(raw, &wire) != nil || wire.ApplicationID != application || wire.VersionID != version {
		return scope.SourceDefinition{}, scope.ErrDenied
	}
	return scope.DecodeSourceWire(raw, definition, wire.Reference(), original.ProjectID, original.ActorID)
}
