package folders

import (
	"context"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/chatactivity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/chatauthority"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// activityWire is a row's `activity` (client contract 1.12): the state and
// when the conversation entered it. The state is a string, not an enum.
type activityWire struct {
	State string `json:"state"`
	Since string `json:"since"`
}

// annotateActivity fills `activity`, `last_activity_at` and `unread` on the
// rows of the given pages, in one statement for all of them
// (chatactivity.Load). Only the rows the response carries are read: the
// listing holds every visible conversation of the project in memory, but a
// bucket sends one page of it. The pages share their backing arrays with
// the listing, so the rows are filled in place.
func (h *Handler) annotateActivity(ctx context.Context, projectID string, pages ...[]conversationItem) error {
	if h.pool == nil {
		return nil
	}
	ids := []int64{}
	for _, page := range pages {
		for _, item := range page {
			ids = append(ids, int64(item.ID))
		}
	}
	if len(ids) == 0 {
		return nil
	}
	schema, err := tenantschema.Quote(projectID)
	if err != nil {
		return err
	}
	reader, _ := chatauthority.Actor(ctx)
	activity, err := chatactivity.Load(ctx, h.pool, schema, reader, ids)
	if err != nil {
		return err
	}
	for _, page := range pages {
		for i := range page {
			a, ok := activity[int64(page[i].ID)]
			if !ok {
				continue
			}
			page[i].Activity = &activityWire{State: a.State, Since: chatactivity.FormatTime(a.Since)}
			page[i].LastActivityAt = chatactivity.FormatTime(a.LastActivityAt)
			page[i].Unread = a.Unread
		}
	}
	return nil
}

// pausedSet answers which of the listing's conversations wait for the user
// (chatactivity.Paused), so each bucket's `needs_you` counts its whole
// bucket and not only its first page. A nil pool answers an empty set.
func (h *Handler) pausedSet(ctx context.Context, projectID string, all []conversationItem) (map[int64]bool, error) {
	if h.pool == nil || len(all) == 0 {
		return map[int64]bool{}, nil
	}
	schema, err := tenantschema.Quote(projectID)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(all))
	for _, item := range all {
		ids = append(ids, int64(item.ID))
	}
	return chatactivity.Paused(ctx, h.pool, schema, ids)
}

// countNeedsYou is how many of items wait for the user.
func countNeedsYou(items []conversationItem, paused map[int64]bool) int {
	n := 0
	for _, item := range items {
		if paused[int64(item.ID)] {
			n++
		}
	}
	return n
}
