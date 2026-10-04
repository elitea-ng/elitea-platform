import { t } from '@/shared/i18n';

import type { Author } from './types';

/** An author as a read can hand it over: possibly absent, possibly with blank or missing fields. */
export type AuthorLike = {
  readonly id?: string | undefined;
  readonly name?: string | null | undefined;
  readonly email?: string | null | undefined;
} | null | undefined;

function filled(value: unknown): value is string {
  return typeof value === 'string' && value.trim() !== '';
}

/**
 * The author's account is gone (#6702).
 *
 * The server still answers the author id when the account was deleted, and
 * the join leaves name and email empty (`applications/handler.go`). An
 * object with neither field is that state, not "no author".
 */
export function isDeletedAuthor(author: AuthorLike): boolean {
  return author != null && !filled(author.name) && !filled(author.email);
}

/**
 * Display name: the name, else the email, else "Deleted user" (#6702).
 *
 * A deleted creator used to reach `.trim()` on a missing name and take the
 * whole agent page down with it. Every field is optional here on purpose.
 */
export function authorDisplayName(author: AuthorLike): string {
  if (filled(author?.name)) return author.name;
  if (filled(author?.email)) return author.email;
  return t('entities.author.deletedUser', 'Deleted user');
}

/**
 * Identity comparison by id — apps/elitea-ui/src/components/DataRowAction.jsx
 * :92,120 (`state.user.id === data?.author_id`) and
 * apps/elitea-ui/src/components/Categories.jsx:84
 * (`state.user.author_id === myAuthorId`) both reduce to an author-id
 * equality check; ported here as one pure comparator rather than the two
 * divergent redux-field read patterns the old app has.
 */
export function isSameAuthor(a: Author, b: Author): boolean {
  return a.id === b.id;
}

/** True when `author.id` matches the given current-user author id. */
export function isCurrentUserAuthor(author: Author, currentUserAuthorId: string | undefined): boolean {
  return currentUserAuthorId !== undefined && author.id === currentUserAuthorId;
}
