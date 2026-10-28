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
  // One step: opening "Execution details" shows it straight away, no second fold.
  await user.click(screen.getByRole('button', { name: 'Execution details' }));
  expect(screen.getByText('get_issues')).toBeVisible();
  await waitFor(() => expect(screen.getByText('persisted issue 11148')).toBeVisible());
  expect(requests).toBe(1);
});

it('preserves the incomplete generation label stored in trace display metadata', async () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithTheme(<QueryClientProvider client={client}><PersistedMessageTrace value={{
    projectId: '2', conversationId: '543', messageGroupId: 5820, failed: false,
    steps: [{ id: 7280, message_group_id: 5820, kind: 'thinking_step', model_name: 'model',
      parent_agent_name: 'Child', is_error: false,
      attrs: { response_metadata: { tool_name: 'Incomplete response' } } }],
  }} /></QueryClientProvider>);
  await userEvent.setup().click(screen.getByRole('button', { name: 'Execution details' }));
  expect(screen.getByText('Child · Incomplete response')).toBeVisible();
});

it('folds each of several steps, fetching only the one opened', async () => {
  let requests = 0;
  server.use(http.get('/api/v2/elitea_core/message_trace/prompt_lib/2/:id', ({ params }) => {
    requests++;
    return HttpResponse.json({ id: Number(params['id']), message_group_id: 5820, kind: 'tool_call', tool_output: `output ${String(params['id'])}`, is_error: false });
  }));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithTheme(<QueryClientProvider client={client}><PersistedMessageTrace value={{ projectId: '2', conversationId: '543', messageGroupId: 5820, failed: false, steps: [
    { id: 1, message_group_id: 5820, kind: 'tool_call', tool_name: 'first', is_error: false },
    { id: 2, message_group_id: 5820, kind: 'tool_call', tool_name: 'second', is_error: false },
  ] }} /></QueryClientProvider>);
  const user = userEvent.setup();
  await user.click(screen.getByRole('button', { name: 'Execution details' }));
  expect(requests).toBe(0);
  await user.click(screen.getByRole('button', { name: 'second' }));
  await waitFor(() => expect(screen.getByText('output 2')).toBeVisible());
  expect(requests).toBe(1);
});
