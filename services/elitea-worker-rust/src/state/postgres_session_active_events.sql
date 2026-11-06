, bounds AS (
 SELECT max(event_ordinal) FILTER (WHERE recovery_marker AND snapshot_agent IS NOT NULL) AS latest_snapshot,
        max(event_ordinal) AS last_event,
        (COALESCE(bool_or(branch <> ''), false) OR $12::boolean) AS has_branches
 FROM classified
), boundary AS (
 SELECT bounds.*, classified.snapshot_agent AS retained_snapshot_agent
 FROM bounds LEFT JOIN classified ON classified.event_ordinal = bounds.latest_snapshot
), ranked AS (
 SELECT classified.*, boundary.*,
        max(event_ordinal) FILTER (WHERE recovery_marker) OVER (PARTITION BY branch) AS latest_marker
 FROM classified CROSS JOIN boundary
), active_events AS (
 SELECT * FROM ranked WHERE
   (recovery_marker AND (event_ordinal = latest_marker
     OR (event_ordinal = latest_snapshot AND ($11::text IS NULL OR has_branches))))
   OR (NOT recovery_marker AND (
      has_branches
      OR (latest_snapshot IS NULL AND $11::text IS NULL)
      OR has_control
      OR author NOT IN ('user', COALESCE($11::text, retained_snapshot_agent))
      OR event_ordinal > CASE WHEN $11::text IS NOT NULL THEN last_event ELSE latest_snapshot END
   ))
)
