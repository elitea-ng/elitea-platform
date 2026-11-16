package agentexecution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	// currentMaxProjectSkillBytes bounds the instructions of all invoked
	// skills together once a turn names a project skill. More is refused, not
	// cut: a cut skill would be applied half-way. It is the desktop host's
	// MAX_SKILL_BYTES, so both clients refuse the same turn.
	currentMaxProjectSkillBytes = 64 * 1024
	// currentMaxProjectSkillPicks bounds the picks one request may carry; with
	// the `~name` skills of the version it shares currentMaxInvokedSkills.
	currentMaxProjectSkillPicks = currentMaxInvokedSkills
)

var (
	// ErrProjectSkillUnknown refuses a project skill the caller cannot read:
	// it does not exist in the project, is hidden from the caller by folder
	// access, or has no base version with instructions. The three answer the
	// same on purpose, so a caller cannot probe for skills it may not see.
	ErrProjectSkillUnknown = errors.New("project skill is not available to the caller")
	// ErrProjectSkillsTooMany refuses a turn that would invoke more than five
	// skills, the version's `~name` skills and the project skills together.
	ErrProjectSkillsTooMany = errors.New("a turn invokes at most five skills")
	// ErrProjectSkillsTooLarge refuses a turn whose invoked skills carry more
	// than 64 KiB of instructions together.
	ErrProjectSkillsTooLarge = errors.New("the invoked skills' instructions exceed 64 KiB")
	// ErrProjectSkillNameConflict refuses a project skill that shares a name
	// with a different skill the agent version carries: the runtime's
	// catalogue holds one skill per name.
	ErrProjectSkillNameConflict = errors.New("a project skill shares its name with another skill of the agent")
)

// CurrentProjectSkill is one project skill as read for a turn: its `base`
// version, which is the only version a project skill is ever applied at.
type CurrentProjectSkill struct {
	SkillID       int64
	BaseVersionID int64
	Name          string
	Description   string
	Instructions  string
	IconMeta      json.RawMessage
	// Untrusted is true for a skill written in another project (its base
	// version records a parent project that is not this one) whose current
	// text this project has not trusted (ADR-0051 amendment 1, A1.6). The
	// worker then shows the text as quoted untrusted data, not as an
	// instruction. The resolver decides it; the request never does.
	Untrusted bool
	// PublisherProjectID is the project the skill was forked from, when
	// Untrusted. Zero otherwise.
	PublisherProjectID int64
}

// CurrentProjectSkillResolver reads the project skills a turn names by id,
// under the caller's own visibility. Implemented by
// internal/infra/db/repos.ProjectSkillsRepo. The ids are what the client
// sent; the content is read here and never taken from the request.
//
// It returns ErrProjectSkillUnknown for any id the caller cannot read, so one
// refused id refuses the turn rather than silently dropping the skill.
type CurrentProjectSkillResolver interface {
	ResolveCurrentProjectSkills(ctx context.Context, projectID int64, skillIDs []int64) ([]CurrentProjectSkill, error)
}

// CurrentListingSkill is a Marketplace skill listing version pinned to a
// conversation, read for a turn: its text comes from the listing version, and
// nothing about it was ever forked into the project. It is always untrusted
// (ADR-0051 amendment 1, A1.6): trusting needs a project skill, so the person
// adds the skill first.
type CurrentListingSkill struct {
	ListingID   int64
	VersionID   int64
	Name        string
	Description string
	// Instructions is the listing version's text.
	Instructions string
}

// ConversationSkills are the skills pinned to a conversation (ADR-0050
// amendment 1, A1.2), oldest pin first.
type ConversationSkills struct {
	// ProjectSkillIDs are the pinned project skills, read like a message's
	// picks, under the caller's visibility.
	ProjectSkillIDs []int64
	// Listing are the pinned listing versions, already read.
	Listing []CurrentListingSkill
}

// CurrentConversationSkillResolver reads what a conversation has pinned. It
// answers ErrProjectSkillUnknown for a pinned listing version that is yanked,
// withdrawn or delisted: the turn is refused with the answer a missing skill
// gets, not run without the skill.
type CurrentConversationSkillResolver interface {
	ResolveConversationSkills(ctx context.Context, projectID int64, conversationUUID string) (ConversationSkills, error)
}

// WithConversationSkills attaches the resolver, in the same idiom as
// WithProjectSkills. A service without it applies no pins.
func (service *CurrentApplicationStartService) WithConversationSkills(
	resolver CurrentConversationSkillResolver,
) *CurrentApplicationStartService {
	service.conversationSkills = resolver
	return service
}

// WithProjectSkills attaches the resolver, in the same after-construction
// idiom as WithMemories. A service without it refuses a request that names a
// project skill (ErrProjectSkillUnknown) instead of ignoring the pick.
func (service *CurrentApplicationStartService) WithProjectSkills(
	resolver CurrentProjectSkillResolver,
) *CurrentApplicationStartService {
	service.projectSkills = resolver
	return service
}

// resolveProjectSkills turns the request's picks into frozen skill snapshots
// of the same shape freezeCurrentInstructionSnapshots gives a version's own
// skills, so the runtime admits them by the same rules (id, digest revision,
// scope). Nothing about a skill but its id comes from the client.
func (service *CurrentApplicationStartService) resolveProjectSkills(
	ctx context.Context,
	request CurrentApplicationStartRequest,
) ([]map[string]any, error) {
<<<<<<< HEAD
	return service.resolveTurnSkills(ctx, request.ProjectID, request.ConversationUUID, request.ProjectSkillIDs)
}

// resolveTurnSkills freezes the skills of one turn: the conversation's pins
// first, then the message's picks (ADR-0050 amendment 1). A pin that is also
// picked is applied once. A pin is marked so that a name another skill of the
// answering agent already holds drops the PIN instead of refusing the turn: a
// pin must never make a conversation unusable for an agent it was not made for
// (mergeProjectSkills).
func (service *CurrentApplicationStartService) resolveTurnSkills(
	ctx context.Context,
	projectID int64,
	conversationUUID string,
	picks []int64,
) ([]map[string]any, error) {
	var pinned ConversationSkills
	if service.conversationSkills != nil && conversationUUID != "" {
		var err error
		if pinned, err = service.conversationSkills.ResolveConversationSkills(ctx, projectID, conversationUUID); err != nil {
			return nil, err
		}
	}
	if len(pinned.ProjectSkillIDs) == 0 && len(pinned.Listing) == 0 {
		return service.resolveProjectSkillIDs(ctx, projectID, picks)
	}
	if len(pinned.ProjectSkillIDs)+len(pinned.Listing)+len(picks) > currentMaxProjectSkillPicks {
		// A pick the pins already hold is not a sixth skill.
		extra := 0
		held := make(map[int64]struct{}, len(pinned.ProjectSkillIDs))
		for _, id := range pinned.ProjectSkillIDs {
			held[id] = struct{}{}
		}
		for _, id := range picks {
			if _, dup := held[id]; !dup {
				extra++
			}
		}
		if len(pinned.ProjectSkillIDs)+len(pinned.Listing)+extra > currentMaxProjectSkillPicks {
			return nil, ErrProjectSkillsTooMany
		}
	}
	ids := make([]int64, 0, len(pinned.ProjectSkillIDs)+len(picks))
	ids = append(ids, pinned.ProjectSkillIDs...)
	for _, id := range picks {
		if !containsSkillID(pinned.ProjectSkillIDs, id) {
			ids = append(ids, id)
		}
	}
	snapshots, err := service.resolveProjectSkillIDs(ctx, projectID, ids)
	if err != nil {
		return nil, err
	}
	result := make([]map[string]any, 0, len(snapshots)+len(pinned.Listing))
	for index, snapshot := range snapshots {
		if index < len(pinned.ProjectSkillIDs) {
			snapshot["pinned"] = true
		}
		result = append(result, snapshot)
	}
	// Pinned listing versions sit with the pins, ahead of the message's picks.
	listing := make([]map[string]any, 0, len(pinned.Listing))
	for _, skill := range pinned.Listing {
		snapshot, err := listingSkillSnapshot(skill)
		if err != nil {
			return nil, err
		}
		listing = append(listing, snapshot)
	}
	at := len(pinned.ProjectSkillIDs)
	return append(result[:at:at], append(listing, result[at:]...)...), nil
}

func containsSkillID(ids []int64, id int64) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

// listingSkillSnapshot freezes a pinned Marketplace listing version as a skill
// snapshot the runtime admits like any other: its id names the listing and the
// version, its scope says where it came from, and it is untrusted.
func listingSkillSnapshot(skill CurrentListingSkill) (map[string]any, error) {
	if skill.ListingID <= 0 || skill.VersionID <= 0 || strings.TrimSpace(skill.Name) == "" ||
		strings.TrimSpace(skill.Instructions) == "" {
		return nil, ErrProjectSkillUnknown
	}
	return map[string]any{
		"skill_id":         nil,
		"skill_version_id": nil,
		"name":             skill.Name,
		"version_name":     "listing",
		"description":      skill.Description,
		"icon_meta":        nil,
		"instructions":     skill.Instructions,
		"id":               fmt.Sprintf("listing:%d:version:%d", skill.ListingID, skill.VersionID),
		"revision":         currentInstructionRevision(skill.Instructions),
		"scope":            fmt.Sprintf("marketplace:%d:%d", skill.ListingID, skill.VersionID),
		"trust":            "untrusted",
		"pinned":           true,
	}, nil
}

// resolveProjectSkillIDs is resolveProjectSkills for the ids of either start
// contract: an agent turn and a model chat read and freeze a pick the same
// way, so the runtime admits both by the same rules.
func (service *CurrentApplicationStartService) resolveProjectSkillIDs(
	ctx context.Context,
	projectID int64,
	skillIDs []int64,
) ([]map[string]any, error) {
	if len(skillIDs) == 0 {
||||||| fa5dbac00
	if len(request.ProjectSkillIDs) == 0 {
=======
	return service.resolveProjectSkillIDs(ctx, request.ProjectID, request.ProjectSkillIDs)
}

// resolveProjectSkillIDs is resolveProjectSkills for the ids of either start
// contract: an agent turn and a model chat read and freeze a pick the same
// way, so the runtime admits both by the same rules.
func (service *CurrentApplicationStartService) resolveProjectSkillIDs(
	ctx context.Context,
	projectID int64,
	skillIDs []int64,
) ([]map[string]any, error) {
	if len(skillIDs) == 0 {
>>>>>>> origin/main
		return nil, nil
	}
	if len(skillIDs) > currentMaxProjectSkillPicks {
		return nil, ErrProjectSkillsTooMany
	}
	if service.projectSkills == nil {
		return nil, ErrProjectSkillUnknown
	}
	skills, err := service.projectSkills.ResolveCurrentProjectSkills(ctx, projectID, skillIDs)
	if err != nil {
		return nil, err
	}
	if len(skills) != len(skillIDs) {
		return nil, ErrProjectSkillUnknown
	}
	scope := fmt.Sprintf("project:%d", projectID)
	snapshots := make([]map[string]any, 0, len(skills))
	for _, skill := range skills {
		if skill.SkillID <= 0 || skill.BaseVersionID <= 0 || strings.TrimSpace(skill.Name) == "" ||
			strings.TrimSpace(skill.Instructions) == "" {
			return nil, ErrProjectSkillUnknown
		}
		var iconMeta any
		if len(skill.IconMeta) != 0 && json.Valid(skill.IconMeta) {
			_ = json.Unmarshal(skill.IconMeta, &iconMeta)
		}
		snapshot := map[string]any{
			"skill_id":         skill.SkillID,
			"skill_version_id": skill.BaseVersionID,
			"name":             skill.Name,
			"version_name":     "base",
			"description":      skill.Description,
			"icon_meta":        iconMeta,
			"instructions":     skill.Instructions,
			"id":               fmt.Sprintf("skill:%d:version:%d", skill.SkillID, skill.BaseVersionID),
			"revision":         currentInstructionRevision(skill.Instructions),
			"scope":            scope,
		}
		if skill.Untrusted {
			// Absent when trusted, so a trusted skill's snapshot and digest
			// are the ones every earlier turn froze.
			snapshot["trust"] = "untrusted"
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, nil
}

// mergeProjectSkills adds the picked project skills to the candidates the
// `~name` pass matches against, and answers the picks that still have to be
// invoked whatever the text says (a pick is an explicit choice, not a
// mention). A pick the version already carries under the same id is that
// attached skill; one sharing only a name with a different skill is refused.
func mergeProjectSkills(attached, picks []map[string]any) (candidates, forced []map[string]any, err error) {
	candidates = append(make([]map[string]any, 0, len(attached)+len(picks)), attached...)
	byName := make(map[string]map[string]any, len(attached))
	for _, skill := range attached {
		if name, ok := skill["name"].(string); ok {
			byName[strings.ToLower(name)] = skill
		}
	}
	seen := make(map[string]struct{}, len(picks))
	for _, pick := range picks {
		identity := currentSkillIdentity(pick["id"])
		if _, dup := seen[identity]; dup {
			continue
		}
		seen[identity] = struct{}{}
		name, _ := pick["name"].(string)
		if existing, ok := byName[strings.ToLower(name)]; ok {
			if currentSkillIdentity(existing["id"]) != identity {
				if pinned, _ := pick["pinned"].(bool); pinned {
					// A conversation pin never refuses a turn: the agent's own
					// skill (or an earlier pin) of that name stands.
					continue
				}
				return nil, nil, ErrProjectSkillNameConflict
			}
			forced = append(forced, existing)
			continue
		}
		byName[strings.ToLower(name)] = pick
		candidates = append(candidates, pick)
		forced = append(forced, pick)
	}
	return candidates, forced, nil
}

// withForcedSkills appends the forced skills the text did not already invoke,
// then holds the whole invoked set to the turn's bounds. The bounds are only
// enforced when a project skill is in play, so a turn with `~name` skills alone
// keeps its existing behaviour.
func withForcedSkills(invoked, forced []map[string]any) ([]map[string]any, error) {
	if len(forced) == 0 {
		return invoked, nil
	}
	have := make(map[string]struct{}, len(invoked))
	for _, skill := range invoked {
		have[currentSkillIdentity(skill["id"])] = struct{}{}
	}
	for _, skill := range forced {
		if _, ok := have[currentSkillIdentity(skill["id"])]; ok {
			continue
		}
		have[currentSkillIdentity(skill["id"])] = struct{}{}
		invoked = append(invoked, currentInvokedSkillProjection(skill))
	}
	if len(invoked) > currentMaxInvokedSkills {
		return nil, ErrProjectSkillsTooMany
	}
	total := 0
	for _, skill := range invoked {
		text, _ := skill["instructions"].(string)
		total += len(text)
	}
	if total > currentMaxProjectSkillBytes {
		return nil, ErrProjectSkillsTooLarge
	}
	return invoked, nil
}

// validProjectSkillIDs admits only positive 32-bit ids (the skills table's
// key), none repeated, at most the picks one turn may carry. A request over
// the count is answered ErrProjectSkillsTooMany by resolveProjectSkills; this
// guards the shape, so a malformed id never reaches a query.
func validProjectSkillIDs(ids []int64) bool {
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 || id > 1<<31-1 {
			return false
		}
		if _, dup := seen[id]; dup {
			return false
		}
		seen[id] = struct{}{}
	}
	return true
}
