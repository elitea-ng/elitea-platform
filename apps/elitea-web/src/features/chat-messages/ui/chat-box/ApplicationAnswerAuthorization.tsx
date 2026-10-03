import type { ReactNode } from 'react';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import type { SubAgentGroupable } from '../../lib/subAgentGrouping';
import { ChatContinue } from '../chat-continue/ChatContinue';
import type { McpAuthRequiredAction } from '../chat-continue/ChatContinue';
import type { ApplicationAnswerContinuation } from './ApplicationAnswer.types';
import { asDraft } from './ApplicationAnswerThinking';

function authorizationRequestId(action: SubAgentGroupable): string | undefined {
  const draft = asDraft(action);
  const metadata = (draft.toolMeta ?? {}) as Record<string, unknown>;
  const value = draft.authorizationRequestId ?? metadata['authorization_request_id'] ?? metadata['interrupt_id'] ?? draft.id;
  return typeof value === 'string' && value !== '' ? value : undefined;
}

/** Render each scoped toolkit authorization request on its assistant answer. */
export function ApplicationAnswerAuthorization({ actions, messageId, continuation }: {
  readonly actions: readonly SubAgentGroupable[];
  readonly messageId: string;
  readonly continuation: Pick<ApplicationAnswerContinuation, 'onContinueMcpExecution' | 'renderAuthModal'>;
}): ReactNode {
  const { onContinueMcpExecution, renderAuthModal } = continuation;
  return (
    <>
      {actions.map((action, index) => {
        const requestId = authorizationRequestId(action);
        const owner = asDraft(action).parent_agent_name || asDraft(action).name || 'Toolkit';
        return (
          <Box key={requestId ?? `authorization-${index}`} component="section"
            aria-label={`${owner} authorization`} sx={{ p: 1.5, border: 1, borderColor: 'warning.main' }}>
            <Typography variant="labelMedium" component="h6">{owner} — Authorization required</Typography>
            <Typography variant="bodyMedium" component="p">Execution is paused. The protected tool has not run.</Typography>
            <ChatContinue
              authRequired
              disabled={!onContinueMcpExecution || !requestId}
              onContinueWithoutAuth={() => { onContinueMcpExecution?.(messageId, true, requestId); }}
              onAuthSuccess={() => { onContinueMcpExecution?.(messageId, false, requestId); }}
              authRequiredAction={action as unknown as McpAuthRequiredAction}
              renderAuthModal={renderAuthModal}
            />
          </Box>
        );
      })}

    </>
  );
}
