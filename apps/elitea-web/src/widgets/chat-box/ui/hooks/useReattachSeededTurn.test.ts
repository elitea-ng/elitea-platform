import { renderHook } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import type { ChatMessage } from '@/features/chat-messages';

import { useReattachSeededTurn } from './useReattachSeededTurn';

const AT = '2026-10-03T00:00:00.000Z';
const question: ChatMessage = { id: 'q1', role: 'user', name: 'Alice', content: 'hi', createdAt: AT };
const inFlight: ChatMessage = {
  id: 'a1', role: 'assistant', name: 'Agent', content: '...', createdAt: AT,
  isStreaming: true, isLoading: true, taskId: 'exec-1', questionId: 'q1',
};

describe('useReattachSeededTurn (#6654)', () => {
  it('reattaches a seeded in-flight turn once per conversation and execution', () => {
    const reattach = vi.fn(() => true);
    const { rerender } = renderHook((props: { messages: readonly ChatMessage[] }) => useReattachSeededTurn({
      messages: props.messages, conversationUuid: 'uuid-1', reattach,
    }), { initialProps: { messages: [question, inFlight] } });

    expect(reattach).toHaveBeenCalledExactlyOnceWith({ executionId: 'exec-1', messageId: 'a1', questionId: 'q1' });
    rerender({ messages: [question, { ...inFlight, content: '' }] });
    expect(reattach).toHaveBeenCalledTimes(1);
  });

  it('does nothing for a settled transcript or an unsaved chat', () => {
    const reattach = vi.fn(() => true);
    renderHook(() => useReattachSeededTurn({ messages: [question, { ...inFlight, isStreaming: false }], conversationUuid: 'uuid-1', reattach }));
    renderHook(() => useReattachSeededTurn({ messages: [question, inFlight], conversationUuid: undefined, reattach }));
    expect(reattach).not.toHaveBeenCalled();
  });
});
