import { describe, expect, it } from 'vitest';
import { screen } from '@testing-library/react';

import { parseDelegationNotice } from '@elitea/client-core/stream';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import type { ChatMessage } from '../../lib/convertMessagesToChatHistory.types';
import { ApplicationAnswer } from './ApplicationAnswer';

const notice = parseDelegationNotice({
  id: 'call-1',
  kind: 'refused',
  reason: 'no_grant',
  code: 'DELEGATION_GRANT_MISSING',
  edge: { caller_id: 2, caller_name: 'Agent2', callee_id: 1, callee_name: 'Agent1', kind: 'agent_tool' },
  missing: [{ kind: 'toolkit', name: 'git' }],
  can_review: true,
});

describe('the answer', () => {
  it('shows its notices beside its sub-agents', () => {
    const answer: ChatMessage = {
      id: 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee',
      role: 'assistant',
      name: 'Agent2',
      content: 'I could not ask Agent1.',
      createdAt: '',
      ...(notice === undefined ? {} : { delegationNotices: [notice] }),
    };
    renderWithTheme(<ApplicationAnswer answer={answer} messageId={answer.id} />);
    expect(screen.getByTestId('delegation-notices')).toHaveTextContent('Agent1 could not use git from Agent2. This delegation is not approved.');
    expect(screen.getByRole('link', { name: 'Review delegation' })).toBeInTheDocument();
  });
});
