import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { ContextBudgetIndicator } from './ContextBudgetIndicator';

beforeEach(() => configureGeneratedClient({ baseUrl: '/api/v2' }));
afterEach(() => resetGeneratedClient());

describe('ContextBudgetIndicator', () => {
  it.each([true, false])('opens a keyboard-accessible usage breakdown; measurement available=%s', async (available) => {
    server.use(http.get('/api/v2/elitea_core/context_analytics/prompt_lib/2/chat', () => HttpResponse.json({ current_tokens: 18000, max_tokens: 20000, context_analytics_available: available, budget_mode: 'balanced' })));
    renderWithTheme(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <ContextBudgetIndicator projectId="2" conversationId="chat" />
    </QueryClientProvider>);
    const user = userEvent.setup();
    const button = await screen.findByTestId('context-budget-indicator');
    expect(button).toHaveAccessibleName(available ? '90% context used' : 'Context usage not yet measured');
    button.focus();
    await user.keyboard('{Enter}');
    expect(await screen.findByRole('dialog', { name: 'Context usage' })).toBeInTheDocument();
    expect(screen.getByTestId('context-budget-utilization')).toHaveTextContent(available ? '90%' : '—');
    await user.keyboard('{Escape}');
    expect(button).toHaveAttribute('aria-expanded', 'false');
  });
});
