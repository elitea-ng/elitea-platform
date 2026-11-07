package agentexecution

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/contextview"
)

func TestDecodeChatHistoryRefsNamesEveryEntry(t *testing.T) {
	history := json.RawMessage(`[{"role":"user"},{"role":"assistant"}]`)
	refs, err := DecodeChatHistoryRefs(history, json.RawMessage(`[
		{"message_id":"00000000-0000-4000-8000-000000000001","readers_own":true},
		{"message_id":"00000000-0000-4000-8000-000000000002","readers_own":false}]`))
	if err != nil || len(refs) != 2 || !refs[0].ReadersOwn || refs[1].ReadersOwn {
		t.Fatalf("refs=%+v err=%v", refs, err)
	}
	input := chatHistoryRefsInput(refs)
	if len(input) != 2 || input[1].GetMessageId() != "00000000-0000-4000-8000-000000000002" || !input[0].GetReadersOwn() {
		t.Fatalf("input=%v", input)
	}
	if chatHistoryRefsInput(nil) != nil {
		t.Fatal("no references must send none")
	}
	for name, raw := range map[string]string{
		"short":    `[{"message_id":"00000000-0000-4000-8000-000000000001","readers_own":true}]`,
		"bad uuid": `[{"message_id":"x","readers_own":true},{"message_id":"00000000-0000-4000-8000-000000000002","readers_own":false}]`,
		"not json": `{`,
	} {
		if _, err := DecodeChatHistoryRefs(history, json.RawMessage(raw)); !errors.Is(err, ErrMalformedChatHistoryRefs) {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
}

// TestBuildReaderViewThreeAgents is the issue 1609 fixture at the filter:
// a person, agents A, B and C. B and C talk on the side; C answers a message
// addressed to it. Each reader's view holds only what it may see.
func TestBuildReaderViewThreeAgents(t *testing.T) {
	history := json.RawMessage(`[{"id":"1"},{"id":"2"},{"id":"3"},{"id":"4"}]`)
	id := func(n string) string { return "00000000-0000-4000-8000-00000000000" + n }
	// 1: the person to everyone, before C joined. 2: A's answer.
	// 3: B to C on the side. 4: C's side reply to B.
	forC := []ChatHistoryRef{
		{MessageID: id("1")},
		{MessageID: id("2"), AfterJoined: false},
		{MessageID: id("3"), Side: true, Addressed: true, AfterJoined: true, AnsweredByReader: true},
		{MessageID: id("4"), Side: true, OwnAgent: true, ReadersOwn: true, AfterJoined: true},
	}
	forA := []ChatHistoryRef{
		{MessageID: id("1"), AfterJoined: true, AnsweredByReader: true},
		{MessageID: id("2"), OwnAgent: true, ReadersOwn: true, AfterJoined: true},
		{MessageID: id("3"), Side: true, AfterJoined: true},
		{MessageID: id("4"), Side: true, AfterJoined: true},
	}
	ids := func(view ReaderView) []string {
		var entries []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(view.History, &entries); err != nil || len(entries) != len(view.Refs) {
			t.Fatalf("view=%s refs=%d err=%v", view.History, len(view.Refs), err)
		}
		out := make([]string, 0, len(entries))
		for _, entry := range entries {
			out = append(out, entry.ID)
		}
		return out
	}
	cases := []struct {
		name     string
		settings contextview.Settings
		refs     []ChatHistoryRef
		want     string
	}{
		{"A full: no side talk between B and C", contextview.Default, forA, "1,2"},
		{"C full: its own side talk", contextview.Default, forC, "1,2,3,4"},
		{"C since joined", contextview.Default.WithParticipant("since_joined"), forC, "3,4"},
		{"C addressed only", contextview.Settings{History: contextview.HistoryAddressedOnly, SideTalk: contextview.SideTalkParty}, forC, "3,4"},
		{"C no side talk", contextview.Settings{History: contextview.HistoryFull, SideTalk: contextview.SideTalkNone}, forC, "1,2"},
		{"C none", contextview.Settings{History: contextview.HistoryNone, SideTalk: contextview.SideTalkParty}, forC, ""},
	}
	for _, c := range cases {
		view, err := BuildReaderView(c.settings, history, c.refs)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := strings.Join(ids(view), ","); got != c.want {
			t.Errorf("%s: view %q, want %q", c.name, got, c.want)
		}
	}
	unfiltered, _ := BuildReaderView(contextview.Default, history, forC)
	if !bytes.Equal(unfiltered.History, history) {
		t.Fatal("an unfiltered view must keep the projection's own bytes")
	}
	if _, err := BuildReaderView(contextview.Default, history, forC[:2]); !errors.Is(err, ErrMalformedChatHistoryRefs) {
		t.Fatalf("mismatched refs: %v", err)
	}
	if _, err := BuildReaderView(contextview.Default, history, nil); !errors.Is(err, ErrMalformedChatHistoryRefs) {
		t.Fatalf("history without refs must fail closed: %v", err)
	}
	empty, err := BuildReaderView(contextview.Default, json.RawMessage(`[]`), nil)
	if err != nil || string(empty.History) != `[]` {
		t.Fatalf("empty history: %s %v", empty.History, err)
	}
}

func TestReaderViewSettingsReadsTheVersionAndParticipant(t *testing.T) {
	decode := func(raw string) map[string]any {
		var version map[string]any
		if err := json.Unmarshal([]byte(raw), &version); err != nil {
			t.Fatal(err)
		}
		return version
	}
	if got := readerViewSettings(decode(`{"meta":{"context":{"history":"addressed_only"}}}`), ""); got.History != contextview.HistoryAddressedOnly {
		t.Fatalf("version mode: %+v", got)
	}
	if got := readerViewSettings(decode(`{"meta":{}}`), "since_joined"); got.History != contextview.HistorySinceJoined {
		t.Fatalf("participant mode: %+v", got)
	}
	if got := readerViewSettings(decode(`{"meta":{"context":{"history":"bogus"}}}`), ""); got != contextview.Default {
		t.Fatalf("malformed block must read as default: %+v", got)
	}
	if got := readerViewSettings(nil, ""); got != contextview.Default {
		t.Fatalf("no version: %+v", got)
	}
	input := contextViewInput(contextview.Default, &runtimev1.ModelContextLimitsV1{ContextWindowTokens: 8192}, []byte(`[]`), nil)
	if input.GetHistory() != "full" || input.GetWindowTokens() != 8192 ||
		input.GetDigest() != contextview.Digest(contextview.HistoryFull, 8192, []byte(`[]`), nil) {
		t.Fatalf("context view input: %v", input)
	}
}
