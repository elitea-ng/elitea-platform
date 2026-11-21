package marketplace

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// The signals of a listing (ADR-0051 decision 5): active hires, 30-day
// retention, dismissal rate and upgrade adoption. A daily pass computes them
// from the installs and stores them in elitea_marketplace.listing_signals_daily
// (shared migration 0252). They sit beside the rating, so a high rating with a
// high dismissal rate shows.
//
// Version 0 is the whole listing. A version's own row holds the hires, the
// dismissals and the retention of that version; upgrade adoption is a property
// of the listing and is only on the version 0 row.

// SignalsRetentionDays is how long the daily rows are kept.
const SignalsRetentionDays = 400

// Signals are the figures of a listing or of one version of it. A rate is null
// when its denominator is zero: nothing measured is not zero.
type Signals struct {
	Day             string   `json:"day"`
	ActiveHires     int      `json:"active_hires"`
	HiresTotal      int      `json:"hires_total"`
	Dismissed       int      `json:"dismissed"`
	DismissalRate   *float64 `json:"dismissal_rate"`
	RetentionCohort int      `json:"retention_cohort"`
	Retention30d    *float64 `json:"retention_30d"`
	UpgradeEligible int      `json:"upgrade_eligible"`
	UpgradeAdoption *float64 `json:"upgrade_adoption"`
	// Live is true when no daily row exists yet and the figures were computed
	// at the request.
	Live bool `json:"live"`
}

// SignalsSet is the signals of a listing and of each of its versions.
type SignalsSet struct {
	Listing  Signals            `json:"listing"`
	Versions map[string]Signals `json:"versions"`
}

func rate(part, whole int) *float64 {
	if whole <= 0 {
		return nil
	}
	value := float64(int(float64(part)/float64(whole)*1000+0.5)) / 1000
	return &value
}

type signalRow struct {
	ListingID, VersionID                       int64
	Day                                        time.Time
	Active, Total, Dismissed, Cohort, Retained int
	UpgradeEligible, UpgradeAdopted            int
}

func (r signalRow) signals(live bool) Signals {
	return Signals{
		Day:         r.Day.Format("2006-01-02"),
		ActiveHires: r.Active, HiresTotal: r.Total, Dismissed: r.Dismissed,
		DismissalRate:   rate(r.Dismissed, r.Total),
		RetentionCohort: r.Cohort, Retention30d: rate(r.Retained, r.Cohort),
		UpgradeEligible: r.UpgradeEligible, UpgradeAdoption: rate(r.UpgradeAdopted, r.UpgradeEligible),
		Live: live,
	}
}

// signalsSelect computes the figures of every listing, or of the listings in
// $1 when it is not null, from the installs. The current version of a listing
// is its newest version that is neither yanked nor withdrawn.
const signalsSelect = `
WITH cur AS (
    SELECT DISTINCT ON (v.listing_id) v.listing_id, v.version_id, v.published_at
    FROM elitea_marketplace.listing_versions v
    WHERE v.withdrawn_at IS NULL AND v.state <> 'yanked'
    ORDER BY v.listing_id, v.published_at DESC, v.version_id DESC)
SELECT i.listing_id,
       COALESCE(i.version_id, 0)::bigint,
       (count(DISTINCT i.project_id) FILTER (WHERE i.dismissed_at IS NULL))::int,
       count(*)::int,
       (count(*) FILTER (WHERE i.dismissed_at IS NOT NULL))::int,
       (count(*) FILTER (WHERE i.created_at <= clock_timestamp() - interval '30 days'))::int,
       (count(*) FILTER (WHERE i.created_at <= clock_timestamp() - interval '30 days'
                           AND (i.dismissed_at IS NULL OR i.dismissed_at >= i.created_at + interval '30 days')))::int,
       CASE WHEN GROUPING(i.version_id) = 1 THEN
            (count(*) FILTER (WHERE i.dismissed_at IS NULL AND c.published_at IS NOT NULL AND i.created_at < c.published_at))::int
            ELSE 0 END,
       CASE WHEN GROUPING(i.version_id) = 1 THEN
            (count(*) FILTER (WHERE i.dismissed_at IS NULL AND c.published_at IS NOT NULL AND i.created_at < c.published_at AND i.version_id = c.version_id))::int
            ELSE 0 END
FROM elitea_marketplace.installs i
LEFT JOIN cur c ON c.listing_id = i.listing_id
WHERE ($1::bigint[] IS NULL OR i.listing_id = ANY($1::bigint[]))
GROUP BY GROUPING SETS ((i.listing_id, i.version_id), (i.listing_id))`

// ComputeSignals writes today's rows for every listing that has a hire, and
// deletes the rows older than SignalsRetentionDays. It answers the number of
// rows written.
func ComputeSignals(ctx context.Context, db DB) (int64, error) {
	rows, err := db.Query(ctx, signalsSelect, nil)
	if err != nil {
		return 0, fmt.Errorf("marketplace: compute the signals: %w", err)
	}
	var computed []signalRow
	for rows.Next() {
		var r signalRow
		if err := rows.Scan(&r.ListingID, &r.VersionID, &r.Active, &r.Total, &r.Dismissed, &r.Cohort, &r.Retained,
			&r.UpgradeEligible, &r.UpgradeAdopted); err != nil {
			rows.Close()
			return 0, err
		}
		computed = append(computed, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var written int64
	for _, r := range computed {
		if _, err := db.Exec(ctx, `
INSERT INTO elitea_marketplace.listing_signals_daily
    (listing_id, version_id, day, active_hires, hires_total, dismissed, cohort_30d, retained_30d,
     upgrade_eligible, upgrade_adopted)
VALUES ($1, $2, (clock_timestamp() AT TIME ZONE 'UTC')::date, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (listing_id, version_id, day) DO UPDATE SET
    active_hires = EXCLUDED.active_hires, hires_total = EXCLUDED.hires_total, dismissed = EXCLUDED.dismissed,
    cohort_30d = EXCLUDED.cohort_30d, retained_30d = EXCLUDED.retained_30d,
    upgrade_eligible = EXCLUDED.upgrade_eligible, upgrade_adopted = EXCLUDED.upgrade_adopted,
    computed_at = clock_timestamp()`,
			r.ListingID, r.VersionID, r.Active, r.Total, r.Dismissed, r.Cohort, r.Retained,
			r.UpgradeEligible, r.UpgradeAdopted); err != nil {
			return written, fmt.Errorf("marketplace: store the signals: %w", err)
		}
		written++
	}
	if _, err := db.Exec(ctx, `
DELETE FROM elitea_marketplace.listing_signals_daily
WHERE day < (clock_timestamp() AT TIME ZONE 'UTC')::date - $1::int`, SignalsRetentionDays); err != nil {
		return written, fmt.Errorf("marketplace: prune the signals: %w", err)
	}
	return written, nil
}

// SignalsPass is one run of the daily routine: the removed hires, the signals,
// then the ratings. The reconcile runs first so the signals count a hire whose
// agent or skill was removed by a path that does not record it (#2089).
func SignalsPass(ctx context.Context, db DB, logger *slog.Logger) (int64, error) {
	dismissed, err := ReconcileDismissals(ctx, db)
	if err != nil {
		return 0, err
	}
	if dismissed > 0 {
		logger.Info("marketplace signals: hires of removed agents and skills dismissed", "hires", dismissed)
	}
	written, err := ComputeSignals(ctx, db)
	if err != nil {
		return written, err
	}
	if err := RefreshAllRatings(ctx, db); err != nil {
		return written, err
	}
	if written > 0 {
		logger.Info("marketplace signals: rows written", "rows", written)
	}
	return written, nil
}

// ReadSignals answers the signals of a listing and its versions: the newest
// daily row of each, or the figures computed now when the routine has not
// written one yet.
func ReadSignals(ctx context.Context, db DB, listingID int64) (SignalsSet, error) {
	set := SignalsSet{Versions: map[string]Signals{}}
	rows, err := db.Query(ctx, `
SELECT DISTINCT ON (version_id) version_id, day, active_hires, hires_total, dismissed, cohort_30d, retained_30d,
       upgrade_eligible, upgrade_adopted
FROM elitea_marketplace.listing_signals_daily WHERE listing_id = $1
ORDER BY version_id, day DESC`, listingID)
	if err != nil {
		return SignalsSet{}, fmt.Errorf("marketplace: read the signals: %w", err)
	}
	found := false
	for rows.Next() {
		var r signalRow
		if err := rows.Scan(&r.VersionID, &r.Day, &r.Active, &r.Total, &r.Dismissed, &r.Cohort, &r.Retained,
			&r.UpgradeEligible, &r.UpgradeAdopted); err != nil {
			rows.Close()
			return SignalsSet{}, err
		}
		found = true
		if r.VersionID == 0 {
			set.Listing = r.signals(false)
		} else {
			set.Versions[fmt.Sprint(r.VersionID)] = r.signals(false)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return SignalsSet{}, err
	}
	// The daily rows answer only when the listing row is today's: an older one
	// could sit beside version rows from other days, or miss a version that was
	// hired since.
	if found && set.Listing.Day == time.Now().UTC().Format("2006-01-02") {
		return set, nil
	}
	return liveSignals(ctx, db, listingID)
}

func liveSignals(ctx context.Context, db DB, listingID int64) (SignalsSet, error) {
	set := SignalsSet{Versions: map[string]Signals{}}
	set.Listing = signalRow{Day: time.Now().UTC()}.signals(true)
	rows, err := db.Query(ctx, signalsSelect, []int64{listingID})
	if err != nil {
		return SignalsSet{}, fmt.Errorf("marketplace: compute the signals: %w", err)
	}
	defer rows.Close()
	now := time.Now().UTC()
	for rows.Next() {
		var r signalRow
		if err := rows.Scan(&r.ListingID, &r.VersionID, &r.Active, &r.Total, &r.Dismissed, &r.Cohort, &r.Retained,
			&r.UpgradeEligible, &r.UpgradeAdopted); err != nil {
			return SignalsSet{}, err
		}
		r.Day = now
		if r.VersionID == 0 {
			set.Listing = r.signals(true)
		} else {
			set.Versions[fmt.Sprint(r.VersionID)] = r.signals(true)
		}
	}
	return set, rows.Err()
}

// ActiveProjectCounts answers, for each public catalogue agent id that has a
// listing, the number of projects with an active hire of it. An id with a
// listing and no hire answers zero; an id with no listing is absent. The
// admin published agents view reads it as its project counter.
func ActiveProjectCounts(ctx context.Context, db DB, entityIDs []int64) (map[int64]int, error) {
	out := map[int64]int{}
	if len(entityIDs) == 0 {
		return out, nil
	}
	rows, err := db.Query(ctx, `
SELECT l.entity_id,
       (SELECT count(DISTINCT i.project_id) FROM elitea_marketplace.installs i
        WHERE i.listing_id = l.id AND i.dismissed_at IS NULL)::int
FROM elitea_marketplace.listings l
WHERE l.kind = 'agent' AND l.entity_id = ANY($1::bigint[])`, entityIDs)
	if err != nil {
		return nil, fmt.Errorf("marketplace: read the project counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var entity int64
		var count int
		if err := rows.Scan(&entity, &count); err != nil {
			return nil, err
		}
		out[entity] = count
	}
	return out, rows.Err()
}
