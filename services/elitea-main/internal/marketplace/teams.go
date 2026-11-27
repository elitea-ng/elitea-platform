package marketplace

// Saved teams in the Marketplace (ADR-0051 amendment "Teams in the
// Marketplace", issue 3137; shared 0286).
//
// A team listing is kind `team`, sourced from a saved team (shared 0233). A
// team is published only when every member agent is itself listed: the team
// version pins each member to the member listing's current version at publish
// time, so a member resolves as its listing, never as the source project's
// agent. The snapshot holds the team's definition (name, description, floor
// policy, lead position, members) and the union of the members'
// requirements. It carries no heartbeat enablement, no mandate and no
// credential: a hire rebuilds the team from the member snapshots and the
// bindings the person chooses.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// KindTeam is the listing kind of a saved team.
const KindTeam = "team"

// TeamMemberSource is one member of the saved team being published: the agent
// and the project that owns it (the team's own, or the public project).
type TeamMemberSource struct {
	Position      int
	ApplicationID int64
	ProjectID     int64
	Name          string
}

// TeamSource is the saved team being published.
type TeamSource struct {
	ProjectID   int64
	TeamID      int64
	Name        string
	Description string
	FloorPolicy string
	// LeadPosition is the lead member's position (1-based); 0 under addressed.
	LeadPosition    int
	Members         []TeamMemberSource
	PublicProjectID int64
}

// TeamMemberRef is one member as a team version stores it.
type TeamMemberRef struct {
	Position  int    `json:"position"`
	ListingID int64  `json:"listing_id"`
	VersionID int64  `json:"version_id"`
	Name      string `json:"name"`
	Digest    string `json:"digest"`
}

// TeamDefinition is the definition a team listing version stores.
type TeamDefinition struct {
	Kind         string          `json:"kind"`
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	FloorPolicy  string          `json:"floor_policy"`
	LeadPosition int             `json:"lead_position"`
	Members      []TeamMemberRef `json:"members"`
}

// UnlistedMember names a member a team publish refused: it has no listed,
// hireable listing.
type UnlistedMember struct {
	Position int    `json:"position"`
	Name     string `json:"name"`
}

// ErrMembersUnlisted is a team publish whose members are not all listed.
type ErrMembersUnlisted struct {
	Members []UnlistedMember
}

func (e *ErrMembersUnlisted) Error() string {
	names := make([]string, 0, len(e.Members))
	for _, member := range e.Members {
		names = append(names, member.Name)
	}
	return "marketplace: team members are not listed: " + strings.Join(names, ", ")
}

// ParseTeamDefinition reads a team version's definition.
func ParseTeamDefinition(raw json.RawMessage) (TeamDefinition, error) {
	var def TeamDefinition
	if err := json.Unmarshal(raw, &def); err != nil {
		return TeamDefinition{}, fmt.Errorf("marketplace: read the team definition: %w", err)
	}
	if def.Kind != KindTeam || len(def.Members) == 0 {
		return TeamDefinition{}, fmt.Errorf("marketplace: the definition is not a team")
	}
	return def, nil
}

// memberListing finds the listed agent listing of a member and its current
// version: the newest that is neither yanked nor withdrawn.
func memberListing(ctx context.Context, db DB, member TeamMemberSource, publicProjectID int64) (StoredVersion, bool, error) {
	var listingID, versionID int64
	err := db.QueryRow(ctx, `
SELECT l.id, cv.version_id
FROM elitea_marketplace.listings l
JOIN LATERAL (
    SELECT v.version_id FROM elitea_marketplace.listing_versions v
    WHERE v.listing_id = l.id AND v.withdrawn_at IS NULL AND v.state <> 'yanked'
    ORDER BY v.published_at DESC, v.version_id DESC LIMIT 1
) AS cv ON true
WHERE l.kind = 'agent' AND l.state = 'listed'
  AND ((l.source_project_id = $1 AND l.source_entity_id = $2) OR ($1 = $3 AND l.entity_id = $2))
ORDER BY (l.source_project_id = $1 AND l.source_entity_id = $2) DESC, l.id
LIMIT 1`, member.ProjectID, member.ApplicationID, publicProjectID).Scan(&listingID, &versionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredVersion{}, false, nil
	}
	if err != nil {
		return StoredVersion{}, false, fmt.Errorf("marketplace: find the listing of team member %d: %w", member.Position, err)
	}
	version, err := LoadVersion(ctx, db, listingID, versionID)
	if err != nil {
		return StoredVersion{}, false, err
	}
	return version, true, nil
}

// BuildTeamSnapshot resolves every member to its listing and builds the
// snapshot of a team version. A member with no listed, hireable listing is
// *ErrMembersUnlisted, which names every such member.
func BuildTeamSnapshot(ctx context.Context, db DB, src TeamSource) (Snapshot, error) {
	def := TeamDefinition{
		Kind: KindTeam, Name: src.Name, Description: src.Description,
		FloorPolicy: src.FloorPolicy, LeadPosition: src.LeadPosition, Members: []TeamMemberRef{},
	}
	collect := newCollector()
	reqs := Requirements{ModelClass: ModelChat, Tier: Tiers[0]}
	var unlisted []UnlistedMember
	names := make([]string, 0, len(src.Members))
	for _, member := range src.Members {
		version, found, err := memberListing(ctx, db, member, src.PublicProjectID)
		if err != nil {
			return Snapshot{}, err
		}
		if !found {
			unlisted = append(unlisted, UnlistedMember{Position: member.Position, Name: member.Name})
			continue
		}
		def.Members = append(def.Members, TeamMemberRef{
			Position: member.Position, ListingID: version.ListingID, VersionID: version.VersionID,
			Name: version.ListingName, Digest: hex.EncodeToString(version.Digest[:]),
		})
		names = append(names, version.ListingName)
		mergeRequirements(collect, &reqs, version.Requirements)
	}
	if len(unlisted) > 0 {
		return Snapshot{}, &ErrMembersUnlisted{Members: unlisted}
	}
	reqs.Items = collect.list()
	definition, err := json.Marshal(def)
	if err != nil {
		return Snapshot{}, fmt.Errorf("marketplace: encode the team definition: %w", err)
	}
	digest, err := DigestOf(definition, reqs)
	if err != nil {
		return Snapshot{}, fmt.Errorf("marketplace: digest the team definition: %w", err)
	}
	return Snapshot{
		Kind: KindTeam, Name: src.Name, Description: src.Description, Tags: []string{},
		SoulSummary: strings.Join(names, " "), Tier: reqs.Tier,
		Definition: definition, Requirements: reqs, Digest: digest,
	}, nil
}

// mergeRequirements adds one member's requirements to the team's: the union of
// the items, the highest tier, tool calling when any member needs it, and the
// write flag when any member has one.
func mergeRequirements(collect *collector, into *Requirements, member Requirements) {
	for _, item := range member.Items {
		key := collect.add(item.Kind, item.Type, item.Name, item.Tools)
		if item.HasWriteTools {
			collect.items[key].HasWriteTools = true
		}
	}
	if TierRank(member.Tier) > TierRank(into.Tier) {
		into.Tier = member.Tier
	}
	if member.ModelClass == ModelToolCalling {
		into.ModelClass = ModelToolCalling
	}
	if member.HasWriteTools {
		into.HasWriteTools = true
	}
}

// NextTeamVersionID is the number the next version of a team's listing takes:
// one above its highest, or 1. The caller holds the team's publish lock.
func NextTeamVersionID(ctx context.Context, db DB, sourceProjectID, teamID int64) (int64, error) {
	var next int64
	if err := db.QueryRow(ctx, `
SELECT COALESCE(max(v.version_id), 0) + 1
FROM elitea_marketplace.listings l JOIN elitea_marketplace.listing_versions v ON v.listing_id = l.id
WHERE l.kind = 'team' AND l.source_project_id = $1 AND l.source_entity_id = $2`, sourceProjectID, teamID).Scan(&next); err != nil {
		return 0, fmt.Errorf("marketplace: number the team version: %w", err)
	}
	return next, nil
}

// TeamMemberJSON is one member of a team listing as its page shows it.
type TeamMemberJSON struct {
	Position    int     `json:"position"`
	ListingID   string  `json:"listing_id"`
	VersionID   string  `json:"version_id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	IconURL     *string `json:"icon_url"`
	VersionName string  `json:"version_name"`
	// Hireable: the member's listing is listed and its pinned version neither
	// yanked nor withdrawn. A team with an unhireable member cannot be hired.
	Hireable     bool         `json:"hireable"`
	Requirements Requirements `json:"requirements"`
	// HiredApplicationID is the project's installed agent of the same listing
	// version, which a team install reuses instead of hiring again; null when
	// there is none.
	HiredApplicationID *string `json:"hired_application_id"`
}

// TeamDetailJSON is the team part of a team listing's page: the current
// version's definition with its members resolved as their listings.
type TeamDetailJSON struct {
	VersionID    string           `json:"version_id"`
	FloorPolicy  string           `json:"floor_policy"`
	LeadPosition int              `json:"lead_position"`
	Members      []TeamMemberJSON `json:"members"`
}

// ReusableInstall is the project's live installed agent of a listing version:
// what a team install reuses. found is false when the project has none.
func ReusableInstall(ctx context.Context, db DB, projectID, listingID, versionID int64) (int64, bool, error) {
	var entityID int64
	err := db.QueryRow(ctx, `
SELECT entity_id FROM elitea_marketplace.installs
WHERE project_id = $1 AND entity_kind = 'agent' AND listing_id = $2 AND version_id = $3
  AND mode = 'install' AND dismissed_at IS NULL
ORDER BY created_at, entity_id LIMIT 1`, projectID, listingID, versionID).Scan(&entityID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("marketplace: find a reusable hire: %w", err)
	}
	return entityID, true, nil
}

func fillTeamDetail(ctx context.Context, db DB, listingID, projectID int64, detail *Detail) error {
	var definition []byte
	var versionID int64
	err := db.QueryRow(ctx, `
SELECT version_id, definition FROM elitea_marketplace.listing_versions
WHERE listing_id = $1 AND withdrawn_at IS NULL AND state <> 'yanked'
ORDER BY published_at DESC, version_id DESC LIMIT 1`, listingID).Scan(&versionID, &definition)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("marketplace: read the team definition: %w", err)
	}
	def, err := ParseTeamDefinition(definition)
	if err != nil {
		return err
	}
	team := &TeamDetailJSON{
		VersionID: strconv.FormatInt(versionID, 10), FloorPolicy: def.FloorPolicy,
		LeadPosition: def.LeadPosition, Members: make([]TeamMemberJSON, 0, len(def.Members)),
	}
	members := append([]TeamMemberRef(nil), def.Members...)
	sort.SliceStable(members, func(i, j int) bool { return members[i].Position < members[j].Position })
	for _, ref := range members {
		member, err := teamMemberJSON(ctx, db, ref, projectID)
		if err != nil {
			return err
		}
		team.Members = append(team.Members, member)
	}
	detail.Team = team
	return nil
}

func teamMemberJSON(ctx context.Context, db DB, ref TeamMemberRef, projectID int64) (TeamMemberJSON, error) {
	member := TeamMemberJSON{
		Position: ref.Position, ListingID: strconv.FormatInt(ref.ListingID, 10),
		VersionID: strconv.FormatInt(ref.VersionID, 10), Name: ref.Name,
		Requirements: Requirements{Items: []Requirement{}},
	}
	version, err := LoadVersion(ctx, db, ref.ListingID, ref.VersionID)
	if errors.Is(err, ErrNotFound) {
		// The snapshot was purged: the member shows by its stored name and the
		// team cannot be hired.
		return member, nil
	}
	if err != nil {
		return TeamMemberJSON{}, err
	}
	member.Name = version.ListingName
	member.VersionName = version.Name
	member.Hireable = version.Hireable() && version.ListingKind == KindAgent
	member.Requirements = version.Requirements
	if member.Requirements.Items == nil {
		member.Requirements.Items = []Requirement{}
	}
	if err := db.QueryRow(ctx, `SELECT description, icon FROM elitea_marketplace.listings WHERE id = $1`, ref.ListingID).
		Scan(&member.Description, &member.IconURL); err != nil {
		return TeamMemberJSON{}, fmt.Errorf("marketplace: read a team member's listing: %w", err)
	}
	if projectID > 0 {
		hired, found, err := ReusableInstall(ctx, db, projectID, ref.ListingID, ref.VersionID)
		if err != nil {
			return TeamMemberJSON{}, err
		}
		if found {
			id := strconv.FormatInt(hired, 10)
			member.HiredApplicationID = &id
		}
	}
	return member, nil
}
