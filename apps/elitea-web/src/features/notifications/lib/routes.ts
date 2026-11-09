/**
 * features/notifications/lib/routes.ts — port of
 * `apps/elitea-ui/src/[fsd]/entities/notifications/lib/helpers/
 * notification.helpers.js`'s `resolveHref` and `parseMessage` (unit A11).
 *
 * ROUTING MODEL, verified against the new app's router (unit R1,
 * `src/routes/_shell/**`), NOT guessed: unlike the baseline's routes.js
 * table, none of `/chat`, `/toolkits/:tab/:toolkitId`, `/artifacts`,
 * `/agents/:tab/:agentId/:version` carry a `:projectId` path SEGMENT in
 * either app — the baseline's own `RouteDefinitions.Chat` etc. are already
 * project-agnostic strings (`routes.js:6,34,66`). What `resolveHref`
 * deliberately PREPENDS is a `/${projectId}/` segment that does not belong
 * to any registered route at all — it exists ONLY so the click is swallowed
 * by ROUTE-070 (`/:projectId/*` -> `ProjectSwitcher`, ported unchanged by
 * unit R3 as `src/routes/$projectId.$.tsx`'s `$projectId/$` splat), which
 * sets the app's selected-project state to `meta.project_id` / the
 * notification's own `project_id` (NOT necessarily the CURRENTLY selected
 * project — a notification can reference an entity in a project the user
 * is a member of but not currently viewing) and then does a hard
 * `window.location.replace()` with that segment stripped, landing on the
 * real, unprefixed route. This is load-bearing, not an old-app quirk to
 * "fix": every case below except `personal_access_token_expiring`
 * (settings is not project-scoped, `notification.helpers.js:14-15`)
 * reproduces the `/${projectId}/...` prefix verbatim for exactly this
 * reason. `PROJECT_ID_URL_PREFIX` + `replacePathParams` are `shared/lib/
 * url.ts`'s (unit S3) exports, built and deliberately left unused there —
 * its own doc comment: "Re-deriving link-building on the new route tree is
 * R1's job, not a shared/lib string utility; flagged in the S3 report" —
 * this is that follow-up.
 *
 * `resolveHref`'s third argument in the baseline is the NOTIFICATION's own
 * `project_id` (`NotificationListItemMessage.jsx:14`:
 * `resolveHref(notification.event_type, notification.meta,
 * notification.project_id)`), not the app's globally-selected project —
 * preserved as `notification.projectId` below, for the same reason.
 */
import { PROJECT_ID_URL_PREFIX, replacePathParams } from '@/shared/lib/url';
import { getConfig } from '@/shared/config';

import type { NotificationEventType } from '@/entities/notification';

import type { FullNotificationMeta } from '../api/normalize';

/* ── route path templates (evidence: src/routes/_shell/**, unit R1) ──────── */

const CHAT_PATH = '/chat';
const ARTIFACTS_PATH = '/artifacts';
const SETTINGS_TOKENS_PATH = '/settings/tokens';
const TOOLKIT_DETAIL_TEMPLATE = `${PROJECT_ID_URL_PREFIX}/toolkits/:tab/:toolkitId`;
const AGENT_VERSION_TEMPLATE = `${PROJECT_ID_URL_PREFIX}/agents/:tab/:agentId/:version`;

/** Search-param key literals (`common/constants.js` `SearchParams`, confirmed against `src/routes/-search/common.ts`'s known-key list: `conversation`, `message_id`, `index_name`, `bucket`). */
const SP_CONVERSATION = 'conversation';
const SP_MESSAGE_ID = 'message_id';
const SP_INDEX_NAME = 'index_name';
const SP_BUCKET = 'bucket';

/**
 * `apps/elitea-ui/src/routes.js:129-131` ported locally (a 3-line function,
 * duplicated rather than imported — `app/providers/basename.ts` (unit R2)
 * is in the `app/` layer, upward of `features/`, R-L1). Same pattern unit
 * R1 used locally in `src/routes/$projectId.$.tsx`'s own `getBasename()`.
 */
function basename(): string {
  if (import.meta.env.DEV) return '';
  const result = getConfig();
  return result.status === 'ok' ? result.config.vite_base_uri : '';
}

function absoluteBase(): string {
  return `${window.location.protocol}//${window.location.host}${basename()}`;
}

type HrefResolver = (base: string, meta: FullNotificationMeta | undefined, projectId: string | undefined) => string | null;

function tokenHref(base: string): string | null {
  return `${base}${SETTINGS_TOKENS_PATH}`;
}

function chatHref(base: string, meta: FullNotificationMeta | undefined, projectId: string | undefined): string | null {
  const convId = meta?.conversationId;
  const messageId = meta?.messageId;
  const route = `${base}/${projectId}${CHAT_PATH}`;
  if (convId === undefined) return route;
  const url = `${route}?${SP_CONVERSATION}=${convId}`;
  return messageId === undefined ? url : `${url}&${SP_MESSAGE_ID}=${messageId}`;
}

function indexHref(base: string, meta: FullNotificationMeta | undefined, projectId: string | undefined): string | null {
  const toolkitId = meta?.toolkitId;
  const indexName = meta?.indexName;
  if (toolkitId === undefined) return null;
  const route = `${base}${replacePathParams(TOOLKIT_DETAIL_TEMPLATE, { projectId: projectId ?? '', tab: 'indexes', toolkitId })}`;
  return indexName === undefined ? route : `${route}?${SP_INDEX_NAME}=${encodeURIComponent(indexName)}`;
}

function bucketHref(base: string, meta: FullNotificationMeta | undefined, projectId: string | undefined): string | null {
  const bucketName = meta?.bucketName;
  const route = `${base}/${projectId}${ARTIFACTS_PATH}`;
  return bucketName === undefined ? route : `${route}?${SP_BUCKET}=${encodeURIComponent(bucketName)}`;
}

/**
 * `notification.helpers.js:41-49` parity: `if (appId && versionId)` and
 * `meta?.project_id || projectId` are truthy/`||` checks, not
 * `undefined`-only ones — an empty string fails both. `FullNotificationMeta`
 * types every one of these fields `string | undefined`, so `''` is the only
 * falsy value besides `undefined` a field can actually hold; these explicit
 * `=== ''` checks alongside the `=== undefined` ones are therefore the full,
 * faithful port of that truthy semantics (not a partial substitute) under
 * this unit's otherwise `??`-preferring style. Getting this wrong silently
 * turns an empty-string `meta.project_id`/`source_application_id`/
 * `source_version_id` (baseline: "absent, use the fallback / return null")
 * into "present", producing a broken href instead of the baseline's
 * null/fallback.
 */
function agentHref(base: string, meta: FullNotificationMeta | undefined, projectId: string | undefined): string | null {
  const appId = meta?.sourceApplicationId;
  const versionId = meta?.sourceVersionId;
  const metaProjectId = meta?.projectId;
  const agentProjectId = metaProjectId !== undefined && metaProjectId !== '' ? metaProjectId : projectId;
  if (appId === undefined || appId === '' || versionId === undefined || versionId === '') return null;
  return buildAgentVersionHref(base, agentProjectId, appId, versionId);
}

function noHref(): null {
  return null;
}

/**
 * One entry per {@link NotificationEventType} member — a `Record` literal
 * over that union forces TypeScript to reject a missing key at compile
 * time, which is the exhaustiveness guarantee `notification.helpers.js`'s
 * `switch`'s `default: return null` gave dynamically. Every key not listed
 * in `notification.helpers.js:10-54` resolves to {@link noHref}, matching
 * that `default` branch exactly.
 */
const HREF_RESOLVERS: Record<NotificationEventType, HrefResolver> = {
  personal_access_token_expiring: tokenHref,
  chat_user_added: chatHref,
  chat_user_mentioned: chatHref,
  index_data_changed: indexHref,
  bucket_expiration_warning: bucketHref,
  agent_unpublished: agentHref,
  moderator_unpublish: noHref,
  author_approval: noHref,
  author_reject: noHref,
  moderator_approval_of_version: noHref,
  moderator_reject_of_version: noHref,
  token_expiring: noHref,
  token_is_expired: noHref,
  spending_limit_expiring: noHref,
  spending_limit_is_expired: noHref,
  rates: noHref,
  comments: noHref,
  reward_new_level: noHref,
  contributor_request_for_publish_approve: noHref,
  user_was_added_to_some_project_as_teammate: noHref,
  private_project_created: noHref,
  moderation_rejected: noHref,
  moderation_approved: noHref,
  // Rendered by `../ui/RunFinishedMessage.tsx`, which opens its own target (`./runFinished.ts`).
  run_finished: noHref,
  agent_heartbeat: chatHref,
};

/**
 * Port of `notification.helpers.js:10-54`'s `resolveHref`. Resolves the
 * navigation href for an empty-href link segment (`[text]()`) based on the
 * notification's `event_type` and `meta`. Returns `null` when the event
 * type has no resolvable link (verbatim — `default: return null`), or when
 * `eventType` is a runtime value outside the known union (`../api/
 * normalize.ts` preserves an unrecognised wire `event_type` verbatim rather
 * than dropping it — `HREF_RESOLVERS[eventType]` would then be `undefined`,
 * so the `?? noHref` fallback is load-bearing, not defensive dead code).
 */
export function resolveNotificationHref(
  eventType: NotificationEventType,
  meta: FullNotificationMeta | undefined,
  projectId: string | undefined,
): string | null {
  const resolver = HREF_RESOLVERS[eventType] ?? noHref;
  return resolver(absoluteBase(), meta, projectId);
}

/**
 * `/${projectId}/agents/all/${appId}/${versionId}?viewMode=owner` —
 * factored out because two DIFFERENT baseline call sites build this exact
 * shape with two DIFFERENT `projectId` sourcing rules:
 * `notification.helpers.js:41-49`'s `resolveHref` (used above, prefers
 * `meta.project_id` over the notification's own `project_id`) and
 * `LegacyNotificationMessage.jsx:96-99` (`../ui/LegacyNotificationMessage.tsx`,
 * always uses the notification's own `project_id`, never `meta.project_id`
 * — the two diverge on purpose in the baseline and this port preserves
 * that divergence rather than silently unifying them).
 */
export function buildAgentVersionHref(
  base: string,
  projectId: string | undefined,
  appId: string,
  versionId: string,
): string {
  const path = replacePathParams(AGENT_VERSION_TEMPLATE, {
    projectId: projectId ?? '',
    tab: 'all',
    agentId: appId,
    version: versionId,
  });
  return `${base}${path}?viewMode=owner`;
}

/** @internal exposed only for {@link buildAgentVersionHref}'s external callers (`LegacyNotificationMessage.tsx`) that need the same absolute base `resolveNotificationHref` builds internally. */
export function notificationBaseUrl(): string {
  return absoluteBase();
}

/* ── message segment parsing (verbatim port, notification.helpers.js:61-78) ── */

export interface NotificationMessageSegment {
  readonly text: string;
  readonly isLink?: boolean;
}

/**
 * Parses a stored notification message into renderable segments. Link
 * syntax: `[visible text]()` — empty href, the URL is resolved at render
 * time by {@link resolveNotificationHref}.
 */
export function parseNotificationMessage(message: string | undefined): NotificationMessageSegment[] {
  if (message === undefined || message === '') return [];
  const segments: NotificationMessageSegment[] = [];
  const linkRegex = /\[([^\]]+)\]\([^)]*\)/g;
  let lastIndex = 0;
  let match: RegExpExecArray | null;
  while ((match = linkRegex.exec(message)) !== null) {
    if (match.index > lastIndex) {
      segments.push({ text: message.slice(lastIndex, match.index) });
    }
    segments.push({ text: match[1] ?? '', isLink: true });
    lastIndex = match.index + match[0].length;
  }
  if (lastIndex < message.length) {
    segments.push({ text: message.slice(lastIndex) });
  }
  return segments.length > 0 ? segments : [{ text: message }];
}
