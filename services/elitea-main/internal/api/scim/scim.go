// Package scim serves SCIM 2.0 user provisioning (RFC 7643, RFC 7644).
//
// # What this is for
//
// An identity provider that federates logins can also PUSH the directory:
// create the account when somebody joins, update it when their name or address
// changes, and deactivate it when they leave. Without it an account is created
// on first login and never removed, so a person who left the company keeps a
// working account until somebody suspends it by hand.
//
// Nothing here is a port. `grep -ril scim legacy/plugins/` finds nothing: pylon
// never had SCIM either.
//
// # The base URL an operator configures
//
//	https://<host>/api/v2/scim/v2
//
// Not `/scim/v2`. Every authenticated route in this service lives under
// `/api/v2`, and that prefix is where the authentication middleware is mounted
// (internal/api/router.go). A second, separately-authenticated tree at the root
// would be a second place for the identity check to be got wrong.
//
// # How a client authenticates, and why there is no SCIM token type
//
// With a PERSONAL ACCESS TOKEN, as a bearer credential, belonging to an account
// that holds `admin.auth.users` in administration mode — the same permission the
// admin Users page's write routes require. A SCIM client that can create and
// deactivate accounts is doing exactly what that page does.
//
// The alternative was a dedicated SCIM token table with its own hashing,
// rotation and admin surface. It was rejected: a PAT is already a static bearer
// credential with an owner, an expiry and a revocation path, and a second
// credential type would be a second set of those to get right. What an operator
// does instead is create a service account, grant it the administration role,
// and mint a token — all of which the platform already supports.
//
// # Groups map onto a project, and the missing half is authored
//
// `/Groups` IS implemented, and `ResourceTypes` lists it. It did not exist in
// the first revision of this package, which refused it with 501 and a reason
// that was true about the data: a SCIM group carries a flat name and a member
// list, this platform's membership is always (user, PROJECT, ROLE), and a group
// says nothing about either half of what it would have to grant.
//
// What changed is where the missing half comes from. It is AUTHORED, in
// `elitea_auth.scim_group_bindings` (shared migration 0098), by an
// administrator, before any push: one binding names one project and one role,
// and a group push supplies only the members. So the identity provider decides
// WHO, and this deployment decides WHAT THEY GET — neither can invent the
// other. ADR-0021 supersedes ADR-0020's decision 8 and records this.
//
// A group with no binding is refused BY NAME. It is not answered with an empty
// list, and it does not provision a project: see internal/api/scim/groups.go for
// what each verb does and does not do.

package scim

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/scimdirectory"
)

// BasePath is where this tree is mounted, and the value that appears in every
// `meta.location`. It is declared once so the router and the resource encoder
// cannot disagree about it.
const BasePath = "/api/v2/scim/v2"

// MountPath is the same tree relative to the `/api/v2` group the router mounts
// it inside.
//
// The two are separate constants because they answer different questions and
// are used in different places: chi needs the relative path, and `meta.location`
// must be the absolute one a client can fetch. Deriving one from the other with
// a TrimPrefix would tie the router's nesting to the wire contract, so a change
// to the nesting would silently rewrite every location a client has stored.
const MountPath = "/scim/v2"

const (
	schemaUser  = "urn:ietf:params:scim:schemas:core:2.0:User"
	schemaGroup = "urn:ietf:params:scim:schemas:core:2.0:Group"
	// schemaProjectGrant is THIS SERVICE'S extension, and it is read-only.
	//
	// It carries the project and the role a group grants, so an operator
	// reading their identity provider's log can see what a push did without
	// opening the admin screen. It is returned and never accepted: a client that
	// sent it would be choosing a project, and choosing the project is what the
	// authored binding exists to keep out of the identity provider's hands.
	schemaProjectGrant = "urn:elitea:params:scim:schemas:extension:projectgrant:2.0:Group"
	schemaListResponse = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	schemaError        = "urn:ietf:params:scim:api:messages:2.0:Error"
	schemaEnterprise   = "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"
	schemaProviderCfg  = "urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"
	schemaResourceType = "urn:ietf:params:scim:schemas:core:2.0:ResourceType"

	// contentType is the media type RFC 7644 defines. `application/json` is
	// also accepted on the way in, because several identity providers send it.
	contentType = "application/scim+json"

	// defaultPageSize and maxPageSize bound a listing. A client that asks for
	// no count gets the default; one that asks for more than the maximum gets
	// the maximum, which is what RFC 7644 §3.4.2.4 says a service provider may
	// do — and it is why `itemsPerPage` is returned rather than assumed.
	defaultPageSize = 100
	maxPageSize     = 500
)

// Directory is the store seam. An interface so this package can be tested
// without a database, and so the HTTP shape and the SQL stay separable.
type Directory interface {
	List(ctx context.Context, filter scimdirectory.Filter, startIndex, count int) ([]scimdirectory.User, int, error)
	Get(ctx context.Context, id int) (scimdirectory.User, error)
	Create(ctx context.Context, user scimdirectory.User) (scimdirectory.User, error)
	Replace(ctx context.Context, id int, user scimdirectory.User) (scimdirectory.User, error)
	SetActive(ctx context.Context, id int, active bool) (scimdirectory.User, error)
	// ApplyUserChanges persists a whole PATCH atomically.
	ApplyUserChanges(ctx context.Context, id int, changes scimdirectory.UserChanges) (scimdirectory.User, error)

	ListGroups(ctx context.Context, filter scimdirectory.Filter, startIndex, count int) ([]scimdirectory.Group, int, error)
	GetGroup(ctx context.Context, id int64) (scimdirectory.Group, error)
	LookupGroup(ctx context.Context, externalID, displayName string) (scimdirectory.Group, error)
	AdoptGroup(ctx context.Context, id int64, externalID, displayName string) error
	RenameGroup(ctx context.Context, id int64, displayName string) (scimdirectory.Group, error)
	AddGroupMembers(ctx context.Context, id int64, members []int) (scimdirectory.Group, error)
	ReplaceGroupMembers(ctx context.Context, id int64, members []int) (scimdirectory.Group, error)
	RemoveGroupMembers(ctx context.Context, id int64, members []int) (scimdirectory.Group, error)
	// ApplyGroupOperations applies a whole PATCH in one transaction. It is a
	// separate method rather than a loop over the three above because a PATCH
	// is sent once: an operation that lands while a later one is refused is a
	// change the client believes it did not make.
	ApplyGroupOperations(ctx context.Context, id int64, operations []scimdirectory.GroupOperation) (scimdirectory.Group, error)
	DeleteGroup(ctx context.Context, id int64) error
	ResolveMember(ctx context.Context, value string) (int, error)
}

// Handler serves the SCIM tree.
type Handler struct {
	directory Directory
}

func NewHandler(directory Directory) *Handler {
	return &Handler{directory: directory}
}

// Routes returns the SCIM sub-router.
//
// It is assembled HERE rather than in internal/api/router.go so the paths and
// their handlers stay in one file — the tree is a published contract an operator
// pastes into an identity provider, and a path spread across two files is a path
// that gets renamed in one of them.
func (h *Handler) Routes() chi.Router {
	router := chi.NewRouter()
	router.Get("/ServiceProviderConfig", h.ServiceProviderConfig)
	router.Get("/ResourceTypes", h.ResourceTypes)
	router.Get("/ResourceTypes/{id}", h.ResourceType)
	router.Get("/Schemas", h.Schemas)
	router.Get("/Schemas/{id}", h.Schema)

	router.Get("/Users", h.ListUsers)
	router.Post("/Users", h.CreateUser)
	router.Get("/Users/{id}", h.GetUser)
	router.Put("/Users/{id}", h.ReplaceUser)
	router.Patch("/Users/{id}", h.PatchUser)
	router.Delete("/Users/{id}", h.DeleteUser)

	router.Get("/Groups", h.ListGroups)
	router.Post("/Groups", h.CreateGroup)
	router.Get("/Groups/{id}", h.GetGroup)
	router.Put("/Groups/{id}", h.ReplaceGroup)
	router.Patch("/Groups/{id}", h.PatchGroup)
	router.Delete("/Groups/{id}", h.DeleteGroup)
	return router
}

// ready reports whether the directory is wired, answering 503 when it is not.
//
// A SCIM client treats a 5xx as retryable and a 2xx as done. Answering an empty
// list from an unwired handler would tell an identity provider that this
// deployment has no users and that every deactivation had already been applied.
func (h *Handler) ready(w http.ResponseWriter) bool {
	if h.directory != nil {
		return true
	}
	writeError(w, http.StatusServiceUnavailable, "",
		"user provisioning is not available on this deployment")
	return false
}

/* ── discovery ─────────────────────────────────────────────────────────── */

// ServiceProviderConfig answers `GET /ServiceProviderConfig`.
//
// Every capability below is reported as it IS. `patch.supported` is true and
// `bulk.supported` is false because that is what this handler does; a
// configuration document that over-reports is how a client comes to send
// requests the server then fails.
func (h *Handler) ServiceProviderConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"schemas": []string{schemaProviderCfg},
		"documentationUri": "https://" +
			"github.com/EliteaAI/elitea-platform/blob/main/services/elitea-main/internal/api/scim/scim.go",
		"patch":          map[string]any{"supported": true},
		"bulk":           map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":         map[string]any{"supported": true, "maxResults": maxPageSize},
		"changePassword": map[string]any{"supported": false},
		"sort":           map[string]any{"supported": false},
		"etag":           map[string]any{"supported": false},
		"authenticationSchemes": []map[string]any{{
			"type":        "oauthbearertoken",
			"name":        "OAuth Bearer Token",
			"description": "A platform personal access token belonging to an account that holds admin.auth.users.",
			"primary":     true,
		}},
		"meta": map[string]any{
			"resourceType": "ServiceProviderConfig",
			"location":     BasePath + "/ServiceProviderConfig",
		},
	})
}

// ResourceTypes answers `GET /ResourceTypes`. It lists User and Group, both of
// which this tree serves.
func resourceTypeDocuments() []any {
	userType := map[string]any{
		"schemas":     []string{schemaResourceType},
		"id":          "User",
		"name":        "User",
		"endpoint":    "/Users",
		"description": "Platform accounts.",
		"schema":      schemaUser,
		// Announced so the enterprise attributes Entra sends are a declared
		// extension. They are accepted and not stored; see patch_user.go.
		"schemaExtensions": []map[string]any{{
			"schema": schemaEnterprise, "required": false,
		}},
		"meta": map[string]any{
			"resourceType": "ResourceType",
			"location":     BasePath + "/ResourceTypes/User",
		},
	}
	groupType := map[string]any{
		"schemas":  []string{schemaResourceType},
		"id":       "Group",
		"name":     "Group",
		"endpoint": "/Groups",
		"description": "An identity provider group bound to one project role. " +
			"The project and the role are authored on this deployment; a push carries the members.",
		"schema": schemaGroup,
		// The extension is declared REQUIRED FALSE because a client never sends
		// it. It is announced so the values it returns are not an undocumented
		// attribute appearing in a response.
		"schemaExtensions": []map[string]any{{
			"schema": schemaProjectGrant, "required": false,
		}},
		"meta": map[string]any{
			"resourceType": "ResourceType",
			"location":     BasePath + "/ResourceTypes/Group",
		},
	}
	return []any{userType, groupType}
}

// ResourceTypes answers `GET /ResourceTypes`.
func (h *Handler) ResourceTypes(w http.ResponseWriter, _ *http.Request) {
	documents := resourceTypeDocuments()
	writeJSON(w, http.StatusOK, listResponse(documents, len(documents), len(documents)))
}

// ResourceType answers `GET /ResourceTypes/{id}`, the address `meta.location`
// advertises.
func (h *Handler) ResourceType(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	for _, document := range resourceTypeDocuments() {
		if strings.EqualFold(document.(map[string]any)["id"].(string), id) {
			writeJSON(w, http.StatusOK, document)
			return
		}
	}
	writeError(w, http.StatusNotFound, "", "no such resource type")
}

// schemaDocuments lists the attributes this directory really
// reads.
//
// Each document describes the attributes this service ACTS ON and no more. A
// client uses it to decide what to send, so listing the whole core schema would
// invite it to push a manager, a department and a set of phone numbers that this
// service stores nowhere.
func schemaDocuments() []any {
	attribute := func(name, kind, description string) map[string]any {
		return map[string]any{
			"name": name, "type": kind, "multiValued": false,
			"description": description, "required": name == "userName",
			"caseExact": false, "mutability": "readWrite",
			"returned": "default", "uniqueness": uniquenessOf(name),
		}
	}
	userSchema := map[string]any{
		"id":          schemaUser,
		"name":        "User",
		"description": "Platform account.",
		"attributes": []map[string]any{
			attribute("userName", "string", "The account's email address. Case-insensitive."),
			attribute("displayName", "string", "The name shown in the product."),
			attribute("active", "boolean", "False suspends the account; it is not deleted."),
			attribute("externalId", "string", "The identity provider's own identifier."),
			complexAttribute("name", "The name parts. The formatted value is kept as the display name.",
				subAttribute("formatted", "The full name."),
				subAttribute("givenName", "The given name."),
				subAttribute("familyName", "The family name.")),
			complexAttribute("emails", "The work or primary value is kept as the account address.",
				subAttribute("value", "The address."),
				subAttribute("type", "work, home or other."),
				map[string]any{
					"name": "primary", "type": "boolean", "multiValued": false,
					"description": "Whether this is the primary address.",
					"required":    false, "mutability": "readWrite", "returned": "default", "uniqueness": "none",
				}),
		},
		"meta": map[string]any{"resourceType": "Schema", "location": BasePath + "/Schemas/" + schemaUser},
	}
	groupSchema := map[string]any{
		"id":   schemaGroup,
		"name": "Group",
		"description": "An identity provider group bound to one project role. " +
			"The binding is authored on this deployment; a push carries the members.",
		"attributes": []map[string]any{
			{
				"name": "displayName", "type": "string", "multiValued": false,
				"description": "The group name. It matches the name on the authored binding, " +
					"and a rename is applied to the binding it already resolved to.",
				"required": true, "caseExact": false, "mutability": "readWrite",
				"returned": "default", "uniqueness": "server",
			},
			{
				"name": "externalId", "type": "string", "multiValued": false,
				"description": "The identity provider's own identifier for the group.",
				"required":    false, "caseExact": false, "mutability": "readWrite",
				"returned": "default", "uniqueness": "server",
			},
			{
				"name": "members", "type": "complex", "multiValued": true,
				"description": "The accounts that hold the bound role. A member value is the " +
					"platform id, the externalId or the address of an account that already exists; " +
					"a value that resolves to no account, or to more than one, is refused.",
				"required": false, "caseExact": false, "mutability": "readWrite",
				"returned": "default", "uniqueness": "none",
				"subAttributes": []map[string]any{
					{
						"name": "value", "type": "string", "multiValued": false,
						"description": "The account this member names.",
						"required":    true, "caseExact": false, "mutability": "immutable",
						"returned": "default", "uniqueness": "none",
					},
					{
						"name": "display", "type": "string", "multiValued": false,
						"description": "The account's address. Returned, never read.",
						"required":    false, "caseExact": false, "mutability": "readOnly",
						"returned": "default", "uniqueness": "none",
					},
				},
			},
		},
		"meta": map[string]any{"resourceType": "Schema", "location": BasePath + "/Schemas/" + schemaGroup},
	}
	grantSchema := map[string]any{
		"id":   schemaProjectGrant,
		"name": "Project grant",
		"description": "What a group grants on this deployment. Authored by an administrator, " +
			"returned on every group, and never accepted on a write.",
		"attributes": []map[string]any{
			{
				"name": "projectId", "type": "integer", "multiValued": false,
				"description": "The project the members join.",
				"required":    false, "caseExact": false, "mutability": "readOnly",
				"returned": "default", "uniqueness": "none",
			},
			{
				"name": "projectName", "type": "string", "multiValued": false,
				"description": "That project's name.",
				"required":    false, "caseExact": false, "mutability": "readOnly",
				"returned": "default", "uniqueness": "none",
			},
			{
				"name": "role", "type": "string", "multiValued": false,
				"description": "The project role every member receives.",
				"required":    false, "caseExact": false, "mutability": "readOnly",
				"returned": "default", "uniqueness": "none",
			},
		},
		"meta": map[string]any{"resourceType": "Schema", "location": BasePath + "/Schemas/" + schemaProjectGrant},
	}
	enterpriseSchema := map[string]any{
		"id":   schemaEnterprise,
		"name": "EnterpriseUser",
		"description": "Enterprise attributes an identity provider sends. They are accepted " +
			"and not stored by this deployment.",
		"attributes": []map[string]any{
			attribute("employeeNumber", "string", "Accepted and not stored."),
			attribute("department", "string", "Accepted and not stored."),
			attribute("organization", "string", "Accepted and not stored."),
			attribute("costCenter", "string", "Accepted and not stored."),
			attribute("division", "string", "Accepted and not stored."),
			complexAttribute("manager", "Accepted and not stored.",
				subAttribute("value", "The manager's id."),
				subAttribute("displayName", "The manager's name.")),
		},
		"meta": map[string]any{"resourceType": "Schema", "location": BasePath + "/Schemas/" + schemaEnterprise},
	}
	return []any{userSchema, groupSchema, grantSchema, enterpriseSchema}
}

// Schemas answers `GET /Schemas`.
func (h *Handler) Schemas(w http.ResponseWriter, _ *http.Request) {
	documents := schemaDocuments()
	writeJSON(w, http.StatusOK, listResponse(documents, len(documents), len(documents)))
}

// Schema answers `GET /Schemas/{id}`, the address `meta.location` advertises.
func (h *Handler) Schema(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	for _, document := range schemaDocuments() {
		if strings.EqualFold(document.(map[string]any)["id"].(string), id) {
			writeJSON(w, http.StatusOK, document)
			return
		}
	}
	writeError(w, http.StatusNotFound, "", "no such schema")
}

func subAttribute(name, description string) map[string]any {
	return map[string]any{
		"name": name, "type": "string", "multiValued": false, "description": description,
		"required": false, "caseExact": false, "mutability": "readWrite",
		"returned": "default", "uniqueness": "none",
	}
}

func complexAttribute(name, description string, subAttributes ...map[string]any) map[string]any {
	return map[string]any{
		"name": name, "type": "complex", "multiValued": name == "emails", "description": description,
		"required": false, "mutability": "readWrite", "returned": "default", "uniqueness": "none",
		"subAttributes": subAttributes,
	}
}

func uniquenessOf(name string) string {
	if name == "userName" || name == "externalId" {
		return "server"
	}
	return "none"
}

/* ── encoding ──────────────────────────────────────────────────────────── */

func listResponse(resources []any, total, perPage int) map[string]any {
	return map[string]any{
		"schemas":      []string{schemaListResponse},
		"totalResults": total,
		"startIndex":   1,
		"itemsPerPage": perPage,
		// Capital R. RFC 7644 §3.4.2 names the member `Resources`, and a client
		// that looks for `resources` finds nothing — which reads to it as an
		// empty directory rather than as a malformed response.
		"Resources": resources,
	}
}

// userResource renders one account.
//
// `id` is a STRING. SCIM ids are strings by definition, and a client that
// received a JSON number would send it back quoted on the next request — or,
// worse, round-trip it through a float.
func userResource(user scimdirectory.User) map[string]any {
	resource := map[string]any{
		"schemas":     []string{schemaUser},
		"id":          strconv.Itoa(user.ID),
		"userName":    user.UserName,
		"displayName": user.DisplayName,
		"name":        nameResource(user),
		"active":      user.Active,
		// ONE address: the account's, which is the userName. The platform
		// stores no second address, so a client that maps a different work
		// email (Entra: `mail` beside a UPN userName) reads back a value that
		// differs from what it sent. Its PATCH of the email is accepted and
		// is a no-op (patch_user.go), so the difference costs one harmless
		// request per cycle and never re-addresses the account.
		"emails": []map[string]any{{
			"value": user.UserName, "primary": true, "type": "work",
		}},
		"meta": map[string]any{
			"resourceType": "User",
			"created":      user.CreatedAt.UTC().Format(time.RFC3339),
			"lastModified": user.UpdatedAt.UTC().Format(time.RFC3339),
			"location":     BasePath + "/Users/" + strconv.Itoa(user.ID),
		},
	}
	// OMITTED when empty rather than sent as "". A client that reads back an
	// empty externalId may take it as an instruction to clear its own mapping.
	if user.ExternalID != "" {
		resource["externalId"] = user.ExternalID
	}
	return resource
}

// nameResource renders the `name` complex attribute from what was STORED.
//
// givenName and familyName are returned exactly as the client sent them, and
// omitted when it sent none. `formatted` is the stored value when the client
// sent one, else the given and family name joined (which is what Entra ID's
// default `Join(" ", givenName, surname)` mapping computes), else the display
// name. Answering with what was sent is what lets a client that compares its
// source against a GET see no difference and stop re-sending the name.
func nameResource(user scimdirectory.User) map[string]any {
	name := map[string]any{}
	if user.GivenName != "" {
		name["givenName"] = user.GivenName
	}
	if user.FamilyName != "" {
		name["familyName"] = user.FamilyName
	}
	formatted := user.FormattedName
	if formatted == "" {
		formatted = strings.TrimSpace(user.GivenName + " " + user.FamilyName)
	}
	if formatted == "" {
		formatted = user.DisplayName
	}
	if formatted != "" {
		name["formatted"] = formatted
	}
	return name
}

// groupResource renders one bound group.
//
// The member `value` is the platform account id — the same id `/Users` returns —
// because that is the identifier a client sends back on the next PATCH. The
// address is rendered as `display`, which is a label; a client that matched on
// it would break the first time somebody's address changed.
func groupResource(group scimdirectory.Group) map[string]any {
	members := make([]map[string]any, 0, len(group.Members))
	for _, member := range group.Members {
		members = append(members, map[string]any{
			"value":   strconv.Itoa(member.UserID),
			"display": member.UserName,
			"type":    "User",
			"$ref":    BasePath + "/Users/" + strconv.Itoa(member.UserID),
		})
	}
	resource := map[string]any{
		"schemas":     []string{schemaGroup, schemaProjectGrant},
		"id":          strconv.FormatInt(group.ID, 10),
		"displayName": group.DisplayName,
		"members":     members,
		schemaProjectGrant: map[string]any{
			"projectId":   group.ProjectID,
			"projectName": group.ProjectName,
			"role":        group.RoleName,
		},
		"meta": map[string]any{
			"resourceType": "Group",
			"created":      group.CreatedAt.UTC().Format(time.RFC3339),
			"lastModified": group.UpdatedAt.UTC().Format(time.RFC3339),
			"location":     BasePath + "/Groups/" + strconv.FormatInt(group.ID, 10),
		},
	}
	if group.ExternalID != "" {
		resource["externalId"] = group.ExternalID
	}
	return resource
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError renders the SCIM error shape.
//
// `scimType` is the machine-readable code from RFC 7644 §3.12 —
// `invalidFilter`, `uniqueness`, `invalidValue`, `mutability`. Identity
// providers switch on it, and an error that carries only prose makes a
// duplicate-address refusal indistinguishable from a malformed request.
func writeError(w http.ResponseWriter, status int, scimType, detail string) {
	body := map[string]any{
		"schemas": []string{schemaError},
		"status":  strconv.Itoa(status),
		"detail":  detail,
	}
	if scimType != "" {
		body["scimType"] = scimType
	}
	writeJSON(w, status, body)
}

// projectResource applies the `attributes` and `excludedAttributes` query
// parameters (RFC 7644 §3.4.2.5) to one rendered resource.
//
// Entra ID sends `excludedAttributes=members` on every group read, because a
// large group's member list is the expensive part, and a server that ignored it
// would return a body the client then has to discard. `attributes` wins when
// both are sent. `id` and `schemas` are always returned. A dotted name such as
// `name.givenName` keeps its parent attribute whole: the platform stores no
// sub-attribute that the parent does not already carry.
func projectResource(resource map[string]any, r *http.Request) map[string]any {
	query := r.URL.Query()
	if wanted := splitAttributeList(query.Get("attributes")); len(wanted) > 0 {
		kept := map[string]any{}
		for key, value := range resource {
			lowered := strings.ToLower(key)
			if lowered == "id" || lowered == "schemas" || wanted[lowered] {
				kept[key] = value
			}
		}
		return kept
	}
	if excluded := splitAttributeList(query.Get("excludedAttributes")); len(excluded) > 0 {
		for key := range resource {
			lowered := strings.ToLower(key)
			if lowered != "id" && lowered != "schemas" && excluded[lowered] {
				delete(resource, key)
			}
		}
	}
	return resource
}

// splitAttributeList reads a comma-separated attribute list into the set of
// lower-cased top-level names. A URN prefix and a sub-attribute are reduced to
// the attribute they sit under.
func splitAttributeList(raw string) map[string]bool {
	names := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" {
			continue
		}
		for _, prefix := range []string{
			strings.ToLower(schemaUser) + ":", strings.ToLower(schemaGroup) + ":",
		} {
			name = strings.TrimPrefix(name, prefix)
		}
		if dot := strings.Index(name, "."); dot > 0 && !strings.HasPrefix(name, "urn:") {
			name = name[:dot]
		}
		names[name] = true
	}
	return names
}
