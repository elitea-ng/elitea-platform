package main

import (
	"context"
	"log/slog"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/conversationactivity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/events"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// conversationActivityBus publishes conversation activity frames (client
// contract 1.12) onto the conversations family of the live-update plane
// (events.ConversationsChannel), each naming its audience for the SSE relay
// to filter on and strip.
type conversationActivityBus struct {
	publisher *events.Publisher
}

func (b conversationActivityBus) Publish(ctx context.Context, projectID int64, audience conversationactivity.Audience, frame conversationactivity.Frame) error {
	b.publisher.Emit(ctx, strconv.FormatInt(projectID, 10), conversationactivity.EventType, struct {
		conversationactivity.Frame
		Audience        []int64 `json:"audience,omitempty"`
		AudienceProject bool    `json:"audience_project,omitempty"`
	}{Frame: frame, Audience: audience.Users, AudienceProject: audience.Project})
	return nil
}

// newConversationActivity answers nil (no frames) without a live-update bus
// or a database. The list rows still carry the activity; clients without
// frames refresh their list on focus.
func newConversationActivity(bus events.Bus, pool *pgxpool.Pool, logger *slog.Logger) *conversationactivity.Notifier {
	if bus == nil || pool == nil {
		return nil
	}
	return conversationactivity.New(dbrepos.NewConversationActivityRepo(pool),
		conversationActivityBus{publisher: events.NewConversationsPublisher(bus)}, logger)
}
