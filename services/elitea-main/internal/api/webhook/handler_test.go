package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// fakeIPResolver is a deterministic, network-free stand-in for
// net.DefaultResolver (see ssrf.go's ipResolver seam). Every test in this
// package that needs a DestinationGuard to actually PASS a host uses this
// rather than a real DNS lookup, so the test suite never depends on the
// runner's network access or on a fixture hostname's real-world DNS answer.
type fakeIPResolver struct {
	ips map[string][]net.IPAddr
}

func (f fakeIPResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if addrs, ok := f.ips[host]; ok {
		return addrs, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// permissiveTestGuard resolves every hostname this file's fixtures use
// (example.com, example.org, ...) to an ordinary PUBLIC address, so
// Handler.Create/Update pass validation without reaching real DNS.
func permissiveTestGuard() *DestinationGuard {
	public := net.IPAddr{IP: net.ParseIP("93.184.216.34")}
	resolver := fakeIPResolver{ips: map[string][]net.IPAddr{
		"example.com": {public},
		"example.org": {public},
	}}
	return NewDestinationGuardWithResolver(nil, resolver)
}

type mockWebhookRepo struct {
	webhooks []Webhook
	// rotatedTo is the last value RotateSecret stored.
	rotatedTo string
}

func (m *mockWebhookRepo) List(_ context.Context, _ string) ([]Webhook, error) {
	return m.webhooks, nil
}

func (m *mockWebhookRepo) Create(_ context.Context, projectID string, wh Webhook) (Webhook, error) {
	wh.ID = "wh-new"
	wh.ProjectID = projectID
	wh.CreatedAt = time.Now()
	wh.UpdatedAt = time.Now()
	m.webhooks = append(m.webhooks, wh)
	return wh, nil
}

func (m *mockWebhookRepo) Get(_ context.Context, _ string, id string) (Webhook, error) {
	for _, wh := range m.webhooks {
		if wh.ID == id {
			return wh, nil
		}
	}
	return Webhook{}, nil
}

func (m *mockWebhookRepo) Update(_ context.Context, _ string, id string, wh Webhook, secret *string) (Webhook, error) {
	for index := range m.webhooks {
		if m.webhooks[index].ID != id {
			continue
		}
		stored := &m.webhooks[index]
		stored.URL, stored.Events, stored.Active = wh.URL, wh.Events, wh.Active
		if secret != nil && *secret != "" {
			stored.Secret, stored.SecretRef = "", "{{secret.replaced}}"
		}
		stored.UpdatedAt = time.Now()
		return *stored, nil
	}
	wh.ID = id
	wh.UpdatedAt = time.Now()
	return wh, nil
}

func (m *mockWebhookRepo) RotateSecret(_ context.Context, _ string, id, secret string) (Webhook, error) {
	m.rotatedTo = secret
	for index := range m.webhooks {
		if m.webhooks[index].ID == id {
			m.webhooks[index].Secret = ""
			m.webhooks[index].SecretRef = "{{secret.rotated}}"
			return m.webhooks[index], nil
		}
	}
	return Webhook{ID: id, SecretRef: "{{secret.rotated}}"}, nil
}

func (m *mockWebhookRepo) Delete(_ context.Context, _ string, _ string) error {
	return nil
}

func (m *mockWebhookRepo) ListByEvent(_ context.Context, _ string, _ string) ([]Webhook, error) {
	return m.webhooks, nil
}

// permissionResolverFunc adapts a function to auth.PermissionResolver.
type permissionResolverFunc func(
	context.Context,
	auth.User,
	string,
	string,
) (auth.PermissionResolution, error)

func (f permissionResolverFunc) ResolvePermissions(
	ctx context.Context,
	principal auth.User,
	mode string,
	projectID string,
) (auth.PermissionResolution, error) {
	return f(ctx, principal, mode, projectID)
}

// webhookPermissions is the ledger: every string Routes() gates on, paired with
// the route that names it. A route added without a row makes
// TestEveryWebhookRouteIsGated fail rather than ship ungated.
var webhookPermissions = []struct {
	method     string
	path       string
	permission string
}{
	{http.MethodGet, "/api/v2/projects/proj-1/webhooks/", listPermission},
	{http.MethodPost, "/api/v2/projects/proj-1/webhooks/", createPermission},
	{http.MethodGet, "/api/v2/projects/proj-1/webhooks/wh-1", detailsPermission},
	{http.MethodPut, "/api/v2/projects/proj-1/webhooks/wh-1", updatePermission},
	{http.MethodDelete, "/api/v2/projects/proj-1/webhooks/wh-1", deletePermission},
	{http.MethodPost, "/api/v2/projects/proj-1/webhooks/wh-1/rotate_secret", updatePermission},
	{http.MethodGet, "/api/v2/projects/proj-1/webhooks/wh-1/deliveries", detailsPermission},
	{http.MethodPost, "/api/v2/projects/proj-1/webhooks/wh-1/deliveries/del-1/redeliver", updatePermission},
}

// allWebhookPermissions is the distinct set the five routes draw from.
var allWebhookPermissions = []string{
	listPermission,
	detailsPermission,
	createPermission,
	updatePermission,
	deletePermission,
}

// entitledResolver admits every webhook permission for any project.
func entitledResolver() permissionResolverFunc {
	return func(
		_ context.Context,
		_ auth.User,
		_ string,
		_ string,
	) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 1, Permissions: allWebhookPermissions}, nil
	}
}

// withTestUser stands in for apimw.Auth, which router.go wraps this mount in.
func withTestUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), auth.User{ID: "1"})))
	})
}

// webhookRouter reproduces router.go's shape: an authenticated caller, the
// {projectID} in the MOUNT pattern, and the subrouter under it.
func webhookRouter(repo Repository, resolver auth.PermissionResolver) *chi.Mux {
	h := NewHandler(repo, WithPermissionResolver(resolver), WithDestinationGuard(permissiveTestGuard()))
	r := chi.NewRouter()
	r.Use(withTestUser)
	r.Route("/api/v2/projects/{projectID}/webhooks", func(r chi.Router) {
		r.Mount("/", h.Routes())
	})
	return r
}

/* ── the refused direction ─────────────────────────────────────────────── */

// A caller who resolves no permission is refused at every route (#496).
//
// Until this change the five routes carried no gate of any kind, so any
// authenticated caller could name any project id and read that project's
// webhook `secret`, repoint its `url`, or rotate the secret.
func TestEveryWebhookRouteIsGated(t *testing.T) {
	resolver := permissionResolverFunc(func(
		_ context.Context,
		_ auth.User,
		mode string,
		projectID string,
	) (auth.PermissionResolution, error) {
		if mode != auth.PermissionModeDefault {
			t.Errorf("resolver called in mode %q, want %q", mode, auth.PermissionModeDefault)
		}
		if projectID != "proj-1" {
			t.Errorf("resolver called for project %q, want the {projectID} of the mount", projectID)
		}
		return auth.PermissionResolution{UserID: 1, Permissions: []string{}}, nil
	})
	r := webhookRouter(&mockWebhookRepo{}, resolver)

	for _, route := range webhookPermissions {
		req := httptest.NewRequest(route.method, route.path, bytes.NewBufferString(`{}`))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s status = %d, want %d. This route is reachable without %s.",
				route.method, route.path, w.Code, http.StatusForbidden, route.permission)
		}
	}

	// The ledger must cover the registrations. chi.Walk reports what is really
	// mounted, so a sixth route cannot be added without a row here.
	registered := 0
	if err := chi.Walk(r, func(string, string, http.Handler, ...func(http.Handler) http.Handler) error {
		registered++
		return nil
	}); err != nil {
		t.Fatalf("walk the mounted webhook routes: %v", err)
	}
	if registered != len(webhookPermissions) {
		t.Fatalf("Routes() registers %d routes and webhookPermissions lists %d",
			registered, len(webhookPermissions))
	}
}

/* ── the entitled direction ────────────────────────────────────────────── */

// Each route passes with the exact string it names, and with no other.
//
// The negative half of this case is what pins the ledger: without it a router
// that gated all five on `list` would still pass every other test in this file
// and would let a viewer delete a webhook.
func TestEachWebhookRouteNeedsItsOwnPermission(t *testing.T) {
	for _, route := range webhookPermissions {
		for _, held := range allWebhookPermissions {
			resolver := permissionResolverFunc(func(
				_ context.Context,
				_ auth.User,
				_ string,
				_ string,
			) (auth.PermissionResolution, error) {
				return auth.PermissionResolution{UserID: 1, Permissions: []string{held}}, nil
			})
			r := webhookRouter(&mockWebhookRepo{}, resolver)

			req := httptest.NewRequest(route.method, route.path, bytes.NewBufferString(`{}`))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			refused := w.Code == http.StatusForbidden
			switch {
			case held == route.permission && refused:
				t.Errorf("%s %s was refused while holding its own permission %q",
					route.method, route.path, held)
			case held != route.permission && !refused:
				t.Errorf("%s %s answered %d while holding only %q; it must need %q",
					route.method, route.path, w.Code, held, route.permission)
			}
		}
	}
}

// A caller with no identity is refused before the resolver is asked.
func TestWebhookRoutesRefuseAnUnauthenticatedCaller(t *testing.T) {
	h := NewHandler(&mockWebhookRepo{}, WithPermissionResolver(entitledResolver()))
	r := chi.NewRouter()
	r.Route("/api/v2/projects/{projectID}/webhooks", func(r chi.Router) {
		r.Mount("/", h.Routes())
	})

	for _, route := range webhookPermissions {
		req := httptest.NewRequest(route.method, route.path, bytes.NewBufferString(`{}`))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s status = %d without a caller, want %d",
				route.method, route.path, w.Code, http.StatusUnauthorized)
		}
	}
}

// A Handler built without WithPermissionResolver serves nothing.
func TestWebhookRoutesFailClosedWithoutAResolver(t *testing.T) {
	r := chi.NewRouter()
	r.Use(withTestUser)
	r.Route("/api/v2/projects/{projectID}/webhooks", func(r chi.Router) {
		r.Mount("/", NewHandler(&mockWebhookRepo{}).Routes())
	})

	for _, route := range webhookPermissions {
		req := httptest.NewRequest(route.method, route.path, bytes.NewBufferString(`{}`))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s status = %d with no resolver composed, want %d",
				route.method, route.path, w.Code, http.StatusForbidden)
		}
	}
}

func TestHandler_List(t *testing.T) {
	repo := &mockWebhookRepo{
		webhooks: []Webhook{
			{ID: "wh-1", ProjectID: "proj-1", URL: "https://example.com/hook", Events: []string{"application.created"}, Active: true},
		},
	}
	r := webhookRouter(repo, entitledResolver())

	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects/proj-1/webhooks/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var body map[string]any
	_ = json.NewDecoder(w.Body).Decode(&body)
	items := body["items"].([]any)
	if len(items) != 1 {
		t.Errorf("expected 1 webhook, got %d", len(items))
	}
}

func TestHandler_Create(t *testing.T) {
	repo := &mockWebhookRepo{}
	r := webhookRouter(repo, entitledResolver())

	payload, _ := json.Marshal(Webhook{URL: "https://example.com/hook", Events: []string{"skill.created"}, Active: true})
	req := httptest.NewRequest(http.MethodPost, "/api/v2/projects/proj-1/webhooks/", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var wh Webhook
	_ = json.NewDecoder(w.Body).Decode(&wh)
	if wh.ID != "wh-new" {
		t.Errorf("expected ID wh-new, got %s", wh.ID)
	}
}

/* ── the secret is write-only (issue 2963) ─────────────────────────────── */

// decodeObject decodes one JSON object into raw members, so a test can tell
// an absent `secret` key from an empty one.
func decodeObject(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return object
}

func serve(r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// assertNoSecretMaterial fails when a response object carries the secret, its
// vault reference, or any of the given values anywhere in its bytes.
func assertNoSecretMaterial(t *testing.T, route string, body []byte, values ...string) {
	t.Helper()
	object := decodeObject(t, body)
	for _, key := range []string{"secret", "secret_ref"} {
		if _, present := object[key]; present {
			t.Errorf("%s returned a %q member: %s", route, key, body)
		}
	}
	for _, value := range values {
		if bytes.Contains(body, []byte(value)) {
			t.Errorf("%s response contains secret material %q", route, value)
		}
	}
	var hasSecret bool
	if err := json.Unmarshal(object["has_secret"], &hasSecret); err != nil || !hasSecret {
		t.Errorf("%s has_secret = %s, want true", route, object["has_secret"])
	}
}

// The list and detail routes never return the secret, for any caller that
// passes the gate: not the vault reference, and not a plaintext the vault move
// has not reached yet. They say only that one is set.
func TestReadRoutesNeverReturnTheSecret(t *testing.T) {
	repo := &mockWebhookRepo{webhooks: []Webhook{
		{ID: "wh-vault", ProjectID: "proj-1", URL: "https://example.com/a", Events: []string{}, SecretRef: "{{secret.0123456789abcdef0123456789abcdef}}", Active: true},
		{ID: "wh-legacy", ProjectID: "proj-1", URL: "https://example.com/b", Events: []string{}, Secret: "legacy-plaintext-value", Active: true},
	}}
	r := webhookRouter(repo, entitledResolver())
	material := []string{"0123456789abcdef0123456789abcdef", "legacy-plaintext-value"}

	list := serve(r, http.MethodGet, "/api/v2/projects/proj-1/webhooks/", "")
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d", list.Code)
	}
	var listBody struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listBody); err != nil || len(listBody.Items) != 2 {
		t.Fatalf("list body = %s", list.Body.String())
	}
	for _, item := range listBody.Items {
		assertNoSecretMaterial(t, "GET list", item, material...)
	}

	for _, id := range []string{"wh-vault", "wh-legacy"} {
		detail := serve(r, http.MethodGet, "/api/v2/projects/proj-1/webhooks/"+id, "")
		if detail.Code != http.StatusOK {
			t.Fatalf("detail status = %d", detail.Code)
		}
		assertNoSecretMaterial(t, "GET "+id, detail.Body.Bytes(), material...)
	}
}

// Create returns the secret the caller set, once; the next read does not.
func TestCreateReturnsTheSecretOnce(t *testing.T) {
	repo := &mockWebhookRepo{}
	r := webhookRouter(repo, entitledResolver())

	created := serve(r, http.MethodPost, "/api/v2/projects/proj-1/webhooks/",
		`{"url":"https://example.com/hook","events":["skill.created"],"secret":"caller-chosen-value","active":true}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", created.Code, created.Body.String())
	}
	var createdBody struct {
		ID        string `json:"id"`
		Secret    string `json:"secret"`
		HasSecret bool   `json:"has_secret"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdBody); err != nil {
		t.Fatal(err)
	}
	if createdBody.Secret != "caller-chosen-value" || !createdBody.HasSecret {
		t.Fatalf("create returned secret %q has_secret %v, want the submitted value once", createdBody.Secret, createdBody.HasSecret)
	}

	detail := serve(r, http.MethodGet, "/api/v2/projects/proj-1/webhooks/"+createdBody.ID, "")
	assertNoSecretMaterial(t, "GET after create", detail.Body.Bytes(), "caller-chosen-value")
}

// A create without a secret gets a generated one (#2959): every webhook is
// signed. The response carries it once, has_secret is true, and the value is
// a fresh 32-byte random string, different for each create.
func TestCreateWithoutASecretGeneratesOne(t *testing.T) {
	repo := &mockWebhookRepo{}
	r := webhookRouter(repo, entitledResolver())
	seen := map[string]bool{}
	for _, body := range []string{
		`{"url":"https://example.com/hook","events":[],"active":true}`,
		`{"url":"https://example.com/hook","events":[],"secret":"","active":true}`,
	} {
		created := serve(r, http.MethodPost, "/api/v2/projects/proj-1/webhooks/", body)
		if created.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", created.Code, created.Body.String())
		}
		object := decodeObject(t, created.Body.Bytes())
		var secret string
		if err := json.Unmarshal(object["secret"], &secret); err != nil || len(secret) < 43 {
			t.Fatalf("create without a secret returned no generated secret: %s", created.Body.String())
		}
		if seen[secret] {
			t.Fatal("two creates returned the same generated secret")
		}
		seen[secret] = true
		if string(object["has_secret"]) != "true" {
			t.Errorf("has_secret = %s, want true", object["has_secret"])
		}
	}
	for _, wh := range repo.webhooks {
		if wh.Secret == "" && wh.SecretRef == "" {
			t.Fatalf("a created webhook was stored with no secret: %+v", wh.redacted())
		}
	}
}

// Rotate generates a new value server-side and returns it once; the update
// route never echoes a secret, even one it was sent.
func TestRotateReturnsTheNewSecretOnceAndUpdateNever(t *testing.T) {
	repo := &mockWebhookRepo{webhooks: []Webhook{
		{ID: "wh-1", ProjectID: "proj-1", URL: "https://example.com/a", Events: []string{}, SecretRef: "{{secret.old}}", Active: true},
	}}
	r := webhookRouter(repo, entitledResolver())

	rotated := serve(r, http.MethodPost, "/api/v2/projects/proj-1/webhooks/wh-1/rotate_secret", "")
	if rotated.Code != http.StatusOK {
		t.Fatalf("rotate status = %d: %s", rotated.Code, rotated.Body.String())
	}
	var rotatedBody struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(rotated.Body.Bytes(), &rotatedBody); err != nil {
		t.Fatal(err)
	}
	if len(rotatedBody.Secret) != 43 || rotatedBody.Secret != repo.rotatedTo {
		t.Fatalf("rotate returned %q, stored %q; want one generated 256-bit value", rotatedBody.Secret, repo.rotatedTo)
	}
	detail := serve(r, http.MethodGet, "/api/v2/projects/proj-1/webhooks/wh-1", "")
	assertNoSecretMaterial(t, "GET after rotate", detail.Body.Bytes(), rotatedBody.Secret)

	updated := serve(r, http.MethodPut, "/api/v2/projects/proj-1/webhooks/wh-1",
		`{"url":"https://example.com/a","events":[],"secret":"sent-on-update","active":false}`)
	if updated.Code != http.StatusOK {
		t.Fatalf("update status = %d: %s", updated.Code, updated.Body.String())
	}
	assertNoSecretMaterial(t, "PUT", updated.Body.Bytes(), "sent-on-update", rotatedBody.Secret)
}

// Two generated secrets are distinct and 256 bits wide.
func TestNewSigningSecretIsRandom(t *testing.T) {
	first, err := NewSigningSecret()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewSigningSecret()
	if err != nil {
		t.Fatal(err)
	}
	if first == second || len(first) != 43 {
		t.Fatalf("NewSigningSecret returned %q and %q", first, second)
	}
}

func TestHandler_Delete(t *testing.T) {
	repo := &mockWebhookRepo{}
	r := webhookRouter(repo, entitledResolver())

	req := httptest.NewRequest(http.MethodDelete, "/api/v2/projects/proj-1/webhooks/wh-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
}
