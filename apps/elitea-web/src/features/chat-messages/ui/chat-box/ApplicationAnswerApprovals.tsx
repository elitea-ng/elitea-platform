/**
 * The approval area of one answer: the derived buckets (reference
 * `useApplicationAnswerState.hooks.js:273-352`) and the `AnswerApprovals`
 * render with its card factories. Split out of `ApplicationAnswer.tsx` for
 * the §3.5 file-length budget.
 */
import type { ReactNode } from 'react';
import { useMemo } from 'react';

import { t } from '@/shared/i18n';

import { buildHitlBuckets, groupToolkitAuthorizationActions } from '../../lib/approvalBuckets';
import { getSubAgentInstanceKey } from '../../lib/executionHierarchy';
import { buildStaticPauseCards } from '../../lib/staticPauseCards';
import type { SubAgentGroupable } from '../../lib/subAgentGrouping';
import type { HitlInterrupt, HitlResumePayload } from '../chat-hitl-actions/ChatHitlActions';
import type { ApplicationAnswerContinuation, ApplicationAnswerStaticPause } from './ApplicationAnswer.types';
import { AuthorizationCard, authorizationRequestId } from './ApplicationAnswerAuthorization';
import { AnswerApprovals } from './AnswerApprovals';
import { StaticPauseCard } from './StaticPauseCard';

interface ApprovalModelInput {
  readonly hitlInterrupt: unknown;
  readonly hitlInterrupts: readonly unknown[] | undefined;
  readonly toolActions: readonly SubAgentGroupable[];
  readonly authRequiredActions: readonly SubAgentGroupable[];
  readonly staticPause: ApplicationAnswerStaticPause | undefined;
}

/** Per-interrupt sign-in cards render in the authorization section, as the reference places authorization. */
function isLedgerAuth(interrupt: HitlInterrupt): boolean {
  return interrupt.ledger_kind === 'delegated_auth';
}

/** Everything the answer derives about its pending cards. */
export function useAnswerApprovalModel({ hitlInterrupt, hitlInterrupts, toolActions, authRequiredActions, staticPause }: ApprovalModelInput) {
  const effective = useMemo<readonly HitlInterrupt[]>(() => {
    const list = Array.isArray(hitlInterrupts) && hitlInterrupts.length > 0 ? hitlInterrupts : hitlInterrupt ? [hitlInterrupt] : [];
    return list as readonly HitlInterrupt[];
  }, [hitlInterrupt, hitlInterrupts]);
  // A queued/decided card is hidden once its decision is recorded; the rest stay answerable.
  const visibleHitlInterrupts = useMemo(() => effective.filter((interrupt) => !interrupt.hidden), [effective]);
  const hitlBuckets = useMemo(
    () => buildHitlBuckets(visibleHitlInterrupts.filter((interrupt) => !isLedgerAuth(interrupt)), toolActions),
    [visibleHitlInterrupts, toolActions],
  );
  const ledgerAuthorizationBuckets = useMemo(
    () => buildHitlBuckets(visibleHitlInterrupts.filter(isLedgerAuth), toolActions),
    [visibleHitlInterrupts, toolActions],
  );
  const authorizationBuckets = useMemo(() => groupToolkitAuthorizationActions(authRequiredActions, toolActions), [authRequiredActions, toolActions]);
  // Instances whose card is still owed an answer stop shimmering (`paused`).
  const pendingInstanceKeys = useMemo(
    () => new Set(effective.filter((interrupt) => !interrupt.decided).map((interrupt) => getSubAgentInstanceKey(interrupt as Readonly<Record<string, unknown>>)).filter(Boolean)),
    [effective],
  );
  const staticCards = useMemo(
    () => buildStaticPauseCards(staticPause?.pause, toolActions, t('features.chatMessages.staticPause.pipeline', 'Pipeline')),
    [staticPause?.pause, toolActions],
  );
  return { visibleHitlInterrupts, hitlBuckets, ledgerAuthorizationBuckets, authorizationBuckets, pendingInstanceKeys, staticCards };
}

type ApprovalModel = ReturnType<typeof useAnswerApprovalModel>;

/** @public Props for `ApplicationAnswerApprovals`. */
export interface ApplicationAnswerApprovalsProps {
  readonly model: ApprovalModel;
  readonly messageId: string;
  readonly onHitlResume?: ((payload: HitlResumePayload) => void) | undefined;
  readonly staticPause: ApplicationAnswerStaticPause | undefined;
  readonly authorization: Pick<ApplicationAnswerContinuation, 'onContinueMcpExecution' | 'renderAuthModal'>;
  readonly confirmation: ReactNode;
}

export function ApplicationAnswerApprovals({ model, messageId, onHitlResume, staticPause, authorization, confirmation }: ApplicationAnswerApprovalsProps): ReactNode {
  const { hitlBuckets, ledgerAuthorizationBuckets, authorizationBuckets, staticCards } = model;
  return (
    <AnswerApprovals
      authorizationBuckets={authorizationBuckets}
      renderAuthorizationCard={(action, index) => (
        <AuthorizationCard
          key={authorizationRequestId(action as SubAgentGroupable) ?? `authorization-${index}`}
          action={action as SubAgentGroupable}
          messageId={messageId}
          continuation={authorization}
        />
      )}
      confirmation={confirmation}
      hitlBuckets={hitlBuckets}
      ledgerAuthorizationBuckets={ledgerAuthorizationBuckets}
      renderAuthModal={authorization.renderAuthModal}
      onHitlResume={onHitlResume}
      extraBuckets={staticCards.buckets.filter((bucket) => !hitlBuckets.subAgents.some((hitl) => hitl.instanceKey === bucket.instanceKey))}
      extraCards={staticCards.cards.map((card) => ({
        key: card.key,
        instanceKey: card.instanceKey,
        node: (
          <StaticPauseCard
            nodeName={card.nodeName}
            disabled={!staticPause || staticPause.busy}
            error={staticPause?.error}
            onContinue={() => staticPause?.onContinue(card.pauseId)}
          />
        ),
      }))}
    />
  );
}
