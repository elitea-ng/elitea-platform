package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// Issue 2559: which credential may mint a personal access token.

type mintResolverStub struct {
	admin []string
	err   error
	calls int
}

func (s *mintResolverStub) ResolvePermissions(
	_ context.Context, _ identity.User, mode string, _ string,
) (identity.PermissionResolution, error) {
	s.calls++
	if mode != identity.PermissionModeAdministration {
		return identity.PermissionResolution{}, errors.New("unexpected mode " + mode)
	}
	if s.err != nil {
		return identity.PermissionResolution{}, s.err
	}
	return identity.PermissionResolution{UserID: 7, Permissions: s.admin}, nil
}

type mintCaller struct {
	name   string
	user   identity.User
	source identity.AuthenticationSource // AuthenticationSourceUnknown: no provenance at all
	bearer string                        // the Authorization bearer, when the case needs one
}

func boundProject(id int64) *int64 { return &id }

var (
	browserCaller = mintCaller{
		name:   "browser session",
		user:   identity.User{ID: "7", UserID: "7", AuthType: "session", BrowserSessionRef: 3},
		source: identity.AuthenticationSourceSession,
	}
	formCaller = mintCaller{
		// The Form session and the legacy signed cookie carry no session ref.
		name:   "form or legacy cookie session",
		user:   identity.User{ID: "7", UserID: "7", AuthType: "session"},
		source: identity.AuthenticationSourceSession,
	}
	patCaller = mintCaller{
		name:   "personal access token (bearer)",
		user:   identity.User{ID: "7", UserID: "7", TokenID: "42", AuthType: "token"},
		source: identity.AuthenticationSourceToken,
		bearer: "eyJhbGciOiJIUzUxMiJ9.e30.sig",
	}
	apiKeyCaller = mintCaller{
		name:   "personal access token (X-API-Key)",
		user:   identity.User{ID: "7", UserID: "7", TokenID: "42", AuthType: "token"},
		source: identity.AuthenticationSourceAPIKey,
	}
	runTokenCaller = mintCaller{
		// runtoken.Validator's principal: the actor and the actor's token row.
		name:   "run token",
		user:   identity.User{ID: "7", UserID: "7", TokenID: "43", AuthType: "token", TokenProjectID: boundProject(5)},
		source: identity.AuthenticationSourceToken,
		bearer: "elrun_abc",
	}
	nativeCaller = mintCaller{
		name:   "native device session",
		user:   identity.User{ID: "7", UserID: "7", TokenID: "44", AuthType: "token", NativeClientID: "elitea-desktop"},
		source: identity.AuthenticationSourceToken,
		bearer: "elnat_abc",
	}
)

// The callers whose kind cannot be established. Each one is an invariant
// break, refused with CodeCallerUnresolved and never told to "use a browser".
var unresolvedCallers = []mintCaller{
	{
		name: "principal without authentication provenance",
		user: identity.User{ID: "7", UserID: "7"},
	},
	{
		name:   "token principal under the session source",
		user:   identity.User{ID: "7", UserID: "7", TokenID: "42", AuthType: "token"},
		source: identity.AuthenticationSourceSession,
	},
	{
		name:   "development provenance",
		user:   identity.User{ID: "7", UserID: "7"},
		source: identity.AuthenticationSourceDevelopment,
	},
	{
		name:   "native bearer whose principal names no native client",
		user:   identity.User{ID: "7", UserID: "7", TokenID: "44", AuthType: "token"},
		source: identity.AuthenticationSourceToken,
		bearer: "elnat_abc",
	},
}

type mintOutcome struct {
	status    int
	code      string
	projectID *int64
	created   *tokenCreateInput
	annotated audit.Annotation
	audited   bool
}

func mint(t *testing.T, caller mintCaller, resolver identity.PermissionResolver, body string) mintOutcome {
	t.Helper()
	var created *tokenCreateInput
	handler := &Handler{
		tokenSigningKey: []byte("test-secret"),
		permissions:     resolver,
		tokens: tokenRepositoryStub{
			createFunc: func(_ context.Context, input tokenCreateInput) (tokenRecord, error) {
				copied := input
				created = &copied
				return tokenRecord{ID: 11, UUID: stringAddress("8ce4be49-0d10-4f05-a63f-d6d46f99a3f0"),
					UserID: input.OwnerID, Name: input.Name, ProjectID: input.ProjectID}, nil
			},
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/token/", bytes.NewBufferString(body))
	if caller.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+caller.bearer)
	}
	ctx := identity.ContextWithUser(req.Context(), caller.user)
	if caller.source != identity.AuthenticationSourceUnknown {
		ctx = identity.ContextWithAuthenticatedUser(req.Context(), caller.user, caller.source)
	}
	ctx, slot := audit.ContextWithAnnotationSlot(ctx)
	rec := httptest.NewRecorder()
	handler.Routes().ServeHTTP(rec, req.WithContext(ctx))

	outcome := mintOutcome{status: rec.Code, created: created}
	outcome.annotated, outcome.audited = slot.Read()
	if rec.Code == http.StatusForbidden || rec.Code == http.StatusInternalServerError {
		var envelope tokenMintJSONError
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("decode %d body %q: %v", rec.Code, rec.Body.String(), err)
		}
		if rec.Code == http.StatusForbidden && envelope.Error.Type != "permission_error" {
			t.Fatalf("403 type = %q, want permission_error", envelope.Error.Type)
		}
		outcome.code = envelope.Error.Code
		outcome.projectID = envelope.Error.ProjectID
	}
	return outcome
}

func assertRefusedAndAudited(t *testing.T, got mintOutcome, code string) {
	t.Helper()
	if got.status != http.StatusForbidden || got.code != code {
		t.Fatalf("status = %d code = %q, want 403 %s", got.status, got.code, code)
	}
	if got.created != nil {
		t.Fatalf("a refused credential created a token: %+v", got.created)
	}
	if !got.audited || got.annotated.Action != TokenMintRefusedAuditAction ||
		got.annotated.EntityType != "token" || got.annotated.EntityName != code {
		t.Fatalf("audit annotation = %+v (present %v), want action %s on token %s",
			got.annotated, got.audited, TokenMintRefusedAuditAction, code)
	}
}

func TestTokenMintAllowsBrowserSessions(t *testing.T) {
	for _, caller := range []mintCaller{browserCaller, formCaller} {
		t.Run(caller.name, func(t *testing.T) {
			resolver := &mintResolverStub{admin: []string{"admin.auth.users.super_admin"}}
			got := mint(t, caller, resolver, `{"name":"ci"}`)
			if got.status != http.StatusOK || got.created == nil || got.created.OwnerID != 7 {
				t.Fatalf("status = %d created = %+v, want 200 and a token for user 7", got.status, got.created)
			}
			if resolver.calls != 0 {
				t.Fatalf("resolver calls = %d: a browser session mints without an administration check", resolver.calls)
			}
		})
	}
}

func TestTokenMintRefusesTokensAndRunTokens(t *testing.T) {
	for _, caller := range []mintCaller{patCaller, apiKeyCaller, runTokenCaller} {
		t.Run(caller.name, func(t *testing.T) {
			got := mint(t, caller, &mintResolverStub{}, `{"name":"ci","project_id":5}`)
			assertRefusedAndAudited(t, got, CodeBrowserSessionRequired)
		})
	}
}

func TestTokenMintRefusesACallerItCannotClassify(t *testing.T) {
	for _, caller := range unresolvedCallers {
		t.Run(caller.name, func(t *testing.T) {
			got := mint(t, caller, &mintResolverStub{}, `{"name":"ci"}`)
			assertRefusedAndAudited(t, got, CodeCallerUnresolved)
		})
	}
}

// A refused credential is refused before the body is read and before the
// service check: it gets the same 403 for a malformed body, and on a
// deployment whose token service is not configured.
func TestTokenMintRefusesBeforeReadingTheBody(t *testing.T) {
	got := mint(t, patCaller, &mintResolverStub{}, `not json`)
	assertRefusedAndAudited(t, got, CodeBrowserSessionRequired)

	handler := &Handler{} // no repository, no signing key: 503 for anyone allowed
	req := httptest.NewRequest(http.MethodPost, "/token/", bytes.NewBufferString(`{"name":"x"}`))
	req = req.WithContext(identity.ContextWithAuthenticatedUser(req.Context(), patCaller.user, patCaller.source))
	rec := httptest.NewRecorder()
	handler.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unconfigured service, token caller: status = %d, want 403", rec.Code)
	}
}

func TestTokenMintNativeSession(t *testing.T) {
	cases := []struct {
		name     string
		resolver identity.PermissionResolver
		status   int
		code     string
	}{
		{"owner without administration permissions", &mintResolverStub{admin: nil}, http.StatusOK, ""},
		{"administrator", &mintResolverStub{admin: []string{"admin.auth.users.list"}}, http.StatusForbidden, CodeNativeAdmin},
		// A refusal is not "none held": a suspended owner, a missing token row
		// or a mismatched owner. The rule fails closed.
		{"owner the resolver refuses", &mintResolverStub{err: identity.ErrPermissionDenied}, http.StatusForbidden, CodeCallerUnresolved},
		{"no resolver composed", nil, http.StatusForbidden, CodeCallerUnresolved},
		{"resolver fault", &mintResolverStub{err: errors.New("pool timeout")}, http.StatusInternalServerError, mintFaultAuditName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mint(t, nativeCaller, tc.resolver, `{"name":"desktop"}`)
			if got.status != tc.status || got.code != tc.code {
				t.Fatalf("status = %d code = %q, want %d %q", got.status, got.code, tc.status, tc.code)
			}
			switch tc.status {
			case http.StatusOK:
				if got.created == nil {
					t.Fatal("an allowed native mint created no token")
				}
			case http.StatusForbidden:
				assertRefusedAndAudited(t, got, tc.code)
			default:
				if got.created != nil || got.annotated.Action != TokenMintRefusedAuditAction ||
					got.annotated.EntityName != mintFaultAuditName {
					t.Fatalf("fault: created = %+v annotation = %+v", got.created, got.annotated)
				}
			}
		})
	}
}

func TestTokenMintFromABoundCredentialStaysInItsProject(t *testing.T) {
	bound := nativeCaller
	bound.user.TokenProjectID = boundProject(5)
	cases := []struct {
		name   string
		body   string
		status int
	}{
		{"unbound request", `{"name":"x"}`, http.StatusForbidden},
		{"explicit null", `{"name":"x","project_id":null}`, http.StatusForbidden},
		{"another project", `{"name":"x","project_id":6}`, http.StatusForbidden},
		{"a malformed project", `{"name":"x","project_id":"5"}`, http.StatusForbidden},
		// The scope check runs before the other fields are validated.
		{"another project and no name", `{"project_id":6}`, http.StatusForbidden},
		{"another project and a bad expiry", `{"name":"x","project_id":6,"expires":{"measure":"years","value":1}}`, http.StatusForbidden},
		{"the same project", `{"name":"x","project_id":5}`, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mint(t, bound, &mintResolverStub{}, tc.body)
			if got.status != tc.status {
				t.Fatalf("status = %d code = %q, want %d", got.status, got.code, tc.status)
			}
			if tc.status != http.StatusForbidden {
				if got.created == nil || got.created.ProjectID == nil || *got.created.ProjectID != 5 {
					t.Fatalf("created = %+v, want a token bound to project 5", got.created)
				}
				return
			}
			assertRefusedAndAudited(t, got, CodeWiderThanCaller)
			if got.projectID == nil || *got.projectID != 5 {
				t.Fatalf("refusal project_id = %v, want the caller's own binding 5", got.projectID)
			}
		})
	}
}

// effectiveSet is what a credential can reach, in the two dimensions a
// personal access token has: the administration permissions it acts with, and
// the project it is confined to (nil: every project of its owner).
type effectiveSet struct {
	admin   map[string]bool
	project *int64
}

func (inner effectiveSet) within(outer effectiveSet) bool {
	for name := range inner.admin {
		if !outer.admin[name] {
			return false
		}
	}
	if outer.project == nil {
		return true
	}
	return inner.project != nil && *inner.project == *outer.project
}

// TestTokenMintIsNeverWiderThanTheCaller checks the invariant over every
// credential, owner and request shape: whenever a mint succeeds, the minted
// token's effective set is inside the calling credential's.
//
// A personal access token carries no scopes, so it acts with its OWNER's full
// administration set. A browser session acts with that same set. A native
// device session is not an administrator credential (ADR-0059 decision 7), so
// its administration set is empty, whatever its owner holds.
func TestTokenMintIsNeverWiderThanTheCaller(t *testing.T) {
	callers := append([]mintCaller{
		browserCaller, formCaller, patCaller, apiKeyCaller, runTokenCaller, nativeCaller,
	}, unresolvedCallers...)
	boundNative := nativeCaller
	boundNative.name = "bound native device session"
	boundNative.user.TokenProjectID = boundProject(5)
	callers = append(callers, boundNative)

	owners := map[string][]string{
		"member":        nil,
		"administrator": {"admin.auth.users.list", "admin.auth.users.super_admin"},
	}
	bodies := []string{`{"name":"x"}`, `{"name":"x","project_id":5}`, `{"name":"x","project_id":6}`}

	minted := 0
	for _, caller := range callers {
		for ownerName, ownerAdmin := range owners {
			for _, body := range bodies {
				got := mint(t, caller, &mintResolverStub{admin: ownerAdmin}, body)
				if got.status != http.StatusOK {
					continue
				}
				minted++
				callerSet := effectiveSet{admin: map[string]bool{}, project: caller.user.TokenProjectID}
				if caller.user.NativeClientID == "" {
					for _, name := range ownerAdmin {
						callerSet.admin[name] = true
					}
				}
				mintedSet := effectiveSet{admin: map[string]bool{}, project: got.created.ProjectID}
				for _, name := range ownerAdmin {
					mintedSet.admin[name] = true
				}
				if !mintedSet.within(callerSet) {
					t.Errorf("%s, %s owner, body %s: minted %+v is wider than the caller %+v",
						caller.name, ownerName, body, mintedSet, callerSet)
				}
			}
		}
	}
	if minted == 0 {
		t.Fatal("no mint succeeded, so the invariant was never exercised")
	}
}

// TestTokenRepositoryCreateHasOnlyTheGuardedCallers pins the two places a
// token row is created: the user-initiated route, which the credential-kind
// rule guards, and the server-initiated CallbackTokenMinter, which is always
// short-lived and bound to one project. A third caller would bypass the rule
// silently, so it must be added here on purpose.
func TestTokenRepositoryCreateHasOnlyTheGuardedCallers(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	callers := map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, source, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "Create" {
					return true
				}
				inner, ok := selector.X.(*ast.SelectorExpr)
				if ok && inner.Sel.Name == "tokens" {
					callers[fn.Name.Name] = true
				}
				return true
			})
		}
	}
	got := make([]string, 0, len(callers))
	for name := range callers {
		got = append(got, name)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "Mint,TokenCreate" {
		t.Fatalf("callers of tokens.Create = %v, want [Mint TokenCreate]", got)
	}
}
