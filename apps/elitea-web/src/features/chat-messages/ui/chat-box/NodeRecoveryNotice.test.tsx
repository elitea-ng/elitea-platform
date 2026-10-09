import { describe, expect, it, vi } from 'vitest';
import { screen } from '@testing-library/react';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import receipt from '@/shared/lib/fixtures/node-recovery-required.json';
import { nodeRecoveryBinding } from '@/shared/lib/nodeRecovery';
import type { ChatMessage } from '../../lib/convertMessagesToChatHistory.types';
import { convertMessagesToChatHistory } from '../../lib/convertMessagesToChatHistory';
import type { MessageGroupWire } from '@/entities/message/lib/wire';
import { ApplicationAnswer } from './ApplicationAnswer';

const response = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';
const binding = nodeRecoveryBinding(response, '1', receipt)!;
const answer: ChatMessage = { id: response, role: 'assistant', name: 'Pipeline', content: '', createdAt: '',
  executionGeneration: '1', isStreaming: true, isLoading: false, nodeRecoveryRequired: binding };

describe('paused recovery answer', () => {
  it('displays an exact active persisted receipt after history conversion', () => {
    const restored = convertMessagesToChatHistory([{
      id: 1, uuid: response, role: 'assistant', content: '', is_streaming: true, created_at: '',
      meta: { execution_generation: '1', node_recovery_required_v1: receipt },
    } as MessageGroupWire]);
    const restoredAnswer = restored[0];
    expect(restoredAnswer).toBeDefined();
    renderWithTheme(<ApplicationAnswer answer={restoredAnswer!} messageId={response}
      status={{ isStreaming: true }} />);
    expect(screen.getByRole('alert')).toHaveTextContent('Execution paused');
    expect(screen.queryByText(receipt.activation_id)).not.toBeInTheDocument();
  });

  it('shows the node and timeout without an active spinner or regeneration', () => {
    const onRegenerate = vi.fn();
    renderWithTheme(<ApplicationAnswer answer={answer} messageId={response} isLastMessage
      status={{ isStreaming: true, isLoading: true, isRegenerating: true }} actions={{ onRegenerate }} />);
    expect(screen.getByRole('alert')).toHaveTextContent('Execution paused');
    expect(screen.getByText('Node python, attempt 1 requires operator recovery.')).toBeInTheDocument();
    expect(screen.getByText('The node reached its execution time limit.')).toBeInTheDocument();
    expect(screen.getByText('Contact an operator to recover this execution. You can stop this execution before starting another test or message.')).toBeInTheDocument();
    expect(screen.queryByText(/Operator recovery is not enabled/)).not.toBeInTheDocument();
    expect(screen.queryByText('Loading...')).not.toBeInTheDocument();
    expect(screen.queryByText('Streaming...')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: /Regenerate/ })).toBeDisabled();
    expect(screen.queryByText(receipt.activation_id)).not.toBeInTheDocument();
    expect(screen.queryByText(receipt.graph_thread)).not.toBeInTheDocument();
  });
  it('ignores a stale receipt instead of hiding active loading', () => {
    renderWithTheme(<ApplicationAnswer answer={{ ...answer, executionGeneration: '2', content: 'working' }} messageId={response}
      status={{ isStreaming: true }} />);
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });
  it('does not keep the notice after the response completes', () => {
    renderWithTheme(<ApplicationAnswer answer={{ ...answer, isStreaming: false, content: 'done' }} messageId={response} />);
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.getByText('done')).toBeInTheDocument();
  });
});
