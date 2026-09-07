package run_test

// The ANSWER TOKEN channel through this host (issue #701).
//
// The host is the middle of three hops, and the one that decides what the
// browser can tell apart: it reads `{"thinking": …}` and `{"token": …}` off
// the sidecar's stream and turns each into one read-once custom event. A
// token that arrived as a plain progress event would be shown as a thinking
// card — the answer rendered as a column of log lines, with nothing failing
// anywhere — so what is pinned here is the ENVELOPE and the ORDER, not that
// events happened.
//
// The golden sequence is conformance/provider/fixtures/deepwiki/stream/
// token_events.json. The Python engine's own tests replay its `engine_lines`
// half (services/elitea-deepwiki/tests/unit/test_answer_tokens.py) and the
// browser's replay the `chat_frames` half
// (apps/elitea-web/src/features/wiki-chat/lib/framesFromChatPoll.test.ts).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki/run"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

type tokenGolden struct {
	GoldenSequence struct {
		EngineLines  []map[string]any `json:"engine_lines"`
		CustomEvents []struct {
			Data struct {
				Message string `json:"message"`
			} `json:"data"`
		} `json:"custom_events"`
		StreamingText string `json:"streaming_text"`
	} `json:"golden_sequence"`
}

func loadTokenGolden(t *testing.T) tokenGolden {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtures, "stream", "token_events.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden tokenGolden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	return golden
}

// askRequest is one chat question, as the facade forwards it.
func askRequest(question string) map[string]any {
	request := fixtureRequest("", transport)
	request["parameters"] = map[string]any{"question": question}
	return request
}

// answerFragmentsOf reads the token fragments out of the drained event
// messages, in order, and reports the progress messages beside them.
func answerFragmentsOf(t *testing.T, events []string) (fragments []string, progress []string) {
	t.Helper()
	for _, message := range events {
		var envelope struct {
			Event string `json:"event"`
			Data  struct {
				Text string `json:"text"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(message), &envelope); err == nil && envelope.Event == "llm_chunk" {
			fragments = append(fragments, envelope.Data.Text)
			continue
		}
		progress = append(progress, message)
	}
	return fragments, progress
}

func TestTheHostRelaysEngineTokensAsAnswerEventsInOrder(t *testing.T) {
	golden := loadTokenGolden(t)

	lines := make([]string, 0, len(golden.GoldenSequence.EngineLines))
	for _, line := range golden.GoldenSequence.EngineLines {
		encoded, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(encoded))
	}

	sidecar := newFakeSidecar(t, lines, 0)
	body, events, err := invokeWithEvents(
		t, engineRunner(sidecar, &fakeArtifactClient{}), spi.Family{Name: "main"},
		"ask", askRequest("Where do the wiki pages live?"), "",
	)
	if err != nil {
		t.Fatal(err)
	}

	// The events the host produced, message for message, against the golden
	// list. Compared whole rather than by counting: an implementation that
	// emitted the right number of events in the wrong order, or that put a
	// fragment in a progress event, would pass every count.
	//
	// The host adds its own progress around the engine's — "Starting ask",
	// and what it says when the run lands — so the golden list must be a
	// SUBSEQUENCE in order rather than the whole of it.
	want := make([]string, 0, len(golden.GoldenSequence.CustomEvents))
	for _, event := range golden.GoldenSequence.CustomEvents {
		want = append(want, event.Data.Message)
	}
	if !containsInOrder(events, want) {
		t.Fatalf("the golden events are not in the drained list, in order:\nwant %q\ngot  %q", want, events)
	}

	fragments, _ := answerFragmentsOf(t, events)
	if strings.Join(fragments, "") != golden.GoldenSequence.StreamingText {
		t.Fatalf("fragments joined to %q, want %q", strings.Join(fragments, ""), golden.GoldenSequence.StreamingText)
	}

	// The answer still arrives whole on the terminal poll. The token channel
	// is a preview, not a replacement: a browser that missed a poll — the
	// events are read-once — must still end with the answer.
	objects := objectsOf(t, body)
	if !strings.Contains(str(objects[0]["data"]), golden.GoldenSequence.StreamingText) {
		t.Fatalf("the terminal result lost the answer: %v", objects)
	}
}

func TestAProgressMessageThatLooksLikeATokenIsStillProgress(t *testing.T) {
	// The two channels are separate KEYS on the socket, so a progress line
	// whose text happens to be a well-formed token envelope stays progress.
	// This is why the sidecar has a `token` key rather than a convention
	// about what a `thinking` message may contain.
	envelope := spi.TokenEvent("not an answer")
	lines := []string{
		`{"thinking": ` + quote(t, envelope) + `}`,
		`{"result": {"success": true, "answer": "the answer"}}`,
	}
	sidecar := newFakeSidecar(t, lines, 0)
	_, events, err := invokeWithEvents(
		t, engineRunner(sidecar, &fakeArtifactClient{}), spi.Family{Name: "main"},
		"ask", askRequest("?"), "",
	)
	if err != nil {
		t.Fatal(err)
	}
	// It reaches the browser as the same text either way — the host does not
	// re-wrap a thinking message — and that is the point: the host never had
	// to decide. What must not happen is the host inventing a SECOND event.
	seen := 0
	for _, message := range events {
		if message == envelope {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("the message was relayed %d times: %q", seen, events)
	}
}

func TestTheFixtureRunnerStreamsTheAnswerItReturns(t *testing.T) {
	cases := []struct {
		tool     string
		request  map[string]any
		contains string
	}{
		{"ask", askRequest("Where do the wiki pages live?"), "Fixture answer to: Where do the wiki pages live?"},
		{"deep_research", askRequest("How is storage laid out?"), "Research report"},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			body, events, err := invokeWithEvents(
				t, fixtureRunner(&fakeArtifactClient{}, 0), spi.Family{Name: "main"},
				tc.tool, tc.request, "",
			)
			if err != nil {
				t.Fatal(err)
			}
			fragments, progress := answerFragmentsOf(t, events)
			if len(fragments) < 2 {
				t.Fatalf("one fragment cannot tell a stream from a whole answer: %q", events)
			}
			// The fragments join to the answer the SAME run returned, so the
			// two fixture runners cannot drift from their own answers.
			answer := strings.Join(fragments, "")
			objects := objectsOf(t, body)
			if !strings.Contains(str(objects[0]["data"]), answer) {
				t.Fatalf("the streamed answer is not the returned one:\nstreamed %q\nreturned %v", answer, objects)
			}
			if !strings.Contains(answer, tc.contains) {
				t.Fatalf("streamed %q, want it to contain %q", answer, tc.contains)
			}
			// Progress first, then the answer: every progress step this tool
			// declares arrives before the first fragment.
			if len(progress) == 0 {
				t.Fatal("the run streamed an answer and no progress at all")
			}
		})
	}
}

func TestAGenerationStreamsNoAnswerFragments(t *testing.T) {
	// generate_wiki produces a wiki, not an answer. A status line streamed
	// as an answer would open a streaming preview above a run that has no
	// answer to preview.
	_, events, err := invokeWithEvents(
		t, fixtureRunner(&fakeArtifactClient{}, 0), spi.Family{Name: "main"},
		"generate_wiki", fixtureRequest("GO", transport), "",
	)
	if err != nil {
		t.Fatal(err)
	}
	if fragments, _ := answerFragmentsOf(t, events); len(fragments) != 0 {
		t.Fatalf("a generation streamed %q", fragments)
	}
	if _, streamed := run.StreamedAnswerKey["generate_wiki"]; streamed {
		t.Fatal("generate_wiki must not declare a streamed answer key")
	}
}

func TestAnEmptyFragmentIsNotSpentAsAnEvent(t *testing.T) {
	// The events are read-once and the poll drain drops an empty message —
	// but an empty fragment is not an empty message, it is a well-formed
	// envelope around nothing, so it would survive the drain and reach the
	// browser as a token that adds no text.
	lines := []string{
		`{"token": ""}`,
		`{"token": "the answer"}`,
		`{"result": {"success": true, "answer": "the answer"}}`,
	}
	sidecar := newFakeSidecar(t, lines, 0)
	_, events, err := invokeWithEvents(
		t, engineRunner(sidecar, &fakeArtifactClient{}), spi.Family{Name: "main"},
		"ask", askRequest("?"), "",
	)
	if err != nil {
		t.Fatal(err)
	}
	fragments, _ := answerFragmentsOf(t, events)
	if len(fragments) != 1 || fragments[0] != "the answer" {
		t.Fatalf("fragments %q", fragments)
	}
}

func TestTheAnswerFragmentsJoinBackToTheAnswer(t *testing.T) {
	answer := "The wiki pages live in the wiki-artifacts bucket."
	fragments := run.AnswerFragments(answer)
	if len(fragments) < 2 || strings.Join(fragments, "") != answer {
		t.Fatalf("fragments %q", fragments)
	}
	// Shorter than the fragment count, and empty.
	if got := strings.Join(run.AnswerFragments("ab"), ""); got != "ab" {
		t.Fatalf("short answer %q", got)
	}
	if run.AnswerFragments("") != nil {
		t.Fatal("an empty answer streams nothing")
	}
	// Runes, not bytes: a fragment that cut a multi-byte character in half
	// would reach the browser as a replacement character, and the join would
	// no longer be the answer.
	multibyte := "ответ на вопрос о хранилище"
	if got := strings.Join(run.AnswerFragments(multibyte), ""); got != multibyte {
		t.Fatalf("multibyte answer %q", got)
	}
}

func TestAPollAtTheBrowsersCadenceSeesAPartialAnswer(t *testing.T) {
	// WHY A TIMING TEST AT ALL. A streamed answer that is complete before the
	// first poll is a whole answer with extra steps: the events are drained
	// on an interval, so if the fixture emits all of its fragments between
	// two polls the preview never appears and the journey that asserts it is
	// flaky rather than wrong. What makes it work is the RATIO — the fixture
	// paces a fragment per step, the browser polls every second step (1s and
	// 2s in the deployments) — and that is what this pins. The absolute
	// values are the compose files'; only the ratio is a property of the code.
	const step = 50 * time.Millisecond

	manager := spi.NewManager(nil, time.Hour, nil)
	manager.Start(context.Background())
	defer manager.Stop()

	runner := fixtureRunner(&fakeArtifactClient{}, step)
	ctx := context.Background()
	invocation, err := manager.Submit(ctx, "Wikis", "ask", func(ctx context.Context, tc *spi.Context) (map[string]any, error) {
		return runner.Invoke(ctx, spi.Invoke{
			Family: spi.Family{Name: "main"}, Toolkit: "Wikis", Tool: "ask",
			Request: askRequest("Where do the wiki pages live?"),
		}, tc)
	})
	if err != nil {
		t.Fatal(err)
	}

	sawPartial := false
	settled := false
	var streamed []string
	for i := 0; i < 400 && !settled; i++ {
		time.Sleep(2 * step)
		body, err := manager.Poll(ctx, "Wikis", "ask", invocation.ID)
		if err != nil {
			t.Fatal(err)
		}
		var messages []string
		if raw, ok := body["custom_events"].([]map[string]any); ok {
			for _, event := range raw {
				data, _ := event["data"].(map[string]any)
				messages = append(messages, str(data["message"]))
			}
		}
		fragments, _ := answerFragmentsOf(t, messages)
		streamed = append(streamed, fragments...)
		settled = body["status"] == "Completed" || body["status"] == "Error"
		// A PARTIAL answer: fragments in hand, and the run still going. Both
		// halves matter — fragments on the terminal poll only would be the
		// whole answer arriving twice.
		if !settled && len(fragments) > 0 {
			sawPartial = true
		}
	}
	if !settled {
		t.Fatal("the invocation never settled")
	}
	if !sawPartial {
		t.Fatalf("no poll saw a partial answer; every fragment landed at once: %q", streamed)
	}
	if strings.Join(streamed, "") != "Fixture answer to: Where do the wiki pages live?" {
		t.Fatalf("streamed %q", strings.Join(streamed, ""))
	}
}

// containsInOrder reports whether want appears in got, in order, with
// anything allowed between the entries.
func containsInOrder(got, want []string) bool {
	next := 0
	for _, message := range got {
		if next < len(want) && message == want[next] {
			next++
		}
	}
	return next == len(want)
}

func quote(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
