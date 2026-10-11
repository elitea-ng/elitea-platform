package indexing_test

// Contract tests for the index routes under ELITEA_INDEXING_RUNTIME=rust
// (ADR-0031 V1). The same URLs and the same response shapes the web client
// reads (apps/elitea-web/src/features/toolkits/indexes/api/indexesApi.ts) must
// come back when the routes run on the index registry. The routes below are the
// real ones; only their application service is the registry Service over an
// in-memory store, so what is asserted is the bytes a browser would receive.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/indexing"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	indexregistryapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexregistry"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type registryMemoryStore struct {
	mu   sync.Mutex
	rows []indexregistryapp.Row
	dead []indexregistryapp.Row
	seq  int
}

func (s *registryMemoryStore) List(_ context.Context, projectID, toolkitID int32) ([]indexregistryapp.Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []indexregistryapp.Row{}
	for _, row := range s.rows {
		if row.ProjectID == projectID && row.ToolkitID == toolkitID {
			out = append(out, row)
		}
	}
	return out, nil
}

func (s *registryMemoryStore) FindByName(_ context.Context, projectID, toolkitID int32, name string) (indexregistryapp.Row, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range s.rows {
		if row.ProjectID == projectID && row.ToolkitID == toolkitID && row.Name == name {
			return row, true, nil
		}
	}
	return indexregistryapp.Row{}, false, nil
}

func (s *registryMemoryStore) SaveConfiguration(_ context.Context, projectID, toolkitID int32, name string, configuration []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, row := range s.rows {
		if row.ProjectID == projectID && row.ToolkitID == toolkitID && row.Name == name {
			var decoded map[string]any
			if err := json.Unmarshal(configuration, &decoded); err != nil {
				return err
			}
			s.rows[i].IndexConf = decoded
			return nil
		}
	}
	return indexregistryapp.ErrNotFound
}

func (s *registryMemoryStore) MarkDeleted(_ context.Context, projectID, toolkitID int32, indexID string) (indexregistryapp.Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, row := range s.rows {
		if row.IndexID == indexID && row.ProjectID == projectID && row.ToolkitID == toolkitID {
			if row.Active() {
				return indexregistryapp.Row{}, indexregistryapp.ErrActiveRun
			}
			s.rows = append(s.rows[:i], s.rows[i+1:]...)
			s.dead = append(s.dead, row)
			return row, nil
		}
	}
	return indexregistryapp.Row{}, indexregistryapp.ErrNotFound
}

func (s *registryMemoryStore) PurgeDeleted(_ context.Context, indexID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, row := range s.dead {
		if row.IndexID == indexID {
			s.dead = append(s.dead[:i], s.dead[i+1:]...)
		}
	}
	return nil
}

// add admits a run for an index and, when complete is set, applies a typed
// result, exactly as the initializer and the output projection would.
func (s *registryMemoryStore) add(t *testing.T, project, toolkit int32, name string, n int, complete bool) indexregistryapp.Row {
	t.Helper()
	run := indexingapp.RegistryInitialRun{
		ProjectID: project, ToolkitID: toolkit, IndexName: name,
		MetaID: name + "-meta", ExecutionID: name + "-exec", CorrelationID: name + "-corr",
		Generation: uint64(n), IndexGeneration: uint64(n),
		Configuration: []byte(`{"index_name":"` + name + `","progress_step":10}`),
		AdmittedAt:    time.Now().UTC().Add(-time.Minute),
	}
	row, _, err := indexregistryapp.StartRun(nil, run)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.seq++
	row.IndexID = "00000000-0000-4000-8000-00000000000" + string(rune('0'+s.seq))
	s.mu.Unlock()
	if complete {
		row, _, _, err = indexregistryapp.ApplyResult(row, indexregistryapp.Result{
			ExecutionID: run.ExecutionID, Generation: run.Generation, OccurredAt: time.Now().UTC(),
			Summary: outputapp.IndexIngestSummary{
				Status: outputapp.IndexIngestStatusOK, Message: "ok", TerminalState: outputapp.IndexIngestTerminalCompleted,
				IndexedDocuments: 4, IndexedChunks: 12, EmbeddingModel: "m", EmbeddingDimension: 8,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// The run's execution job is live until a test says otherwise (the real
	// store reads this from execution_jobs).
	row.RunActive = !complete
	s.mu.Lock()
	s.rows = append(s.rows, row)
	s.mu.Unlock()
	return row
}

type registryToolkits struct{ missing bool }

func (k registryToolkits) GetCurrentToolkit(_ context.Context, project, _, toolkit int32) (indexingapp.CurrentToolkitSnapshot, bool, error) {
	if k.missing {
		return indexingapp.CurrentToolkitSnapshot{}, false, nil
	}
	// No pgvector_configuration: a rust deployment's toolkit needs none.
	return indexingapp.CurrentToolkitSnapshot{ID: toolkit, Type: "github", Settings: map[string]any{}}, true, nil
}

type registrySchedules struct{ deleted []string }

func (s *registrySchedules) DeleteCurrentIndexSchedule(_ context.Context, _, _ int32, name string) error {
	s.deleted = append(s.deleted, name)
	return nil
}

type registryVectors struct {
	err     error
	deleted []indexingapp.IndexVectorNamespace
}

func (v *registryVectors) DeleteIndexVectors(_ context.Context, ns indexingapp.IndexVectorNamespace) error {
	v.deleted = append(v.deleted, ns)
	return v.err
}

type registryFixture struct {
	store     *registryMemoryStore
	schedules *registrySchedules
	vectors   *registryVectors
	service   *indexregistryapp.Service
	router    http.Handler
}

func newRegistryFixture(t *testing.T, toolkits registryToolkits) *registryFixture {
	t.Helper()
	f := &registryFixture{store: &registryMemoryStore{}, schedules: &registrySchedules{}, vectors: &registryVectors{err: indexingapp.ErrIndexVectorDeletionDeferred}}
	service, err := indexregistryapp.NewService(toolkits, f.store, f.schedules, f.vectors, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	f.service = service
	cfg := apimw.AuthConfig{
		PrincipalValidator: principalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) { return user, nil }),
		ForwardedIdentityVerifier: forwardedPeerVerifierFunc(func(r *http.Request) error {
			if r.RemoteAddr != "10.0.0.8:43120" {
				return errors.New("untrusted peer")
			}
			return nil
		}),
	}
	all := permissionResolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
		return authAll(), nil
	})
	list, err := handler.NewCurrentIndexMetaRoute(service, cfg, all)
	if err != nil {
		t.Fatal(err)
	}
	del, err := handler.NewCurrentIndexMetaDeleteRoute(service, cfg, all)
	if err != nil {
		t.Fatal(err)
	}
	save, err := handler.NewCurrentIndexConfigurationRoute(service, cfg, all)
	if err != nil {
		t.Fatal(err)
	}
	// The three real routes. Each owns one method of the shared prefix, as the
	// router composes them.
	f.router = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			list.ServeHTTP(w, r)
		case http.MethodDelete:
			del.ServeHTTP(w, r)
		case http.MethodPut:
			save.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	return f
}

func authAll() auth.PermissionResolution {
	return auth.PermissionResolution{UserID: 11, Permissions: []string{
		handler.CurrentIndexMetaListPermission,
		handler.CurrentIndexMetaDeletePermission,
		handler.CurrentIndexConfigurationPermission,
	}}
}

func (f *registryFixture) do(method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "11")
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "10.0.0.8:43120"
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, request)
	return response
}

const listPath = "/api/v2/elitea_core/index_meta/prompt_lib/7/9"

// The index list: a bare JSON array of {id, metadata, stale}. The client's
// shape guard (indexesApi.ts getIndexesList) turns anything else into an empty
// list, which would hide every index.
func TestRegistryIndexListKeepsTheShapeTheClientReads(t *testing.T) {
	f := newRegistryFixture(t, registryToolkits{})
	done := f.store.add(t, 7, 9, "docs", 1, true)
	running := f.store.add(t, 7, 9, "code", 2, false)
	f.store.add(t, 7, 8, "other-toolkit", 3, true)

	response := f.do(http.MethodGet, listPath, "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body)
	}
	body := strings.TrimSpace(response.Body.String())
	if !strings.HasPrefix(body, "[") {
		t.Fatalf("the list is not a bare array: %s", body)
	}
	var list []struct {
		ID       string         `json:"id"`
		Metadata map[string]any `json:"metadata"`
		Stale    bool           `json:"stale"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil || len(list) != 2 {
		t.Fatalf("list = %s err=%v", body, err)
	}
	if list[0].ID != done.IndexID || list[1].ID != running.IndexID {
		t.Fatalf("ids = %q, %q", list[0].ID, list[1].ID)
	}

	// Every key index_meta's metadata carries, with the type the client reads.
	for _, entry := range list {
		m := entry.Metadata
		for key, kind := range map[string]string{
			"collection": "string", "type": "string", "state": "string",
			"indexed": "number", "updated": "number", "created_on": "number", "updated_on": "number",
			"index_configuration": "object", "history": "array", "toolkit_id": "number",
			"index_generation": "number", "execution_generation": "number",
			"execution_id": "string", "index_meta_id": "string", "correlation_id": "string",
		} {
			if got := jsonKind(m[key]); got != kind {
				t.Fatalf("metadata[%q] is %s (%v), the client reads %s", key, got, m[key], kind)
			}
		}
		// task_id and conversation_id are string-or-null, never absent: the client
		// indexes them directly (metadata['task_id'], metadata['conversation_id']).
		for _, key := range []string{"task_id", "conversation_id"} {
			value, present := m[key]
			if !present || (value != nil && jsonKind(value) != "string") {
				t.Fatalf("metadata[%q] = %v (present=%v)", key, value, present)
			}
		}
		if m["type"] != "index_meta" {
			t.Fatalf("type = %v", m["type"])
		}
	}
	if list[0].Metadata["collection"] != "docs" || list[0].Metadata["state"] != "completed" ||
		list[0].Metadata["indexed"] != float64(4) {
		t.Fatalf("completed index metadata = %+v", list[0].Metadata)
	}
	if list[1].Metadata["state"] != "in_progress" || list[1].Metadata["task_id"] != "code-exec" {
		t.Fatalf("running index metadata = %+v", list[1].Metadata)
	}
	if list[0].Stale || list[1].Stale {
		t.Fatalf("a fresh run is not stale: %v %v", list[0].Stale, list[1].Stale)
	}

	// Each history entry is what IndexHistory.tsx renders: a state and the
	// timestamps, and the first is the run-neutral created marker.
	history := list[0].Metadata["history"].([]any)
	if len(history) != 2 {
		t.Fatalf("history = %v", history)
	}
	if first := history[0].(map[string]any); first["state"] != "created" {
		t.Fatalf("first history entry = %v", first)
	}
	last := history[len(history)-1].(map[string]any)
	if last["state"] != "completed" || jsonKind(last["created_on"]) != "number" || jsonKind(last["updated_on"]) != "number" {
		t.Fatalf("last history entry = %v", last)
	}
}

func jsonKind(value any) string {
	switch value.(type) {
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case nil:
		return "null"
	}
	return "other"
}

func TestRegistryIndexListOfAnEmptyToolkitIsAnEmptyArrayNotNull(t *testing.T) {
	f := newRegistryFixture(t, registryToolkits{})
	response := f.do(http.MethodGet, listPath, "")
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "[]" {
		t.Fatalf("status=%d body=%q", response.Code, response.Body)
	}
}

// A run is alive exactly while its execution job is: a dead run is flagged
// stale and keeps its state, however recently it last wrote.
func TestRegistryIndexListFlagsARunWhoseJobEndedWithoutChangingItsState(t *testing.T) {
	f := newRegistryFixture(t, registryToolkits{})
	row := f.store.add(t, 7, 9, "old", 1, false)
	f.store.rows[0].RunActive = false
	response := f.do(http.MethodGet, listPath, "")
	var list []struct {
		ID       string         `json:"id"`
		Metadata map[string]any `json:"metadata"`
		Stale    bool           `json:"stale"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil || len(list) != 1 || list[0].ID != row.IndexID {
		t.Fatalf("body=%s err=%v", response.Body, err)
	}
	if !list[0].Stale || list[0].Metadata["state"] != "in_progress" {
		t.Fatalf("stale=%v state=%v", list[0].Stale, list[0].Metadata["state"])
	}
}

// A long healthy run reports rarely: with its job RUNNING it is in progress and
// not stale no matter how old updated_at is.
func TestRegistryIndexListShowsALongHealthyRunAsInProgressNotStale(t *testing.T) {
	f := newRegistryFixture(t, registryToolkits{})
	f.store.add(t, 7, 9, "long", 1, false)
	f.store.rows[0].UpdatedAt = time.Now().Add(-48 * time.Hour)
	response := f.do(http.MethodGet, listPath, "")
	var list []struct {
		Metadata map[string]any `json:"metadata"`
		Stale    bool           `json:"stale"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil || len(list) != 1 {
		t.Fatalf("body=%s err=%v", response.Body, err)
	}
	if list[0].Stale || list[0].Metadata["state"] != "in_progress" {
		t.Fatalf("stale=%v state=%v", list[0].Stale, list[0].Metadata["state"])
	}
}

func TestRegistryIndexListOfAMissingToolkitAnswersLikeThePythonPath(t *testing.T) {
	f := newRegistryFixture(t, registryToolkits{missing: true})
	response := f.do(http.MethodGet, listPath, "")
	var body map[string]any
	if response.Code != http.StatusBadRequest || json.Unmarshal(response.Body.Bytes(), &body) != nil ||
		!strings.Contains(body["error"].(string), "Toolkit id is missing for toolkit 9") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
}

func TestRegistryIndexListNeverFailsForWantOfAPgvectorConfiguration(t *testing.T) {
	// The python path answers 400 "PGVector configuration is missing" for a
	// toolkit without one. A rust deployment's toolkits need none.
	f := newRegistryFixture(t, registryToolkits{})
	f.store.add(t, 7, 9, "docs", 1, true)
	if response := f.do(http.MethodGet, listPath, ""); response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
}

func TestRegistryIndexDeleteTombstonesFreesTheNameAndRemovesTheSchedule(t *testing.T) {
	f := newRegistryFixture(t, registryToolkits{})
	row := f.store.add(t, 7, 9, "docs", 1, true)

	response := f.do(http.MethodDelete, listPath+"/"+row.IndexID, `{"is_hidden":true}`)
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"ok":true}` {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	if got := f.do(http.MethodGet, listPath, "").Body.String(); strings.TrimSpace(got) != "[]" {
		t.Fatalf("the deleted index is still listed: %s", got)
	}
	// The vector deletion was asked for, with the index's namespace; the hook
	// deferred, so the tombstone is kept for the sweeper rather than purged.
	if len(f.vectors.deleted) != 1 || f.vectors.deleted[0] != (indexingapp.IndexVectorNamespace{ProjectID: 7, IndexID: row.IndexID}) {
		t.Fatalf("vector deletions = %+v", f.vectors.deleted)
	}
	if len(f.store.dead) != 1 {
		t.Fatalf("a tombstone must survive a deferred vector deletion, have %d", len(f.store.dead))
	}
	if len(f.schedules.deleted) != 1 || f.schedules.deleted[0] != "docs" {
		t.Fatalf("schedule cleanup = %v", f.schedules.deleted)
	}
}

func TestRegistryIndexDeletePurgesTheTombstoneOnceVectorsAreGone(t *testing.T) {
	f := newRegistryFixture(t, registryToolkits{})
	f.vectors.err = nil
	row := f.store.add(t, 7, 9, "docs", 1, true)
	if response := f.do(http.MethodDelete, listPath+"/"+row.IndexID, ""); response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	if len(f.store.dead) != 0 || len(f.store.rows) != 0 {
		t.Fatalf("rows=%d tombstones=%d, want both gone", len(f.store.rows), len(f.store.dead))
	}
}

func TestRegistryIndexDeleteUnknownAndMalformedIdsAre404(t *testing.T) {
	f := newRegistryFixture(t, registryToolkits{})
	for _, id := range []string{"00000000-0000-4000-8000-0000000000ff", "not-a-uuid", "docs"} {
		response := f.do(http.MethodDelete, listPath+"/"+id, "")
		var body map[string]any
		if response.Code != http.StatusNotFound || json.Unmarshal(response.Body.Bytes(), &body) != nil ||
			body["ok"] != false || body["error"] != "index_meta "+id+" not found" {
			t.Fatalf("id %q: status=%d body=%s", id, response.Code, response.Body)
		}
	}
}

func TestRegistryIndexDeleteRefusesARunStillWriting(t *testing.T) {
	f := newRegistryFixture(t, registryToolkits{})
	row := f.store.add(t, 7, 9, "busy", 1, false)
	response := f.do(http.MethodDelete, listPath+"/"+row.IndexID, "")
	var body map[string]any
	if response.Code != http.StatusConflict || json.Unmarshal(response.Body.Bytes(), &body) != nil || body["ok"] != false {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	if len(f.vectors.deleted) != 0 || len(f.store.rows) != 1 {
		t.Fatal("an active run's index was deleted")
	}
}

func TestRegistryIndexDeleteRefusesALongHealthyRunButClearsADeadOne(t *testing.T) {
	f := newRegistryFixture(t, registryToolkits{})
	long := f.store.add(t, 7, 9, "long", 1, false)
	f.store.rows[0].UpdatedAt = time.Now().Add(-48 * time.Hour)
	if got := f.do(http.MethodDelete, listPath+"/"+long.IndexID, "").Code; got != http.StatusConflict {
		t.Fatalf("a healthy run with an old updated_at: status = %d, want 409", got)
	}
	f.store.rows[0].RunActive = false // its job ended, or is missing
	if got := f.do(http.MethodDelete, listPath+"/"+long.IndexID, "").Code; got != http.StatusOK {
		t.Fatalf("a run whose job ended: status = %d, want 200", got)
	}
}

func TestRegistryIndexConfigurationSave(t *testing.T) {
	f := newRegistryFixture(t, registryToolkits{})
	row := f.store.add(t, 7, 9, "docs", 1, true)
	path := listPath + "/docs/configuration"

	response := f.do(http.MethodPut, path, `{"index_configuration":{"index_name":"docs","progress_step":50,"fetch_limit":null}}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	listed := f.do(http.MethodGet, listPath, "").Body.Bytes()
	var list []struct {
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(listed, &list); err != nil || len(list) != 1 {
		t.Fatal(err)
	}
	configuration := list[0].Metadata["index_configuration"].(map[string]any)
	if configuration["progress_step"] != float64(50) {
		t.Fatalf("saved configuration = %v", configuration)
	}
	if value, present := configuration["fetch_limit"]; !present || value != nil {
		t.Fatalf("a cleared field must stay present as null (indexesApi.ts preserveExplicitClears): %v", configuration)
	}
	// Saving does not start a run or touch the run's fields.
	if list[0].Metadata["state"] != string(row.State) || list[0].Metadata["task_id"] != indexregistryapp.Metadata(row)["task_id"] {
		t.Fatalf("saving changed the run state: %v", list[0].Metadata)
	}

	for body, want := range map[string]int{
		`{"index_configuration":[1]}`: http.StatusBadRequest,
		`{"nope":{}}`:                 http.StatusBadRequest,
	} {
		if got := f.do(http.MethodPut, path, body).Code; got != want {
			t.Fatalf("body %s: status = %d, want %d", body, got, want)
		}
	}
	if got := f.do(http.MethodPut, listPath+"/ghost/configuration", `{"index_configuration":{}}`).Code; got != http.StatusNotFound {
		t.Fatalf("saving a configuration for an unknown index: status = %d, want 404", got)
	}
}
