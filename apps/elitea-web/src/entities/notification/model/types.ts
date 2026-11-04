/**
 * Notification domain type. No OpenAPI schema exists for this resource;
 * shape derived entirely from old-app evidence:
 *
 * - apps/elitea-ui/src/api/notifications.js:13-58 — `notificationList` (GET
 *   `/notifications/notifications/prompt_lib/{projectId}`, response
 *   `{ rows, total }`), notificationRead/Delete/BulkDelete/BulkMarkSeen.
 * - apps/elitea-ui/src/common/constants.js:997-1021 — `NotificationType` enum.
 * - apps/elitea-ui/src/[fsd]/entities/notifications/lib/helpers/
 *   getIcon.helpers.jsx:11-104 — event_type (+ `meta.error` for one case) ->
 *   icon/variant mapping; no stored `severity` field exists.
 * - apps/elitea-ui/src/[fsd]/widgets/Notifications/ui/NotificationListItem.jsx
 *   :42,51,70,80,94,105,111 — field usage (`id`, `event_type`, `meta`,
 *   `created_at`, `is_seen`).
 *
 * Socket event `notifications_notify` (constants.js:898) carries NO payload
 * — it only signals a refetch (NotificationButton.jsx:39-43); it does not
 * widen this type.
 */

/**
 * `NotificationType` (constants.js:997-1021). Verbatim — do not add cases
 * the enum does not have; `getIcon.helpers.jsx` references several legacy
 * strings (PromptModeratorApproval etc.) NOT in this enum, flagged by the
 * research agent as likely dead code, deliberately excluded here.
 */
export type NotificationEventType =
  | 'moderator_unpublish'
  | 'author_approval'
  | 'author_reject'
  | 'moderator_approval_of_version'
  | 'moderator_reject_of_version'
  | 'token_expiring'
  | 'token_is_expired'
  | 'spending_limit_expiring'
  | 'spending_limit_is_expired'
  | 'rates'
  | 'comments'
  | 'reward_new_level'
  | 'contributor_request_for_publish_approve'
  | 'user_was_added_to_some_project_as_teammate'
  | 'chat_user_added'
  | 'chat_user_mentioned'
  | 'private_project_created'
  | 'index_data_changed'
  | 'bucket_expiration_warning'
  | 'agent_unpublished'
  | 'personal_access_token_expiring'
  | 'moderation_rejected'
  | 'moderation_approved'
  /** ADR-0045 decision 5: a chain of runs ended. The meta is content-free (outcome and ids only). */
  | 'run_finished';

/**
 * Freeform per-event-type payload (`notification.helpers.js:10-54`,
 * `notificationLegacy.helpers.js:50-230`). No single event type uses every
 * key; all are optional.
 */
export interface NotificationMeta {
  readonly message?: string;
  readonly conversationId?: string;
  readonly messageId?: string;
  readonly toolkitId?: string;
  readonly indexName?: string;
  readonly bucketName?: string;
  readonly sourceApplicationId?: string;
  readonly sourceVersionId?: string;
  readonly projectId?: string;
  readonly reason?: string;
  readonly error?: unknown;
}

export interface Notification {
  readonly id: string;
  readonly eventType: NotificationEventType;
  readonly meta?: NotificationMeta;
  readonly createdAt: string;
  readonly isSeen: boolean;
  readonly projectId?: string;
}

export interface NotificationPage {
  readonly rows: readonly Notification[];
  readonly total: number;
}
