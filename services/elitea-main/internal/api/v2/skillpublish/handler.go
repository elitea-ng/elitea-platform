// Package skillpublish is the skill-level publishing surface (#249): the six
// legacy modules that let a skill leave its project and appear in the public
// catalog, plus the three small skills-parity extras that sit in the same
// domain.
//
// Legacy module → Go route (all under /api/v2/elitea_core, prompt_lib mode):
//
//	publish_skill          → POST   /publish_skill/prompt_lib/{projectID}/{skillID}/{versionID}
//	unpublish_skill        → POST   /unpublish_skill/prompt_lib/{projectID}/{skillID}/{versionID}
//	publish_skill_validate → POST   /publish_skill_validate/prompt_lib/{projectID}/{skillID}/{versionID}
//	public_skills          → GET    /public_skills/prompt_lib
//	public_skill           → GET    /public_skill/prompt_lib/{skillID}[/{versionName}]
//	attach_public_skill    → POST   /attach_public_skill/prompt_lib/{projectID}
//	skill_categories       → GET    /skill_categories/prompt_lib/{projectID}
//	skill_export_fork      → GET    /skill_export_fork/prompt_lib/{projectID}/{skillID}[/{versionID}]
//	agents_with_skill      → GET    /agents_with_skill/prompt_lib/{projectID}/{skillID}
//
// The publishing model is the one application publishing already established in
// `internal/api/v2/eliteacore/handler.go` and the one the legacy runtime
// implements for skills, which agree on the shape that matters here:
//
//   - publishing SNAPSHOTS. It never flips the version the user is editing; it
//     materialises a new source version under the requested `version_name` and
//     publishes that. Editing the draft afterwards cannot mutate what the
//     catalog serves.
//   - the public catalog lives in ONE project schema, `p_{PUBLIC_PROJECT_ID}`.
//     A skill published out of project 7 gets a twin skill row there, keyed
//     back to its origin by (shared_owner_id, shared_id) so the second publish
//     of the same skill appends a version instead of forking a second catalog
//     entry.
//   - unpublishing DELETES the public copy and reverts the source version to
//     draft. It does not leave a hidden published row behind.
//
// Deliberately not ported, each for one reason:
//
//   - AI pre-publish validation (`run_skill_ai_validation`). The Predictor
//     transport it needs was retired from this service (#126/#194), so there is
//     nothing to call. Validation here is the deterministic half only, and says
//     so on the wire: `ai_validation_available: false`.
//   - the icon checker (`SkillIconChecker`, a CRITICAL for a missing icon).
//     Skill icons are #36 and do not exist in this stack yet; porting the check
//     would make every publish fail for a field no client can set.
//   - the unpublish notification (`notify_skill_author_unpublished`). It fires
//     a legacy RPC event; the Go notifications surface has no skill event type.
//   - `my_liked` / `trend_*` filters on the public listing, and the
//     likes/is_liked/authors decoration on its rows. Those come from the social
//     plugin's like store, which is a separate surface.
//
// Admin-configurable extra skill categories
// (`skill_publishing_guardrail.skill_categories`) WERE on that list, for the
// reason that the Go admin config schema had no section to author them in and
// reading a key no page can write is the lookup-that-can-only-miss defect
// `AgentCategories` documents. The `skill_publishing` section exists now, so
// they are read (categories.go) and the guardrail is the skill one
// (`publishBlocked`).
package skillpublish

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// maxPublishedVersionsPerSkill caps how many versions of one skill the public
// catalog carries at once, matching the reference's
// `max_published_versions_per_skill` default of 3.
const maxPublishedVersionsPerSkill = 3

// defaultVersionName is the name every skill's implicit single version carries
// (repos/skills.go upsertBaseSkillVersion, and the reference's
// DEFAULT_VERSION_NAME).
const defaultVersionName = "base"

// entityTypeAgent is the only value the legacy SkillEntityTypes enum has.
const entityTypeAgent = "agent"

type Handler struct {
	pool *pgxpool.Pool
	// validationSecret keys the validation token. It is generated per process
	// rather than read from configuration because the token is only meaningful
	// between a validate call and the publish that follows it, which is one
	// user action against one process. A restart invalidates outstanding
	// tokens, and the publish path answers that with the same
	// "validate again" error a tampered token gets.
	validationSecret []byte
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	secret := make([]byte, 32)
	_, _ = rand.Read(secret) // crypto/rand.Read never fails on supported platforms
	return &Handler{pool: pool, validationSecret: secret}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// projectSchema turns a path segment into the tenant schema, QUOTED as a
// PostgreSQL identifier and ready to interpolate with %s.
//
// Every query in this package interpolates the schema into its statement text,
// because a schema name cannot be bound as a parameter. A segment that is not
// a plain decimal project id is refused here, and the name that does pass is
// quoted with SQL rules rather than with %q, which quotes with Go rules and
// lets an embedded quote close the identifier (issue #543).
func projectSchema(raw string) (string, bool) {
	quoted, err := tenantschema.Quote(raw)
	if err != nil {
		return "", false
	}
	return quoted, true
}

// publicSchema is projectSchema for the PUBLIC project, whose schema holds the
// published catalogue. publicProjectID only returns a validated id, so the
// quoting cannot fail.
func publicSchema() string {
	quoted, err := tenantschema.Quote(publicProjectID())
	if err != nil {
		return `"p_1"`
	}
	return quoted
}

func isPositiveInt(raw string) bool {
	value, err := strconv.ParseInt(raw, 10, 64)
	return err == nil && value > 0
}

// publicProjectID is the project whose schema holds the public catalog. Same
// env var and same default as the application-level publish surface.
func publicProjectID() string {
	if id := os.Getenv("PUBLIC_PROJECT_ID"); tenantschema.Valid(id) {
		return id
	}
	return "1"
}

// actingUserID resolves the author id to stamp on published rows, falling back
// to the id already on the source row when the request carries no resolvable
// user. The fallback is the SOURCE's author rather than a hardcoded 1: a
// published snapshot attributed to user 1 misattributes authorship in the
// catalog, which is the defect #161 records on the social surface.
func actingUserID(ctx context.Context, fallback int) int {
	user, ok := auth.UserFromContext(ctx)
	if !ok {
		return fallback
	}
	// The author columns these ids land in are INTEGER, so anything outside
	// int32 is not a user id this schema can hold — and converting it to `int`
	// unchecked would truncate it into one that belongs to somebody else on a
	// 32-bit build. Out of range falls back rather than mis-attributing.
	if id, ok := user.OwningUserID(); ok && id <= math.MaxInt32 {
		return int(id)
	}
	return fallback
}

// publishBlocked reports whether platform policy forbids publishing from a
// project.
//
// This reads the SKILL guardrail — `skill_publishing.is_skill_publish_blocked`
// and its whitelist — and no longer the agent one.
//
// It used to read `agent_publishing`, and that was the right answer while it
// was true: the admin Features page declared exactly one publishing section, so
// a skill-only switch would have been a control no operator could reach, and
// this comment named itself as "the single call site to repoint" if a
// skill-specific section ever arrived. It has (`skillPublishingSection` in
// internal/api/v2/admin/config_schemas.go), so this is that repoint.
//
// The consequence is deliberate and worth stating: a deployment that had
// blocked AGENT publishing was, until now, also refusing skill publishes. After
// this it is not — the skill switch defaults to off, and an operator who wants
// both frozen throws both. That is the reference's behaviour
// (`is_skill_publish_blocked` is a separate flag in its platform_settings
// payload) and the one the admin page's two independent switches now promise.
func (h *Handler) publishBlocked(ctx context.Context, projectID string) bool {
	values, err := platformconfig.Load(ctx, h.pool, platformconfig.SectionSkillPublishing)
	if err != nil {
		// Permissive on an unreadable store, for the reason
		// eliteacore/platform_flags.go documents: a database hiccup must not
		// present itself as a platform-wide publishing freeze.
		return false
	}
	if !values.Bool(platformconfig.KeySkillPublishBlocked, false) {
		return false
	}
	parsed, err := strconv.ParseInt(projectID, 10, 64)
	if err != nil {
		return true
	}
	for _, allowed := range values.Ints(platformconfig.KeySkillPublishWhitelistProjectIDs) {
		if allowed == parsed {
			return false
		}
	}
	return true
}

/* ── validation token ─────────────────────────────────────────────────────── */

// contentHash pins everything the validation gate judged. Publishing with a
// token whose hash no longer matches means the skill was edited after it
// passed, so the token is refused and validation runs again.
func contentHash(name, description, instructions string) string {
	payload, _ := json.Marshal(map[string]string{
		"name":         name,
		"description":  description,
		"instructions": instructions,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func (h *Handler) issueValidationToken(versionID, hash string) string {
	mac := hmac.New(sha256.New, h.validationSecret)
	mac.Write([]byte(versionID + ":" + hash))
	return hex.EncodeToString(mac.Sum(nil))
}

func (h *Handler) verifyValidationToken(token, versionID, hash string) bool {
	expected := h.issueValidationToken(versionID, hash)
	return hmac.Equal([]byte(strings.ToLower(token)), []byte(expected))
}

/* ── shared row readers ───────────────────────────────────────────────────── */

// queryExecer is the subset of pgx both *pgxpool.Pool and pgx.Tx satisfy, so
// the row helpers below work inside a transaction and outside one.
type queryExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// skillVersionRow is one skill_versions row plus the parent skill's presentation
// fields — the unit every path in this package works with.
type skillVersionRow struct {
	VersionID    int
	SkillID      int
	VersionName  string
	Instructions string
	Status       string
	AuthorID     int
	Meta         map[string]any
	Tags         []string

	SkillName        string
	SkillDescription string
}

func (h *Handler) readSkillVersion(ctx context.Context, schema string, skillID, versionID string) (skillVersionRow, bool) {
	var row skillVersionRow
	var metaText string
	err := h.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT sv.id, sv.skill_id, sv.name, sv.instructions, sv.status, sv.author_id,
		       COALESCE(sv.meta::text, '{}'), sk.name, COALESCE(sk.description, '')
		FROM %s.skill_versions sv
		JOIN %s.skills sk ON sk.id = sv.skill_id
		WHERE sv.id = $1 AND sv.skill_id = $2`, schema, schema), versionID, skillID).
		Scan(&row.VersionID, &row.SkillID, &row.VersionName, &row.Instructions, &row.Status,
			&row.AuthorID, &metaText, &row.SkillName, &row.SkillDescription)
	if err != nil {
		return skillVersionRow{}, false
	}
	_ = json.Unmarshal([]byte(metaText), &row.Meta) // DB jsonb column; malformed means nil meta
	row.Tags = h.readVersionTags(ctx, schema, row.VersionID)
	return row, true
}

func (h *Handler) readVersionTags(ctx context.Context, schema string, versionID int) []string {
	rows, err := h.pool.Query(ctx, fmt.Sprintf(`
		SELECT t.name FROM %s.skill_version_tag_association svta
		JOIN %s.tags t ON t.id = svta.tag_id
		WHERE svta.version_id = $1
		ORDER BY t.name`, schema, schema), versionID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var tags []string
	for rows.Next() {
		var name string
		if rows.Scan(&name) != nil {
			continue
		}
		tags = append(tags, name)
	}
	return tags
}

// applyTags replaces a version's tag associations with exactly `tags`, upserting
// each tag by name (tags.name is UNIQUE per schema) — the same delete-then-
// reinsert shape repos/skills.go uses for the base version.
func applyTags(ctx context.Context, tx queryExecer, schema string, versionID int, tags []string) error {
	if _, err := tx.Exec(ctx, fmt.Sprintf(
		`DELETE FROM %s.skill_version_tag_association WHERE version_id = $1`, schema), versionID); err != nil {
		return err
	}
	seen := make(map[string]bool, len(tags))
	for _, name := range tags {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true

		var tagID int
		if err := tx.QueryRow(ctx, fmt.Sprintf(`
			INSERT INTO %s.tags (name) VALUES ($1)
			ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
			RETURNING id`, schema), name).Scan(&tagID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %s.skill_version_tag_association (version_id, tag_id)
			VALUES ($1, $2) ON CONFLICT DO NOTHING`, schema), versionID, tagID); err != nil {
			return err
		}
	}
	return nil
}
