import type { ReactNode } from 'react';
import { useCallback, useMemo, useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import type { DelegationGrant, DelegationItem, DelegationPreview } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';

import { fetchDelegationReview, invalidateDelegationGrants, settleDelegation } from '../api/delegationGrants';
import { grantView, grantViewLabel } from '../lib/delegationReview';
import type { GrantView } from '../lib/delegationReview';

import { DelegationReviewDialog } from './DelegationReviewDialog';

/**
 * The grant state of one attached agent or pipeline, on its row, with the
 * "Review delegation" action (ADR-0044 amendment 1, A1.2 rule 2; epic 2156
 * story S3): approved, awaiting approval (a governed change request is open),
 * delegation not approved (a pending grant an editor can approve now) or
 * needs re-approval (the callee changed after approval).
 *
 * The review opens the same dialog as the attach. What it can decide follows
 * the state: a pending grant is approved here, and a stale one is re-approved
 * against what the callee holds now (S4: the old grant is kept as superseded).
 * A governed grant is decided in its change request by a different approver, so
 * it opens read-only; a governed stale grant records that request when the
 * person re-approves. A read-only viewer sees what is covered and why they
 * cannot act.
 */
export interface DelegationGrantStatusProps {
  readonly projectId: string;
  /** The caller: the agent or pipeline whose Tools panel this is. */
  readonly applicationId: number;
  readonly grant: DelegationGrant;
  /** A read-only viewer, or a locked version, may look but not approve. */
  readonly readOnly: boolean;
}

interface Review {
  readonly preview: DelegationPreview;
  readonly names: ReadonlyMap<string, string>;
  readonly calleeName: string;
}

/** What the dialog lists for a grant: what was approved once it is decided, what is asked while it is pending. */
function previewFor(grant: DelegationGrant, current: DelegationPreview, canAct: boolean): DelegationPreview {
  if (grant.state === 'pending' || (grant.state === 'stale' && canAct)) return current;
  return { ...current, items: grant.requested_items, approvable: [], withheld: grant.omitted };
}

function noticeFor(grant: DelegationGrant, view: GrantView): string | undefined {
  if (view === 'awaiting') {
    return t('agents.delegation.noticeAwaiting', 'A different approver decides this delegation in its change request. It is not approved yet, so the agent runs without these.');
  }
  if (view === 'stale') {
    return t('agents.delegation.noticeStale', 'This agent changed after the delegation was approved. It keeps using only what was approved, and anything new is left out until it is approved again.');
  }
  if (view === 'approved' && grant.omitted.length > 0) {
    return t('agents.delegation.noticeOmitted', 'Some items were left out of the approval, so the agent runs without them.');
  }
  return undefined;
}

async function loadReview(queryClient: ReturnType<typeof useQueryClient>, projectId: string, applicationId: number, grant: DelegationGrant, canAct: boolean): Promise<Review> {
  // A stale grant is re-approved against the version the drift was read from.
  const driftVersion = grant.drift === undefined ? undefined : grant.drift.callee_version_id;
  const calleeVersionId = grant.state === 'stale' && canAct && driftVersion !== undefined ? driftVersion : grant.callee_version_id;
  const request = await fetchDelegationReview({
    queryClient,
    projectId,
    applicationId,
    calleeId: grant.callee_application_id,
    calleeName: '',
    calleeVersionId,
    edgeKind: grant.edge_kind,
  });
  if (request === null) throw new Error('the delegation has no callee version');
  return { preview: previewFor(grant, request.preview, canAct), names: request.names, calleeName: request.calleeName };
}

export function DelegationGrantStatus({ projectId, applicationId, grant, readOnly }: DelegationGrantStatusProps): ReactNode {
  const queryClient = useQueryClient();
  const view = grantView(grant);
  const [review, setReview] = useState<Review>();
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  const canApprove = (view === 'pending' || view === 'stale') && !readOnly;
  const open = useCallback(() => {
    setLoading(true);
    setError(undefined);
    loadReview(queryClient, projectId, applicationId, grant, canApprove)
      .then(setReview)
      .catch(() => setError(t('agents.delegation.reviewFailed', 'The delegation could not be loaded.')))
      .finally(() => setLoading(false));
  }, [queryClient, projectId, applicationId, grant, canApprove]);

  const close = useCallback(() => {
    setReview(undefined);
    setError(undefined);
  }, []);

  const approve = useCallback(
    (items: readonly DelegationItem[] | undefined) => {
      setBusy(true);
      void settleDelegation({ projectId, applicationId, calleeId: grant.callee_application_id, items })
        .then(async (outcome) => {
          if (outcome.kind === 'failed') return setError(outcome.message);
          await invalidateDelegationGrants(queryClient, projectId, applicationId);
          close();
        })
        .finally(() => setBusy(false));
    },
    [projectId, applicationId, grant.callee_application_id, queryClient, close],
  );

  const confirmLabel = useMemo(() => {
    if (!canApprove) return undefined;
    return view === 'stale' ? t('agents.delegation.reapprove', 'Re-approve') : t('agents.delegation.approve', 'Approve');
  }, [canApprove, view]);

  return (
    <Box
      data-testid="delegation-grant-status"
      data-grant-state={view}
      sx={{ display: 'flex', alignItems: 'center', flexWrap: 'wrap', gap: 1 }}
    >
      <Typography
        variant="labelSmall"
        color={view === 'approved' ? 'text.secondary' : 'text.warningText'}
      >
        {grantViewLabel(view)}
      </Typography>
      <BaseBtn
        variant="text"
        onClick={open}
        disabled={loading}
        data-testid="delegation-review-button"
      >
        {t('agents.delegation.review', 'Review delegation')}
      </BaseBtn>
      {error !== undefined && review === undefined ? (
        <Typography
          role="alert"
          variant="bodySmall"
        >
          {error}
        </Typography>
      ) : null}
      {review === undefined ? null : (
        <DelegationReviewDialog
          open
          calleeName={review.calleeName}
          preview={review.preview}
          names={review.names}
          confirmLabel={confirmLabel}
          notice={noticeFor(grant, view)}
          omittedReason={grant.state === 'pending' || (grant.state === 'stale' && canApprove) ? undefined : t('agents.delegation.omittedReason', 'Left out of the approval, so the agent runs without it.')}
          busy={busy}
          error={error}
          onCancel={close}
          onConfirm={approve}
        />
      )}
    </Box>
  );
}
