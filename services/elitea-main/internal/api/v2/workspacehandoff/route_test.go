package workspacehandoff

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	handoffapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

const (
	conversation = "6f1c2d3e-4a5b-4c6d-8e7f-901234567890"
	sha1Value    = "0123456789abcdef0123456789abcdef01234567"
)

type fakeHandoff struct {
	err      error
	handoff  handoffapp.Handoff
	view     handoffapp.View
	began    handoffapp.BeginRequest
	erased   handoffapp.EraseRequest
	released handoffapp.ReleaseRequest
	policy   handoffapp.Policy
}

func (f *fakeHandoff) Begin(_ context.Context, request handoffapp.BeginRequest) (handoffapp.BeginOutcome, error) {
	f.began = request
	expires := time.Unix(1000, 0)
	return handoffapp.BeginOutcome{
		Handoff:     handoffapp.Handoff{HandoffID: "7d5a8c0e-3f4b-4a2b-9c1d-0e2f3a4b5c6d", Epoch: 1, State: workspacestore.StateHandingOff, ExpiresAt: &expires},
		AllowedRefs: []string{workspacestore.RefBase, workspacestore.RefHandoff1},
		ExpectedOld: map[string]*string{workspacestore.RefBase: nil, workspacestore.RefHandoff1: nil},
		Limits:      workspacestore.Limits{MaxSegmentBytes: 209715200, MaxFileBytes: 26214400, MaxFiles: 20000, MaxUnpackedBytes: 2147483648},
		BytesLeft:   53687091200,
	}, f.err
}
func (f *fakeHandoff) Complete(context.Context, handoffapp.CompleteRequest) (handoffapp.Handoff, error) {
	return f.handoff, f.err
}
func (f *fakeHandoff) Reclaim(context.Context, handoffapp.ReclaimRequest) (handoffapp.Handoff, error) {
	return f.handoff, f.err
}
func (f *fakeHandoff) RequestReclaim(context.Context, handoffapp.Thread, int64) error { return f.err }
func (f *fakeHandoff) Release(_ context.Context, request handoffapp.ReleaseRequest) (handoffapp.Handoff, error) {
	f.released = request
	return f.handoff, f.err
}
func (f *fakeHandoff) State(context.Context, handoffapp.Thread) (handoffapp.View, error) {
	return f.view, f.err
}
func (f *fakeHandoff) EffectivePolicy(context.Context, int64) (handoffapp.Policy, error) {
	return f.policy, f.err
}
func (f *fakeHandoff) Erase(_ context.Context, request handoffapp.EraseRequest) (handoffapp.Handoff, error) {
	f.erased = request
	return f.handoff, f.err
}

type fakeStore struct {
	err       error
	grant     workspacestore.UploadGrant
	published workspacestore.PublishRequest
}

func (f *fakeStore) RequestUpload(context.Context, workspacestore.UploadRequest) (workspacestore.UploadGrant, error) {
	return f.grant, f.err
}
func (f *fakeStore) CompleteUpload(context.Context, int64, workspacestore.Caller, string, string, []workspacestore.CompletedPart) error {
	return f.err
}
func (f *fakeStore) Publish(_ context.Context, request workspacestore.PublishRequest) (workspacestore.PublishResult, error) {
	f.published = request
	return workspacestore.PublishResult{Seq: 1, Refs: map[string]string{workspacestore.RefBase: sha1Value}}, f.err
}
func (f *fakeStore) Plan(context.Context, workspacestore.Thread, workspacestore.Caller, int64) (workspacestore.FetchPlan, error) {
	return workspacestore.FetchPlan{Refs: map[string]string{}, Segments: []workspacestore.FetchSegment{}}, f.err
}
func (f *fakeStore) PutObject(context.Context, workspacestore.Thread, workspacestore.Caller, string, io.Reader) error {
	return f.err
}
func (f *fakeStore) OpenObject(context.Context, workspacestore.Thread, workspacestore.Caller, string) (io.ReadCloser, int64, error) {
	return io.NopCloser(strings.NewReader("bundle")), 6, f.err
}
func (f *fakeStore) ThreadBytes(context.Context, workspacestore.Thread) (int64, error) {
	return 42, nil
}
func (f *fakeStore) ProjectBytes(context.Context, int64) (int64, error) { return 1000, nil }

type fakeConversations struct{ err error }

func (f fakeConversations) AuthorizeChatResource(context.Context, string, string, string) error {
	return f.err
}

type grantResolver []string

func (g grantResolver) ResolvePermissions(_ context.Context, user auth.User, _, _ string) (auth.PermissionResolution, error) {
	id, ok := user.OwningUserID()
	if !ok {
		id = 7
	}
	return auth.PermissionResolution{UserID: id, Permissions: g}, nil
}

func nativeUser() *auth.User {
	return &auth.User{ID: "7", UserID: "7", TokenID: "70", AuthType: "token", NativeClientID: "ai.elitea.desktop"}
}

// serve runs one handler as the router would: the principal on the context,
// the project permission gate, and the chi route params.
func serve(t *testing.T, h http.HandlerFunc, user *auth.User, granted []string, method, pattern, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	gated := apimw.RequireResolvedPermissionsForProject(grantResolver(granted), Mode,
		func(r *http.Request) (string, bool) { return chi.URLParam(r, "projectID"), true },
		SendPermission, ReadPermission, ManagePermission)(h)
	router := chi.NewRouter()
	router.Method(method, pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user != nil {
			r = r.WithContext(auth.ContextWithUser(r.Context(), *user))
		}
		gated.ServeHTTP(w, r)
	}))
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func decode(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", recorder.Body.String(), err)
	}
	return body
}

const handoffURL = "/api/v2/elitea_core/workspace_handoff/prompt_lib/1/" + conversation

func TestBeginNeedsTheNativeTokenAndAnswersTheContractShape(t *testing.T) {
	handoff := &fakeHandoff{}
	h := &handler{handoff: handoff, store: &fakeStore{}, conversations: fakeConversations{}}
	body := `{"device_id":"d1","history_mode":"none","stats":{"files":42,"bytes":1234567,"unpacked_bytes":5555555,"excluded":["./.env"],"scan_findings":0},"has_upstream":true,"note":"n"}`
	for name, user := range map[string]*auth.User{
		"a browser session": {ID: "7", UserID: "7", AuthType: "session"},
		"a PAT":             {ID: "7", UserID: "7", TokenID: "71", AuthType: "token"},
	} {
		response := serve(t, h.begin, user, []string{SendPermission}, http.MethodPost, HandoffPath, handoffURL, body)
		if response.Code != http.StatusForbidden || decode(t, response)["error"] != "handoff_requires_token" {
			t.Fatalf("%s: %d %s", name, response.Code, response.Body)
		}
	}
	response := serve(t, h.begin, nativeUser(), []string{SendPermission}, http.MethodPost, HandoffPath, handoffURL, body)
	if response.Code != http.StatusCreated {
		t.Fatalf("begin = %d %s", response.Code, response.Body)
	}
	answer := decode(t, response)
	limits, _ := answer["limits"].(map[string]any)
	expected, _ := answer["expected_old"].(map[string]any)
	if answer["epoch"] != float64(1) || answer["state"] != "HANDING_OFF" || answer["expires_at"] == "" ||
		limits["project_workspace_bytes_left"] != float64(53687091200) || len(expected) != 2 ||
		expected[workspacestore.RefBase] != nil {
		t.Fatalf("begin answer = %v", answer)
	}
	if handoff.began.ActorUserID != 7 || handoff.began.TokenID != "70" || handoff.began.Stats.Files != 42 ||
		handoff.began.Stats.Excluded[0] != "./.env" || !handoff.began.HasUpstream || handoff.began.DeviceID != "d1" {
		t.Fatalf("begin request = %+v", handoff.began)
	}
}

func TestPublishConflictCarriesTheCurrentRefsInDetails(t *testing.T) {
	stored := sha1Value
	store := &fakeStore{err: &workspacestore.RefConflictError{Current: map[string]*string{
		workspacestore.RefBase: &stored, workspacestore.RefHandoff1: nil,
	}}}
	h := &handler{handoff: &fakeHandoff{}, store: store, conversations: fakeConversations{}}
	response := serve(t, h.publish, nativeUser(), []string{SendPermission}, http.MethodPost, PublishPath,
		"/api/v2/elitea_core/workspace_store_publish/prompt_lib/1/"+conversation,
		`{"epoch":1,"kind":"COMPACT","segment_sha256":"`+strings.Repeat("a", 64)+`","bytes":3,"prerequisites":[],
		  "refs":[{"name":"refs/elitea/base","old":null,"new":"`+sha1Value+`"}],"object_format":"sha1"}`)
	answer := decode(t, response)
	details, _ := answer["details"].(map[string]any)
	current, _ := details["current"].(map[string]any)
	if response.Code != http.StatusConflict || answer["error"] != "ref_conflict" ||
		current[workspacestore.RefBase] != sha1Value || current[workspacestore.RefHandoff1] != nil {
		t.Fatalf("conflict = %d %v", response.Code, answer)
	}
	if _, present := current[workspacestore.RefHandoff1]; !present {
		t.Fatalf("an absent ref must be named with null: %v", current)
	}
	if store.published.Refs[0].Old != nil || store.published.Caller.Writer != workspacestore.WriterDesktop {
		t.Fatalf("publish request = %+v", store.published)
	}
}

func TestErrorsMapToTheContractCodes(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{handoffapp.ErrDisabled, http.StatusForbidden, "cloud_handoff_disabled"},
		{handoffapp.ErrExecutorIsCloud, http.StatusConflict, "thread_executor_is_cloud"},
		{handoffapp.ErrInProgress, http.StatusConflict, "handoff_in_progress"},
		{handoffapp.ErrLocalTurnInProgress, http.StatusConflict, "local_turn_in_progress"},
		{handoffapp.ErrQuotaExceeded, http.StatusRequestEntityTooLarge, "workspace_quota_exceeded"},
		{handoffapp.ErrInvalid, http.StatusUnprocessableEntity, "invalid_handoff"},
		{handoffapp.ErrOverLimits, http.StatusUnprocessableEntity, "invalid_handoff"},
		{handoffapp.ErrNotLocalWork, http.StatusUnprocessableEntity, "invalid_handoff"},
		{handoffapp.ErrHistoryModeNotAllowed, http.StatusUnprocessableEntity, "history_mode_not_allowed"},
		{handoffapp.ErrStaleEpoch, http.StatusConflict, "stale_epoch"},
		{handoffapp.ErrNotFound, http.StatusNotFound, "handoff_not_found"},
		{handoffapp.ErrNotInCloud, http.StatusConflict, "handoff_not_in_cloud"},
		{handoffapp.ErrErased, http.StatusConflict, "handoff_erased"},
		{handoffapp.ErrUnavailable, http.StatusServiceUnavailable, "handoff_unavailable"},
		{workspacestore.ErrStaleEpoch, http.StatusConflict, "stale_epoch"},
		{workspacestore.ErrSegmentTooLarge, http.StatusRequestEntityTooLarge, "segment_too_large"},
		{workspacestore.ErrQuotaExceeded, http.StatusRequestEntityTooLarge, "workspace_quota_exceeded"},
		{workspacestore.ErrRefNotAllowed, http.StatusForbidden, "ref_not_allowed"},
		{workspacestore.ErrSegmentMissing, http.StatusUnprocessableEntity, "segment_missing"},
		{workspacestore.ErrChecksumMismatch, http.StatusUnprocessableEntity, "checksum_mismatch"},
		{workspacestore.ErrUnsupportedObjectFormat, http.StatusUnprocessableEntity, "unsupported_object_format"},
		{errors.New("boom"), http.StatusInternalServerError, "handoff_failed"},
	} {
		recorder := httptest.NewRecorder()
		writeError(context.Background(), recorder, "test", tc.err)
		if recorder.Code != tc.status || decode(t, recorder)["error"] != tc.code {
			t.Fatalf("%v: %d %s, want %d %s", tc.err, recorder.Code, recorder.Body, tc.status, tc.code)
		}
		if strings.Contains(recorder.Body.String(), "boom") {
			t.Fatalf("a raw error reached the caller: %s", recorder.Body)
		}
	}
}

func TestStateAnswersLocalForAThreadNeverSentAndHidesAnUnreadableConversation(t *testing.T) {
	h := &handler{handoff: &fakeHandoff{}, store: &fakeStore{}, conversations: fakeConversations{}}
	session := &auth.User{ID: "7", UserID: "7", AuthType: "session"}
	response := serve(t, h.state, session, []string{ReadPermission}, http.MethodGet, HandoffPath, handoffURL, "")
	if answer := decode(t, response); response.Code != http.StatusOK || answer["state"] != "LOCAL" || answer["epoch"] != float64(0) || len(answer) != 2 {
		t.Fatalf("state = %d %v", response.Code, answer)
	}
	hidden := &handler{handoff: &fakeHandoff{}, store: &fakeStore{}, conversations: fakeConversations{err: errors.New("private")}}
	if response := serve(t, hidden.state, session, []string{ReadPermission}, http.MethodGet, HandoffPath, handoffURL, ""); response.Code != http.StatusNotFound {
		t.Fatalf("an unreadable conversation = %d", response.Code)
	}
	present := &handler{handoff: &fakeHandoff{view: handoffapp.View{Present: true, Handoff: handoffapp.Handoff{
		State: workspacestore.StateCloudIdle, Epoch: 2, Stats: json.RawMessage(`{"files":1}`), UpdatedAt: time.Unix(0, 0),
	}}}, store: &fakeStore{}, conversations: fakeConversations{}}
	response = serve(t, present.state, session, []string{ReadPermission}, http.MethodGet, HandoffPath, handoffURL, "")
	if answer := decode(t, response); answer["state"] != "CLOUD_IDLE" || answer["workspace_bytes"] != float64(42) || answer["cloud_sha"] != nil {
		t.Fatalf("state = %v", answer)
	}
}

// stats.excluded lists paths on the owner's computer: only the owner reads
// them. Anyone else who may read the conversation gets the count.
func TestStateShowsExcludedPathsToTheOwnerOnly(t *testing.T) {
	h := &handler{handoff: &fakeHandoff{view: handoffapp.View{Present: true, Handoff: handoffapp.Handoff{
		State: workspacestore.StateCloudIdle, Epoch: 2, OwnerUserID: 7, UpdatedAt: time.Unix(0, 0),
		Stats: json.RawMessage(`{"files":3,"excluded":["./.env","./secrets/key.pem"],"scan_findings":0}`),
	}}}, store: &fakeStore{}, conversations: fakeConversations{}}
	statsOf := func(user *auth.User) map[string]any {
		t.Helper()
		response := serve(t, h.state, user, []string{ReadPermission}, http.MethodGet, HandoffPath, handoffURL, "")
		answer := decode(t, response)
		stats, _ := answer["stats"].(map[string]any)
		if response.Code != http.StatusOK || stats == nil {
			t.Fatalf("state = %d %v", response.Code, answer)
		}
		return stats
	}
	owner := statsOf(&auth.User{ID: "7", UserID: "7", AuthType: "session"})
	if paths, _ := owner["excluded"].([]any); len(paths) != 2 || paths[0] != "./.env" || owner["excluded_count"] != float64(2) {
		t.Fatalf("owner stats = %v, want the paths", owner)
	}
	other := statsOf(&auth.User{ID: "8", UserID: "8", AuthType: "session"})
	if _, present := other["excluded"]; present || other["excluded_count"] != float64(2) || other["files"] != float64(3) {
		t.Fatalf("another reader's stats = %v, want the count only", other)
	}
	raw, _ := json.Marshal(other)
	if strings.Contains(string(raw), ".env") || strings.Contains(string(raw), "secrets") {
		t.Fatalf("a path reached another reader: %s", raw)
	}
}

func TestEraseMarksAProjectAdministratorAsManager(t *testing.T) {
	for _, tc := range []struct {
		granted []string
		manager bool
	}{
		{[]string{SendPermission}, false},
		{[]string{ManagePermission}, true},
	} {
		handoff := &fakeHandoff{handoff: handoffapp.Handoff{State: workspacestore.StateLocal, Epoch: 3}}
		h := &handler{handoff: handoff, store: &fakeStore{}, conversations: fakeConversations{}}
		response := serve(t, h.erase, &auth.User{ID: "7", UserID: "7", AuthType: "session"}, tc.granted,
			http.MethodDelete, HandoffPath, handoffURL, "")
		if response.Code != http.StatusOK || handoff.erased.Manager != tc.manager || decode(t, response)["epoch"] != float64(3) {
			t.Fatalf("granted %v: %d %s manager=%t", tc.granted, response.Code, response.Body, handoff.erased.Manager)
		}
	}
}

func TestReclaimDuringATurnAnswers202(t *testing.T) {
	h := &handler{handoff: &fakeHandoff{handoff: handoffapp.Handoff{
		State: workspacestore.StateCloudRunning, Epoch: 2, ReclaimPending: true, RunningExecutionID: "exec-1",
	}}, store: &fakeStore{}, conversations: fakeConversations{}}
	response := serve(t, h.reclaim, nativeUser(), []string{SendPermission}, http.MethodPost, ReclaimPath,
		"/api/v2/elitea_core/workspace_handoff_reclaim/prompt_lib/1/"+conversation, `{"epoch":2,"device_id":"d1"}`)
	answer := decode(t, response)
	if response.Code != http.StatusAccepted || answer["reclaim_pending"] != true || answer["waiting_for_execution_id"] != "exec-1" {
		t.Fatalf("reclaim = %d %v", response.Code, answer)
	}
}

func TestUploadGrantShape(t *testing.T) {
	h := &handler{handoff: &fakeHandoff{}, store: &fakeStore{grant: workspacestore.UploadGrant{
		PutURL: "https://store.test/put", Headers: map[string]string{"Content-Type": workspacestore.MediaType},
		ExpiresAt: time.Unix(0, 0),
	}}, conversations: fakeConversations{}}
	response := serve(t, h.upload, nativeUser(), []string{SendPermission}, http.MethodPost, UploadPath,
		"/api/v2/elitea_core/workspace_store_upload/prompt_lib/1/"+conversation, `{"epoch":1,"sha256":"`+strings.Repeat("a", 64)+`","bytes":3}`)
	answer := decode(t, response)
	put, _ := answer["put"].(map[string]any)
	if response.Code != http.StatusOK || answer["exists"] != false || put["url"] != "https://store.test/put" || answer["multipart"] != nil {
		t.Fatalf("upload = %d %v", response.Code, answer)
	}
}

func TestFetchRefusesANegativeSinceSeq(t *testing.T) {
	h := &handler{handoff: &fakeHandoff{}, store: &fakeStore{}, conversations: fakeConversations{}}
	response := serve(t, h.fetch, nativeUser(), []string{SendPermission}, http.MethodGet, FetchPath,
		"/api/v2/elitea_core/workspace_store_fetch/prompt_lib/1/"+conversation+"?since_seq=-1", "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("fetch = %d", response.Code)
	}
}

func TestBeginPassesIncludesAndEnvAndMapsTheirRefusals(t *testing.T) {
	handoff := &fakeHandoff{}
	h := &handler{handoff: handoff, store: &fakeStore{}, conversations: fakeConversations{}}
	body := `{"device_id":"d1","stats":{"files":1},"included_excluded":["./.env"],"env":{"DATABASE_URL":"x"}}`
	response := serve(t, h.begin, nativeUser(), []string{SendPermission}, http.MethodPost, HandoffPath, handoffURL, body)
	if response.Code != http.StatusCreated || handoff.began.IncludedExcluded[0] != "./.env" || handoff.began.Env["DATABASE_URL"] != "x" {
		t.Fatalf("begin = %d, request %+v", response.Code, handoff.began)
	}
	// An absent env stays nil (keep); `{}` is an empty map (clear).
	serve(t, h.begin, nativeUser(), []string{SendPermission}, http.MethodPost, HandoffPath, handoffURL, `{"device_id":"d1","stats":{}}`)
	if handoff.began.Env != nil {
		t.Fatalf("an absent env decoded as %v", handoff.began.Env)
	}
	serve(t, h.begin, nativeUser(), []string{SendPermission}, http.MethodPost, HandoffPath, handoffURL, `{"device_id":"d1","stats":{},"env":{}}`)
	if handoff.began.Env == nil || len(handoff.began.Env) != 0 {
		t.Fatalf("env {} decoded as %v", handoff.began.Env)
	}
	for _, tc := range []struct {
		err    error
		status int
		code   string
		detail string
	}{
		{&handoffapp.EnvNameDeniedError{Names: []string{"SSH_AUTH_SOCK"}}, http.StatusUnprocessableEntity, "env_name_denied", "names"},
		{&handoffapp.PathDeniedError{Paths: []string{"./.env.production"}}, http.StatusUnprocessableEntity, "path_denied", "paths"},
		{handoffapp.ErrIncludeNotAllowed, http.StatusUnprocessableEntity, "include_not_allowed", ""},
		{handoffapp.ErrEnvHarvestDisabled, http.StatusUnprocessableEntity, "env_harvest_disabled", ""},
		{handoffapp.ErrEnvTooLarge, http.StatusRequestEntityTooLarge, "env_too_large", ""},
	} {
		recorder := httptest.NewRecorder()
		writeError(context.Background(), recorder, "begin", tc.err)
		answer := decode(t, recorder)
		if recorder.Code != tc.status || answer["error"] != tc.code {
			t.Fatalf("%v: %d %v", tc.err, recorder.Code, answer)
		}
		if tc.detail != "" {
			details, _ := answer["details"].(map[string]any)
			if list, _ := details[tc.detail].([]any); len(list) != 1 {
				t.Fatalf("%v: details = %v", tc.err, answer["details"])
			}
		}
	}
}

func TestPolicyAnswersTheEffectivePolicy(t *testing.T) {
	allow := []string{"DATABASE_URL"}
	h := &handler{handoff: &fakeHandoff{policy: handoffapp.Policy{
		Enabled: true, Shell: true, Network: true, HistoryFullAllowed: true,
		Exclusions: []string{".env", "secrets/**"}, ExclusionsPlatform: []string{".env"},
		ExclusionsProject: []string{"secrets/**"}, PathDeny: []string{"**/.env.production"},
		AllowUserInclude: true, EnvHarvest: true, EnvDeny: []string{"PATH"}, EnvAllow: &allow,
		Limits: workspacestore.Limits{MaxSegmentBytes: 10, MaxProjectWorkspaceBytes: 5000},
	}}, store: &fakeStore{}, conversations: fakeConversations{}}
	response := serve(t, h.policy, &auth.User{ID: "7", UserID: "7", AuthType: "session"}, []string{SendPermission},
		http.MethodGet, PolicyPath, "/api/v2/elitea_core/workspace_handoff_policy/prompt_lib/1", "")
	answer := decode(t, response)
	limits, _ := answer["limits"].(map[string]any)
	envLimits, _ := answer["env_limits"].(map[string]any)
	modes, _ := answer["history_modes"].([]any)
	if response.Code != http.StatusOK || answer["enabled"] != true || len(modes) != 2 ||
		len(answer["exclusions"].([]any)) != 2 || answer["path_deny"].([]any)[0] != "**/.env.production" ||
		answer["env_allow"].([]any)[0] != "DATABASE_URL" || limits["project_workspace_bytes_left"] != float64(4000) ||
		envLimits["max_vars"] != float64(200) || envLimits["max_bytes"] != float64(65536) {
		t.Fatalf("policy = %d %v", response.Code, answer)
	}
	if platform, project := answer["exclusions_platform"].([]any), answer["exclusions_project"].([]any); len(platform) != 1 ||
		platform[0] != ".env" || len(project) != 1 || project[0] != "secrets/**" {
		t.Fatalf("exclusions split = %v / %v", answer["exclusions_platform"], answer["exclusions_project"])
	}
	unset := &handler{handoff: &fakeHandoff{}, store: &fakeStore{}, conversations: fakeConversations{}}
	response = serve(t, unset.policy, nativeUser(), []string{SendPermission}, http.MethodGet, PolicyPath,
		"/api/v2/elitea_core/workspace_handoff_policy/prompt_lib/1", "")
	if answer := decode(t, response); answer["env_allow"] != nil || answer["enabled"] != false ||
		len(answer["exclusions_platform"].([]any)) != 0 || len(answer["exclusions_project"].([]any)) != 0 {
		t.Fatalf("an empty policy = %v", answer)
	}
}
