package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applications"

	v2artifacts "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/artifacts"
)

// The S3-shaped bucket listing is the one artifact route that names its
// project in a QUERY parameter rather than a path segment, because that is
// the SDK's fixed wire format. A query parameter is caller-controlled input,
// so the danger is specific and worth its own tests: that this route becomes
// a way to read another tenant's artifacts by editing a query string.
//
// Route shape and RBAC-tier coverage live in router_security_test.go, where
// this route is a row in artifactRoutePermissions (401 unauthenticated, 403
// without the view permission) and in artifactSuccessCases (a genuine 200 at
// its real path). What follows is the cross-tenant guarantee those tables
// cannot express, because they only ever name one project.

func newS3ListingRouter(resolver fakePermissionResolver) http.Handler {
	return NewRouter(RouterConfig{
		AuthValidator:              testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:         testPrincipalValidator{},
		AppsRepo:                   struct{ applications.Repository }{},
		ArtifactHandler:            v2artifacts.NewHandler(alwaysSucceedsArtifactRepo{}, alwaysSucceedsArtifactStore{}),
		ArtifactPermissionResolver: resolver,
	})
}

// TestS3ListingRefusesAnotherProjectsBucket is the central authorization
// claim. The principal is genuinely entitled to project 7 and holds the exact
// permission the route requires — the only thing that changes between the two
// subtests is the project_id in the query string.
//
// alwaysSucceedsArtifactRepo answers GetBucket for ANY project id, so the
// handler itself would happily list project 8's bucket. That is deliberate:
// it means a 403 here can only come from the RBAC gate in front of the
// handler, and if that gate were dropped or given a path-based extractor
// (which would find no {projectID} param and fall through), this test would
// see a 200 instead. A repo that refused project 8 on its own would make the
// test pass without proving anything about authorization.
func TestS3ListingRefusesAnotherProjectsBucket(t *testing.T) {
	// forProject scopes the grant to project 7 exactly as the real
	// legacyrbac resolver does: its projectPermissions query keys on
	// (project_id, user_id) in auth_core__project_user_role, so a user with
	// no role assignment in the named project resolves to zero permissions.
	resolver := fakePermissionResolver{granted: []string{artifactPermissionView}, forProject: "7"}
	router := newS3ListingRouter(resolver)

	t.Run("own project is allowed", func(t *testing.T) {
		req := testAuthHeader(httptest.NewRequest(http.MethodGet, "/artifacts/s3/reports?project_id=7&list-type=2", nil))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 for the caller's own project; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("another project is refused", func(t *testing.T) {
		req := testAuthHeader(httptest.NewRequest(http.MethodGet, "/artifacts/s3/reports?project_id=8&list-type=2", nil))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 — the query parameter must not be trusted as an authorization claim; body=%s",
				rec.Code, rec.Body.String())
		}
	})
}

// TestS3ListingRejectsMalformedProjectID proves the query extractor fails
// closed. ProjectIDFromQuery accepts only a canonical positive integer, so
// each of these is refused before the handler runs — an extractor that
// coerced or ignored a malformed value could otherwise resolve permissions
// against a different project than the handler later reads.
func TestS3ListingRejectsMalformedProjectID(t *testing.T) {
	// The resolver grants view unconditionally here, so a 403 can only come
	// from the extractor rejecting the value, never from a missing grant.
	router := newS3ListingRouter(fakePermissionResolver{granted: []string{artifactPermissionView}})

	for _, query := range []string{
		"",                       // absent entirely
		"?project_id=",           // present but empty
		"?project_id=0",          // not positive
		"?project_id=-1",         // negative
		"?project_id=1.5",        // not an integer
		"?project_id=+1",         // non-canonical
		"?project_id=01",         // non-canonical leading zero
		"?project_id=1%20OR%201", // injection-shaped
		"?project_id=abc",        // not a number
	} {
		t.Run("project_id="+query, func(t *testing.T) {
			req := testAuthHeader(httptest.NewRequest(http.MethodGet, "/artifacts/s3/reports"+query, nil))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 for a malformed project id; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestS3DownloadRefusesAnotherProjectsObject is the same central claim for
// the object read, which is the route that actually carries bytes: a caller
// entitled to project 7 must not be able to read project 8's file by editing
// the query string. As above, alwaysSucceedsArtifactRepo/Store answer for ANY
// project, so a 403 can only come from the RBAC gate — if the download route
// were mounted without viewByQueryProject, or with the path-based extractor
// (which finds no {projectID} param and gates nothing), this would be a 200
// carrying the other tenant's object.
func TestS3DownloadRefusesAnotherProjectsObject(t *testing.T) {
	resolver := fakePermissionResolver{granted: []string{artifactPermissionView}, forProject: "7"}
	router := newS3ListingRouter(resolver)

	t.Run("own project is allowed", func(t *testing.T) {
		req := testAuthHeader(httptest.NewRequest(http.MethodGet, "/artifacts/s3/reports/folder/sub/file.txt?project_id=7", nil))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 for the caller's own project; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("another project is refused", func(t *testing.T) {
		req := testAuthHeader(httptest.NewRequest(http.MethodGet, "/artifacts/s3/reports/folder/sub/file.txt?project_id=8", nil))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 — the query parameter must not be trusted as an authorization claim; body=%s",
				rec.Code, rec.Body.String())
		}
	})
}

// TestS3DownloadRejectsMalformedProjectID is the download half of the
// fail-closed extractor check. It matters independently of the listing's:
// the two routes are separate mounts, and only a test that names this one
// proves the gate was applied to it.
func TestS3DownloadRejectsMalformedProjectID(t *testing.T) {
	router := newS3ListingRouter(fakePermissionResolver{granted: []string{artifactPermissionView}})

	for _, query := range []string{
		"",              // absent entirely
		"?project_id=",  // present but empty
		"?project_id=0", // not positive
		"?project_id=-1",
		"?project_id=01",
		"?project_id=abc",
	} {
		t.Run("project_id="+query, func(t *testing.T) {
			req := testAuthHeader(httptest.NewRequest(http.MethodGet, "/artifacts/s3/reports/a.txt"+query, nil))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 for a malformed project id; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestS3DownloadIsMountedAtRootNotUnderAPIV2 pins the download route's prefix
// and its wildcard key capture in one place — the two ways it can be mounted
// wrong, both of which produce a 404 that the SDK logs and swallows
// (artifact.py:_extend_data yields the document with no content), leaving a
// run green having indexed empty files.
func TestS3DownloadIsMountedAtRootNotUnderAPIV2(t *testing.T) {
	router := newS3ListingRouter(fakePermissionResolver{granted: []string{artifactPermissionView}})

	t.Run("root path serves a nested key", func(t *testing.T) {
		req := testAuthHeader(httptest.NewRequest(http.MethodGet, "/artifacts/s3/reports/folder/sub/file.txt?project_id=1", nil))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 at the path the SDK actually requests; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("api/v2 path does not serve it", func(t *testing.T) {
		req := testAuthHeader(httptest.NewRequest(http.MethodGet, "/api/v2/artifacts/s3/reports/file.txt?project_id=1", nil))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Fatalf("the download answered under /api/v2, where the SDK never looks; body=%s", rec.Body.String())
		}
	})
}

// TestS3ListingIsMountedAtRootNotUnderAPIV2 pins the prefix, which is the
// one thing that cannot be got wrong quietly: the SDK builds this URL from a
// bare origin with no /api/v2 segment (elitea-sdk client.py:115 — every
// sibling URL on the surrounding lines DOES include it), and the platform
// edge forwards the path verbatim. A route mounted one prefix off produces a
// 404 that the SDK swallows into an empty listing, which is indistinguishable
// from the original defect: an index run that reports success having indexed
// nothing.
func TestS3ListingIsMountedAtRootNotUnderAPIV2(t *testing.T) {
	router := newS3ListingRouter(fakePermissionResolver{granted: []string{artifactPermissionView}})

	t.Run("root path serves the listing", func(t *testing.T) {
		req := testAuthHeader(httptest.NewRequest(http.MethodGet, "/artifacts/s3/reports?project_id=1", nil))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 at the path the SDK actually requests; body=%s", rec.Code, rec.Body.String())
		}
		// Prove it is the listing and not some catch-all that happens to
		// 200 — the catch-all mount at "/" is a real possibility here.
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body is not JSON: %v (body=%s)", err, rec.Body.String())
		}
		if _, ok := body["contents"]; !ok {
			t.Fatalf(`body is not an S3 listing (no "contents" key); body=%s`, rec.Body.String())
		}
	})

	t.Run("api/v2 path does not serve it", func(t *testing.T) {
		req := testAuthHeader(httptest.NewRequest(http.MethodGet, "/api/v2/artifacts/s3/reports?project_id=1", nil))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Fatalf("the listing answered under /api/v2, where the SDK never looks; body=%s", rec.Body.String())
		}
	})
}

// ---------------------------------------------------------------------------
// The S3-shaped WRITE verbs. Everything above applies to them and more: a
// query parameter that is trusted as an authorization claim on a read leaks
// another tenant's artifacts, but on a write it lets one tenant create or
// destroy another's.
// ---------------------------------------------------------------------------

// s3WriteVerbs is the write surface under test, with the permission the
// router assigns each. Kept as a table so a verb added later without a
// deliberate permission decision shows up as a compile-time gap here rather
// than as a silently ungated route.
var s3WriteVerbs = []struct {
	name       string
	method     string
	permission string
	// wantStatus is what the real handler chain returns once RBAC allows the
	// request through (alwaysSucceedsArtifactRepo/Store answer for anything).
	wantStatus int
	body       string
}{
	{name: "PUT", method: http.MethodPut, permission: artifactPermissionCreate, wantStatus: http.StatusOK, body: "payload"},
	{name: "DELETE", method: http.MethodDelete, permission: artifactPermissionDelete, wantStatus: http.StatusNoContent},
	{name: "HEAD", method: http.MethodHead, permission: artifactPermissionView, wantStatus: http.StatusOK},
}

func newS3WriteRequest(method, target, body string) *http.Request {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := testAuthHeader(httptest.NewRequest(method, target, reader))
	if body != "" {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	return req
}

// TestS3WriteVerbsRefuseAnotherProjectsBucket is the central authorization
// claim for the write half. The principal is genuinely entitled to project 7
// and holds every permission the routes require — the only thing that changes
// between the subtests is the project_id in the query string.
//
// alwaysSucceedsArtifactRepo/Store answer for ANY project, so the handler
// itself would happily write into (or delete out of) project 8. That is what
// makes this meaningful: a 403 here can only come from the RBAC gate in front
// of the handler, and if that gate were dropped — or given the path-based
// extractor, which finds no {projectID} param and therefore gates nothing —
// this would be a 200 that had actually modified another tenant's bucket.
func TestS3WriteVerbsRefuseAnotherProjectsBucket(t *testing.T) {
	for _, verb := range s3WriteVerbs {
		t.Run(verb.name, func(t *testing.T) {
			router := newS3ListingRouter(fakePermissionResolver{
				granted:    allArtifactPermissions,
				forProject: "7",
			})

			t.Run("own project is allowed", func(t *testing.T) {
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, newS3WriteRequest(verb.method, "/artifacts/s3/reports/folder/sub/file.txt?project_id=7", verb.body))
				if rec.Code != verb.wantStatus {
					t.Fatalf("status = %d, want %d for the caller's own project; body=%s", rec.Code, verb.wantStatus, rec.Body.String())
				}
			})

			t.Run("another project is refused", func(t *testing.T) {
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, newS3WriteRequest(verb.method, "/artifacts/s3/reports/folder/sub/file.txt?project_id=8", verb.body))
				if rec.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403 — the query parameter must not be trusted as an authorization claim; body=%s",
						rec.Code, rec.Body.String())
				}
			})
		})
	}
}

// TestS3WriteVerbsRejectMalformedProjectID proves each write route's extractor
// fails closed independently. It matters per-route because each is its own
// mount: only a test that names this method proves the gate was applied to it.
func TestS3WriteVerbsRejectMalformedProjectID(t *testing.T) {
	// The resolver grants everything, so a 403 can only come from the
	// extractor rejecting the value, never from a missing grant.
	router := newS3ListingRouter(fakePermissionResolver{granted: allArtifactPermissions})

	for _, verb := range s3WriteVerbs {
		for _, query := range []string{
			"",              // absent entirely
			"?project_id=",  // present but empty
			"?project_id=0", // not positive
			"?project_id=-1",
			"?project_id=01", // non-canonical leading zero
			"?project_id=abc",
		} {
			t.Run(verb.name+" project_id="+query, func(t *testing.T) {
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, newS3WriteRequest(verb.method, "/artifacts/s3/reports/a.txt"+query, verb.body))
				if rec.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403 for a malformed project id; body=%s", rec.Code, rec.Body.String())
				}
			})
		}
	}
}

// TestS3WriteVerbsRequireTheirOwnPermissionTier is the "a write must not be
// authorized by a view permission" claim, stated exhaustively: for every
// (verb, single granted permission) pair, the request succeeds if and only if
// the granted permission is the one that verb requires.
//
// The view-only row is the one that matters most — before these routes
// existed the only S3 gate in the router was `view`, so reusing it here would
// have made an agent that can merely read a project's artifacts able to
// overwrite and delete them. The delete/create rows are the other half: they
// forbid the two write tiers from standing in for each other.
func TestS3WriteVerbsRequireTheirOwnPermissionTier(t *testing.T) {
	const target = "/artifacts/s3/reports/folder/sub/file.txt?project_id=1"

	for _, verb := range s3WriteVerbs {
		for _, granted := range allArtifactPermissions {
			wantStatus := http.StatusForbidden
			if granted == verb.permission {
				wantStatus = verb.wantStatus
			}
			t.Run(verb.name+" with "+granted, func(t *testing.T) {
				router := newS3ListingRouter(fakePermissionResolver{granted: []string{granted}})
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, newS3WriteRequest(verb.method, target, verb.body))
				if rec.Code != wantStatus {
					t.Fatalf("granted %q on %s: status = %d, want %d; body=%s",
						granted, verb.name, rec.Code, wantStatus, rec.Body.String())
				}
			})
		}
	}
}

// TestS3WriteVerbsAreMountedAtRootNotUnderAPIV2 pins each write verb's prefix
// and its wildcard key capture — the two ways a route can be mounted wrong.
// Both produce a 404 the SDK cannot act on, and for HEAD specifically a 404 is
// not an error at all: head_artifact_s3 reports {"exists": False}, so a
// missing route reads as "the file is not there" and an agent's next step
// proceeds on a false premise.
func TestS3WriteVerbsAreMountedAtRootNotUnderAPIV2(t *testing.T) {
	router := newS3ListingRouter(fakePermissionResolver{granted: allArtifactPermissions})

	for _, verb := range s3WriteVerbs {
		t.Run(verb.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, newS3WriteRequest(verb.method, "/artifacts/s3/reports/folder/sub/file.txt?project_id=1", verb.body))
			if rec.Code != verb.wantStatus {
				t.Fatalf("root path: status = %d, want %d at the path the SDK actually requests; body=%s",
					rec.Code, verb.wantStatus, rec.Body.String())
			}

			underAPIV2 := httptest.NewRecorder()
			router.ServeHTTP(underAPIV2, newS3WriteRequest(verb.method, "/api/v2/artifacts/s3/reports/file.txt?project_id=1", verb.body))
			if underAPIV2.Code == verb.wantStatus {
				t.Fatalf("the verb answered under /api/v2, where the SDK never looks; body=%s", underAPIV2.Body.String())
			}
		})
	}
}
