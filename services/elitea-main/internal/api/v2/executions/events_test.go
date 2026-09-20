package executions

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type eventAuthorizerStub struct {
	err   error
	calls int
}

type eventAuthorizerFunc func(context.Context, string, string) error

func (function eventAuthorizerFunc) AuthorizeExecutionEvents(
	ctx context.Context,
	projectID,
	executionID string,
) error {
	return function(ctx, projectID, executionID)
}

func (s *eventAuthorizerStub) AuthorizeExecutionEvents(_ context.Context, _, _ string) error {
	s.calls++
	return s.err
}

type eventRepositoryStub struct {
	events  map[uint64][]DurableEvent
	err     error
	cursors []uint64
}

type concurrentEmptyEventRepository struct{}

func (concurrentEmptyEventRepository) Replay(
	context.Context,
	string,
	string,
	uint64,
	int,
) ([]DurableEvent, error) {
	return nil, nil
}

func (s *eventRepositoryStub) Replay(_ context.Context, _, _ string, afterCursor uint64, _ int) ([]DurableEvent, error) {
	s.cursors = append(s.cursors, afterCursor)
	if s.err != nil {
		return nil, s.err
	}
	return append([]DurableEvent(nil), s.events[afterCursor]...), nil
}

type replayWaiterStub struct {
	calls []uint64
}

type oneWakeWaiter struct{}

func (oneWakeWaiter) Wait(context.Context, string, string, uint64) (bool, error) {
	return true, nil
}

type revokingEventAuthorizer struct {
	calls int
}

type simultaneousReauthorizationWaiter struct {
	arrived chan string
	release chan struct{}
	mu      sync.Mutex
	calls   map[string]int
}

func (w *simultaneousReauthorizationWaiter) Wait(
	ctx context.Context,
	_ string,
	executionID string,
	_ uint64,
) (bool, error) {
	w.mu.Lock()
	w.calls[executionID]++
	call := w.calls[executionID]
	w.mu.Unlock()
	if call > 1 {
		return false, context.Canceled
	}
	select {
	case w.arrived <- executionID:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	select {
	case <-w.release:
		return false, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (a *revokingEventAuthorizer) AuthorizeExecutionEvents(context.Context, string, string) error {
	a.calls++
	if a.calls > 1 {
		return ErrExecutionEventsForbidden
	}
	return nil
}

type streamingRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func newStreamingRecorder() *streamingRecorder {
	return &streamingRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (r *streamingRecorder) SetWriteDeadline(deadline time.Time) error {
	r.deadlines = append(r.deadlines, deadline)
	return nil
}

func (r *streamingRecorder) Flush() { r.ResponseRecorder.Flush() }

func (s *replayWaiterStub) Wait(_ context.Context, _, _ string, afterCursor uint64) (bool, error) {
	s.calls = append(s.calls, afterCursor)
	return false, context.Canceled
}

func TestEventHandlerReplaysFromDurableLastEventIDAndNeverAcceptsPayloadFromWaiter(t *testing.T) {
	authorizer := &eventAuthorizerStub{}
	repository := &eventRepositoryStub{events: map[uint64][]DurableEvent{
		4: {{Cursor: 5, Type: "configuration.validation.completed", Data: json.RawMessage(`{"valid":false}`)}},
	}}
	waiter := &replayWaiterStub{}
	handler, err := NewEventHandler(authorizer, repository, waiter)
	if err != nil {
		t.Fatal(err)
	}
	request := executionEventsRequest()
	request.Header.Set("Last-Event-ID", "4")
	response := newStreamingRecorder()
	handler.Stream(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", response.Code, response.Body.String())
	}
	want := "id: 5\nevent: configuration.validation.completed\ndata: {\"valid\":false}\n\n"
	if response.Body.String() != want {
		t.Fatalf("unexpected SSE replay:\n%s", response.Body.String())
	}
	if !reflect.DeepEqual(repository.cursors, []uint64{4}) || !reflect.DeepEqual(waiter.calls, []uint64{5}) {
		t.Fatalf("cursor was not advanced from durable replay: repository=%v waiter=%v", repository.cursors, waiter.calls)
	}
	if len(response.deadlines) < 5 || response.deadlines[0] != (time.Time{}) || response.deadlines[len(response.deadlines)-1] != (time.Time{}) {
		t.Fatalf("SSE writes did not clear and bound write deadlines: %v", response.deadlines)
	}
	bounded := false
	for _, deadline := range response.deadlines {
		if !deadline.IsZero() {
			bounded = true
			break
		}
	}
	if !bounded {
		t.Fatal("SSE stream installed no bounded per-write deadline")
	}
}

func TestEventHandlerStreamsReplayResetInsteadOfRetryingExpiredCursor(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		authorizer := &eventAuthorizerStub{}
		repository := &eventRepositoryStub{events: map[uint64][]DurableEvent{
			9: {{
				Cursor: 12,
				Type:   "execution.replay_reset",
				Data:   json.RawMessage(`{"reason":"progress_retention_window_elapsed"}`),
			}, {
				Cursor: 13,
				Type:   "index.ingest.completed",
				Data:   json.RawMessage(`{"status":"ok"}`),
			}},
		}}
		handler, err := NewEventHandler(authorizer, repository, &replayWaiterStub{})
		if err != nil {
			t.Fatal(err)
		}
		request := executionEventsRequest()
		request.Header.Set("Last-Event-ID", "9")
		response := newStreamingRecorder()
		handler.Stream(response, request)
		if response.Code != http.StatusOK ||
			!strings.Contains(response.Body.String(), "id: 12\nevent: execution.replay_reset\ndata:") ||
			!strings.Contains(response.Body.String(), "id: 13\nevent: index.ingest.completed\ndata:") {
			t.Fatalf("unexpected expired-cursor reset %d %s", response.Code, response.Body.String())
		}
	})
}

func TestEventHandlerRejectsConflictingCursorSourcesBeforeStreaming(t *testing.T) {
	t.Run("conflicting", func(t *testing.T) {
		authorizer := &eventAuthorizerStub{}
		repository := &eventRepositoryStub{}
		handler, err := NewEventHandler(authorizer, repository, &replayWaiterStub{})
		if err != nil {
			t.Fatal(err)
		}
		request := executionEventsRequest()
		request.Header.Set("Last-Event-ID", "9")
		query := request.URL.Query()
		query.Set("cursor", "8")
		request.URL.RawQuery = query.Encode()
		response := newStreamingRecorder()
		handler.Stream(response, request)
		if response.Code != http.StatusBadRequest || len(repository.cursors) != 0 {
			t.Fatalf("conflicting cursor reached durable repository: status=%d cursors=%v", response.Code, repository.cursors)
		}
	})
}

func TestEventHandlerAuthorizesBeforeDurableLookup(t *testing.T) {
	authorizer := &eventAuthorizerStub{err: ErrExecutionEventsForbidden}
	repository := &eventRepositoryStub{}
	handler, err := NewEventHandler(authorizer, repository, &replayWaiterStub{})
	if err != nil {
		t.Fatal(err)
	}
	response := newStreamingRecorder()
	handler.Stream(response, executionEventsRequest())
	if response.Code != http.StatusForbidden || len(repository.cursors) != 0 {
		t.Fatalf("forbidden stream reached repository: status=%d cursors=%v", response.Code, repository.cursors)
	}
}

func TestEventHandlerRejectsPrincipalWithoutServerAuthenticationProvenance(t *testing.T) {
	authorizer := &eventAuthorizerStub{}
	repository := &eventRepositoryStub{}
	handler, err := NewEventHandler(authorizer, repository, &replayWaiterStub{})
	if err != nil {
		t.Fatal(err)
	}

	for name, request := range map[string]*http.Request{
		"missing principal": executionEventsRequestWithoutPrincipal(),
		"unprovenanced principal": func() *http.Request {
			request := executionEventsRequestWithoutPrincipal()
			return request.WithContext(auth.ContextWithUser(
				request.Context(),
				auth.User{ID: "7", UserID: "7", AuthType: "user"},
			))
		}(),
		"token without owner": func() *http.Request {
			request := executionEventsRequestWithoutPrincipal()
			return request.WithContext(auth.ContextWithAuthenticatedUser(
				request.Context(),
				auth.User{ID: "901", TokenID: "901", AuthType: "token"},
				auth.AuthenticationSourceToken,
			))
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			response := newStreamingRecorder()
			handler.Stream(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d, want=%d", response.Code, http.StatusUnauthorized)
			}
		})
	}
	if authorizer.calls != 0 || len(repository.cursors) != 0 {
		t.Fatalf(
			"unprovenanced request reached protected dependencies: authorizer=%d replay=%v",
			authorizer.calls,
			repository.cursors,
		)
	}
}

func TestEventHandlerAuthorizationCapacityUsesOwningPrincipalAcrossPATs(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	authorizer := eventAuthorizerFunc(func(ctx context.Context, _, _ string) error {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	handler, err := NewEventHandler(authorizer, &eventRepositoryStub{}, &replayWaiterStub{})
	if err != nil {
		t.Fatal(err)
	}
	handler.authorizationTimeout = time.Second

	firstDone := make(chan *streamingRecorder, 1)
	go func() {
		response := newStreamingRecorder()
		handler.Stream(response, executionEventsRequestForPrincipal(auth.User{
			ID: "7", UserID: "7", TokenID: "901", AuthType: "token",
		}, auth.AuthenticationSourceToken))
		firstDone <- response
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first PAT did not enter authorization")
	}

	second := newStreamingRecorder()
	handler.Stream(second, executionEventsRequestForPrincipal(auth.User{
		ID: "7", UserID: "7", TokenID: "902", AuthType: "token",
	}, auth.AuthenticationSourceToken))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second PAT status=%d, want=%d", second.Code, http.StatusTooManyRequests)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("same-owner PAT bypassed authorization gate: calls=%d", got)
	}

	close(release)
	select {
	case first := <-firstDone:
		if first.Code != http.StatusOK {
			t.Fatalf("first PAT status=%d body=%s", first.Code, first.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("first PAT did not complete")
	}
}

func TestEventHandlerSerializesContinuedReauthorizationForFourSameOwnerStreams(
	t *testing.T,
) {
	waiter := &simultaneousReauthorizationWaiter{
		arrived: make(chan string, 4),
		release: make(chan struct{}),
		calls:   make(map[string]int),
	}
	firstReauthorizationEntered := make(chan struct{})
	releaseFirstReauthorization := make(chan struct{})
	var authorizationCalls atomic.Int32
	authorizer := eventAuthorizerFunc(func(ctx context.Context, _, _ string) error {
		call := authorizationCalls.Add(1)
		if call != 5 {
			return nil
		}
		close(firstReauthorizationEntered)
		select {
		case <-releaseFirstReauthorization:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	handler, err := NewEventHandler(
		authorizer,
		concurrentEmptyEventRepository{},
		waiter,
	)
	if err != nil {
		t.Fatal(err)
	}
	handler.authorizationTimeout = time.Second

	responses := make(chan *streamingRecorder, 4)
	for index := range 4 {
		executionID := "execution-" + strconv.Itoa(index+1)
		go func() {
			response := newStreamingRecorder()
			handler.Stream(
				response,
				executionEventsRequestForExecution(executionID),
			)
			responses <- response
		}()
		select {
		case arrivedExecution := <-waiter.arrived:
			if arrivedExecution != executionID {
				t.Fatalf(
					"stream %q reached waiter as %q",
					executionID,
					arrivedExecution,
				)
			}
		case <-time.After(time.Second):
			t.Fatalf("stream %q did not reach the reauthorization barrier", executionID)
		}
	}

	close(waiter.release)
	select {
	case <-firstReauthorizationEntered:
	case <-time.After(time.Second):
		t.Fatal("no continued stream entered authorization")
	}
	if got := authorizationCalls.Load(); got != 5 {
		t.Fatalf(
			"same-owner continuation bypassed serialization before release: calls=%d",
			got,
		)
	}
	close(releaseFirstReauthorization)

	for range 4 {
		select {
		case response := <-responses:
			if response.Code != http.StatusOK {
				t.Fatalf(
					"established stream was dropped during gate contention: status=%d body=%s",
					response.Code,
					response.Body.String(),
				)
			}
		case <-time.After(time.Second):
			t.Fatal("established stream did not complete reauthorization")
		}
	}
	if got := authorizationCalls.Load(); got != 8 {
		t.Fatalf(
			"continued authorization calls=%d, want four initial plus four serialized",
			got,
		)
	}
}

func TestEventHandlerAuthorizationGateReleasesOnDenyErrorAndCancellation(t *testing.T) {
	tests := []struct {
		name      string
		firstCall func(context.Context) error
		want      error
	}{
		{
			name:      "deny",
			firstCall: func(context.Context) error { return ErrExecutionEventsForbidden },
			want:      ErrExecutionEventsForbidden,
		},
		{
			name:      "dependency error",
			firstCall: func(context.Context) error { return errors.New("database unavailable") },
		},
		{
			name: "caller cancellation",
			firstCall: func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			},
			want: context.Canceled,
		},
		{
			name: "authorization timeout",
			firstCall: func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			},
			want: context.DeadlineExceeded,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			authorizer := eventAuthorizerFunc(func(ctx context.Context, _, _ string) error {
				mu.Lock()
				calls++
				call := calls
				mu.Unlock()
				if call == 1 {
					return test.firstCall(ctx)
				}
				return nil
			})
			handler, err := NewEventHandler(authorizer, &eventRepositoryStub{}, &replayWaiterStub{})
			if err != nil {
				t.Fatal(err)
			}
			handler.authorizationTimeout = 20 * time.Millisecond

			ctx := context.Background()
			cancel := func() {}
			if test.name == "caller cancellation" {
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			defer cancel()
			firstErr := handler.authorizeInitial(ctx, "7", "project-1", "execution-1")
			if test.want != nil && !errors.Is(firstErr, test.want) {
				t.Fatalf("first authorization error=%v, want=%v", firstErr, test.want)
			}
			if firstErr == nil {
				t.Fatal("first authorization unexpectedly succeeded")
			}
			if err := handler.authorizeInitial(
				context.Background(),
				"7",
				"project-1",
				"execution-1",
			); err != nil {
				t.Fatalf("released capacity was not reusable: %v", err)
			}
		})
	}
}

func TestEventHandlerForbiddenRequestNeverConsumesStreamLifetimeCapacity(t *testing.T) {
	authorizer := &eventAuthorizerStub{err: ErrExecutionEventsForbidden}
	handler, err := NewEventHandler(authorizer, &eventRepositoryStub{}, &replayWaiterStub{})
	if err != nil {
		t.Fatal(err)
	}
	handler.admission = newSSEAdmissionGate(1, 1, 1)

	forbidden := newStreamingRecorder()
	handler.Stream(forbidden, executionEventsRequest())
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("forbidden status=%d", forbidden.Code)
	}
	authorizer.err = nil
	allowed := newStreamingRecorder()
	handler.Stream(allowed, executionEventsRequest())
	if allowed.Code != http.StatusOK {
		t.Fatalf("stream capacity was retained after denial: status=%d", allowed.Code)
	}
}

func TestEventHandlerReauthorizesAfterWakeAndWritesNothingAfterRevocation(t *testing.T) {
	authorizer := &revokingEventAuthorizer{}
	repository := &eventRepositoryStub{events: map[uint64][]DurableEvent{
		0: nil,
	}}
	handler, err := NewEventHandler(authorizer, repository, oneWakeWaiter{})
	if err != nil {
		t.Fatal(err)
	}
	response := newStreamingRecorder()
	handler.Stream(response, executionEventsRequest())
	if authorizer.calls != 2 {
		t.Fatalf("authorization calls = %d, want initial plus post-wake", authorizer.calls)
	}
	if len(repository.cursors) != 1 {
		t.Fatalf("revoked stream replayed after wake: cursors=%v", repository.cursors)
	}
	if response.Body.String() != ": connected\n\n" {
		t.Fatalf("revoked stream wrote data after wake: %q", response.Body.String())
	}
}

func TestEventHandlerRevocationExposureStopsAtOneAuthorizedReplayBatch(t *testing.T) {
	events := replayProgressEvents(1, defaultReplayBatchSize)
	authorizer := &revokingEventAuthorizer{}
	repository := &eventRepositoryStub{events: map[uint64][]DurableEvent{
		0:   events,
		100: {{Cursor: 101, Type: "index.ingest.completed", Data: json.RawMessage(`{"status":"ok"}`)}},
	}}
	waiter := &replayWaiterStub{}
	handler, err := NewEventHandler(authorizer, repository, waiter)
	if err != nil {
		t.Fatal(err)
	}

	response := newStreamingRecorder()
	handler.Stream(response, executionEventsRequest())
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if handler.batchSize != 100 || handler.writeTimeout != 10*time.Second {
		t.Fatalf(
			"revocation exposure profile changed: batch=%d write_timeout=%s",
			handler.batchSize,
			handler.writeTimeout,
		)
	}
	if authorizer.calls != 2 {
		t.Fatalf("authorization calls=%d, want initial plus batch continuation", authorizer.calls)
	}
	if !reflect.DeepEqual(repository.cursors, []uint64{0}) {
		t.Fatalf("revoked or suspended stream fetched next backlog batch: cursors=%v", repository.cursors)
	}
	if strings.Contains(response.Body.String(), "id: 101\n") {
		t.Fatal("revoked or suspended stream exposed post-revocation backlog")
	}
	if got := strings.Count(response.Body.String(), "\nid: "); got != defaultReplayBatchSize-1 {
		// The first event begins at byte zero, so only the remaining 99 IDs have
		// a leading newline.
		t.Fatalf("authorized replay event count=%d, want=%d", got+1, defaultReplayBatchSize)
	}
	if len(waiter.calls) != 0 {
		t.Fatalf("full backlog unexpectedly entered waiter: calls=%v", waiter.calls)
	}
}

func TestEventHandlerReauthorizesEachBatchAcrossLargeBacklog(t *testing.T) {
	authorizer := &eventAuthorizerStub{}
	repository := &eventRepositoryStub{events: map[uint64][]DurableEvent{
		0:   replayProgressEvents(1, defaultReplayBatchSize),
		100: replayProgressEvents(101, defaultReplayBatchSize),
		200: {{
			Cursor: 201,
			Type:   "index.ingest.completed",
			Data:   json.RawMessage(`{"status":"ok"}`),
		}},
	}}
	waiter := &replayWaiterStub{}
	handler, err := NewEventHandler(authorizer, repository, waiter)
	if err != nil {
		t.Fatal(err)
	}

	response := newStreamingRecorder()
	handler.Stream(response, executionEventsRequest())
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "id: 201\n") {
		t.Fatalf("large replay was incomplete: status=%d", response.Code)
	}
	if authorizer.calls != 3 {
		t.Fatalf("authorization calls=%d, want initial plus two continuations", authorizer.calls)
	}
	if !reflect.DeepEqual(repository.cursors, []uint64{0, 100, 200}) {
		t.Fatalf("large replay cursors=%v", repository.cursors)
	}
}

func replayProgressEvents(firstCursor, count int) []DurableEvent {
	events := make([]DurableEvent, count)
	for index := range events {
		events[index] = DurableEvent{
			Cursor: uint64(firstCursor + index),
			Type:   "index.ingest.progress",
			Data:   json.RawMessage(`{"status":"running"}`),
		}
	}
	return events
}

func TestSSEAuthorizationGateIsNonBlockingAndBounded(t *testing.T) {
	gate := newSSEAuthorizationGate(2, 1)
	if gate == nil {
		t.Fatal("valid authorization gate profile was rejected")
	}
	releaseFirst, ok := gate.acquire("principal-1")
	if !ok {
		t.Fatal("first authorization was rejected")
	}
	if _, ok := gate.acquire("principal-1"); ok {
		t.Fatal("per-principal authorization limit was bypassed")
	}
	releaseSecond, ok := gate.acquire("principal-2")
	if !ok {
		t.Fatal("second authorization was rejected")
	}
	if _, ok := gate.acquire("principal-3"); ok {
		t.Fatal("global authorization limit was bypassed")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := gate.acquireContext(cancelled, "principal-3"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled authorization wait error=%v, want=%v", err, context.Canceled)
	}

	replacement := make(chan func(), 1)
	waitErr := make(chan error, 1)
	go func() {
		release, err := gate.acquireContext(context.Background(), "principal-3")
		if err != nil {
			waitErr <- err
			return
		}
		replacement <- release
	}()
	releaseFirst()
	releaseFirst()
	select {
	case releaseReplacement := <-replacement:
		releaseReplacement()
	case err := <-waitErr:
		t.Fatalf("released authorization capacity returned error: %v", err)
	case <-time.After(time.Second):
		t.Fatal("released authorization capacity did not wake bounded waiter")
	}
	releaseSecond()
}

func TestEventHandlerAuthorizationCapacityFollowsReplayPoolCapacity(t *testing.T) {
	handler, err := NewEventHandlerWithReplayCapacity(
		&eventAuthorizerStub{},
		&eventRepositoryStub{},
		&replayWaiterStub{},
		2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if handler.authorizationAdmission.globalLimit != 2 ||
		handler.authorizationAdmission.principalLimit != 1 {
		t.Fatalf(
			"authorization profile does not follow replay capacity: %+v",
			handler.authorizationAdmission,
		)
	}
	if handler.admission.globalLimit != 16 ||
		handler.admission.principalLimit != 4 ||
		handler.admission.projectLimit != 8 {
		t.Fatalf("unexpected conservative stream lifetime profile: %+v", handler.admission)
	}
	if _, err := NewEventHandlerWithReplayCapacity(
		&eventAuthorizerStub{},
		&eventRepositoryStub{},
		&replayWaiterStub{},
		0,
	); err == nil {
		t.Fatal("zero replay capacity was accepted")
	}
}

func TestSSEAdmissionGateBoundsGlobalPrincipalAndProjectStreams(t *testing.T) {
	gate := newSSEAdmissionGate(2, 1, 1)
	if gate == nil {
		t.Fatal("valid gate profile was rejected")
	}
	releaseFirst, ok := gate.acquire("principal-1", "project-1")
	if !ok {
		t.Fatal("first stream was rejected")
	}
	if _, ok := gate.acquire("principal-1", "project-2"); ok {
		t.Fatal("principal stream limit was bypassed")
	}
	if _, ok := gate.acquire("principal-2", "project-1"); ok {
		t.Fatal("project stream limit was bypassed")
	}
	releaseSecond, ok := gate.acquire("principal-2", "project-2")
	if !ok {
		t.Fatal("independent second stream was rejected")
	}
	if _, ok := gate.acquire("principal-3", "project-3"); ok {
		t.Fatal("global stream limit was bypassed")
	}

	releaseFirst()
	releaseFirst()
	if releaseReplacement, ok := gate.acquire("principal-3", "project-1"); !ok {
		t.Fatal("released stream capacity was not reusable")
	} else {
		releaseReplacement()
	}
	releaseSecond()
}

func TestValidateDurableEventRejectsInjectionAndOversize(t *testing.T) {
	tests := []DurableEvent{
		{Cursor: 1, Type: "bad\nevent", Data: json.RawMessage(`{}`)},
		{Cursor: 1, Type: "valid", Data: json.RawMessage(`not-json`)},
		{Cursor: 1, Type: "valid", Data: json.RawMessage(`"` + strings.Repeat("x", maxSSEEventBytes) + `"`)},
	}
	for _, event := range tests {
		if !errors.Is(validateDurableEvent(event), ErrInvalidEventStream) {
			t.Fatalf("unsafe event accepted: %+v", event)
		}
	}
}

func TestWriteDurableEventPreservesCompactJSONAndCompactsFallback(t *testing.T) {
	tests := []struct {
		name string
		data json.RawMessage
		want string
	}{
		{
			name: "compact object",
			data: json.RawMessage(`{"message":"space and escaped\nline","nested":{"ok":true}}`),
			want: `{"message":"space and escaped\nline","nested":{"ok":true}}`,
		},
		{
			name: "insignificant spaces",
			data: json.RawMessage(` { "message": "space in value", "ok": true } `),
			want: `{"message":"space in value","ok":true}`,
		},
		{
			name: "multiline",
			data: json.RawMessage("{\n\t\"ok\": true\n}"),
			want: `{"ok":true}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			err := writeDurableEvent(response, DurableEvent{
				Cursor: 7,
				Type:   "index.ingest.progress",
				Data:   test.data,
			})
			if err != nil {
				t.Fatal(err)
			}
			want := "id: 7\nevent: index.ingest.progress\ndata: " + test.want + "\n\n"
			if response.Body.String() != want {
				t.Fatalf("SSE event = %q, want %q", response.Body.String(), want)
			}
		})
	}
}

func TestWriteDurableEventRejectsInvalidJSONBeforeWriting(t *testing.T) {
	for _, data := range []json.RawMessage{
		json.RawMessage(`{"missing":"brace"`),
		json.RawMessage("{\n\"missing\":\"brace\""),
	} {
		response := httptest.NewRecorder()
		if err := writeDurableEvent(response, DurableEvent{
			Cursor: 7,
			Type:   "index.ingest.progress",
			Data:   data,
		}); err == nil {
			t.Fatalf("invalid JSON was accepted: %q", data)
		}
		if response.Body.Len() != 0 {
			t.Fatalf("invalid JSON wrote partial SSE data: %q", response.Body.String())
		}
	}
}

type discardEventResponseWriter struct{}

func (discardEventResponseWriter) Header() http.Header            { return nil }
func (discardEventResponseWriter) WriteHeader(int)                {}
func (discardEventResponseWriter) Write(data []byte) (int, error) { return len(data), nil }

func BenchmarkWriteDurableEvent(b *testing.B) {
	events := map[string]DurableEvent{
		"compact": {
			Cursor: 7,
			Type:   "index.ingest.progress",
			Data:   json.RawMessage(`{"type":"agent_thinking_step","message":"20 files processed","metadata":{"tool_name":"index_data"}}`),
		},
		"whitespace_fallback": {
			Cursor: 7,
			Type:   "index.ingest.progress",
			Data:   json.RawMessage(` { "type": "agent_thinking_step", "message": "20 files processed", "metadata": { "tool_name": "index_data" } } `),
		},
	}
	writer := discardEventResponseWriter{}
	for name, event := range events {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if err := writeDurableEvent(writer, event); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSSEGatesAcquireRelease(b *testing.B) {
	b.Run("authorization", func(b *testing.B) {
		gate := newSSEAuthorizationGate(4, 1)
		b.ReportAllocs()
		for range b.N {
			release, ok := gate.acquire("principal-1")
			if !ok {
				b.Fatal("authorization gate rejected uncontended acquisition")
			}
			release()
		}
	})
	b.Run("stream_lifetime", func(b *testing.B) {
		gate := newSSEAdmissionGate(16, 4, 8)
		b.ReportAllocs()
		for range b.N {
			release, ok := gate.acquire("principal-1", "project-1")
			if !ok {
				b.Fatal("stream gate rejected uncontended acquisition")
			}
			release()
		}
	})
}

func executionEventsRequest() *http.Request {
	return executionEventsRequestForExecution("execution-1")
}

func executionEventsRequestForExecution(executionID string) *http.Request {
	return executionEventsRequestForPrincipalAndExecution(
		auth.User{ID: "7", UserID: "7", AuthType: "user"},
		auth.AuthenticationSourceSession,
		executionID,
	)
}

func executionEventsRequestForPrincipal(
	principal auth.User,
	source auth.AuthenticationSource,
) *http.Request {
	return executionEventsRequestForPrincipalAndExecution(
		principal,
		source,
		"execution-1",
	)
}

func executionEventsRequestForPrincipalAndExecution(
	principal auth.User,
	source auth.AuthenticationSource,
	executionID string,
) *http.Request {
	request := executionEventsRequestWithoutPrincipalForExecution(executionID)
	return request.WithContext(auth.ContextWithAuthenticatedUser(
		request.Context(),
		principal,
		source,
	))
}

func executionEventsRequestWithoutPrincipal() *http.Request {
	return executionEventsRequestWithoutPrincipalForExecution("execution-1")
}

func executionEventsRequestWithoutPrincipalForExecution(
	executionID string,
) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", "project-1")
	routeContext.URLParams.Add("executionID", executionID)
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
}

func executionEventsRequestForPrincipalAndProject(
	principalID,
	projectID,
	executionID string,
) *http.Request {
	request := executionEventsRequestWithoutPrincipalForExecution(executionID)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", projectID)
	routeContext.URLParams.Add("executionID", executionID)
	request = request.WithContext(
		context.WithValue(request.Context(), chi.RouteCtxKey, routeContext),
	)
	principal := auth.User{ID: principalID, UserID: principalID, AuthType: "user"}
	return request.WithContext(auth.ContextWithAuthenticatedUser(
		request.Context(),
		principal,
		auth.AuthenticationSourceSession,
	))
}

// heldReplayWaiter parks each stream in its first Wait call. Closing the
// per-execution channel ends that stream and releases its admission slot.
type heldReplayWaiter struct {
	parked chan string
	holds  map[string]chan struct{}
}

func (w heldReplayWaiter) Wait(ctx context.Context, _, executionID string, _ uint64) (bool, error) {
	select {
	case w.parked <- executionID:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	select {
	case <-w.holds[executionID]:
		return false, context.Canceled
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func TestNewEventHandlerWithStreamLimitsBoundsGlobalStreams(t *testing.T) {
	limits := SSEStreamLimits{MaxStreams: 2, MaxPerPrincipal: 2, MaxPerProject: 2}
	holdFirst := make(chan struct{})
	holdSecond := make(chan struct{})
	holdThird := make(chan struct{})
	waiter := &heldReplayWaiter{
		parked: make(chan string, 3),
		holds: map[string]chan struct{}{
			"execution-1": holdFirst,
			"execution-2": holdSecond,
			"execution-3": holdThird,
		},
	}
	authorizer := eventAuthorizerFunc(
		func(context.Context, string, string) error { return nil },
	)
	repository := concurrentEmptyEventRepository{}
	handler, err := NewEventHandlerWithStreamLimits(
		authorizer,
		repository,
		waiter,
		64,
		limits,
	)
	if err != nil {
		t.Fatal(err)
	}

	streams := [2]struct {
		principal string
		project   string
		execution string
	}{
		{"1", "project-1", "execution-1"},
		{"2", "project-2", "execution-2"},
	}
	dones := make([]chan struct{}, 2)
	for index := range streams {
		dones[index] = make(chan struct{})
		go func(index int) {
			defer close(dones[index])
			handler.Stream(
				newStreamingRecorder(),
				executionEventsRequestForPrincipalAndProject(
					streams[index].principal,
					streams[index].project,
					streams[index].execution,
				),
			)
		}(index)
	}
	for range 2 {
		select {
		case <-waiter.parked:
		case <-time.After(2 * time.Second):
			t.Fatal("stream did not park in the replay waiter")
		}
	}

	rejected := newStreamingRecorder()
	handler.Stream(rejected, executionEventsRequestForPrincipalAndProject("3", "project-3", "execution-3"))
	if rejected.Code != http.StatusTooManyRequests {
		t.Fatalf("global stream limit was bypassed: status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	if rejected.Body.String() != "too many active event streams\n" {
		t.Fatalf("429 body changed: %q", rejected.Body.String())
	}
	if rejected.Header().Get("Retry-After") != "2" {
		t.Fatalf("429 Retry-After = %q, want \"2\"", rejected.Header().Get("Retry-After"))
	}

	close(holdFirst)
	select {
	case <-dones[0]:
	case <-time.After(2 * time.Second):
		t.Fatal("released stream did not end")
	}

	reused := newStreamingRecorder()
	doneThird := make(chan struct{})
	go func() {
		defer close(doneThird)
		handler.Stream(reused, executionEventsRequestForPrincipalAndProject("3", "project-3", "execution-3"))
	}()
	select {
	case <-waiter.parked:
	case <-time.After(2 * time.Second):
		t.Fatalf("released stream capacity was not reusable: status=%d body=%s", reused.Code, reused.Body.String())
	}
	if reused.Code != http.StatusOK {
		t.Fatalf("released stream capacity was not reusable: status=%d", reused.Code)
	}
	if !strings.Contains(reused.Body.String(), ": connected\n\n") {
		t.Fatalf("reused stream did not open: %q", reused.Body.String())
	}

	close(holdSecond)
	close(holdThird)
	select {
	case <-dones[1]:
	case <-time.After(2 * time.Second):
		t.Fatal("second stream did not end")
	}
	select {
	case <-doneThird:
	case <-time.After(2 * time.Second):
		t.Fatal("reused stream did not end")
	}
}

func TestNewEventHandlerWithStreamLimitsBoundsPerPrincipalStreams(t *testing.T) {
	limits := SSEStreamLimits{MaxStreams: 4, MaxPerPrincipal: 1, MaxPerProject: 4}
	holdFirst := make(chan struct{})
	waiter := &heldReplayWaiter{
		parked: make(chan string, 2),
		holds: map[string]chan struct{}{
			"execution-1": holdFirst,
		},
	}
	authorizer := eventAuthorizerFunc(
		func(context.Context, string, string) error { return nil },
	)
	repository := concurrentEmptyEventRepository{}
	handler, err := NewEventHandlerWithStreamLimits(
		authorizer,
		repository,
		waiter,
		64,
		limits,
	)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.Stream(newStreamingRecorder(), executionEventsRequestForPrincipalAndProject("1", "project-1", "execution-1"))
	}()
	select {
	case <-waiter.parked:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not park in the replay waiter")
	}

	rejected := newStreamingRecorder()
	handler.Stream(rejected, executionEventsRequestForPrincipalAndProject("1", "project-2", "execution-2"))
	if rejected.Code != http.StatusTooManyRequests {
		t.Fatalf("per-principal limit was bypassed: status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	if rejected.Body.String() != "too many active event streams\n" {
		t.Fatalf("429 body changed: %q", rejected.Body.String())
	}

	close(holdFirst)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not end")
	}
}

func TestNewEventHandlerWithStreamLimitsBoundsPerProjectStreams(t *testing.T) {
	limits := SSEStreamLimits{MaxStreams: 4, MaxPerPrincipal: 4, MaxPerProject: 1}
	holdFirst := make(chan struct{})
	waiter := &heldReplayWaiter{
		parked: make(chan string, 2),
		holds: map[string]chan struct{}{
			"execution-1": holdFirst,
		},
	}
	authorizer := eventAuthorizerFunc(
		func(context.Context, string, string) error { return nil },
	)
	repository := concurrentEmptyEventRepository{}
	handler, err := NewEventHandlerWithStreamLimits(
		authorizer,
		repository,
		waiter,
		64,
		limits,
	)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.Stream(newStreamingRecorder(), executionEventsRequestForPrincipalAndProject("1", "project-1", "execution-1"))
	}()
	select {
	case <-waiter.parked:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not park in the replay waiter")
	}

	rejected := newStreamingRecorder()
	handler.Stream(rejected, executionEventsRequestForPrincipalAndProject("2", "project-1", "execution-2"))
	if rejected.Code != http.StatusTooManyRequests {
		t.Fatalf("per-project limit was bypassed: status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	if rejected.Body.String() != "too many active event streams\n" {
		t.Fatalf("429 body changed: %q", rejected.Body.String())
	}

	close(holdFirst)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not end")
	}
}

func TestNewEventHandlerWithStreamLimitsDefaultsFromTheZeroValue(t *testing.T) {
	handler, err := NewEventHandlerWithStreamLimits(
		&eventAuthorizerStub{},
		&eventRepositoryStub{},
		&replayWaiterStub{},
		64,
		SSEStreamLimits{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if handler.admission.globalLimit != 16 ||
		handler.admission.principalLimit != 4 ||
		handler.admission.projectLimit != 8 {
		t.Fatalf("zero value did not keep the built-in profile: %+v", handler.admission)
	}
}
