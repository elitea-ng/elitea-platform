package routes_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/routes"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhub"
)

type forwarded struct{ path, project, user, method string }

func table(record *[]forwarded, invoke http.HandlerFunc) routes.Table {
	return routes.Table{
		SlotsPath:        "/x/slots/{project_id}",
		InvokePath:       "/x/tools/{project_id}/{toolkit_name}/{tool_name}/invoke",
		InvocationPath:   "/x/invocations/{project_id}/{toolkit_name}/{tool_name}/{invocation_id}",
		Mode:             "default",
		ReadPermission:   "x.read",
		InvokePermission: "x.invoke",
		Auth:             apimw.AuthConfig{Validator: tokens{}, PrincipalValidator: principals{}, SessionSecret: "s"},
		Permissions:      resolver{},
		Forward: func(w http.ResponseWriter, r *http.Request, providerPath, projectID, userID string) {
			*record = append(*record, forwarded{providerPath, projectID, userID, r.Method})
			w.WriteHeader(http.StatusOK)
		},
		Invoke: invoke,
		UserID: func(*http.Request) string { return "7" },
	}
}

func do(h http.Handler, method, path, bearer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestEveryRouteForwardsToItsProviderPath(t *testing.T) {
	var got []forwarded
	h, err := routes.Build(table(&got, nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ method, path, want string }{
		{http.MethodGet, "/x/slots/1", "/slots"},
		{http.MethodPost, "/x/tools/1/Wikis/generate_wiki/invoke", "/tools/Wikis/generate_wiki/invoke"},
		{http.MethodGet, "/x/invocations/1/Wikis/generate_wiki/inv-1", "/tools/Wikis/generate_wiki/invocations/inv-1"},
		{http.MethodDelete, "/x/invocations/1/Wikis/generate_wiki/inv-1", "/tools/Wikis/generate_wiki/invocations/inv-1"},
	} {
		got = nil
		if w := do(h, c.method, c.path, "all"); w.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", c.method, c.path, w.Code, w.Body.String())
		}
		if len(got) != 1 || got[0].path != c.want || got[0].project != "1" || got[0].user != "7" {
			t.Fatalf("%s %s forwarded %+v, want %s for project 1 user 7", c.method, c.path, got, c.want)
		}
	}
}

func TestPollingIsReadingAndCancellingIsInvoking(t *testing.T) {
	var got []forwarded
	h, err := routes.Build(table(&got, nil))
	if err != nil {
		t.Fatal(err)
	}
	// A reader may poll but not cancel; an invoker may cancel.
	if w := do(h, http.MethodGet, "/x/invocations/1/Wikis/t/inv-1", "reader"); w.Code != http.StatusOK {
		t.Fatalf("a reader could not poll: %d", w.Code)
	}
	if w := do(h, http.MethodDelete, "/x/invocations/1/Wikis/t/inv-1", "reader"); w.Code != http.StatusForbidden {
		t.Fatalf("a reader cancelled: %d", w.Code)
	}
	if w := do(h, http.MethodPost, "/x/tools/1/Wikis/t/invoke", "reader"); w.Code != http.StatusForbidden {
		t.Fatalf("a reader invoked: %d", w.Code)
	}
	if w := do(h, http.MethodGet, "/x/slots/1", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no credential reached the hop: %d", w.Code)
	}
	if len(got) != 1 {
		t.Fatalf("the hop saw %d requests, want the one poll", len(got))
	}
}

func TestAnInvokeOverrideServesThePostBehindTheSameGuard(t *testing.T) {
	var got []forwarded
	override := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusAccepted) }
	h, err := routes.Build(table(&got, override))
	if err != nil {
		t.Fatal(err)
	}
	if w := do(h, http.MethodPost, "/x/tools/1/Wikis/t/invoke", "all"); w.Code != http.StatusAccepted {
		t.Fatalf("the override did not serve the invoke: %d", w.Code)
	}
	if len(got) != 0 {
		t.Fatal("the override was forwarded as well")
	}
	if w := do(h, http.MethodPost, "/x/tools/1/Wikis/t/invoke", "reader"); w.Code != http.StatusForbidden {
		t.Fatalf("the override bypassed the guard: %d", w.Code)
	}
}

func TestAdmissionRefusesTheInvokeAndNothingElse(t *testing.T) {
	for _, c := range []struct{ reason, wantMessage string }{
		// The sentence for `inactive` is providerhub's constant, so the row an
		// operator reads and the refusal a user reads say the same thing.
		{"provider_admission_inactive", providerhub.InactiveReason},
		{"provider_admission_revoked", "revoked"},
	} {
		var got []forwarded
		asked := 0
		x := table(&got, nil)
		x.Admission = func(*http.Request) (bool, string) { asked++; return false, c.reason }
		h, err := routes.Build(x)
		if err != nil {
			t.Fatal(err)
		}

		w := do(h, http.MethodPost, "/x/tools/1/Wikis/generate_wiki/invoke", "all")
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("a refused invoke answered %d, want 503", w.Code)
		}
		// NEVER REACHED THE FORWARDER. A 503 written after the hop would mean
		// the provider had already started the work the refusal denies.
		if len(got) != 0 {
			t.Fatalf("a refused invoke still forwarded: %+v", got)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("the refusal is %q, not JSON", ct)
		}
		var body map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("the refusal body is not JSON: %v (%s)", err, w.Body.String())
		}
		if body["reason"] != c.reason {
			t.Fatalf("the refusal carries reason %q, want %q", body["reason"], c.reason)
		}
		if !strings.Contains(body["message"], c.wantMessage) {
			t.Fatalf("the %s message is %q, want it to mention %q", c.reason, body["message"], c.wantMessage)
		}

		// SLOTS, POLLING AND CANCELLING ARE NOT GATED. A revocation stops new
		// work; it must not blind or strand a run that was already accepted.
		got = nil
		for _, r := range []struct{ method, path string }{
			{http.MethodGet, "/x/slots/1"},
			{http.MethodGet, "/x/invocations/1/Wikis/generate_wiki/inv-1"},
			{http.MethodDelete, "/x/invocations/1/Wikis/generate_wiki/inv-1"},
		} {
			if w := do(h, r.method, r.path, "all"); w.Code != http.StatusOK {
				t.Fatalf("%s %s answered %d while the provider was refused", r.method, r.path, w.Code)
			}
		}
		if len(got) != 3 {
			t.Fatalf("%d of the three ungated routes reached the hop", len(got))
		}
		if asked != 1 {
			t.Fatalf("the gate was asked %d times, want once — only the invoke may consult it", asked)
		}
	}
}

func TestAnAdmittedInvokeAndANilHookBothForward(t *testing.T) {
	// Allowed: the invoke is served exactly as it is without a hook.
	var got []forwarded
	allowing := table(&got, nil)
	allowing.Admission = func(*http.Request) (bool, string) { return true, "" }
	h, err := routes.Build(allowing)
	if err != nil {
		t.Fatal(err)
	}
	if w := do(h, http.MethodPost, "/x/tools/1/Wikis/generate_wiki/invoke", "all"); w.Code != http.StatusOK {
		t.Fatalf("an admitted invoke answered %d", w.Code)
	}
	if len(got) != 1 || got[0].path != "/tools/Wikis/generate_wiki/invoke" {
		t.Fatalf("an admitted invoke forwarded %+v", got)
	}

	// A facade's own invoke override is gated too — DeepWiki's rewrite is the
	// invoke, and a gate that only covered the plain forward would leave the
	// one provider this exists for ungated.
	overridden := 0
	refusing := table(&got, func(w http.ResponseWriter, r *http.Request) { overridden++; w.WriteHeader(http.StatusAccepted) })
	refusing.Admission = func(*http.Request) (bool, string) { return false, "provider_admission_revoked" }
	h, err = routes.Build(refusing)
	if err != nil {
		t.Fatal(err)
	}
	if w := do(h, http.MethodPost, "/x/tools/1/Wikis/generate_wiki/invoke", "all"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("the override bypassed admission: %d", w.Code)
	}
	if overridden != 0 {
		t.Fatal("the override ran behind a refusal")
	}

	// Refused BEHIND the guard, not in front of it: a caller with no
	// credential still learns nothing about which providers this deployment
	// admits.
	asked := 0
	counting := table(&got, nil)
	counting.Admission = func(*http.Request) (bool, string) { asked++; return false, "provider_admission_revoked" }
	h, err = routes.Build(counting)
	if err != nil {
		t.Fatal(err)
	}
	if w := do(h, http.MethodPost, "/x/tools/1/Wikis/generate_wiki/invoke", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated invoke answered %d, want 401", w.Code)
	}
	if w := do(h, http.MethodPost, "/x/tools/1/Wikis/generate_wiki/invoke", "reader"); w.Code != http.StatusForbidden {
		t.Fatalf("an unpermitted invoke answered %d, want 403", w.Code)
	}
	if asked != 0 {
		t.Fatalf("the gate was consulted %d times for callers the guard turns away", asked)
	}
}

func TestAnIncompleteTableIsRefused(t *testing.T) {
	var got []forwarded
	complete := table(&got, nil)
	cases := map[string]func(*routes.Table){
		"no slots path":          func(x *routes.Table) { x.SlotsPath = "" },
		"no invoke permission":   func(x *routes.Table) { x.InvokePermission = "" },
		"no hop":                 func(x *routes.Table) { x.Forward = nil },
		"no principal validator": func(x *routes.Table) { x.Auth.PrincipalValidator = nil },
		"no permission resolver": func(x *routes.Table) { x.Permissions = nil },
	}
	for name, breakIt := range cases {
		broken := complete
		breakIt(&broken)
		if _, err := routes.Build(broken); !errors.Is(err, routes.ErrIncompleteTable) {
			t.Errorf("%s was accepted: %v", name, err)
		}
	}
}

// The bearer names the caller's permissions: "all" or "reader".
type tokens struct{}

func (tokens) ValidateToken(_ context.Context, token string) (auth.User, error) {
	if token != "all" && token != "reader" {
		return auth.User{}, auth.ErrCredentialRejected
	}
	return auth.User{ID: "7", UserID: "7", Name: token}, nil
}

type principals struct{}

func (principals) ValidatePrincipal(_ context.Context, u auth.User) (auth.User, error) { return u, nil }

type resolver struct{}

func (resolver) ResolvePermissions(_ context.Context, u auth.User, _, _ string) (auth.PermissionResolution, error) {
	if strings.EqualFold(u.Name, "all") {
		return auth.PermissionResolution{UserID: 7, Permissions: []string{"x.read", "x.invoke"}}, nil
	}
	return auth.PermissionResolution{UserID: 7, Permissions: []string{"x.read"}}, nil
}
