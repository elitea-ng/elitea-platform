package conversations

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/chatactivity"
)

// annotateActivity adds `activity`, `last_activity_at` and `unread` (client
// contract 1.12) to conversation-list rows, for reader, in one statement for
// the whole page (chatactivity.Load). Unlike the archiver's name this is not
// an enrichment the row may lack: a failure fails the request, because a row
// that reads `idle` and read while a turn runs or an answer waits would be a
// wrong answer, not a degraded one.
//
// The fields are computed when the row is read, so a delta row carries the
// state at read time. A turn transition stamps the conversation's sync_at
// (tenant/0153), which is what brings the row back in the delta; a streamed
// token does not.
func annotateActivity(ctx context.Context, pool *pgxpool.Pool, s string, reader int64, rows []map[string]any) error {
	if len(rows) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		if id, ok := row["id"].(int); ok {
			ids = append(ids, int64(id))
		}
	}
	activity, err := chatactivity.Load(ctx, pool, s, reader, ids)
	if err != nil {
		return err
	}
	for _, row := range rows {
		id, ok := row["id"].(int)
		if !ok {
			continue
		}
		if a, found := activity[int64(id)]; found {
			a.Annotate(row)
		}
	}
	return nil
}
