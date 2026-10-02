/** Preserve unrelated author fields when changing the default context preset. */
import { resolveContextBudgetMode, type ContextBudgetMode } from '@/shared/lib/contextBudget';
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

export function selectBudgetMode(author: AuthorContextProfile | undefined): ContextBudgetMode {
  return resolveContextBudgetMode(readContextBlock(author)['budget_mode']);
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
  budgetMode: ContextBudgetMode,
): AuthorUpdateRequest {
  const { max_context_tokens: _legacyLimit, ...context } = readContextBlock(author);
  return {
    ...(author?.name === undefined ? {} : { name: author.name }),
    ...(author?.description === undefined ? {} : { description: author.description }),
    ...(author?.avatar === undefined ? {} : { avatar: author.avatar }),
    personalization: author?.personalization ?? {},
    default_context_management: {
      ...context,
      budget_mode: budgetMode,
    },
  };
}
