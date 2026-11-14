/**
 * The guardrail notices under one answer (ADR-0043 decision 1, issue 2012).
 *
 * A message the gateway blocked used to show as a generic runtime error. It
 * now says which guardrail refused it; a message it redacted or warned about
 * says so too. When a notice explains why the turn failed, the generic error
 * (`children`) is left out, because it would only repeat the problem less
 * clearly.
 */
import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';

import { guardrailNoticeExplainsFailure, guardrailNoticeSeverity, guardrailNoticeText } from '../../lib/guardrailNoticeText';
import type { ChatMessage } from '../../lib/convertMessagesToChatHistory.types';
import { useGuardrailNotices } from '../../model/useGuardrailNotices';

export interface AnswerGuardrailsProps {
  readonly answer: Pick<ChatMessage, 'executionId' | 'taskId' | 'exception' | 'failureCode'>;
  readonly projectId: string | undefined;
  /** The turn has ended: nothing is streaming or loading. */
  readonly settled: boolean;
  /** The generic error of a failed answer; shown unless a notice explains the failure. */
  readonly children?: ReactNode;
}

export function AnswerGuardrails({ answer, projectId, settled, children }: AnswerGuardrailsProps): ReactNode {
  const failed = !!answer.exception;
  const notices = useGuardrailNotices({
    projectId, runId: answer.executionId ?? answer.taskId, settled, failed,
    failureCode: answer.failureCode, live: answer.executionId !== undefined,
  });
  const explained = failed && guardrailNoticeExplainsFailure(notices);
  return (
    <>
      {notices.length > 0 && (
        <Box data-testid="guardrail-notices" sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, mt: 1, mb: 0.5 }}>
          {notices.map((notice) => (
            <Alert
              key={`${notice.stage}:${notice.guardrail}:${notice.detector}:${notice.kind}`}
              severity={guardrailNoticeSeverity(notice.kind)}
              variant="outlined"
              data-testid={`guardrail-notice-${notice.kind}`}
            >
              {guardrailNoticeText(notice)}
            </Alert>
          ))}
        </Box>
      )}
      {explained ? null : children}
    </>
  );
}
