package run

// The SPI poll envelope, read the way the browser reads it.
//
// THAT AGREEMENT IS THE POINT, not an aspiration: what a transcript records
// and what the user was shown come from the same bytes, and they only match
// while the status vocabulary, the fallback order and the result shapes are
// read the same way on both sides. Each case below has a twin in
// `apps/elitea-web/src/entities/provider-run/model/poll.test.ts` or
// `features/wiki-chat/model/reducer.test.ts`.

import (
	"encoding/json"
	"testing"
)

func TestTerminalOfReadsACompletedRun(t *testing.T) {
	terminal, ok := TerminalOf([]byte(
		`{"invocation_id":"inv-1","status":"Completed","result":"The pages live in wiki_pages/."}`))
	if !ok {
		t.Fatal("a completed poll was not read as terminal")
	}
	if terminal.InvocationID != "inv-1" || terminal.Content != "The pages live in wiki_pages/." {
		t.Fatalf("terminal = %+v", terminal)
	}
	if terminal.Failed {
		t.Error("a completed run was reported as failed")
	}
}

// `Error` and `Stopped` are terminal too, and they are carried rather than
// dropped: a consumer that saw nothing cannot tell a run that ended badly
// from one that never finished.
func TestTerminalOfReadsAFailedRun(t *testing.T) {
	for name, body := range map[string]string{
		"error":                `{"invocation_id":"inv-1","status":"Error","result":"the clone failed"}`,
		"stopped after cancel": `{"invocation_id":"inv-1","status":"Stopped","message":"the clone failed"}`,
	} {
		t.Run(name, func(t *testing.T) {
			terminal, ok := TerminalOf([]byte(body))
			if !ok || !terminal.Failed {
				t.Fatalf("terminal = %+v, ok = %v; want a failed run", terminal, ok)
			}
			if terminal.Content != "the clone failed" {
				t.Errorf("content = %q", terminal.Content)
			}
		})
	}
}

// A failure with nothing to say still says something. An empty bubble reads
// as a run that produced no answer, which is a different event.
func TestAFailureWithNoMessageFallsBackToASentence(t *testing.T) {
	terminal, ok := TerminalOf([]byte(`{"invocation_id":"inv-1","status":"Error"}`))
	if !ok || terminal.Content == "" {
		t.Fatalf("terminal = %+v, ok = %v", terminal, ok)
	}
}

func TestTerminalOfRefusesAnythingThatDidNotEnd(t *testing.T) {
	for name, body := range map[string]string{
		"started":                        `{"invocation_id":"inv-1","status":"Started"}`,
		"in progress":                    `{"invocation_id":"inv-1","status":"InProgress"}`,
		"a status we do not know":        `{"invocation_id":"inv-1","status":"Reticulating"}`,
		"no status at all":               `{"invocation_id":"inv-1"}`,
		"no invocation id":               `{"status":"Completed","result":"done"}`,
		"a completed run with no answer": `{"invocation_id":"inv-1","status":"Completed","result":""}`,
		"not json":                       `<html>gateway</html>`,
	} {
		t.Run(name, func(t *testing.T) {
			if terminal, ok := TerminalOf([]byte(body)); ok {
				t.Fatalf("%s was read as terminal: %+v", name, terminal)
			}
		})
	}
}

func TestInvocationIDOfReadsAnAcceptedInvoke(t *testing.T) {
	if got := InvocationIDOf([]byte(`{"invocation_id":"inv-7","status":"Started"}`)); got != "inv-7" {
		t.Errorf("invocation = %q, want inv-7", got)
	}
	// An acceptance with no id is a run nothing can follow. Empty, not a
	// guess: the browser refuses it too.
	if got := InvocationIDOf([]byte(`{"status":"Started"}`)); got != "" {
		t.Errorf("invocation = %q, want empty", got)
	}
	if got := InvocationIDOf(nil); got != "" {
		t.Errorf("invocation = %q, want empty", got)
	}
}

// AnswerText is a transcription of the browser's readAnswer. Where the two
// disagree the user sees one thing and the transcript records another, so the
// same shapes are pinned here.
func TestAnswerTextReadsTheShapesTheBrowserReads(t *testing.T) {
	sources := mustJSON(t, []map[string]any{
		{"object_type": "message", "data": "The answer."},
		{"object_type": "message", "data": "Sources:\n- wiki_pages/overview.md"},
		{"object_type": "artifact", "data": "ignored"},
	})
	for name, testCase := range map[string]struct{ result, want string }{
		"a bare string":             {"Just the answer.", "Just the answer."},
		"the platform result array": {sources, "The answer.\nSources:\n- wiki_pages/overview.md"},
		"an envelope naming answer": {`{"answer":"From answer."}`, "From answer."},
		"an empty answer falls through to result": {
			`{"answer":"","result":"From result."}`, "From result."},
		"an envelope naming nothing":      {`{"other":1}`, `{"other":1}`},
		"a brace-first sentence":          {`{not json at all`, `{not json at all`},
		"a result array with no messages": {`[{"object_type":"artifact","data":"x"}]`, ""},
		"nothing at all":                  {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := AnswerText(testCase.result); got != testCase.want {
				t.Fatalf("AnswerText(%q) = %q, want %q", testCase.result, got, testCase.want)
			}
		})
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
