/**
 * features/notifications/api/normalize.ts — wire (snake_case) -> domain
 * (camelCase) mapping for the `entities/notification` type (unit A11).
 *
 * `entities/notification`'s `NotificationMeta` (unit E1) is intentionally
 * narrow: it captures only the fields `notification.helpers.js`'s
 * `resolveHref`/`parseMessage` (the `meta.message`-driven "new" rendering
 * path) reads. `notificationLegacy.helpers.js`'s `parseInformation` (the
 * pre-backfill fallback, `../lib/legacyText.ts`) reads a WIDER set of
 * `meta` fields that `NotificationMeta` does not declare. Rather than widen
 * E1's owned type (out of this unit's ownership fence — E1 is closed), this
 * module's `NormalizedNotification`/`FullNotificationMeta` are a strict
 * STRUCTURAL SUPERSET of `Notification`/`NotificationMeta`: every field
 * `NotificationMeta` declares is present with the same optionality, so a
 * `NormalizedNotification` is assignable anywhere a plain `Notification` is
 * expected (e.g. `entities/notification`'s selectors), while
 * `../lib/legacyText.ts` and `../ui/LegacyNotificationMessage.tsx` can read
 * the extra legacy-only fields off the SAME object with no second fetch or
 * parallel wire type.
 */
import type { Notification, NotificationEventType } from '@/entities/notification';

import type { NotificationMetaWire, NotificationWire } from './notifications';

/**
 * Extra `meta` fields `notificationLegacy.helpers.js:50-230`'s `parseInformation`
 * reads that `entities/notification`'s `NotificationMeta` does not declare
 * (see module doc comment). Not exported: only `FullNotificationMeta` (its
 * extender, below) is consumed outside this file.
 */
interface LegacyNotificationMeta {
  readonly tokenName?: string;
  readonly ratesCount?: number;
  readonly commentsCount?: number;
  readonly repliesCount?: number;
  readonly promptName?: string;
  readonly promptId?: string;
  readonly promptVersionId?: string;
  readonly newLevel?: string | number;
  readonly authorName?: string;
  readonly users?: readonly string[];
  readonly projectName?: string;
  readonly initiatorName?: string;
  readonly conversationName?: string;
  readonly indexed?: number;
  readonly updated?: number;
  readonly reindex?: boolean;
  readonly initiator?: string;
}

/** `run_finished` meta (ADR-0045 decision 5): how a chain of runs ended, and where it ran. */
interface RunFinishedMeta {
  readonly outcome?: string;
  readonly limit?: string;
  readonly conversationUuid?: string;
  readonly threadId?: string;
  readonly origin?: string;
  readonly continuations?: number;
}

/** Structural superset of `entities/notification`'s `NotificationMeta` — see module doc comment. */
export interface FullNotificationMeta extends LegacyNotificationMeta, RunFinishedMeta {
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

/** Structural superset of `entities/notification`'s `Notification` — see module doc comment. */
export interface NormalizedNotification extends Omit<Notification, 'meta'> {
  readonly meta?: FullNotificationMeta;
}

/**
 * `entities/notification`'s `NotificationEventType` is a closed union
 * (verbatim from `constants.js:997-1021`; `getIcon.helpers.jsx` references
 * several additional strings that are NOT in that enum — E1's own doc
 * comment flags them as dead code, deliberately excluded). A wire
 * `event_type` outside the known set is preserved as-is rather than
 * dropped or defaulted: every consumer in this slice (`NotificationIcon`,
 * `../lib/routes.ts`, `../lib/legacyText.ts`) already falls through an
 * exhaustive `switch`'s `default` branch for an unrecognised value, so an
 * unknown event type degrades to "no icon / no link / no legacy text"
 * rather than crashing — matching `getIcon.helpers.jsx`'s own
 * `default: return null`.
 */
function toEventType(raw: string): NotificationEventType {
  return raw as NotificationEventType;
}

/**
 * `exactOptionalPropertyTypes` (spec §2.1) forbids assigning `undefined` to
 * an optional (`key?: T`, not `key?: T | undefined`) property — every field
 * below is present-or-absent on the wire, not present-with-`undefined`, so
 * building the object with every key unconditionally and stripping the
 * undefined ones afterward (rather than one individual conditional spreads)
 * is the pragmatic middle ground `shared/api/generated/mutator.ts` uses at
 * smaller scale (`...(options.signal ? { signal: options.signal } : {})`).
 * The cast is safe: `Object.entries`/`Object.fromEntries` round-trip loses
 * no keys and `FullNotificationMeta` declares every key optional, so a
 * subset object is always a valid value of the type.
 */
function normalizeMeta(meta: NotificationMetaWire | undefined): FullNotificationMeta | undefined {
  if (meta === undefined) return undefined;
  const mapped: Record<string, unknown> = {
    message: meta.message,
    conversationId: meta.conversation_id,
    messageId: meta.message_id,
    toolkitId: meta.toolkit_id,
    indexName: meta.index_name,
    bucketName: meta.bucket_name,
    sourceApplicationId: meta.source_application_id,
    sourceVersionId: meta.source_version_id,
    projectId: meta.project_id,
    reason: meta.reason,
    error: meta.error,
    tokenName: meta.token_name,
    ratesCount: meta.rates_count,
    commentsCount: meta.comments_count,
    repliesCount: meta.replies_count,
    promptName: meta.prompt_name,
    promptId: meta.prompt_id,
    promptVersionId: meta.prompt_version_id,
    newLevel: meta.new_level,
    authorName: meta.author_name,
    users: meta.users,
    projectName: meta.project_name,
    initiatorName: meta.initiator_name,
    conversationName: meta.conversation_name,
    indexed: meta.indexed,
    updated: meta.updated,
    reindex: meta.reindex,
    initiator: meta.initiator,
    outcome: meta.outcome,
    limit: meta.limit,
    conversationUuid: meta.conversation_uuid,
    threadId: meta.thread_id,
    origin: meta.origin,
    continuations: meta.continuations,
  };
  return Object.fromEntries(Object.entries(mapped).filter(([, value]) => value !== undefined));
}

/** Wire -> domain. `id`/`project_id` are coerced to `string` (E1 types both as `string`; the Go backend uses numeric primary keys). */
export function normalizeNotification(wire: NotificationWire): NormalizedNotification {
  const meta = normalizeMeta(wire.meta);
  return {
    id: String(wire.id),
    eventType: toEventType(wire.event_type),
    createdAt: wire.created_at,
    isSeen: wire.is_seen,
    ...(meta !== undefined && { meta }),
    ...(wire.project_id !== undefined && { projectId: String(wire.project_id) }),
  };
}

export function normalizeNotificationList(wires: readonly NotificationWire[]): NormalizedNotification[] {
  return wires.map(normalizeNotification);
}
