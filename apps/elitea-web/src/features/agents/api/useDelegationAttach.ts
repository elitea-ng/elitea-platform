import { useCallback, useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import type { DelegationItem } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import type { FilterableToolRef } from '../lib/useFilterAddedItems';

import { invalidateDelegationGrants, loadDelegationReview, settleDelegation } from './delegationGrants';
import type { DelegationReviewRequest } from './delegationGrants';
import type { AssociateAgentOutcome, AssociationCandidate } from './useAgentPipelineAssociation';

/**
 * The attach of an agent or pipeline X to Y through the review of what X
 * brings (ADR-0044 amendment 1, A1.2 rule 2; epic 2156 story S3).
 *
 * `begin` reads what the attach would let X use. When there is something to
 * review it parks the request for the dialog; otherwise it attaches at once,
 * exactly as before. `confirm` attaches, then approves the grant the attach
 * proposed with the items the editor kept (under the governed profile the grant
 * is a change request another approver decides, so nothing is approved and the
 * row says "Awaiting approval"). An approval that fails never undoes the
 * attach: the grant stays pending, the row says so, and the message says why.
 */
interface AttachParams {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  readonly parentIsPipeline: boolean;
  readonly currentTools: readonly FilterableToolRef[];
  readonly associate: (candidate: AssociationCandidate, options: { readonly isPipeline: boolean; readonly currentTools: readonly FilterableToolRef[] }) => Promise<AssociateAgentOutcome>;
  /** Told the outcome of the attach itself, as the menu has always been. */
  readonly report: (outcome: AttachReport) => void;
}

/** The outcome of the attach, with what to say when the attach worked but its delegation is still not approved. */
export type AttachReport = AssociateAgentOutcome & { readonly warning?: string };

interface PendingAttach {
  readonly request: DelegationReviewRequest;
  readonly isPipeline: boolean;
}

export interface DelegationAttach {
  readonly begin: (candidate: AssociationCandidate, isPipeline: boolean) => void;
  readonly pending: DelegationReviewRequest | undefined;
  readonly busy: boolean;
  readonly confirm: (items: readonly DelegationItem[] | undefined) => void;
  readonly cancel: () => void;
}

export function useDelegationAttach({ projectId, applicationId, parentIsPipeline, currentTools, associate, report }: AttachParams): DelegationAttach {
  const queryClient = useQueryClient();
  const [pending, setPending] = useState<PendingAttach>();
  const [busy, setBusy] = useState(false);

  const attach = useCallback(
    async (candidate: AssociationCandidate, isPipeline: boolean) => {
      const outcome = await associate(candidate, { isPipeline, currentTools });
      if (projectId !== undefined && applicationId !== undefined) await invalidateDelegationGrants(queryClient, projectId, applicationId);
      return outcome;
    },
    [associate, currentTools, projectId, applicationId, queryClient],
  );

  const begin = useCallback(
    (candidate: AssociationCandidate, isPipeline: boolean) => {
      const review = projectId === undefined || applicationId === undefined ? Promise.resolve(null) : loadDelegationReview({ queryClient, projectId, applicationId, candidate, parentIsPipeline });
      void review.then(async (request) => {
        if (request === null) return report(await attach(candidate, isPipeline));
        setPending({ request, isPipeline });
      });
    },
    [projectId, applicationId, queryClient, parentIsPipeline, attach, report],
  );

  const cancel = useCallback(() => {
    setPending(undefined);
  }, []);

  const confirm = useCallback(
    (items: readonly DelegationItem[] | undefined) => {
      if (pending === undefined || projectId === undefined || applicationId === undefined) return;
      const { request, isPipeline } = pending;
      setBusy(true);
      void (async () => {
        const outcome = await attach({ id: request.calleeId, name: request.calleeName }, isPipeline);
        setPending(undefined);
        if (!outcome.ok) return report(outcome);
        const settled = await settleDelegation({ projectId, applicationId, calleeId: request.calleeId, items });
        await invalidateDelegationGrants(queryClient, projectId, applicationId);
        if (settled.kind === 'failed') {
          return report({
            ...outcome,
            warning: t('agents.delegation.attachedNotApproved', 'Added, but the delegation is not approved: {{reason}} Use Review delegation on the row to try again.', { reason: settled.message }),
          });
        }
        report(outcome);
      })().finally(() => setBusy(false));
    },
    [pending, projectId, applicationId, attach, queryClient, report],
  );

  return { begin, pending: pending?.request, busy, confirm, cancel };
}
