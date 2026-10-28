/**
 * The agent page's Stop agent action (ADR-0041 decision 1): shown only with
 * safety.suspend in the project, sends an agent suspension with a reason,
 * then shows the stopped state with Resume.
 */
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { StopAgentButton } from './StopAgentButton';

const BASE = '/api/v2';

let active: Record<string, unknown>[];
let creates: unknown[];
let resumes: string[];

function serve(granted: readonly string[]): void {
  server.use(
    http.get(`${BASE}/auth/permissions/prompt_lib/:projectId`, () =>
      HttpResponse.json(granted.map((name) => ({ name, enabled: true }))),
    ),
    http.get(`${BASE}/elitea_core/execution_suspensions/prompt_lib/:projectId`, () =>
      HttpResponse.json({ suspensions: active }),
    ),
    http.post(`${BASE}/elitea_core/execution_suspensions/prompt_lib/:projectId`, async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>;
      creates.push(body);
      const row = { id: 3, scope: 'agent', project_id: 7, application_id: 31, reason: body['reason'], active: true };
      active = [row];
      return HttpResponse.json(row, { status: 201 });
    }),
    http.post(`${BASE}/elitea_core/execution_suspensions/prompt_lib/:projectId/:id`, ({ params }) => {
      resumes.push(String(params['id']));
      active = [];
      return HttpResponse.json({ id: 3, active: false });
    }),
  );
}

function renderButton(onError = vi.fn()) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <StopAgentButton projectId="7" applicationId="31" onError={onError} />
    </QueryClientProvider>,
  );
  return onError;
}

beforeEach(() => {
  active = [];
  creates = [];
  resumes = [];
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('StopAgentButton', () => {
  it('is hidden without safety.suspend', async () => {
    serve(['models.applications.applications.update']);
    renderButton();
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(screen.queryByTestId('stop-agent-button')).not.toBeInTheDocument();
  });

  it('stops the agent with a reason and then offers Resume', async () => {
    serve(['safety.suspend']);
    const user = userEvent.setup();
    renderButton();

    await user.click(await screen.findByTestId('stop-agent-button'));
    expect(screen.getByTestId('stop-agent-confirm')).toBeDisabled();
    await user.type(screen.getByTestId('stop-agent-reason'), 'loops on customers');
    await user.click(screen.getByTestId('stop-agent-confirm'));

    expect(await screen.findByTestId('agent-stopped-chip')).toHaveTextContent('Agent stopped');
    expect(creates).toEqual([{ scope: 'agent', application_id: 31, reason: 'loops on customers' }]);

    await user.click(screen.getByRole('button', { name: 'Resume agent' }));
    await waitFor(() => {
      expect(resumes).toEqual(['3:resume']);
    });
    expect(await screen.findByTestId('stop-agent-button')).toBeInTheDocument();
  });

  it('shows a project-wide stop without an agent Resume', async () => {
    serve(['safety.suspend']);
    active = [{ id: 9, scope: 'project', project_id: 7, application_id: null, reason: 'incident', active: true }];
    renderButton();

    expect(await screen.findByTestId('agent-stopped-chip')).toHaveTextContent('Project stopped');
    expect(screen.queryByRole('button', { name: 'Resume agent' })).not.toBeInTheDocument();
  });
});
