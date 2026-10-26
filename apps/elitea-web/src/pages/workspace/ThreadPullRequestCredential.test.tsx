/**
 * The desktop's cloud-thread banner reads the thread's current push
 * credential itself and says a refusal inline (ADR-0034 decision 10).
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';

import { ThreadPullRequestCredential } from './ThreadPullRequestCredential';

const BASE = '/api/v2';
const UUID = '0b1d6f4e-3c52-4d3a-9a54-5f1c9a0e7c11';

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('ThreadPullRequestCredential', () => {
  it('shows the current choice and says a refusal inline', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/workspace_handoff/prompt_lib/42/${UUID}`, () =>
        HttpResponse.json({ state: 'CLOUD_IDLE', epoch: 3, pull_request_credential: { type: 'gitlab', elitea_title: 'team-gitlab', private: false } }),
      ),
      http.get(`${BASE}/configurations/configurations/42`, () => HttpResponse.json({ items: [], total: 0, offset: 0, limit: 200 })),
      http.get(`${BASE}/social/author`, () => HttpResponse.json({ id: 1, name: 'Alice', personal_project_id: '42' })),
      http.put(`${BASE}/elitea_core/workspace_handoff_pull_request_credential/prompt_lib/42/${UUID}`, () => HttpResponse.json({ error: 'pull_requests_disabled' }, { status: 403 })),
    );
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    renderWithTheme(
      <QueryClientProvider client={client}>
        <ThreadPullRequestCredential projectId="42" conversationUuid={UUID} />
      </QueryClientProvider>,
    );
    expect(await screen.findByTestId('cloud-pr-credential-button')).toHaveTextContent('Pushes and pull requests: team-gitlab (GitLab)');
    await userEvent.click(screen.getByTestId('cloud-pr-credential-button'));
    await userEvent.click(await screen.findByLabelText('None'));
    await userEvent.click(screen.getByTestId('cloud-pr-credential-save'));
    expect(await screen.findByTestId('cloud-pr-credential-error')).toHaveTextContent('Pushes and pull requests from the cloud are turned off for this project.');
  });
});
