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
  onHitlResume,
  tools,
  extraCards = [],
  extraBuckets = [],
}: AnswerApprovalsProps): ReactNode {
  const renderHitlCard = ({ interrupt, index }: { readonly interrupt: HitlInterrupt; readonly index: number }): ReactNode => {
    const interruptId = getInterruptIdentity(interrupt as Readonly<Record<string, unknown>>);
    return (
      <ChatHitlActions
        key={interruptId || `hitl-${index}`}
        hitlInterrupt={interrupt}
        toolCallId={interrupt.tool_call_id ?? ''}
        interruptId={interruptId}
        onHitlResume={onHitlResume}
        disabled={!onHitlResume || Boolean(interrupt.decided) || Boolean(interrupt.queued)}
      />
    );
  };
  const extrasFor = (instanceKey: string): ReactNode =>
    extraCards.filter((card) => card.instanceKey === instanceKey).map((card) => <Fragment key={card.key}>{card.node}</Fragment>);
  const hasSubAgents = hitlBuckets.hasSubAgents || extraBuckets.length > 0;

  return (
    <>
      {authorizationBuckets.coordinator.map(renderAuthorizationCard)}
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
