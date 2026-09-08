package eliteacore

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

/* ── the withdrawn release name ───────────────────────────────────────────── */

// withdrawnNameMarker separates a withdrawn clone's old release name from the
// counter that makes the new name unique. The characters are the ones a version
// name is allowed to carry (`^[a-zA-Z0-9._-]+$`), so a renamed row is still a
// name the publish route would accept.
const withdrawnNameMarker = "-withdrawn-"

// versionNameMaxLen is the width of application_versions.name (VARCHAR(128)).
// A base name long enough to fill it is truncated so the suffix fits, rather
// than making the rename fail on a value too long for the column.
const versionNameMaxLen = 128

// withdrawnNameAttempts is how many counters are tried before the rename gives
// up. An agent with fifty withdrawn releases of one name is beyond what a
// counter is for; the release name simply stays taken in that case, which is
// the behaviour this whole function replaces and therefore a safe floor.
const withdrawnNameAttempts = 50

// releaseWithdrawnVersionName frees the release name a withdrawn clone holds.
//
// WHY THE RENAME EXISTS. Withdrawal reverts the published clone to a draft
// rather than deleting it, so the author keeps the version they published. The
// reverted row kept the RELEASE NAME as well, and the name is unique per agent
// (`_application_version_name_uc`), so the name was spent for good: publish →
// withdraw → publish again under the same name was refused permanently, with no
// way to recover the name (issue 854). That is not what a withdrawal means. A
// release that is no longer live must not keep holding its name.
//
// WHY NOT DELETE THE CLONE. The clone is the only record of what was actually
// published, and something may still point at it — a conversation that ran
// against the published version, an export, a link an author kept. Renaming
// gives the name back without taking the row away, and the old name is written
// into the row's own `meta` as `withdrawn_from_name`, so the version list can
// still say which release this row was.
//
// The rename is BEST EFFORT: a withdrawal that has already taken the agent out
// of the catalogue must answer 200. A failure here leaves the name taken, which
// is exactly the state this platform was in before, so the caller loses nothing
// they had.
func (h *Handler) releaseWithdrawnVersionName(ctx context.Context, schema, versionID string) {
	var appID int
	var currentName string
	if err := h.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT application_id, COALESCE(name, '') FROM %s.application_versions WHERE id = $1`, schema),
		versionID).Scan(&appID, &currentName); err != nil {
		return // the row is gone (an embedded copy this withdrawal deleted), or unreadable
	}
	if currentName == "" {
		return
	}

	for attempt := 1; attempt <= withdrawnNameAttempts; attempt++ {
		candidate := withdrawnVersionName(currentName, attempt)
		tag, err := h.pool.Exec(ctx, fmt.Sprintf(`
			UPDATE %s.application_versions
			SET name = $2,
			    meta = COALESCE(meta, '{}'::jsonb) || jsonb_build_object('withdrawn_from_name', $3::text)
			WHERE id = $1
			  AND NOT EXISTS (
			      SELECT 1 FROM %s.application_versions other
			      WHERE other.application_id = $4 AND other.name = $2
			  )`, schema, schema), versionID, candidate, currentName, appID)
		if err != nil {
			// A concurrent withdrawal of another clone can win the same
			// candidate between the NOT EXISTS and the write; the unique
			// constraint reports it and the next counter is tried.
			if strings.Contains(err.Error(), "_application_version_name_uc") || strings.Contains(err.Error(), "duplicate key") {
				continue
			}
			slog.ErrorContext(ctx, "unpublish: releasing the withdrawn release name failed",
				"schema", schema, "version_id", versionID, "error", err)
			return
		}
		if tag.RowsAffected() > 0 {
			return
		}
	}
	slog.WarnContext(ctx, "unpublish: the withdrawn release name is still taken",
		"schema", schema, "version_id", versionID, "name", currentName)
}

// withdrawnVersionName is the name the clone carries once its release name has
// been released. The base is truncated when the suffix would not fit the
// column, and the counter always survives the truncation: it is what makes the
// name unique.
func withdrawnVersionName(base string, attempt int) string {
	suffix := fmt.Sprintf("%s%d", withdrawnNameMarker, attempt)
	// Counted in RUNES, because VARCHAR(128) counts characters and because
	// cutting a name mid-rune would store bytes PostgreSQL refuses.
	if runes := []rune(base); len(runes)+len([]rune(suffix)) > versionNameMaxLen {
		base = string(runes[:versionNameMaxLen-len([]rune(suffix))])
	}
	return base + suffix
}
