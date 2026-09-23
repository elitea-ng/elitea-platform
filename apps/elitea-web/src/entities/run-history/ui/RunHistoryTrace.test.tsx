/**
 * Behaviour tests for `RunHistoryTrace` (#938): the generic trace pane must
 * show a step's toolkit-qualified label, its request parameters
 * (`tool_inputs`), its `thinking`, its timing, and a chunk/partial-aware
 * `tool_output` — none of which the pane rendered before this fix (only bare
 * `tool_name`, `text` and a raw `tool_output` dump).
 */
import type { ReactElement } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it } from 'vitest';

import { server } from '../../../test/setup';

import {
  getGetMessageTraceMockHandler,
  getListMessageTracesMockHandler,
} from '@/shared/api/generated/chat/chat.msw';
import type { MessageTraceListing, MessageTraceStepDetail } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import { RunHistoryTrace } from './RunHistoryTrace';

function renderTrace(ui: ReactElement) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={queryClient}>{ui}</QueryClientProvider>);
}

const BASE_STEP = {
  id: 7,
  message_group_id: 1,
  kind: 'tool_call' as const,
  tool_name: 'get_issue',
  parent_agent_name: null,
  parent_agent_call_id: null,
  started_at: '2026-01-01T00:00:10.000000Z',
  finished_at: '2026-01-01T00:00:12.000000Z',
  is_error: false,
  step_type: null,
  model_name: null,
  finish_reason: null,
};

afterEach(() => {
  resetGeneratedClient();
});

describe('RunHistoryTrace', () => {
  it('labels a tool-call step "[Toolkit]: [tool]" when the row carries a toolkit name', async () => {
    const user = userEvent.setup();
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const listing: MessageTraceListing = {
      total: 1,
      rows: [{ ...BASE_STEP, attrs: { metadata: { toolkit_name: 'GitHub' } } }],
    };
    const detail: MessageTraceStepDetail = {
      ...BASE_STEP,
      attrs: { metadata: { toolkit_name: 'GitHub' } },
      tool_inputs: { issue_id: 42 },
      tool_output: '{"title": "A bug"}',
      text: null,
      thinking: 'Deciding which issue to fetch',
    };
    server.use(getListMessageTracesMockHandler(listing), getGetMessageTraceMockHandler(detail));

    renderTrace(<RunHistoryTrace projectId="1" conversationId="9" />);

    const step = await screen.findByTestId('run-history-trace-step');
    expect(step).toHaveTextContent('GitHub: get_issue');

    await user.click(step);

    const requestDetail = await screen.findByTestId('run-history-trace-detail');
    expect(requestDetail).toBeInTheDocument();
    await waitFor(() => expect(screen.getByTestId('run-history-trace-tool-inputs')).toHaveTextContent('42'));
    expect(screen.getByTestId('run-history-trace-thinking')).toHaveTextContent('Deciding which issue to fetch');
    expect(screen.getByTestId('run-history-trace-timing')).toBeInTheDocument();
    // Pretty-printed, not a raw single-line dump.
    expect(screen.getByTestId('run-history-trace-tool-output')).toHaveTextContent('"title": "A bug"');
    expect(screen.queryByTestId('run-history-trace-output-partial')).not.toBeInTheDocument();
  });

  it('falls back to the bare tool name when the row has no toolkit identity', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const listing: MessageTraceListing = { total: 1, rows: [{ ...BASE_STEP, attrs: null }] };
    server.use(getListMessageTracesMockHandler(listing));

    renderTrace(<RunHistoryTrace projectId="1" conversationId="9" />);

    const step = await screen.findByTestId('run-history-trace-step');
    // Exact match (not `toContainText`) so a toolkit-prefixed label ("X: get_issue")
    // would fail this the same way a missing one would: the row's only other text
    // is the timestamp `secondary`, so the full content is known precisely.
    expect(step).toHaveTextContent(`get_issue${BASE_STEP.started_at}`);
  });

  it('marks a chunked tool_output as partial rather than rendering it as the whole result', async () => {
    const user = userEvent.setup();
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const listing: MessageTraceListing = { total: 1, rows: [{ ...BASE_STEP, attrs: null }] };
    const detail: MessageTraceStepDetail = {
      ...BASE_STEP,
      attrs: { tool_output_chunks: { received: 2, total: 5, complete: false } },
      tool_inputs: null,
      tool_output: 'partial text so far',
      text: null,
      thinking: null,
    };
    server.use(getListMessageTracesMockHandler(listing), getGetMessageTraceMockHandler(detail));

    renderTrace(<RunHistoryTrace projectId="1" conversationId="9" />);

    await user.click(await screen.findByTestId('run-history-trace-step'));

    const partial = await screen.findByTestId('run-history-trace-output-partial');
    expect(partial).toHaveTextContent('2/5');
    expect(screen.getByTestId('run-history-trace-tool-output')).toHaveTextContent('partial text so far');
  });
});
