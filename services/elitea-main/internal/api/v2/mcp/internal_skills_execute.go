package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	applicationskillsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/applicationskills"
	skillsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

type internalSkillExecutor interface {
	Execute(
		context.Context,
		int64,
		int64,
		internalSkillOperation,
		map[string]any,
	) (internalApplicationExecution, error)
}

type repositoryInternalSkillExecutor struct {
	repo     skillsapi.Repository
	attached applicationskillsapi.CurrentApplicationSkillsReader
}

var internalSkillNamePattern = regexp.MustCompile(`^[a-z0-9]$|^[a-z0-9][a-z0-9-]*[a-z0-9]$`)

func newPostgresInternalSkillExecutor(pool *pgxpool.Pool) internalSkillExecutor {
	if pool == nil {
		return nil
	}
	attached, err := applicationskillsapi.NewCurrentApplicationSkillsRepository(pool)
	if err != nil {
		return nil
	}
	return &repositoryInternalSkillExecutor{repo: repos.NewSkillsRepo(pool), attached: attached}
}

func (executor *repositoryInternalSkillExecutor) Execute(
	ctx context.Context,
	projectID int64,
	actorID int64,
	operation internalSkillOperation,
	arguments map[string]any,
) (internalApplicationExecution, error) {
	if executor == nil || executor.repo == nil {
		return internalApplicationExecution{}, errors.New("internal skill executor is unavailable")
	}
	if ctx == nil || projectID <= 0 || actorID <= 0 {
		return internalApplicationExecution{}, errors.New("internal skill request is invalid")
	}
	if err := ctx.Err(); err != nil {
		return internalApplicationExecution{}, err
	}

	project := strconv.FormatInt(projectID, 10)
	switch operation {
	case internalListSkills:
		return executor.list(ctx, project, arguments)
	case internalCreateSkill:
		return executor.create(ctx, project, arguments)
	case internalGetSkill:
		return executor.get(ctx, project, arguments)
	case internalUpdateSkill:
		return executor.update(ctx, project, arguments)
	case internalUpdateSkillRelation:
		return executor.updateRelation(ctx, project, arguments)
	case internalListAttachedSkills:
		return executor.listAttached(ctx, project, arguments)
	default:
		return internalApplicationExecution{}, errors.New("unknown internal skill operation")
	}
}

func (executor *repositoryInternalSkillExecutor) list(
	ctx context.Context,
	projectID string,
	arguments map[string]any,
) (internalApplicationExecution, error) {
	page, err := boundedIntegerArgument(arguments, "page", 1, 1, 1<<31-1)
	if err != nil {
		return internalSkillBadRequest(err.Error())
	}
	pageSize, err := boundedIntegerArgument(arguments, "page_size", 10, 1, 100)
	if err != nil {
		return internalSkillBadRequest(err.Error())
	}
	sortBy := stringArgument(arguments["sort_by"])
	if sortBy != "" && sortBy != "created_at" && sortBy != "name" {
		return internalSkillBadRequest("sort_by must be created_at or name")
	}
	sortOrder := stringArgument(arguments["sort_order"])
	if sortOrder != "" && sortOrder != "asc" && sortOrder != "desc" {
		return internalSkillBadRequest("sort_order must be asc or desc")
	}

	result, err := executor.repo.List(ctx, projectID, skillsapi.ListParams{
		Page:      page,
		PageSize:  pageSize,
		Query:     strings.TrimSpace(stringArgument(arguments["query"])),
		SortBy:    sortBy,
		SortOrder: sortOrder,
	})
	if err != nil {
		return internalSkillRepositoryError(err)
	}
	rows := make([]map[string]any, 0, len(result.Items))
	for _, skill := range result.Items {
		row, mapErr := internalSkillMap(skill)
		if mapErr != nil {
			return internalApplicationExecution{}, mapErr
		}
		rows = append(rows, row)
	}
	return jsonExecution(http.StatusOK, map[string]any{
		"rows":  rows,
		"total": result.Total,
	})
}

func (executor *repositoryInternalSkillExecutor) create(
	ctx context.Context,
	projectID string,
	arguments map[string]any,
) (internalApplicationExecution, error) {
	name, err := requiredInternalSkillName(arguments["name"])
	if err != nil {
		return internalSkillBadRequest(err.Error())
	}
	description, err := boundedRequiredString(arguments, "description", 2304)
	if err != nil {
		return internalSkillBadRequest(err.Error())
	}
	version, err := createSkillVersion(arguments["versions"])
	if err != nil {
		return internalSkillBadRequest(err.Error())
	}

	created, err := executor.repo.Create(ctx, projectID, skillsapi.Skill{
		Name:         name,
		Description:  description,
		Instructions: version.instructions,
		Tags:         version.tags,
	})
	if err != nil {
		return internalSkillRepositoryError(err)
	}
	return internalSkillDetail(http.StatusCreated, created)
}

func (executor *repositoryInternalSkillExecutor) get(
	ctx context.Context,
	projectID string,
	arguments map[string]any,
) (internalApplicationExecution, error) {
	skillID, err := requiredPositiveID(arguments, "skill_id")
	if err != nil {
		return internalSkillBadRequest(err.Error())
	}
	skill, err := executor.repo.Get(ctx, projectID, skillID)
	if err != nil {
		return internalSkillRepositoryError(err)
	}
	return internalSkillDetail(http.StatusOK, skill)
}

func (executor *repositoryInternalSkillExecutor) update(
	ctx context.Context,
	projectID string,
	arguments map[string]any,
) (internalApplicationExecution, error) {
	skillID, err := requiredPositiveID(arguments, "skill_id")
	if err != nil {
		return internalSkillBadRequest(err.Error())
	}
	current, err := executor.repo.Get(ctx, projectID, skillID)
	if err != nil {
		return internalSkillRepositoryError(err)
	}

	changed := false
	if raw, present := arguments["name"]; present {
		current.Name, err = requiredInternalSkillName(raw)
		if err != nil {
			return internalSkillBadRequest(err.Error())
		}
		changed = true
	}
	if raw, present := arguments["description"]; present {
		current.Description, err = boundedRequiredString(
			map[string]any{"description": raw}, "description", 2304)
		if err != nil {
			return internalSkillBadRequest(err.Error())
		}
		changed = true
	}
	if raw, present := arguments["version"]; present {
		version, ok := raw.(map[string]any)
		if !ok {
			return internalSkillBadRequest("version must be an object")
		}
		if versionID, present := version["id"]; present {
			requestedID, idErr := positiveIDValue(versionID, "version.id")
			if idErr != nil {
				return internalSkillBadRequest(idErr.Error())
			}
			if current.VersionDetails == nil || requestedID != current.VersionDetails.ID {
				return internalSkillNotFound("skill version not found")
			}
		}
		if name, present := version["name"]; present && stringArgument(name) != "base" {
			return internalSkillBadRequest("version.name must be base")
		}
		if instructions, present := version["instructions"]; present {
			current.Instructions, err = boundedRequiredString(
				map[string]any{"instructions": instructions}, "instructions", 5000)
			if err != nil {
				return internalSkillBadRequest(err.Error())
			}
			changed = true
		}
		if tags, present := version["tags"]; present {
			current.Tags, err = skillTagNames(tags)
			if err != nil {
				return internalSkillBadRequest(err.Error())
			}
			changed = true
		}
	}
	if !changed {
		return internalSkillBadRequest("the skill update has no supported changes")
	}

	updated, err := executor.repo.Update(ctx, projectID, skillID, current)
	if err != nil {
		return internalSkillRepositoryError(err)
	}
	return internalSkillDetail(http.StatusOK, updated)
}

func (executor *repositoryInternalSkillExecutor) updateRelation(
	ctx context.Context,
	projectID string,
	arguments map[string]any,
) (internalApplicationExecution, error) {
	skillID, err := requiredPositiveID(arguments, "skill_id")
	if err != nil {
		return internalSkillBadRequest(err.Error())
	}
	entityVersionID, err := requiredPositiveID(arguments, "entity_version_id")
	if err != nil {
		return internalSkillBadRequest(err.Error())
	}
	hasRelation, ok := arguments["has_relation"].(bool)
	if !ok {
		return internalSkillBadRequest("has_relation must be true or false")
	}
	entityType := stringArgument(arguments["entity_type"])
	if entityType == "" {
		entityType = skillsapi.SkillEntityTypeAgent
	}
	if entityType != skillsapi.SkillEntityTypeAgent {
		return internalSkillBadRequest("entity_type must be agent")
	}
	relation := skillsapi.SkillRelation{
		EntityVersionID: entityVersionID,
		EntityType:      entityType,
	}
	if !hasRelation {
		if err := executor.repo.DetachSkill(ctx, projectID, skillID, relation); err != nil {
			return internalSkillRepositoryError(err)
		}
		return jsonExecution(http.StatusOK, map[string]any{"ok": true})
	}

	relation.SkillVersionID, err = requiredPositiveID(arguments, "skill_version_id")
	if err != nil {
		return internalSkillBadRequest("skill_version_id is required when has_relation is true")
	}
	attachment, err := executor.repo.AttachSkill(ctx, projectID, skillID, relation)
	if err != nil {
		return internalSkillRepositoryError(err)
	}
	return jsonExecution(http.StatusCreated, attachment)
}

func (executor *repositoryInternalSkillExecutor) listAttached(
	ctx context.Context,
	projectID string,
	arguments map[string]any,
) (internalApplicationExecution, error) {
	versionID, err := requiredPositiveID(arguments, "app_version_id")
	if err != nil {
		return internalSkillBadRequest(err.Error())
	}
	project, projectErr := strconv.ParseInt(projectID, 10, 32)
	version, versionErr := strconv.ParseInt(versionID, 10, 32)
	if projectErr != nil || versionErr != nil {
		return internalSkillBadRequest("project and application version IDs must fit PostgreSQL integer keys")
	}
	if executor.attached == nil {
		return internalApplicationExecution{}, errors.New("attached skill reader is unavailable")
	}
	result, err := executor.attached.ListCurrentApplicationSkills(ctx, int32(project), int32(version))
	if err != nil {
		return internalSkillRepositoryError(err)
	}
	attached := make([]map[string]any, 0, len(result))
	for _, skill := range result {
		item := map[string]any{
			"name":        skill.Name,
			"description": skill.Description,
			"skill_id":    strconv.FormatInt(int64(skill.SkillID), 10),
		}
		if skill.VersionID != nil && !skill.VersionMissing {
			item["version_id"] = strconv.FormatInt(int64(*skill.VersionID), 10)
			item["version_name"] = skill.VersionName
			item["instructions"] = skill.Instructions
		}
		attached = append(attached, item)
	}
	return jsonExecution(http.StatusOK, map[string]any{
		"skills":     attached,
		"max_skills": skillsapi.MaxSkillsPerEntityVersion,
	})
}

type parsedSkillVersion struct {
	instructions string
	tags         []string
}

func createSkillVersion(raw any) (parsedSkillVersion, error) {
	versions, ok := raw.([]any)
	if !ok || len(versions) != 1 {
		return parsedSkillVersion{}, errors.New("versions must contain exactly one base version")
	}
	version, ok := versions[0].(map[string]any)
	if !ok || stringArgument(version["name"]) != "base" {
		return parsedSkillVersion{}, errors.New("the initial skill version must be named base")
	}
	instructions, err := boundedRequiredString(version, "instructions", 5000)
	if err != nil {
		return parsedSkillVersion{}, err
	}
	tags, err := skillTagNames(version["tags"])
	if err != nil {
		return parsedSkillVersion{}, err
	}
	return parsedSkillVersion{instructions: instructions, tags: tags}, nil
}

func skillTagNames(raw any) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	entries, ok := raw.([]any)
	if !ok {
		return nil, errors.New("tags must be an array")
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		tag, ok := entry.(map[string]any)
		if !ok {
			return nil, errors.New("each tag must be an object with a name")
		}
		name, err := boundedRequiredString(tag, "name", 128)
		if err != nil {
			return nil, errors.New("each tag must have a valid name")
		}
		names = append(names, name)
	}
	return names, nil
}

func boundedRequiredString(arguments map[string]any, name string, maximum int) (string, error) {
	value, ok := arguments[name].(string)
	length := utf8.RuneCountInString(value)
	if !ok || strings.TrimSpace(value) == "" || length > maximum {
		return "", errors.New(name + " must contain between 1 and " + strconv.Itoa(maximum) + " characters")
	}
	return value, nil
}

func requiredInternalSkillName(raw any) (string, error) {
	name, ok := raw.(string)
	if !ok || utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 64 {
		return "", errors.New("name must contain between 1 and 64 characters")
	}
	if !internalSkillNamePattern.MatchString(name) {
		return "", errors.New("name must use lowercase letters, digits, or internal hyphens")
	}
	if strings.Contains(name, "claude") || strings.Contains(name, "anthropic") {
		return "", errors.New("name cannot contain claude or anthropic")
	}
	return name, nil
}

func boundedIntegerArgument(
	arguments map[string]any,
	name string,
	fallback int,
	minimum int,
	maximum int,
) (int, error) {
	raw, present := arguments[name]
	if !present {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(scalarArgument(raw), 10, 32)
	if err != nil || parsed < int64(minimum) || parsed > int64(maximum) {
		return 0, errors.New(name + " is outside its supported range")
	}
	return int(parsed), nil
}

func positiveIDValue(raw any, name string) (string, error) {
	return requiredPositiveID(map[string]any{name: raw}, name)
}

func internalSkillDetail(status int, skill skillsapi.Skill) (internalApplicationExecution, error) {
	body, err := internalSkillMap(skill)
	if err != nil {
		return internalApplicationExecution{}, err
	}
	if skill.VersionDetails != nil && skill.VersionDetails.ID != "" {
		body["default_version_id"] = skill.VersionDetails.ID
		body["version_id"] = skill.VersionDetails.ID
	}
	return jsonExecution(status, body)
}

func internalSkillMap(skill skillsapi.Skill) (map[string]any, error) {
	encoded, err := json.Marshal(skill)
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		return nil, err
	}
	if _, present := body["tags"]; present {
		body["tags"] = internalSkillTagObjects(skill.Tags)
	}
	if rawVersions, present := body["versions"].([]any); present {
		for index, rawVersion := range rawVersions {
			if version, ok := rawVersion.(map[string]any); ok {
				version["tags"] = internalSkillTagObjects(skill.Versions[index].Tags)
			}
		}
	}
	if details, present := body["version_details"].(map[string]any); present {
		details["tags"] = internalSkillTagObjects(skill.VersionDetails.Tags)
	}
	return body, nil
}

func internalSkillTagObjects(tags []string) []map[string]any {
	objects := make([]map[string]any, 0, len(tags))
	for _, name := range tags {
		objects = append(objects, map[string]any{"name": name})
	}
	return objects
}

func internalSkillRepositoryError(err error) (internalApplicationExecution, error) {
	var failure *apierr.APIError
	if errors.As(err, &failure) {
		return jsonExecution(failure.Status, map[string]any{"error": failure.Message})
	}
	return internalApplicationExecution{}, err
}

func internalSkillBadRequest(message string) (internalApplicationExecution, error) {
	return jsonExecution(http.StatusBadRequest, map[string]any{"error": message})
}

func internalSkillNotFound(message string) (internalApplicationExecution, error) {
	return jsonExecution(http.StatusNotFound, map[string]any{"error": message})
}
