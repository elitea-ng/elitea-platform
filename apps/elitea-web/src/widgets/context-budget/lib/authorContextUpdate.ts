/**
 * lib/authorContextUpdate.ts — reading and rewriting the reader's own default
 * context budget on the author profile.
 *
 * The budget the panel reports is resolved server-side from
 * `conversation strategy > the user's defaults > the constants`
 * (`internal/domain/contextsettings`.`Resolve`). The chat panel can only reach
 * the middle term, which is `centry.social_users.default_context_management` —
 * exactly the block Settings › Memory writes with `PUT /social/author`. The
 * panel therefore writes that one field, and the two surfaces stay one setting
 * rather than two.
 *
 * Two properties of `UpdateAuthor` shape everything here:
 *
 *  1. `name`/`description`/`avatar`/`personalization` are UPSERT FROM THE
 *     BODY. Omitting them blanks the stored value, so every one of them is
 *     read back and carried forward.
 *  2. `default_context_management` is COALESCEd — the stored value survives a
 *     body that does not mention it — which is why this can send that block
 *     alone without carrying Settings › Memory's summarization block too.
 *
 * `personalization` is carried forward BYTE FOR BYTE, including a legacy
 * nested `default_context_management`. `features/settings`' own
 * `buildAuthorUpdate` strips the nested copy, because it also sends a
 * top-level replacement for both blocks; this writer sends only one, and
 * stripping the nested `default_summarization` while sending no top-level
 * replacement for it would delete that profile's summarization settings. A
 * stale nested context block is harmless: every reader prefers the top-level
 * column, and Settings › Memory's next save removes it.
 *
 * This lives in `widgets/context-budget/lib` rather than being imported from
 * `features/settings`: a widget may not import a feature slice
 * (`.dependency-cruiser.cjs`), and the two writers agree on the rule above by
 * following the same documented handler contract, not by sharing a module.
 */
import type { AuthorUpdateRequest } from '@/shared/api/generated/model';

/** @public The author record as `GET /social/author` answers it, narrowed to what this writer must carry. */
export interface AuthorContextProfile {
  readonly name?: string;
  readonly description?: string;
  readonly avatar?: string;
  readonly personalization?: unknown;
  /** Absent for an account that has never saved Settings › Memory. */
  readonly default_context_management?: Record<string, unknown>;
}

/** `memoryContextManagementMaxContextTokensMin` — the generated contract's own floor, enforced server-side. */
export const MIN_MAX_CONTEXT_TOKENS = 1000;

/**
 * The ceiling `features/settings`' own Memory form applies
 * (`lib/profile/context-budget/constants.ts`'s `VALIDATION_LIMITS`). Repeated
 * rather than imported for the layer reason in the module doc.
 */
export const MAX_MAX_CONTEXT_TOKENS = 10_000_000;

function asRecord(value: unknown): Record<string, unknown> {
  return typeof value === 'object' && value !== null ? (value as Record<string, unknown>) : {};
}

/**
 * The stored context block, preferring the top-level column over a copy an
 * older client nested inside `personalization`.
 *
 * The fallback is not decoration: a profile last saved before this app stopped
 * nesting the block carries its values ONLY in `personalization`, and ignoring
 * that would show the reader the platform default as if they had never
 * configured anything — and then overwrite their real setting on save.
 */
export function readContextBlock(author: AuthorContextProfile | undefined): Record<string, unknown> {
  const top = author?.default_context_management;
  if (top !== undefined && top !== null) return asRecord(top);
  return asRecord(asRecord(author?.personalization)['default_context_management']);
}

/** The reader's own default budget, or `undefined` when they have never set one. */
export function selectMaxContextTokens(author: AuthorContextProfile | undefined): number | undefined {
  const value = readContextBlock(author)['max_context_tokens'];
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined;
}

/** `undefined` when `value` is a budget the server would refuse; otherwise the reason-free `number`. */
export function validateMaxContextTokens(value: string): number | undefined {
  const parsed = Number(value.trim());
  if (value.trim() === '' || !Number.isInteger(parsed)) return undefined;
  if (parsed < MIN_MAX_CONTEXT_TOKENS || parsed > MAX_MAX_CONTEXT_TOKENS) return undefined;
  return parsed;
}

/**
 * The body for `PUT /social/author` that changes the budget and nothing else.
 *
 * Every other key of the stored context block is carried forward, so changing
 * the budget does not silently reset `preserve_recent_messages` or turn the
 * context manager off.
 */
export function buildContextBudgetUpdate(
  author: AuthorContextProfile | undefined,
  maxContextTokens: number,
): AuthorUpdateRequest {
  return {
    ...(author?.name === undefined ? {} : { name: author.name }),
    ...(author?.description === undefined ? {} : { description: author.description }),
    ...(author?.avatar === undefined ? {} : { avatar: author.avatar }),
    personalization: author?.personalization ?? {},
    default_context_management: {
      ...readContextBlock(author),
      // A budget the reader typed only makes sense with the manager on. The
      // stored `enabled` is preserved when it is already true; an absent one
      // resolves to the platform default, which this makes explicit rather
      // than leaving the new budget inert.
      enabled: true,
      max_context_tokens: maxContextTokens,
    },
  };
}
