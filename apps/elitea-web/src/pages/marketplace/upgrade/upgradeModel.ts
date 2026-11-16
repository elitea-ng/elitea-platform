/**
 * The rules of the upgrade and upstream-update dialogs, with no React in them
 * (ADR-0051 decision 4): what a widening says, which fields still need a
 * choice, what a refusal says, and how an edited field becomes JSON.
 */
import type {
  MarketplaceMergeResolution,
  MarketplaceUpstreamReview,
  MarketplaceWidening,
} from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { hireErrorCode } from '../hire/hireModel';

export type UpgradePolicy = 'pinned' | 'notify' | 'auto';

export const UPGRADE_POLICIES: readonly UpgradePolicy[] = ['pinned', 'notify', 'auto'];

export function policyLabel(policy: UpgradePolicy): string {
  switch (policy) {
    case 'pinned':
      return t('marketplace.upgrade.policy.pinned', 'Pinned: change nothing until I act');
    case 'notify':
      return t('marketplace.upgrade.policy.notify', 'Notify me of new versions');
    case 'auto':
      return t('marketplace.upgrade.policy.auto', 'Upgrade automatically (low tier, nothing added)');
  }
}

/** The policy the server sent, or pinned for an unknown word. */
export function readPolicy(value: string | undefined): UpgradePolicy {
  return value === 'notify' || value === 'auto' ? value : 'pinned';
}

/** One line for what a version adds. The server's detail is the thing added. */
export function wideningText(widening: MarketplaceWidening): string {
  switch (widening.kind) {
    case 'tool_added':
      return t('marketplace.upgrade.widening.tool', 'A new tool: {{detail}}', { detail: widening.detail });
    case 'capability_added':
      return t('marketplace.upgrade.widening.capability', 'A new capability: {{detail}}', { detail: widening.detail });
    case 'requirement_added':
      return t('marketplace.upgrade.widening.requirement', 'A new requirement: {{detail}}', {
        detail: widening.detail,
      });
    case 'tier_raised':
      return t('marketplace.upgrade.widening.tier', 'A higher risk tier: {{detail}}', { detail: widening.detail });
    default:
      return widening.detail;
  }
}

const REFUSALS: Readonly<Record<string, () => string>> = {
  hire_needs_shared_project: () =>
    t(
      'marketplace.upgrade.error.sharedProject',
      'This change needs a second person to approve it. A personal project has none. Use a shared project.',
    ),
  binding_missing: () =>
    t('marketplace.upgrade.error.binding', 'Bind every required toolkit, or choose Disable for now.'),
  version_not_hireable: () =>
    t('marketplace.upgrade.error.version', 'This version cannot be used now. Choose another version.'),
  already_pinned: () => t('marketplace.upgrade.error.pinned', 'The agent is already on this version.'),
  hire_pending: () =>
    t('marketplace.upgrade.error.hirePending', 'The hire of this agent is not approved yet. Approve it first.'),
  unresolved_conflicts: () =>
    t('marketplace.upstream.error.conflicts', 'Choose theirs, ours or an edit for every conflict.'),
  change_request_open: () =>
    t('marketplace.upgrade.error.open', 'The agent already has an open change request. Finish or withdraw it first.'),
};

/** One sentence for a failed upgrade or merge. A code we know gets its own words. */
export function upgradeErrorMessage(error: unknown): string {
  const known = REFUSALS[hireErrorCode(error) ?? ''];
  return known === undefined ? t('marketplace.upgrade.error.generic', 'The change failed. Try again.') : known();
}

/** What a person decided for one field of the three-way view. */
export type FieldPick = { readonly choice: 'theirs' | 'ours' } | { readonly choice: 'edit'; readonly text: string };

export type FieldPicks = Readonly<Record<string, FieldPick | undefined>>;

const JSON_FIELDS = new Set(['conversation_starters', 'llm_settings', 'pipeline_settings', 'soul']);

/** Text a person edits: a string as it is, any other value as indented JSON. */
export function fieldText(value: unknown): string {
  if (value === null || value === undefined) return '';
  return typeof value === 'string' ? value : JSON.stringify(value, null, 2);
}

/** The value an edited text stands for; `undefined` when a JSON field does not parse. */
export function parseEdit(key: string, text: string): { readonly value: unknown } | undefined {
  if (!JSON_FIELDS.has(key)) return { value: text };
  if (text.trim() === '') return { value: null };
  try {
    return { value: JSON.parse(text) as unknown };
  } catch {
    return undefined;
  }
}

/** The fields that still block the merge: an unanswered conflict, or an edit that is not valid. */
export function blockingFields(review: MarketplaceUpstreamReview, picks: FieldPicks): readonly string[] {
  const out: string[] = [];
  for (const field of review.fields) {
    const pick = picks[field.key];
    if (field.status === 'conflict' && pick === undefined) out.push(field.key);
    else if (pick?.choice === 'edit' && parseEdit(field.key, pick.text) === undefined) out.push(field.key);
  }
  return out;
}

/** The request's resolutions: one per field the person touched; the rest take the review's default. */
export function resolutionsOf(
  review: MarketplaceUpstreamReview,
  picks: FieldPicks,
): Record<string, MarketplaceMergeResolution> {
  const out: Record<string, MarketplaceMergeResolution> = {};
  for (const field of review.fields) {
    const pick = picks[field.key];
    if (pick === undefined) continue;
    if (pick.choice !== 'edit') {
      out[field.key] = { choice: pick.choice };
      continue;
    }
    const parsed = parseEdit(field.key, pick.text);
    if (parsed !== undefined) out[field.key] = { choice: 'edit', value: parsed.value };
  }
  return out;
}

export function statusText(status: string): string {
  switch (status) {
    case 'conflict':
      return t('marketplace.upstream.status.conflict', 'Conflict: both sides changed it');
    case 'theirs_only':
      return t('marketplace.upstream.status.theirs', 'Changed upstream');
    case 'ours_only':
      return t('marketplace.upstream.status.ours', 'Changed in your fork');
    case 'differs':
      return t('marketplace.upstream.status.differs', 'Differs from upstream');
    default:
      return t('marketplace.upstream.status.same', 'Same on both sides');
  }
}

/** The merge cannot be proposed: the version is not usable, the fork has an open request, or a field still needs a choice. */
export function mergeBlocked(review: MarketplaceUpstreamReview, picks: FieldPicks): boolean {
  return !review.hireable || review.open_change_request || blockingFields(review, picks).length > 0;
}
