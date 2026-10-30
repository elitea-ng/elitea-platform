/**
 * The id lists of the LLM Governance dialog: parsing and the strict check that
 * blocks a save (ADR-0041 decision 4).
 *
 * Kept out of `useGatewayGovernancePage.ts` so that file stays the draft ↔
 * payload mapping and the page state.
 */
import { t } from '@/shared/i18n';

import { isGlobalGovernanceType } from './api/adminGovernanceApi';
import type { GovernanceDraft } from './useGatewayGovernancePage';

/** Splits a comma or newline separated list into trimmed, non-empty entries. */
export function splitList(raw: string): string[] {
  return raw
    .split(/[\n,]/)
    .map((part) => part.trim())
    .filter((part) => part !== '');
}

/** A positive whole number written in plain digits: "7", not "7.5", "07x" or "-1". */
const POSITIVE_INT = /^[1-9]\d*$/;

/**
 * Parses a list of ids. Call it only on a field `draftFieldErrors` accepts:
 * every entry is then a positive integer and none is dropped.
 */
export function splitIntList(raw: string): number[] {
  return splitList(raw).map((part) => Number(part));
}

/** The entries of an id list that are not a positive integer. */
function invalidIdEntries(raw: string): string[] {
  return splitList(raw).filter((part) => !POSITIVE_INT.test(part));
}

/** The draft fields that hold id lists, and so are checked by `draftFieldErrors`. */
export type IdListField = 'scopeProjectIds' | 'scopeAgentIds' | 'scopeUserIds';

/**
 * The id-list fields of a draft that cannot be saved, each with its reason.
 *
 * An entry that is not a positive integer is an ERROR, never dropped. Dropping
 * it changes what the row means: a mistyped "Throttled agents" list that loses
 * every entry saves as a rate limit on the whole project, and a mistyped
 * "Projects" list saves as a rule for every project. A decimal is not
 * truncated for the same reason: "3.5" is not agent 3.
 *
 * Only the fields the chosen type sends are checked, so a value left in a
 * field the dialog does not show cannot block a save.
 */
export function draftFieldErrors(draft: GovernanceDraft): Partial<Record<IdListField, string>> {
  const errors: Partial<Record<IdListField, string>> = {};
  if (isGlobalGovernanceType(draft.type)) return errors;
  const fields: IdListField[] =
    draft.type === 'rate_limit' ? ['scopeProjectIds', 'scopeAgentIds', 'scopeUserIds'] : ['scopeProjectIds'];
  for (const field of fields) {
    const invalid = invalidIdEntries(draft[field]);
    if (invalid.length > 0) {
      errors[field] = t(
        'pages.admin.governance.field.idListError',
        'Use positive whole numbers only. Not valid: {{entries}}',
        { entries: invalid.join(', ') },
      );
    }
  }
  return errors;
}
