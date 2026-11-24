package eliteacore

// The team import (issue 3010): a set of agents imported AS a saved team
// (ADR-0050 decision 8), in one transaction.
//
// The body is the import document the import route takes, plus the team:
//
//	{"team": {"name", "description", "floor_policy", "lead", "members": [import_uuid, ...]},
//	 "applications": [...], "toolkits": [...], "skills": [...]}
//
// `members` lists the agents of the team by the `import_uuid` of their entry,
// in roster order; `lead` is one of them. Every agent of the document is
// created, with its toolkits, skills and links, and then the team, or nothing
// is: the import route keeps whatever entity it could write and answers 207,
// which is the wrong answer for a team (half a team is not a team).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentteams"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// The refusal codes of the team import, beside the sentence in `error`.
const (
	teamImportInvalid      = "team_invalid"
	teamImportNameTaken    = "team_name_taken"
	teamImportAgentTaken   = "agent_name_taken"
	teamImportFailed       = "import_failed"
	teamImportMissingTools = "toolkit_missing"
)

// maxTeamImportBody bounds the body. A team is at most 16 agents, and the web
// reader stops a zip at 10 MiB of markdown; the JSON of that is well inside.
const maxTeamImportBody = 32 << 20

// importedAgentRef is what an import wrote for one agent entry: the team
// import pins the first version as the member's version.
type importedAgentRef struct {
	index     int
	appID     int
	versionID int
	name      string
}

// importOutcome is what runImport wrote and what it could not.
type importOutcome struct {
	resultAgents   []map[string]any
	errorAgents    []any
	resultToolkits []map[string]any
	errorToolkits  []any
	resultSkills   []map[string]any
	errorSkills    []any
	// agents maps an agent entry's import_uuid to what was written for it.
	agents map[string]importedAgentRef
}

func (o importOutcome) failed() bool {
	return len(o.errorAgents)+len(o.errorToolkits)+len(o.errorSkills) > 0
}

// status is the import route's answer: 400 if all failed, 207 if mixed, 201
// if all succeeded.
func (o importOutcome) status() int {
	if !o.failed() {
		return http.StatusCreated
	}
	if len(o.resultAgents)+len(o.resultToolkits)+len(o.resultSkills) == 0 {
		return http.StatusBadRequest
	}
	return 207
}

func (o importOutcome) envelope() map[string]any {
	return map[string]any{
		"result": importChannels(o.resultAgents, o.resultToolkits, o.resultSkills),
		"errors": importChannels(o.errorAgents, o.errorToolkits, o.errorSkills),
	}
}

// importEntitiesFrom reads an import body into its flat entity list. A body is
// a bare entity array (the legacy wizard), or an object whose `applications`
// array holds the entities. The object may also carry top-level `toolkits`
// and `skills` arrays, which is the shape this app's own export and the web
// Import send: they used to be ignored, so every imported agent with a
// toolkit answered 207 "unable to link tools" (issue 3010). An entry of
// those arrays without an `entity` key takes the array's. The object is
// returned for the caller's own keys. A non-empty refusal is the 400's text.
func importEntitiesFrom(body []byte) ([]any, map[string]any, string) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var entities []any
		if err := json.Unmarshal(trimmed, &entities); err != nil {
			return nil, nil, "invalid request body: " + err.Error()
		}
		return entities, nil, ""
	}
	var bodyMap map[string]any
	if err := json.Unmarshal(trimmed, &bodyMap); err != nil {
		return nil, nil, "invalid request body: " + err.Error()
	}
	// A body that carries no `applications` array names nothing to import.
	// It used to answer 201 with an empty result, which the wizard reads as
	// a completed import. An `applications` key that IS an array and IS
	// empty keeps that answer: it asks for nothing and gets nothing.
	apps, ok := bodyMap["applications"].([]any)
	if !ok {
		return nil, nil, "the request body must carry an applications array"
	}
	entities := append([]any{}, apps...)
	for _, channel := range []string{"toolkits", "skills"} {
		raw, present := bodyMap[channel]
		if !present || raw == nil {
			continue
		}
		list, isArray := raw.([]any)
		if !isArray {
			return nil, nil, "the request body's " + channel + " must be an array"
		}
		for _, item := range list {
			if entry, isObject := item.(map[string]any); isObject {
				if _, named := entry["entity"]; !named {
					entry["entity"] = channel
				}
			}
			entities = append(entities, item)
		}
	}
	return entities, bodyMap, ""
}

// teamImportBody is the `team` object of the body.
type teamImportBody struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	FloorPolicy string   `json:"floor_policy"`
	Lead        string   `json:"lead"`
	Members     []string `json:"members"`
}

// TeamImportPost is POST /elitea_core/team_import/prompt_lib/{projectID}.
func (h *Handler) TeamImportPost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.pool == nil {
		slog.ErrorContext(ctx, "team import: "+importWriteFailed)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": importWriteFailed})
		return
	}
	projectID := chi.URLParam(r, "projectID")
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	project, err := strconv.ParseInt(projectID, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid project id"})
		return
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, maxTeamImportBody+1))
	if err != nil || len(bodyBytes) > maxTeamImportBody {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unable to read the request body"})
		return
	}
	entities, bodyMap, refusal := importEntitiesFrom(bodyBytes)
	if refusal == "" && bodyMap == nil {
		refusal = "the request body must be an object with a team and an applications array"
	}
	if refusal != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": refusal})
		return
	}
	var body teamImportBody
	if raw, err := json.Marshal(bodyMap["team"]); err != nil || bodyMap["team"] == nil || json.Unmarshal(raw, &body) != nil {
		writeTeamImportRefusal(w, http.StatusUnprocessableEntity, teamImportInvalid, "the request body must carry a team object", nil)
		return
	}
	userID, ok := importPrincipalUserID(ctx)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "an authenticated principal is required to import"})
		return
	}

	agents := agentEntriesByUUID(entities)
	team, roster, refusalText := teamFromBody(body, agents)
	if refusalText != "" {
		writeTeamImportRefusal(w, http.StatusUnprocessableEntity, teamImportInvalid, refusalText, nil)
		return
	}
	if missing := missingToolkitReferences(entities); len(missing) > 0 {
		writeTeamImportRefusal(w, http.StatusUnprocessableEntity, teamImportMissingTools,
			"Some agents use toolkits the file does not carry: "+strings.Join(missing, "; ")+".",
			map[string]any{"missing": missing})
		return
	}
	if taken, err := h.takenAgentNames(ctx, s, agents); err != nil {
		slog.ErrorContext(ctx, "team import: read the project's agent names", "schema", s, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": importWriteFailed})
		return
	} else if len(taken) > 0 {
		writeTeamImportRefusal(w, http.StatusConflict, teamImportAgentTaken,
			"These agents already exist in this project: "+strings.Join(taken, ", ")+". Rename them in the file, or delete the existing agents, and import again.",
			map[string]any{"names": taken})
		return
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": importWriteFailed})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has run

	outcome := h.runImport(ctx, tx, s, projectID, userID, entities)
	if outcome.failed() {
		slog.WarnContext(ctx, "team import: refused, nothing kept", "schema", s, "team", team.Name)
		writeTeamImportRefusal(w, http.StatusUnprocessableEntity, teamImportFailed,
			"The team was not imported, and nothing of it was kept: "+firstImportError(outcome),
			map[string]any{"errors": withoutAbortedStatements(outcome).envelope()["errors"]})
		return
	}
	for index, uuid := range roster {
		written := outcome.agents[uuid]
		team.Members[index] = agentteams.Member{
			ApplicationID: int64(written.appID), ProjectID: project,
			VersionID: int64(written.versionID), Name: written.name,
		}
	}
	if err := team.Normalize(); err != nil {
		writeTeamImportRefusal(w, http.StatusUnprocessableEntity, teamImportInvalid, err.Error(), nil)
		return
	}
	teamID, err := repos.InsertTeam(ctx, tx, project, userID.Int64(), team)
	if errors.Is(err, agentteams.ErrNameTaken) {
		writeTeamImportRefusal(w, http.StatusConflict, teamImportNameTaken, err.Error(), nil)
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "team import: write the team", "schema", s, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": importWriteFailed})
		return
	}
	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "team import: commit", "schema", s, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": importWriteFailed})
		return
	}
	response := outcome.envelope()
	response["team"] = teamImportJSON(teamID, project, team)
	writeJSON(w, http.StatusCreated, response)
}

func writeTeamImportRefusal(w http.ResponseWriter, status int, code, sentence string, extra map[string]any) {
	body := map[string]any{"error": sentence, "code": code}
	for key, value := range extra {
		body[key] = value
	}
	writeJSON(w, status, body)
}

// agentEntriesByUUID returns the document's agent entries (everything that is
// not a toolkit or a skill, the rule runImport routes by) keyed by import_uuid.
func agentEntriesByUUID(entities []any) map[string]map[string]any {
	agents := map[string]map[string]any{}
	for _, raw := range entities {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if entity, _ := entry["entity"].(string); entity == "toolkits" || entity == "skills" {
			continue
		}
		if uuid, _ := entry["import_uuid"].(string); uuid != "" {
			agents[uuid] = entry
		}
	}
	return agents
}

// teamFromBody checks the team object against the document and answers the
// team (members filled in after the import) and its roster of import_uuids.
func teamFromBody(body teamImportBody, agents map[string]map[string]any) (agentteams.Team, []string, string) {
	team := agentteams.Team{Name: body.Name, Description: body.Description}
	if len(body.Members) == 0 {
		return team, nil, "the team names no members"
	}
	if len(body.Members) > agentteams.MaxMembers {
		return team, nil, fmt.Sprintf("a team has at most %d agents", agentteams.MaxMembers)
	}
	seen := map[string]bool{}
	for _, uuid := range body.Members {
		if _, found := agents[uuid]; !found {
			return team, nil, fmt.Sprintf("the team member %q is not an agent of the file", uuid)
		}
		if seen[uuid] {
			return team, nil, fmt.Sprintf("the team names the member %q twice", uuid)
		}
		seen[uuid] = true
	}
	team.Members = make([]agentteams.Member, len(body.Members))
	policy := strings.TrimSpace(body.FloorPolicy)
	switch {
	case body.Lead != "":
		position := 0
		for index, uuid := range body.Members {
			if uuid == body.Lead {
				position = index + 1
			}
		}
		if position == 0 {
			return team, nil, "the team's lead is not one of its members"
		}
		if policy != "" && policy != string(turntaking.PolicyLead) {
			return team, nil, "a team with a lead has floor_policy lead"
		}
		team.Policy, team.Lead = turntaking.PolicyLead, position
	case policy == "" || policy == string(turntaking.PolicyAddressed):
		team.Policy = turntaking.PolicyAddressed
	default:
		return team, nil, "floor_policy lead needs a lead"
	}
	// The name and the sizes are checked now, before anything is written; the
	// members are checked again once they have ids.
	probe := team
	probe.Members = make([]agentteams.Member, len(body.Members))
	for index := range probe.Members {
		probe.Members[index] = agentteams.Member{ApplicationID: int64(index + 1), ProjectID: 1, VersionID: 1}
	}
	if err := probe.Normalize(); err != nil {
		return team, nil, err.Error()
	}
	team.Name, team.Description = probe.Name, probe.Description
	return team, body.Members, ""
}

// missingToolkitReferences names every version tool reference whose
// import_uuid no toolkit entry of the document carries, as "agent: reference".
func missingToolkitReferences(entities []any) []string {
	carried := map[string]bool{}
	for _, raw := range entities {
		if entry, ok := raw.(map[string]any); ok {
			if entity, _ := entry["entity"].(string); entity == "toolkits" {
				if uuid, _ := entry["import_uuid"].(string); uuid != "" {
					carried[uuid] = true
				}
			}
		}
	}
	var missing []string
	for _, entry := range agentEntriesByUUID(entities) {
		name, _ := entry["name"].(string)
		versions, _ := entry["versions"].([]any)
		for _, rawVersion := range versions {
			version, _ := rawVersion.(map[string]any)
			tools, _ := version["tools"].([]any)
			for _, rawTool := range tools {
				tool, _ := rawTool.(map[string]any)
				uuid, _ := tool["import_uuid"].(string)
				if uuid != "" && !carried[uuid] {
					label := uuid
					if toolName, _ := tool["name"].(string); toolName != "" {
						label = toolName
					}
					missing = append(missing, name+": "+label)
				}
			}
		}
	}
	sort.Strings(missing)
	return missing
}

// takenAgentNames answers the names of the document's agents, as the file
// spells them, that an agent of the project already has (compared without
// case), and the names the file itself repeats.
func (h *Handler) takenAgentNames(ctx context.Context, s string, agents map[string]map[string]any) ([]string, error) {
	names := make([]string, 0, len(agents))
	spelled := map[string]string{}
	count := map[string]int{}
	for _, entry := range agents {
		name, _ := entry["name"].(string)
		name = strings.TrimSpace(name)
		key := strings.ToLower(name)
		names = append(names, key)
		spelled[key] = name
		count[key]++
	}
	taken := map[string]bool{}
	for key, n := range count {
		if n > 1 {
			taken[spelled[key]] = true
		}
	}
	rows, err := h.pool.Query(ctx, fmt.Sprintf(
		`SELECT DISTINCT lower(btrim(name)) FROM %s.applications WHERE lower(btrim(name)) = ANY($1::text[])`, s), names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		taken[spelled[key]] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(taken))
	for name := range taken {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// abortedStatement marks the errors every statement after the first failure
// in a transaction answers. They repeat the first error's consequence, not a
// cause of their own.
const abortedStatement = "SQLSTATE 25P02"

func withoutAbortedStatements(o importOutcome) importOutcome {
	keep := func(entries []any) []any {
		out := make([]any, 0, len(entries))
		for _, entry := range entries {
			if record, ok := entry.(map[string]any); ok {
				if message, _ := record["msg"].(string); strings.Contains(message, abortedStatement) {
					continue
				}
			}
			out = append(out, entry)
		}
		return out
	}
	o.errorAgents, o.errorToolkits, o.errorSkills = keep(o.errorAgents), keep(o.errorToolkits), keep(o.errorSkills)
	return o
}

// firstImportError is the first cause of a refused team import, as
// "name: message".
func firstImportError(o importOutcome) string {
	cleaned := withoutAbortedStatements(o)
	for _, channel := range [][]any{cleaned.errorAgents, cleaned.errorToolkits, cleaned.errorSkills} {
		for _, entry := range channel {
			if record, ok := entry.(map[string]any); ok {
				name, _ := record["name"].(string)
				message, _ := record["msg"].(string)
				message = strings.TrimPrefix(message, "Import function has been failed: ")
				if name != "" {
					return name + ": " + message
				}
				return message
			}
		}
	}
	return "an entity could not be written"
}

func teamImportJSON(id, project int64, team agentteams.Team) map[string]any {
	members := make([]map[string]any, 0, len(team.Members))
	for _, member := range team.Members {
		members = append(members, map[string]any{
			"application_id": member.ApplicationID, "project_id": member.ProjectID,
			"version_id": member.VersionID, "name": member.Name,
		})
	}
	var lead any
	if team.Lead > 0 {
		lead = team.Lead
	}
	return map[string]any{
		"id": id, "project_id": project, "name": team.Name, "description": team.Description,
		"floor_policy": string(team.Policy), "lead_position": lead, "members": members,
	}
}
