package toolkitcalltool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"google.golang.org/protobuf/proto"
)

type stubResolver struct {
	inputs AuthoritativeInputs
	err    error
	calls  int
}

func (s *stubResolver) Resolve(context.Context, RunRequest) (AuthoritativeInputs, error) {
	s.calls++
	return s.inputs, s.err
}

type stubVerdict struct {
	supported bool
	reason    string
}

func (s stubVerdict) SupportsToolkitType(string) (bool, string) { return s.supported, s.reason }

type stubAdmissions struct {
	run      AdmittedRun
	err      error
	calls    int
	captured SubmitRequest
}

func (s *stubAdmissions) Submit(_ context.Context, request SubmitRequest) (AdmittedRun, error) {
	s.calls++
	s.captured = request
	return s.run, s.err
}

type stubDispatcher struct {
	err      error
	calls    int
	captured Dispatch
}

func (s *stubDispatcher) Dispatch(_ context.Context, dispatch Dispatch) error {
	s.calls++
	s.captured = dispatch
	return s.err
}

type stubSettlements struct {
	settlement Settlement
	found      bool
	err        error
	calls      int
}

func (s *stubSettlements) ReadToolkitCallToolSettlement(
	context.Context, string, uint64,
) (Settlement, bool, error) {
	s.calls++
	return s.settlement, s.found, s.err
}

func testInputs() AuthoritativeInputs {
	return AuthoritativeInputs{
		ToolkitType: "github",
		ToolkitID:   19,
		ToolName:    "list_issues",
		Settings:    json.RawMessage(`{"id":19,"type":"github","toolkit_name":"gh","settings":{}}`),
		Arguments:   json.RawMessage(`{}`),
	}
}

func testAdmitted() AdmittedRun {
	bundle, binding := mustBuildBundle()
	return AdmittedRun{
		Outcome: executionapp.AdmissionOutcome{
			ExecutionID: "exec-1",
			CommandID:   "cmd-1",
			Created:     true,
			AdmittedAt:  time.Unix(1, 0).UTC(),
			Deadline:    time.Unix(3600, 0).UTC(),
		},
		Binding:     binding,
		OutboxID:    "outbox-1",
		InputBundle: bundle,
	}
}

func mustBuildBundle() (executiondomain.InputBundle, executiondomain.ToolkitCallToolBinding) {
	counter := 0
	factory, err := NewInputBundleFactory(
		InputProfile{Classification: "tenant-confidential", RequiredGrantAudience: "aud"},
		func() (string, error) {
			counter++
			return "id-" + string(rune('a'+counter)), nil
		},
	)
	if err != nil {
		panic(err)
	}
	bundle, binding, err := factory.Build(context.Background(), testInputs())
	if err != nil {
		panic(err)
	}
	return bundle, binding
}

func newTestService(
	t *testing.T,
	resolver AuthoritativeInputResolver,
	verdict ToolkitTypeVerdict,
	admissions admissionSubmitter,
	dispatcher runDispatcher,
	settlements SettlementReader,
	deadline time.Duration,
) *RunService {
	t.Helper()
	service, err := NewRunService(
		resolver, verdict, admissions, dispatcher, settlements,
		DispatchPolicy{
			CapabilityVersion: "1",
			ResourceClass:     "indexing",
			IsolationClass:    "project",
			Priority:          1,
			LimitsRevision:    "limits-v1",
		},
		func() (string, error) { return "generated", nil },
		deadline,
	)
	if err != nil {
		t.Fatalf("compose run service: %v", err)
	}
	return service
}

func settledPayload(t *testing.T, status runtimev1.ToolkitCallToolStatusV1, body, message string) Settlement {
	t.Helper()
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(&runtimev1.ToolkitCallToolResultV1{
		ToolkitType: "github",
		ToolName:    "list_issues",
		ResultSummary: &runtimev1.ToolkitCallToolSummaryV1{
			Status:       status,
			ResultJson:   body,
			ErrorMessage: message,
		},
	})
	if err != nil {
		t.Fatalf("encode tool-run result: %v", err)
	}
	outcome := executionapp.SettlementSucceeded
	switch status {
	case runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_UNSUPPORTED_TOOLKIT,
		runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_UNKNOWN_TOOL:
		outcome = executionapp.SettlementFailed
	}
	return Settlement{
		Outcome:     outcome,
		PayloadType: PayloadTypeToolkitCallToolResult,
		Payload:     encoded,
	}
}

func validRequest() RunRequest {
	return RunRequest{
		ProjectID:   1,
		ActorUserID: 7,
		ToolkitID:   19,
		ToolName:    "list_issues",
		Arguments:   json.RawMessage(`{"repo":"a"}`),
	}
}

// A request this boundary cannot read never reaches the resolver, so an
// admission is never attempted for one.
func TestRunToolRefusesAnInvalidRequestBeforeResolving(t *testing.T) {
	resolver := &stubResolver{inputs: testInputs()}
	admissions := &stubAdmissions{run: testAdmitted()}
	service := newTestService(t, resolver, stubVerdict{supported: true}, admissions,
		&stubDispatcher{}, &stubSettlements{}, time.Second)

	for name, request := range map[string]RunRequest{
		"no project": {ActorUserID: 7, ToolkitID: 19, ToolName: "x"},
		"no actor":   {ProjectID: 1, ToolkitID: 19, ToolName: "x"},
		"no toolkit": {ProjectID: 1, ActorUserID: 7, ToolName: "x"},
		"no tool":    {ProjectID: 1, ActorUserID: 7, ToolkitID: 19},
		"arguments are not an object": {
			ProjectID: 1, ActorUserID: 7, ToolkitID: 19, ToolName: "x",
			Arguments: json.RawMessage(`["a"]`),
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := service.RunTool(context.Background(), request); !errors.Is(err, ErrInvalidToolRun) {
				t.Fatalf("expected ErrInvalidToolRun, got %v", err)
			}
		})
	}
	if resolver.calls != 0 || admissions.calls != 0 {
		t.Fatalf("an unreadable request reached the resolver or the admission store: %d/%d",
			resolver.calls, admissions.calls)
	}
}

// CROSS-PROJECT REFUSAL. The resolver is the only place visibility is decided,
// and a toolkit it cannot see must stop the run before anything durable is
// written.
func TestRunToolRefusesAToolkitTheResolverCannotSee(t *testing.T) {
	resolver := &stubResolver{err: ErrToolkitNotVisible}
	admissions := &stubAdmissions{run: testAdmitted()}
	dispatcher := &stubDispatcher{}
	service := newTestService(t, resolver, stubVerdict{supported: true}, admissions,
		dispatcher, &stubSettlements{}, time.Second)

	_, err := service.RunTool(context.Background(), validRequest())
	if !errors.Is(err, ErrToolkitNotVisible) {
		t.Fatalf("expected ErrToolkitNotVisible, got %v", err)
	}
	if admissions.calls != 0 || dispatcher.calls != 0 {
		t.Fatalf("an invisible toolkit was admitted or dispatched: %d/%d",
			admissions.calls, dispatcher.calls)
	}
}

// The unrunnable-type refusal happens BEFORE admission. Admitting it would burn
// an admission slot and a bounded wait to reach a conclusion already known.
func TestRunToolRefusesAnUnrunnableTypeBeforeAdmission(t *testing.T) {
	resolver := &stubResolver{inputs: testInputs()}
	admissions := &stubAdmissions{run: testAdmitted()}
	dispatcher := &stubDispatcher{}
	service := newTestService(t, resolver,
		stubVerdict{supported: false, reason: "the admitted image does not carry github"},
		admissions, dispatcher, &stubSettlements{}, time.Second)

	_, err := service.RunTool(context.Background(), validRequest())
	if !errors.Is(err, ErrUnsupportedToolkitType) {
		t.Fatalf("expected ErrUnsupportedToolkitType, got %v", err)
	}
	if admissions.calls != 0 {
		t.Fatal("an unrunnable toolkit type was admitted")
	}
	if dispatcher.calls != 0 {
		t.Fatal("an unrunnable toolkit type was dispatched")
	}
	// The refusal NAMES the type and the reason. "Unsupported" alone leaves an
	// operator hunting a type the catalogue also hides.
	for _, want := range []string{"github", "does not carry github"} {
		if !contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q", err.Error(), want)
		}
	}
}

// The dispatched command must reference the SAME bundle admission wrote, and
// the two entries by their distinct roles.
func TestRunToolDispatchesTheAdmittedBundleReferences(t *testing.T) {
	admitted := testAdmitted()
	resolver := &stubResolver{inputs: testInputs()}
	admissions := &stubAdmissions{run: admitted}
	dispatcher := &stubDispatcher{}
	settlements := &stubSettlements{
		settlement: settledPayload(t, runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_OK, `{"a":1}`, ""),
		found:      true,
	}
	service := newTestService(t, resolver, stubVerdict{supported: true}, admissions,
		dispatcher, settlements, time.Second)

	if _, err := service.RunTool(context.Background(), validRequest()); err != nil {
		t.Fatalf("run tool: %v", err)
	}
	dispatch := dispatcher.captured
	if err := dispatch.Validate(); err != nil {
		t.Fatalf("dispatched an invalid command: %v", err)
	}
	if dispatch.InputBundleID != admitted.InputBundle.ID ||
		dispatch.InputBundleVersion != admitted.InputBundle.Version ||
		dispatch.InputBundleDigest != admitted.InputBundle.Digest ||
		dispatch.InputBundleByteLength != uint64(len(admitted.InputBundle.Manifest)) {
		t.Fatalf("dispatch does not reference the admitted bundle: %+v", dispatch)
	}
	if dispatch.SettingsEntryID != SettingsEntryID || dispatch.ArgumentsEntryID != ArgumentsEntryID {
		t.Fatalf("dispatch entry references are wrong: %q/%q",
			dispatch.SettingsEntryID, dispatch.ArgumentsEntryID)
	}
	// BOTH entry references must resolve inside the admitted bundle, under the
	// role each one claims. This is the check that would fail if one entry ever
	// served as both.
	for _, reference := range []struct{ id, role string }{
		{dispatch.SettingsEntryID, executiondomain.ToolkitCallToolSettingsRole},
		{dispatch.ArgumentsEntryID, executiondomain.ToolkitCallToolArgumentsRole},
	} {
		found := false
		for _, entry := range admitted.InputBundle.Entries {
			if entry.ID == reference.id {
				found = true
				if entry.SemanticRole != reference.role {
					t.Fatalf("entry %q carries role %q, not %q", entry.ID, entry.SemanticRole, reference.role)
				}
			}
		}
		if !found {
			t.Fatalf("dispatch names entry %q, which the admitted bundle does not carry", reference.id)
		}
	}
	if dispatch.ToolkitType != "github" || dispatch.ToolName != "list_issues" || dispatch.ToolkitID != "19" {
		t.Fatalf("dispatch identity is wrong: %+v", dispatch)
	}
	if dispatch.CapabilityID != executiondomain.ToolkitCallToolCapability {
		t.Fatalf("dispatch capability is %q", dispatch.CapabilityID)
	}
}

// A REPLAYED admission is not dispatched again. Created=false means the command
// is already on the stream, and appending a second one would run the tool twice.
func TestRunToolDoesNotDispatchAReplayedAdmission(t *testing.T) {
	admitted := testAdmitted()
	admitted.Outcome.Created = false
	dispatcher := &stubDispatcher{}
	settlements := &stubSettlements{
		settlement: settledPayload(t, runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_OK, `{}`, ""),
		found:      true,
	}
	service := newTestService(t, &stubResolver{inputs: testInputs()}, stubVerdict{supported: true},
		&stubAdmissions{run: admitted}, dispatcher, settlements, time.Second)

	if _, err := service.RunTool(context.Background(), validRequest()); err != nil {
		t.Fatalf("run tool: %v", err)
	}
	if dispatcher.calls != 0 {
		t.Fatalf("a replayed admission was dispatched %d times", dispatcher.calls)
	}
}

func TestRunToolMapsEverySettlementOutcome(t *testing.T) {
	cases := []struct {
		name       string
		status     runtimev1.ToolkitCallToolStatusV1
		body       string
		message    string
		wantStatus RunStatus
	}{
		{"ok", runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_OK, `{"issues":[]}`, "", RunStatusOK},
		{"tool error", runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_TOOL_ERROR, "", "401 from the provider", RunStatusToolError},
		{"unsupported toolkit", runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_UNSUPPORTED_TOOLKIT, "", "no dependencies", RunStatusUnsupportedToolkit},
		{"unknown tool", runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_UNKNOWN_TOOL, "", "no such tool", RunStatusUnknownTool},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			settlements := &stubSettlements{
				settlement: settledPayload(t, testCase.status, testCase.body, testCase.message),
				found:      true,
			}
			service := newTestService(t, &stubResolver{inputs: testInputs()}, stubVerdict{supported: true},
				&stubAdmissions{run: testAdmitted()}, &stubDispatcher{}, settlements, time.Second)

			outcome, err := service.RunTool(context.Background(), validRequest())
			if err != nil {
				t.Fatalf("run tool: %v", err)
			}
			if outcome.Status != testCase.wantStatus {
				t.Fatalf("status %q, want %q", outcome.Status, testCase.wantStatus)
			}
			if outcome.ExecutionID != "exec-1" {
				t.Fatalf("execution id %q", outcome.ExecutionID)
			}
			if outcome.ResultJSON != testCase.body || outcome.ErrorMessage != testCase.message {
				t.Fatalf("body/message %q/%q", outcome.ResultJSON, outcome.ErrorMessage)
			}
		})
	}
}

// A RUNTIME failure — a lost claim, a passed deadline — is not a tool result at
// all. It arrives on the other payload type and must not be reported as one of
// the four tool statuses.
func TestRunToolMapsARuntimeFailure(t *testing.T) {
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(&runtimev1.RuntimeErrorV1{
		Code:        runtimev1.RuntimeErrorCodeV1_RUNTIME_ERROR_CODE_V1_INTERNAL,
		SafeMessage: "The runtime operation failed.",
	})
	if err != nil {
		t.Fatal(err)
	}
	settlements := &stubSettlements{
		settlement: Settlement{
			Outcome:     executionapp.SettlementFailed,
			PayloadType: PayloadTypeRuntimeFailure,
			Payload:     encoded,
		},
		found: true,
	}
	service := newTestService(t, &stubResolver{inputs: testInputs()}, stubVerdict{supported: true},
		&stubAdmissions{run: testAdmitted()}, &stubDispatcher{}, settlements, time.Second)

	outcome, err := service.RunTool(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("run tool: %v", err)
	}
	if outcome.Status != RunStatusRuntimeFailure ||
		outcome.ErrorMessage != "The runtime operation failed." {
		t.Fatalf("runtime failure mapped to %+v", outcome)
	}
}

// The bounded wait expiring is NOT a failure of the run: the execution is
// durable and still going, and the caller is told its id so it can be found.
func TestRunToolReportsThePendingExecutionWhenTheWaitExpires(t *testing.T) {
	settlements := &stubSettlements{found: false}
	service := newTestService(t, &stubResolver{inputs: testInputs()}, stubVerdict{supported: true},
		&stubAdmissions{run: testAdmitted()}, &stubDispatcher{}, settlements, 120*time.Millisecond)

	start := time.Now()
	_, err := service.RunTool(context.Background(), validRequest())
	elapsed := time.Since(start)
	var pending *PendingRun
	if !errors.As(err, &pending) {
		t.Fatalf("expected a PendingRun, got %v", err)
	}
	if !errors.Is(err, ErrToolRunDeadlineExceeded) {
		t.Fatalf("a PendingRun must unwrap to ErrToolRunDeadlineExceeded, got %v", err)
	}
	if pending.ExecutionID != "exec-1" {
		t.Fatalf("the expiry does not name the execution: %+v", pending)
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("the wait returned after %s, which is shorter than its own bound", elapsed)
	}
	if settlements.calls == 0 {
		t.Fatal("the wait never polled")
	}
}

// #616's idempotency criterion. The key covers the arguments, so two identical
// calls share an admission and a changed argument does not.
func TestIdempotencyKeyCoversTheArguments(t *testing.T) {
	admissions := &stubAdmissions{run: testAdmitted()}
	settlements := &stubSettlements{
		settlement: settledPayload(t, runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_OK, `{}`, ""),
		found:      true,
	}
	service := newTestService(t, &stubResolver{inputs: testInputs()}, stubVerdict{supported: true},
		admissions, &stubDispatcher{}, settlements, time.Second)

	if _, err := service.RunTool(context.Background(), validRequest()); err != nil {
		t.Fatal(err)
	}
	first := admissions.captured.IdempotencyKey
	if _, err := service.RunTool(context.Background(), validRequest()); err != nil {
		t.Fatal(err)
	}
	if admissions.captured.IdempotencyKey != first {
		t.Fatal("two identical calls derived different idempotency keys")
	}
	changed := validRequest()
	changed.Arguments = json.RawMessage(`{"repo":"b"}`)
	if _, err := service.RunTool(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	if admissions.captured.IdempotencyKey == first {
		t.Fatal("a changed argument reused the first call's idempotency key")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for index := 0; index+len(needle) <= len(haystack); index++ {
			if haystack[index:index+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

/* ── the analytics record (issue 618) ─────────────────────────────────── */

type stubRecorder struct {
	records []ToolRunRecord
	err     error
}

func (s *stubRecorder) RecordToolRun(_ context.Context, record ToolRunRecord) error {
	s.records = append(s.records, record)
	return s.err
}

func newRecordingService(
	t *testing.T,
	settlements SettlementReader,
	recorder RunRecorder,
	deadline time.Duration,
) *RunService {
	t.Helper()
	service, err := NewRunService(
		&stubResolver{inputs: testInputs()}, stubVerdict{supported: true},
		&stubAdmissions{run: testAdmitted()}, &stubDispatcher{}, settlements,
		DispatchPolicy{
			CapabilityVersion: "1", ResourceClass: "indexing", IsolationClass: "project",
			Priority: 1, LimitsRevision: "limits-v1",
		},
		func() (string, error) { return "generated", nil },
		deadline,
		WithRunRecorder(recorder),
	)
	if err != nil {
		t.Fatalf("compose run service: %v", err)
	}
	service.now = func() time.Time { return time.Unix(2, 0).UTC() }
	return service
}

// The EXPLICIT half of issue 618's producer: a tool run a person or an MCP
// client asked for leaves a durable record carrying the identity the analytics
// read groups by. Before this, execution_jobs held the run with neither a
// toolkit id nor a tool name on it.
func TestRunToolRecordsASettledRunForAnalytics(t *testing.T) {
	recorder := &stubRecorder{}
	service := newRecordingService(t, &stubSettlements{
		settlement: settledPayload(t, runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_OK, `{}`, ""),
		found:      true,
	}, recorder, time.Second)

	if _, err := service.RunTool(context.Background(), validRequest()); err != nil {
		t.Fatal(err)
	}
	if len(recorder.records) != 1 {
		t.Fatalf("expected one record, got %d", len(recorder.records))
	}
	record := recorder.records[0]
	if record.ProjectID != 1 || record.ActorUserID != 7 || record.ToolkitID != 19 ||
		record.ToolName != "list_issues" || record.ToolkitType != "github" {
		t.Fatalf("record identity: %+v", record)
	}
	if record.ExecutionID != "exec-1" {
		t.Fatalf("the record must name the execution it describes: %+v", record)
	}
	// The clock comes from the ADMISSION, which is durable and survives an
	// idempotent replay, not from this process reading its own clock twice.
	if !record.StartedAt.Equal(time.Unix(1, 0).UTC()) {
		t.Fatalf("started_at = %s, want the admitted_at", record.StartedAt)
	}
	if !record.FinishedAt.Equal(time.Unix(2, 0).UTC()) {
		t.Fatalf("finished_at = %s, want the settlement moment", record.FinishedAt)
	}
	if record.IsError {
		t.Fatal("a successful run must not be recorded as an error")
	}
}

// A tool that answered with an error still RAN, so it is still a call. Recording
// only successes would make the tab's error rate structurally zero.
func TestRunToolRecordsAToolErrorAsAnError(t *testing.T) {
	recorder := &stubRecorder{}
	service := newRecordingService(t, &stubSettlements{
		settlement: settledPayload(t, runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_TOOL_ERROR, ``, "boom"),
		found:      true,
	}, recorder, time.Second)

	if _, err := service.RunTool(context.Background(), validRequest()); err != nil {
		t.Fatal(err)
	}
	if len(recorder.records) != 1 || !recorder.records[0].IsError {
		t.Fatalf("a tool error must be recorded as one: %+v", recorder.records)
	}
}

// A run whose bounded wait expired is DURABLE and still going. It is recorded
// with no finish time rather than dropped: dropping it would make a hanging tool
// disappear from the tab that should show it.
func TestRunToolRecordsAPendingRunWithoutAFinishTime(t *testing.T) {
	recorder := &stubRecorder{}
	service := newRecordingService(t, &stubSettlements{found: false}, recorder, 80*time.Millisecond)

	if _, err := service.RunTool(context.Background(), validRequest()); !errors.Is(err, ErrToolRunDeadlineExceeded) {
		t.Fatalf("expected the bounded wait to expire, got %v", err)
	}
	if len(recorder.records) != 1 {
		t.Fatalf("expected one record, got %d", len(recorder.records))
	}
	record := recorder.records[0]
	if !record.FinishedAt.IsZero() {
		t.Fatalf("a run still going must carry no finish time: %+v", record)
	}
	if record.IsError {
		t.Fatal("a run that has not settled is not a failure")
	}
}

// Nothing ran, so nothing is recorded. The refusal happens before any durable
// write, and a record here would invent a call.
func TestRunToolRecordsNothingWhenTheToolNeverRan(t *testing.T) {
	recorder := &stubRecorder{}
	service, err := NewRunService(
		&stubResolver{inputs: testInputs()}, stubVerdict{supported: false, reason: "no image"},
		&stubAdmissions{run: testAdmitted()}, &stubDispatcher{}, &stubSettlements{},
		DispatchPolicy{
			CapabilityVersion: "1", ResourceClass: "indexing", IsolationClass: "project",
			Priority: 1, LimitsRevision: "limits-v1",
		},
		func() (string, error) { return "generated", nil },
		time.Second,
		WithRunRecorder(recorder),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.RunTool(context.Background(), validRequest()); !errors.Is(err, ErrUnsupportedToolkitType) {
		t.Fatalf("expected the type refusal, got %v", err)
	}
	if len(recorder.records) != 0 {
		t.Fatalf("a refused run must not be recorded: %+v", recorder.records)
	}
}

// A recorder failure must not fail the run. The tool has already executed and
// its result is in hand; refusing to hand it back because a statistics row did
// not commit would turn a reporting gap into an outage.
func TestRunToolSurvivesARecorderFailure(t *testing.T) {
	recorder := &stubRecorder{err: errors.New("record unavailable")}
	service := newRecordingService(t, &stubSettlements{
		settlement: settledPayload(t, runtimev1.ToolkitCallToolStatusV1_TOOLKIT_CALL_TOOL_STATUS_V1_OK, `{"ok":true}`, ""),
		found:      true,
	}, recorder, time.Second)

	outcome, err := service.RunTool(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("a recorder failure must not fail the run: %v", err)
	}
	if outcome.Status != RunStatusOK || outcome.ResultJSON != `{"ok":true}` {
		t.Fatalf("the tool result was altered by a recorder failure: %+v", outcome)
	}
}
