package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/conversationskills"
	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/skilltrust"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// ConversationSkillsRepo stores the skills pinned to a conversation (tenant
// 0251, ADR-0050 amendment 1, A1.2) and reads them for a turn.
type ConversationSkillsRepo struct {
	pool *pgxpool.Pool
}

func NewConversationSkillsRepo(pool *pgxpool.Pool) *ConversationSkillsRepo {
	return &ConversationSkillsRepo{pool: pool}
}

var (
	_ conversationskills.Repository                      = (*ConversationSkillsRepo)(nil)
	_ agentexecutionapp.CurrentConversationSkillResolver = (*ConversationSkillsRepo)(nil)
)

func (r *ConversationSkillsRepo) actor(ctx context.Context) (int64, error) {
	user, ok := auth.UserFromContext(ctx)
	if !ok {
		return 0, apierr.Unauthorized("authentication required")
	}
	id, ok := user.OwningUserID()
	if !ok {
		return 0, apierr.Forbidden("a conversation skill is pinned by a person")
	}
	return id, nil
}

// pinColumns reads a pin with what the list shows of its skill: for a project
// skill the name, description and the revisions this project trusts; the
// text of a listing is never read here.
func pinColumns(schema string) string {
	return fmt.Sprintf(`
SELECT cs.id, cs.skill_id, cs.listing_id, cs.listing_version_id, cs.pinned_by, cs.pinned_at,
       COALESCE(sk.name, ''), COALESCE(sk.description, ''),
       COALESCE(sv.instructions, ''),
       COALESCE(sv.meta ->> 'parent_project_id', ''),
       COALESCE((SELECT array_agg(st.revision) FROM %[1]s.skill_trust st
                 WHERE st.skill_id = sk.id AND st.revoked_at IS NULL), '{}'::text[])
FROM %[1]s.conversation_skills cs
LEFT JOIN %[1]s.skills sk ON sk.id = cs.skill_id
LEFT JOIN %[1]s.skill_versions sv ON sv.skill_id = sk.id AND sv.name = 'base'`, schema)
}

func (r *ConversationSkillsRepo) scanPins(ctx context.Context, schema string, projectID int64, rows pgx.Rows) ([]conversationskills.Pin, error) {
	defer rows.Close()
	var pins []conversationskills.Pin
	for rows.Next() {
		var (
			pin                   conversationskills.Pin
			skillID               *int32
			listingID, listingVer *int64
			pinnedAt              time.Time
			instructions, parent  string
			trusted               []string
		)
		if err := rows.Scan(&pin.ID, &skillID, &listingID, &listingVer, &pin.PinnedBy, &pinnedAt,
			&pin.Name, &pin.Description, &instructions, &parent, &trusted); err != nil {
			return nil, fmt.Errorf("conversation skills: scan: %w", err)
		}
		pin.PinnedAt = pinnedAt.UTC()
		if skillID != nil {
			id := int64(*skillID)
			pin.Kind, pin.SkillID = conversationskills.KindProject, &id
			pin.Trust = conversationskills.Trust{State: "trusted"}
			if publisher, foreign := skilltrust.Foreign(parent, projectID); foreign {
				if publisher > 0 {
					pin.Trust.PublisherProjectID = &publisher
				}
				if !skilltrust.Trusted(instructions, trusted) {
					pin.Trust.State = "untrusted"
				}
			}
		} else {
			pin.Kind, pin.ListingID, pin.ListingVersionID = conversationskills.KindListing, listingID, listingVer
			// A listing is always untrusted: trusting needs a project skill.
			pin.Trust = conversationskills.Trust{State: "untrusted"}
		}
		pins = append(pins, pin)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("conversation skills: read: %w", err)
	}
	// The listing's own name and version come from the listing, one read each
	// (a conversation holds at most five pins).
	for index := range pins {
		if pins[index].Kind != conversationskills.KindListing {
			continue
		}
		var name, description, version string
		err := r.pool.QueryRow(ctx, `
SELECT l.name, l.description, lv.name
FROM elitea_marketplace.listing_versions lv
JOIN elitea_marketplace.listings l ON l.id = lv.listing_id
WHERE lv.listing_id = $1 AND lv.version_id = $2`, *pins[index].ListingID, *pins[index].ListingVersionID).Scan(&name, &description, &version)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("conversation skills: read listing: %w", err)
		}
		// A listing that is gone still shows its pin, so the person can remove it.
		pins[index].Name, pins[index].Description, pins[index].VersionName = name, description, version
	}
	_ = schema
	return pins, nil
}

func projectNumber(projectID string) (int64, error) {
	id, err := strconv.ParseInt(projectID, 10, 64)
	if err != nil || id <= 0 {
		return 0, apierr.NotFound("project not found")
	}
	return id, nil
}

func (r *ConversationSkillsRepo) List(ctx context.Context, projectID, conversationID string) ([]conversationskills.Pin, error) {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return nil, err
	}
	project, err := projectNumber(projectID)
	if err != nil {
		return nil, err
	}
	conversation, err := (&ConversationsRepo{pool: r.pool}).resolveConversationID(ctx, projectID, conversationID)
	if err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, pinColumns(schema)+` WHERE cs.conversation_id = $1 ORDER BY cs.id`, conversation)
	if err != nil {
		return nil, fmt.Errorf("conversation skills: list: %w", err)
	}
	return r.scanPins(ctx, schema, project, rows)
}

// lockConversation takes the conversation's row lock and answers how many pins
// it holds, so the five-pin bound holds under concurrent pins.
func lockConversation(ctx context.Context, tx pgx.Tx, schema string, conversation int64) (int, error) {
	var locked int64
	if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT id FROM %s.chat_conversations WHERE id = $1 FOR UPDATE`, schema), conversation).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, apierr.NotFound("conversation not found")
		}
		return 0, fmt.Errorf("conversation skills: lock: %w", err)
	}
	var held int
	if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s.conversation_skills WHERE conversation_id = $1`, schema), conversation).Scan(&held); err != nil {
		return 0, fmt.Errorf("conversation skills: count: %w", err)
	}
	return held, nil
}

func (r *ConversationSkillsRepo) PinSkill(ctx context.Context, projectID, conversationID string, skillID int64) (conversationskills.Pin, bool, error) {
	actor, err := r.actor(ctx)
	if err != nil {
		return conversationskills.Pin{}, false, err
	}
	schema, err := tenantSchema(projectID)
	if err != nil {
		return conversationskills.Pin{}, false, err
	}
	project, err := projectNumber(projectID)
	if err != nil {
		return conversationskills.Pin{}, false, err
	}
	// The caller must be able to read the skill: the same read, and the same
	// answer for a missing or hidden one, as a message's pick.
	if _, err := NewProjectSkillsRepo(r.pool).ResolveCurrentProjectSkills(ctx, project, []int64{skillID}); err != nil {
		if errors.Is(err, agentexecutionapp.ErrProjectSkillUnknown) {
			return conversationskills.Pin{}, false, apierr.NotFound("skill not found")
		}
		return conversationskills.Pin{}, false, err
	}
	conversation, err := (&ConversationsRepo{pool: r.pool}).resolveConversationID(ctx, projectID, conversationID)
	if err != nil {
		return conversationskills.Pin{}, false, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return conversationskills.Pin{}, false, fmt.Errorf("conversation skills: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	held, err := lockConversation(ctx, tx, schema, conversation)
	if err != nil {
		return conversationskills.Pin{}, false, err
	}
	var pinID int64
	err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT id FROM %s.conversation_skills WHERE conversation_id = $1 AND skill_id = $2`, schema), conversation, skillID).Scan(&pinID)
	created := false
	switch {
	case err == nil:
	case errors.Is(err, pgx.ErrNoRows):
		if held >= conversationskills.MaxPins {
			return conversationskills.Pin{}, false, apierr.Conflict("a conversation holds at most five skills")
		}
		if err := tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.conversation_skills (conversation_id, skill_id, pinned_by) VALUES ($1, $2, $3) RETURNING id`, schema),
			conversation, skillID, actor).Scan(&pinID); err != nil {
			return conversationskills.Pin{}, false, fmt.Errorf("conversation skills: pin: %w", err)
		}
		created = true
	default:
		return conversationskills.Pin{}, false, fmt.Errorf("conversation skills: read pin: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return conversationskills.Pin{}, false, fmt.Errorf("conversation skills: commit: %w", err)
	}
	pin, err := r.one(ctx, schema, project, pinID)
	return pin, created, err
}

func (r *ConversationSkillsRepo) one(ctx context.Context, schema string, project, pinID int64) (conversationskills.Pin, error) {
	rows, err := r.pool.Query(ctx, pinColumns(schema)+` WHERE cs.id = $1`, pinID)
	if err != nil {
		return conversationskills.Pin{}, fmt.Errorf("conversation skills: read pin: %w", err)
	}
	pins, err := r.scanPins(ctx, schema, project, rows)
	if err != nil {
		return conversationskills.Pin{}, err
	}
	if len(pins) != 1 {
		return conversationskills.Pin{}, apierr.NotFound("pin not found")
	}
	return pins[0], nil
}

func (r *ConversationSkillsRepo) PinListing(ctx context.Context, projectID, conversationID string, listingID, versionID int64) (conversationskills.Pin, bool, error) {
	actor, err := r.actor(ctx)
	if err != nil {
		return conversationskills.Pin{}, false, err
	}
	schema, err := tenantSchema(projectID)
	if err != nil {
		return conversationskills.Pin{}, false, err
	}
	project, err := projectNumber(projectID)
	if err != nil {
		return conversationskills.Pin{}, false, err
	}
	// Only a skill listing that is listed, at a version that is not yanked or
	// withdrawn: one generic answer for every other case.
	var ok bool
	if err := r.pool.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM elitea_marketplace.listing_versions lv
  JOIN elitea_marketplace.listings l ON l.id = lv.listing_id
  WHERE lv.listing_id = $1 AND lv.version_id = $2
    AND l.kind = 'skill' AND l.state = 'listed' AND lv.state <> 'yanked' AND lv.withdrawn_at IS NULL)`,
		listingID, versionID).Scan(&ok); err != nil {
		return conversationskills.Pin{}, false, fmt.Errorf("conversation skills: read listing: %w", err)
	}
	if !ok {
		return conversationskills.Pin{}, false, apierr.NotFound("skill listing not found")
	}
	conversation, err := (&ConversationsRepo{pool: r.pool}).resolveConversationID(ctx, projectID, conversationID)
	if err != nil {
		return conversationskills.Pin{}, false, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return conversationskills.Pin{}, false, fmt.Errorf("conversation skills: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	held, err := lockConversation(ctx, tx, schema, conversation)
	if err != nil {
		return conversationskills.Pin{}, false, err
	}
	var pinID int64
	err = tx.QueryRow(ctx, fmt.Sprintf(`
SELECT id FROM %s.conversation_skills WHERE conversation_id = $1 AND listing_id = $2 AND listing_version_id = $3`, schema),
		conversation, listingID, versionID).Scan(&pinID)
	created := false
	switch {
	case err == nil:
	case errors.Is(err, pgx.ErrNoRows):
		if held >= conversationskills.MaxPins {
			return conversationskills.Pin{}, false, apierr.Conflict("a conversation holds at most five skills")
		}
		if err := tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.conversation_skills (conversation_id, listing_id, listing_version_id, pinned_by) VALUES ($1, $2, $3, $4) RETURNING id`, schema),
			conversation, listingID, versionID, actor).Scan(&pinID); err != nil {
			return conversationskills.Pin{}, false, fmt.Errorf("conversation skills: pin: %w", err)
		}
		created = true
	default:
		return conversationskills.Pin{}, false, fmt.Errorf("conversation skills: read pin: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return conversationskills.Pin{}, false, fmt.Errorf("conversation skills: commit: %w", err)
	}
	pin, err := r.one(ctx, schema, project, pinID)
	return pin, created, err
}

func (r *ConversationSkillsRepo) Unpin(ctx context.Context, projectID, conversationID string, pinID int64) error {
	schema, err := tenantSchema(projectID)
	if err != nil {
		return err
	}
	conversation, err := (&ConversationsRepo{pool: r.pool}).resolveConversationID(ctx, projectID, conversationID)
	if err != nil {
		return err
	}
	tag, err := r.pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.conversation_skills WHERE id = $1 AND conversation_id = $2`, schema), pinID, conversation)
	if err != nil {
		return fmt.Errorf("conversation skills: unpin: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return apierr.NotFound("pin not found")
	}
	return nil
}

// ResolveConversationSkills reads the pins of a conversation for a turn: the
// pinned project skill ids (read later under the caller's visibility, like a
// message's picks) and the pinned listing versions' text. A pinned listing
// version that is yanked, withdrawn or delisted answers ErrProjectSkillUnknown,
// the answer a missing skill gets, so the turn is refused and nothing says why.
func (r *ConversationSkillsRepo) ResolveConversationSkills(ctx context.Context, projectID int64, conversationUUID string) (agentexecutionapp.ConversationSkills, error) {
	var result agentexecutionapp.ConversationSkills
	if r == nil || r.pool == nil {
		return result, errors.New("conversation skills: no pool")
	}
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return result, err
	}
	rows, err := r.pool.Query(ctx, fmt.Sprintf(`
SELECT cs.skill_id, cs.listing_id, cs.listing_version_id
FROM %[1]s.conversation_skills cs
JOIN %[1]s.chat_conversations c ON c.id = cs.conversation_id
WHERE c.uuid = $1::uuid ORDER BY cs.id`, schema), conversationUUID)
	if err != nil {
		return result, fmt.Errorf("conversation skills: read pins: %w", err)
	}
	type listingRef struct{ listing, version int64 }
	var listings []listingRef
	for rows.Next() {
		var skillID *int32
		var listingID, versionID *int64
		if err := rows.Scan(&skillID, &listingID, &versionID); err != nil {
			rows.Close()
			return result, fmt.Errorf("conversation skills: scan pins: %w", err)
		}
		switch {
		case skillID != nil:
			result.ProjectSkillIDs = append(result.ProjectSkillIDs, int64(*skillID))
		case listingID != nil && versionID != nil:
			listings = append(listings, listingRef{*listingID, *versionID})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("conversation skills: read pins: %w", err)
	}
	for _, ref := range listings {
		var definition []byte
		err := r.pool.QueryRow(ctx, `
SELECT lv.definition FROM elitea_marketplace.listing_versions lv
JOIN elitea_marketplace.listings l ON l.id = lv.listing_id
WHERE lv.listing_id = $1 AND lv.version_id = $2
  AND l.kind = 'skill' AND l.state = 'listed' AND lv.state <> 'yanked' AND lv.withdrawn_at IS NULL`,
			ref.listing, ref.version).Scan(&definition)
		if errors.Is(err, pgx.ErrNoRows) {
			return result, agentexecutionapp.ErrProjectSkillUnknown
		}
		if err != nil {
			return result, fmt.Errorf("conversation skills: read listing text: %w", err)
		}
		var body struct {
			Name         string `json:"name"`
			Description  string `json:"description"`
			Instructions string `json:"instructions"`
		}
		if err := json.Unmarshal(definition, &body); err != nil {
			return result, agentexecutionapp.ErrProjectSkillUnknown
		}
		result.Listing = append(result.Listing, agentexecutionapp.CurrentListingSkill{
			ListingID: ref.listing, VersionID: ref.version,
			Name: body.Name, Description: body.Description, Instructions: body.Instructions,
		})
	}
	return result, nil
}
