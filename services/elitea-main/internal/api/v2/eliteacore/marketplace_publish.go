package eliteacore

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/marketplace"
)

// The Marketplace side of publish and unpublish (ADR-0051 decision 2).
//
// A publish records, in its own transaction, the listing of the agent and a
// listing version: the snapshot of the published clone with its requirements,
// the release notes and the digest. An unpublish does not delete the snapshot.
// It withdraws the versions, which delists a listing with none left, while the
// chat twin keeps being deleted by removeCatalogTwins as before.

// recordListing writes the listing and the listing version of one publish.
func (h *Handler) recordListing(ctx context.Context, tx pgx.Tx, p publishWrite, cloneID int, twin catalogTwinTargets) error {
	projectNumeric, err := strconv.ParseInt(strings.TrimSpace(p.projectID), 10, 64)
	if err != nil {
		return fmt.Errorf("marketplace listing: project id %q is not numeric: %w", p.projectID, err)
	}
	snap, err := marketplace.BuildAgentSnapshot(ctx, tx, marketplace.AgentSource{
		Schema: p.schema, ProjectID: projectNumeric, ApplicationID: int64(p.appID), VersionID: int64(cloneID),
	})
	if err != nil {
		return err
	}
	// The catalogue row the listing points at: the twin, or the clone itself
	// when the author already stands in the public project.
	entityID, catalogueVersionID := int64(twin.ApplicationID), int64(twin.VersionID)
	if twin.VersionID == 0 {
		entityID, catalogueVersionID = int64(p.appID), int64(cloneID)
	}
	var publisher int64
	if p.hasPublisher {
		publisher = p.publishedBy
	}
	_, err = marketplace.RecordPublish(ctx, tx, marketplace.PublishRecord{
		Snapshot: snap, SourceProjectID: projectNumeric, SourceEntityID: int64(p.appID),
		EntityID: entityID, VersionID: catalogueVersionID, VersionName: p.versionName,
		ReleaseNotes: p.releaseNotes, PublisherID: publisher,
	})
	return err
}

// withdrawListingVersions withdraws the listing versions of source versions an
// unpublish reverts. It runs before removeCatalogTwins, which deletes the twin
// rows whose ids it reads.
func withdrawListingVersions(ctx context.Context, tx pgx.Tx, sourceProjectID string, sourceVersionIDs []int) error {
	if len(sourceVersionIDs) == 0 {
		return nil
	}
	projectNumeric, err := strconv.ParseInt(strings.TrimSpace(sourceProjectID), 10, 64)
	if err != nil {
		return fmt.Errorf("marketplace withdraw: project id %q is not numeric: %w", sourceProjectID, err)
	}
	catalogueIDs := make([]int64, 0, len(sourceVersionIDs))
	if isPublicProject(sourceProjectID) {
		// The clone is the catalogue row.
		for _, id := range sourceVersionIDs {
			catalogueIDs = append(catalogueIDs, int64(id))
		}
	} else {
		rows, err := tx.Query(ctx, fmt.Sprintf(
			`SELECT id FROM %s.application_versions WHERE shared_owner_id = $1 AND shared_id = ANY($2::int[])`,
			publicTenantSchema()), projectNumeric, sourceVersionIDs)
		if err != nil {
			return fmt.Errorf("marketplace withdraw: read the twin versions: %w", err)
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			catalogueIDs = append(catalogueIDs, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return marketplace.Withdraw(ctx, tx, marketplace.KindAgent, projectNumeric, catalogueIDs)
}
