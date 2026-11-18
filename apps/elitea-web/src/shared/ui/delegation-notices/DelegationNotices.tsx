/**
 * The notice of a delegation the run refused or narrowed (client contract
 * 1.60, ADR-0044 Amendment 1, A1.2 rule 5), at the sub-agent's place in the
 * answer: "Agent1 could not use Git from Agent2. This delegation is not
 * approved." An editor of the caller gets Review delegation; anyone else is
 * told who to ask. A refusal the person's own access caused (`principal`) or
 * the depth limit never offers it: an editor cannot fix those.
 *
 * Every text here is untrusted (agent and toolkit names) and renders as plain
 * text. The review link is a plain anchor, not a router link: transcripts also
 * render outside the app router (shared chats, the playback view), the same as
 * `BudgetUsageLink`.
 */
import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import AlertTitle from '@mui/material/AlertTitle';
import Box from '@mui/material/Box';
import Link from '@mui/material/Link';
import Typography from '@mui/material/Typography';

import { missingNames, offersReview, type DelegationNotice } from '@elitea/client-core/stream';

import { getConfig } from '@/shared/config';
import { t } from '@/shared/i18n';
import { normalizeBasename } from '@/shared/lib/basename';

/**
 * @public The calling agent's editor under the configured app base. With the
 * called agent's id it carries `review_delegation`, which opens that edge's
 * review dialog on arrival (S3's dialog, in the Tools panel); without it the
 * editor opens as before.
 */
export function delegationReviewHref(basePath: string, callerId: number, calleeId?: number): string {
  const path = `${normalizeBasename(basePath)}/agents/all/${String(callerId)}`;
  return calleeId === undefined ? path : `${path}?review_delegation=${String(calleeId)}`;
}

function resolveReviewHref(callerId: number, calleeId: number | undefined): string {
  if (import.meta.env.DEV) return delegationReviewHref('', callerId, calleeId);
  const result = getConfig();
  return delegationReviewHref(result.status === 'ok' ? result.config.vite_base_uri : '', callerId, calleeId);
}

function namesOf(notice: DelegationNotice): string {
  const listed = missingNames(notice).join(', ');
  const more = notice.missingTotal - notice.missing.length;
  if (listed === '') return t('features.chatMessages.delegationNotice.somethingNew', 'something it holds');
  return more > 0 ? t('features.chatMessages.delegationNotice.namesMore', '{{names}} and {{count}} more', { names: listed, count: more }) : listed;
}

/** @public The sentence a notice reads as, by what caused it. */
export function delegationNoticeSentence(notice: DelegationNotice): string {
  const callee = notice.calleeName;
  switch (notice.reason) {
    case 'stale':
      return t('features.chatMessages.delegationNotice.stale', '{{callee}} ran without {{names}}. It was added after this delegation was approved.', {
        callee,
        names: namesOf(notice),
      });
    case 'principal':
      return t('features.chatMessages.delegationNotice.principal', '{{callee}} could not use {{names}}. You do not have access to it.', {
        callee,
        names: namesOf(notice),
      });
    case 'depth':
      return notice.maxDepth === undefined
        ? t('features.chatMessages.delegationNotice.depthUnknown', '{{callee}} was not called: the delegation depth limit was reached.', { callee })
        : t('features.chatMessages.delegationNotice.depth', '{{callee}} was not called: the delegation depth limit of {{max}} was reached.', {
            callee,
            max: notice.maxDepth,
          });
    case 'no_grant':
      return notice.callerName === undefined
        ? t('features.chatMessages.delegationNotice.noGrantNoCaller', '{{callee}} could not use {{names}} from its caller. This delegation is not approved.', {
            callee,
            names: namesOf(notice),
          })
        : t('features.chatMessages.delegationNotice.noGrant', '{{callee}} could not use {{names}} from {{caller}}. This delegation is not approved.', {
            callee,
            names: namesOf(notice),
            caller: notice.callerName,
          });
  }
}

function Notice({ notice }: { readonly notice: DelegationNotice }): ReactNode {
  const refused = notice.kind === 'refused';
  const review = offersReview(notice) && notice.callerId !== undefined ? notice.callerId : undefined;
  const ask = review === undefined && notice.reason !== 'principal' && notice.reason !== 'depth' && notice.editors.length > 0;
  return (
    <Alert severity={refused ? 'warning' : 'info'} data-testid="delegation-notice" data-reason={notice.reason} sx={{ alignSelf: 'stretch', width: '100%' }}>
      <AlertTitle>
        {refused
          ? t('features.chatMessages.delegationNotice.titleRefused', 'Delegation refused')
          : t('features.chatMessages.delegationNotice.titleNarrowed', 'Delegation narrowed')}
      </AlertTitle>
      <Typography variant="bodySmall" component="p">
        {delegationNoticeSentence(notice)}
      </Typography>
      {review !== undefined && (
        <Link href={resolveReviewHref(review, notice.calleeId)} variant="bodyMedium" data-testid="delegation-review" sx={{ display: 'inline-block', mt: 0.5 }}>
          {t('features.chatMessages.delegationNotice.review', 'Review delegation')}
        </Link>
      )}
      {ask && (
        <Typography variant="bodySmall" component="p" data-testid="delegation-editors">
          {t('features.chatMessages.delegationNotice.ask', 'Ask {{names}} to review this delegation.', { names: notice.editors.join(', ') })}
        </Typography>
      )}
    </Alert>
  );
}

/** One notice per refused or narrowed delegation of the answer, in the order they happened. */
export function DelegationNotices({ notices }: { readonly notices: readonly DelegationNotice[] | undefined }): ReactNode {
  if (notices === undefined || notices.length === 0) return null;
  return (
    <Box data-testid="delegation-notices" sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, mb: 0.5 }}>
      {notices.map((notice) => (
        <Notice key={notice.id} notice={notice} />
      ))}
    </Box>
  );
}
