/**
 * The id lists of the LLM Governance dialog: parsing and the strict check that
 * blocks a save (ADR-0041 decision 4).
 *
 * Kept out of `useGatewayGovernancePage.ts` so that file stays the draft ↔
 * payload mapping and the page state.
 */
import { t } from '@/shared/i18n';

import { isGlobalGovernanceType, type GovernanceType } from './api/adminGovernanceApi';
import { parseGuardrailDetectors } from './governanceGuardrailDraft';

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
type IdListField = 'scopeProjectIds' | 'scopeAgentIds' | 'scopeUserIds';

/** The draft fields `draftFieldErrors` checks: the id lists and the guardrail detectors. */
type CheckedField = IdListField | 'guardrailDetectors';

/** The part of a governance draft that the check reads. */
type IdListDraft = { readonly type: GovernanceType; readonly guardrailDetectors: string } & Readonly<
  Record<IdListField, string>
>;

/** The id-list fields each type sends. */
function idListFields(type: GovernanceType): IdListField[] {
  if (type === 'rate_limit') return ['scopeProjectIds', 'scopeAgentIds', 'scopeUserIds'];
  // A guardrail can name agents of one project (ADR-0043 decision 1), not users.
  if (type === 'guardrail') return ['scopeProjectIds', 'scopeAgentIds'];
  return ['scopeProjectIds'];
}


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
export function draftFieldErrors(draft: IdListDraft): Partial<Record<CheckedField, string>> {
  const errors: Partial<Record<CheckedField, string>> = {};
  if (isGlobalGovernanceType(draft.type)) return errors;
  if (draft.type === 'guardrail') {
    const parsed = parseGuardrailDetectors(draft.guardrailDetectors);
    if ('error' in parsed) errors.guardrailDetectors = parsed.error;
  }
  for (const field of idListFields(draft.type)) {
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
