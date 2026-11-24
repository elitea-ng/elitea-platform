/**
 * An Allow rule's grant and conditions (#3005–#3007) as the panel's fields
 * type them, and back:
 *
 * - the credential is a profile id (`gh`) or none;
 * - hosts, forbidden flags and required flags are words separated by spaces,
 *   commas or new lines;
 * - flag values are one flag per line followed by its values
 *   (`-X GET` / `--method GET HEAD`);
 * - "GraphQL queries only" is a checkbox.
 *
 * The host validates the result; this only keeps the shapes. A grant on a
 * rule that is not an Allow is dropped (the host would refuse it).
 */
import type { PrefixRule, RuleConditions } from '@/shared/desktop/rulesIpc';

/** The credential profiles the host knows (`credentials::PROFILES`). */
export const CREDENTIALS = ['gh'] as const;

export interface GrantDraft {
  /** `''`: no login. */
  credential: string;
  hosts: string;
  forbidFlags: string;
  requireFlags: string;
  flagValues: string;
  graphqlQueryOnly: boolean;
}

export const EMPTY_GRANT: GrantDraft = {
  credential: '',
  hosts: '',
  forbidFlags: '',
  requireFlags: '',
  flagValues: '',
  graphqlQueryOnly: false,
};

const words = (text: string): string[] => text.split(/[\s,]+/u).filter((word) => word !== '');

export function grantDraftOf(rule: PrefixRule): GrantDraft {
  const conditions = rule.conditions ?? {};
  return {
    credential: rule.credential ?? '',
    hosts: (rule.hosts ?? []).join(' '),
    forbidFlags: (conditions.forbid_flags ?? []).join(' '),
    requireFlags: (conditions.require_flags ?? []).join(' '),
    flagValues: Object.entries(conditions.flag_values ?? {})
      .map(([flag, values]) => [flag, ...values].join(' '))
      .join('\n'),
    graphqlQueryOnly: conditions.graphql === 'query',
  };
}

function flagValuesOf(text: string): Record<string, string[]> {
  const out: Record<string, string[]> = {};
  for (const line of text.split('\n')) {
    const [flag, ...values] = words(line);
    if (flag !== undefined) out[flag] = [...(out[flag] ?? []), ...values];
  }
  return out;
}

function conditionsOf(draft: GrantDraft): RuleConditions | undefined {
  const conditions: RuleConditions = {};
  const forbid = words(draft.forbidFlags);
  const require = words(draft.requireFlags);
  const values = flagValuesOf(draft.flagValues);
  if (forbid.length > 0) conditions.forbid_flags = forbid;
  if (require.length > 0) conditions.require_flags = require;
  if (Object.keys(values).length > 0) conditions.flag_values = values;
  if (draft.graphqlQueryOnly) conditions.graphql = 'query';
  return Object.keys(conditions).length > 0 ? conditions : undefined;
}

/** `rule` with the draft's grant and conditions (none on a rule that is not an Allow; no login or hosts on a repository's rule). */
export function withGrant(rule: PrefixRule, draft: GrantDraft): PrefixRule {
  const out: PrefixRule = { ...rule };
  delete out.credential;
  delete out.hosts;
  delete out.conditions;
  if ((rule.decision ?? 'allow') !== 'allow') return out;
  const hosts = words(draft.hosts);
  const conditions = conditionsOf(draft);
  // A repository's rule never grants (the host ignores it): keep only its conditions.
  if (rule.origin === undefined && draft.credential !== '') out.credential = draft.credential;
  if (rule.origin === undefined && hosts.length > 0) out.hosts = hosts;
  if (conditions !== undefined) out.conditions = conditions;
  // The network a grant gives is its hosts' only: never the whole network too.
  if (out.credential !== undefined || out.hosts !== undefined) delete out.network;
  return out;
}

/** The conditions, in parts the list shows (`values`: `-X GET|HEAD`). */
export function conditionParts(conditions: RuleConditions | undefined): {
  forbid: string[];
  require: string[];
  values: string[];
  graphql: boolean;
} {
  return {
    forbid: conditions?.forbid_flags ?? [],
    require: conditions?.require_flags ?? [],
    values: Object.entries(conditions?.flag_values ?? {}).map(([flag, allowed]) => `${flag} ${allowed.join('|')}`),
    graphql: conditions?.graphql === 'query',
  };
}
