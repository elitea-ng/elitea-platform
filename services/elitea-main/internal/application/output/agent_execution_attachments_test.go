package output

import (
	"encoding/json"
	"strings"
	"testing"

	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

// scaffoldChunk is the header chunk admission wrote, marker intact
// (application/agentexecution/attachments.go, attachmentContentScaffold). The
// marker is what a later turn's worker reads to decide the file has already
// been extracted, so it has to survive every layer below.
const scaffoldChunk = `{"type":"text","text":"Bucket: chat-attachments\nFilename: conv/report.pdf",` +
	`"elitea_attachment":{"needs_content_extraction":true,"bucket":"chat-attachments",` +
	`"name":"conv/report.pdf","filepath":"/chat-attachments/conv/report.pdf",` +
	`"item_id":"50000000-0000-4000-8000-000000000001"}}`

func enrichedContent(text string) json.RawMessage {
	return json.RawMessage(`[` + scaffoldChunk + `,{"type":"text","text":"` + text + `"}]`)
}

func TestAcceptedAttachmentContentsKeepsAWellFormedEntryVerbatim(t *testing.T) {
	content := enrichedContent("EXTRACTED TEXT")
	original := string(content)
	accepted := AcceptedAgentExecutionAttachmentContents([]AgentExecutionAttachmentContent{
		{ItemID: "50000000-0000-4000-8000-000000000001", Content: content},
	})
	if len(accepted) != 1 || accepted[0].ItemID != "50000000-0000-4000-8000-000000000001" {
		t.Fatalf("accepted=%+v", accepted)
	}
	if string(accepted[0].Content) != original {
		t.Fatalf("content = %s", accepted[0].Content)
	}
	// The kept value must not alias the caller's buffer: on the transport path
	// that buffer belongs to the decoded protobuf frame, and it reaches a
	// transaction that outlives the mapping.
	content[1] = 'X'
	if string(accepted[0].Content) != original {
		t.Fatalf("accepted entry aliases the caller's buffer: %s", accepted[0].Content)
	}
}

func TestAcceptedAttachmentContentsIsNilForNoEntries(t *testing.T) {
	if accepted := AcceptedAgentExecutionAttachmentContents(nil); accepted != nil {
		t.Fatalf("accepted=%+v", accepted)
	}
	if accepted := AcceptedAgentExecutionAttachmentContents(
		[]AgentExecutionAttachmentContent{},
	); accepted != nil {
		t.Fatalf("accepted=%+v", accepted)
	}
}

// A malformed entry is DROPPED and its well-formed siblings still land -- the
// decision AcceptedAgentExecutionAttachmentContents records. Each case here is
// one shape a worker (or a forged frame) can produce.
func TestAcceptedAttachmentContentsDropsOnlyTheMalformedEntry(t *testing.T) {
	good := AgentExecutionAttachmentContent{
		ItemID: "50000000-0000-4000-8000-000000000009", Content: enrichedContent("KEEP ME"),
	}
	cases := map[string]AgentExecutionAttachmentContent{
		"item id is not a uuid": {
			ItemID: "report.pdf", Content: enrichedContent("A"),
		},
		"item id is an uppercased uuid": {
			ItemID: "50000000-0000-4000-8000-00000000000A", Content: enrichedContent("A"),
		},
		"item id is empty": {
			ItemID: "", Content: enrichedContent("A"),
		},
		"content is a JSON object, not an array": {
			ItemID:  "50000000-0000-4000-8000-000000000001",
			Content: json.RawMessage(`{"type":"text","text":"A"}`),
		},
		"content is an empty array": {
			ItemID: "50000000-0000-4000-8000-000000000001", Content: json.RawMessage(`[]`),
		},
		"content's first element is not an object": {
			ItemID: "50000000-0000-4000-8000-000000000001", Content: json.RawMessage(`["A"]`),
		},
		"content is not valid JSON": {
			ItemID: "50000000-0000-4000-8000-000000000001", Content: json.RawMessage(`[{"type":`),
		},
		"content is absent": {
			ItemID: "50000000-0000-4000-8000-000000000001", Content: nil,
		},
		"one entry alone exceeds the aggregate cap": {
			ItemID:  "50000000-0000-4000-8000-000000000001",
			Content: enrichedContent(strings.Repeat("x", MaxAgentExecutionAttachmentContentBytes)),
		},
	}
	for name, malformed := range cases {
		t.Run(name, func(t *testing.T) {
			accepted := AcceptedAgentExecutionAttachmentContents(
				[]AgentExecutionAttachmentContent{malformed, good},
			)
			if name == "one entry alone exceeds the aggregate cap" {
				// The cap is a list-level rule, so it busts the whole list;
				// every other case here loses one entry and keeps the other.
				if accepted != nil {
					t.Fatalf("accepted item ids=%v", acceptedItemIDs(accepted))
				}
				return
			}
			if len(accepted) != 1 || accepted[0].ItemID != good.ItemID {
				t.Fatalf("accepted item ids=%v", acceptedItemIDs(accepted))
			}
		})
	}
}

func TestAcceptedAttachmentContentsDropsEveryEntrySharingADuplicateItemID(t *testing.T) {
	duplicate := "50000000-0000-4000-8000-000000000001"
	accepted := AcceptedAgentExecutionAttachmentContents([]AgentExecutionAttachmentContent{
		{ItemID: duplicate, Content: enrichedContent("FIRST")},
		{ItemID: duplicate, Content: enrichedContent("SECOND")},
		{ItemID: "50000000-0000-4000-8000-000000000002", Content: enrichedContent("OTHER")},
	})
	// Neither of the two survives: picking "the first" would be choosing one of
	// two contents for one row by arrival order, which is the coin flip
	// `item_id` exists to prevent. The unambiguous third entry is untouched.
	if len(accepted) != 1 || accepted[0].ItemID != "50000000-0000-4000-8000-000000000002" {
		t.Fatalf("accepted item ids=%v", acceptedItemIDs(accepted))
	}
}

func TestAcceptedAttachmentContentsDropsTheWholeListOverTheAggregateCap(t *testing.T) {
	half := strings.Repeat("x", MaxAgentExecutionAttachmentContentBytes/2)
	under := []AgentExecutionAttachmentContent{
		{ItemID: "50000000-0000-4000-8000-000000000001", Content: enrichedContent(half)},
	}
	if accepted := AcceptedAgentExecutionAttachmentContents(under); len(accepted) != 1 {
		t.Fatalf("a list under the cap must be accepted: %+v", accepted)
	}
	over := append(under, AgentExecutionAttachmentContent{
		ItemID: "50000000-0000-4000-8000-000000000002", Content: enrichedContent(half),
	})
	// Not trimmed to fit: trimming would be a choice about which attachment
	// matters, made with no information to make it with.
	if accepted := AcceptedAgentExecutionAttachmentContents(over); accepted != nil {
		t.Fatalf("accepted item ids=%v", acceptedItemIDs(accepted))
	}
}

func TestAgentExecutionResultValidateRefusesAHandBuiltAttachmentList(t *testing.T) {
	base := validAgentExecutionResultForTest()
	base.AttachmentContents = AcceptedAgentExecutionAttachmentContents(
		[]AgentExecutionAttachmentContent{
			{ItemID: "50000000-0000-4000-8000-000000000001", Content: enrichedContent("OK")},
		},
	)
	if err := base.Validate(); err != nil {
		t.Fatalf("accepted contents must validate: %v", err)
	}
	// The transport cannot produce these, because it goes through the
	// acceptance rule. A future caller that assembles the struct by hand can,
	// and Validate() is what stops it reaching the projection.
	for name, entries := range map[string][]AgentExecutionAttachmentContent{
		"non-uuid item id": {
			{ItemID: "nope", Content: enrichedContent("A")},
		},
		"non-array content": {
			{ItemID: "50000000-0000-4000-8000-000000000001", Content: json.RawMessage(`{}`)},
		},
		"duplicate item ids": {
			{ItemID: "50000000-0000-4000-8000-000000000001", Content: enrichedContent("A")},
			{ItemID: "50000000-0000-4000-8000-000000000001", Content: enrichedContent("B")},
		},
		// Two entries each comfortably under the per-entry limit, so only the
		// LIST-level rule can refuse them.
		"over the aggregate cap": {
			{
				ItemID:  "50000000-0000-4000-8000-000000000001",
				Content: enrichedContent(strings.Repeat("x", MaxAgentExecutionAttachmentContentBytes/2)),
			},
			{
				ItemID:  "50000000-0000-4000-8000-000000000002",
				Content: enrichedContent(strings.Repeat("x", MaxAgentExecutionAttachmentContentBytes/2)),
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			result := validAgentExecutionResultForTest()
			result.AttachmentContents = entries
			if err := result.Validate(); err == nil {
				t.Fatal("Validate() accepted an unsanitised attachment list")
			}
		})
	}
}

// validAgentExecutionResultForTest is the smallest result Validate() accepts
// with no attachment write-back at all, so every failure below is attributable
// to the list under test.
func validAgentExecutionResultForTest() AgentExecutionResult {
	digest := runtimedomain.SHA256([]byte("agent-execution-result"))
	return AgentExecutionResult{
		InputBundleID:           "bundle-1",
		InputBundleDigest:       digest,
		RequestEntryID:          "entry-1",
		RequestImmutableVersion: "sha256:1",
		RequestContentDigest:    digest,
		TerminalState:           AgentExecutionTerminalCompleted,
		ResultArtifact: AgentExecutionArtifactReference{
			ArtifactID:       "node-event:execution-1:full-message",
			ImmutableVersion: "sha256:1",
			MediaType:        AgentResultMediaType,
			ByteLength:       11,
			Digest:           digest,
			Classification:   AgentResultClassification,
		},
	}
}

func acceptedItemIDs(entries []AgentExecutionAttachmentContent) []string {
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.ItemID)
	}
	return ids
}
