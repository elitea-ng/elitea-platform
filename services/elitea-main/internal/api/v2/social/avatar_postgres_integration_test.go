package social_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	platformapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api"
	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/social"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	dbrepos "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestCurrentAvatarProductionHTTPPostgresParityAndNegativeSecurity proves the
// upload endpoint actually lands the file in the object store (not just a 200
// with an unchecked body), the fetch endpoint round-trips that exact URL, and
// RBAC matches the other current_* social routes (models.social.avatar.*),
// following the #128 pattern: assert via the storage API and persisted
// centry.social_users row, not HTTP status alone.
func TestCurrentAvatarProductionHTTPPostgresParityAndNegativeSecurity(t *testing.T) {
	pool := newCurrentAvatarPostgresPool(t)
	prepareCurrentAvatarDatabase(t, pool)

	store, err := dbrepos.NewCurrentSocialAvatarRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	objectStore := newFakeAvatarObjectStore()
	if _, err := handler.NewCurrentAvatarRoute(
		store,
		objectStore,
		apimw.AuthConfig{
			PrincipalValidator: nil,
			ForwardedIdentityVerifier: currentAvatarIntegrationPeerVerifier{
				trustedRemote: "10.0.0.8:43120",
			},
		},
		legacyrbac.NewPostgresResolver(pool),
	); err == nil {
		t.Fatal("expected NewCurrentAvatarRoute to reject a nil PrincipalValidator")
	}

	route, err := handler.NewCurrentAvatarRoute(
		store,
		objectStore,
		apimw.AuthConfig{
			PrincipalValidator: currentAvatarPrincipalValidator{},
			ForwardedIdentityVerifier: currentAvatarIntegrationPeerVerifier{
				trustedRemote: "10.0.0.8:43120",
			},
		},
		legacyrbac.NewPostgresResolver(pool),
	)
	if err != nil {
		t.Fatal(err)
	}
	router := platformapi.NewRouter(platformapi.RouterConfig{
		CurrentSocialAvatar: route,
		ObjectStore:         objectStore,
	})

	t.Run("upload lands the object in storage and fetch round-trips it", func(t *testing.T) {
		body, contentType := currentAvatarMultipartBody(t, "me.png", []byte("fake-png-bytes"))
		uploadRequest := currentAvatarIntegrationRequest(http.MethodPut, "/api/v2/social/avatar/7", "11", "10.0.0.8:43120", body)
		uploadRequest.Header.Set("Content-Type", contentType)
		uploadResponse := httptest.NewRecorder()
		router.ServeHTTP(uploadResponse, uploadRequest)
		if uploadResponse.Code != http.StatusOK {
			t.Fatalf("upload status=%d body=%q", uploadResponse.Code, uploadResponse.Body.String())
		}
		var uploaded struct{ Avatar *string }
		if err := json.Unmarshal(uploadResponse.Body.Bytes(), &uploaded); err != nil || uploaded.Avatar == nil {
			t.Fatalf("decode upload response %q: %v", uploadResponse.Body.String(), err)
		}
		url := *uploaded.Avatar
		if !strings.HasPrefix(url, "/avatars/7/") {
			t.Fatalf("unexpected avatar url %q", url)
		}

		// #128 pattern: assert via the storage API, not just the HTTP 200.
		filename := strings.TrimPrefix(url, "/avatars/7/")
		ref, err := storage.NewObjectRef("7", "avatars", filename)
		if err != nil {
			t.Fatal(err)
		}
		stored, _, err := objectStore.Get(context.Background(), ref, nil)
		if err != nil {
			t.Fatalf("object never landed in storage: %v", err)
		}
		data, _ := io.ReadAll(stored)
		if string(data) != "fake-png-bytes" {
			t.Fatalf("stored object mismatch: %q", data)
		}

		// Fetch round-trips the exact URL upload returned.
		fetchRequest := currentAvatarIntegrationRequest(http.MethodGet, "/api/v2/social/avatar/7", "11", "10.0.0.8:43120", nil)
		fetchResponse := httptest.NewRecorder()
		router.ServeHTTP(fetchResponse, fetchRequest)
		want := fmt.Sprintf(`{"avatar":%q}`+"\n", url)
		if fetchResponse.Code != http.StatusOK || fetchResponse.Body.String() != want {
			t.Fatalf("fetch status=%d body=%q want=%q", fetchResponse.Code, fetchResponse.Body.String(), want)
		}

		// The download route is what a browser <img src> actually hits — an
		// HTTP GET through it, not just a direct ObjectStore.Get call above.
		downloadRequest := httptest.NewRequest(http.MethodGet, url, nil)
		downloadResponse := httptest.NewRecorder()
		router.ServeHTTP(downloadResponse, downloadRequest)
		if downloadResponse.Code != http.StatusOK || downloadResponse.Body.String() != "fake-png-bytes" {
			t.Fatalf("download status=%d body=%q", downloadResponse.Code, downloadResponse.Body.String())
		}
		if contentType := downloadResponse.Header().Get("Content-Type"); contentType != "image/png" {
			t.Fatalf("download content-type=%q", contentType)
		}
	})

	t.Run("fetch for a user with no avatar returns null, not an error", func(t *testing.T) {
		request := currentAvatarIntegrationRequest(http.MethodGet, "/api/v2/social/avatar/7", "12", "10.0.0.8:43120", nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		const want = `{"avatar":null}` + "\n"
		if response.Code != http.StatusOK || response.Body.String() != want {
			t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
		}
	})

	// The PER-USER property (#402).
	//
	// #402 widened `models.social.avatar.get` and `models.social.avatar.update`
	// to the editor and the viewer, because both routes act on the CALLER's own
	// record. That widening is only safe if the route really is per-user. So
	// this subtest measures the property directly, and it measures it in both
	// directions: a caller changes their own row, and a caller cannot reach
	// another user's row.
	//
	// The route offers no way to name another user — there is no user id in the
	// path, and `currentAvatarUserID` reads the principal. A test that only
	// uploaded as two users would not prove that, because it would never TRY to
	// address someone else. So the second upload below carries a `user_id` form
	// field AND a `?user_id=` query parameter that both name user 11, and user
	// 11's row must be untouched.
	//
	// Users 11 and 14 share project role 101, so the two callers are equally
	// entitled. Any difference in outcome is therefore about identity, not about
	// permission.
	t.Run("a caller writes only their own avatar row", func(t *testing.T) {
		before := readStoredAvatar(t, pool, 11)
		if before == "" {
			t.Fatal("user 11 has no avatar yet, so this test cannot detect an overwrite")
		}

		body, contentType := currentAvatarMultipartBody(t, "second.png", []byte("second-user-bytes"))
		request := currentAvatarIntegrationRequest(
			http.MethodPut, "/api/v2/social/avatar/7?user_id=11", "14", "10.0.0.8:43120", body)
		request.Header.Set("Content-Type", contentType)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("upload status=%d body=%q", response.Code, response.Body.String())
		}
		var uploaded struct{ Avatar *string }
		if err := json.Unmarshal(response.Body.Bytes(), &uploaded); err != nil || uploaded.Avatar == nil {
			t.Fatalf("decode upload response %q: %v", response.Body.String(), err)
		}

		// The caller's OWN row carries the new URL.
		if stored := readStoredAvatar(t, pool, 14); stored != *uploaded.Avatar {
			t.Fatalf("user 14's stored avatar=%q, want %q", stored, *uploaded.Avatar)
		}
		// The other user's row is untouched, despite the `user_id` field and
		// the `?user_id=` parameter that both named them.
		if after := readStoredAvatar(t, pool, 11); after != before {
			t.Fatalf("user 11's avatar changed from %q to %q; the route let one caller "+
				"write another user's record", before, after)
		}
		if *uploaded.Avatar == before {
			t.Fatal("both users share one avatar URL; the route is not per-user")
		}

		// Each GET answers with the caller's own row.
		for _, expectation := range []struct {
			userID string
			want   string
		}{{"11", before}, {"14", *uploaded.Avatar}} {
			fetch := currentAvatarIntegrationRequest(
				http.MethodGet, "/api/v2/social/avatar/7", expectation.userID, "10.0.0.8:43120", nil)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, fetch)
			want := fmt.Sprintf(`{"avatar":%q}`+"\n", expectation.want)
			if recorder.Code != http.StatusOK || recorder.Body.String() != want {
				t.Fatalf("user %s fetch status=%d body=%q want=%q",
					expectation.userID, recorder.Code, recorder.Body.String(), want)
			}
		}
	})

	for _, test := range []struct {
		name          string
		method        string
		userID        string
		withoutHeader bool
		wantStatus    int
	}{
		{name: "authentication required", method: http.MethodGet, withoutHeader: true, wantStatus: http.StatusUnauthorized},
		{name: "wrong permission on GET", method: http.MethodGet, userID: "13", wantStatus: http.StatusForbidden},
		{name: "wrong permission on PUT", method: http.MethodPut, userID: "12", wantStatus: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			var body io.Reader
			var contentType string
			if test.method == http.MethodPut {
				body, contentType = currentAvatarMultipartBody(t, "me.png", []byte("irrelevant"))
			}
			request := currentAvatarIntegrationRequest(test.method, "/api/v2/social/avatar/7", test.userID, "10.0.0.8:43120", body)
			if contentType != "" {
				request.Header.Set("Content-Type", contentType)
			}
			if test.withoutHeader {
				request.Header.Del("X-Auth-Type")
				request.Header.Del("X-Auth-ID")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d want=%d body=%q", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
}

type currentAvatarPrincipalValidator struct{}

func (currentAvatarPrincipalValidator) ValidatePrincipal(
	_ context.Context, principal auth.User,
) (auth.User, error) {
	return principal, nil
}

type currentAvatarIntegrationPeerVerifier struct {
	trustedRemote string
}

func (verifier currentAvatarIntegrationPeerVerifier) VerifyForwardedIdentityPeer(
	request *http.Request,
) error {
	if request.RemoteAddr != verifier.trustedRemote {
		return errors.New("untrusted peer")
	}
	return nil
}

func currentAvatarIntegrationRequest(
	method string,
	target string,
	userID string,
	remote string,
	body io.Reader,
) *http.Request {
	request := httptest.NewRequest(method, target, body)
	request.RemoteAddr = remote
	if userID != "" {
		request.Header.Set("X-Auth-Type", "user")
		request.Header.Set("X-Auth-ID", userID)
	}
	return request
}

func currentAvatarMultipartBody(t *testing.T, filename string, content []byte) (io.Reader, string) {
	t.Helper()
	buffer := &bytes.Buffer{}
	writer := multipart.NewWriter(buffer)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer, writer.FormDataContentType()
}

// fakeAvatarObjectStore implements storage.ObjectStore in memory — matching
// eliteacore's own per-file fake convention (no shared testutil exists).
type fakeAvatarObjectStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newFakeAvatarObjectStore() *fakeAvatarObjectStore {
	return &fakeAvatarObjectStore{objects: map[string][]byte{}}
}

func avatarStoreKey(ref storage.ObjectRef) string {
	return ref.ProjectID() + "/" + ref.Bucket() + "/" + ref.Key()
}

func (f *fakeAvatarObjectStore) Put(_ context.Context, ref storage.ObjectRef, body io.Reader, _ storage.PutOptions) (storage.ObjectInfo, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	f.mu.Lock()
	f.objects[avatarStoreKey(ref)] = data
	f.mu.Unlock()
	return storage.ObjectInfo{Key: ref.Key(), Size: int64(len(data))}, nil
}

func (f *fakeAvatarObjectStore) Get(_ context.Context, ref storage.ObjectRef, _ *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	f.mu.Lock()
	data, ok := f.objects[avatarStoreKey(ref)]
	f.mu.Unlock()
	if !ok {
		return nil, storage.ObjectInfo{}, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), storage.ObjectInfo{Key: ref.Key(), Size: int64(len(data))}, nil
}

func (f *fakeAvatarObjectStore) Delete(context.Context, storage.ObjectRef) error { return nil }
func (f *fakeAvatarObjectStore) DeleteBatch(context.Context, []storage.ObjectRef) (storage.BatchResult, error) {
	return storage.BatchResult{}, storage.ErrNotSupported
}
func (f *fakeAvatarObjectStore) Stat(context.Context, storage.ObjectRef) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}
func (f *fakeAvatarObjectStore) List(context.Context, storage.ListQuery) (storage.ListPage, error) {
	return storage.ListPage{}, storage.ErrNotSupported
}
func (f *fakeAvatarObjectStore) PresignGet(context.Context, storage.ObjectRef, time.Duration) (string, error) {
	return "", storage.ErrNotSupported
}
func (f *fakeAvatarObjectStore) PresignPut(context.Context, storage.ObjectRef, time.Duration, storage.PutOptions) (string, error) {
	return "", storage.ErrNotSupported
}
func (f *fakeAvatarObjectStore) StartMultipart(context.Context, storage.ObjectRef, storage.PutOptions) (storage.UploadID, error) {
	return "", storage.ErrNotSupported
}
func (f *fakeAvatarObjectStore) PresignPart(context.Context, storage.ObjectRef, storage.UploadID, int32, time.Duration) (string, error) {
	return "", storage.ErrNotSupported
}
func (f *fakeAvatarObjectStore) CompleteMultipart(context.Context, storage.ObjectRef, storage.UploadID, []storage.Part) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}
func (f *fakeAvatarObjectStore) AbortMultipart(context.Context, storage.ObjectRef, storage.UploadID) error {
	return storage.ErrNotSupported
}
func (f *fakeAvatarObjectStore) Capabilities() storage.Capabilities { return storage.Capabilities{} }

func newCurrentAvatarPostgresPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_social_avatar_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("ping isolated PostgreSQL integration database: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		dropContext, dropCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(
			dropContext,
			"DROP DATABASE "+quotedDatabase+" WITH (FORCE)",
		); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})
	return pool
}

// readStoredAvatar reads one user's persisted avatar URL straight from
// centry.social_users.
//
// The assertion has to read the ROW, not the response body. A handler that
// answered the caller's own URL while writing somebody else's row would pass an
// assertion made against the response alone. An absent row reads as the empty
// string, which is the "no avatar yet" state.
func readStoredAvatar(t *testing.T, pool *pgxpool.Pool, userID int) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var avatar *string
	err := pool.QueryRow(ctx,
		`SELECT avatar FROM centry.social_users WHERE user_id = $1`, userID).Scan(&avatar)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("read the stored avatar for user %d: %v", userID, err)
	}
	if avatar == nil {
		return ""
	}
	return *avatar
}

func prepareCurrentAvatarDatabase(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
CREATE SCHEMA centry;
CREATE TABLE centry.project (
    id INTEGER PRIMARY KEY,
    suspended BOOLEAN NOT NULL DEFAULT FALSE
);
INSERT INTO centry.project (id, suspended) VALUES (7, FALSE);

CREATE TABLE public.auth_core__user (
    id INTEGER PRIMARY KEY,
    email TEXT UNIQUE,
    name TEXT,
    last_login TIMESTAMP WITHOUT TIME ZONE,
    suspended BOOLEAN NOT NULL DEFAULT FALSE
);
INSERT INTO public.auth_core__user (id, email, name, last_login, suspended) VALUES
    (11, 'uploader@elitea.example', 'Uploader', NULL, FALSE),
    (12, 'viewer@elitea.example', 'Viewer', NULL, FALSE),
    (13, 'no-permission@elitea.example', 'No Permission', NULL, FALSE),
    (14, 'second-uploader@elitea.example', 'Second Uploader', NULL, FALSE);

CREATE TABLE public.auth_core__role (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    mode TEXT NOT NULL
);
CREATE TABLE public.auth_core__role_permission (
    role_id INTEGER NOT NULL,
    permission TEXT NOT NULL
);
CREATE TABLE public.auth_core__project_role (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL,
    name TEXT NOT NULL
);
CREATE TABLE public.auth_core__project_role_permission (
    project_id INTEGER NOT NULL,
    role_id INTEGER NOT NULL,
    permission TEXT NOT NULL
);
CREATE TABLE public.auth_core__project_user_role (
    id SERIAL PRIMARY KEY,
    project_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    role_id INTEGER NOT NULL,
    UNIQUE (project_id, user_id, role_id)
);
CREATE INDEX ix_auth_core__project_user_role_project_id
    ON public.auth_core__project_user_role (project_id);

INSERT INTO public.auth_core__project_role (id, project_id, name) VALUES
    (101, 7, 'avatar-uploader'),
    (102, 7, 'avatar-viewer'),
    (103, 7, 'no-avatar-permission');
INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission) VALUES
    (7, 101, 'models.social.avatar.get'),
    (7, 101, 'models.social.avatar.update'),
    (7, 102, 'models.social.avatar.get'),
    (7, 103, 'models.social.authors.get');
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id) VALUES
    (7, 11, 101),
    (7, 12, 102),
    (7, 13, 103),
    (7, 14, 101);

CREATE TABLE centry.social_users (
    id SERIAL PRIMARY KEY,
    user_id INTEGER NOT NULL UNIQUE,
    avatar VARCHAR
);`); err != nil {
		t.Fatalf("prepare current Social avatar database: %v", err)
	}
}
