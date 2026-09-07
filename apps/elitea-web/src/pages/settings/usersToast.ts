/**
 * What the Users page SAYS — the toast lines, and nothing else.
 *
 * Split out of `Users.tsx` because these are the only pure decisions on that
 * page (counts in, a sentence out) and because a page over the 400-line budget
 * is a page nobody reads. The bulk half has a real history: the port carried
 * only the SINGULAR strings, so inviting twelve people reported "The user has
 * been invited by e-mail". The reference had both forms.
 */
import { t } from '@/shared/i18n';
import type { InviteSummary } from '@/shared/ui/settings/inviteResults';

/**
 * The toast for a batch where EVERY address was invited.
 *
 * The delivered/added split is ADR-0024 WP7's and is unchanged: "invited" only
 * when an invitation e-mail actually went out. A deployment with no SMTP has
 * ADDED the user — they sign in on their own — and must not claim otherwise.
 */
export function inviteSuccessMessage(summary: InviteSummary): string {
  if (summary.invited > 1) {
    return summary.delivered
      ? t('shared.ui.settings.users.multipleUsersInvited', 'The users have been invited by e-mail')
      : t('shared.ui.settings.users.multipleUsersAdded', 'The users have been added. No invitation e-mail was sent.');
  }
  return summary.delivered
    ? t('shared.ui.settings.users.userInvited', 'The user has been invited by e-mail')
    : t('shared.ui.settings.users.userAdded', 'The user has been added. No invitation e-mail was sent.');
}

/**
 * The toast for a batch that did NOT end the same way for every address.
 *
 * It says a count and points at the list, rather than naming an address: the
 * per-address rows are on screen beside it, and a toast that repeated one of
 * them would be choosing which failure matters.
 */
export function invitePartialMessage(summary: InviteSummary): string {
  const refused = summary.alreadyMember + summary.invalid + summary.failed;
  if (summary.invited === 0) {
    return t('shared.ui.settings.users.inviteNoneAdded', 'No user was added. See the list below.');
  }
  return t('shared.ui.settings.users.invitePartial', 'Added {{added}} of {{total}}. See the list below.', {
    added: summary.invited,
    total: summary.invited + refused,
  });
}

/**
 * `EliteaApiError` always carries a real `.message` (local-helper convention,
 * matching `features/apps/lib/errorMessage.ts`). Used for the invite path's
 * LAST resort — a failure that carried no per-address rows at all, such as a
 * network error or a 403 — where there is nothing better to say.
 */
export function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
