/**
 * The "Request connector" offer (ADR-0044 decision 1, #1501): a refused
 * connector names its target, the dialog sends the declared tools with
 * their categories, the egress hosts, the data classes and the
 * justification, and the notice then says the request is waiting.
 */
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { EliteaApiError, configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { readConnectorRefusal, splitList } from '../api/connectorRequests';
import { ConnectorNotApprovedNotice } from './ConnectorNotApprovedNotice';

const BASE = '/api/v2';
let created: Record<string, unknown>[];

beforeEach(() => {
  created = [];
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.post(`${BASE}/elitea_core/connector_requests/prompt_lib/:projectId`, async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>;
      created.push(body);
      return HttpResponse.json({ id: 42, state: 'submitted', ...body }, { status: 201 });
    }),
  );
});

afterEach(() => resetGeneratedClient());

function renderNotice(props: Partial<Parameters<typeof ConnectorNotApprovedNotice>[0]> = {}) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <ConnectorNotApprovedNotice projectId="7" target={{ targetKind: 'toolkit_type', target: 'jira' }} {...props} />
    </QueryClientProvider>,
  );
}

describe('readConnectorRefusal', () => {
  it('reads the target of a connector_not_approved refusal and nothing else', () => {
    const refusal = new EliteaApiError({
      kind: 'http',
      status: 403,
      url: '/x',
      body: { error: 'connector_not_approved', code: 'connector_not_approved', target_kind: 'custom_mcp', target: 'https://m.example.com/mcp' },
    });
    expect(readConnectorRefusal(refusal)).toEqual({ targetKind: 'custom_mcp', target: 'https://m.example.com/mcp' });
    const other = new EliteaApiError({
      kind: 'http',
      status: 403,
      url: '/x',
      body: { code: 'toolkit_type_disabled' },
    });
    expect(readConnectorRefusal(other)).toBeUndefined();
    expect(readConnectorRefusal(new Error('boom'))).toBeUndefined();
  });

  it('splits a typed list', () => {
    expect(splitList('a.example.com, b.example.com\na.example.com,, ')).toEqual(['a.example.com', 'b.example.com']);
  });
});

describe('ConnectorNotApprovedNotice', () => {
  it('sends a request for the refused toolkit type and says it is waiting', async () => {
    const user = userEvent.setup();
    renderNotice();
    expect(screen.getByTestId('connector-not-approved')).toHaveTextContent('jira');
    await user.click(screen.getByTestId('connector-request-open'));

    await user.type(screen.getByTestId('connector-request-new-tool'), 'search_issues');
    await user.click(screen.getByRole('button', { name: 'Add tool' }));
    await user.type(screen.getByTestId('connector-request-hosts'), 'jira.example.com, api.atlassian.com');
    await user.type(screen.getByTestId('connector-request-classes'), 'Internal');
    const submit = screen.getByTestId('connector-request-submit');
    expect(submit).toBeDisabled();
    await user.type(screen.getByTestId('connector-request-justification'), 'Support triage');
    await user.click(submit);

    await screen.findByTestId('connector-request-sent');
    expect(created).toEqual([
      {
        target_kind: 'toolkit_type',
        target: 'jira',
        tools: [{ name: 'search_issues', category: 'write' }],
        egress_hosts: ['jira.example.com', 'api.atlassian.com'],
        data_classes: ['internal'],
        justification: 'Support triage',
        platform_wide: false,
        submit: true,
      },
    ]);
  });

  it('sends a custom MCP server’s discovered tools with their schemas', async () => {
    const user = userEvent.setup();
    renderNotice({
      target: { targetKind: 'custom_mcp', target: 'https://mcp.example.com/mcp' },
      tools: [{ name: 'search', category: 'write', input_schema: { type: 'object' } }],
    });
    await user.click(screen.getByTestId('connector-request-open'));
    expect(screen.queryByTestId('connector-request-new-tool')).toBeNull();
    await user.type(screen.getByTestId('connector-request-justification'), 'Docs search');
    await user.click(screen.getByTestId('connector-request-submit'));
    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0]?.['tools']).toEqual([{ name: 'search', category: 'write', input_schema: { type: 'object' } }]);
  });
});
