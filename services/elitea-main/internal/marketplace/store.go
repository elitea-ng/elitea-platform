package marketplace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// PublishRecord is what a publish writes into the Marketplace, in the publish
// transaction.
type PublishRecord struct {
	Snapshot        Snapshot
	SourceProjectID int64
	// SourceEntityID is the agent or skill in the source project.
	SourceEntityID int64
	// EntityID is the public catalogue agent or skill, and VersionID the
	// catalogue version this publish made.
	EntityID     int64
	VersionID    int64
	VersionName  string
	ReleaseNotes string
	PublisherID  int64
	// Category overrides the snapshot's: a skill's category is a tag on the
	// catalogue version.
	Category string
	// PublishedAt is set by the backfill, which replays old versions in their
	// order; a live publish leaves it zero and the database stamps the time.
	PublishedAt time.Time
}

const maxReleaseNotes = 4000

// ClampReleaseNotes trims release notes and cuts them to the stored limit.
func ClampReleaseNotes(notes string) string {
	notes = strings.TrimSpace(notes)
	if r := []rune(notes); len(r) > maxReleaseNotes {
		return string(r[:maxReleaseNotes])
	}
	return notes
}

func searchText(parts ...string) string {
	text := strings.ToLower(strings.Join(parts, " "))
	if r := []rune(text); len(r) > 8000 {
		text = string(r[:8000])
	}
	return text
}

// RecordPublish creates the listing on the first publish of a source entity and
// adds the version to it. A republish after an unpublish finds the same listing
// and lists it again, unless an administrator delisted it.
func RecordPublish(ctx context.Context, db DB, rec PublishRecord) (int64, error) {
	snap := rec.Snapshot
	category := snap.Category
	if rec.Category != "" {
		category = rec.Category
	}
	tags := snap.Tags
	if tags == nil {
		tags = []string{}
	}
	var publisher any
	if rec.PublisherID > 0 {
		publisher = rec.PublisherID
	}
	types := snap.Requirements.Types()
	var listingID int64
	err := db.QueryRow(ctx, `
INSERT INTO elitea_marketplace.listings
    (kind, source_project_id, source_entity_id, entity_id, name, description, icon, category, tags,
     required_types, has_write_tools, search_text, publisher_id, risk_tier)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT ON CONSTRAINT listings_origin_key DO UPDATE SET
    entity_id = EXCLUDED.entity_id, name = EXCLUDED.name, description = EXCLUDED.description,
    icon = EXCLUDED.icon, category = EXCLUDED.category, tags = EXCLUDED.tags,
    required_types = EXCLUDED.required_types, has_write_tools = EXCLUDED.has_write_tools,
    search_text = EXCLUDED.search_text, publisher_id = COALESCE(EXCLUDED.publisher_id, listings.publisher_id),
    risk_tier = EXCLUDED.risk_tier,
    state = CASE WHEN listings.delisted_by IS NOT NULL THEN 'delisted' ELSE 'listed' END,
    delisted_at = CASE WHEN listings.delisted_by IS NOT NULL THEN listings.delisted_at ELSE NULL END,
    delist_reason = CASE WHEN listings.delisted_by IS NOT NULL THEN listings.delist_reason ELSE '' END,
    updated_at = clock_timestamp()
RETURNING id`,
		snap.Kind, rec.SourceProjectID, rec.SourceEntityID, nullablePositive(rec.EntityID),
		clamp(snap.Name, 255), snap.Description, snap.Icon, clamp(category, 64), tags, types,
		snap.Requirements.HasWriteTools, searchText(snap.Name, snap.Description, snap.SoulSummary, strings.Join(tags, " ")),
		publisher, snap.Tier).Scan(&listingID)
	if err != nil {
		return 0, fmt.Errorf("marketplace: write the listing: %w", err)
	}
	reqs, err := json.Marshal(snap.Requirements)
	if err != nil {
		return 0, fmt.Errorf("marketplace: encode the requirements: %w", err)
	}
	if _, err := db.Exec(ctx, `
INSERT INTO elitea_marketplace.listing_versions
    (listing_id, version_id, name, digest, definition, requirements, release_notes, published_by, published_at)
VALUES ($1, $2, $3, $4, $5::jsonb, $6::jsonb, $7, $8, COALESCE($9::timestamptz, clock_timestamp()))
ON CONFLICT (listing_id, version_id) DO NOTHING`,
		listingID, rec.VersionID, clamp(rec.VersionName, 128), snap.Digest[:], string(snap.Definition), string(reqs),
		ClampReleaseNotes(rec.ReleaseNotes), publisher, nullableTime(rec.PublishedAt)); err != nil {
		return 0, fmt.Errorf("marketplace: write the listing version: %w", err)
	}
	if err := refreshFromCurrent(ctx, db, listingID); err != nil {
		return 0, err
	}
	return listingID, nil
}

func clamp(text string, limit int) string {
	if r := []rune(text); len(r) > limit {
		return string(r[:limit])
	}
	return text
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func nullablePositive(v int64) any {
	if v <= 0 {
		return nil
	}
	return v
}

// refreshFromCurrent sets the listing's tier, required types and write flag
// from its current version, the newest that is neither yanked nor withdrawn.
// A listing with no such version keeps the values it has.
func refreshFromCurrent(ctx context.Context, db DB, listingID int64) error {
	if _, err := db.Exec(ctx, `
UPDATE elitea_marketplace.listings AS l SET
    required_types = COALESCE(ARRAY(
        SELECT DISTINCT item->>'type'
        FROM jsonb_array_elements(COALESCE(cur.requirements->'items', '[]'::jsonb)) AS item
        WHERE item->>'kind' IN ('toolkit', 'mcp') AND COALESCE(item->>'type', '') <> ''
        ORDER BY 1), '{}'::text[]),
    has_write_tools = COALESCE((cur.requirements->>'has_write_tools')::boolean, false),
    risk_tier = CASE WHEN cur.requirements->>'tier' IN ('low', 'medium', 'high', 'critical')
                     THEN cur.requirements->>'tier' ELSE l.risk_tier END,
    updated_at = clock_timestamp()
FROM (
    SELECT requirements FROM elitea_marketplace.listing_versions
    WHERE listing_id = $1 AND withdrawn_at IS NULL AND state <> 'yanked'
    ORDER BY published_at DESC, version_id DESC LIMIT 1
) AS cur
WHERE l.id = $1`, listingID); err != nil {
		return fmt.Errorf("marketplace: refresh the listing: %w", err)
	}
	return nil
}

// Withdraw marks catalogue versions as unpublished by their publisher and
// delists a listing that has no hireable version left. The snapshots stay: a
// project may have them installed or as a fork origin. kind is the listing
// kind and sourceProjectID the project that published them.
func Withdraw(ctx context.Context, db DB, kind string, sourceProjectID int64, catalogueVersionIDs []int64) error {
	if len(catalogueVersionIDs) == 0 {
		return nil
	}
	rows, err := db.Query(ctx, `
UPDATE elitea_marketplace.listing_versions AS v SET withdrawn_at = COALESCE(v.withdrawn_at, clock_timestamp())
FROM elitea_marketplace.listings AS l
WHERE l.id = v.listing_id AND l.kind = $1 AND l.source_project_id = $2 AND v.version_id = ANY($3::bigint[])
RETURNING v.listing_id`, kind, sourceProjectID, catalogueVersionIDs)
	if err != nil {
		return fmt.Errorf("marketplace: withdraw the versions: %w", err)
	}
	seen := map[int64]bool{}
	var listings []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if !seen[id] {
			seen[id] = true
			listings = append(listings, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range listings {
		if _, err := db.Exec(ctx, `
UPDATE elitea_marketplace.listings AS l SET state = 'delisted', delisted_at = clock_timestamp(),
    delist_reason = 'The publisher unpublished it.', updated_at = clock_timestamp()
WHERE l.id = $1 AND l.state = 'listed' AND l.delisted_by IS NULL
  AND NOT EXISTS (SELECT 1 FROM elitea_marketplace.listing_versions v
                  WHERE v.listing_id = l.id AND v.withdrawn_at IS NULL)`, id); err != nil {
			return fmt.Errorf("marketplace: delist the listing: %w", err)
		}
		if err := refreshFromCurrent(ctx, db, id); err != nil {
			return err
		}
	}
	return nil
}

// SetVersionState sets available, deprecated or yanked on a version. who is
// the person acting.
func SetVersionState(ctx context.Context, db DB, listingID, versionID int64, state, reason string, who int64) (bool, error) {
	switch state {
	case VersionAvailable, VersionDeprecated, VersionYanked:
	default:
		return false, ErrInvalidState
	}
	reason = clamp(strings.TrimSpace(reason), 500)
	tag, err := db.Exec(ctx, `
UPDATE elitea_marketplace.listing_versions
SET state = $3, state_reason = $4, state_changed_at = clock_timestamp(), state_changed_by = $5
WHERE listing_id = $1 AND version_id = $2`, listingID, versionID, state, reason, who)
	if err != nil {
		return false, fmt.Errorf("marketplace: set the version state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	return true, refreshFromCurrent(ctx, db, listingID)
}

// SetFeatured sets the editorial Featured flag.
func SetFeatured(ctx context.Context, db DB, listingID int64, featured bool, who int64) (bool, error) {
	tag, err := db.Exec(ctx, `
UPDATE elitea_marketplace.listings SET featured = $2,
    featured_at = CASE WHEN $2 THEN clock_timestamp() ELSE NULL END,
    featured_by = CASE WHEN $2 THEN $3::bigint ELSE NULL END, updated_at = clock_timestamp()
WHERE id = $1`, listingID, featured, who)
	if err != nil {
		return false, fmt.Errorf("marketplace: set the featured flag: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SetListed lists or delists a listing for an administrator. A delisted listing
// leaves search and cannot be hired.
func SetListed(ctx context.Context, db DB, listingID int64, listed bool, reason string, who int64) (bool, error) {
	var tag interface{ RowsAffected() int64 }
	var err error
	if listed {
		tag, err = db.Exec(ctx, `
UPDATE elitea_marketplace.listings SET state = 'listed', delisted_at = NULL, delisted_by = NULL,
    delist_reason = '', updated_at = clock_timestamp()
WHERE id = $1 AND EXISTS (SELECT 1 FROM elitea_marketplace.listing_versions v
                          WHERE v.listing_id = $1 AND v.withdrawn_at IS NULL)`, listingID)
	} else {
		tag, err = db.Exec(ctx, `
UPDATE elitea_marketplace.listings SET state = 'delisted', delisted_at = clock_timestamp(), delisted_by = $3,
    delist_reason = $2, updated_at = clock_timestamp()
WHERE id = $1`, listingID, clamp(strings.TrimSpace(reason), 500), who)
	}
	if err != nil {
		return false, fmt.Errorf("marketplace: set the listing state: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// PublisherOf is the person who published the listing last; 0 when unknown.
func PublisherOf(ctx context.Context, db DB, listingID int64) (int64, error) {
	var id *int64
	err := db.QueryRow(ctx, `SELECT publisher_id FROM elitea_marketplace.listings WHERE id = $1`, listingID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("marketplace: read the publisher: %w", err)
	}
	if id == nil {
		return 0, nil
	}
	return *id, nil
}

// PurgeUnheld deletes the snapshots that no project holds and that the
// publisher withdrew more than olderThanDays ago, and the listings
// left with no version at all (ADR-0051 decision 2: the retention of ADR-0042).
func PurgeUnheld(ctx context.Context, db DB, olderThanDays int) (int64, error) {
	// A dismissed hire holds nothing: its agent is gone. Drop those rows first, or
	// the restricting foreign key would keep the snapshot for ever.
	if _, err := db.Exec(ctx, `
DELETE FROM elitea_marketplace.installs AS d
USING elitea_marketplace.listing_versions AS v
WHERE d.dismissed_at IS NOT NULL AND d.listing_id = v.listing_id AND d.version_id = v.version_id
  AND v.withdrawn_at < clock_timestamp() - make_interval(days => $1)
  AND NOT EXISTS (SELECT 1 FROM elitea_marketplace.installs a
                  WHERE a.listing_id = v.listing_id AND a.version_id = v.version_id AND a.dismissed_at IS NULL)`, olderThanDays); err != nil {
		return 0, fmt.Errorf("marketplace: drop the dismissed hires: %w", err)
	}
	tag, err := db.Exec(ctx, `
DELETE FROM elitea_marketplace.listing_versions AS v
WHERE (v.withdrawn_at < clock_timestamp() - make_interval(days => $1))
  AND NOT EXISTS (SELECT 1 FROM elitea_marketplace.installs i
                  WHERE i.listing_id = v.listing_id AND i.version_id = v.version_id)`, olderThanDays)
	if err != nil {
		return 0, fmt.Errorf("marketplace: purge the snapshots: %w", err)
	}
	removed := tag.RowsAffected()
	if _, err := db.Exec(ctx, `
DELETE FROM elitea_marketplace.listings AS l
WHERE NOT EXISTS (SELECT 1 FROM elitea_marketplace.listing_versions v WHERE v.listing_id = l.id)`); err != nil {
		return removed, fmt.Errorf("marketplace: purge the empty listings: %w", err)
	}
	return removed, nil
}
