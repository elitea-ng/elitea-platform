package marketplace

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// MoveFork records that a fork now stands on a listing version: the origin keys
// of every version of the agent move, and the install record is written or
// moved, so the next update diffs from there. A fork made before forks recorded
// their origin gets the record here, as its first one. schema is the quoted
// tenant schema.
func MoveFork(ctx context.Context, db DB, schema string, projectID, applicationID int64, version StoredVersion, actor int64) error {
	origin, err := json.Marshal(map[string]any{
		MetaOriginKind:        OriginForked,
		MetaOriginListingID:   strconv.FormatInt(version.ListingID, 10),
		MetaOriginVersionID:   strconv.FormatInt(version.VersionID, 10),
		MetaOriginDigest:      hex.EncodeToString(version.Digest[:]),
		MetaOriginVersionName: version.Name,
		MetaOriginListingName: version.ListingName,
	})
	if err != nil {
		return fmt.Errorf("marketplace: encode the origin: %w", err)
	}
	if _, err := db.Exec(ctx, fmt.Sprintf(`
UPDATE %s.application_versions SET meta = COALESCE(meta, '{}'::jsonb) || $2::jsonb WHERE application_id = $1`, schema),
		applicationID, string(origin)); err != nil {
		return fmt.Errorf("marketplace: move the origin of the fork: %w", err)
	}
	if _, err := db.Exec(ctx, `
INSERT INTO elitea_marketplace.installs
    (project_id, entity_kind, entity_id, listing_id, version_id, digest, mode, bindings, tools_enabled, installed_by)
VALUES ($1, 'agent', $2, $3, $4, $5, 'fork', '[]'::jsonb, true, $6)
ON CONFLICT (project_id, entity_kind, entity_id) DO UPDATE SET
    listing_id = EXCLUDED.listing_id, version_id = EXCLUDED.version_id, digest = EXCLUDED.digest,
    updated_at = clock_timestamp()`,
		projectID, applicationID, version.ListingID, version.VersionID, version.Digest[:], actor); err != nil {
		return fmt.Errorf("marketplace: move the install of the fork: %w", err)
	}
	return nil
}

// MoveInstall pins an installed agent to a listing version with the bindings
// of that version, and turns its tools on.
func MoveInstall(ctx context.Context, db DB, projectID, applicationID int64, version StoredVersion, bindings []Binding) error {
	raw, err := json.Marshal(bindings)
	if err != nil {
		return fmt.Errorf("marketplace: encode the bindings: %w", err)
	}
	if _, err := db.Exec(ctx, `
UPDATE elitea_marketplace.installs SET version_id = $3, digest = $4, bindings = $5::jsonb, tools_enabled = true,
    updated_at = clock_timestamp()
WHERE project_id = $1 AND entity_kind = 'agent' AND entity_id = $2 AND mode = 'install'`,
		projectID, applicationID, version.VersionID, version.Digest[:], string(raw)); err != nil {
		return fmt.Errorf("marketplace: pin the install: %w", err)
	}
	return nil
}

// LoadVersionInfo reads one listing version as the upgrade plan shows it.
func LoadVersionInfo(ctx context.Context, db DB, listingID, versionID int64) (Version, error) {
	var (
		v       Version
		digest  []byte
		reqsRaw []byte
	)
	err := db.QueryRow(ctx, `
SELECT version_id, name, state, state_reason, release_notes, digest, published_at, withdrawn_at IS NOT NULL, requirements
FROM elitea_marketplace.listing_versions WHERE listing_id = $1 AND version_id = $2`, listingID, versionID).
		Scan(&v.ID, &v.Name, &v.State, &v.StateReason, &v.ReleaseNotes, &digest, &v.PublishedAt, &v.Withdrawn, &reqsRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, ErrNotFound
	}
	if err != nil {
		return Version{}, fmt.Errorf("marketplace: read the version: %w", err)
	}
	v.Digest = hex.EncodeToString(digest)
	if err := json.Unmarshal(reqsRaw, &v.Requirements); err != nil {
		return Version{}, fmt.Errorf("marketplace: read the requirements: %w", err)
	}
	return v, nil
}

// RatingOf is the listing's rating, as the plan's feedback summary shows it.
func RatingOf(ctx context.Context, db DB, listingID int64) (avg *float64, count int, err error) {
	err = db.QueryRow(ctx, `SELECT rating_avg::float8, rating_count FROM elitea_marketplace.listings WHERE id = $1`, listingID).
		Scan(&avg, &count)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, fmt.Errorf("marketplace: read the rating: %w", err)
	}
	return avg, count, nil
}

// Newer reports whether version a was published after version b.
func Newer(a, b Version) bool {
	if a.PublishedAt.Equal(b.PublishedAt) {
		return a.ID > b.ID
	}
	return a.PublishedAt.After(b.PublishedAt)
}

// StateOf reads the stored state of a version and whether it was withdrawn.
func StateOf(ctx context.Context, db DB, listingID, versionID int64) (state, reason string, err error) {
	err = db.QueryRow(ctx, `
SELECT state, state_reason FROM elitea_marketplace.listing_versions WHERE listing_id = $1 AND version_id = $2`,
		listingID, versionID).Scan(&state, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return state, reason, err
}
