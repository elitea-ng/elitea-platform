package marketplace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentrelease"
)

// Upgrade policies of an installed agent (ADR-0051 decision 4).
const (
	PolicyPinned = "pinned"
	PolicyNotify = "notify"
	PolicyAuto   = "auto"
)

// ValidPolicy reports whether policy is one of the three.
func ValidPolicy(policy string) bool {
	return policy == PolicyPinned || policy == PolicyNotify || policy == PolicyAuto
}

// The kinds of widening: the changes that always need a person, whatever the
// upgrade policy (the "permission change on update" rule of app stores).
const (
	WidenTool        = "tool_added"
	WidenCapability  = "capability_added"
	WidenRequirement = "requirement_added"
	WidenTier        = "tier_raised"
)

// Widening is one way a target version holds more than the pinned one.
type Widening struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// WideningOf lists what next adds to prev: a requirement that prev did not
// have, a tool of a requirement both have, a capability (write tools, a model
// class that calls tools) or a higher risk tier. An empty list means next asks
// for nothing more than prev did. Removing a requirement or a tool, or lowering
// the tier, is not a widening.
func WideningOf(prev, next Requirements) []Widening {
	out := []Widening{}
	before := make(map[string]Requirement, len(prev.Items))
	for _, item := range prev.Items {
		before[item.Key] = item
	}
	for _, item := range next.Items {
		old, had := before[item.Key]
		if !had {
			out = append(out, Widening{Kind: WidenRequirement, Detail: describeRequirement(item)})
			continue
		}
		seen := make(map[string]bool, len(old.Tools))
		for _, tool := range old.Tools {
			seen[tool] = true
		}
		var added []string
		for _, tool := range item.Tools {
			if !seen[tool] {
				added = append(added, tool)
			}
		}
		sort.Strings(added)
		for _, tool := range added {
			out = append(out, Widening{Kind: WidenTool, Detail: describeRequirement(item) + ": " + tool})
		}
	}
	if next.HasWriteTools && !prev.HasWriteTools {
		out = append(out, Widening{Kind: WidenCapability, Detail: "write tools"})
	}
	if next.ModelClass == ModelToolCalling && prev.ModelClass != ModelToolCalling {
		out = append(out, Widening{Kind: WidenCapability, Detail: "tool calling model"})
	}
	if tierRankOrLow(next.Tier) > tierRankOrLow(prev.Tier) {
		out = append(out, Widening{Kind: WidenTier, Detail: tierOrLow(prev.Tier) + " to " + tierOrLow(next.Tier)})
	}
	return out
}

func describeRequirement(item Requirement) string {
	switch {
	case item.Type != "" && item.Name != "" && item.Name != item.Type:
		return item.Kind + " " + item.Type + " (" + item.Name + ")"
	case item.Type != "":
		return item.Kind + " " + item.Type
	}
	return item.Kind + " " + item.Name
}

func tierRankOrLow(tier string) int {
	if rank := TierRank(tier); rank >= 0 {
		return rank
	}
	return 0
}

func tierOrLow(tier string) string {
	if TierRank(tier) >= 0 {
		return tier
	}
	return "low"
}

// AutoUpgradeAllowed is the rule of the `auto` policy. An installed agent
// takes a new version by itself only when its own tier is low, the target is
// low, and the target is not wider than the pinned version in any way
// (WideningOf). A widening, or any tier above low, always needs a person. The
// second answer names what blocked it, for the card that tells the person.
func AutoUpgradeAllowed(agentTier string, prev, next Requirements) (bool, []Widening) {
	wider := WideningOf(prev, next)
	if len(wider) > 0 {
		return false, wider
	}
	if tierRankOrLow(agentTier) > 0 || tierRankOrLow(next.Tier) > 0 {
		return false, []Widening{{Kind: WidenTier, Detail: "above low tier"}}
	}
	return true, nil
}

// NeedsApproval is the rule that sends an upgrade, like a hire, through a
// change request that a person approves: the target is above low tier, the
// agent's own tier is, or the target holds a write tool.
func NeedsApproval(agentTier string, next Requirements) bool {
	return tierRankOrLow(agentTier) > 0 || tierRankOrLow(next.Tier) > 0 || next.HasWriteTools
}

// MaxTier is the higher of two tiers; an unknown tier reads as low.
func MaxTier(a, b string) string {
	if tierRankOrLow(b) > tierRankOrLow(a) {
		return tierOrLow(b)
	}
	return tierOrLow(a)
}

// Changes is the diff between two listing versions, in the form a release diff
// has (agentrelease.Diff): the definition field by field, and the requirements.
// The embedded agents compare as part of the definition.
func Changes(prev, next StoredVersion) ([]agentrelease.Change, error) {
	before, err := diffView(prev)
	if err != nil {
		return nil, err
	}
	after, err := diffView(next)
	if err != nil {
		return nil, err
	}
	return agentrelease.Diff(before, after), nil
}

func diffView(version StoredVersion) (map[string]any, error) {
	var definition map[string]any
	if err := json.Unmarshal(version.Definition, &definition); err != nil {
		return nil, fmt.Errorf("marketplace: read the definition for the diff: %w", err)
	}
	reqs, err := json.Marshal(version.Requirements)
	if err != nil {
		return nil, fmt.Errorf("marketplace: encode the requirements for the diff: %w", err)
	}
	var requirements map[string]any
	if err := json.Unmarshal(reqs, &requirements); err != nil {
		return nil, fmt.Errorf("marketplace: read the requirements for the diff: %w", err)
	}
	return map[string]any{"definition": definition, "requirements": requirements}, nil
}

// Held is the install record of one agent: what a project holds of a listing.
type Held struct {
	ProjectID    int64
	EntityID     int64
	ListingID    int64
	VersionID    int64
	Digest       [32]byte
	Mode         string
	Bindings     []Binding
	ToolsEnabled bool
	Policy       string
}

// LoadHeld reads the install record of an agent. lock takes the row lock.
// It answers ErrNotFound for an agent that was not hired from the Marketplace.
func LoadHeld(ctx context.Context, db DB, projectID, applicationID int64, lock bool) (Held, error) {
	sql := `SELECT listing_id, version_id, digest, mode, bindings, tools_enabled, upgrade_policy
FROM elitea_marketplace.installs WHERE project_id = $1 AND entity_kind = 'agent' AND entity_id = $2`
	if lock {
		sql += ` FOR UPDATE`
	}
	held := Held{ProjectID: projectID, EntityID: applicationID}
	var digest, raw []byte
	err := db.QueryRow(ctx, sql, projectID, applicationID).
		Scan(&held.ListingID, &held.VersionID, &digest, &held.Mode, &raw, &held.ToolsEnabled, &held.Policy)
	if errors.Is(err, pgx.ErrNoRows) {
		return Held{}, ErrNotFound
	}
	if err != nil {
		return Held{}, fmt.Errorf("marketplace: read the install: %w", err)
	}
	copy(held.Digest[:], digest)
	_ = json.Unmarshal(raw, &held.Bindings) // a malformed column reads as no bindings
	return held, nil
}

// SetPolicy sets the upgrade policy of an installed agent. It answers false
// for an agent that is not an install (a fork has no policy).
func SetPolicy(ctx context.Context, db DB, projectID, applicationID int64, policy string) (bool, error) {
	if !ValidPolicy(policy) {
		return false, ErrInvalidRequest
	}
	tag, err := db.Exec(ctx, `
UPDATE elitea_marketplace.installs SET upgrade_policy = $3, updated_at = clock_timestamp()
WHERE project_id = $1 AND entity_kind = 'agent' AND entity_id = $2 AND mode = 'install'`, projectID, applicationID, policy)
	if err != nil {
		return false, fmt.Errorf("marketplace: set the upgrade policy: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// NewerVersion answers the newest hireable version of the listing that is
// newer than versionID, or ErrNotFound when the install is up to date.
func NewerVersion(ctx context.Context, db DB, listingID, versionID int64) (StoredVersion, error) {
	var newest int64
	err := db.QueryRow(ctx, `
SELECT cv.version_id FROM elitea_marketplace.listing_versions cv
JOIN elitea_marketplace.listings l ON l.id = cv.listing_id
WHERE cv.listing_id = $1 AND cv.withdrawn_at IS NULL AND cv.state <> 'yanked' AND l.state = 'listed'
  AND (cv.published_at, cv.version_id) > (SELECT pv.published_at, pv.version_id FROM elitea_marketplace.listing_versions pv
                                          WHERE pv.listing_id = $1 AND pv.version_id = $2)
ORDER BY cv.published_at DESC, cv.version_id DESC LIMIT 1`, listingID, versionID).Scan(&newest)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredVersion{}, ErrNotFound
	}
	if err != nil {
		return StoredVersion{}, fmt.Errorf("marketplace: find the newer version: %w", err)
	}
	return LoadVersion(ctx, db, listingID, newest)
}

// UpgradePayload is what an upgrade change request carries for its approval.
type UpgradePayload struct {
	Bindings []Binding `json:"bindings"`
}

// MergePayload is what an upstream merge change request carries: the draft the
// merge wrote and how each field was resolved.
type MergePayload struct {
	DraftVersionID int64             `json:"draft_version_id"`
	Resolutions    map[string]string `json:"resolutions"`
}

// Holder is a project agent that holds a listing version, installed or forked.
type Holder struct {
	ProjectID   int64
	EntityID    int64
	InstalledBy int64
	Mode        string
}

// HoldersOf lists the installs and forks that hold a version.
func HoldersOf(ctx context.Context, db DB, listingID, versionID int64) ([]Holder, error) {
	rows, err := db.Query(ctx, `
SELECT project_id, entity_id, installed_by, mode FROM elitea_marketplace.installs
WHERE listing_id = $1 AND version_id = $2 AND entity_kind = 'agent' ORDER BY project_id, entity_id`, listingID, versionID)
	if err != nil {
		return nil, fmt.Errorf("marketplace: list the holders: %w", err)
	}
	defer rows.Close()
	var out []Holder
	for rows.Next() {
		var ref Holder
		if err := rows.Scan(&ref.ProjectID, &ref.EntityID, &ref.InstalledBy, &ref.Mode); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// IDText formats an id for a JSON body.
func IDText(id int64) string { return strconv.FormatInt(id, 10) }
