package runevidence

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runevidence"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

type fakeStore struct {
	head     repos.RunHead
	records  []runevidence.Record
	auditErr error
	audited  []string
}

func (f *fakeStore) Head(_ context.Context, projectID int64, runID string) (repos.RunHead, error) {
	if projectID != f.head.ProjectID || runID != f.head.RunID {
		return repos.RunHead{}, repos.ErrRunNotFound
	}
	return f.head, nil
}

func (f *fakeStore) StepsPage(_ context.Context, _ int64, _ string, after int64, limit int) ([]runevidence.Record, error) {
	var out []runevidence.Record
	for _, r := range f.records {
		if r.Seq > after && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeStore) VerifyRun(ctx context.Context, projectID int64, runID string) (repos.RunVerification, error) {
	head, err := f.Head(ctx, projectID, runID)
	if err != nil {
		return repos.RunVerification{}, err
	}
	var v runevidence.Verifier
	for _, r := range f.records {
		if b := v.Next(r); b != nil {
			return repos.RunVerification{Head: head, Verified: v.Count(), Break: b}, nil
		}
	}
	return repos.RunVerification{Head: head, Verified: v.Count(), Break: v.Finish(head.LastSeq, head.LastHash)}, nil
}

func (f *fakeStore) Step(_ context.Context, _ int64, _ string, stepID string) (runevidence.Record, error) {
	for _, r := range f.records {
		if r.StepID == stepID {
			return r, nil
		}
	}
	return runevidence.Record{}, repos.ErrRunNotFound
}

func (f *fakeStore) ConversationRuns(context.Context, int64, string) ([]repos.ConversationRun, error) {
	return []repos.ConversationRun{{RunID: "run-1", Steps: 2}}, nil
}

func (f *fakeStore) RecordContentRead(_ context.Context, userID, _ int64, runID, stepID string, side runevidence.Side, route string) error {
	if f.auditErr != nil {
		return f.auditErr
	}
	f.audited = append(f.audited, runID+"/"+stepID+"/"+string(side)+"@"+route)
	return nil
}

type fakeContent map[string][]byte

func (f fakeContent) Put(context.Context, string, []byte, string, []byte) error { return nil }
func (f fakeContent) Get(_ context.Context, key string) ([]byte, error)         { return f[key], nil }
func (f fakeContent) Delete(context.Context, []string) error                    { return nil }

const (
	stepOne = "0192f0c1-7b6e-7c3a-8f00-0000000000a1"
	stepTwo = "0192f0c1-7b6e-7c3a-8f00-0000000000a2"
)

// chain builds two chained records; the first has stored inputs, the
// second expired content.
func chain(t *testing.T, body []byte) *fakeStore {
	t.Helper()
	at := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	digest := sha256.Sum256(body)
	expired := at.Add(time.Hour)
	records := []runevidence.Record{
		{RunID: "run-1", Seq: 1, StepID: stepOne, Generation: 1, ProjectID: 3, Kind: runevidence.KindToolCall, Actor: "user:8",
			StartedAt: at, EndedAt: at, Summary: []byte(`{}`),
			Inputs: runevidence.ContentMeta{Digest: digest[:], Size: int64(len(body)), OriginalSize: int64(len(body)), MediaType: runevidence.MediaJSON, ObjectKey: "3/runs/run-1/a1/inputs"}},
		{RunID: "run-1", Seq: 2, StepID: stepTwo, Generation: 1, ProjectID: 3, Kind: runevidence.KindOutput, Actor: "user:8",
			StartedAt: at, EndedAt: at, Summary: []byte(`{}`),
			Outputs:          runevidence.ContentMeta{Digest: digest[:], Size: 2, OriginalSize: 2, MediaType: runevidence.MediaText},
			ContentExpiredAt: &expired, ContentExpiredPolicy: "retention.step_content:30d"},
	}
	prev := runevidence.ZeroHash
	for i := range records {
		records[i].PrevHash = prev
		hash, err := runevidence.ChainHash(prev, records[i])
		if err != nil {
			t.Fatal(err)
		}
		records[i].Hash, prev = hash, hash
	}
	return &fakeStore{head: repos.RunHead{RunID: "run-1", ProjectID: 3, LastSeq: 2, LastHash: prev}, records: records}
}

func serve(t *testing.T, pattern, target string, handler http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Get(pattern, handler)
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: "8", UserID: "8"}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func decode[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
		t.Fatalf("%d %s: %v", recorder.Code, recorder.Body, err)
	}
	return out
}

func TestVerifyNamesTheFirstBrokenSeq(t *testing.T) {
	store := chain(t, []byte(`{"q":1}`))
	handler := NewHandler(store, nil)
	ok := decode[Verification](t, serve(t, VerifyPath, "/run_steps/prompt_lib/3/run-1/verify", handler.Verify))
	if !ok.OK || ok.Break != nil || ok.Verified != 2 {
		t.Fatalf("intact chain: %+v", ok)
	}
	store.records[1].Name = "rewritten"
	broken := decode[Verification](t, serve(t, VerifyPath, "/run_steps/prompt_lib/3/run-1/verify", handler.Verify))
	if broken.OK || broken.Break == nil || broken.Break.Seq != 2 {
		t.Fatalf("tampered chain: %+v", broken)
	}
	if recorder := serve(t, VerifyPath, "/run_steps/prompt_lib/4/run-1/verify", handler.Verify); recorder.Code != http.StatusNotFound {
		t.Fatalf("another project's run: %d", recorder.Code)
	}
}

func TestListStepsPagesAndCarriesNoContent(t *testing.T) {
	handler := NewHandler(chain(t, []byte(`{"secret":1}`)), nil)
	recorder := serve(t, StepsPath, "/run_steps/prompt_lib/3/run-1?limit=1", handler.ListSteps)
	page := decode[StepsPage](t, recorder)
	if len(page.Steps) != 1 || page.NextSeq == nil || *page.NextSeq != 1 || page.Run.Steps != 2 {
		t.Fatalf("page = %+v", page)
	}
	if page.Steps[0].Inputs.Status != "stored" || page.Steps[0].Inputs.Digest == nil {
		t.Fatalf("inputs = %+v", page.Steps[0].Inputs)
	}
	if strings.Contains(recorder.Body.String(), "secret") {
		t.Fatal("a step list carries content")
	}
	if serve(t, StepsPath, "/run_steps/prompt_lib/3/run-1?limit=5000", handler.ListSteps).Code != http.StatusBadRequest {
		t.Fatal("limit over the bound accepted")
	}
}

func TestGetContentAuditsBeforeAnswering(t *testing.T) {
	body := []byte(`{"q":1}`)
	store := chain(t, body)
	handler := NewHandler(store, fakeContent{"3/runs/run-1/a1/inputs": body})
	recorder := serve(t, ContentPath, "/run_steps/prompt_lib/3/run-1/"+stepOne+"/inputs", handler.GetContent)
	answer := decode[Content](t, recorder)
	if answer.Status != "stored" || answer.Content == nil || *answer.Content != string(body) || *answer.Encoding != "utf-8" {
		t.Fatalf("content = %+v", answer)
	}
	if len(store.audited) != 1 || store.audited[0] != "run-1/"+stepOne+"/inputs@"+ContentPath {
		t.Fatalf("audited = %v", store.audited)
	}

	store.auditErr = errors.New("db down")
	if recorder := serve(t, ContentPath, "/run_steps/prompt_lib/3/run-1/"+stepOne+"/inputs", handler.GetContent); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unaudited read answered %d", recorder.Code)
	}
	store.auditErr = nil

	tampered := NewHandler(store, fakeContent{"3/runs/run-1/a1/inputs": []byte(`{"q":2}`)})
	if recorder := serve(t, ContentPath, "/run_steps/prompt_lib/3/run-1/"+stepOne+"/inputs", tampered.GetContent); recorder.Code != http.StatusConflict {
		t.Fatalf("content not matching its digest answered %d", recorder.Code)
	}

	expired := decode[Content](t, serve(t, ContentPath, "/run_steps/prompt_lib/3/run-1/"+stepTwo+"/outputs", handler.GetContent))
	if expired.Status != "expired" || expired.ExpiredAt == nil || expired.Policy == nil || *expired.Policy != "retention.step_content:30d" || expired.Content != nil {
		t.Fatalf("expired = %+v", expired)
	}
	if serve(t, ContentPath, "/run_steps/prompt_lib/3/run-1/"+stepOne+"/everything", handler.GetContent).Code != http.StatusBadRequest {
		t.Fatal("unknown side accepted")
	}
}

func TestNilStoreAnswers503(t *testing.T) {
	var typed *repos.RunSteps
	handler := NewHandler(typed, nil)
	if serve(t, ConversationRunsPath, "/conversation_runs/prompt_lib/3/x", handler.ListConversationRuns).Code != http.StatusServiceUnavailable {
		t.Fatal("nil store did not answer 503")
	}
}
