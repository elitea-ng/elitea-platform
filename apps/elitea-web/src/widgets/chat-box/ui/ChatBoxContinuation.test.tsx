/**
 * A13 (ELITEA-0725): `buildChatBoxContinuationProps`'s `renderAuthModal`
 * fills `ChatContinue`'s slot with the real `McpAuthModal` — before this
 * wiring landed, the mid-run "Continue (Auth)" button opened `showAuthModal`
 * internally and the slot stayed `undefined`, so nothing ever rendered.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { buildChatBoxContinuationProps } from './ChatBoxContinuation';

function baseHandlers() {
  return {
    onHitlResume: vi.fn(),
    onContinueMcpExecution: vi.fn(),
    onContinueTokenLimitExecution: vi.fn(),
  };
}

describe('buildChatBoxContinuationProps', () => {
  it('passes the plain handlers through unchanged', () => {
    const handlers = baseHandlers();
    const continuation = buildChatBoxContinuationProps(handlers, 'proj-1');

    expect(continuation.onHitlResume).toBe(handlers.onHitlResume);
    expect(continuation.onContinueMcpExecution).toBe(handlers.onContinueMcpExecution);
    expect(continuation.onContinueTokenLimitExecution).toBe(handlers.onContinueTokenLimitExecution);
  });

  it('renderAuthModal mounts the real McpAuthModal, open, with the slot props forwarded', () => {
    const continuation = buildChatBoxContinuationProps(baseHandlers(), 'proj-1');
    const onClose = vi.fn();
    const onCancel = vi.fn();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });

    const element = continuation.renderAuthModal!({
      open: true,
      mcpAuthMetadata: { authServers: ['https://as.example.com'] },
      serverUrl: 'https://mcp.example.com',
      tokenStorageKey: 'key-1',
      toolkitId: 'tk-1',
      toolkitType: 'mcp',
      onClose,
      onCancel,
    });

    renderWithTheme(<QueryClientProvider client={client}>{element}</QueryClientProvider>);

    expect(screen.getByTestId('mcp-auth-modal')).toBeInTheDocument();
    expect(screen.getByText(/mcp\.example\.com/)).toBeInTheDocument();
  });
});
