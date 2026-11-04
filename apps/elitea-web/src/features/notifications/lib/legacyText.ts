/**
 * features/notifications/lib/legacyText.ts — port of
 * `apps/elitea-ui/src/[fsd]/entities/notifications/lib/helpers/
 * notificationLegacy.helpers.js` (unit A11).
 *
 * "Legacy helpers for pre-backfill notification rendering" — the source
 * file's own header. This path only activates when `notification.meta.message`
 * is absent (`NotificationListItemMessage.jsx:17`'s `if (message) {...}`
 * fallback), i.e. for notifications persisted before the backend started
 * storing a pre-formatted `meta.message` string. Ported in full for parity
 * (this is exactly the shape `LegacyNotificationMessage.jsx` reads), not
 * trimmed down.
 */
import type { NotificationEventType } from '@/entities/notification';

import type { FullNotificationMeta } from '../api/normalize';

const MAX_NAME_LEN = 33;

/** `notificationLegacy.helpers.js:8-20`. */
export function leadingText(param1: string, param2: string): Partial<Record<NotificationEventType, string>> {
  return {
    token_expiring: `Token ${param1} will be expired in 5 days. For more details view your `,
    token_is_expired: `Token ${param1} is expired! For more details view your `,
    spending_limit_expiring: 'Your spending limit is expiring. For more details view your ',
    spending_limit_is_expired: 'Your spending limit is expired. For more details view your ',
    reward_new_level: `Congratulations! You've got ${param1} level of prompt expert!`,
    user_was_added_to_some_project_as_teammate: `${param1} added into `,
    chat_user_added: `${param1} added ${param2} to `,
    private_project_created: 'Project was successfully created',
    index_data_changed: param1,
    bucket_expiration_warning: 'Bucket ',
    personal_access_token_expiring: `Your personal access token ${param1} will expire in 24 hours. After expiration, it will no longer work. You can delete and recreate a new token if needed. `,
  };
}

/** `notificationLegacy.helpers.js:22` — verbatim empty (no event type has middle text). */
export const middleText: Partial<Record<NotificationEventType, string>> = {};

/** `notificationLegacy.helpers.js:24-43`. */
export function endingText(param: string): Partial<Record<NotificationEventType, string>> {
  return {
    moderator_unpublish: ' is unpublished after complaint.',
    author_approval: ` is approved by ${param} for publishing.`,
    author_reject: ` is rejected by ${param}.`,
    moderator_approval_of_version: ' is published.',
    moderator_reject_of_version: ' is rejected.',
    token_expiring: '.',
    token_is_expired: '.',
    spending_limit_is_expired: '.',
    spending_limit_expiring: '.',
    rates: '.',
    comments: '.',
    user_was_added_to_some_project_as_teammate: '.',
    private_project_created: '.',
    index_data_changed: '',
    bucket_expiration_warning:
      " will start deleting files in 24 hours according to its retention policy (files are removed based on each file's creation date; the bucket itself will remain).",
    personal_access_token_expiring: '',
  };
}

/** `notificationLegacy.helpers.js:45-47`. */
export function formatName(name: string | undefined): string {
  return name !== undefined && name.length > MAX_NAME_LEN ? `${name.slice(0, MAX_NAME_LEN)}...` : (name ?? '');
}

/** `notificationLegacy.helpers.js:49-65`. */
export function formatIndexMessage(meta: FullNotificationMeta, withLink = false): string {
  const indexName = meta.indexName;
  const error = meta.error;
  const reindex = meta.reindex;
  const indexed = meta.indexed;
  const updated = meta.updated;
  const indexNamePlaceholder = withLink ? '{INDEX_LINK}' : (indexName ?? 'Index');
  const reindexedCount = updated ?? 0;

  if (typeof error === 'string' && error.trim() !== '') {
    return `Index ${indexNamePlaceholder} is failed.`;
  }

  if (reindex === true) {
    const isScheduled = meta.initiator === 'schedule';
    const scheduledText = isScheduled ? ' by schedule' : '';
    return `Index ${indexNamePlaceholder} is successfully reindexed${scheduledText}. { "reindexed": ${reindexedCount}, "indexed": ${indexed ?? 0} }`;
  }

  return `Index ${indexNamePlaceholder} is successfully created: { "indexed": ${indexed ?? 0} }`;
}

/**
 * A `[text]()`-style link target the legacy renderer resolves inline
 * (`notificationLegacy.helpers.js`'s inline `linkInfo` object literals).
 * Optional fields declare `| undefined` explicitly (not just `?:`) because
 * every value below flows straight from another already-optional source
 * (`FullNotificationMeta`'s fields) — under `exactOptionalPropertyTypes`
 * this is the sanctioned way to allow "explicitly no value" without a
 * conditional-spread at every one of the ~10 construction sites below; the
 * key/absent-vs-undefined distinction has no behavioural meaning for this
 * plain data-transfer type (unlike e.g. `shared/api/http.ts`'s
 * `HttpRequestOptions`, where it does).
 */
export interface LegacyLinkInfo {
  readonly linkText: string;
  readonly id?: string | undefined;
  readonly indexName?: string | undefined;
  readonly isNewTab?: boolean;
}

export interface ParsedLegacyInformation {
  readonly leadingTextParam1?: string | undefined;
  readonly leadingTextParam2?: string | undefined;
  readonly firstLinkInfo?: LegacyLinkInfo | undefined;
  readonly hasMiddleText?: boolean;
  readonly secondLinkInfo?: LegacyLinkInfo;
  readonly endingTextParam?: string;
  readonly agentUnpublishedMeta?: {
    readonly sourceVersionId?: string | undefined;
    readonly sourceApplicationId?: string | undefined;
    readonly projectId?: string | undefined;
    readonly reasonSuffix: string;
  };
}

type LegacyInfoParser = (meta: FullNotificationMeta, projectId: string | undefined) => ParsedLegacyInformation;

function parseAgentUnpublished(meta: FullNotificationMeta, projectId: string | undefined): ParsedLegacyInformation {
  const reasonSuffix = meta.reason !== undefined && meta.reason !== '' ? ` Reason: ${meta.reason}` : '';
  return {
    leadingTextParam1: '',
    leadingTextParam2: '',
    agentUnpublishedMeta: {
      sourceVersionId: meta.sourceVersionId,
      sourceApplicationId: meta.sourceApplicationId,
      projectId,
      reasonSuffix,
    },
    endingTextParam: '',
  };
}

/** `moderator_approval_of_version`/`moderator_reject_of_version`/`moderator_unpublish`/`author_approval`/`author_reject`/`token_expiring`/`token_is_expired`. */
function parseConfigurationLink(meta: FullNotificationMeta): ParsedLegacyInformation {
  return {
    leadingTextParam1: meta.tokenName,
    leadingTextParam2: '',
    firstLinkInfo: { linkText: 'Configuration' },
    endingTextParam: '',
  };
}

/** `spending_limit_is_expired`/`spending_limit_expiring`. */
function parseSettingsSectionLink(): ParsedLegacyInformation {
  return {
    leadingTextParam1: '',
    leadingTextParam2: '',
    firstLinkInfo: { linkText: 'settings section' },
    endingTextParam: '',
  };
}

function parseRates(meta: FullNotificationMeta): ParsedLegacyInformation {
  return {
    leadingTextParam1: meta.ratesCount === undefined ? undefined : String(meta.ratesCount),
    leadingTextParam2: '',
    firstLinkInfo: { linkText: meta.promptName ?? '', id: meta.promptId },
    endingTextParam: '',
  };
}

function parseComments(meta: FullNotificationMeta): ParsedLegacyInformation {
  return {
    leadingTextParam1: meta.commentsCount === undefined ? undefined : String(meta.commentsCount),
    leadingTextParam2: meta.repliesCount === undefined ? undefined : String(meta.repliesCount),
    firstLinkInfo: { linkText: meta.promptName ?? '', id: meta.promptId },
    endingTextParam: '',
  };
}

function parseRewardNewLevel(meta: FullNotificationMeta): ParsedLegacyInformation {
  return {
    leadingTextParam1: meta.newLevel === undefined ? undefined : String(meta.newLevel),
    leadingTextParam2: '',
    endingTextParam: '',
  };
}

function parseContributorRequest(meta: FullNotificationMeta): ParsedLegacyInformation {
  return {
    leadingTextParam1: meta.authorName,
    leadingTextParam2: '',
    firstLinkInfo: { linkText: meta.promptName ?? '', id: meta.promptId },
    endingTextParam: '',
  };
}

function parseTeammateAdded(meta: FullNotificationMeta): ParsedLegacyInformation {
  const users = meta.users ?? [];
  return {
    leadingTextParam1: `${users.join(', ')} ${users.length > 1 ? 'are' : 'is'}`,
    leadingTextParam2: '',
    firstLinkInfo: { linkText: meta.projectName ?? '' },
    endingTextParam: '',
  };
}

function parseChatUserAdded(meta: FullNotificationMeta): ParsedLegacyInformation {
  return {
    leadingTextParam1: meta.initiatorName ?? 'You were ',
    leadingTextParam2: meta.initiatorName !== undefined ? 'you ' : '',
    firstLinkInfo: { linkText: meta.conversationName ?? '', id: meta.conversationId, isNewTab: true },
    endingTextParam: '',
  };
}

function parsePrivateProjectCreated(): ParsedLegacyInformation {
  return { leadingTextParam1: '', leadingTextParam2: '', endingTextParam: '' };
}

function parseIndexDataChanged(meta: FullNotificationMeta): ParsedLegacyInformation {
  return {
    leadingTextParam1: formatIndexMessage(meta, meta.toolkitId !== undefined),
    leadingTextParam2: '',
    firstLinkInfo:
      meta.toolkitId === undefined
        ? undefined
        : { linkText: meta.indexName ?? 'Index', id: meta.toolkitId, indexName: meta.indexName, isNewTab: true },
    endingTextParam: '',
  };
}

function parseBucketExpirationWarning(meta: FullNotificationMeta): ParsedLegacyInformation {
  return {
    leadingTextParam1: '',
    leadingTextParam2: '',
    firstLinkInfo: { linkText: meta.bucketName ?? 'Bucket', id: meta.bucketName, isNewTab: true },
    endingTextParam: '',
  };
}

function parsePersonalAccessTokenExpiring(meta: FullNotificationMeta): ParsedLegacyInformation {
  return {
    leadingTextParam1: meta.tokenName,
    leadingTextParam2: '',
    firstLinkInfo: { linkText: 'Manage Personal Access Tokens', isNewTab: true },
    endingTextParam: '',
  };
}

/** `notificationLegacy.helpers.js`'s implicit `default: return {}` — no case handles these event types (`ChatUserMentioned` has no baseline `parseInformation` case at all; `moderation_rejected`/`moderation_approved` postdate that file). */
function parseNothing(): ParsedLegacyInformation {
  return {};
}

/**
 * One entry per {@link NotificationEventType} member — see `../lib/
 * routes.ts`'s `HREF_RESOLVERS` doc comment for why a `Record` literal
 * (rather than a `switch`) is this file's exhaustiveness mechanism.
 */
const LEGACY_INFO_PARSERS: Record<NotificationEventType, LegacyInfoParser> = {
  agent_unpublished: parseAgentUnpublished,
  moderator_approval_of_version: parseConfigurationLink,
  moderator_reject_of_version: parseConfigurationLink,
  moderator_unpublish: parseConfigurationLink,
  author_approval: parseConfigurationLink,
  author_reject: parseConfigurationLink,
  token_expiring: parseConfigurationLink,
  token_is_expired: parseConfigurationLink,
  spending_limit_is_expired: parseSettingsSectionLink,
  spending_limit_expiring: parseSettingsSectionLink,
  rates: parseRates,
  comments: parseComments,
  reward_new_level: parseRewardNewLevel,
  contributor_request_for_publish_approve: parseContributorRequest,
  user_was_added_to_some_project_as_teammate: parseTeammateAdded,
  chat_user_added: parseChatUserAdded,
  chat_user_mentioned: parseNothing,
  private_project_created: parsePrivateProjectCreated,
  index_data_changed: parseIndexDataChanged,
  bucket_expiration_warning: parseBucketExpirationWarning,
  personal_access_token_expiring: parsePersonalAccessTokenExpiring,
  moderation_rejected: parseNothing,
  moderation_approved: parseNothing,
  run_finished: parseNothing,
};

/**
 * Port of `notificationLegacy.helpers.js:67-234`'s `parseInformation`.
 * `notification` here is the pre-normalization shape it reads
 * (`event_type`/`project_id` top-level, `meta` nested) — `LegacyNotificationMessage`
 * passes its own props through unchanged from what it receives. Falls back
 * to {@link parseNothing} for an `eventType` outside the known union (same
 * reasoning as `../lib/routes.ts`'s `resolveNotificationHref`).
 */
export function parseLegacyInformation(
  eventType: NotificationEventType,
  meta: FullNotificationMeta,
  projectId: string | undefined,
): ParsedLegacyInformation {
  const parse = LEGACY_INFO_PARSERS[eventType] ?? parseNothing;
  return parse(meta, projectId);
}
