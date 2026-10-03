package scim

// The `/Users` resource.
//
// Read the package comment first: it says what this tree is, how a client
// authenticates, and why there is no `/Groups`.
//
// # A DELETE deactivates, and this is the one place that says so out loud
//
// RFC 7644 defines DELETE as removing the resource. This handler suspends the
// account instead, and returns 204 as the specification requires, because
// removing the row is not a thing this platform can safely do: the account id is
// the author of every agent, prompt, conversation and schedule that person
// created, and the row is referenced from a dozen tables. Deleting it would
// cascade that work away or orphan it, and neither is what an identity provider
// means by "this person left".
//
// Suspension is what the platform's own admin surface does with a departing
// person, it revokes access immediately, and it is reversible — a re-hired
// person's account returns with their work attached. An operator who wants the
// row gone deletes it from the admin Users page, where the consequences are on
// screen.
//
// # The bounds on a request body
//
// Every write is size-bounded before it is decoded. These routes are reachable
// by an authenticated machine credential, and an unbounded decode would let one
// ask this process to buffer an arbitrary body.

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/scimdirectory"
)

// maxRequestBytes bounds one SCIM write. A user resource is well under a
// kilobyte; the bound is generous and exists so the size is stated rather than
// assumed.
const maxRequestBytes = 256 << 10

// userBody is the subset of the SCIM User resource this service stores.
//
// Fields the platform has nowhere to put — `title`, `phoneNumbers`, `addresses`,
// `manager` — are DROPPED rather than rejected. RFC 7643 §3.3 requires a service
// provider to accept a resource carrying attributes it does not support, and
// refusing them would break provisioning from every identity provider that sends
// its full default profile. What must never be dropped silently is a value this
// service DOES act on, and there are none of those outside this struct.
type userBody struct {
	Schemas     []string `json:"schemas"`
	ExternalID  string   `json:"externalId"`
	UserName    string   `json:"userName"`
	DisplayName string   `json:"displayName"`
	// Active is a POINTER so "absent" and "false" stay distinct. A PUT that
	// omitted it would otherwise suspend the account it was updating.
	Active *bool `json:"active"`
	Name   *struct {
		Formatted  string `json:"formatted"`
		GivenName  string `json:"givenName"`
		FamilyName string `json:"familyName"`
	} `json:"name"`
	Emails []struct {
		Value   string `json:"value"`
		Primary bool   `json:"primary"`
		Type    string `json:"type"`
	} `json:"emails"`
}

// resolveUserName picks the address from the body.
//
// `userName` wins. The primary email is the fallback, because Entra ID sends a
// `userName` that is a UPN and an `emails[primary]` that is the routable
// address, and several deployments' UPNs are not addresses at all. The first
// email is the last resort.
func (b userBody) resolveUserName() string {
	if name := strings.TrimSpace(b.UserName); name != "" {
		return name
	}
	for _, email := range b.Emails {
		if email.Primary && strings.TrimSpace(email.Value) != "" {
			return email.Value
		}
	}
	for _, email := range b.Emails {
		if strings.TrimSpace(email.Value) != "" {
			return email.Value
		}
	}
	return ""
}

// resolveDisplayName picks the name to show.
func (b userBody) resolveDisplayName() string {
	if name := strings.TrimSpace(b.DisplayName); name != "" {
		return name
	}
	if b.Name == nil {
		return ""
	}
	if formatted := strings.TrimSpace(b.Name.Formatted); formatted != "" {
		return formatted
	}
	return strings.TrimSpace(b.Name.GivenName + " " + b.Name.FamilyName)
}

// toUser builds the directory record. `activeDefault` decides what an omitted
// `active` means, which differs between a create and a replace.
func (b userBody) toUser(activeDefault bool) scimdirectory.User {
	active := activeDefault
	if b.Active != nil {
		active = *b.Active
	}
	user := scimdirectory.User{
		ExternalID:  strings.TrimSpace(b.ExternalID),
		UserName:    b.resolveUserName(),
		DisplayName: b.resolveDisplayName(),
		// A name composed from the parts fills an empty display name only; it
		// never overwrites one the client manages through `displayName`.
		DisplayNameDerived: strings.TrimSpace(b.DisplayName) == "",
		Active:             active,
		// Carried through so the store can tell an explicit flag from a
		// default. Create uses it to leave an operator's manual suspension
		// alone on a re-sync; see scimdirectory.Create.
		ActiveStated: b.Active != nil,
	}
	if b.Name != nil {
		user.NameStated = true
		user.GivenName = strings.TrimSpace(b.Name.GivenName)
		user.FamilyName = strings.TrimSpace(b.Name.FamilyName)
		user.FormattedName = strings.TrimSpace(b.Name.Formatted)
	}
	return user
}

/* ── read ──────────────────────────────────────────────────────────────── */

// ListUsers answers `GET /Users`.
func (h *Handler) ListUsers(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	filter, err := scimdirectory.ParseFilter(r.URL.Query().Get("filter"))
	if err != nil {
		var unsupported scimdirectory.UnsupportedFilterError
		if errors.As(err, &unsupported) {
			// `invalidFilter` is the code RFC 7644 §3.12 defines for this, and
			// the reason names the construct. A filter this directory cannot
			// represent is refused, NEVER ignored: ignoring it would answer
			// "does an account with this address exist" with somebody else.
			writeError(w, http.StatusBadRequest, "invalidFilter", unsupported.Reason)
			return
		}
		writeError(w, http.StatusBadRequest, "invalidFilter", "the filter could not be read")
		return
	}

	startIndex, count := pagination(r)
	users, total, err := h.directory.List(r.Context(), filter, startIndex, count)
	if err != nil {
		h.writeStoreFailure(w, err, "list users")
		return
	}

	resources := make([]any, 0, len(users))
	for _, user := range users {
		resources = append(resources, projectResource(userResource(user), r))
	}
	body := listResponse(resources, total, len(resources))
	body["startIndex"] = startIndex
	writeJSON(w, http.StatusOK, body)
}

// pagination reads `startIndex` and `count`, clamped.
//
// `startIndex` is ONE-BASED in SCIM. A zero or negative value is clamped to 1
// rather than refused, which is what RFC 7644 §3.4.2.4 says; treating it as an
// offset would silently skip the first account.
func pagination(r *http.Request) (startIndex, count int) {
	startIndex = 1
	if raw := r.URL.Query().Get("startIndex"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 1 {
			startIndex = parsed
		}
	}
	count = defaultPageSize
	if raw := r.URL.Query().Get("count"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			switch {
			case parsed < 0:
				count = 0
			case parsed > maxPageSize:
				count = maxPageSize
			default:
				count = parsed
			}
		}
	}
	return startIndex, count
}

// GetUser answers `GET /Users/{id}`.
func (h *Handler) GetUser(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	id, ok := userID(w, r)
	if !ok {
		return
	}
	user, err := h.directory.Get(r.Context(), id)
	if err != nil {
		h.writeStoreFailure(w, err, "read user")
		return
	}
	writeJSON(w, http.StatusOK, projectResource(userResource(user), r))
}

/* ── write ─────────────────────────────────────────────────────────────── */

// CreateUser answers `POST /Users`.
//
// An account that already carries the address is ADOPTED rather than duplicated
// — see scimdirectory.Create. The response is 201 either way, because from the
// client's side the resource now exists at the location it is given.
func (h *Handler) CreateUser(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	body, ok := decodeUser(w, r)
	if !ok {
		return
	}
	if body.resolveUserName() == "" {
		writeError(w, http.StatusBadRequest, "invalidValue",
			"userName is required, or a primary email to use as one")
		return
	}
	// A create with no `active` means active. An identity provider that pushes
	// a new joiner rarely states it, and defaulting to suspended would create
	// every account locked out.
	user, err := h.directory.Create(r.Context(), body.toUser(true))
	if err != nil {
		h.writeStoreFailure(w, err, "create user")
		return
	}
	w.Header().Set("Location", BasePath+"/Users/"+strconv.Itoa(user.ID))
	writeJSON(w, http.StatusCreated, userResource(user))
}

// ReplaceUser answers `PUT /Users/{id}`.
func (h *Handler) ReplaceUser(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	id, ok := userID(w, r)
	if !ok {
		return
	}
	body, ok := decodeUser(w, r)
	if !ok {
		return
	}
	if body.resolveUserName() == "" {
		writeError(w, http.StatusBadRequest, "invalidValue",
			"userName is required, or a primary email to use as one")
		return
	}
	// A PUT with no `active` means active, for the same reason: a replace is
	// the whole resource, and an omitted flag on a person the provider is
	// actively managing means they are there. Replace has no adoption branch,
	// so it writes `suspended` unconditionally and needs no ActiveStated.
	user, err := h.directory.Replace(r.Context(), id, body.toUser(true))
	if err != nil {
		h.writeStoreFailure(w, err, "replace user")
		return
	}
	writeJSON(w, http.StatusOK, userResource(user))
}

// DeleteUser answers `DELETE /Users/{id}`. It DEACTIVATES; see the file header.
func (h *Handler) DeleteUser(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	id, ok := userID(w, r)
	if !ok {
		return
	}
	if _, err := h.directory.SetActive(r.Context(), id, false); err != nil {
		h.writeStoreFailure(w, err, "deactivate user")
		return
	}
	// 204, as the specification requires. The body would be ignored anyway, and
	// a client that saw 200-with-a-resource would treat the delete as failed.
	w.WriteHeader(http.StatusNoContent)
}

/* ── PATCH ─────────────────────────────────────────────────────────────── */

// patchRequest is the PatchOp message.
//
// `Operations` is capitalised because RFC 7644 §3.5.2 capitalises it. Go's JSON
// decoder is case-insensitive on field names, so a client sending `operations`
// is also read — which is deliberate rather than accidental: several identity
// providers send the lower-case spelling.
type patchRequest struct {
	Schemas    []string         `json:"schemas"`
	Operations []patchOperation `json:"Operations"`
}

type patchOperation struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

// PatchUser answers `PATCH /Users/{id}`.
//
// The operations are interpreted by applyUserOperations (patch_user.go): every
// attribute the platform stores is applied, every attribute it does not is
// accepted and dropped, and the whole request is persisted once or not at all.
func (h *Handler) PatchUser(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	id, ok := userID(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	var request patchRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalidSyntax", "the patch request could not be read")
		return
	}
	if len(request.Operations) == 0 {
		writeError(w, http.StatusBadRequest, "invalidValue", "the patch request carries no operations")
		return
	}

	before, err := h.directory.Get(r.Context(), id)
	if err != nil {
		h.writeStoreFailure(w, err, "read user")
		return
	}
	patch := newUserPatch(before)
	if problem := applyUserOperations(patch, request.Operations); problem != nil {
		writeError(w, problem.status, problem.scimType, problem.detail)
		return
	}
	changes, changed := patch.changes(before)
	if !changed {
		// Every operation was understood and none of them changed anything this
		// service stores. The resource is returned unchanged, which is a true
		// answer: nothing was refused and nothing was applied.
		writeJSON(w, http.StatusOK, userResource(before))
		return
	}

	user, err := h.directory.ApplyUserChanges(r.Context(), id, changes)
	if err != nil {
		h.writeStoreFailure(w, err, "patch user")
		return
	}
	writeJSON(w, http.StatusOK, userResource(user))
}

/* ── shared ────────────────────────────────────────────────────────────── */

func userID(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil || id <= 0 {
		// 404, not 400. From the client's side an unusable id and an id that
		// names nothing are the same fact: there is no such resource.
		writeError(w, http.StatusNotFound, "", "no such user")
		return 0, false
	}
	return id, true
}

func decodeUser(w http.ResponseWriter, r *http.Request) (userBody, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	var body userBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalidSyntax", "the user resource could not be read")
		return userBody{}, false
	}
	return body, true
}

// writeStoreFailure maps a store outcome to a SCIM response.
//
// The cause is LOGGED and never returned. These responses go to an identity
// provider, and a database error naming a table and a constraint is internal
// detail crossing a trust boundary.
func (h *Handler) writeStoreFailure(w http.ResponseWriter, err error, operation string) {
	switch {
	case errors.Is(err, scimdirectory.ErrNotFound):
		writeError(w, http.StatusNotFound, "", "no such user")
	case errors.Is(err, scimdirectory.ErrProtected):
		// 403 with `mutability`: the request is well formed, and this
		// credential may not make it against this account. The reason names
		// which rule refused it, so an operator reading the identity
		// provider's log knows to act on the admin Users page instead.
		var protected *scimdirectory.ProtectedError
		detail := "this account is not managed through SCIM"
		if errors.As(err, &protected) {
			detail = protected.Reason
		}
		writeError(w, http.StatusForbidden, "mutability", detail)
	case errors.Is(err, scimdirectory.ErrConflict):
		// `uniqueness` is the code a client switches on to decide it should
		// look the existing resource up rather than retry the create.
		writeError(w, http.StatusConflict, "uniqueness",
			"another account already uses that address or external identifier")
	default:
		slog.Error("SCIM: "+operation+" failed", "err", err)
		writeError(w, http.StatusInternalServerError, "", "the directory could not be reached")
	}
}
