/**
 * ADR-0041 decision 3: a response that stopped at a run cap offers the
 * override only to a holder of safety.override_cap, and only on CAP_REACHED.
 */
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { CapOverrideAction } from './CapOverrideAction';

const BASE = '/api/v2';
const MESSAGE = '0190f2a6-7d1c-7cc1-9a35-3f4b4d2a1b11';

let grants: unknown[];

function serve(granted: readonly string[], refusal?: { status: number; code: string }): void {
  server.use(
    http.get(`${BASE}/auth/permissions/prompt_lib/:projectId`, () =>
      HttpResponse.json(granted.map((name) => ({ name, enabled: true }))),
    ),
    http.post(`${BASE}/elitea_core/run_cap_overrides/prompt_lib/:projectId`, async ({ request }) => {
      grants.push(await request.json());
      if (refusal) return HttpResponse.json({ error: 'refused', code: refusal.code }, { status: refusal.status });
      return HttpResponse.json(
        { id: 1, cap: 'max_tool_calls', granted_at: '2026-10-30T10:00:00Z', expires_at: '2026-10-30T11:00:00Z' },
        { status: 201 },
      );
    }),
  );
}

function renderAction(code: string | undefined) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <CapOverrideAction projectId="7" messageId={MESSAGE} code={code} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  grants = [];
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('CapOverrideAction', () => {
  it('renders nothing for another failure', () => {
    serve(['safety.override_cap']);
    const { container } = renderAction('MODEL_TIMEOUT');
    expect(container).toBeEmptyDOMElement();
  });

  it('is hidden without safety.override_cap', async () => {
    serve(['models.applications.applications.list']);
    const { container } = renderAction('CAP_REACHED');
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(container).toBeEmptyDOMElement();
  });

  it('grants the override on the stopped response', async () => {
    serve(['safety.override_cap']);
    renderAction('CAP_REACHED');
    await userEvent.click(await screen.findByTestId('cap-override-button'));
    expect(await screen.findByTestId('cap-override-granted')).toHaveTextContent('Send a message to continue.');
    expect(grants).toEqual([{ message_id: MESSAGE }]);
  });

  it('shows the refusal words', async () => {
    serve(['safety.override_cap'], { status: 409, code: 'open_grant' });
    renderAction('CAP_REACHED');
    await userEvent.click(await screen.findByTestId('cap-override-button'));
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('already has an unused allowance'));
  });
});
