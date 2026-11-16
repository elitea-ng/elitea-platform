package marketplace

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Install is the record of one hire.
type Install struct {
	ProjectID   int64
	EntityKind  string
	EntityID    int64
	ListingID   int64
	VersionID   int64
	Digest      [32]byte
	Mode        string
	Bindings    json.RawMessage
	ToolsOn     bool
	InstalledBy int64
}

// RecordInstall writes the hire. A skill attached to a second agent is added
// once, so a second record of the same project entity changes nothing.
func RecordInstall(ctx context.Context, db DB, in Install) error {
	bindings := string(in.Bindings)
	if bindings == "" {
		bindings = "[]"
	}
	if _, err := db.Exec(ctx, `
INSERT INTO elitea_marketplace.installs
    (project_id, entity_kind, entity_id, listing_id, version_id, digest, mode, bindings, tools_enabled, installed_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10)
ON CONFLICT (project_id, entity_kind, entity_id) DO NOTHING`,
		in.ProjectID, in.EntityKind, in.EntityID, in.ListingID, in.VersionID, in.Digest[:], in.Mode, bindings, in.ToolsOn, in.InstalledBy); err != nil {
		return fmt.Errorf("marketplace: record the hire: %w", err)
	}
	return nil
}

// StoredVersion is a listing version with what a hire needs.
type StoredVersion struct {
	ListingID     int64
	ListingKind   string
	ListingName   string
	ListingState  string
	VersionID     int64
	Name          string
	State         string
	Withdrawn     bool
	Digest        [32]byte
	Definition    json.RawMessage
	Requirements  Requirements
	SourceProject int64
	EntityID      int64
}

// Hireable reports whether a new hire may use the version: the listing is
// listed and the version is neither yanked nor withdrawn.
func (v StoredVersion) Hireable() bool {
	return v.ListingState == ListingListed && !v.Withdrawn && v.State != VersionYanked
}

// LoadVersion reads one listing version with its snapshot.
func LoadVersion(ctx context.Context, db DB, listingID, versionID int64) (StoredVersion, error) {
	var (
		v       StoredVersion
		digest  []byte
		reqsRaw []byte
		def     []byte
		entity  *int64
	)
	err := db.QueryRow(ctx, `
SELECT l.id, l.kind, l.name, l.state, l.source_project_id, l.entity_id, v.version_id, v.name, v.state,
       v.withdrawn_at IS NOT NULL, v.digest, v.definition, v.requirements
FROM elitea_marketplace.listing_versions v JOIN elitea_marketplace.listings l ON l.id = v.listing_id
WHERE v.listing_id = $1 AND v.version_id = $2`, listingID, versionID).
		Scan(&v.ListingID, &v.ListingKind, &v.ListingName, &v.ListingState, &v.SourceProject, &entity, &v.VersionID, &v.Name, &v.State,
			&v.Withdrawn, &digest, &def, &reqsRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredVersion{}, ErrNotFound
	}
	if err != nil {
		return StoredVersion{}, fmt.Errorf("marketplace: read the version: %w", err)
	}
	copy(v.Digest[:], digest)
	v.Definition = def
	if entity != nil {
		v.EntityID = *entity
	}
	if err := json.Unmarshal(reqsRaw, &v.Requirements); err != nil {
		return StoredVersion{}, fmt.Errorf("marketplace: read the requirements: %w", err)
	}
	return v, nil
}

// Origin is where an installed or forked agent came from.
type Origin struct {
	Mode         string      `json:"mode"`
	ListingID    string      `json:"listing_id"`
	ListingName  string      `json:"listing_name"`
	ListingState string      `json:"listing_state"`
	VersionID    string      `json:"version_id"`
	VersionName  string      `json:"version_name"`
	VersionState string      `json:"version_state"`
	Digest       string      `json:"digest"`
	LatestVer    *VersionRef `json:"latest_version"`
}

// GetOrigin reads the origin of an agent from its version meta and the
// listing. schema is the quoted tenant schema. It answers ErrNotFound for an
// agent that was not hired from the Marketplace.
func GetOrigin(ctx context.Context, db DB, schema string, applicationID int64) (Origin, error) {
	var raw []byte
	err := db.QueryRow(ctx, fmt.Sprintf(`
SELECT meta::text FROM %s.application_versions
WHERE application_id = $1 AND meta ? 'origin_listing_id'
ORDER BY id LIMIT 1`, schema), applicationID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return Origin{}, ErrNotFound
	}
	if err != nil {
		return Origin{}, fmt.Errorf("marketplace: read the agent meta: %w", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		return Origin{}, ErrNotFound
	}
	listingID, versionID := metaInt(meta, MetaOriginListingID), metaInt(meta, MetaOriginVersionID)
	kind, _ := meta[MetaOriginKind].(string)
	if listingID <= 0 || versionID <= 0 || (kind != OriginInstalled && kind != OriginForked) {
		return Origin{}, ErrNotFound
	}
	var (
		origin     Origin
		versionRaw int64
		digest     []byte
		withdrawn  bool
	)
	err = db.QueryRow(ctx, `
SELECT l.id, l.name, l.state, v.version_id, v.name, v.state, v.withdrawn_at IS NOT NULL, v.digest
FROM elitea_marketplace.listing_versions v JOIN elitea_marketplace.listings l ON l.id = v.listing_id
WHERE v.listing_id = $1 AND v.version_id = $2`, listingID, versionID).
		Scan(&listingID, &origin.ListingName, &origin.ListingState, &versionRaw, &origin.VersionName, &origin.VersionState, &withdrawn, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return Origin{}, ErrNotFound
	}
	if err != nil {
		return Origin{}, fmt.Errorf("marketplace: read the origin: %w", err)
	}
	origin.Mode = kind
	origin.ListingID = strconv.FormatInt(listingID, 10)
	origin.VersionID = strconv.FormatInt(versionRaw, 10)
	origin.Digest = hex.EncodeToString(digest)
	if withdrawn && origin.VersionState == VersionAvailable {
		// A withdrawn version reads as yanked to the agent that holds it: it
		// cannot be hired again, and the agent keeps running on its snapshot.
		origin.VersionState = VersionYanked
	}
	var latest *string
	if err := db.QueryRow(ctx, `
SELECT (SELECT cv.version_id || '|' || cv.name || '|' || to_char(cv.published_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
        FROM elitea_marketplace.listing_versions cv
        WHERE cv.listing_id = $1 AND cv.withdrawn_at IS NULL AND cv.state <> 'yanked'
          AND (cv.published_at, cv.version_id) > (SELECT pv.published_at, pv.version_id FROM elitea_marketplace.listing_versions pv
                                                  WHERE pv.listing_id = $1 AND pv.version_id = $2)
        ORDER BY cv.published_at DESC, cv.version_id DESC LIMIT 1)`, listingID, versionID).Scan(&latest); err != nil {
		return Origin{}, fmt.Errorf("marketplace: read the latest version: %w", err)
	}
	origin.LatestVer = parseVersionRef(latest)
	return origin, nil
}

func metaInt(meta map[string]any, key string) int64 {
	switch value := meta[key].(type) {
	case string:
		parsed, _ := strconv.ParseInt(value, 10, 64)
		return parsed
	case float64:
		return int64(value)
	}
	return 0
}

// CatalogueMatch is the listing version of a catalogue version id.
type CatalogueMatch struct {
	ListingID   int64
	ListingName string
	VersionName string
	Digest      [32]byte
}

// LookupByCatalogueVersion finds the listing version of a catalogue version
// id, for a fork of a published agent that names its source by that id.
func LookupByCatalogueVersion(ctx context.Context, db DB, kind string, catalogueVersionID int64) (CatalogueMatch, bool, error) {
	var match CatalogueMatch
	var raw []byte
	err := db.QueryRow(ctx, `
SELECT v.listing_id, l.name, v.name, v.digest FROM elitea_marketplace.listing_versions v
JOIN elitea_marketplace.listings l ON l.id = v.listing_id
WHERE l.kind = $1 AND v.version_id = $2`, kind, catalogueVersionID).Scan(&match.ListingID, &match.ListingName, &match.VersionName, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return CatalogueMatch{}, false, nil
	}
	if err != nil {
		return CatalogueMatch{}, false, fmt.Errorf("marketplace: look up the catalogue version: %w", err)
	}
	copy(match.Digest[:], raw)
	return match, true, nil
}

// IsPersonalProject reports whether a project is a person's own: one member,
// so no second approver. The platform names a personal project
// project_user_<id>. A database without centry.project answers false.
func IsPersonalProject(ctx context.Context, db DB, projectID int64) (bool, error) {
	var personal bool
	err := db.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM centry.project WHERE id = $1 AND name LIKE 'project\_user\_%')`, projectID).Scan(&personal)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "42P01" || pgErr.Code == "3F000") {
			return false, nil
		}
		return false, fmt.Errorf("marketplace: read the project: %w", err)
	}
	return personal, nil
}
