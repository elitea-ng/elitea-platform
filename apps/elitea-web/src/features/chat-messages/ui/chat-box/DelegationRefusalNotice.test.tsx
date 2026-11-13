import { describe, expect, it } from 'vitest';
import { screen } from '@testing-library/react';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import type { ChatMessage } from '../../lib/convertMessagesToChatHistory';
import { ApplicationAnswer } from './ApplicationAnswer';

const SENTENCE = 'This run did not start because a saved agent or pipeline it runs needs a toolkit, tool or other grant that its caller does not hold; attach it to the caller, as the notice above says, or remove it from that agent or pipeline.';

function refused(toolkit: unknown, node = 'agent'): ChatMessage {
  return {
    id: 'answer-1', role: 'assistant', content: '', exception: SENTENCE, failureCode: 'AUTHORIZATION_FAILED',
    delegationRefusal: { code: 'DELEGATION_GRANT_MISSING', node, toolkit },
  } as ChatMessage;
}

describe('DelegationRefusalNotice in an answer', () => {
  it('names the toolkit above the failure text', () => {
    renderWithTheme(<ApplicationAnswer answer={refused('batch')} messageId="answer-1" />);
    const notice = screen.getByTestId('delegation-refusal-notice');
    expect(notice).toHaveTextContent('needs the toolkit "batch"');
    const failure = screen.getByText(SENTENCE);
    expect(notice.compareDocumentPosition(failure) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('shows a pipeline node refusal without a toolkit', () => {
    renderWithTheme(<ApplicationAnswer answer={refused(null, 'pipeline')} messageId="answer-1" />);
    expect(screen.getByTestId('delegation-refusal-notice')).toHaveTextContent('A saved pipeline that a pipeline node of this run runs needs a grant');
  });

  it('renders a name with markup or a link as inert text', () => {
    const { container } = renderWithTheme(<ApplicationAnswer answer={refused('<img src=x onerror=alert(1)><script>alert(2)</script>[Jira](javascript:alert(3))')} messageId="answer-1" />);
    const notice = screen.getByTestId('delegation-refusal-notice');
    expect(notice).toHaveTextContent('needs the toolkit "[Jira](javascript:alert(3))"');
    expect(notice.querySelector('img, script, a')).toBeNull();
    expect(container.innerHTML).not.toContain('onerror');
  });

  it('shows nothing without a failure', () => {
    renderWithTheme(<ApplicationAnswer answer={{ ...refused('batch'), exception: undefined } as ChatMessage} messageId="answer-1" />);
    expect(screen.queryByTestId('delegation-refusal-notice')).toBeNull();
  });
});
