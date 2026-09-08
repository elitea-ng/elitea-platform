package secrets

// The GLOBAL secret vault — the store behind admin_ui's Secrets page, ported as
// unit A14 (issue #200).
//
// ## Why this is a separate handler and not a mode flag on the project one
//
// `requireMode` used to answer 501 to `administration`, reasoning that the mode
// addresses the global vault while every method on this Handler is the project
// handler keyed by `dbKey(projectID)`. That reasoning was CORRECT, and the
// evidence is unambiguous — unlike `/admin/users/{mode}/{projectID}`, where the
// equivalent 501 turned out to be a hole (pylon maps both modes of that resource
// to the same body, and the project id is in the path).
//
// Here the two modes are two different stores AND two different contracts:
//
//   - `legacy/plugins/shared/tools/secret_engines/database.py` — `Engine.db_key`
//     returns the literal string `'admin'` when `project_id is None`, and
//     `f'project-{id}'` otherwise. The row id is `admin`, not `project-None`
//     (the previous comment in handler.go said `project-None`; that came from
//     the HashiCorp engine's `VAULT_ADMINISTRATION_NAME` naming, not from the
//     database engine this deployment runs).
//   - `legacy/plugins/secrets/api/v2/{secrets,secret,hide}.py` — every `AdminAPI`
//     method constructs a bare `VaultClient()` (no project), while every
//     `ProjectAPI` method constructs `VaultClient.from_project(project_id)`. The
//     `project_id` path segment is accepted and then IGNORED by all six admin
//     methods, which is why admin_ui sends the placeholder `0`.
//   - The request and response bodies differ in every single method. Listing
//     returns `{"name","secret":"******"}` rather than `{"name","secret_name"}`;
//     reading returns `{"secret": …}` rather than a `SecretDetail`; creating
//     takes `{"secret": "<value>"}` at `POST …/secret/…/{name}` rather than
//     `{"name","value"}` at `POST …/secrets/…`; updating takes a NESTED
//     `{"secret":{"old_name","value"}}`.
//
// Confirmed against both running databases: `centry.secrets_key` holds an
// `admin` row alongside the `project-*` rows, and this repository already agrees
// — `internal/infra/storage/postgres_secret_vault.go` calls the same id
// `currentAdminVaultID = "admin"` for its shared-admin fallback.
//
// So serving `administration` from the project handler with the placeholder `0`
// really would have read and WRITTEN `project-0`: the wrong store, silently.
// The 501 was policy. This file is the handler it was pointing at.
//
// ## The global vault is shared into every project
//
// `EngineBase.get_all_secrets` merges `self.__class__().get_secrets()` — the
// GLOBAL vault — into the secrets every project resolves `{{secret.name}}`
// against. A write here is therefore platform-wide, which is why this file is
// stricter than the project handler in three places (see `adminVault`,
// `AdminCreate` and `validSecretName`).
//
// ## Deliberate divergences from the pylon original
//
//  1. **An unreadable existing vault is never overwritten.** Pylon's vault
//     client, and this handler's own project path until the guarantee was
//     hoisted, fall back to writing a fresh empty vault whenever the read fails
//     — including when the rows exist but fail to decrypt or unmarshal. That is
//     a secret wipe disguised as a first write, platform-wide here and
//     project-wide there. `readVaultByID` (handler.go) distinguishes "no rows"
//     from "rows I could not open" and only the first may be written over; both
//     vaults now go through it.
//  2. **Create does not silently overwrite.** Pylon's admin POST assigns into
//     the dict unconditionally, so creating a name that already exists destroys
//     the current value with a 200 and no warning. This returns 400.
//  3. **Names are validated.** `{{secret.<name>}}` interpolation matches
//     `[A-Za-z0-9_]+` (`EngineBase._secret_pattern`), so a name outside that
//     class can be stored but never resolved. Pylon accepts it and creates a
//     secret nothing can read; this returns 400.
//
// ## Authorisation
//
// Every route is gated on the permission its pylon counterpart declares,
// resolved from `auth_core__user_role` in `administration` mode on each request.
// The admin SPA's `window.admin_ui_config.permissions` is presentation state and
// is never consulted here.
//
// Note the grant table this reproduces is uneven, and deliberately so: on the
// reference deployment the administration-mode `editor` role holds `.list`,
// `.create`, `.edit`, `.delete` and `.unsecret` but NOT `.view` — so an editor
// may write global secrets and may not read them. That is pylon's behaviour
// (both of its admin READ methods declare `.view`), and the page surfaces the
// server's refusal rather than pretending otherwise.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"

	"github.com/go-chi/chi/v5"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
)

// adminVaultKey is the `centry.secrets_key` / `centry.secrets_data` row id of
// the global vault. See this file's header for the three independent sources.
const adminVaultKey = "admin"

// The permissions pylon's `AdminAPI` methods declare, one per operation.
const (
	permSecretView   = "configuration.secrets.secret.view"
	permSecretCreate = "configuration.secrets.secret.create"
	permSecretEdit   = "configuration.secrets.secret.edit"
	permSecretDelete = "configuration.secrets.secret.delete"
)

// validSecretName mirrors `EngineBase._secret_pattern`: the character class
// `{{secret.<name>}}` interpolation can actually resolve.
var validSecretName = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

/* ── wire shapes (pylon's, exactly) ───────────────────────────────────────── */

// adminSecretListItem is one row of `GET …/secrets/administration/{projectID}`.
// The value is always the literal mask — the listing never carries plaintext,
// and the page fetches a single value on demand through AdminGet.
type adminSecretListItem struct {
	Name   string `json:"name"`
	Secret string `json:"secret"`
}

// adminSecretMask is what pylon puts in every listing row's `secret` field.
const adminSecretMask = "******"

// adminSecretUpdateBody is the NESTED body pylon's admin PUT takes. `OldName`
// selects the entry to replace; the new name comes from the URL, which is what
// makes a rename expressible.
type adminSecretUpdateBody struct {
	Secret struct {
		OldName string `json:"old_name"`
		Value   string `json:"value"`
	} `json:"secret"`
}

/* ── permission gate ──────────────────────────────────────────────────────── */

// adminGate wraps an admin-mode handler in the central permission check.
//
// It is applied here rather than in `router.go` because the mode that selects
// this handler is a PATH SEGMENT resolved at request time: one chi route serves
// both modes, so route-level middleware could not gate one and not the other.
//
// Fail-closed by construction — `RequireCentralPermissions` answers 403 when the
// resolver is nil, so a Handler built without one (the programmatic constructors
// in applications/ and conversations/, which never serve HTTP) exposes nothing.
func (h *Handler) adminGate(permission string, next http.HandlerFunc) http.HandlerFunc {
	gated := apimw.RequireCentralPermissions(
		h.permissionResolver,
		modeAdministration,
		permission,
	)(next)
	return gated.ServeHTTP
}

/* ── handler methods ──────────────────────────────────────────────────────── */

// AdminList answers `GET /secrets/secrets/administration/{projectID}` with every
// global secret NAME, each masked. Pylon returns `[]` shaped exactly this way.
//
// A vault that does not exist yet is an empty list, not an error: the deployment
// simply has no global secrets, and 500ing would make a fresh install look
// broken.
func (h *Handler) AdminList(w http.ResponseWriter, r *http.Request) {
	vault, err := h.adminVault(r.Context())
	if errors.Is(err, ErrVaultAbsent) {
		writeJSON(w, http.StatusOK, []adminSecretListItem{})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "global vault is unreadable"})
		return
	}
	items := make([]adminSecretListItem, 0, len(vault.Secrets))
	for name := range vault.Secrets {
		items = append(items, adminSecretListItem{Name: name, Secret: adminSecretMask})
	}
	sortByName(items)
	writeJSON(w, http.StatusOK, items)
}

// AdminGet answers `GET /secrets/secret/administration/{projectID}/{name}` with
// `{"secret": "<value>"}`, or `{"secret": null}` when the name is unknown —
// pylon's `secrets.get(secret)`, 200 either way.
//
// Hidden secrets are NOT consulted, matching pylon (its admin GET has the
// hidden-secret lookup commented out, and its hide endpoint refuses outright —
// the global vault has no hidden section in practice).
func (h *Handler) AdminGet(w http.ResponseWriter, r *http.Request) {
	name, ok := adminSecretNameParam(w, r)
	if !ok {
		return
	}
	vault, err := h.adminVault(r.Context())
	if errors.Is(err, ErrVaultAbsent) {
		writeJSON(w, http.StatusOK, map[string]*string{"secret": nil})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "global vault is unreadable"})
		return
	}
	if value, exists := vault.Secrets[name]; exists {
		writeJSON(w, http.StatusOK, map[string]*string{"secret": &value})
		return
	}
	writeJSON(w, http.StatusOK, map[string]*string{"secret": nil})
}

// AdminCreate answers `POST /secrets/secret/administration/{projectID}/{name}`
// with body `{"secret": "<value>"}`.
//
// Unlike pylon it refuses to overwrite an existing name — see divergence 2 in
// this file's header.
func (h *Handler) AdminCreate(w http.ResponseWriter, r *http.Request) {
	name, ok := adminSecretNameParam(w, r)
	if !ok {
		return
	}
	if !validSecretName.MatchString(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"message": "secret name must contain only letters, digits and underscores",
		})
		return
	}
	var body struct {
		Secret *string `json:"secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Secret == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "secret value is required"})
		return
	}

	err := h.mutateAdminVault(r.Context(), true, func(vault *vaultData) (bool, error) {
		if _, exists := vault.Secrets[name]; exists {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"message": fmt.Sprintf("Secret %q already exists", name),
			})
			return false, errMutationRefused
		}
		vault.Secrets[name] = *body.Secret
		return true, nil
	})
	switch {
	case errors.Is(err, errMutationRefused):
		return
	case errors.Is(err, errVaultWrite):
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "failed to save the secret"})
		return
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "global vault is unreadable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Project secret was saved"})
}

// AdminUpdate answers `PUT /secrets/secret/administration/{projectID}/{name}`
// with body `{"secret":{"old_name":"…","value":"…"}}`.
//
// `old_name` names the entry being replaced and `{name}` is what it becomes, so
// a rename is `old_name != name` and a plain value change is `old_name == name`
// (which is all admin_ui sends). Pylon 404s when `old_name` is unknown.
func (h *Handler) AdminUpdate(w http.ResponseWriter, r *http.Request) {
	name, ok := adminSecretNameParam(w, r)
	if !ok {
		return
	}
	if !validSecretName.MatchString(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"message": "secret name must contain only letters, digits and underscores",
		})
		return
	}
	var body adminSecretUpdateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid request body"})
		return
	}
	oldName := body.Secret.OldName
	if oldName == "" {
		oldName = name
	}

	err := h.mutateAdminVault(r.Context(), false, func(vault *vaultData) (bool, error) {
		if _, exists := vault.Secrets[oldName]; !exists {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Project secret was not found"})
			return false, errMutationRefused
		}
		// A rename onto an occupied name would silently destroy that entry.
		if oldName != name {
			if _, occupied := vault.Secrets[name]; occupied {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"message": fmt.Sprintf("Secret %q already exists", name),
				})
				return false, errMutationRefused
			}
		}
		delete(vault.Secrets, oldName)
		vault.Secrets[name] = body.Secret.Value
		return true, nil
	})
	switch {
	case errors.Is(err, errMutationRefused):
		return
	case errors.Is(err, ErrVaultAbsent):
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Project secret was not found"})
		return
	case errors.Is(err, errVaultWrite):
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "failed to save the secret"})
		return
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "global vault is unreadable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Project secret was updated"})
}

// AdminDelete answers `DELETE /secrets/secret/administration/{projectID}/{name}`
// with 204. Deleting an unknown name is a no-op success, as in pylon.
func (h *Handler) AdminDelete(w http.ResponseWriter, r *http.Request) {
	name, ok := adminSecretNameParam(w, r)
	if !ok {
		return
	}
	err := h.mutateAdminVault(r.Context(), false, func(vault *vaultData) (bool, error) {
		if _, exists := vault.Secrets[name]; !exists {
			return false, nil
		}
		delete(vault.Secrets, name)
		return true, nil
	})
	switch {
	case errors.Is(err, ErrVaultAbsent):
		w.WriteHeader(http.StatusNoContent)
		return
	case errors.Is(err, errVaultWrite):
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "failed to delete the secret"})
		return
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "global vault is unreadable"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AdminHide answers `POST /secrets/hide/administration/{projectID}/{name}`.
//
// Pylon refuses it with 401 and that sentence — the global vault has no hidden
// section — so the route exists to say so rather than 404ing as an unknown path.
// The admin page renders no hide control at all; this is the contract, kept
// complete because elitea-sdk and qa/ speak to the same surface.
func (h *Handler) AdminHide(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusUnauthorized, map[string]string{
		"message": "There are no hidden secrets in administration mode",
	})
}

/* ── small helpers ────────────────────────────────────────────────────────── */

// adminSecretNameParam reads `{name}` and rejects an empty one.
//
// No unescaping is applied. Pylon calls `unquote()` here because its route
// accepts any string; this surface only ever stores names matching
// `[A-Za-z0-9_]+` (see validSecretName), which are their own percent-encoding,
// so a second decode could only turn a legitimate literal into something else.
func adminSecretNameParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := chi.URLParam(r, "name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "secret name is required"})
		return "", false
	}
	return name, true
}

// sortByName gives the listing a stable order. Go map iteration is randomised,
// so without this the page's rows would reshuffle on every poll — and a test
// asserting order would pass or fail by luck.
func sortByName(items []adminSecretListItem) {
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
}

/* ── vault access ─────────────────────────────────────────────────────────── */

// The three functions below are the global vault's names for the shared
// id-keyed layer in handler.go — `readVaultByID`, `mutateVaultByID` and
// `writeVaultByID`, called with `adminVaultKey`. They were this file's own
// implementations until the project path needed the same guarantees; the
// contract they carry is documented there, and it is now ONE contract rather
// than two implementations of it that could drift apart.

// adminVault reads and decrypts the global vault. It returns ErrVaultAbsent
// ONLY when neither row exists; every other failure is returned as itself, so
// no caller can mistake "I could not open this" for "there is nothing here"
// and write over it.
func (h *Handler) adminVault(ctx context.Context) (vaultData, error) {
	return h.readVaultByID(ctx, adminVaultKey)
}

// mutateAdminVault is the global vault's locked read-modify-write. With
// `init`, an ABSENT vault becomes an empty one, ready to be written. An
// unreadable vault still fails.
func (h *Handler) mutateAdminVault(ctx context.Context, init bool, mutate func(v *vaultData) (bool, error)) error {
	return h.mutateVaultByID(ctx, adminVaultKey, init, mutate)
}

// writeAdminVault replaces the global vault wholesale — a test seed, see
// writeVaultByID.
func (h *Handler) writeAdminVault(ctx context.Context, v vaultData) error {
	return h.writeVaultByID(ctx, adminVaultKey, v)
}
