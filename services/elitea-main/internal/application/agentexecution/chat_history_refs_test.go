package agentexecution

import (
	"encoding/json"
	"errors"
	"testing"
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
