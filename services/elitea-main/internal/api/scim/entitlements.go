package scim

// The read-only entitlement tree (ADR-0044 decision 5, issue 1501).
//
// # What it is for
//
// An identity governance tool (SailPoint class) runs access review campaigns
// on WHO HOLDS WHAT. Most of them already speak SCIM 2.0, so the export is
// served as a second SCIM service provider whose base URL is
//
//	https://<host>/api/v2/scim/v2/entitlements
//
// and which answers /Users, /Users/{id}, /Groups, /Groups/{id} and the three
// discovery documents. Every write verb is answered 405 by the router: the
// tree reports grants, it never makes one.
//
// # Why a second tree and not more attributes on /Users and /Groups
//
// The provisioning tree's /Groups are the AUTHORED group bindings (shared
// 0098): a group an identity provider pushes members into. A project role is
// a different thing — every project role on the deployment, whoever granted
// it — and serving both under one /Groups would make a PATCH from the
// identity provider land on a role nobody bound. So the role groups live
// here, with ids (`project-role-<id>`, `platform-role-<id>`) no binding id
// can collide with.
//
// # Authentication and audit
//
// The same SCIM client credential as the provisioning tree (auth.go): the
// tree is mounted inside it. SCIM clients carry no scope, so any client can
// read it; it reveals no secret, and it lists only what an administrator's
// Users and Roles pages already show. Every Users and Groups answer writes an
// audit_events row (actor `scim:<client>`, format scim, resource count)
// before the body is sent, and is refused with 503 when the row cannot be
// written.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/entitlements"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/scimdirectory"
)

// EntitlementsPath is the entitlement tree, relative to the SCIM mount.
const EntitlementsPath = "/entitlements"

// EntitlementsBasePath is the base URL path an identity governance tool is
// configured with.
const EntitlementsBasePath = BasePath + EntitlementsPath

const (
	// schemaEntitlementUser carries every grant of an account with its scope,
	// role, capability, permissions and how it is held — the CSV row shape.
	schemaEntitlementUser = "urn:elitea:params:scim:schemas:extension:entitlements:2.0:User"
	// schemaEntitlementGroup carries a role group's scope, permissions and
	// capabilities.
	schemaEntitlementGroup = "urn:elitea:params:scim:schemas:extension:entitlements:2.0:Group"
)

// EntitlementSource is *repos.Entitlements.
type EntitlementSource interface {
	ListUsers(ctx context.Context, filter scimdirectory.Filter, startIndex, count int) ([]entitlements.UserEntitlements, int, error)
	GetUser(ctx context.Context, id int64) (entitlements.UserEntitlements, error)
	ListGroups(ctx context.Context, filter scimdirectory.Filter, startIndex, count int) ([]entitlements.Group, int, error)
	GetGroup(ctx context.Context, id string) (entitlements.Group, error)
	RecordExport(ctx context.Context, record repos.EntitlementExportAudit) error
}

// WithEntitlements attaches the entitlement source. Without one the tree
// answers 503.
func (h *Handler) WithEntitlements(source EntitlementSource) *Handler {
	h.entitlements = source
	return h
}

func (h *Handler) entitlementRoutes(router chi.Router) {
	router.Get("/ServiceProviderConfig", h.EntitlementServiceProviderConfig)
	router.Get("/ResourceTypes", h.EntitlementResourceTypes)
	router.Get("/Schemas", h.EntitlementSchemas)
	router.Get("/Users", h.ListEntitlementUsers)
	router.Get("/Users/{id}", h.GetEntitlementUser)
	router.Get("/Groups", h.ListEntitlementGroups)
	router.Get("/Groups/{id}", h.GetEntitlementGroup)
}

func (h *Handler) entitlementsReady(w http.ResponseWriter) bool {
	if h.entitlements != nil {
		return true
	}
	writeError(w, http.StatusServiceUnavailable, "", "the entitlement export is not available on this deployment")
	return false
}

// recordEntitlementRead writes the audit row of one answer, or answers 503.
func (h *Handler) recordEntitlementRead(w http.ResponseWriter, r *http.Request, resources int) bool {
	actor := ""
	if principal, ok := PrincipalFromContext(r.Context()); ok {
		actor = principal.ActorLabel()
	}
	err := h.entitlements.RecordExport(r.Context(), repos.EntitlementExportAudit{
		Actor: actor, Format: "scim", Rows: resources,
		Route: chi.RouteContext(r.Context()).RoutePattern(),
	})
	if err != nil {
		slog.ErrorContext(r.Context(), "SCIM entitlements: read not audited, refusing it", "error", err)
		writeError(w, http.StatusServiceUnavailable, "", "the read could not be recorded in the audit trail; retry later")
		return false
	}
	return true
}

/* ── discovery ─────────────────────────────────────────────────────────── */

// EntitlementServiceProviderConfig answers the tree's configuration: filter
// and paging, nothing writable.
func (h *Handler) EntitlementServiceProviderConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"schemas":        []string{schemaProviderCfg},
		"patch":          map[string]any{"supported": false},
		"bulk":           map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":         map[string]any{"supported": true, "maxResults": maxPageSize},
		"changePassword": map[string]any{"supported": false},
		"sort":           map[string]any{"supported": false},
		"etag":           map[string]any{"supported": false},
		"authenticationSchemes": []map[string]any{{
			"type": "oauthbearertoken",
			"name": "OAuth Bearer Token",
			"description": "A SCIM client credential: the secret of a bearer client, or an access token " +
				"from the OAuth 2.0 client credentials grant at " + TokenPath + ".",
			"specUri": "https://www.rfc-editor.org/rfc/rfc6750",
			"primary": true,
		}},
		"meta": map[string]any{
			"resourceType": "ServiceProviderConfig",
			"location":     EntitlementsBasePath + "/ServiceProviderConfig",
		},
	})
}

// EntitlementResourceTypes answers the two read-only resource types.
func (h *Handler) EntitlementResourceTypes(w http.ResponseWriter, _ *http.Request) {
	documents := []any{
		map[string]any{
			"schemas": []string{schemaResourceType}, "id": "User", "name": "User", "endpoint": "/Users",
			"description":      "An account and every role and governance capability it holds. Read-only.",
			"schema":           schemaUser,
			"schemaExtensions": []map[string]any{{"schema": schemaEntitlementUser, "required": true}},
			"meta":             map[string]any{"resourceType": "ResourceType", "location": EntitlementsBasePath + "/ResourceTypes/User"},
		},
		map[string]any{
			"schemas": []string{schemaResourceType}, "id": "Group", "name": "Group", "endpoint": "/Groups",
			"description":      "One platform role or project role, its capabilities and its holders. Read-only.",
			"schema":           schemaGroup,
			"schemaExtensions": []map[string]any{{"schema": schemaEntitlementGroup, "required": true}},
			"meta":             map[string]any{"resourceType": "ResourceType", "location": EntitlementsBasePath + "/ResourceTypes/Group"},
		},
	}
	writeJSON(w, http.StatusOK, listResponse(documents, len(documents), len(documents)))
}

// EntitlementSchemas answers the two extension schemas.
func (h *Handler) EntitlementSchemas(w http.ResponseWriter, _ *http.Request) {
	readOnly := func(name, kind, description string) map[string]any {
		return map[string]any{
			"name": name, "type": kind, "multiValued": false, "description": description,
			"required": false, "caseExact": true, "mutability": "readOnly", "returned": "default", "uniqueness": "none",
		}
	}
	grant := []map[string]any{
		readOnly("scopeType", "string", "platform, project or agent."),
		readOnly("scopeId", "string", "The mode (platform), the project id or the agent id."),
		readOnly("scopeName", "string", "The scope's name."),
		readOnly("projectId", "integer", "The project of a project or agent scope."),
		readOnly("scopeActive", "boolean", "False when the project is suspended: the grant is dormant."),
		readOnly("role", "string", "The role, or business_owner / technical_owner for an agent."),
		readOnly("capability", "string", "The governance capability, or empty for a role that carries none."),
		{"name": "permissions", "type": "string", "multiValued": true, "description": "The permissions that carry the capability.",
			"required": false, "caseExact": true, "mutability": "readOnly", "returned": "default", "uniqueness": "none"},
		readOnly("grantedVia", "string", "role, owner or direct."),
	}
	documents := []any{
		map[string]any{
			"id": schemaEntitlementUser, "name": "EntitlementUser",
			"description": "Every grant an account holds.",
			"attributes": []map[string]any{{
				"name": "grants", "type": "complex", "multiValued": true, "description": "One entry per export row.",
				"required": false, "mutability": "readOnly", "returned": "default", "uniqueness": "none",
				"subAttributes": grant,
			}},
			"meta": map[string]any{"resourceType": "Schema", "location": EntitlementsBasePath + "/Schemas/" + schemaEntitlementUser},
		},
		map[string]any{
			"id": schemaEntitlementGroup, "name": "EntitlementGroup",
			"description": "What a role group grants.",
			"attributes": []map[string]any{
				readOnly("scopeType", "string", "platform or project."),
				readOnly("scopeId", "string", "The mode (platform) or the project id."),
				readOnly("scopeName", "string", "The scope's name."),
				readOnly("projectId", "integer", "The project of a project role."),
				readOnly("scopeActive", "boolean", "False when the project is suspended."),
				readOnly("role", "string", "The role name."),
				{"name": "capabilities", "type": "string", "multiValued": true, "description": "The governance capabilities.",
					"required": false, "caseExact": true, "mutability": "readOnly", "returned": "default", "uniqueness": "none"},
				{"name": "permissions", "type": "string", "multiValued": true, "description": "Every permission the role grants.",
					"required": false, "caseExact": true, "mutability": "readOnly", "returned": "default", "uniqueness": "none"},
			},
			"meta": map[string]any{"resourceType": "Schema", "location": EntitlementsBasePath + "/Schemas/" + schemaEntitlementGroup},
		},
	}
	writeJSON(w, http.StatusOK, listResponse(documents, len(documents), len(documents)))
}

/* ── users ─────────────────────────────────────────────────────────────── */

// ListEntitlementUsers answers `GET /entitlements/Users`.
func (h *Handler) ListEntitlementUsers(w http.ResponseWriter, r *http.Request) {
	if !h.entitlementsReady(w) {
		return
	}
	filter, ok := entitlementFilter(w, r, scimdirectory.ParseEntitlementUserFilter)
	if !ok {
		return
	}
	startIndex, count := pagination(r)
	users, total, err := h.entitlements.ListUsers(r.Context(), filter, startIndex, count)
	if err != nil {
		slog.ErrorContext(r.Context(), "SCIM entitlements: list users", "error", err)
		writeError(w, http.StatusInternalServerError, "", "the entitlements could not be read")
		return
	}
	if !h.recordEntitlementRead(w, r, len(users)) {
		return
	}
	resources := make([]any, 0, len(users))
	for _, user := range users {
		resources = append(resources, projectResource(entitlementUserResource(user), r))
	}
	body := listResponse(resources, total, len(resources))
	body["startIndex"] = startIndex
	writeJSON(w, http.StatusOK, body)
}

// GetEntitlementUser answers `GET /entitlements/Users/{id}`.
func (h *Handler) GetEntitlementUser(w http.ResponseWriter, r *http.Request) {
	if !h.entitlementsReady(w) {
		return
	}
	raw := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "", "no such user")
		return
	}
	user, err := h.entitlements.GetUser(r.Context(), id)
	if errors.Is(err, repos.ErrEntitlementNotFound) {
		writeError(w, http.StatusNotFound, "", "no such user")
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "SCIM entitlements: get user", "error", err)
		writeError(w, http.StatusInternalServerError, "", "the entitlements could not be read")
		return
	}
	if !h.recordEntitlementRead(w, r, 1) {
		return
	}
	writeJSON(w, http.StatusOK, projectResource(entitlementUserResource(user), r))
}

// scopeKey is the scope part of a role or entitlement value:
// `platform:<mode>`, `project:<id>` or `agent:<project id>/<agent id>`.
func scopeKey(g entitlements.Grant) string {
	if g.ScopeType == entitlements.ScopeAgent {
		return g.ScopeType + ":" + strconv.FormatInt(g.ProjectID, 10) + "/" + g.ScopeID
	}
	return g.ScopeType + ":" + g.ScopeID
}

func scopeLabel(g entitlements.Grant) string {
	name := g.ScopeName
	if name == "" {
		name = g.ScopeType + " " + g.ScopeID
	}
	switch g.ScopeType {
	case entitlements.ScopeProject:
		return "project " + name
	case entitlements.ScopeAgent:
		return "agent " + name + " (project " + strconv.FormatInt(g.ProjectID, 10) + ")"
	}
	return name
}

// entitlementUserResource renders one account with its grants.
//
//   - `groups`: the role groups it is a member of (direct membership).
//   - `roles`: every role held, value `<scope>:<role>`, type the scope type.
//   - `entitlements`: every governance capability held, value
//     `<scope>:<capability>`, type the capability. Distinct per scope.
//   - the extension's `grants`: the export rows, one per (scope, role,
//     capability, granted_via), with the permissions behind each.
func entitlementUserResource(user entitlements.UserEntitlements) map[string]any {
	groups := []map[string]any{}
	roles := []map[string]any{}
	values := []map[string]any{}
	grants := []map[string]any{}
	seenGroup, seenRole, seenEntitlement := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, g := range user.Grants {
		if id := entitlements.GroupID(g.ScopeType, g.RoleID); id != "" && !seenGroup[id] {
			seenGroup[id] = true
			groups = append(groups, map[string]any{
				"value": id, "display": scopeLabel(g) + " / " + g.Role, "type": "direct",
				"$ref": EntitlementsBasePath + "/Groups/" + id,
			})
		}
		roleValue := scopeKey(g) + ":" + g.Role
		if !seenRole[roleValue] {
			seenRole[roleValue] = true
			roles = append(roles, map[string]any{
				"value": roleValue, "display": g.Role + " on " + scopeLabel(g), "type": g.ScopeType,
			})
		}
		if g.Capability != "" {
			value := scopeKey(g) + ":" + g.Capability
			if !seenEntitlement[value] {
				seenEntitlement[value] = true
				values = append(values, map[string]any{
					"value": value, "display": g.Capability + " on " + scopeLabel(g), "type": g.Capability,
				})
			}
		}
		permissions := g.Permissions
		if permissions == nil {
			permissions = []string{}
		}
		grant := map[string]any{
			"scopeType": g.ScopeType, "scopeId": g.ScopeID, "scopeName": g.ScopeName,
			"scopeActive": g.ScopeActive, "role": g.Role, "capability": g.Capability,
			"permissions": permissions, "grantedVia": g.GrantedVia,
		}
		if g.ProjectID > 0 {
			grant["projectId"] = g.ProjectID
		}
		grants = append(grants, grant)
	}
	id := strconv.FormatInt(user.UserID, 10)
	return map[string]any{
		"schemas":             []string{schemaUser, schemaEntitlementUser},
		"id":                  id,
		"userName":            user.UserName,
		"displayName":         user.DisplayName,
		"active":              user.Active,
		"groups":              groups,
		"roles":               roles,
		"entitlements":        values,
		schemaEntitlementUser: map[string]any{"grants": grants},
		"meta": map[string]any{
			"resourceType": "User",
			"location":     EntitlementsBasePath + "/Users/" + id,
		},
	}
}

/* ── groups ────────────────────────────────────────────────────────────── */

// ListEntitlementGroups answers `GET /entitlements/Groups`.
func (h *Handler) ListEntitlementGroups(w http.ResponseWriter, r *http.Request) {
	if !h.entitlementsReady(w) {
		return
	}
	filter, ok := entitlementFilter(w, r, scimdirectory.ParseEntitlementGroupFilter)
	if !ok {
		return
	}
	startIndex, count := pagination(r)
	groups, total, err := h.entitlements.ListGroups(r.Context(), filter, startIndex, count)
	if err != nil {
		slog.ErrorContext(r.Context(), "SCIM entitlements: list groups", "error", err)
		writeError(w, http.StatusInternalServerError, "", "the entitlements could not be read")
		return
	}
	if !h.recordEntitlementRead(w, r, len(groups)) {
		return
	}
	resources := make([]any, 0, len(groups))
	for _, group := range groups {
		resources = append(resources, projectResource(entitlementGroupResource(group), r))
	}
	body := listResponse(resources, total, len(resources))
	body["startIndex"] = startIndex
	writeJSON(w, http.StatusOK, body)
}

// GetEntitlementGroup answers `GET /entitlements/Groups/{id}`.
func (h *Handler) GetEntitlementGroup(w http.ResponseWriter, r *http.Request) {
	if !h.entitlementsReady(w) {
		return
	}
	group, err := h.entitlements.GetGroup(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, repos.ErrEntitlementNotFound) {
		writeError(w, http.StatusNotFound, "", "no such group")
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "SCIM entitlements: get group", "error", err)
		writeError(w, http.StatusInternalServerError, "", "the entitlements could not be read")
		return
	}
	if !h.recordEntitlementRead(w, r, 1) {
		return
	}
	writeJSON(w, http.StatusOK, projectResource(entitlementGroupResource(group), r))
}

func entitlementGroupResource(group entitlements.Group) map[string]any {
	members := make([]map[string]any, 0, len(group.Members))
	for _, member := range group.Members {
		id := strconv.FormatInt(member.UserID, 10)
		members = append(members, map[string]any{
			"value": id, "display": member.UserName, "type": "User",
			"$ref": EntitlementsBasePath + "/Users/" + id,
		})
	}
	extension := map[string]any{
		"scopeType": group.ScopeType, "scopeId": group.ScopeID, "scopeName": group.ScopeName,
		"scopeActive": group.ScopeActive, "role": group.Role,
		"capabilities": group.Capabilities, "permissions": group.Permissions,
	}
	if group.ProjectID > 0 {
		extension["projectId"] = group.ProjectID
	}
	return map[string]any{
		"schemas":              []string{schemaGroup, schemaEntitlementGroup},
		"id":                   group.ID,
		"displayName":          group.DisplayName,
		"members":              members,
		schemaEntitlementGroup: extension,
		"meta": map[string]any{
			"resourceType": "Group",
			"location":     EntitlementsBasePath + "/Groups/" + group.ID,
		},
	}
}

// entitlementFilter parses the `filter` parameter, answering 400
// invalidFilter for an expression the tree cannot represent.
func entitlementFilter(w http.ResponseWriter, r *http.Request, parse func(string) (scimdirectory.Filter, error)) (scimdirectory.Filter, bool) {
	filter, err := parse(r.URL.Query().Get("filter"))
	if err == nil {
		return filter, true
	}
	var unsupported scimdirectory.UnsupportedFilterError
	if errors.As(err, &unsupported) {
		writeError(w, http.StatusBadRequest, "invalidFilter", unsupported.Reason)
		return scimdirectory.Filter{}, false
	}
	writeError(w, http.StatusBadRequest, "invalidFilter", "the filter could not be read")
	return scimdirectory.Filter{}, false
}
