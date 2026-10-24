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
	if len(request.ProjectSkillIDs) == 0 {
		return nil, nil
	}
	if len(request.ProjectSkillIDs) > currentMaxProjectSkillPicks {
		return nil, ErrProjectSkillsTooMany
	}
	if service.projectSkills == nil {
		return nil, ErrProjectSkillUnknown
	}
	skills, err := service.projectSkills.ResolveCurrentProjectSkills(ctx, request.ProjectID, request.ProjectSkillIDs)
	if err != nil {
		return nil, err
	}
	if len(skills) != len(request.ProjectSkillIDs) {
		return nil, ErrProjectSkillUnknown
	}
	scope := fmt.Sprintf("project:%d", request.ProjectID)
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
		snapshots = append(snapshots, map[string]any{
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
		})
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
