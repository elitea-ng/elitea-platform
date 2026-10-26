/**
 * The owner chooses the GitHub configuration a cloud thread opens pull
 * requests through (contract 1.29): only a reference is sent, and a refusal
 * says why. Real hooks over MSW.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '../../test/setup';

import CloudPullRequestCredential, { credentialChoices } from './CloudPullRequestCredential';

const BASE = '/api/v2';
const UUID = '0b1d6f4e-3c52-4d3a-9a54-5f1c9a0e7c11';
const PUT = `${BASE}/elitea_core/workspace_handoff_pull_request_credential/prompt_lib/42/${UUID}`;

function serve(status = 200): { bodies: unknown[] } {
  const served = { bodies: [] as unknown[] };
  server.use(
    http.get(`${BASE}/configurations/configurations/42`, ({ request }) => {
      const url = new URL(request.url);
      expect(url.searchParams.get('type')).toBe('github');
      return HttpResponse.json({
        items: [{ elitea_title: 'team-github' }],
        total: 1,
        offset: 0,
        limit: 200,
        shared: { items: [{ elitea_title: 'org-github' }], total: 1 },
      });
    }),
    http.get(`${BASE}/social/author`, () => HttpResponse.json({ id: 1, name: 'Alice', personal_project_id: '7' })),
    http.get(`${BASE}/configurations/configurations/7`, () => HttpResponse.json({ items: [{ elitea_title: 'my-pat' }], total: 1, offset: 0, limit: 200 })),
    http.put(PUT, async ({ request }) => {
      const body = (await request.json()) as { credential: unknown };
      served.bodies.push(body);
      if (status !== 200) return HttpResponse.json({ error: 'pull_requests_disabled' }, { status });
      return HttpResponse.json({ pull_request_credential: body.credential });
    }),
  );
  return served;
}

function mount(current: { elitea_title: string; private: boolean } | null, onError = vi.fn()): typeof onError {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithTheme(
    <QueryClientProvider client={client}>
      <CloudPullRequestCredential projectId="42" conversationUuid={UUID} current={current} onError={onError} />
    </QueryClientProvider>,
  );
  return onError;
}

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('credentialChoices', () => {
  it('lists the project, shared and personal GitHub configurations once each', () => {
    const choices = credentialChoices({ items: [{ elitea_title: 'a' }, { elitea_title: 'a' }], shared: { items: [{ elitea_title: 'b' }, { elitea_title: null }] } }, { items: [{ elitea_title: 'a' }] });
    expect(choices.map(({ elitea_title, private: isPrivate }) => [elitea_title, isPrivate])).toEqual([
      ['a', false],
      ['b', false],
      ['a', true],
    ]);
  });
});

describe('CloudPullRequestCredential', () => {
  it('sends the chosen reference, never a secret', async () => {
    const served = serve();
    mount(null);
    expect(screen.getByTestId('cloud-pr-credential-button')).toHaveTextContent('Pull requests: choose a GitHub credential');
    await userEvent.click(screen.getByTestId('cloud-pr-credential-button'));
    await userEvent.click(await screen.findByLabelText('my-pat (yours)'));
    await userEvent.click(screen.getByTestId('cloud-pr-credential-save'));
    await waitFor(() => expect(served.bodies).toEqual([{ credential: { elitea_title: 'my-pat', private: true } }]));
  });

  it('clears the choice with None', async () => {
    const served = serve();
    mount({ elitea_title: 'team-github', private: false });
    expect(screen.getByTestId('cloud-pr-credential-button')).toHaveTextContent('Pull requests: team-github');
    await userEvent.click(screen.getByTestId('cloud-pr-credential-button'));
    await userEvent.click(await screen.findByLabelText('None'));
    await userEvent.click(screen.getByTestId('cloud-pr-credential-save'));
    await waitFor(() => expect(served.bodies).toEqual([{ credential: null }]));
  });

  it('says when the project turned pull requests off', async () => {
    serve(403);
    const onError = mount(null);
    await userEvent.click(screen.getByTestId('cloud-pr-credential-button'));
    await userEvent.click(await screen.findByLabelText('org-github'));
    await userEvent.click(screen.getByTestId('cloud-pr-credential-save'));
    await waitFor(() => expect(onError).toHaveBeenCalledWith('Pull requests from the cloud are turned off for this project.'));
  });
});
