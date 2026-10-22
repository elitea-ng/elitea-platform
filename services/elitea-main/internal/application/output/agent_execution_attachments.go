package output

// #607: the ADMISSION side of the attachment write-back.
//
// The legacy runtime extracts a document's text once, at message-persist time,
// and stores it (rpc/chat_all.py:344-377 reads the files through the SDK
// artifact toolkit and appends the text as a second `{"type":"text"}` chunk of
// the item's `content`, with `flag_modified(item, "content")` at :376 because
// the column is `json`). Every later turn is then pure DB --
// utils/chat_history.py:67-73 extends the stored chunks straight into the
// message and re-reads nothing.
//
// In this port the two halves are in different services: only the worker can
// reach the artifact toolkit, and only elitea-main can write
// `chat_messages_attachment`. AgentExecutionResultV1.attachment_contents is the
// seam. This file is what elitea-main is allowed to believe about what comes
// across it.

import (
	"bytes"
	"encoding/json"

	"github.com/google/uuid"
)

// AcceptedAgentExecutionAttachmentContents is the ONLY sanctioned way to build
// AgentExecutionResult.AttachmentContents from a wire frame. It returns the
// entries that may be persisted and DROPS the rest.
//
// WHY DROPPING, NOT FAILING THE RESULT. This decision is load-bearing, so it is
// written down rather than left to the shape of the code.
//
// The terminal projection is the path that RECORDS THE ASSISTANT'S ANSWER: the
// same transaction that would perform this write-back is the one that inserts
// the response's text item and clears `is_streaming`
// (repos/agent_execution_results.go, persistCurrentAgentTerminal). It runs once
// per settled terminal frame. Refusing the whole result over a malformed
// attachment entry would therefore not "reject a bad write-back" -- it would
// throw away the answer the user is already watching stream, permanently, and
// leave the message group streaming forever.
//
// The write-back, by contrast, is a CACHE of work that has already been done
// and already been used: the worker read the file this turn and spliced the
// text into the human message before it called the model, so the model saw it.
// Skipping the persistence degrades to exactly the pre-#607 behaviour -- a
// later turn sees the filename instead of the contents, and a turn that
// re-attaches re-pays the extraction. That is a smaller loss than the answer,
// by a wide margin, and it is recoverable (the next turn carrying the file
// extracts it again); a lost answer is not.
//
// It is also the rule ALREADY CHOSEN on the other side of the seam: the worker
// measures this list against MAX_ATTACHMENT_CONTENT_WRITEBACK_BYTES and drops
// whole entries that do not fit rather than failing its turn
// (agents/attachments.py, cited by the proto). Failing here would mean the two
// halves of one mechanism answer "what if it does not fit" differently.
//
// What dropping is NOT allowed to become is "write what we can and hope". Every
// refusal below drops a WHOLE entry, never part of one -- a partial chunk that
// still claims to be the file is worse than no chunk, because nothing
// downstream can tell the difference.
func AcceptedAgentExecutionAttachmentContents(
	entries []AgentExecutionAttachmentContent,
) []AgentExecutionAttachmentContent {
	if len(entries) == 0 {
		return nil
	}
	// The aggregate ceiling is measured over the RAW list, before any entry is
	// dropped, and busts the WHOLE list rather than trimming it to fit.
	//
	// Trimming would be a choice about which attachment matters, made here,
	// with no information to make it with. And an over-size list is not a big
	// file -- the worker already refuses to send one (it drops entries that do
	// not fit) -- it is a frame that did not come from a conforming worker, so
	// nothing in it has earned a write.
	//
	// Only `Content` is counted: it is the value that reaches a column, and
	// item ids are 36 bytes of fixed-shape uuid whose count is already bounded
	// by maxCurrentTurnAttachments on the admission side.
	total := 0
	for _, entry := range entries {
		total += len(entry.Content)
		if total > MaxAgentExecutionAttachmentContentBytes {
			return nil
		}
	}
	// Duplicate ids drop EVERY entry carrying the id, not just the later ones.
	// The proto guarantees at most one entry per item id, so a duplicate means
	// the sender is broken or the frame is forged; keeping "the first" would be
	// picking one of two contents for one row by arrival order, which is the
	// coin flip `item_id` exists to prevent. Both are dropped; other entries in
	// the same frame are unaffected, because they are individually unambiguous.
	seen := make(map[string]int, len(entries))
	for _, entry := range entries {
		seen[entry.ItemID]++
	}
	accepted := make([]AgentExecutionAttachmentContent, 0, len(entries))
	for _, entry := range entries {
		if seen[entry.ItemID] != 1 || entry.Validate() != nil {
			continue
		}
		accepted = append(accepted, AgentExecutionAttachmentContent{
			ItemID:  entry.ItemID,
			Content: bytes.Clone(entry.Content),
		})
	}
	if len(accepted) == 0 {
		return nil
	}
	return accepted
}

// validateAgentExecutionAttachmentContents re-asserts the list invariant on the
// value the repository is actually handed, the way every other field of a
// result is re-checked in Validate() rather than trusted from its constructor.
//
// It is unreachable through the transport, which calls
// AcceptedAgentExecutionAttachmentContents and therefore cannot produce a
// violating slice. It exists so that a future caller that assembles the struct
// by hand -- a test, a replay tool, a second transport -- cannot quietly hand
// the projection something the drop rule would have refused.
func validateAgentExecutionAttachmentContents(entries []AgentExecutionAttachmentContent) error {
	if len(entries) == 0 {
		return nil
	}
	total := 0
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if err := entry.Validate(); err != nil {
			return err
		}
		if _, duplicate := seen[entry.ItemID]; duplicate {
			return ErrInvalidAgentExecutionOutput
		}
		seen[entry.ItemID] = struct{}{}
		total += len(entry.Content)
	}
	if total > MaxAgentExecutionAttachmentContentBytes {
		return ErrInvalidAgentExecutionOutput
	}
	return nil
}

// validAttachmentItemID demands a canonical lowercase uuid, the same rule
// application/agentexecution's validUUID applies to the id it MINTED for the
// row (start.go's currentTurnUUID). The two have to agree: this value is
// matched against `chat_message_items.uuid` in SQL, and a value that is not a
// uuid at all would reach Postgres as a `::uuid` cast that raises 22P02 and
// takes the whole terminal transaction -- and with it the answer -- down with
// it. Rejecting the shape here is what keeps that a dropped entry instead.
func validAttachmentItemID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value
}

// validAttachmentContentChunks demands one complete JSON ARRAY whose first
// element is an OBJECT.
//
// The array is the column's contract: the chat-history projection expands
// `chat_messages_attachment.content` with `jsonb_array_elements` and
// `jsonb_typeof(...) = 'array'` (queries/agent_chat.sql), and the legacy
// reader treats the value as a list of chunks (utils/chat_history.py:67-73). A
// stored object -- the shape the legacy `'{}'::json` column default leaves
// behind -- contributes nothing to a prompt, so writing one would be a silent
// erasure of the scaffold that IS there.
//
// The first element must be an object because that is the scaffold's header
// chunk, the one carrying the `elitea_attachment` marker. The marker is not
// decoration: it is what a later turn's worker reads to decide the file has
// already been extracted and must not be fetched again. This check cannot
// prove the marker survived byte-for-byte without parsing prose the worker
// owns, but it does refuse the shapes -- `[]`, `["text"]`, `[null]` -- in which
// it demonstrably did not.
//
// Beyond that the chunks are NOT inspected. Their content is the worker's and
// the model's contract; reshaping or filtering them here would make what the
// projection stores disagree with what the transcript renders, which is the
// same reason the SQL side declines to validate them.
func validAttachmentContentChunks(content json.RawMessage) bool {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) < 2 || trimmed[0] != '[' || trimmed[len(trimmed)-1] != ']' ||
		!json.Valid(trimmed) {
		return false
	}
	var chunks []json.RawMessage
	if err := json.Unmarshal(trimmed, &chunks); err != nil || len(chunks) == 0 {
		return false
	}
	first := bytes.TrimSpace(chunks[0])
	return len(first) >= 2 && first[0] == '{' && first[len(first)-1] == '}'
}
