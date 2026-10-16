import type { ReactNode } from 'react';

import type { SubAgentGroupable } from '../../lib/subAgentGrouping';
import { ChatContinue } from '../chat-continue/ChatContinue';
import type { McpAuthRequiredAction } from '../chat-continue/ChatContinue';
import { t } from '@/shared/i18n';
import type { ApplicationAnswerContinuation } from './ApplicationAnswer.types';
import { asDraft } from './ApplicationAnswerThinking';

/** The exact authorization request a card answers. */
export function authorizationRequestId(action: SubAgentGroupable): string | undefined {
  const draft = asDraft(action);
  const metadata = (draft.toolMeta ?? {}) as Record<string, unknown>;
  const value = draft.authorizationRequestId ?? metadata['authorization_request_id'] ?? metadata['interrupt_id'] ?? draft.id;
  return typeof value === 'string' && value !== '' ? value : undefined;
}

/**
 * One toolkit authorization card: a bare `ChatContinue`, exactly the
 * reference `renderAuthorizationCard` (`ApplicationAnswer.jsx:140-152`) —
 * no extra wrapper, heading or copy. Placement is `AnswerApprovals`'.
 */
export function AuthorizationCard({ action, messageId, continuation }: {
  readonly action: SubAgentGroupable;
  readonly messageId: string;
  readonly continuation: Pick<ApplicationAnswerContinuation, 'onContinueMcpExecution' | 'renderAuthModal'>;
}): ReactNode {
  const { onContinueMcpExecution, renderAuthModal } = continuation;
  const requestId = authorizationRequestId(action);
  return (
    <ChatContinue
      authRequired
      disabled={!onContinueMcpExecution || !requestId}
      onContinueWithoutAuth={() => { onContinueMcpExecution?.(messageId, true, requestId); }}
      onAuthSuccess={() => { onContinueMcpExecution?.(messageId, false, requestId); }}
      authRequiredAction={action as unknown as McpAuthRequiredAction}
      renderAuthModal={renderAuthModal}
      continueLabel={t('features.chatMessages.authorization.skip', 'Skip')}
    />
  );
}
