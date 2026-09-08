package providerhub

// Reading a manifest back.
//
// Migration 0107 stores every published descriptor byte for byte in
// `provider_hub.provider_published_manifest`, but until now nothing in this
// service ever selected those bytes again. The registrar writes them, the
// admission gate reads only the STATUS beside them, and the administration
// listing reports the digest. The manifest itself was write-only.
//
// The toolkit catalogue needs the document, because a provider's descriptor is
// where its toolkits are declared. pylon reads the same document from process
// memory (`elitea_core.present_providers`, filled by an Arbiter registration)
// and projects `provided_toolkits` into toolkit type schemas. This service has
// no such memory, so the table is the source.

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AdmittedManifest is one provider's published descriptor with the admission
// decision that governs it.
//
// Status carries the same three values `provider_admitted_revision.status`
// does — 'inactive', 'active' or 'revoked' — so a caller applies the SAME rule
// the request-path gate applies rather than inventing a second one.
type AdmittedManifest struct {
	ProviderID string
	RevisionID string
	Digest     string
	Status     string
	Reason     string
	// Origin is the reviewed service location: scheme, host and port, no path.
	// It is carried so a caller can report where a projected toolkit comes from
	// without a second query. It is NEVER dialled by this package.
	Origin   string
	Manifest []byte
}

// AdmittedManifests returns the governing revision of every provider
// registered under one project, with its manifest bytes.
//
// "Governing" is `LatestAdmission`'s own preference order, applied per
// provider with DISTINCT ON: an active revision outranks a later inactive one,
// and among equals `revoked` sorts first so an ambiguous pair resolves to the
// closed answer. The two must agree, because a catalogue that projected a
// revision the gate would refuse would offer a toolkit that cannot run.
//
// A provider with a registration but no admitted revision yet does not appear.
// That is a real state — the facade registers on its first boot — and it is
// distinct from a read failure, which is returned as an error.
func AdmittedManifests(ctx context.Context, pool *pgxpool.Pool, projectID int64) ([]AdmittedManifest, error) {
	if pool == nil {
		return nil, fmt.Errorf("providerhub: no database pool configured")
	}
	rows, err := pool.Query(ctx, `
SELECT DISTINCT ON (r.provider_id)
       r.provider_id, r.revision_id, r.manifest_digest, r.status, r.reason,
       COALESCE(o.origin, ''), m.manifest_bytes
  FROM provider_hub.provider_admitted_revision AS r
  JOIN provider_hub.provider_published_manifest AS m
    ON m.digest = r.manifest_digest
  LEFT JOIN provider_hub.provider_origin_registration AS o
    ON o.project_id = r.project_id AND o.provider_id = r.provider_id
 WHERE r.project_id = $1
 ORDER BY r.provider_id,
          (r.status = 'active') DESC,
          r.admitted_at DESC,
          (r.status = 'revoked') DESC,
          r.revision_id DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("admitted manifests: %w", err)
	}
	defer rows.Close()

	manifests := make([]AdmittedManifest, 0)
	for rows.Next() {
		var manifest AdmittedManifest
		if err := rows.Scan(
			&manifest.ProviderID, &manifest.RevisionID, &manifest.Digest,
			&manifest.Status, &manifest.Reason, &manifest.Origin, &manifest.Manifest,
		); err != nil {
			return nil, fmt.Errorf("admitted manifests: %w", err)
		}
		manifests = append(manifests, manifest)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("admitted manifests: %w", err)
	}
	return manifests, nil
}
