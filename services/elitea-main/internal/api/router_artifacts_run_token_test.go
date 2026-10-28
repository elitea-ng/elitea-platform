package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v2artifacts "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/artifacts"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/runtoken"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applications"
)

// The five artifact routes elitea-main's ArtifactObjectService calls reach
// for the worker (internal/transport/artifactgrpc): CreateBucket,
// ListObjects, PutObject, GetObject, Stat. A governed run's run token is
// admitted on these and no other artifact route (#1461).
var runTokenArtifactRoutes = map[string]bool{
	"create bucket":   true,
	"list objects":    true,
	"upload object":   true,
	"download object": true,
	"stat object":     true,
}

type switchableLeases struct{ live *bool }

func (l switchableLeases) ClaimLive(context.Context, runtoken.Binding) (bool, error) {
	return *l.live, nil
}

func runTokenArtifactRouter(t *testing.T, live *bool) (http.Handler, string) {
	t.Helper()
	signer, err := runtoken.NewSigner(runtoken.DeriveKey([]byte("pat-signing-key")))
	if err != nil {
		t.Fatal(err)
	}
	// Project 1: the project every artifactSuccessCases path names.
	token, err := signer.Issue(runtoken.Binding{
		ExecutionID: "exec-1", Generation: 1, ClaimID: "claim-1",
		FenceToken: bytes.Repeat([]byte{1}, 32), ProjectID: 1, UserID: 1, TokenID: 11,
	})
	if err != nil {
		t.Fatal(err)
	}
	validator, err := runtoken.NewValidator(signer, switchableLeases{live: live})
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter(RouterConfig{
		AuthValidator:              testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:         testPrincipalValidator{},
		AppsRepo:                   struct{ applications.Repository }{},
		ArtifactHandler:            v2artifacts.NewHandler(alwaysSucceedsArtifactRepo{}, alwaysSucceedsArtifactStore{}),
		ArtifactPermissionResolver: fakePermissionResolver{granted: allArtifactPermissions},
		RunTokens:                  validator,
	})
	return router, token
}

func serveRunTokenCase(router http.Handler, t *testing.T, token string, sc artifactSuccessCase, path string) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	var contentType string
	if sc.newBody != nil {
		body, contentType = sc.newBody(t)
	}
	request := httptest.NewRequest(sc.method, path, body)
	request.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

// TestRunTokenReachesOnlyTheWorkersArtifactRoutes: the run token reads and
// writes its own project's artifacts on the worker's five routes, and every
// other artifact route — the rest of the native plane and the S3-shaped
// ones — answers 403 run_token_audience.
func TestRunTokenReachesOnlyTheWorkersArtifactRoutes(t *testing.T) {
	live := true
	router, token := runTokenArtifactRouter(t, &live)
	seen := map[string]bool{}
	for _, sc := range artifactSuccessCases {
		t.Run(sc.desc, func(t *testing.T) {
			response := serveRunTokenCase(router, t, token, sc, sc.path)
			if runTokenArtifactRoutes[sc.desc] {
				seen[sc.desc] = true
				if response.Code != sc.wantStatus {
					t.Fatalf("status = %d, want %d; body=%s", response.Code, sc.wantStatus, response.Body.String())
				}
				return
			}
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body=%s", response.Code, response.Body.String())
			}
			if sc.method != http.MethodHead && !strings.Contains(response.Body.String(), "run_token_audience") {
				t.Fatalf("body = %s, want run_token_audience", response.Body.String())
			}
		})
	}
	if len(seen) != len(runTokenArtifactRoutes) {
		t.Fatalf("exercised %v, want every worker route %v", seen, runTokenArtifactRoutes)
	}
}

// TestRunTokenArtifactRoutesStayInTheRunsProject: on the worker's routes the
// token is refused (403 run_token_scope) for any other project, wherever the
// request names it, and refused (401) once the claim's lease is gone.
func TestRunTokenArtifactRoutesStayInTheRunsProject(t *testing.T) {
	live := true
	router, token := runTokenArtifactRouter(t, &live)
	for _, sc := range artifactSuccessCases {
		if !runTokenArtifactRoutes[sc.desc] {
			continue
		}
		t.Run(sc.desc, func(t *testing.T) {
			other := strings.Replace(sc.path, "/1/", "/2/", 1)
			if other == sc.path {
				other = strings.Replace(sc.path, "/1", "/2", 1)
			}
			if response := serveRunTokenCase(router, t, token, sc, other); response.Code != http.StatusForbidden {
				t.Fatalf("%s: status = %d, want 403; body=%s", other, response.Code, response.Body.String())
			}
			sep := "?"
			if strings.Contains(sc.path, "?") {
				sep = "&"
			}
			if response := serveRunTokenCase(router, t, token, sc, sc.path+sep+"project_id=2"); response.Code != http.StatusForbidden {
				t.Fatalf("project_id query: status = %d, want 403", response.Code)
			}

			live = false
			defer func() { live = true }()
			if response := serveRunTokenCase(router, t, token, sc, sc.path); response.Code != http.StatusUnauthorized {
				t.Fatalf("lapsed lease: status = %d, want 401", response.Code)
			}
		})
	}
}

// A user's credential still reaches every artifact route, and an unknown
// artifact path is still authenticated before it is a 404.
func TestArtifactRoutesKeepUserAuthenticationAfterTheSplit(t *testing.T) {
	live := true
	router, _ := runTokenArtifactRouter(t, &live)
	for _, sc := range artifactSuccessCases {
		var body io.Reader
		var contentType string
		if sc.newBody != nil {
			body, contentType = sc.newBody(t)
		}
		request := testAuthHeader(httptest.NewRequest(sc.method, sc.path, body))
		if contentType != "" {
			request.Header.Set("Content-Type", contentType)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != sc.wantStatus {
			t.Fatalf("%s: status = %d, want %d; body=%s", sc.desc, response.Code, sc.wantStatus, response.Body.String())
		}
	}
	unknown := httptest.NewRecorder()
	router.ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/api/v2/artifacts/nothing/here", nil))
	if unknown.Code != http.StatusUnauthorized {
		t.Fatalf("unknown path without a credential: status = %d, want 401", unknown.Code)
	}
	known := httptest.NewRecorder()
	router.ServeHTTP(known, testAuthHeader(httptest.NewRequest(http.MethodGet, "/api/v2/artifacts/nothing/here", nil)))
	if known.Code != http.StatusNotFound {
		t.Fatalf("unknown path with a credential: status = %d, want 404", known.Code)
	}
	// A method the path does not serve: authenticated first, then chi's 405
	// with the Allow header.
	const listPath = "/api/v2/artifacts/objects/1/bucket"
	anonymous := httptest.NewRecorder()
	router.ServeHTTP(anonymous, httptest.NewRequest(http.MethodDelete, listPath, nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("wrong method without a credential: status = %d, want 401", anonymous.Code)
	}
	wrongMethod := httptest.NewRecorder()
	router.ServeHTTP(wrongMethod, testAuthHeader(httptest.NewRequest(http.MethodDelete, listPath, nil)))
	if wrongMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method with a credential: status = %d, want 405", wrongMethod.Code)
	}
	if allow := strings.Join(wrongMethod.Header().Values("Allow"), ","); allow != "GET,POST" {
		t.Fatalf("Allow = %q, want GET,POST", allow)
	}
}
