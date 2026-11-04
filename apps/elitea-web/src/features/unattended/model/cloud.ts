/**
 * The unattended grant of a cloud chat send (ADR-0045 decisions 1 and 2): its
 * wire (`unattended` on the chat send body, `ChatUnattendedRequest`), the
 * tools of the conversation agent that the grant can name, and the readable
 * text of Main's 422 `unattended_grant_refused`.
 *
 * The cloud wire never carries the desktop-only parts (the tool-call cap, hook
 * questions, remote confirmations): Main refuses a cloud send that sets one.
 */
import type { ChatUnattendedRequest } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { isRecord, nonBlankText } from '@/shared/lib/object';

import { chainErrors, defaultGrant, type ChainField, type SensitiveToolRef, type UnattendedGrantValue } from './grant';

/** The cloud grant; `goal_check` (contract #1564, absent = true) is sent always. */
export type CloudGrantResult = { ok: true; grant: ChatUnattendedRequest } | { ok: false; invalid: ChainField[] };

const SECONDS_PER_HOUR = 3600;

/** The cloud wire of `value` (with `grantId` when given), or the chain fields that are not valid. */
export function toCloudGrant(value: UnattendedGrantValue, grantId?: string): CloudGrantResult {
  const invalid = chainErrors(value.chain);
  if (invalid.length > 0) return { ok: false, invalid };
  const [count, amount, hours] = [value.chain.max_continuations.trim(), value.chain.max_spend.trim(), value.chain.deadline_hours.trim()];
  const chain: NonNullable<ChatUnattendedRequest['chain']> = {
    ...(count === '' ? {} : { max_continuations: Number(count) }),
    ...(amount === '' ? {} : { max_spend: Number(amount) }),
    ...(hours === '' ? {} : { deadline_seconds: Math.round(Number(hours) * SECONDS_PER_HOUR) }),
  };
  const grant: ChatUnattendedRequest = {
    ...(grantId === undefined ? {} : { grant_id: grantId }),
    continue_step_limit: value.continue_step_limit,
    continue_output_limit: value.continue_output_limit,
    goal_check: value.goal_check,
    ...(value.sensitive_tools.length === 0 ? {} : { sensitive_tools: value.sensitive_tools.map(({ toolkit, tool }) => ({ toolkit_name: toolkit, tool_name: tool })) }),
    ...(Object.keys(chain).length === 0 ? {} : { chain }),
  };
  return { ok: true, grant };
}

/** A number as the chain field types it; absent is the empty field (the platform default). */
const typed = (value: number | undefined): string => (value === undefined ? '' : String(value));

/**
 * The form value of a stored cloud grant (a schedule's or a trigger's, ADR-0045
 * decision 9): the inverse of `toCloudGrant`. An absent part takes the default.
 */
export function fromCloudGrant(grant: ChatUnattendedRequest): UnattendedGrantValue {
  const value = defaultGrant();
  const seconds = grant.chain?.deadline_seconds;
  return {
    ...value,
    continue_step_limit: grant.continue_step_limit ?? value.continue_step_limit,
    continue_output_limit: grant.continue_output_limit ?? value.continue_output_limit,
    goal_check: grant.goal_check ?? value.goal_check,
    chain: {
      max_continuations: typed(grant.chain?.max_continuations),
      max_spend: typed(grant.chain?.max_spend),
      deadline_hours: seconds === undefined ? '' : String(seconds / SECONDS_PER_HOUR),
    },
  };
}

/** The tool names a version tool entry selects: top-level `selected_tools`, else in its settings or config. */
function selectedTools(entry: Record<string, unknown>): string[] {
  const settings = isRecord(entry['settings']) ? entry['settings'] : {};
  const config = isRecord(entry['config']) ? entry['config'] : {};
  const configSettings = isRecord(config['settings']) ? config['settings'] : {};
  const candidates = [entry['selected_tools'], settings['selected_tools'], configSettings['selected_tools']];
  const list = candidates.find((candidate) => Array.isArray(candidate) && candidate.length > 0);
  return Array.isArray(list) ? list.filter((name): name is string => typeof name === 'string' && name !== '') : [];
}

/** The toolkit name of a version tool entry, or undefined for a sub-agent or an entry without a name. */
function toolkitNameOf(entry: Record<string, unknown>): string | undefined {
  if (entry['type'] === 'application' || entry['entity_type'] === 'application') return undefined;
  const config = isRecord(entry['config']) ? entry['config'] : {};
  return nonBlankText(entry['toolkit_name']) ?? nonBlankText(entry['name']) ?? nonBlankText(config['toolkit_name']) ?? nonBlankText(config['name']);
}

/**
 * The tools that a grant can name, from an agent version's `tools`: each
 * selected tool of each toolkit (not of a sub-agent). The web does not know
 * which tools the guardrails mark sensitive, so it lists every selected tool;
 * Main checks each named tool and refuses a bad one by name. A toolkit that
 * selects no tool (all tools) gives no row: its tool names are not known here.
 */
export function grantableTools(versionDetails: unknown): SensitiveToolRef[] {
  if (!isRecord(versionDetails) || !Array.isArray(versionDetails['tools'])) return [];
  const seen = new Set<string>();
  const out: SensitiveToolRef[] = [];
  for (const entry of versionDetails['tools']) {
    const toolkit = isRecord(entry) ? toolkitNameOf(entry) : undefined;
    if (toolkit === undefined || !isRecord(entry)) continue;
    for (const tool of selectedTools(entry)) {
      const key = `${toolkit}/${tool}`;
      if (seen.has(key)) continue;
      seen.add(key);
      out.push({ toolkit, tool });
    }
  }
  return out;
}

/** The form's name of a refused chain field; any other field as sent. */
function fieldText(field: string): string {
  switch (field) {
    case 'chain.max_continuations':
      return t('unattended.chain.continuations', 'Most automatic continuations');
    case 'chain.max_spend':
      return t('unattended.chain.spend', 'Most spend (USD)');
    case 'chain.deadline_seconds':
      return t('unattended.chain.deadline', 'Deadline (hours)');
    default:
      return field;
  }
}

function reasonText(reason: string, field: string, tool: string): string {
  switch (reason) {
    case 'disabled':
      return t('unattended.refused.disabled', 'An administrator turned off unattended grants.');
    case 'over_maximum':
      return t('unattended.refused.overMaximum', '{{field}}: the value is more than the platform maximum. Type a lower value.', { field: fieldText(field) });
    case 'not_owner':
      return t('unattended.refused.notOwner', 'Only the person who starts the run can give the grant.');
    case 'origin_not_allowed':
      return t('unattended.refused.origin', 'A schedule or a webhook cannot name sensitive tools.');
    case 'tool_not_resolved':
      return t('unattended.refused.toolNotResolved', 'The tool {{tool}} is not one of the tools of this agent.', { tool });
    case 'category_withheld':
      return t('unattended.refused.categoryWithheld', 'The tool {{tool}} is in a category that the platform keeps out of grants.', { tool });
    case 'tier_requires_hitl':
      return t('unattended.refused.tierRequiresHitl', 'The tier of this agent requires a person to approve the tool {{tool}}.', { tool });
    case 'chain_not_found':
      return t('unattended.refused.chainNotFound', 'The run chain of this grant does not exist.');
    default:
      return t('unattended.refused.invalid', 'The value of {{field}} is not valid.', { field: field === '' ? reason : fieldText(field) });
  }
}

/**
 * The readable text of a 422 `unattended_grant_refused` body (`reason`,
 * `field`, `tool`), or undefined for any other body.
 */
export function unattendedRefusalText(body: unknown): string | undefined {
  if (!isRecord(body) || body['error'] !== 'unattended_grant_refused') return undefined;
  const reason = nonBlankText(body['reason']) ?? 'invalid';
  const detail = reasonText(reason, nonBlankText(body['field']) ?? '', nonBlankText(body['tool']) ?? '');
  return t('unattended.refused.message', 'The unattended grant was refused. {{detail}} Change the grant and send again.', { detail });
}
