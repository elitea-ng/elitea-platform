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

  it('suppresses the redundant generic authorization error banner when a refused delegation notice explains the refusal', () => {
    const pipelineNodeNotice = parseDelegationNotice({
      id: 'pipeline_node:delegate',
      kind: 'refused',
      reason: 'no_grant',
      code: 'DELEGATION_GRANT_MISSING',
      edge: { caller_id: 174, caller_name: 'P0r refusal', callee_id: 171, callee_name: 'delegate', kind: 'pipeline_node' },
      missing: [{ kind: 'toolkit', name: 'c1mockmcp' }],
      can_review: true,
    });
    const answer: ChatMessage = {
      id: 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee',
      role: 'assistant',
      name: 'P0r refusal',
      content: '',
      createdAt: '',
      exception: 'Execution authorization failed.',
      failureCode: 'AUTHORIZATION_FAILED',
      ...(pipelineNodeNotice === undefined ? {} : { delegationNotices: [pipelineNodeNotice] }),
    };
    renderWithTheme(<ApplicationAnswer answer={answer} messageId={answer.id} />);
    expect(screen.getByTestId('delegation-notices')).toHaveTextContent('delegate could not use c1mockmcp from P0r refusal. This delegation is not approved.');
    expect(screen.getByRole('link', { name: 'Review delegation' })).toHaveAttribute('href', '/pipelines/all/174');
    expect(screen.queryByTestId('error-trace')).not.toBeInTheDocument();
    expect(screen.getByTestId('failure-reference')).toHaveTextContent('Error code: AUTHORIZATION_FAILED');
  });

  it('shows a refusal banner (not a generic red error) for a typed refusal without a delegation notice, and keeps real errors red', () => {
    const refusal: ChatMessage = {
      id: 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee',
      role: 'assistant',
      name: 'Agent',
      content: '',
      createdAt: '',
      exception: 'Configuration type is not supported.',
      failureCode: 'UNSUPPORTED_CAPABILITY',
    };
    const { rerender } = renderWithTheme(<ApplicationAnswer answer={refusal} messageId={refusal.id} />);
    const refusalBanner = screen.getByTestId('error-trace');
    expect(refusalBanner).toHaveAttribute('data-outcome', 'refusal');
    expect(refusalBanner).toHaveTextContent('Request refused');
    expect(refusalBanner).toHaveTextContent('Configuration type is not supported.');

    const runtimeError: ChatMessage = {
      ...refusal,
      exception: 'The runtime operation failed.',
      failureCode: 'INTERNAL',
    };
    rerender(<ApplicationAnswer answer={runtimeError} messageId={runtimeError.id} />);
    const errorBanner = screen.getByTestId('error-trace');
    expect(errorBanner).toHaveAttribute('data-outcome', 'error');
    expect(errorBanner).toHaveTextContent('The runtime operation failed.');
  });
});
