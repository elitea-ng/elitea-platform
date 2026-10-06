import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';

import { buildToolActions } from '@/entities/message/lib/toolActions';
import { groupTraceStepsByMessageGroup } from '@/entities/message/lib/traceSteps';
import { RunHistoryTraceSteps } from '@/entities/run-history/ui/RunHistoryTraceSteps';
import type { MessageTraceStep, MessageTraceStepDetail } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import fixture from '@/shared/lib/fixtures/code-debug-public-trace.json';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';
import { TraceStepDetailProvider } from '../model/traceStepDetail';
import { PersistedMessageTrace } from './chat-box/PersistedMessageTrace';
import { ToolModal } from './ToolModal';

installCodeMirrorTestPolyfills();
beforeEach(() => configureGeneratedClient({ baseUrl: '/api/v2' }));
afterEach(() => resetGeneratedClient());

const step: MessageTraceStep = {
  id: 101, message_group_id: 51, kind: 'tool_call', tool_name: 'run / debug export',
  is_error: false, attrs: fixture.first_attempt_history,
};
const detail: MessageTraceStepDetail = { ...step, tool_inputs: {}, tool_output: null };

it('retains the verified public proof in the live tool action and uses the caller project', () => {
  const action = buildToolActions([], [fixture.first_attempt_history], '', undefined, undefined)[0]!;
  expect(action.toolMeta?.['code_debug_v1']).toEqual(fixture.first_attempt_history.metadata.code_debug_v1);
  renderWithTheme(<TraceStepDetailProvider projectId="7"><ToolModal open onClose={vi.fn()} toolAction={action} /></TraceStepDetailProvider>);
  expect(screen.getByText('Code debug · run · attempt 1')).toBeVisible();
  expect(screen.getByRole('button', { name: 'Download verified snapshot' })).toBeVisible();
});

it('retains distinct first-attempt history and retry references after trace-row restoration', () => {
  const restored = groupTraceStepsByMessageGroup([
    { id: 101, message_group_id: 51, kind: 'tool_call', attrs: fixture.first_attempt_history },
    { id: 102, message_group_id: 51, kind: 'tool_call', attrs: fixture.retry_committed },
  ]).get('51')!;
  const actions = buildToolActions([], restored.toolCalls, '', undefined, undefined);
  expect(actions).toHaveLength(2);
  expect(actions.map(action => action.toolMeta?.['code_debug_v1'])).toEqual([
    fixture.first_attempt_history.metadata.code_debug_v1, fixture.retry_committed.metadata.code_debug_v1,
  ]);
  server.use(http.get('/api/v2/elitea_core/message_trace/prompt_lib/7/102', () => HttpResponse.json({ ...detail, id: 102 })));
  renderWithTheme(<TraceStepDetailProvider projectId="7"><ToolModal open onClose={vi.fn()} toolAction={actions[1]!} /></TraceStepDetailProvider>);
  expect(screen.getByText('Code debug · run · attempt 2')).toBeVisible();
  expect(screen.queryByText('Code debug · run · attempt 1')).toBeNull();
});

it('keeps a retry warning without a first-attempt artifact fallback', () => {
  const action = buildToolActions([], [fixture.retry_current_warning], '', undefined, undefined)[0]!;
  renderWithTheme(<TraceStepDetailProvider projectId="7"><ToolModal open onClose={vi.fn()} toolAction={action} /></TraceStepDetailProvider>);
  expect(screen.getByText('Code debug · run · attempt 2')).toBeVisible();
  expect(screen.getByRole('status')).toHaveTextContent('artifact is unavailable');
  expect(screen.queryByRole('button', { name: 'Download verified snapshot' })).toBeNull();
});

it('leaves unrelated tool metadata and modal panes unchanged', () => {
  const action = buildToolActions([], [{ tool_name: 'search', tool_run_id: 'ordinary', metadata: { langgraph_node: 'tools' }, tool_inputs: { query: 'example' }, tool_output: 'ordinary result' }], '', undefined, undefined)[0]!;
  expect(action.toolMeta).not.toHaveProperty('code_debug_v1');
  renderWithTheme(<ToolModal open onClose={vi.fn()} toolAction={action} />);
  expect(screen.queryByTestId('code-debug-artifact')).toBeNull();
  expect(screen.getByText('INPUT')).toBeVisible();
  expect(screen.getByText('OUTPUT')).toBeVisible();
});

it('shows only the exact selected run-history row artifact', () => {
  const view = renderWithTheme(<RunHistoryTraceSteps projectId="7" scopeKey="conversation-1" steps={[step]} selectedStep={step} detail={detail} onSelect={vi.fn()} />);
  expect(screen.getByRole('button', { name: 'Download verified snapshot' })).toBeVisible();
  view.rerender(<RunHistoryTraceSteps projectId="7" scopeKey="conversation-1" steps={[step]} selectedStep={step} detail={{ ...detail, id: 102 }} onSelect={vi.fn()} />);
  expect(screen.queryByTestId('code-debug-artifact')).toBeNull();
  view.rerender(<RunHistoryTraceSteps projectId="7" scopeKey="conversation-1" steps={[step]} selectedStep={step} detail={{ ...detail, message_group_id: 52 }} onSelect={vi.fn()} />);
  expect(screen.queryByTestId('code-debug-artifact')).toBeNull();
});

it('renders persisted artifact metadata after the authorized detail read', async () => {
  server.use(http.get('/api/v2/elitea_core/message_trace/prompt_lib/7/101', ({ request }) => {
    expect(new URL(request.url).searchParams.get('message_group_id')).toBe('51');
    return HttpResponse.json(detail);
  }));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithTheme(<QueryClientProvider client={client}><PersistedMessageTrace value={{ projectId: '7', conversationId: '10', messageGroupId: 51, failed: false, steps: [step] }} /></QueryClientProvider>);
  const user = userEvent.setup();
  await user.click(screen.getByRole('button', { name: 'Execution details' }));
  await user.click(screen.getByRole('button', { name: 'run / debug export' }));
  expect(await screen.findByRole('button', { name: 'Download verified snapshot' })).toBeVisible();
});

it('keeps foreign persisted detail metadata inert', async () => {
  server.use(http.get('/api/v2/elitea_core/message_trace/prompt_lib/7/101', () => HttpResponse.json({ ...detail, message_group_id: 52, tool_output: 'foreign row' })));
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithTheme(<QueryClientProvider client={client}><PersistedMessageTrace value={{ projectId: '7', conversationId: '10', messageGroupId: 51, failed: false, steps: [step] }} /></QueryClientProvider>);
  const user = userEvent.setup();
  await user.click(screen.getByRole('button', { name: 'Execution details' }));
  await user.click(screen.getByRole('button', { name: 'run / debug export' }));
  await screen.findByText('foreign row');
  expect(screen.queryByTestId('code-debug-artifact')).toBeNull();
});
