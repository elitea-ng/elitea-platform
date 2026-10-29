/**
 * Port of EliteaUI `features/chat/ui/chat-box/application-answer/AnswerApprovals.jsx`.
 *
 * Every card that hands the turn to a human renders here, inside the answer
 * block below the thinking view, in the reference order:
 *   1. coordinator toolkit-authorization cards, then one `SubAgentAccordion`
 *      per delegated-authorization instance;
 *   2. the token-limit confirmation `ChatContinue`;
 *   3. HITL cards (sensitive tool, ask_user, HITL node, static pause):
 *      coordinator cards plain, then one `SubAgentAccordion` (`paused`,
 *      `defaultExpanded`, `transparent`) per spawning agent/pipeline instance.
 *
 * Each HITL card is keyed by `getInterruptIdentity` and disabled while its
 * decision is `decided` or `queued`; hidden ones never reach this component.
 * Fan-out members (Parallel branches, Map items) reuse the same buckets.
 */
import type { ReactNode } from 'react';
import { Fragment } from 'react';

import Box from '@mui/material/Box';

import type { AuthorizationBuckets, HitlBuckets } from '../../lib/approvalBuckets';
import { getInterruptIdentity } from '../../lib/hitlInterrupts';
import type { HitlInterrupt, HitlResumePayload } from '../chat-hitl-actions/ChatHitlActions';
import { ChatHitlActions } from '../chat-hitl-actions/ChatHitlActions';
import { SubAgentAccordion } from '../sub-agent-section/SubAgentAccordion';
import type { SubAgentTool } from '../sub-agent-section/subAgentIcon.helpers';
import { ChatContinue, type ChatContinueProps } from '../chat-continue/ChatContinue';
import { t } from '@/shared/i18n';
import { StaticPauseCard } from './StaticPauseCard';

/** One extra card owned by a bucket instance (static pause) — rendered after that instance's HITL cards. */
interface ApprovalExtraCard {
  readonly key: string;
  /** `''` = coordinator; otherwise the HITL bucket instance key it belongs to. */
  readonly instanceKey: string;
  readonly node: ReactNode;
}

/** @public Props for `AnswerApprovals`. */
export interface AnswerApprovalsProps {
  readonly authorizationBuckets: AuthorizationBuckets<unknown>;
  readonly renderAuthorizationCard: (action: unknown, index: number) => ReactNode;
  readonly confirmation?: ReactNode;
  readonly hitlBuckets: HitlBuckets<HitlInterrupt>;
  /**
   * Per-interrupt delegated-auth cards (fan-out ledger), bucketed like the HITL
   * cards and rendered where the reference renders authorization: first.
   */
  readonly ledgerAuthorizationBuckets?: HitlBuckets<HitlInterrupt> | undefined;
  readonly renderAuthModal?: ChatContinueProps['renderAuthModal'];
  readonly onHitlResume?: ((payload: HitlResumePayload) => void) | undefined;
  readonly tools?: readonly SubAgentTool[] | undefined;
  /** Static-pause cards with their owning bucket (or extra buckets they open). */
  readonly extraCards?: readonly ApprovalExtraCard[];
  /** Buckets that exist only for extra cards (no HITL interrupt of their own). */
  readonly extraBuckets?: readonly { readonly instanceKey: string; readonly name: string; readonly label?: string | undefined; readonly agentType?: string }[];
}

const groupsSx = { display: 'flex', flexDirection: 'column', gap: '0.75rem', width: '100%' } as const;

export function AnswerApprovals({
  authorizationBuckets,
  renderAuthorizationCard,
  confirmation,
  hitlBuckets,
  ledgerAuthorizationBuckets,
  renderAuthModal,
  onHitlResume,
  tools,
  extraCards = [],
  extraBuckets = [],
}: AnswerApprovalsProps): ReactNode {
  const renderHitlCard = ({ interrupt, index }: { readonly interrupt: HitlInterrupt; readonly index: number }): ReactNode => {
    const interruptId = getInterruptIdentity(interrupt as Readonly<Record<string, unknown>>);
    const disabled = !onHitlResume || Boolean(interrupt.decided) || Boolean(interrupt.queued);
    // A per-interrupt static pause (`continue`) uses the static-pause card; a
    // per-interrupt sign-in uses the bare authorization `ChatContinue`.
    if (interrupt.ledger_kind === 'hitl_node' && interrupt.available_actions?.includes('continue')) {
      return (
        <StaticPauseCard
          key={interruptId || `hitl-${index}`}
          nodeName={interrupt.node_name ?? ''}
          disabled={disabled}
          onContinue={() => onHitlResume?.({ action: 'continue', interruptId })}
        />
      );
    }
    if (interrupt.ledger_kind === 'delegated_auth') {
      return (
        <ChatContinue
          key={interruptId || `hitl-${index}`}
          authRequired
          disabled={disabled}
          onContinueWithoutAuth={() => onHitlResume?.({ action: 'skip', interruptId })}
          onAuthSuccess={() => onHitlResume?.({ action: 'authorize', interruptId })}
          authRequiredAction={{ id: interruptId, authorizationRequestId: interruptId, ...(interrupt.tool_name ? { name: interrupt.tool_name } : {}), toolMeta: interrupt.server_url ? { server_url: interrupt.server_url } : {} }}
          renderAuthModal={renderAuthModal}
          message={interrupt.message}
          continueLabel={t('features.chatMessages.authorization.skip', 'Skip')}
        />
      );
    }
    return (
      <ChatHitlActions
        key={interruptId || `hitl-${index}`}
        hitlInterrupt={interrupt}
        toolCallId={interrupt.tool_call_id ?? ''}
        interruptId={interruptId}
        onHitlResume={onHitlResume}
        disabled={disabled}
      />
    );
  };
  const extrasFor = (instanceKey: string): ReactNode =>
    extraCards.filter((card) => card.instanceKey === instanceKey).map((card) => <Fragment key={card.key}>{card.node}</Fragment>);
  const hasSubAgents = hitlBuckets.hasSubAgents || extraBuckets.length > 0;

  return (
    <>
      {authorizationBuckets.coordinator.map(renderAuthorizationCard)}
      {ledgerAuthorizationBuckets?.coordinator.map(renderHitlCard)}
      {authorizationBuckets.subAgents.map((bucket) => (
        <SubAgentAccordion
          key={`authorization-sa-${bucket.instanceKey}`}
          name={bucket.name}
          label={bucket.label}
          tools={tools}
          agentType={bucket.agentType}
          paused
          defaultExpanded
          transparent
        >
          {bucket.actions.map(renderAuthorizationCard)}
        </SubAgentAccordion>
      ))}
      {ledgerAuthorizationBuckets?.subAgents.map((bucket) => (
        <SubAgentAccordion
          key={`ledger-authorization-sa-${bucket.instanceKey}`}
          name={bucket.name}
          label={bucket.label}
          tools={tools}
          agentType={bucket.agentType}
          paused
          defaultExpanded
          transparent
        >
          {bucket.entries.map(renderHitlCard)}
        </SubAgentAccordion>
      ))}

      {confirmation}

      {hasSubAgents ? (
        <Box sx={groupsSx}>
          {hitlBuckets.coordinator.map(renderHitlCard)}
          {extrasFor('')}
          {hitlBuckets.subAgents.map((bucket) => (
            <SubAgentAccordion
              key={`hitl-sa-${bucket.instanceKey}`}
              name={bucket.name}
              label={bucket.label}
              tools={tools}
              agentType={bucket.agentType}
              paused
              defaultExpanded
              transparent
            >
              {bucket.entries.map(renderHitlCard)}
              {extrasFor(bucket.instanceKey)}
            </SubAgentAccordion>
          ))}
          {extraBuckets.map((bucket) => (
            <SubAgentAccordion
              key={`static-sa-${bucket.instanceKey}`}
              name={bucket.name}
              label={bucket.label}
              tools={tools}
              agentType={bucket.agentType}
              paused
              defaultExpanded
              transparent
            >
              {extrasFor(bucket.instanceKey)}
            </SubAgentAccordion>
          ))}
        </Box>
      ) : (
        <>
          {hitlBuckets.coordinator.map(renderHitlCard)}
          {extrasFor('')}
        </>
      )}
    </>
  );
}
