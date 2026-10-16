package toolkits_test

// A file-store toolkit (`artifact`, `pptx`, `docx`, `xlsx`) saved without a
// bucket failed every turn of every agent it was attached to (live 4.45.0).
// Create and Update now refuse it with the form's field error, through the
// real handlers and the real pinned catalogue.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
)

func TestAFileStoreToolkitIsNotSavedWithoutABucket(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		toolkitType, field, settings string
	}{
		{"pptx", "bucket_name", `{}`},
		{"docx", "bucket_name", `{"bucket_name": "   "}`},
		{"xlsx", "bucket_name", `{"bucket_name": null}`},
		{"artifact", "bucket", `{"bucket": ""}`},
	} {
		repo := &defaultsRecordingRepo{storedType: tc.toolkitType}
		router := newDefaultsRouter(t, repo)

		created := postToolkit(t, router,
			`{"name": "kit", "type": "`+tc.toolkitType+`", "settings": `+tc.settings+`}`)
		assertMissingBucket(t, created, tc.toolkitType+" create", tc.field)

		// An update restating no type is judged by the stored row's type.
		request := httptest.NewRequest(http.MethodPut, "/tool/prompt_lib/1/11",
			bytes.NewBufferString(`{"settings": `+tc.settings+`}`))
		request.Header.Set("Content-Type", "application/json")
		updated := httptest.NewRecorder()
		router.ServeHTTP(updated, request)
		assertMissingBucket(t, updated, tc.toolkitType+" update", tc.field)

		if len(repo.creates)+len(repo.updates) != 0 {
			t.Fatalf("%s: a refused save was written", tc.toolkitType)
		}
	}
}

// Review S2: a create that sends no settings at all is a missing bucket too.
func TestAFileStoreToolkitCreatedWithoutSettingsIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ toolkitType, field string }{
		{"pptx", "bucket_name"}, {"docx", "bucket_name"}, {"xlsx", "bucket_name"}, {"artifact", "bucket"},
	} {
		repo := &defaultsRecordingRepo{storedType: tc.toolkitType}
		router := newDefaultsRouter(t, repo)
		for _, payload := range []string{
			`{"name": "kit", "type": "` + tc.toolkitType + `"}`,
			`{"name": "kit", "type": "` + tc.toolkitType + `", "settings": null}`,
		} {
			assertMissingBucket(t, postToolkit(t, router, payload), tc.toolkitType+" create "+payload, tc.field)
		}
		if len(repo.creates) != 0 {
			t.Fatalf("%s: a refused create was written", tc.toolkitType)
		}
	}
}

func TestAFileStoreToolkitWithABucketSaves(t *testing.T) {
	t.Parallel()

	repo := &defaultsRecordingRepo{storedType: "docx"}
	router := newDefaultsRouter(t, repo)
	if recorder := postToolkit(t, router,
		`{"name": "kit", "type": "docx", "settings": {"bucket_name": "reports"}}`); recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", recorder.Code, recorder.Body.String())
	}
	// A rename carries no settings and is never judged by them.
	request := httptest.NewRequest(http.MethodPut, "/tool/prompt_lib/1/11",
		bytes.NewBufferString(`{"name": "renamed"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("rename status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	// Another type is not judged by the bucket rule.
	if recorder := postToolkit(t, router,
		`{"name": "q", "type": "qtest", "settings": {"qtest_project_id": 1}}`); recorder.Code != http.StatusCreated {
		t.Fatalf("qtest status = %d, want 201; body=%s", recorder.Code, recorder.Body.String())
	}
}

func assertMissingBucket(t *testing.T, recorder *httptest.ResponseRecorder, label, field string) {
	t.Helper()
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("%s: status = %d, want 400; body=%s", label, recorder.Code, recorder.Body.String())
	}
	var body struct {
		SettingsErrors []struct {
			Loc  []string `json:"loc"`
			Code string   `json:"code"`
		} `json:"settings_errors"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: body %s: %v", label, recorder.Body.String(), err)
	}
	if len(body.SettingsErrors) != 1 || body.SettingsErrors[0].Code != "missing_bucket" ||
		len(body.SettingsErrors[0].Loc) != 2 || body.SettingsErrors[0].Loc[1] != field {
		t.Fatalf("%s: settings_errors = %+v", label, body.SettingsErrors)
	}
}

// Review round 2: an update that turns a toolkit into a file-store type
// without sending settings is judged against the STORED settings.
func TestATypeOnlyUpdateToAFileStoreTypeIsJudgedOnTheStoredSettings(t *testing.T) {
	t.Parallel()

	put := func(repo *defaultsRecordingRepo, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPut, "/tool/prompt_lib/1/11", bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		newDefaultsRouter(t, repo).ServeHTTP(recorder, request)
		return recorder
	}
	bucketless := &defaultsRecordingRepo{storedType: "github", storedSettings: map[string]any{"repository": "x"}}
	assertMissingBucket(t, put(bucketless, `{"type": "xlsx"}`), "type-only update", "bucket_name")
	if len(bucketless.updates) != 0 {
		t.Fatal("a refused update was written")
	}
	withBucket := &defaultsRecordingRepo{storedType: "docx", storedSettings: map[string]any{"bucket_name": "reports"}}
	if recorder := put(withBucket, `{"type": "xlsx"}`); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}

	// Review round 3: the check fails CLOSED. An unreadable stored row is
	// an error, never an unjudged save.
	unreadable := &defaultsRecordingRepo{storedType: "docx", getErr: errors.New("connection reset")}
	if recorder := put(unreadable, `{"type": "xlsx"}`); recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", recorder.Code, recorder.Body.String())
	}
	if len(unreadable.updates) != 0 {
		t.Fatal("an update whose stored row could not be read was written")
	}
	missing := &defaultsRecordingRepo{storedType: "docx", getErr: fmt.Errorf("get toolkit: %w", pgx.ErrNoRows)}
	if recorder := put(missing, `{"type": "xlsx"}`); recorder.Code != http.StatusNotFound || len(missing.updates) != 0 {
		t.Fatalf("status = %d (updates %d), want 404 and no write", recorder.Code, len(missing.updates))
	}
}

// The served artifact schema states what the save enforces: `bucket` is
// required (the SDK's schema requires it too), so the create form asks for it.
func TestTheServedArtifactSchemaRequiresItsBucket(t *testing.T) {
	t.Parallel()
	body := getToolkitTypeCatalogue(t, toolkits.WithArgumentSchemas(pinnedSnapshot(t)))
	schema, _ := body["artifact"].(map[string]any)
	required, _ := schema["required"].([]any)
	for _, key := range required {
		if key == "bucket" {
			return
		}
	}
	t.Fatalf("artifact required = %v, want it to name bucket", schema["required"])
}
