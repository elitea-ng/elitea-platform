package agentexecution

// #946: an ENABLED project context used to make every turn in the project
// answer 422 unsupported_agent_execution, because the admission SQL carried a
// `NOT EXISTS (… type = 'project_context' …)` refusal gate instead of an
// injection. The gate is gone (internal/db/queries/agent_chat.sql) and these
// assert what replaced it, on the ADMISSION the start path actually submitted
// rather than on the splice helpers alone — the helper being right while the
// field the worker reads is unchanged is precisely the class of defect the
// e2e journeys keep catching (`chat.tail-project-context.spec.ts` reads the
// journaled SYSTEM PROMPT for the same reason).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type stubProjectContextResolver struct {
	context    CurrentProjectContext
	err        error
	projectIDs []int64
}

func (s *stubProjectContextResolver) ResolveCurrentProjectContext(
	_ context.Context,
	projectID int64,
) (CurrentProjectContext, error) {
	s.projectIDs = append(s.projectIDs, projectID)
	if s.err != nil {
		return CurrentProjectContext{}, s.err
	}
	return s.context, nil
}

func TestCurrentProjectContextInjectableText(t *testing.T) {
	cases := []struct {
		name string
		in   CurrentProjectContext
		want string
	}{
		{"enabled with content injects it", CurrentProjectContext{Content: "House style: terse.", Enabled: true}, "House style: terse."},
		{"the toggle OFF injects nothing even with content stored", CurrentProjectContext{Content: "House style: terse.", Enabled: false}, ""},
		{"enabled but blank injects nothing rather than an empty block", CurrentProjectContext{Content: "   \n ", Enabled: true}, ""},
		{"no row at all injects nothing", CurrentProjectContext{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.InjectableText(); got != tc.want {
				t.Errorf("InjectableText() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveCurrentProjectContextTextFailsOpen(t *testing.T) {
	t.Run("no resolver attached injects nothing", func(t *testing.T) {
		service := &CurrentApplicationStartService{}
		if got := service.resolveCurrentProjectContextText(context.Background(), 7); got != "" {
			t.Errorf("resolveCurrentProjectContextText with no resolver = %q", got)
		}
	})

	// THE POINT OF #946. The previous design failed CLOSED on the mere
	// presence of a context row and took the project's chat down with it; a
	// read that errors must cost this turn its context, never the turn.
	t.Run("a failing read degrades to no injection, not an error", func(t *testing.T) {
		service := (&CurrentApplicationStartService{}).
			WithProjectContext(&stubProjectContextResolver{err: errors.New("configuration unreadable")})
		if got := service.resolveCurrentProjectContextText(context.Background(), 7); got != "" {
			t.Errorf("resolveCurrentProjectContextText on a failing resolver = %q, want \"\" (fail open)", got)
		}
	})

	t.Run("the turn's own project is the one read", func(t *testing.T) {
		stub := &stubProjectContextResolver{context: CurrentProjectContext{Content: "ctx", Enabled: true}}
		service := (&CurrentApplicationStartService{}).WithProjectContext(stub)
		if got := service.resolveCurrentProjectContextText(context.Background(), 42); got != "ctx" {
			t.Errorf("resolveCurrentProjectContextText = %q", got)
		}
		if len(stub.projectIDs) != 1 || stub.projectIDs[0] != 42 {
			t.Errorf("resolver was asked for %v, want [42]", stub.projectIDs)
		}
	})
}

func TestCurrentProjectContextIgnoredReadsTheAgentsOwnToggle(t *testing.T) {
	cases := []struct {
		name           string
		versionDetails string
		want           bool
	}{
		{"the Advanced panel's toggle ON opts the agent out", `{"meta":{"ignore_project_context":true}}`, true},
		{"the toggle OFF does not", `{"meta":{"ignore_project_context":false}}`, false},
		{"no toggle stored does not", `{"meta":{"step_limit":25}}`, false},
		{"no meta at all does not", `{"instructions":"Be helpful."}`, false},
		// Anything unreadable means "do not ignore": silently stripping a
		// project's context out of its prompts is a change the user cannot see
		// and would never be told about.
		{"a non-boolean toggle does not", `{"meta":{"ignore_project_context":"yes"}}`, false},
		{"a non-object meta does not", `{"meta":"nope"}`, false},
		{"an undecodable document does not", `{`, false},
		{"an empty document does not", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := currentProjectContextIgnored(json.RawMessage(tc.versionDetails)); got != tc.want {
				t.Errorf("currentProjectContextIgnored(%s) = %v, want %v", tc.versionDetails, got, tc.want)
			}
		})
	}
}

func TestAppendCurrentApplicationProjectContext(t *testing.T) {
	const version = `{"instructions":"Be helpful.","step_limit":25,"meta":{}}`

	t.Run("splices a delimited block onto instructions, siblings untouched", func(t *testing.T) {
		got := appendCurrentApplicationProjectContext(json.RawMessage(version), "House style: terse.")
		instructions := decodeInstructions(t, got)
		if !strings.Contains(instructions, "House style: terse.") {
			t.Errorf("instructions = %q, want the project context in it", instructions)
		}
		if !strings.Contains(instructions, currentProjectContextBlockOpen) ||
			!strings.Contains(instructions, currentProjectContextBlockClose) {
			t.Errorf("instructions = %q, want a delimited project_context block", instructions)
		}
		if !strings.HasPrefix(instructions, "Be helpful.") {
			t.Errorf("the agent's own instructions must come first: %q", instructions)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(got, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["step_limit"]) != "25" {
			t.Errorf("step_limit re-encoded as %q, want 25", fields["step_limit"])
		}
	})

	t.Run("no context is a byte-identical no-op", func(t *testing.T) {
		got := appendCurrentApplicationProjectContext(json.RawMessage(version), "")
		if string(got) != version {
			t.Errorf("empty context changed the version: %s", got)
		}
	})

	// ELITEA-0945: the per-agent opt-out.
	t.Run("an agent with Ignore Project Context ON is left alone", func(t *testing.T) {
		ignoring := `{"instructions":"Be helpful.","meta":{"ignore_project_context":true}}`
		got := appendCurrentApplicationProjectContext(json.RawMessage(ignoring), "House style: terse.")
		if string(got) != ignoring {
			t.Errorf("the ignoring agent's version was modified: %s", got)
		}
	})
}

func TestAppendCurrentInstructionsProjectContext(t *testing.T) {
	got := appendCurrentInstructionsProjectContext("Project chat instructions", "House style: terse.")
	if !strings.HasPrefix(got, "Project chat instructions\n\n") ||
		!strings.Contains(got, "House style: terse.") {
		t.Errorf("adhoc instructions = %q", got)
	}
	if same := appendCurrentInstructionsProjectContext("Project chat instructions", ""); same != "Project chat instructions" {
		t.Errorf("empty context changed the adhoc instructions: %q", same)
	}
}

// ── The admission, not the helper ───────────────────────────────────────────

func projectContextStartService(
	t *testing.T,
	resolver *currentApplicationResolverStub,
	projectContext CurrentProjectContextResolver,
) (*CurrentApplicationStartService, *currentApplicationAdmissionStub) {
	t.Helper()
	admissions := newCurrentAttachmentAdmissionStub()
	service, err := NewCurrentApplicationStartService(
		resolver, resolver, resolver, resolver, resolver,
		&currentAgentGuardrailStub{}, &currentApplicationVersionFreezerStub{}, admissions,
	)
	if err != nil {
		t.Fatal(err)
	}
	return service.WithProjectContext(projectContext), admissions
}

func submittedApplicationInstructions(t *testing.T, application []byte) string {
	t.Helper()
	var decoded struct {
		VersionDetails json.RawMessage `json:"version_details"`
		Instructions   string          `json:"instructions"`
	}
	if err := json.Unmarshal(application, &decoded); err != nil {
		t.Fatalf("decode application: %v", err)
	}
	if len(decoded.VersionDetails) == 0 {
		return decoded.Instructions
	}
	return decodeInstructions(t, decoded.VersionDetails)
}

func decodeInstructions(t *testing.T, versionDetails []byte) string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(versionDetails, &fields); err != nil {
		t.Fatalf("decode version details: %v", err)
	}
	var instructions string
	if err := json.Unmarshal(fields["instructions"], &instructions); err != nil {
		t.Fatalf("decode instructions: %v", err)
	}
	return instructions
}

const projectContextVersionDetails = `{"id":41,"application_id":31,"agent_type":"agent",` +
	`"instructions":"Be concise","llm_settings":{"model_name":"test"},"meta":{},"tools":[]}`

const projectContextIgnoringVersionDetails = `{"id":41,"application_id":31,"agent_type":"agent",` +
	`"instructions":"Be concise","llm_settings":{"model_name":"test"},` +
	`"meta":{"ignore_project_context":true},"tools":[]}`

// ELITEA-0951/0943/0948: the content the project saved reaches the system
// prompt of the turn the model is sent.
func TestCurrentApplicationStartInjectsTheProjectContextIntoTheTurnsInstructions(t *testing.T) {
	const phrase = "QA-TEST-PHRASE: Green heron dives at twilight"
	resolver := &currentApplicationResolverStub{target: CurrentApplicationTarget{
		ApplicationID: 31, ApplicationVersionID: 41,
		Variables:      json.RawMessage(`[]`),
		VersionDetails: json.RawMessage(projectContextVersionDetails),
		ChatHistory:    json.RawMessage(`[]`),
	}}
	service, admissions := projectContextStartService(
		t, resolver, &stubProjectContextResolver{context: CurrentProjectContext{Content: phrase, Enabled: true}},
	)
	if _, err := service.StartCurrentApplication(
		context.Background(), validCurrentApplicationStartRequest(),
	); err != nil {
		t.Fatalf("StartCurrentApplication() error = %v", err)
	}
	instructions := submittedApplicationInstructions(t, admissions.requests[0].Input.GetApplication())
	if !strings.Contains(instructions, phrase) {
		t.Fatalf("the enabled project context never reached the submitted instructions: %q", instructions)
	}
	if !strings.HasPrefix(instructions, "Be concise") {
		t.Fatalf("the agent's own instructions were displaced: %q", instructions)
	}
}

// ELITEA-0951's second half: the toggle OFF keeps it out.
func TestCurrentApplicationStartOmitsADisabledProjectContext(t *testing.T) {
	const phrase = "QA-TEST-PHRASE: Green heron dives at twilight"
	resolver := &currentApplicationResolverStub{target: CurrentApplicationTarget{
		ApplicationID: 31, ApplicationVersionID: 41,
		Variables:      json.RawMessage(`[]`),
		VersionDetails: json.RawMessage(projectContextVersionDetails),
		ChatHistory:    json.RawMessage(`[]`),
	}}
	service, admissions := projectContextStartService(
		t, resolver, &stubProjectContextResolver{context: CurrentProjectContext{Content: phrase, Enabled: false}},
	)
	if _, err := service.StartCurrentApplication(
		context.Background(), validCurrentApplicationStartRequest(),
	); err != nil {
		t.Fatalf("StartCurrentApplication() error = %v", err)
	}
	instructions := submittedApplicationInstructions(t, admissions.requests[0].Input.GetApplication())
	if strings.Contains(instructions, phrase) {
		t.Fatalf("a DISABLED project context reached the submitted instructions: %q", instructions)
	}
}

// ELITEA-0945: the per-agent toggle, end to end through the start path.
func TestCurrentApplicationStartHonoursIgnoreProjectContext(t *testing.T) {
	const phrase = "QA-TEST-PHRASE: Golden crane flies at noon"
	resolver := &currentApplicationResolverStub{target: CurrentApplicationTarget{
		ApplicationID: 31, ApplicationVersionID: 41,
		Variables:      json.RawMessage(`[]`),
		VersionDetails: json.RawMessage(projectContextIgnoringVersionDetails),
		ChatHistory:    json.RawMessage(`[]`),
	}}
	service, admissions := projectContextStartService(
		t, resolver, &stubProjectContextResolver{context: CurrentProjectContext{Content: phrase, Enabled: true}},
	)
	if _, err := service.StartCurrentApplication(
		context.Background(), validCurrentApplicationStartRequest(),
	); err != nil {
		t.Fatalf("StartCurrentApplication() error = %v", err)
	}
	instructions := submittedApplicationInstructions(t, admissions.requests[0].Input.GetApplication())
	if strings.Contains(instructions, phrase) {
		t.Fatalf("an agent with Ignore Project Context ON received it: %q", instructions)
	}
}

// The regression #946 itself is: the turn must be ADMITTED at all. A service
// with a project context attached still submits, rather than answering
// ErrUnsupportedCurrentAgentStart the way the SQL gate made the route do.
func TestCurrentApplicationStartIsAdmittedWithAnEnabledProjectContext(t *testing.T) {
	resolver := &currentApplicationResolverStub{target: CurrentApplicationTarget{
		ApplicationID: 31, ApplicationVersionID: 41,
		Variables:      json.RawMessage(`[]`),
		VersionDetails: json.RawMessage(projectContextVersionDetails),
		ChatHistory:    json.RawMessage(`[]`),
	}}
	service, admissions := projectContextStartService(
		t, resolver,
		&stubProjectContextResolver{context: CurrentProjectContext{Content: "anything", Enabled: true}},
	)
	if _, err := service.StartCurrentApplication(
		context.Background(), validCurrentApplicationStartRequest(),
	); err != nil {
		t.Fatalf("an enabled project context refused the turn: %v", err)
	}
	if len(admissions.requests) != 1 {
		t.Fatalf("submitted %d admissions, want 1", len(admissions.requests))
	}
}

func TestCurrentAdhocStartInjectsTheProjectContext(t *testing.T) {
	const phrase = "QA-TEST-PHRASE: Red kite soars at midday"
	resolver := &currentApplicationResolverStub{adhocTarget: CurrentAdhocTarget{
		TargetParticipantID: 21,
		LLMSettings:         json.RawMessage(`{"model_name":"saved"}`),
		Instructions:        "Project chat instructions",
		Tools:               json.RawMessage(`[]`),
		ChatHistory:         json.RawMessage(`[]`),
		ConversationMeta:    json.RawMessage(`{}`),
	}}
	service, admissions := projectContextStartService(
		t, resolver, &stubProjectContextResolver{context: CurrentProjectContext{Content: phrase, Enabled: true}},
	)
	if _, err := service.StartCurrentAdhoc(
		context.Background(), validCurrentAdhocStartRequest(),
	); err != nil {
		t.Fatalf("StartCurrentAdhoc() error = %v", err)
	}
	instructions := submittedApplicationInstructions(t, admissions.requests[0].Input.GetApplication())
	if !strings.Contains(instructions, phrase) ||
		!strings.HasPrefix(instructions, "Project chat instructions") {
		t.Fatalf("adhoc instructions = %q", instructions)
	}
}

// The runtime REFUSES an instruction string past 64 KiB rather than trimming
// it (bounded_instruction, services/elitea-worker-rust/src/agents/assembly.rs),
// and the builder tool accepts a 48 KiB context — so "inject it regardless"
// would put the failure #946 is about back, in a different layer. An injection
// that does not fit is skipped; the turn still runs.
func TestProjectContextThatWouldNotFitIsSkippedRatherThanBreakingTheTurn(t *testing.T) {
	oversized := strings.Repeat("x", maxCurrentProjectContextInstructionsBytes+1)

	version := json.RawMessage(`{"instructions":"Be helpful.","meta":{}}`)
	if got := appendCurrentApplicationProjectContext(version, oversized); string(got) != string(version) {
		t.Errorf("an oversized project context was injected anyway: %d bytes", len(got))
	}
	if got := appendCurrentInstructionsProjectContext("Be helpful.", oversized); got != "Be helpful." {
		t.Errorf("an oversized adhoc project context was injected anyway: %d bytes", len(got))
	}

	// A context that DOES fit alongside long instructions still lands — the
	// budget is a ceiling, not a blanket refusal of large contexts.
	long := strings.Repeat("y", 1024)
	got := appendCurrentApplicationProjectContext(version, long)
	if !strings.Contains(decodeInstructions(t, got), long) {
		t.Error("a context well inside the budget was skipped")
	}
}
