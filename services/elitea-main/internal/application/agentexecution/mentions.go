package agentexecution

import (
	"context"
	"log/slog"
)

// @MENTION NOTIFICATIONS (#977).
//
// The composer has always resolved an `@` mention into a user list and put it
// on the wire. Nothing on this side read it: the start route declared
// `user_ids` only to REFUSE a request that carried one, the client's own
// spelling (`userIds`) was not bound at all, and no producer anywhere wrote a
// mention row — so a person tagged a colleague, watched the tag render, and
// the colleague was never told. The route now parses the list; this file is
// the half that tells somebody.
//
// # THE SHAPE IS THE PAT-EXPIRY PRODUCER'S, DELIBERATELY
//
// One `centry.notifications` row per recipient, with `event_type` and a `meta`
// blob the web already knows how to render:
// `features/notifications/lib/routes.ts` maps `chat_user_mentioned` through
// `chatHref` to `/{project}/chat?conversation=…`, and `legacyText.ts` carries
// its copy. As with `personal_access_token_expiring`, the RENDERING was never
// the missing half — the producer was. Adding a new event type here would have
// meant a row the client shows as a blank line.
//
// # WHO IS NOTIFIED, AND WHO IS NOT
//
//   - every DISTINCT user named, once per MESSAGE. The count follows messages,
//     not tags: naming the same person twice in one message is one row, which
//     is what ELITEA-0396 asks and what the route's own deduplication already
//     guarantees before this code runs;
//   - never the SENDER. Tagging yourself is a normal thing to do in a sentence
//     addressed to a group, and a notification about your own message is
//     noise the reader cannot act on (ELITEA-0399);
//   - for `@everyone`, every member of the project as the SERVER resolves it —
//     not the id list the client sent alongside. That list is a client's view
//     of the membership; trusting it would let a stale or tampered client
//     notify somebody who is not in the project, which is the same class as
//     rule 1 of the pipeline-trigger auth story: nothing the caller sends
//     selects who is affected.
//
// # WHY IT IS BEST EFFORT
//
// The message is already admitted, stored and streaming by the time this runs.
// A notification that cannot be written must cost the MENTION and never the
// turn — the alternative is a send that fails because a secondary row could
// not be inserted, which is strictly worse than the pre-#977 behaviour this
// replaces.

// ChatMentionNotificationEventType is the event type the web already resolves
// to the conversation (`HREF_RESOLVERS.chat_user_mentioned`). Stated here
// rather than invented: a new string would render as a blank notification.
const ChatMentionNotificationEventType = "chat_user_mentioned"

// MentionNotification is one row to write.
type MentionNotification struct {
	ProjectID int64
	// UserID is the RECIPIENT — the person who was named.
	UserID int64
	// ConversationUUID and MessageID land in `meta` under the snake_case keys
	// `features/notifications/api/normalize.ts` reads (`conversation_id`,
	// `message_id`), so the notification links back to the exact message.
	ConversationUUID string
	MessageID        string
	// SenderUserID is who did the naming, for the copy.
	SenderUserID int64
}

// MentionNotificationWriter writes the rows. Implemented over
// `centry.notifications` by internal/infra/db/repos, the same table and the
// same insert shape the PAT-expiry sweep uses.
type MentionNotificationWriter interface {
	WriteChatMentionNotifications(ctx context.Context, rows []MentionNotification) error
	// ProjectMemberUserIDs answers `@everyone` from the SERVER's view of the
	// project. Separate from the write so the resolution can be asserted on
	// its own, and so a deployment without it simply notifies nobody for
	// `@everyone` rather than notifying the client's list.
	ProjectMemberUserIDs(ctx context.Context, projectID int64) ([]int64, error)
}

// WithMentionNotifications attaches the writer, in the same
// after-construction idiom WithMemories and WithProjectContext use: every
// existing constructor call site keeps working and a service nobody attaches
// it to writes no mention rows — the behaviour every test that predates this
// file expects.
func (service *CurrentApplicationStartService) WithMentionNotifications(
	writer MentionNotificationWriter,
) *CurrentApplicationStartService {
	service.mentions = writer
	return service
}

// mentionAudience answers WHO this request notifies, with the sender removed.
//
// Exported behaviour in one pure function so the rules above are testable
// without a database: it is the whole of the policy, and the writer below only
// persists what it returns.
func mentionAudience(
	request CurrentApplicationStartRequest,
	projectMembers []int64,
) []int64 {
	named := request.MentionedUserIDs
	if request.MentionsEveryone {
		named = projectMembers
	}
	audience := make([]int64, 0, len(named))
	seen := make(map[int64]struct{}, len(named))
	for _, id := range named {
		if id <= 0 || id == request.ActorUserID {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		audience = append(audience, id)
	}
	return audience
}

// notifyMentionedUsers writes one row per recipient. Best effort; see the
// header.
func (service *CurrentApplicationStartService) notifyMentionedUsers(
	ctx context.Context,
	request CurrentApplicationStartRequest,
) {
	if service.mentions == nil {
		return
	}
	if len(request.MentionedUserIDs) == 0 && !request.MentionsEveryone {
		return
	}

	members := []int64(nil)
	if request.MentionsEveryone {
		resolved, err := service.mentions.ProjectMemberUserIDs(ctx, request.ProjectID)
		if err != nil {
			slog.Error("agentexecution: resolve @everyone membership",
				"project_id", request.ProjectID, "err", err)
			return
		}
		members = resolved
	}

	audience := mentionAudience(request, members)
	if len(audience) == 0 {
		return
	}

	rows := make([]MentionNotification, 0, len(audience))
	for _, userID := range audience {
		rows = append(rows, MentionNotification{
			ProjectID:        request.ProjectID,
			UserID:           userID,
			ConversationUUID: request.ConversationUUID,
			MessageID:        request.QuestionID,
			SenderUserID:     request.ActorUserID,
		})
	}
	if err := service.mentions.WriteChatMentionNotifications(ctx, rows); err != nil {
		slog.Error("agentexecution: write mention notifications",
			"project_id", request.ProjectID, "recipients", len(rows), "err", err)
	}
}
