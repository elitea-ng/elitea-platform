package runbrief

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

const (
	testRoot = "0123456789abcdef0123456789abcdef"
	testRun  = "fedcba9876543210fedcba9876543210"
)

type fakeCompleter struct {
	mu       sync.Mutex
	answers  []string
	err      error
	requests []CompletionRequest
	gate     chan struct{}
}

func (f *fakeCompleter) Complete(ctx context.Context, request CompletionRequest) (string, error) {
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	if f.err != nil {
		return "", f.err
	}
	answer := f.answers[0]
	if len(f.answers) > 1 {
		f.answers = f.answers[1:]
	}
	return answer, nil
}

func (f *fakeCompleter) calls() []CompletionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]CompletionRequest(nil), f.requests...)
}

type fakeModels struct{ model string }

func (f fakeModels) LowTierModel(context.Context, int64) (string, error) { return f.model, nil }

type fakeSettings struct{ off bool }

func (f fakeSettings) RunBriefEnabled(context.Context) (bool, error) { return !f.off, nil }

type fakeStore struct {
	mu     sync.Mutex
	chain  ChainBrief
	links  map[string]*Result
	stored int
}

func (f *fakeStore) ChainBrief(_ context.Context, root string) (ChainBrief, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if root != f.chain.RootExecutionID {
		return ChainBrief{}, false, nil
	}
	return f.chain, true, nil
}

func (f *fakeStore) SaveBrief(ctx context.Context, root, state string, brief *Brief) (ChainBrief, error) {
	f.mu.Lock()
	if f.chain.State == unattended.BriefPending {
		f.chain.State, f.chain.Brief = state, brief
		f.stored++
	}
	f.mu.Unlock()
	chain, _, err := f.ChainBrief(ctx, root)
	return chain, err
}

func (f *fakeStore) LinkCheck(_ context.Context, _ string, executionID string) (*Result, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	result, ok := f.links[executionID]
	return result, ok, nil
}

func (f *fakeStore) RecordCheck(
	_ context.Context, _ string, executionID string, result Result, failures map[string]int,
) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if stored := f.links[executionID]; stored != nil {
		return *stored, nil
	}
	f.links[executionID] = &result
	f.chain.Failures = failures
	f.chain.LastCheck = &result
	return result, nil
}

type fakeTranscripts struct{ request, transcript string }

func (f fakeTranscripts) RootRequest(context.Context, string) (string, error) { return f.request, nil }
func (f fakeTranscripts) Transcript(context.Context, string) (string, error) {
	return f.transcript, nil
}

const readyAnswer = `{"goal":"Ship the export","done_when":[{"id":"c1","text":"The button exists"},` +
	`{"id":"c2","text":"The test passes"}],"constraints":[],"assumptions":[],"ambiguity":"low"}`

func newTestService(completer *fakeCompleter, store *fakeStore, settings fakeSettings) *Service {
	return New(completer, fakeModels{model: "small-model"}, settings, store,
		fakeTranscripts{request: "Add a CSV export", transcript: "User:\nAdd a CSV export"}, nil)
}

func pendingStore(origin string) *fakeStore {
	return &fakeStore{
		chain: ChainBrief{RootExecutionID: testRoot, ProjectID: 7, OwnerUserID: 3, Origin: origin,
			State: unattended.BriefPending, Failures: map[string]int{}},
		links: map[string]*Result{testRoot: nil, testRun: nil},
	}
}

func readyStore() *fakeStore {
	store := pendingStore(unattended.OriginCloud)
	brief, err := ParseBrief(readyAnswer)
	if err != nil {
		panic(err)
	}
	store.chain.State, store.chain.Brief = unattended.BriefReady, &brief
	return store
}

func TestEnsureBriefWritesTheBriefOnceOnTheLowTierModel(t *testing.T) {
	completer := &fakeCompleter{answers: []string{readyAnswer}}
	store := pendingStore(unattended.OriginDesktop)
	service := newTestService(completer, store, fakeSettings{})

	chain, found, err := service.EnsureBrief(t.Context(), testRoot, "Add a CSV export")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, unattended.BriefReady, chain.State)
	require.Equal(t, "Ship the export", chain.Brief.Goal)
	calls := completer.calls()
	require.Len(t, calls, 1)
	require.Equal(t, CompletionRequest{
		ProjectID: 7, UserID: 3, Model: "small-model", System: briefSystemPrompt,
		User: "<request>\nAdd a CSV export\n</request>", AttributionID: "run_brief:" + testRoot,
		MaxTokens: briefMaxTokens,
	}, calls[0])

	again, _, err := service.EnsureBrief(t.Context(), testRoot, "Another request")
	require.NoError(t, err)
	require.Equal(t, chain, again, "the brief is frozen")
	require.Len(t, completer.calls(), 1)
}

func TestEnsureBriefFailsOnAModelErrorOrAnInvalidAnswer(t *testing.T) {
	for name, completer := range map[string]*fakeCompleter{
		"error":   {err: errors.New("gateway down")},
		"invalid": {answers: []string{"Sure! Here is a brief."}},
	} {
		t.Run(name, func(t *testing.T) {
			store := pendingStore(unattended.OriginDesktop)
			chain, _, err := newTestService(completer, store, fakeSettings{}).EnsureBrief(t.Context(), testRoot, "r")
			require.NoError(t, err)
			require.Equal(t, unattended.BriefFailed, chain.State)
			require.Nil(t, chain.Brief)
		})
	}
}

func TestEnsureBriefWithTheSwitchOffMakesNoCall(t *testing.T) {
	completer := &fakeCompleter{answers: []string{readyAnswer}}
	chain, _, err := newTestService(completer, pendingStore(unattended.OriginDesktop), fakeSettings{off: true}).
		EnsureBrief(t.Context(), testRoot, "r")
	require.NoError(t, err)
	require.Equal(t, unattended.BriefOff, chain.State)
	require.Empty(t, completer.calls())
}

func TestEnsureBriefReadsTheCloudRequestAndWaitsForAWriterInFlight(t *testing.T) {
	completer := &fakeCompleter{answers: []string{readyAnswer}, gate: make(chan struct{})}
	store := pendingStore(unattended.OriginCloud)
	service := newTestService(completer, store, fakeSettings{})

	started := time.Now()
	service.Begin(t.Context(), testRoot, "")
	require.Less(t, time.Since(started), time.Second, "Begin never blocks")

	done := make(chan ChainBrief)
	go func() {
		require.Eventually(t, func() bool {
			service.mu.Lock()
			defer service.mu.Unlock()
			return len(service.inflight) == 1
		}, time.Second, time.Millisecond)
		chain, _, err := service.EnsureBrief(context.Background(), testRoot, "")
		require.NoError(t, err)
		done <- chain
	}()
	time.Sleep(20 * time.Millisecond)
	close(completer.gate)
	chain := <-done
	require.Equal(t, unattended.BriefReady, chain.State)
	calls := completer.calls()
	require.Len(t, calls, 1, "the waiting call makes no second model call")
	require.Equal(t, "<request>\nAdd a CSV export\n</request>", calls[0].User)
}

func TestCheckMetIsRecordedAndReplaysTheStoredCheck(t *testing.T) {
	completer := &fakeCompleter{answers: []string{
		`{"conditions":[{"id":"c1","verdict":"met","reason":"seen"},{"id":"c2","verdict":"met","reason":"passed"}]}`,
	}}
	store := readyStore()
	service := newTestService(completer, store, fakeSettings{})

	result, checked, err := service.CheckCloud(t.Context(), testRoot, testRun)
	require.NoError(t, err)
	require.True(t, checked)
	require.Equal(t, OutcomeMet, result.Outcome)
	require.Equal(t, 2, result.Met)
	require.Equal(t, 2, result.Total)
	calls := completer.calls()
	require.Len(t, calls, 1)
	require.Equal(t, "goal_check:"+testRun, calls[0].AttributionID)
	require.Contains(t, calls[0].User, "c2: The test passes")
	require.Contains(t, calls[0].User, "<transcript>\nUser:\nAdd a CSV export\n</transcript>")

	replay, checked, err := service.CheckCloud(t.Context(), testRoot, testRun)
	require.NoError(t, err)
	require.True(t, checked)
	require.Equal(t, result, replay)
	require.Len(t, completer.calls(), 1, "a replay never asks the model again")
}

func TestCheckNotMetCarriesTheContinuationText(t *testing.T) {
	completer := &fakeCompleter{answers: []string{
		`{"conditions":[{"id":"c1","verdict":"met","reason":"seen"},{"id":"c2","verdict":"not_met","reason":"No test run."}]}`,
	}}
	store := readyStore()
	result, _, err := newTestService(completer, store, fakeSettings{}).CheckCloud(t.Context(), testRoot, testRun)
	require.NoError(t, err)
	require.Equal(t, OutcomeNotMet, result.Outcome)
	require.Equal(t, "No test run.", result.Reason)
	require.Equal(t, ContinuationText(store.chain.Brief, &result), result.ContinueText)
	require.Contains(t, result.ContinueText, "Not met yet:\n- The test passes\nLast check: No test run.\n")
	require.Equal(t, map[string]int{"c2": 1}, store.chain.Failures)
}

func TestCheckModelFailureIsNotChecked(t *testing.T) {
	store := readyStore()
	store.chain.Failures = map[string]int{"c2": 2}
	result, checked, err := newTestService(&fakeCompleter{err: context.DeadlineExceeded}, store, fakeSettings{}).
		CheckCloud(t.Context(), testRoot, testRun)
	require.NoError(t, err)
	require.True(t, checked)
	require.Equal(t, OutcomeNotChecked, result.Outcome)
	require.Equal(t, 2, result.Total)
	require.Empty(t, result.ContinueText)
	require.Equal(t, map[string]int{"c2": 2}, store.chain.Failures, "a failed check keeps the counts")
}

func TestCheckWithoutConditionsOrWithTheSwitchOffChecksNothing(t *testing.T) {
	completer := &fakeCompleter{answers: []string{"{}"}}
	store := readyStore()
	store.chain.Brief.DoneWhen = []Condition{}
	_, checked, err := newTestService(completer, store, fakeSettings{}).CheckCloud(t.Context(), testRoot, testRun)
	require.NoError(t, err)
	require.False(t, checked)

	_, checked, err = newTestService(completer, readyStore(), fakeSettings{off: true}).
		CheckCloud(t.Context(), testRoot, testRun)
	require.NoError(t, err)
	require.False(t, checked)
	require.Empty(t, completer.calls())
}

func TestCheckRefusesAnExecutionOutsideTheChain(t *testing.T) {
	_, _, err := newTestService(&fakeCompleter{}, readyStore(), fakeSettings{}).
		CheckCloud(t.Context(), testRoot, "00000000000000000000000000000000")
	require.ErrorIs(t, err, ErrNotLink)
}

func TestContinueTextRestatesAReadyBrief(t *testing.T) {
	store := readyStore()
	service := newTestService(&fakeCompleter{}, store, fakeSettings{})
	require.Equal(t, ContinuationText(store.chain.Brief, nil), service.ContinueText(t.Context(), testRoot))
	store.chain.State, store.chain.Brief = unattended.BriefFailed, nil
	require.Equal(t, unattended.ContinuePrompt, service.ContinueText(t.Context(), testRoot))
	var none *Service
	require.Equal(t, unattended.ContinuePrompt, none.ContinueText(t.Context(), testRoot))
}
