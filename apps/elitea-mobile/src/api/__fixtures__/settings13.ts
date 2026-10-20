/**
 * Client contract 1.3 settings shapes, CAPTURED 2026-10-06 from the zefir-wave
 * stack (elitea-platform client contract 1.3, the same document as the v4.7.0
 * pin) as the admin persona (user 3) in its personal project 90503, and, for
 * usage, as Alice (owner) and Bob (editor) in the team project 90504. The
 * bodies are verbatim; only the variable names are ours. The notification rows
 * were inserted for the capture (event types as the server writes them).
 */

export const SETTINGS_CAPTURE = { projectId: 90503, teamProjectId: 90504 } as const;

/** listNotifications `changes_since=0`: three rows, no tombstones. */
export const notificationsDeltaFull = {
  "total": 3,
  "rows": [
    {
      "id": 8,
      "uuid": "af37c5aa-c26e-4149-8a03-29dbed4fc62f",
      "is_seen": false,
      "project_id": 90503,
      "user_id": 3,
      "meta": {
        "source": "zefir-sq",
        "message": "Your project request was approved"
      },
      "created_at": "2026-10-06T23:37:01Z",
      "updated_at": "2026-10-06T23:37:01Z",
      "event_type": "moderation_approved"
    },
    {
      "id": 9,
      "uuid": "e2af3c81-a132-4188-a7a4-9217cac4fb1f",
      "is_seen": false,
      "project_id": 90503,
      "user_id": 3,
      "meta": {
        "source": "zefir-sq",
        "initiator_name": "Bob Zefir",
        "conversation_id": "35a5538d-993d-4c66-bf5f-ad8509c581d4"
      },
      "created_at": "2026-10-06T23:37:01Z",
      "updated_at": "2026-10-06T23:37:01Z",
      "event_type": "chat_user_mentioned"
    },
    {
      "id": 10,
      "uuid": "442eedb8-de48-4292-a65e-08311ae2e89a",
      "is_seen": true,
      "project_id": 90503,
      "user_id": 3,
      "meta": {
        "source": "zefir-sq"
      },
      "created_at": "2026-10-06T23:37:01Z",
      "updated_at": "2026-10-06T23:37:01Z",
      "event_type": "agent_finished"
    }
  ],
  "tombstones": [],
  "next_cursor": "eyJ2IjoxLCJzIjoibm90aWYiLCJzY29wZSI6IjMiLCJ0IjoiMjAyNi0xMC0wNlQyMzozNjo1Ni44Mzg1MTNaIiwiaSI6OTIyMzM3MjAzNjg1NDc3NTgwNywiZHQiOiIyMDI2LTEwLTA2VDIzOjM2OjU2LjgzODUxM1oiLCJkaSI6OTIyMzM3MjAzNjg1NDc3NTgwN30",
  "has_more": false
} as const;

/** listNotifications paged (`limit=30&sort_order=desc`). */
export const notificationsPage = {
  "total": 3,
  "rows": [
    {
      "id": 10,
      "uuid": "442eedb8-de48-4292-a65e-08311ae2e89a",
      "is_seen": true,
      "project_id": 90503,
      "user_id": 3,
      "meta": {
        "source": "zefir-sq"
      },
      "created_at": "2026-10-06T23:37:01Z",
      "updated_at": "2026-10-06T23:37:01Z",
      "event_type": "agent_finished"
    },
    {
      "id": 9,
      "uuid": "e2af3c81-a132-4188-a7a4-9217cac4fb1f",
      "is_seen": false,
      "project_id": 90503,
      "user_id": 3,
      "meta": {
        "source": "zefir-sq",
        "initiator_name": "Bob Zefir",
        "conversation_id": "35a5538d-993d-4c66-bf5f-ad8509c581d4"
      },
      "created_at": "2026-10-06T23:37:01Z",
      "updated_at": "2026-10-06T23:37:01Z",
      "event_type": "chat_user_mentioned"
    },
    {
      "id": 8,
      "uuid": "af37c5aa-c26e-4149-8a03-29dbed4fc62f",
      "is_seen": false,
      "project_id": 90503,
      "user_id": 3,
      "meta": {
        "source": "zefir-sq",
        "message": "Your project request was approved"
      },
      "created_at": "2026-10-06T23:37:01Z",
      "updated_at": "2026-10-06T23:37:01Z",
      "event_type": "moderation_approved"
    }
  ]
} as const;

/** deleteNotification of an id that is already gone: 400. */
export const notificationNotFound = {
  "ok": false,
  "error": "Notification is not found"
} as const;

/** deleteNotification of a non-numeric id: 400. */
export const notificationInvalidId = {
  "ok": false,
  "error": "Invalid notification"
} as const;

/** deleteNotifications `{ids: [9, 999999999]}`: the foreign id is skipped. */
export const notificationsBulkDeleted = {
  "deleted": 1
} as const;

/** listNotifications with the cursor of the full page, after two deletes: tombstones `deleted`. */
export const notificationsDeltaAfterDelete = {
  "total": 1,
  "rows": [
    {
      "id": 10,
      "uuid": "442eedb8-de48-4292-a65e-08311ae2e89a",
      "is_seen": true,
      "project_id": 90503,
      "user_id": 3,
      "meta": {
        "source": "zefir-sq"
      },
      "created_at": "2026-10-06T23:37:01Z",
      "updated_at": "2026-10-06T23:37:01Z",
      "event_type": "agent_finished"
    }
  ],
  "tombstones": [
    {
      "id": 8,
      "uuid": "af37c5aa-c26e-4149-8a03-29dbed4fc62f",
      "reason": "deleted",
      "deleted_at": "2026-10-06T23:37:01.85884Z"
    },
    {
      "id": 9,
      "uuid": "e2af3c81-a132-4188-a7a4-9217cac4fb1f",
      "reason": "deleted",
      "deleted_at": "2026-10-06T23:37:01.882039Z"
    }
  ],
  "next_cursor": "eyJ2IjoxLCJzIjoibm90aWYiLCJzY29wZSI6IjMiLCJ0IjoiMjAyNi0xMC0wNlQyMzozNjo1Ni44OTE5NloiLCJpIjo5MjIzMzcyMDM2ODU0Nzc1ODA3LCJkdCI6IjIwMjYtMTAtMDZUMjM6MzY6NTYuODkxOTZaIiwiZGkiOjkyMjMzNzIwMzY4NTQ3NzU4MDd9",
  "has_more": false
} as const;

/** listMemories: most recent first. */
export const memoriesList = {
  "items": [
    {
      "id": "2",
      "project_id": "90503",
      "content": "My team ships on Thursdays.",
      "tags": [],
      "enabled": true,
      "created_at": "2026-10-06T23:37:01.921215Z",
      "updated_at": "2026-10-06T23:37:01.921215Z"
    },
    {
      "id": "1",
      "project_id": "90503",
      "content": "I prefer answers in British English.",
      "tags": [
        "style"
      ],
      "enabled": true,
      "created_at": "2026-10-06T23:37:01.910148Z",
      "updated_at": "2026-10-06T23:37:01.910148Z"
    }
  ],
  "total": 2
} as const;

/** clearMemories after one of two was deleted. */
export const memoriesCleared = {
  "removed": 1
} as const;

/** listMemories with nothing saved. */
export const memoriesEmpty = {
  "items": [],
  "total": 0
} as const;

/** getProjectUsage scope=project as the project owner (unlimited, no ledger rows). */
export const usageProjectOwner = {
  "budget_period": "monthly",
  "can_see_amounts": true,
  "currency": "USD",
  "effective_limit": null,
  "enabled": false,
  "limit_source": "unlimited",
  "monthly_limit": null,
  "nats_fail_mode": null,
  "percent_used": null,
  "period": "202610",
  "period_end": "2026-10-31",
  "period_start": "2026-10-01",
  "project_id": 90504,
  "remaining": null,
  "resets_at": "2026-11-01T00:00:00Z",
  "scope": "project",
  "spend": 0,
  "spend_available": false,
  "usage_events_available": false,
  "user_id": null,
  "warning_active": false,
  "warning_pct": 80
} as const;

/** getProjectUsage scope=user as the project owner. */
export const usageUserOwner = {
  "can_see_amounts": true,
  "currency": "USD",
  "effective_limit": null,
  "enabled": false,
  "enforced": true,
  "limit_source": "unlimited",
  "monthly_limit": null,
  "percent_used": null,
  "period": "202610",
  "period_end": "2026-10-31",
  "period_start": "2026-10-01",
  "project_id": 90504,
  "remaining": null,
  "resets_at": "2026-11-01T00:00:00Z",
  "scope": "user",
  "spend": 0,
  "spend_available": false,
  "usage_events_available": false,
  "user_id": 8,
  "warning_active": false,
  "warning_pct": 80
} as const;

/** getProjectUsage scope=project as an editor: the money fields are REMOVED. */
export const usageProjectMember = {
  "budget_period": "monthly",
  "can_see_amounts": false,
  "enabled": false,
  "limit_source": "unlimited",
  "nats_fail_mode": null,
  "percent_used": null,
  "period": "202610",
  "period_end": "2026-10-31",
  "period_start": "2026-10-01",
  "project_id": 90504,
  "resets_at": "2026-11-01T00:00:00Z",
  "scope": "project",
  "spend_available": false,
  "usage_events_available": false,
  "user_id": null,
  "warning_active": false,
  "warning_pct": 80
} as const;

/** getProjectUsage scope=user as an editor: redacted too. */
export const usageUserMember = {
  "can_see_amounts": false,
  "enabled": false,
  "enforced": true,
  "limit_source": "unlimited",
  "percent_used": null,
  "period": "202610",
  "period_end": "2026-10-31",
  "period_start": "2026-10-01",
  "project_id": 90504,
  "resets_at": "2026-11-01T00:00:00Z",
  "scope": "user",
  "spend_available": false,
  "usage_events_available": false,
  "user_id": 10,
  "warning_active": false,
  "warning_pct": 80
} as const;

/** getProjectUsage scope=nope: 400. */
export const usageBadScope = {
  "error": "scope must be 'project' or 'user'"
} as const;
