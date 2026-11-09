/**
 * What an agent's answer did to agents and memory, under the answer:
 *
 * - the "remembered" chips (ADR-0050 decision 1b, issue 1608), rendered by
 *   the agent-memory slice;
 * - the proposal cards (ADR-0050 decision 5a, issue 1615), rendered by the
 *   agent-proposals slice.
 *
 * The chat lends both the trace-step body loader so neither slice imports
 * this one.
 */
import type { ReactNode } from 'react';

import { RememberedChips, type RememberToolAction } from '@/features/agent-memory';
import { ProposalCards } from '@/features/agent-proposals';

import { useTraceStepDetail } from '../../model/traceStepDetail';

export interface AnswerRememberedChipsProps {
  readonly toolActions: readonly RememberToolAction[];
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  readonly agentName: string | undefined;
}

export function AnswerRememberedChips({ toolActions, projectId, applicationId, agentName }: AnswerRememberedChipsProps): ReactNode {
  return (
    <>
      <RememberedChips toolActions={toolActions} projectId={projectId} applicationId={applicationId} agentName={agentName} useStepBody={useTraceStepDetail} />
      <ProposalCards toolActions={toolActions} projectId={projectId} agentName={agentName} useStepBody={useTraceStepDetail} />
    </>
  );
}
