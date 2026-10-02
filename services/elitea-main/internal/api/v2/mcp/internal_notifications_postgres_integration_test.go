package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	notificationsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/notifications"
	dbrepos "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

func TestInternalNotificationLifecyclePreservesGlobalUserScopeAndIdempotentSeen(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	ctx := context.Background()
	var targetID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO centry.notifications
			(uuid, is_seen, project_id, user_id, meta, event_type, created_at)
		VALUES
			('20000000-0000-4000-8000-000000000001', FALSE, 8, 73,
			 '{"message":"Budget 100%_safe indexing completed"}', 'budget_threshold', '2026-09-04 10:00:00.123456')
		RETURNING id`).Scan(&targetID); err != nil {
		t.Fatalf("seed target notification: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO centry.notifications
			(uuid, is_seen, project_id, user_id, meta, event_type, created_at)
		VALUES
			('20000000-0000-4000-8000-000000000002', FALSE, 1, 73,
			 '{"message":"Pipeline completed"}', 'pipeline_completed', '2026-09-04 10:00:01'),
			('20000000-0000-4000-8000-000000000003', FALSE, 1, 74,
			 '{"message":"Other user activity"}', 'budget_threshold', '2026-09-04 10:00:02')`); err != nil {
		t.Fatalf("seed notification isolation rows: %v", err)
	}

	repository, err := dbrepos.NewCurrentNotificationRepository(pool)
	if err != nil {
		t.Fatalf("compose notification repository: %v", err)
	}
	executor := newHandlerInternalNotificationExecutor(
		notificationsapi.NewCurrentNotificationToolHandler(repository),
	)

	listed, err := executor.Execute(ctx, 1, 73, internalListNotifications, map[string]any{
		"only_new": true, "search": "Budget 100%_safe", "event_type": "budget_threshold",
		"sort_by": "created_at", "sort_order": "desc", "limit": json.Number("10"),
	})
	if err != nil || listed.status != http.StatusOK || !bytes.Contains(listed.body, []byte(`"total":1`)) ||
		!bytes.Contains(listed.body, []byte(`"project_id":8`)) ||
		bytes.Contains(listed.body, []byte("Other user activity")) {
		t.Fatalf("list notifications: status=%d error=%v body=%s", listed.status, err, listed.body)
	}

	total, err := executor.Execute(ctx, 1, 73, internalListNotifications, map[string]any{
		"only_new": true, "only_total": true,
	})
	if err != nil || total.status != http.StatusOK || string(total.body) != `{"total":2}` {
		t.Fatalf("count notifications: status=%d error=%v body=%s", total.status, err, total.body)
	}

	detail, err := executor.Execute(ctx, 1, 73, internalGetNotification, map[string]any{
		"notification_id": targetID,
	})
	if err != nil || detail.status != http.StatusOK || !bytes.Contains(detail.body, []byte("Budget 100%_safe")) {
		t.Fatalf("get notification: status=%d error=%v body=%s", detail.status, err, detail.body)
	}
	foreign, err := executor.Execute(ctx, 1, 74, internalGetNotification, map[string]any{
		"notification_id": targetID,
	})
	if err != nil || foreign.status != http.StatusBadRequest || bytes.Contains(foreign.body, []byte("Budget")) {
		t.Fatalf("cross-user get: status=%d error=%v body=%s", foreign.status, err, foreign.body)
	}

	first, err := executor.Execute(ctx, 1, 73, internalMarkNotification, map[string]any{
		"notification_id": targetID,
	})
	if err != nil || first.status != http.StatusOK {
		t.Fatalf("mark notification: status=%d error=%v body=%s", first.status, err, first.body)
	}
	second, err := executor.Execute(ctx, 1, 73, internalMarkNotification, map[string]any{
		"notification_id": targetID,
	})
	if err != nil || second.status != http.StatusOK || !bytes.Equal(first.body, second.body) {
		t.Fatalf("idempotent mark: first=%s second=%s error=%v", first.body, second.body, err)
	}
	var storedSeen bool
	if err := pool.QueryRow(ctx, `SELECT is_seen FROM centry.notifications WHERE id = $1`, targetID).
		Scan(&storedSeen); err != nil || !storedSeen {
		t.Fatalf("stored seen=%v error=%v", storedSeen, err)
	}
}
