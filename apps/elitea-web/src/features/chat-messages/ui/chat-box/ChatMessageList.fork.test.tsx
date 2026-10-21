/**
 * "Fork from here" (client contract 1.11) on the message rows: offered on
 * user and assistant messages that are settled, withheld on a reply still
 * streaming or waiting for an approval, and absent when the host wires no
 * fork handler.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ThemeProvider } from '@mui/material/styles';
import { fireEvent, render, screen, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

import type { ChatMessage } from '../../lib/convertMessagesToChatHistory';
import { ChatMessageList } from './ChatMessageList';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function renderList(chatHistory: readonly ChatMessage[], onForkFromMessage?: (messageId: string) => void) {
  Element.prototype.scrollIntoView = vi.fn();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  render(
    <QueryClientProvider client={queryClient}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <ChatMessageList chatHistory={chatHistory} userId="6" messageActions={onForkFromMessage ? { onForkFromMessage } : {}} />
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

const settled = [
  { id: 'q1', role: 'user', name: 'Reader', userId: '6', content: 'first question', createdAt: '2026-10-20T10:00:00Z' },
  { id: 'a1', role: 'assistant', name: 'agent', content: 'first answer', createdAt: '2026-10-20T10:00:01Z' },
  { id: 'q2', role: 'user', name: 'Reader', userId: '6', content: 'second question', createdAt: '2026-10-20T10:01:00Z' },
] as unknown as readonly ChatMessage[];

describe('ChatMessageList — Fork from here', () => {
  it('forks from a user message and from an assistant message, naming the fork point', () => {
    const onFork = vi.fn();
    renderList(settled, onFork);
    const rows = screen.getAllByTestId('chat-message-item');
    expect(screen.getAllByRole('button', { name: 'Fork from here' })).toHaveLength(3);
    fireEvent.click(within(rows[1] as HTMLElement).getByRole('button', { name: 'Fork from here' }));
    fireEvent.click(within(rows[0] as HTMLElement).getByRole('button', { name: 'Fork from here' }));
    expect(onFork.mock.calls).toEqual([['a1'], ['q1']]);
  });

  it('withholds it on a reply that is still streaming or waits for an approval', () => {
    renderList(
      [
        ...settled.slice(0, 2),
        { id: 'a2', role: 'assistant', name: 'agent', content: 'needs approval', createdAt: '2026-10-20T10:00:02Z', hitlInterrupt: { id: 'h1' } },
        { id: 'q3', role: 'user', name: 'Reader', userId: '6', content: 'go', createdAt: '2026-10-20T10:02:00Z' },
        { id: 'a3', role: 'assistant', name: 'agent', content: 'partial', createdAt: '2026-10-20T10:02:01Z', isStreaming: true },
      ] as unknown as readonly ChatMessage[],
      vi.fn(),
    );
    const rows = screen.getAllByTestId('chat-message-item');
    const forkIn = (index: number) => within(rows[index] as HTMLElement).queryByRole('button', { name: 'Fork from here' });
    expect(forkIn(0)).not.toBeNull();
    expect(forkIn(1)).not.toBeNull();
    expect(forkIn(2)).toBeNull();
    expect(forkIn(3)).not.toBeNull();
    expect(forkIn(4)).toBeNull();
  });

  it('shows no fork control when the host wires no fork handler', () => {
    renderList(settled);
    expect(screen.queryByRole('button', { name: 'Fork from here' })).toBeNull();
  });
});
