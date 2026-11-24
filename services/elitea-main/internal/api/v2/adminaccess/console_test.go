package adminaccess_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	registry "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	console "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// The six built-in roles that declare a set, each held alone by one user.
var personas = []string{
	registry.RoleSuperAdmin, registry.RoleAdmin, registry.RoleLLMManager,
	registry.RoleProjectAdministrator, registry.RoleSecurityAuditor, registry.RoleEmergencyAccess,
}

// directory is a fake database: users hold roles, roles hold permissions.
type directory struct {
	users map[int64][]string
	roles map[string][]string
	// builtIn marks the seeded roles, with their marker.
	builtIn map[string]string
	err     error
}

func newDirectory() *directory {
	d := &directory{users: map[int64][]string{}, roles: map[string][]string{}, builtIn: map[string]string{}}
	for _, role := range registry.BuiltInRoles() {
		d.builtIn[role.Name] = string(role.Marker)
		d.roles[role.Name] = role.Set()
	}
	return d
}

func (d *directory) role(name string) console.Role {
	marker, ok := d.builtIn[name]
	return console.Role{Name: name, BuiltIn: ok, Marker: marker}
}

func (d *directory) HeldRoles(_ context.Context, userID int64) ([]console.Role, error) {
	if d.err != nil {
		return nil, d.err
	}
	var roles []console.Role
	for _, name := range d.users[userID] {
		roles = append(roles, d.role(name))
	}
	return roles, nil
}

func (d *directory) GrantingRoles(_ context.Context, names []string) ([]console.GrantingRole, error) {
	if d.err != nil {
		return nil, d.err
	}
	var out []console.GrantingRole
	for name, set := range d.roles {
		var held []string
		for _, permission := range set {
			if slices.Contains(names, permission) {
				held = append(held, permission)
			}
		}
		if len(held) > 0 {
			out = append(out, console.GrantingRole{Role: d.role(name), Permissions: held})
		}
	}
	return out, nil
}

// ResolvePermissions answers the union of the user's role sets.
func (d *directory) ResolvePermissions(_ context.Context, user auth.User, mode, _ string) (auth.PermissionResolution, error) {
	if mode != auth.PermissionModeAdministration {
		return auth.PermissionResolution{}, errors.New("unexpected mode " + mode)
	}
	var id int64
	for candidate := range d.users {
		if user.UserID == itoa(candidate) {
			id = candidate
		}
	}
	if id == 0 {
		return auth.PermissionResolution{}, auth.ErrPermissionDenied
	}
	var permissions []string
	for _, role := range d.users[id] {
		permissions = append(permissions, d.roles[role]...)
	}
	return auth.PermissionResolution{UserID: id, Permissions: permissions}, nil
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func serve(t *testing.T, h *console.Handler, user *auth.User, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	h.Register(r)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if user != nil {
		req = req.WithContext(auth.ContextWithUser(req.Context(), *user))
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func session(id int64) *auth.User {
	return &auth.User{ID: itoa(id), UserID: itoa(id), CredentialKind: auth.CredentialKindSession}
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	var indented bytes.Buffer
	if err := json.Indent(&indented, got, "", "  "); err != nil {
		t.Fatalf("indent: %v (%s)", err, got)
	}
	path := filepath.Join("testdata", "console", name+".golden.json")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(path, indented.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with UPDATE_GOLDEN=1)", path, err)
	}
	if !bytes.Equal(indented.Bytes(), want) {
		t.Errorf("%s drifted (regenerate with UPDATE_GOLDEN=1 and review):\n%s", path, indented.String())
	}
}

// TestConsoleDocumentPerBuiltInRole: one golden document per built-in role
// held alone.
func TestConsoleDocumentPerBuiltInRole(t *testing.T) {
	t.Parallel()
	for i, persona := range personas {
		t.Run(persona, func(t *testing.T) {
			t.Parallel()
			d := newDirectory()
			id := int64(100 + i)
			d.users[id] = []string{persona}
			rec := serve(t, console.NewHandler(d, d, nil), session(id), "/console")
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control %q, want no-store", got)
			}
			body := rec.Body.Bytes()
			for _, name := range registry.Names() {
				if bytes.Contains(body, []byte(`"`+name+`"`)) {
					t.Errorf("the document names the permission %q", name)
				}
			}
			golden(t, persona, body)
		})
	}
}

type document struct {
	Areas []struct {
		ID    string `json:"id"`
		Items []struct {
			ID    string `json:"id"`
			Level string `json:"level"`
		} `json:"items"`
	} `json:"areas"`
	Roles []struct {
		Name string `json:"name"`
	} `json:"roles"`
	Elevation *json.RawMessage `json:"elevation"`
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) document {
	t.Helper()
	var doc document
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	return doc
}

func items(doc document) map[string]string {
	out := map[string]string{}
	for _, area := range doc.Areas {
		for _, item := range area.Items {
			out[item.ID] = item.Level
		}
	}
	return out
}

func TestConsoleLeavesOutAnItemWhosePermissionsTheCallerLacks(t *testing.T) {
	t.Parallel()
	d := newDirectory()
	d.roles["secrets_reader"] = []string{registry.ConfigurationSecretsSecretView}
	d.users[7] = []string{"secrets_reader"}
	doc := decode(t, serve(t, console.NewHandler(d, d, nil), session(7), "/console"))
	got := items(doc)
	if len(got) != 1 || got["secrets"] != "read" {
		t.Errorf("items: got %v, want only secrets at read", got)
	}
	if len(doc.Areas) != 1 || doc.Areas[0].ID != string(registry.AreaPlatform) {
		t.Errorf("areas: got %+v, want only platform", doc.Areas)
	}
	if len(doc.Roles) != 1 || doc.Roles[0].Name != "secrets_reader" {
		t.Errorf("roles: got %+v, want the custom role", doc.Roles)
	}
}

func TestConsoleWithNoAdministrationPermissionAnswersNoArea(t *testing.T) {
	t.Parallel()
	d := newDirectory()
	d.users[8] = nil // signed in, no administration role
	for name, user := range map[string]*auth.User{
		"no role":         session(8),
		"refused":         session(9), // the resolver refuses an unknown principal
		"native with one": {ID: "10", UserID: "10", CredentialKind: auth.CredentialKindNative},
	} {
		if name == "native with one" {
			d.users[10] = []string{registry.RoleAdmin}
		}
		rec := serve(t, console.NewHandler(d, d, nil), user, "/console")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200", name, rec.Code)
		}
		doc := decode(t, rec)
		if len(doc.Areas) != 0 || doc.Elevation != nil {
			t.Errorf("%s: got %+v, want no area and no elevation", name, doc)
		}
		if !strings.Contains(rec.Body.String(), `"areas":[]`) {
			t.Errorf("%s: areas must be an empty list, not absent: %s", name, rec.Body)
		}
	}
}

func TestConsoleFailureIsNotAnEmptyDocument(t *testing.T) {
	t.Parallel()
	d := newDirectory()
	d.users[11] = []string{registry.RoleAdmin}
	d.err = errors.New("pool exhausted")
	if rec := serve(t, console.NewHandler(d, d, nil), session(11), "/console"); rec.Code != http.StatusInternalServerError {
		t.Errorf("a failed role read: status %d, want 500", rec.Code)
	}
	failing := failingResolver{}
	if rec := serve(t, console.NewHandler(failing, d, nil), session(11), "/console"); rec.Code != http.StatusInternalServerError {
		t.Errorf("a failed resolution: status %d, want 500", rec.Code)
	}
	if rec := serve(t, console.NewHandler(d, d, nil), nil, "/console"); rec.Code != http.StatusUnauthorized {
		t.Errorf("no principal: status %d, want 401", rec.Code)
	}
	if rec := serve(t, console.NewHandler(nil, nil, nil), session(11), "/console"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("not configured: status %d, want 503", rec.Code)
	}
}

type failingResolver struct{}

func (failingResolver) ResolvePermissions(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{}, errors.New("query timeout")
}

func TestConsoleElevationComesFromTheReader(t *testing.T) {
	t.Parallel()
	d := newDirectory()
	d.users[12] = []string{registry.RoleEmergencyAccess}
	doc := decode(t, serve(t, console.NewHandler(d, d, nil), session(12), "/console"))
	if doc.Elevation != nil {
		t.Errorf("elevation: got %s, want null until the ledger ships", *doc.Elevation)
	}
}

// TestConsoleItemNamesTheGrantingRolesAndNoPerson: an item the caller cannot
// open answers the roles that grant it, by role name only.
func TestConsoleItemNamesTheGrantingRolesAndNoPerson(t *testing.T) {
	t.Parallel()
	d := newDirectory()
	d.roles["budget_reader"] = []string{registry.ModelsAdminProjectBudgetsView}
	d.users[13] = []string{registry.RoleEmergencyAccess}
	d.users[14] = []string{"budget_reader"}
	rec := serve(t, console.NewHandler(d, d, nil), session(13), "/console/items/budgets-alerts")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "@") {
		t.Errorf("the item names an e-mail address: %s", rec.Body)
	}
	golden(t, "item-budgets-alerts", rec.Body.Bytes())

	var item struct {
		GrantingRoles []struct {
			Name  string `json:"name"`
			Level string `json:"level"`
		} `json:"granting_roles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, role := range item.GrantingRoles {
		names = append(names, role.Name+":"+role.Level)
	}
	want := []string{"super_admin:write", "admin:write", "llm_manager:write", "project_administrator:read", "budget_reader:read"}
	if !slices.Equal(names, want) {
		t.Errorf("granting roles: got %q, want %q", names, want)
	}
}

func TestConsoleItemUnknownAndUnavailable(t *testing.T) {
	t.Parallel()
	d := newDirectory()
	d.users[15] = nil
	if rec := serve(t, console.NewHandler(d, d, nil), session(15), "/console/items/no-such-item"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown item: status %d, want 404", rec.Code)
	}
	rec := serve(t, console.NewHandler(d, d, nil), session(15), "/console/items/break-glass")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"available":false`) {
		t.Errorf("an item that ships later: status %d, body %s; want 200 with available false", rec.Code, rec.Body)
	}
	// The legacy `system` role is never assigned, so it is never named.
	d.roles[registry.RoleSystem] = []string{registry.RetentionManage}
	rec = serve(t, console.NewHandler(d, d, nil), session(15), "/console/items/data-retention")
	if strings.Contains(rec.Body.String(), `"system"`) {
		t.Errorf("the unassignable role is named: %s", rec.Body)
	}
	if rec := serve(t, console.NewHandler(d, d, nil), nil, "/console/items/users"); rec.Code != http.StatusUnauthorized {
		t.Errorf("no principal: status %d, want 401", rec.Code)
	}
}
