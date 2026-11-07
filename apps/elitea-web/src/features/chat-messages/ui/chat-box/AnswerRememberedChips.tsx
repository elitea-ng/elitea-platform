/**
 * The "remembered" chips under an agent's answer (ADR-0050 decision 1b,
 * issue 1608): the agent-memory slice renders them; the chat lends it the
 * trace-step body loader so that slice does not import this one.
 */
import type { ReactNode } from 'react';

import { RememberedChips, type RememberToolAction } from '@/features/agent-memory';

import { useTraceStepDetail } from '../../model/traceStepDetail';

export interface AnswerRememberedChipsProps {
  readonly toolActions: readonly RememberToolAction[];
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  readonly agentName: string | undefined;
}

export function AnswerRememberedChips({ toolActions, projectId, applicationId, agentName }: AnswerRememberedChipsProps): ReactNode {
  return <RememberedChips toolActions={toolActions} projectId={projectId} applicationId={applicationId} agentName={agentName} useStepBody={useTraceStepDetail} />;
}
