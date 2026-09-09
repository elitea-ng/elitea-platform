package secrets

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// Handler serves the secrets API, backed by the same centry.secrets_key /
// centry.secrets_data tables that the Python pylon secrets plugin uses.
//
// Encryption scheme (Python cryptography.fernet.Fernet):
//
//	32-byte key   = <first-16 bytes: HMAC-SHA256 signing key>
//	                <last-16 bytes:  AES-128-CBC encryption key>
//	Token layout  = base64url( version[1] | timestamp[8] | iv[16] |
//	                            ciphertext[N] | hmac[32] )
//
// The project-level key is itself stored encrypted with a master key
// (SECRETS_MASTER_KEY env var, base64url-encoded 32-byte Fernet key).
type Handler struct {
	pool      *pgxpool.Pool
	masterKey []byte // nil when SECRETS_MASTER_KEY is absent
	// masterKeyErr is set when SECRETS_MASTER_KEY is present and malformed.
	// Every vault read and every vault write then fails with it, so the
	// handler stores NOTHING rather than storing project keys unwrapped
	// (#412). See MasterKeyFromEnv for why the two cases differ.
	masterKeyErr error
	// permissionResolver authorises BOTH mode families: the `administration`
	// routes (admin.go, central mode) and the project routes below (`default`
	// mode, keyed on the `{projectID}` in the path). nil for the two
	// programmatic constructors that never serve HTTP, which is safe: both
	// gates fail closed on a nil resolver.
	permissionResolver  auth.PermissionResolver
	defaultSecretPolicy func(context.Context) (defaultSecretPolicy, error)
}

// Option configures a Handler. Same shape as the other v2 packages'.
type Option func(*Handler)

// WithPermissionResolver supplies the resolver EVERY route is gated on — the
// `administration`-mode ones in admin.go and the project ones in Routes().
// Without it every one of them answers 403.
func WithPermissionResolver(resolver auth.PermissionResolver) Option {
	return func(h *Handler) { h.permissionResolver = resolver }
}

// MasterKeyEnvVar names the one variable that decides whether a project
// vault's key row is wrapped. Every message this package writes about the
// master key names it, so an operator can search the logs for it.
const MasterKeyEnvVar = "SECRETS_MASTER_KEY"

// MasterKeyFromEnv reads the master key and validates it.
//
// It separates the two cases the old NewHandler answered identically:
//
//   - ABSENT: key is nil and err is nil. The deployment asks for unwrapped
//     storage. That stays supported, because centry supports it and because a
//     local stack has no key to give. The caller must say so out loud —
//     cmd/elitea-main logs a warning that names the consequence.
//   - MALFORMED: err is not nil. The deployment asks for wrapped storage and
//     cannot get it. A wrong length, bad base64, or a stray space or tab from a
//     mounted secret is an operator error. It is never an instruction to
//     downgrade the storage.
//
// The old code read the second case as the first. It left masterKey nil, and
// the handler then minted and wrote UNWRAPPED vaults while the operator
// believed the keys were wrapped. Nothing logged and nothing failed, because
// every later read of an unwrapped vault also succeeds (#412).
//
// WHAT COUNTS AS MALFORMED IS UNCHANGED. This function validates with
// fernetDecodeKey, exactly as the old code did, and adds no whitespace rule of
// its own. That matters for one shape in particular: Go's base64 decoder
// IGNORES "\r" and "\n", so a key read from a file with a trailing newline
// decodes to the same 32 bytes and keeps working. Python's
// base64.urlsafe_b64decode ignores it too, so pylon's secrets engine agrees.
// Rejecting a newline here would stop a deployment that works today, which is
// the opposite of the repair. A space or a tab is a different matter: neither
// decoder pair agrees on it, so it stays an error.
//
// getenv is injected so a test can supply a value without t.Setenv, which
// forbids t.Parallel.
func MasterKeyFromEnv(getenv func(string) string) ([]byte, error) {
	value := getenv(MasterKeyEnvVar)
	if value == "" {
		return nil, nil
	}
	raw, err := fernetDecodeKey(value)
	if err != nil {
		return nil, fmt.Errorf(
			"%s is set and malformed: %w; supply a base64url-encoded 32-byte Fernet key "+
				"with no stray spaces or tabs, or remove the variable to store project "+
				"vault keys unwrapped", MasterKeyEnvVar, err)
	}
	return raw, nil
}

// NewHandler constructs the secrets handler.  The pool is used for
// centry.secrets_key / centry.secrets_data reads and writes.
//
// A malformed SECRETS_MASTER_KEY does not stop construction, because this
// constructor cannot report an error: two of its four callers build a handler
// per request, so an error here would surface long after provisioning had
// already written vaults. cmd/elitea-main validates the variable at start-up
// instead, and stops. This constructor keeps the fault so that a handler built
// WITHOUT that gate still fails closed (#412).
func NewHandler(pool *pgxpool.Pool, opts ...Option) *Handler {
	h := &Handler{pool: pool}
	h.masterKey, h.masterKeyErr = MasterKeyFromEnv(os.Getenv)
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Legacy pylon API modes (legacy/plugins/shared/tools/config.py:40-41).
// `mode` is a real path segment that SELECTS THE HANDLER in pylon
// (api_tools.APIBase.proxy_method looks it up in mode_handlers and
// abort(404)s on a miss), not decoration.
const (
	// modeDefault is pylon's c.DEFAULT_MODE: the project-scoped vault
	// (VaultClient.from_project(project_id)).  It is also what pylon uses
	// when the segment is omitted entirely — proxy_method's `mode` kwarg
	// defaults to "default", and api_tools.with_modes registers both the
	// mode-ful and the mode-less URL for every resource.
	modeDefault = "default"
	// modeAdministration is pylon's c.ADMINISTRATION_MODE: a DIFFERENT
	// handler over the GLOBAL vault (a bare VaultClient(), project_id nil
	// → row id "admin"), with different request/response shapes.  Unit A14
	// implements it, in admin.go — see that file's header for why it had to
	// be a separate handler rather than a flag on this one, and where the
	// "admin" row id is established.  (Earlier revisions of this comment
	// said the row id was "project-None"; that is the HashiCorp engine's
	// naming, not the database engine this deployment runs.)
	modeAdministration = "administration"
)

// Routes returns the secrets subrouter.  It is Mount()ed at "/secrets" by
// internal/api/router.go, which reproduces the pylon URL shape exactly:
//
//	/api/v2/<plugin>/<resource-module>/<mode>/<params>
//
// The plugin is `secrets` (the mount prefix) and the resource modules are
// legacy/plugins/secrets/api/v2/{secrets,secret,hide}.py, so the served
// paths are /api/v2/secrets/{secrets,secret,hide}/…  The doubled "secrets"
// is the REAL legacy shape, not the double-mount bug #137 took it for: the
// pinned baseline client agrees (apps/elitea-ui/src/api/secrets.js:3 sets
// apiSlicePath = '/secrets' and appends '/secrets/default/<id>'), and so do
// elitea-sdk (runtime/clients/{client,sandbox_client}.py), admin_ui
// (frontend/src/api/secretsApi.js) and qa/elitea-api-testing
// (utils/utils.py:322).  #137 moved these routes to the v2 root and broke
// all four; #151 restores them and moves the new client onto this shape.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	// Each route serves BOTH pylon modes: the first handler is the project
	// vault, the second the global vault (admin.go).  They are separate
	// handlers over separate stores with separate bodies — see withModes.
	//
	// GET  /secrets/{mode}/{projectID}            – list secret names
	r.Get("/secrets/{mode}/{projectID}", h.withModes(
		h.projectGate(permSecretList, h.List),
		h.adminGate(permSecretView, h.AdminList)))
	// POST /secrets/{mode}/{projectID}            – create a new secret
	//
	// The administration form of this ONE route is not implemented.  Pylon's
	// `AdminAPI.post` takes `{"secrets": {…}}` and REPLACES the entire global
	// vault in a single call; no client in this workspace calls it (admin_ui
	// creates through /secret/…/{name}, and elitea-sdk and qa/ never touch
	// administration mode), so it is a bulk-destructive operation with no
	// caller and no test that could discriminate a correct implementation
	// from a wrong one.  501 says so instead of guessing.
	r.Post("/secrets/{mode}/{projectID}", h.withModes(
		h.projectGate(permSecretCreate, h.Create), notImplementedBulkReplace))
	// GET  /secret/{mode}/{projectID}/{name}      – get a single secret (with value)
	r.Get("/secret/{mode}/{projectID}/{name}", h.withModes(
		h.projectGate(permSecretUnsecret, h.Get),
		h.adminGate(permSecretView, h.AdminGet)))
	// POST /secret/{mode}/{projectID}/{name}      – administration-mode create
	//
	// Project mode has no POST on this path (pylon's ProjectAPI defines only
	// get/put/delete here), so it 405s rather than pretending.
	r.Post("/secret/{mode}/{projectID}/{name}", h.withModes(methodNotAllowed,
		h.adminGate(permSecretCreate, h.AdminCreate)))
	// PUT  /secret/{mode}/{projectID}/{name}      – rename / update a secret
	r.Put("/secret/{mode}/{projectID}/{name}", h.withModes(
		h.projectGate(permSecretEdit, h.Update),
		h.adminGate(permSecretEdit, h.AdminUpdate)))
	// DELETE /secret/{mode}/{projectID}/{name}    – delete a secret
	r.Delete("/secret/{mode}/{projectID}/{name}", h.withModes(
		h.projectGate(permSecretDelete, h.Delete),
		h.adminGate(permSecretDelete, h.AdminDelete)))
	// POST /hide/{mode}/{projectID}/{name}        – move secret to hidden_secrets
	r.Post("/hide/{mode}/{projectID}/{name}", h.withModes(
		h.projectGate(permSecretHide, h.Hide),
		h.adminGate(permSecretEdit, h.AdminHide)))

	// The mode-LESS form of the show route, which pylon also serves
	// (with_modes registers `<project_id>/<secret>` alongside
	// `<mode>/<project_id>/<secret>`) and which elitea-sdk is the sole
	// caller of: elitea_sdk/runtime/clients/client.py:108 and
	// sandbox_client.py:237 build
	// {api_v2}/secrets/secret/{project_id} and append /{name}.
	// Only this one variant is registered: pylon serves the mode-less form
	// of every route, but no consumer in the workspace calls any of the
	// others, and a route with no caller is a route no test can discriminate.
	//
	// It carries the SAME gate as the mode-ful GET, because in pylon it IS
	// the mode-ful GET. `api_tools.with_modes` registers the mode-less URL
	// against the same `APIBase`, and `proxy_method`'s signature is
	// `def proxy_method(self, method, mode='default', **kwargs)` — so a
	// request with no mode segment dispatches to `DEFAULT_MODE` →
	// `ProjectAPI.get`, which declares
	// `configuration.secrets.secret.unsecret` (secret.py:26). The runtime is
	// therefore already passing this exact check against pylon in production;
	// gating it here is parity, not a new restriction.
	//
	// The principal is not a permission-less service identity: both SDK
	// clients send `Authorization: Bearer <auth_token>` of the invoking user
	// (runtime/clients/client.py:92, sandbox_client.py:228), and
	// legacyrbac.PostgresResolver resolves a token to its OWNING USER and
	// then that user's project roles. The `X-SECRET` header those clients
	// also send is not an auth credential — pylon compares it to the
	// `secrets_header_value` secret purely to decide whether to suppress
	// default secret keys (`ignore_default_secret_api`), and it grants
	// nothing.
	r.Get("/secret/{projectID}/{name}", h.projectGate(permSecretUnsecret, h.Get))
	return r
}

// The permissions pylon's `ProjectAPI` methods declare that its `AdminAPI`
// ones do not (legacy/plugins/secrets/api/v2/{secrets,secret,hide}.py). The
// three the two families share — create, edit, delete — are declared once in
// admin.go and reused here; the values are the same strings in both modes.
const (
	permSecretList     = "configuration.secrets.secret.list"
	permSecretUnsecret = "configuration.secrets.secret.unsecret"
	permSecretHide     = "configuration.secrets.secret.hide"
)

// projectGate wraps a project-mode handler in the central permission check,
// resolved against the `{projectID}` in the path in `default` mode.
//
// It is applied here rather than in `router.go` because the mode that selects
// this handler is a PATH SEGMENT resolved at request time: one chi route serves
// both pylon modes, so route-level middleware could not gate one and not the
// other.
//
// It is passed as the `project` branch of withModes rather than wrapped around
// it, so each mode keeps its OWN gate: `administration` resolves centrally via
// adminGate (it ignores the {projectID} entirely), and an unknown mode stays a
// 404 rather than becoming a 403 that says nothing about why.
//
// Fail-closed by construction — RequireResolvedPermissionsForProject answers
// 403 when the resolver is nil, so a Handler built without one (the
// programmatic constructors in applications/ and conversations/, which never
// serve HTTP) exposes nothing.
func (h *Handler) projectGate(permission string, next http.HandlerFunc) http.HandlerFunc {
	gated := apimw.RequireResolvedPermissionsForProject(
		h.permissionResolver,
		auth.PermissionModeDefault,
		func(r *http.Request) (string, bool) {
			projectID := chi.URLParam(r, "projectID")
			return projectID, projectID != ""
		},
		permission,
	)(next)
	return gated.ServeHTTP
}

// withModes reproduces pylon's mode dispatch for the routes that carry a
// {mode} segment.  Anything other than the two modes pylon defines is a 404,
// exactly as APIBase.proxy_method's `abort(404)` on an unknown mode — which
// is what makes the third convention the new client had invented
// (`prompt_lib`, #151) a hard error rather than a silently-accepted alias.
//
// The two branches are genuinely different handlers, not one handler with a
// flag.  `project` is keyed by dbKey(projectID); `administration` addresses the
// GLOBAL vault (row id "admin") with its own request and response bodies, and
// IGNORES the {projectID} segment entirely — which is why admin_ui sends the
// placeholder `0` there.  Routing `administration` into the project handler
// would read and WRITE project 0's vault: the wrong store, silently.  That is
// what the 501 this replaced was protecting against; unit A14 implements the
// real second handler in admin.go instead.
func (h *Handler) withModes(project, administration http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch chi.URLParam(r, "mode") {
		case modeDefault:
			project(w, r)
		case modeAdministration:
			administration(w, r)
		default:
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown mode"})
		}
	}
}

// notImplementedBulkReplace is the administration branch of
// `POST /secrets/{mode}/{projectID}` — see the route comment for why it is not
// built.
func notImplementedBulkReplace(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{
		"error": "bulk replacement of the global vault is not implemented; " +
			"create secrets one at a time through POST /secret/administration/{projectID}/{name}",
	})
}

// methodNotAllowed is the project branch of routes pylon defines for the
// administration mode only.
func methodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed for this mode"})
}

// ─── response models ─────────────────────────────────────────────────────────

// SecretListItem mirrors the Python SecretList pydantic model returned by
// the pylon secrets plugin for list responses.
type SecretListItem struct {
	Name       string `json:"name"`
	SecretName string `json:"secret_name"` // {{secret.<name>}}
	IsDefault  bool   `json:"is_default"`
}

// SecretDetail mirrors the Python SecretDetail pydantic model.
type SecretDetail struct {
	Name       string `json:"name"`
	SecretName string `json:"secret_name"`
	IsDefault  bool   `json:"is_default"`
	IsHidden   bool   `json:"is_hidden"`
	Value      string `json:"value"`
}

// ─── vault data layout ────────────────────────────────────────────────────────

// vaultData is the JSON stored (after Fernet encryption) in centry.secrets_data.
type vaultData struct {
	Secrets       map[string]string `json:"secrets"`
	HiddenSecrets map[string]string `json:"hidden_secrets"`
}

func dbKey(projectID string) string {
	return fmt.Sprintf("project-%s", projectID)
}

// ─── handler methods ──────────────────────────────────────────────────────────

// List returns the names of all (non-hidden) secrets for a project.
// Response format: JSON array of SecretListItem (same as Python plugin).
//
// A project with no vault is an empty list: it simply has no secrets, and
// 500ing would make every new project look broken.  A vault that EXISTS and
// will not open is a 500 — it used to be an empty list too, which showed the
// page "no secrets" for a project whose secrets were all still there, and
// invited the create that would then have replaced them.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	policy, policyOK := h.readDefaultSecretPolicy(w, r)
	if !policyOK {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	vault, err := h.readVaultCtx(r.Context(), projectID)
	if errors.Is(err, ErrVaultAbsent) {
		writeJSON(w, http.StatusOK, []SecretListItem{})
		return
	}
	if err != nil {
		vaultUnreadable(w)
		return
	}
	items := make([]SecretListItem, 0, len(vault.Secrets))
	for name := range vault.Secrets {
		if policy.isDefault(name) && policy.suppressed(vault, r) {
			continue
		}
		items = append(items, SecretListItem{
			Name:       name,
			IsDefault:  policy.isDefault(name),
			SecretName: fmt.Sprintf("{{secret.%s}}", name),
		})
	}
	writeJSON(w, http.StatusOK, items)
}

// secretNameTaken reports whether a project vault already holds `name`, in
// either map.
//
// Both maps matter. `{{secret.<name>}}` resolves one namespace, so a name that
// is present in `secrets` and in `hidden_secrets` at the same time is an
// ambiguous vault: Get returns the visible value and hides the other, and
// Delete then removes both.
func secretNameTaken(vault vaultData, name string) bool {
	if _, visible := vault.Secrets[name]; visible {
		return true
	}
	_, hidden := vault.HiddenSecrets[name]
	return hidden
}

// maxSecretNameLength is the bound the vault WRITER applies
// (infra/centrysecrets/mutate.go validSecretName). The API must agree with it,
// or the API accepts a name the writer refuses.
const maxSecretNameLength = 128

// invalidSecretNameMessage is the refusal the administration-mode routes give
// for the same name (admin.go). One rule, one message.
const invalidSecretNameMessage = "secret name must contain only letters, digits and underscores"

// acceptableSecretName reports whether `{{secret.<name>}}` can resolve to this
// name, and whether the vault writer accepts it.
func acceptableSecretName(name string) bool {
	return len(name) <= maxSecretNameLength && validSecretName.MatchString(name)
}

// Create adds a new secret.  Body: {"name": "...", "value": "..."}.
// Response: SecretListItem (201).
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	policy, policyOK := h.readDefaultSecretPolicy(w, r)
	if !policyOK {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	var body struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		apierr.WriteStatus(w, http.StatusBadRequest, "name is required")
		return
	}
	// A name outside this class can be STORED and can never be RESOLVED. The
	// 201 below hands the user `{{secret.<name>}}` as if it could be. The
	// expander matches [A-Za-z0-9_]+ (infra/storage/expansion_unsecreter.go).
	// It leaves a name it does not match as the literal placeholder. The
	// toolkit then sends `{{secret.openai-api-key}}` to the provider as the
	// API key. The administration-mode routes already refuse the same name
	// (admin.go AdminCreate).
	if !acceptableSecretName(body.Name) {
		apierr.WriteStatus(w, http.StatusBadRequest, invalidSecretNameMessage)
		return
	}

	// An absent vault is initialised here; an UNREADABLE one is refused.  The
	// fallback this replaced wrote a fresh empty vault on any read failure, so
	// one create against a vault that would not decrypt replaced every secret
	// in it and answered 201.
	err := h.mutateVaultCtx(r.Context(), projectID, true, func(vault *vaultData) (bool, error) {
		if policy.refuse(w, r, *vault, body.Name) {
			return false, errMutationRefused
		}
		// The hidden map is checked too. A name that lives in hidden_secrets is
		// taken: writing it into `secrets` as well puts one name in both maps,
		// and Get then returns the visible value and shadows the hidden one.
		if secretNameTaken(*vault, body.Name) {
			apierr.WriteStatus(w, http.StatusBadRequest, fmt.Sprintf("Secret %q already exists", body.Name))
			return false, errMutationRefused
		}
		vault.Secrets[body.Name] = body.Value
		return true, nil
	})
	switch {
	case errors.Is(err, errMutationRefused):
		return
	case errors.Is(err, errVaultWrite):
		vaultSaveFailed(w, "failed to save the secret")
		return
	case err != nil:
		vaultUnreadable(w)
		return
	}
	writeJSON(w, http.StatusCreated, SecretListItem{
		Name:       body.Name,
		IsDefault:  policy.isDefault(body.Name),
		SecretName: fmt.Sprintf("{{secret.%s}}", body.Name),
	})
}

// Get returns a single secret including its plaintext value.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	policy, policyOK := h.readDefaultSecretPolicy(w, r)
	if !policyOK {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	name := chi.URLParam(r, "name")

	vault, err := h.readVaultCtx(r.Context(), projectID)
	if errors.Is(err, ErrVaultAbsent) {
		apierr.WriteStatus(w, http.StatusNotFound, "secret not found")
		return
	}
	if err != nil {
		vaultUnreadable(w)
		return
	}

	if policy.refuse(w, r, vault, name) {
		return
	}
	if policy.suppressed(vault, r) {
		writeJSON(w, http.StatusOK, SecretDetail{Name: name, SecretName: fmt.Sprintf("{{secret.%s}}", name)})
		return
	}
	if val, ok := vault.Secrets[name]; ok {
		writeJSON(w, http.StatusOK, SecretDetail{
			Name:       name,
			IsDefault:  policy.isDefault(name),
			SecretName: fmt.Sprintf("{{secret.%s}}", name),
			Value:      val,
		})
		return
	}
	if val, ok := vault.HiddenSecrets[name]; ok {
		writeJSON(w, http.StatusOK, SecretDetail{
			Name:       name,
			IsDefault:  policy.isDefault(name),
			SecretName: fmt.Sprintf("{{secret.%s}}", name),
			Value:      val,
			IsHidden:   true,
		})
		return
	}
	apierr.WriteStatus(w, http.StatusNotFound, "secret not found")
}

// Update renames and/or changes the value of an existing secret.
// Body: {"name": "<new_name>", "value": "<new_value>"}.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	policy, policyOK := h.readDefaultSecretPolicy(w, r)
	if !policyOK {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	oldName := chi.URLParam(r, "name")

	var body struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Name == "" {
		body.Name = oldName
	}
	// Validate ONLY a name that changes. A vault written before this rule can
	// hold a name outside the class, and the SDK route reads such a secret by
	// its exact name. A value-only edit of that secret must keep working, so
	// the check covers a rename and a create, never an unchanged name.
	if body.Name != oldName && !acceptableSecretName(body.Name) {
		apierr.WriteStatus(w, http.StatusBadRequest, invalidSecretNameMessage)
		return
	}

	err := h.mutateVaultCtx(r.Context(), projectID, false, func(vault *vaultData) (bool, error) {
		if policy.refuse(w, r, *vault, oldName, body.Name) {
			return false, errMutationRefused
		}
		if _, ok := vault.Secrets[oldName]; !ok {
			apierr.WriteStatus(w, http.StatusBadRequest, fmt.Sprintf("secret %q not found", oldName))
			return false, errMutationRefused
		}
		// A rename onto an occupied name would silently destroy that entry.
		// The vault is one encrypted blob with no history, so the overwritten
		// value is unrecoverable. The administration-mode sibling AdminUpdate
		// has always refused this; the project-mode route did not, and
		// answered 200.
		if body.Name != oldName && secretNameTaken(*vault, body.Name) {
			apierr.WriteStatus(w, http.StatusBadRequest, fmt.Sprintf("Secret %q already exists", body.Name))
			return false, errMutationRefused
		}
		delete(vault.Secrets, oldName)
		vault.Secrets[body.Name] = body.Value
		return true, nil
	})
	switch {
	case errors.Is(err, errMutationRefused):
		return
	case errors.Is(err, ErrVaultAbsent):
		apierr.WriteStatus(w, http.StatusBadRequest, fmt.Sprintf("secret %q not found", oldName))
		return
	case errors.Is(err, errVaultWrite):
		vaultSaveFailed(w, "failed to save the secret")
		return
	case err != nil:
		vaultUnreadable(w)
		return
	}
	writeJSON(w, http.StatusOK, SecretListItem{
		Name:       body.Name,
		IsDefault:  policy.isDefault(body.Name),
		SecretName: fmt.Sprintf("{{secret.%s}}", body.Name),
	})
}

// Delete removes a secret by name (from either secrets or hidden_secrets).
// Deleting from a project that has no vault is a no-op success, as in pylon.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	policy, policyOK := h.readDefaultSecretPolicy(w, r)
	if !policyOK {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	name := chi.URLParam(r, "name")

	err := h.mutateVaultCtx(r.Context(), projectID, false, func(vault *vaultData) (bool, error) {
		if policy.refuse(w, r, *vault, name) {
			return false, errMutationRefused
		}
		delete(vault.Secrets, name)
		delete(vault.HiddenSecrets, name)
		return true, nil
	})
	switch {
	case errors.Is(err, errMutationRefused):
		return
	case errors.Is(err, ErrVaultAbsent):
		w.WriteHeader(http.StatusNoContent)
		return
	case errors.Is(err, errVaultWrite):
		// The write error was swallowed here, so a delete that did not persist
		// still answered 204 and the page removed the row it had just re-listed.
		vaultSaveFailed(w, "failed to delete the secret")
		return
	case err != nil:
		vaultUnreadable(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Hide moves a secret from secrets → hidden_secrets.
func (h *Handler) Hide(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	name := chi.URLParam(r, "name")

	err := h.mutateVaultCtx(r.Context(), projectID, false, func(vault *vaultData) (bool, error) {
		val, ok := vault.Secrets[name]
		if !ok {
			apierr.WriteStatus(w, http.StatusBadRequest, fmt.Sprintf("secret %q not found", name))
			return false, errMutationRefused
		}
		delete(vault.Secrets, name)
		vault.HiddenSecrets[name] = val
		return true, nil
	})
	switch {
	case errors.Is(err, errMutationRefused):
		return
	case errors.Is(err, ErrVaultAbsent):
		apierr.WriteStatus(w, http.StatusBadRequest, fmt.Sprintf("secret %q not found", name))
		return
	case errors.Is(err, errVaultWrite):
		vaultSaveFailed(w, "failed to hide the secret")
		return
	case err != nil:
		vaultUnreadable(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Project secret was moved to hidden secrets"})
}

// ─── vault read / write ───────────────────────────────────────────────────────
//
// One vault is one `centry.secrets_key` row plus the `centry.secrets_data` row
// with the same id.  The three functions below are keyed by that id and know
// nothing else about the vault, so the project store (`project-<id>`) and the
// global store (`admin`, admin.go) go through the SAME code.  They used to be
// two implementations with two different error contracts, and only the global
// one distinguished "there is nothing here" from "I could not open this".
//
// The distinction is the whole point.  A read failure has two causes that look
// identical to a caller comparing against nil:
//
//   - the rows do not exist — a project that has never had a secret, where a
//     write must create them; and
//   - the rows exist and would not open — the wrong SECRETS_MASTER_KEY, a key
//     row in an unexpected format, a data row that is not a Fernet token, a
//     vault body that is not `{"secrets":{…},"hidden_secrets":{…}}`.
//
// Collapsing the two is a silent data loss: the project path's old
// readOrInitVault answered ANY read failure by writing a fresh empty vault, so
// a single POST against an unreadable-but-present vault replaced every secret
// in it and reported 201.  ErrVaultAbsent is returned for the first cause only,
// and only a caller that has checked for it may write.

// ErrVaultAbsent means the vault's rows do not exist yet — the only condition
// under which a write is allowed to create them.  Any OTHER read failure means
// rows exist that could not be opened, and must never be overwritten.
var ErrVaultAbsent = errors.New("secrets: vault has not been initialised")

// errVaultWrite marks a failure AFTER the vault was opened and mutated: the
// re-encryption, the row write or the commit.  Routes answer it as "failed to
// save" rather than as "unreadable", exactly as they did when the read and the
// write were two separate calls.
var errVaultWrite = errors.New("secrets: vault write failed")

// errMutationRefused is returned by a mutation callback that has already
// written the HTTP answer itself.  It rolls the transaction back and tells the
// route that the response is done.
var errMutationRefused = errors.New("secrets: mutation refused")

// vaultQuerier is what the id-keyed primitives run their statements on: the
// pool for a plain read, and a transaction for a locked read-modify-write.
// Both *pgxpool.Pool and pgx.Tx satisfy it.
type vaultQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// newFernetKey returns 32 fresh random bytes — the raw form; `encryptKey`
// renders them in centry's on-disk representation.
func newFernetKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate fernet key: %w", err)
	}
	return key, nil
}

// openVault reads one vault's two rows on `q` and decrypts them, returning the
// vault and the project's Fernet key (the key the data row is sealed with, and
// the one a write back must use).
//
// With `lock` set, the rows are selected FOR UPDATE OF k, d — the SAME lock,
// in the SAME statement, that infra/db/repos.lockCurrentSecretVault takes on
// these rows — so a caller inside a transaction holds them until it commits,
// and every other writer through either path waits rather than reads stale
// bytes (#858).  Identical SQL matters beyond the lock itself: two statements
// that lock the same two rows in a different order can deadlock, and the
// planner orders the locks from the statement.
//
// It returns ErrVaultAbsent ONLY when neither row exists.  Every other failure —
// a key row beside no data row, a decrypt failure, a body that is not the
// expected shape — is returned as itself, so no caller can mistake "I could
// not open this" for "there is nothing here" and write over it.  pgx.ErrNoRows
// is never returned unwrapped, so a transport failure during the lookup cannot
// be read as an absent vault either.
func (h *Handler) openVault(ctx context.Context, q vaultQuerier, vaultID string, lock bool) (vaultData, []byte, error) {
	query := `SELECT k.data, d.data
FROM centry.secrets_key AS k
JOIN centry.secrets_data AS d ON d.id = k.id
WHERE k.id = $1`
	keyOnly := `SELECT data FROM centry.secrets_key WHERE id = $1`
	if lock {
		query += `
FOR UPDATE OF k, d`
		keyOnly += ` FOR UPDATE`
	}
	var keyBytes, dataBytes []byte
	err := q.QueryRow(ctx, query, vaultID).Scan(&keyBytes, &dataBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		// No joined row: either the vault is absent, or it is half there.  A
		// key with no data is a half-initialised vault, not an absent one.
		// Treating it as absent would let the next write mint a SECOND key over
		// the first, orphaning whatever data row arrives later.
		var orphanKey []byte
		switch err := q.QueryRow(ctx, keyOnly, vaultID).Scan(&orphanKey); {
		case errors.Is(err, pgx.ErrNoRows):
			return vaultData{}, nil, ErrVaultAbsent
		case err != nil:
			return vaultData{}, nil, fmt.Errorf("read %s secrets_key: %w", vaultID, err)
		default:
			return vaultData{}, nil, fmt.Errorf("vault %s has a key row but no data row", vaultID)
		}
	}
	if err != nil {
		return vaultData{}, nil, fmt.Errorf("read %s vault rows: %w", vaultID, err)
	}

	fernetKey, err := h.decryptKey(keyBytes)
	if err != nil {
		return vaultData{}, nil, fmt.Errorf("decrypt %s vault key: %w", vaultID, err)
	}
	plaintext, err := fernetDecrypt(fernetKey, dataBytes)
	if err != nil {
		return vaultData{}, nil, fmt.Errorf("decrypt %s vault data: %w", vaultID, err)
	}
	var v vaultData
	if err := json.Unmarshal(plaintext, &v); err != nil {
		return vaultData{}, nil, fmt.Errorf("unmarshal %s vault data: %w", vaultID, err)
	}
	if v.Secrets == nil {
		v.Secrets = map[string]string{}
	}
	if v.HiddenSecrets == nil {
		v.HiddenSecrets = map[string]string{}
	}
	return v, fernetKey, nil
}

// readVaultByID reads and decrypts one vault, unlocked.  It is the read for a
// caller that will NOT write: a read that feeds a write goes through
// mutateVaultByID, where it runs under the row lock.
func (h *Handler) readVaultByID(ctx context.Context, vaultID string) (vaultData, error) {
	v, _, err := h.openVault(ctx, h.pool, vaultID, false)
	return v, err
}

// mutateVaultByID is the ONE read-modify-write of a vault: it opens the vault,
// hands it to `mutate`, and writes it back — all inside one transaction that
// holds `SELECT … FOR UPDATE OF k, d` on the vault's rows from the read to the
// commit.
//
// WHY A LOCK, AND WHY THIS ONE.  A vault is one encrypted blob.  Every write
// re-encrypts the WHOLE of it, so two writers that read the same version and
// write back their own edit do not merge: the second write replaces the first,
// and every secret only the first writer added is gone, silently, with 2xx
// answers to both.  The read and the write used to be two pool calls with
// nothing held between them, while infra/db/repos.CurrentSecretVaultRepository
// mutated the SAME rows under a row lock — so a Settings edit racing a
// credential save, or two Playwright workers creating secrets in one project,
// lost writes (#858).  Taking the repository's own lock here makes the two
// paths one queue.
//
// A VAULT THAT DOES NOT EXIST YET is the other half of the race.  Nothing can
// be locked before the rows exist, so with `init` the key row is inserted
// FIRST — `ON CONFLICT DO NOTHING RETURNING id` — and the answer says who
// minted it.  Two concurrent first writers both reach that INSERT; PostgreSQL
// makes the second wait for the first to commit, then reports no row to it,
// and its locked read then sees the first writer's committed vault.  Whatever
// key survives the insert is the key the data is sealed with, so no data row
// is ever written under a key that was not stored.  The insert never replaces
// an existing key row: an upsert would orphan the data row encrypted under the
// old key.
//
// Without `init`, an absent vault is ErrVaultAbsent and nothing is written —
// the contract every update-style route and the provisioning-time writers
// rely on, unchanged.  An UNREADABLE vault fails in either mode and its rows
// are left exactly as they are.
//
// `mutate` sees a vault whose two maps are never nil.  It reports whether the
// vault must be written back; a vault this call CREATED is written regardless,
// because a key row with no data row is the half state every reader refuses.
// Any error it returns rolls the transaction back and is returned as itself;
// errMutationRefused is the one a route uses after answering the request
// inside the callback.
func (h *Handler) mutateVaultByID(
	ctx context.Context,
	vaultID string,
	init bool,
	mutate func(v *vaultData) (write bool, err error),
) error {
	tx, err := h.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		return fmt.Errorf("begin %s vault write: %w", vaultID, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var (
		v         vaultData
		fernetKey []byte
		created   bool
	)
	if init {
		fernetKey, created, err = h.claimVaultKey(ctx, tx, vaultID)
		if err != nil {
			return err
		}
	}
	if created {
		// The key row is this transaction's own uncommitted insert: no other
		// writer can lock it, insert beside it or see it until the commit, so
		// it is already held as firmly as FOR UPDATE would hold it.
		v = vaultData{Secrets: map[string]string{}, HiddenSecrets: map[string]string{}}
	} else {
		v, fernetKey, err = h.openVault(ctx, tx, vaultID, true)
		if err != nil {
			return err
		}
	}

	write, err := mutate(&v)
	if err != nil {
		return err
	}
	if !write && !created {
		return nil
	}
	if err := h.writeVaultRow(ctx, tx, vaultID, fernetKey, v); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: commit %s vault: %w", errVaultWrite, vaultID, err)
	}
	return nil
}

// claimVaultKey inserts a freshly minted key row for `vaultID` if the vault has
// none, and reports whether THIS call inserted it.  The key is returned only
// on the created answer; otherwise the locked read that follows opens the
// vault under the key that is stored.
func (h *Handler) claimVaultKey(ctx context.Context, tx vaultQuerier, vaultID string) ([]byte, bool, error) {
	minted, err := newFernetKey()
	if err != nil {
		return nil, false, err
	}
	encoded, err := h.encryptKey(minted)
	if err != nil {
		return nil, false, fmt.Errorf("encrypt %s vault key: %w", vaultID, err)
	}
	var insertedID string
	err = tx.QueryRow(ctx,
		`INSERT INTO centry.secrets_key (id, data) VALUES ($1, $2)
		 ON CONFLICT (id) DO NOTHING
		 RETURNING id`,
		vaultID, encoded,
	).Scan(&insertedID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// A key row exists (committed by somebody else, possibly just now).
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("write %s secrets_key: %w", vaultID, err)
	}
	return minted, true, nil
}

// writeVaultRow encrypts one vault under `fernetKey` and upserts its data row
// on `tx`.  Only mutateVaultByID calls it, with the key openVault or
// claimVaultKey returned inside the same transaction.
//
// The key row is stored in centry's on-disk form (the 44-byte base64 ENCODING
// of the 32 key bytes) via `encryptKey`, not as the raw bytes — see that
// function for why (#196/#197).
func (h *Handler) writeVaultRow(ctx context.Context, tx vaultQuerier, vaultID string, fernetKey []byte, v vaultData) error {
	plaintext, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("%w: marshal %s vault data: %w", errVaultWrite, vaultID, err)
	}
	ciphertext, err := fernetEncrypt(fernetKey, plaintext)
	if err != nil {
		return fmt.Errorf("%w: encrypt %s vault data: %w", errVaultWrite, vaultID, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO centry.secrets_data (id, data) VALUES ($1, $2)
		 ON CONFLICT (id) DO UPDATE SET data = excluded.data`,
		vaultID, ciphertext,
	); err != nil {
		return fmt.Errorf("%w: write %s secrets_data: %w", errVaultWrite, vaultID, err)
	}
	return nil
}

// writeVaultByID REPLACES one vault with `v`, creating it when absent.  It is
// a seed for tests and a whole-vault rewrite for nothing else: every product
// write edits the vault it read under the lock, through mutateVaultByID.
func (h *Handler) writeVaultByID(ctx context.Context, vaultID string, v vaultData) error {
	return h.mutateVaultByID(ctx, vaultID, true, func(current *vaultData) (bool, error) {
		*current = v
		return true, nil
	})
}

// ─── the project vault ────────────────────────────────────────────────────────

// readVaultCtx reads project `projectID`'s vault.  ErrVaultAbsent means the
// project has no vault yet; any other error means one exists and would not open.
func (h *Handler) readVaultCtx(ctx context.Context, projectID string) (vaultData, error) {
	return h.readVaultByID(ctx, dbKey(projectID))
}

// mutateVaultCtx is mutateVaultByID for project `projectID`'s vault.  With
// `init`, a project that has no vault gets an empty one to write; it does NOT
// fall back for an unreadable vault.
func (h *Handler) mutateVaultCtx(
	ctx context.Context,
	projectID string,
	init bool,
	mutate func(v *vaultData) (bool, error),
) error {
	return h.mutateVaultByID(ctx, dbKey(projectID), init, mutate)
}

// writeVaultCtx replaces project `projectID`'s vault wholesale — a test seed,
// see writeVaultByID.
func (h *Handler) writeVaultCtx(ctx context.Context, projectID string, v vaultData) error {
	return h.writeVaultByID(ctx, dbKey(projectID), v)
}

// vaultSaveFailed answers a write that failed after the vault was opened.
func vaultSaveFailed(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": message})
}

// vaultUnreadable answers the one failure every project route shares: the vault
// exists and could not be opened.  It is a 500 and not an empty result, because
// an empty result is what invites the write that destroys it.
func vaultUnreadable(w http.ResponseWriter) {
	writeJSON(w, http.StatusInternalServerError, map[string]string{
		"error": "project vault is unreadable",
	})
}

// encryptKey renders a raw 32-byte Fernet key in the ON-DISK representation
// centry writes, then wraps it with the master key (if set).
//
// centry's database secret engine stores `cryptography.fernet.Fernet.
// generate_key()` output — the 44-byte URL-safe base64 ENCODING of the 32 key
// bytes — not the raw bytes (legacy/…/secret_engines/database.py `_write_key`).
// This handler used to persist the raw 32 bytes, which no other reader in this
// repository can open: `centrysecrets.decodeFernetKey` (the reader behind the
// current chat-config and Configurations vault paths) requires exactly 44
// base64 bytes and rejects a 32-byte row outright. A project whose vault this
// handler created was therefore unreadable by the current generation, and a
// project whose vault centry created was unwritable by this handler
// (`fernetEncrypt` would slice a 28-byte AES key out of the 44 and fail).
// Found while making the chat-config route reachable (#194).
func (h *Handler) encryptKey(raw []byte) ([]byte, error) {
	// A malformed master key stops the write here (#412). Returning the
	// unwrapped encoding instead is what made the defect silent: the vault was
	// minted in the clear and every later read of it succeeded.
	if h.masterKeyErr != nil {
		return nil, h.masterKeyErr
	}
	encoded := []byte(base64.URLEncoding.EncodeToString(raw))
	if h.masterKey == nil {
		return encoded, nil
	}
	return fernetEncrypt(h.masterKey, encoded)
}

// decryptKey unwraps the stored key bytes back to a 32-byte Fernet key. It
// accepts BOTH representations: centry's 44-byte base64 encoding (what
// encryptKey now writes) and the raw 32 bytes earlier builds of this handler
// wrote, so an existing database keeps opening.
func (h *Handler) decryptKey(stored []byte) ([]byte, error) {
	// A malformed master key stops the read too (#412). A wrapped key row would
	// otherwise be read as an unwrapped one, which fails later and further away.
	if h.masterKeyErr != nil {
		return nil, h.masterKeyErr
	}
	if h.masterKey != nil {
		unwrapped, err := fernetDecrypt(h.masterKey, stored)
		if err != nil {
			return nil, err
		}
		stored = unwrapped
	}
	if len(stored) == 32 {
		return stored, nil
	}
	return fernetDecodeKey(string(stored))
}

// ─── Fernet implementation ────────────────────────────────────────────────────
//
// Fernet spec: https://github.com/fernet/spec/blob/master/Spec.md
//
// Token = base64url( Version[1] | Timestamp[8] | IV[16] |
//                    Ciphertext[16*ceil(n/16)] | HMAC[32] )
//
// Key layout: first 16 bytes = HMAC-SHA256 signing key
//             last  16 bytes = AES-128-CBC encryption key

// fernetDecodeKey base64url-decodes a Fernet key string into 32 bytes.
func fernetDecodeKey(key string) ([]byte, error) {
	b, err := base64.URLEncoding.DecodeString(key)
	if err != nil {
		return nil, err
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("fernet key must be 32 bytes, got %d", len(b))
	}
	return b, nil
}

// fernetEncrypt encrypts plaintext using a raw 32-byte Fernet key.
// The returned value is the base64url-encoded Fernet token as bytes.
func fernetEncrypt(key, plaintext []byte) ([]byte, error) {
	signingKey := key[:16]
	encKey := key[16:]

	// PKCS7-pad plaintext to a multiple of 16.
	padded, err := pkcs7Pad(plaintext, aes.BlockSize)
	if err != nil {
		return nil, err
	}

	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, err
	}
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, padded)

	// Build the token body (before HMAC).
	ts := make([]byte, 8)
	binary.BigEndian.PutUint64(ts, uint64(time.Now().Unix()))

	var body bytes.Buffer
	body.WriteByte(0x80) // version
	body.Write(ts)
	body.Write(iv)
	body.Write(ciphertext)

	mac := hmac.New(sha256.New, signingKey)
	mac.Write(body.Bytes())
	body.Write(mac.Sum(nil))

	token := base64.URLEncoding.EncodeToString(body.Bytes())
	return []byte(token), nil
}

// fernetDecrypt decrypts a Fernet token (base64url bytes) with a raw 32-byte key.
func fernetDecrypt(key, token []byte) ([]byte, error) {
	signingKey := key[:16]
	encKey := key[16:]

	raw, err := base64.URLEncoding.DecodeString(string(token))
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	// Minimum: 1 (ver) + 8 (ts) + 16 (iv) + 16 (≥1 block) + 32 (hmac) = 73
	if len(raw) < 73 {
		return nil, fmt.Errorf("token too short (%d bytes)", len(raw))
	}
	if raw[0] != 0x80 {
		return nil, fmt.Errorf("unsupported fernet version 0x%02x", raw[0])
	}

	// Verify HMAC.
	mac := hmac.New(sha256.New, signingKey)
	mac.Write(raw[:len(raw)-32])
	if !hmac.Equal(mac.Sum(nil), raw[len(raw)-32:]) {
		return nil, fmt.Errorf("fernet HMAC mismatch")
	}

	iv := raw[9:25]
	ciphertext := raw[25 : len(raw)-32]
	if len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext length not a multiple of block size")
	}

	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, err
	}
	plaintext := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plaintext, ciphertext)

	plaintext, err = pkcs7Unpad(plaintext)
	if err != nil {
		return nil, err
	}
	return plaintext, nil
}

// pkcs7Pad pads data to a multiple of blockSize using PKCS#7.
//
// Returns an error rather than padding blindly, for two reasons the previous
// signature could not express:
//
//   - blockSize must be in 1..255. PKCS#7 encodes the pad length in a single
//     byte, so a larger block size cannot be represented and `byte(pad)` would
//     silently truncate — producing padding that pkcs7Unpad rejects, or worse,
//     padding that unpads to the wrong length. The only current caller passes
//     aes.BlockSize, but a future one passing 256 would get silent corruption
//     of a SECRET rather than a loud failure.
//   - len(data)+pad must not overflow int (CodeQL go/allocation-size-overflow,
//     alert 11). Unreachable with today's caller, since data is a secret value
//     bounded long before here — but the guard costs one comparison and removes
//     the need for anyone to re-derive that reasoning.
func pkcs7Pad(data []byte, blockSize int) ([]byte, error) {
	if blockSize <= 0 || blockSize > 255 {
		return nil, fmt.Errorf("pkcs7: block size %d out of range (1..255)", blockSize)
	}
	pad := blockSize - (len(data) % blockSize)
	if len(data) > math.MaxInt-pad {
		return nil, fmt.Errorf("pkcs7: input too large to pad")
	}
	result := make([]byte, len(data)+pad)
	copy(result, data)
	for i := len(data); i < len(result); i++ {
		result[i] = byte(pad)
	}
	return result, nil
}

// pkcs7Unpad removes PKCS#7 padding.
func pkcs7Unpad(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty data")
	}
	pad := int(data[len(data)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(data) {
		return nil, fmt.Errorf("invalid PKCS#7 padding byte %d", pad)
	}
	for i := len(data) - pad; i < len(data); i++ {
		if data[i] != byte(pad) {
			return nil, fmt.Errorf("invalid PKCS#7 padding")
		}
	}
	return data[:len(data)-pad], nil
}

// EnsureProjectVault creates an EMPTY vault for a project that has none, and
// does nothing for a project that already has a readable one (#373).
//
// WHY PROVISIONING STILL DOES THIS. Every write route below mints a project's
// Fernet key lazily, so no read is BROKEN by a project that has none: an absent
// vault is now a distinct answer (storage.ErrVaultAbsent) and every reader that
// consults the vault for a DEFAULT — the model catalogue, the chat
// configuration, the index staleness timeout — reads it as "never set" and
// answers normally. It did not used to be. The loader reported absent rows as
// the generic ErrContentUnavailable, the model defaults reader failed the WHOLE
// read on it, and the configurations route turned that into a 500, so a project
// with no vault rows presented to its owner as "the product has no models".
// That is what #373 was.
//
// The step remains because it is what pylon does — the vault is part of a
// provisioned project, not an artefact of the first write — and because it
// keeps every project's vault minted by the one minter described below rather
// than by whichever route happens to write first.
// internal/application/projectprovisioning calls this as its project_secrets
// step.
//
// WHY IT IS HERE AND NOT IN THE PROVISIONER. This is the only code in the tree
// that mints a vault key, and it decides — from SECRETS_MASTER_KEY — whether
// the stored key is wrapped. A second minter with its own rule would write
// vaults this handler cannot open, and readVaultByID never overwrites an
// unreadable vault, so that project's secrets would 500 for ever with no way
// back. One minter, one rule.
//
// IDEMPOTENT, and deliberately narrow about which failure it acts on. Only
// ErrVaultAbsent — neither row present — permits a write. A vault that exists
// and will not open is reported, never replaced.
//
// The write CREATES and never updates. The ordinary vault write updates the
// data row, and that is correct for it; here it is not. Two callers can both
// read an absent vault, and the second one would then overwrite whatever the
// first one sealed in between with an empty object. The configurations write
// path calls this on the first credential save of a project, so that race
// would discard a provider credential the user had just saved. Under
// mutateVaultByID the two callers are serialised on the key row: the one that
// minted it writes the empty vault, and the other opens the vault that is
// there and leaves it untouched.
func (h *Handler) EnsureProjectVault(ctx context.Context, projectID string) error {
	return h.mutateVaultCtx(ctx, projectID, true, func(*vaultData) (bool, error) {
		// Nothing to change: a vault that exists stays as it is, and one this
		// call created is written by mutateVaultByID regardless.
		return false, nil
	})
}

// RemoveProjectVault deletes a project's vault rows.
//
// It is the compensation half of EnsureProjectVault, and the delete half of
// project deprovisioning. Neither centry.secrets_key nor centry.secrets_data
// carries a foreign key to centry.project — the id is the TEXT `project-<id>`
// rather than the integer — so nothing removes these rows for us, and a vault
// left behind would be adopted by the next project that draws the same id.
//
// Removing an absent vault is success, so a re-run and a compensation for a
// step that never ran both converge.
func (h *Handler) RemoveProjectVault(ctx context.Context, projectID string) error {
	vaultID := dbKey(projectID)
	// One transaction: a key row without its data row is the half state
	// readVaultByID reports as a hard error rather than as an absent vault.
	transaction, err := h.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin %s vault delete: %w", vaultID, err)
	}
	defer func() { _ = transaction.Rollback(context.WithoutCancel(ctx)) }()

	for _, statement := range []string{
		`DELETE FROM centry.secrets_data WHERE id = $1`,
		`DELETE FROM centry.secrets_key WHERE id = $1`,
	} {
		if _, err := transaction.Exec(ctx, statement, vaultID); err != nil {
			return fmt.Errorf("delete %s vault rows: %w", vaultID, err)
		}
	}
	return transaction.Commit(ctx)
}

// StoreProjectSecrets writes several regular secrets to one project vault in a
// SINGLE rewrite. It never creates a vault: an absent or unreadable vault is an
// error (#399).
//
// WHY IT IS HERE AND NOT IN THE PROVISIONER, for the same reason
// EnsureProjectVault is. This handler holds the only master key a deployment
// actually sets. A material writer built anywhere else derives its own key, so
// it writes material this handler cannot open, and it cannot open material this
// handler wrote. The creator and the writer must share one key source, or the
// vault is unusable whichever creator ran first.
//
// WHY ONE REWRITE, AND NOT REPEATED StoreSecret CALLS. Provisioning stores a
// project's PgVector password and its connection string together. Two calls are
// two read-modify-write cycles, so a failure between them leaves half the
// material behind. A later index run then reads a password with no connection
// string.
//
// The caller must create the vault first. The project_secrets provisioning step
// does that, and it runs before every caller of this.
func (h *Handler) StoreProjectSecrets(ctx context.Context, projectID string, values map[string]string) error {
	vaultID := dbKey(projectID)
	if len(values) == 0 {
		return fmt.Errorf("store %s secrets: no values given", vaultID)
	}
	for name := range values {
		if name == "" {
			return fmt.Errorf("store %s secrets: a secret name is empty", vaultID)
		}
	}
	err := h.mutateVaultCtx(ctx, projectID, false, func(vault *vaultData) (bool, error) {
		for name, value := range values {
			vault.Secrets[name] = value
		}
		return true, nil
	})
	if err != nil && !errors.Is(err, errVaultWrite) {
		return fmt.Errorf("store %s secrets: %w", vaultID, err)
	}
	return err
}

// LookupProjectSecret reads one regular secret from a project vault.
//
// It answers with the package's ONE not-found idiom, the ErrSecretNotFound
// sentinel below (#416). It used to answer with a `found bool` instead, which
// made this package state the same condition two ways: a caller had to know
// which function it held before it could tell "absent" from "failed".
//
// THE SENTINEL WON, for two reasons. It carries a cause, so the three answers
// stay separable through any number of wrapping layers: ErrSecretNotFound (the
// vault opened and holds no such name), ErrVaultAbsent (the project has no
// vault yet), and every other error (a vault that exists and would not open).
// A bool carries none of that, so a caller that wants the third case apart from
// the second must invent its own convention. And every neighbouring vault API
// already uses it — centrysecrets.ErrSecretNotFound and storage.ErrVaultAbsent
// — so one idiom now spans the whole vault path rather than stopping here.
//
// Vault behaviour is unchanged. An absent vault and an unreadable vault were
// errors before and stay errors, so no caller can read a key mismatch as an
// empty result.
func (h *Handler) LookupProjectSecret(
	ctx context.Context,
	projectID string,
	name string,
) (string, error) {
	vaultID := dbKey(projectID)
	if name == "" {
		return "", fmt.Errorf("look up %s secret: the name is empty", vaultID)
	}
	vault, err := h.readVaultCtx(ctx, projectID)
	if err != nil {
		return "", fmt.Errorf("look up %s secret: %w", vaultID, err)
	}
	if stored, ok := vault.Secrets[name]; ok {
		return stored, nil
	}
	return "", fmt.Errorf("look up %s secret: %w: %q", vaultID, ErrSecretNotFound, name)
}

// StoreSecret programmatically stores a secret value without going through HTTP.
func (h *Handler) StoreSecret(ctx context.Context, _ *http.Request, projectID, name, value string) error {
	return h.mutateVaultCtx(ctx, projectID, true, func(vault *vaultData) (bool, error) {
		vault.Secrets[name] = value
		return true, nil
	})
}

// ErrSecretNotFound means the vault opened and holds no secret of that name.
//
// It is this package's ONLY idiom for "not found" (#416). No exported function
// here reports the condition with a `found bool`, so a caller never has to know
// which function it holds before it can read the answer.
//
// It is distinct from ErrVaultAbsent (the project has no vault yet) and from
// every other read failure (a vault that exists and would not open). A caller
// that applies a default value must separate the three: only the first two mean
// "not set", and treating an unreadable vault as "not set" turns a broken vault
// into an accepted default.
var ErrSecretNotFound = errors.New("secrets: secret not found")

// ResolveSecretValue resolves a {{secret.name}} reference to its plaintext value.
func (h *Handler) ResolveSecretValue(ctx context.Context, projectID, secretRef string) (string, error) {
	name := strings.TrimSuffix(strings.TrimPrefix(secretRef, "{{secret."), "}}")
	vault, err := h.readVaultCtx(ctx, projectID)
	if err != nil {
		return "", err
	}
	if val, ok := vault.Secrets[name]; ok {
		return val, nil
	}
	if val, ok := vault.HiddenSecrets[name]; ok {
		return val, nil
	}
	return "", fmt.Errorf("%w: %q", ErrSecretNotFound, name)
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
