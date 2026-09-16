import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, expect, it } from 'vitest';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';
import { PersistedMessageTrace } from './PersistedMessageTrace';

beforeEach(() => configureGeneratedClient({ baseUrl: '/api/v2' }));
afterEach(() => resetGeneratedClient());
it('fetches heavy output only when its step opens and keeps the message identity', async () => {
  let requests = 0;
  server.use(http.get('/api/v2/elitea_core/message_trace/prompt_lib/2/7279', ({ request }) => {
    requests++;
    expect(new URL(request.url).searchParams.get('message_group_id')).toBe('5820');
    return HttpResponse.json({ id: 7279, message_group_id: 5820, kind: 'tool_call', tool_output: 'persisted issue 11148', is_error: false });
  }));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithTheme(<QueryClientProvider client={client}><PersistedMessageTrace value={{ projectId: '2', conversationId: '543', messageGroupId: 5820, failed: false, steps: [{ id: 7279, message_group_id: 5820, kind: 'tool_call', tool_name: 'get_issues', is_error: false }] }} /></QueryClientProvider>);
  const user = userEvent.setup();
  expect(requests).toBe(0);
  await user.click(screen.getByRole('button', { name: 'Execution details' }));
  expect(requests).toBe(0);
  await user.click(screen.getByRole('button', { name: 'get_issues' }));
  await waitFor(() => expect(screen.getByText('persisted issue 11148')).toBeVisible());
  expect(requests).toBe(1);
});
