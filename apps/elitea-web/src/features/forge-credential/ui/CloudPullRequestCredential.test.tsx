/**
 * The owner chooses the GitHub or GitLab configuration a cloud thread pushes
 * and opens pull requests through (contract 1.29): only a typed reference is
 * sent, and a refusal says why. Real hooks over MSW.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';

import { CloudPullRequestCredential, credentialChoices, savedCredential } from './CloudPullRequestCredential';

const BASE = '/api/v2';
const UUID = '0b1d6f4e-3c52-4d3a-9a54-5f1c9a0e7c11';
const PUT = `${BASE}/elitea_core/workspace_handoff_pull_request_credential/prompt_lib/42/${UUID}`;

const page = (titles: string[], shared: string[] = []) => ({
  items: titles.map((elitea_title) => ({ elitea_title })),
  total: titles.length,
  offset: 0,
  limit: 200,
  shared: { items: shared.map((elitea_title) => ({ elitea_title })), total: shared.length },
});

function serve(status = 200, listStatus = 200): { bodies: unknown[] } {
  const served = { bodies: [] as unknown[] };
  server.use(
    http.get(`${BASE}/configurations/configurations/42`, ({ request }) => {
      const type = new URL(request.url).searchParams.get('type');
      if (listStatus !== 200 && type === 'gitlab') return HttpResponse.json({ error: 'unavailable' }, { status: listStatus });
      return HttpResponse.json(type === 'gitlab' ? page(['team-gitlab']) : page(['team-github'], ['org-github']));
    }),
    http.get(`${BASE}/social/author`, () => HttpResponse.json({ id: 1, name: 'Alice', personal_project_id: '7' })),
    http.get(`${BASE}/configurations/configurations/7`, ({ request }) => {
      const type = new URL(request.url).searchParams.get('type');
      return HttpResponse.json(type === 'github' ? page(['my-pat']) : page([]));
    }),
    http.put(PUT, async ({ request }) => {
      const body = (await request.json()) as { credential: unknown };
      served.bodies.push(body);
      if (status !== 200) return HttpResponse.json({ error: 'pull_requests_disabled' }, { status });
      return HttpResponse.json({ pull_request_credential: body.credential });
    }),
  );
  return served;
}

function wrap(node: React.ReactNode): void {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithTheme(<QueryClientProvider client={client}>{node}</QueryClientProvider>);
}

function mount(current: { type: 'github' | 'gitlab'; elitea_title: string; private: boolean } | null, onError = vi.fn(), onSaved = vi.fn()): typeof onError {
  wrap(<CloudPullRequestCredential projectId="42" conversationUuid={UUID} current={current} onSaved={onSaved} onError={onError} />);
  return onError;
}

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('credentialChoices', () => {
  it('lists each forge’s project, shared and personal configurations once each', () => {
    const choices = credentialChoices([
      { type: 'github', project: { items: [{ elitea_title: 'a' }, { elitea_title: 'a' }], shared: { items: [{ elitea_title: 'b' }, { elitea_title: null }] } }, personal: { items: [{ elitea_title: 'a' }] } },
      { type: 'gitlab', project: { items: [{ elitea_title: 'a' }] }, personal: undefined },
    ]);
    expect(choices.map(({ type, elitea_title, private: isPrivate }) => [type, elitea_title, isPrivate])).toEqual([
      ['github', 'a', false],
      ['github', 'b', false],
      ['github', 'a', true],
      ['gitlab', 'a', false],
    ]);
  });
});

describe('savedCredential', () => {
  const current = { type: 'github' as const, elitea_title: 'gone', private: false };
  const listed = credentialChoices([{ type: 'gitlab', project: { items: [{ elitea_title: 'lab' }] }, personal: undefined }]);
  it('sends null only for an explicit None, and nothing when the current choice is kept', () => {
    expect(savedCredential('', current, listed)).toBeNull();
    expect(savedCredential('', null, listed)).toBeUndefined();
    expect(savedCredential('github:project:gone', current, listed)).toBeUndefined();
    expect(savedCredential('gitlab:project:lab', current, listed)).toEqual({ type: 'gitlab', elitea_title: 'lab', private: false });
    // A key that is neither listed nor current changes nothing.
    expect(savedCredential('github:project:other', current, listed)).toBeUndefined();
  });
});

describe('CloudPullRequestCredential', () => {
  it('keeps a current choice the listing does not show, and Save leaves it as it is', async () => {
    const served = serve();
    mount({ type: 'github', elitea_title: 'gone', private: true });
    await userEvent.click(screen.getByTestId('cloud-pr-credential-button'));
    expect(await screen.findByLabelText('gone (GitHub, yours) (current, not listed)')).toBeChecked();
    await userEvent.click(screen.getByTestId('cloud-pr-credential-save'));
    await waitFor(() => expect(screen.queryByTestId('cloud-pr-credential-save')).not.toBeInTheDocument());
    expect(served.bodies).toEqual([]);
  });

  it('treats a failed listing as an error: nothing can be saved, so nothing is cleared', async () => {
    const served = serve(200, 503);
    mount({ type: 'github', elitea_title: 'team-github', private: false });
    await userEvent.click(screen.getByTestId('cloud-pr-credential-button'));
    expect(await screen.findByTestId('cloud-pr-credential-list-failed')).toBeInTheDocument();
    expect(screen.getByTestId('cloud-pr-credential-save')).toBeDisabled();
    expect(screen.queryByLabelText('None')).not.toBeInTheDocument();
    expect(served.bodies).toEqual([]);
  });

  it('sends the chosen typed reference, never a secret', async () => {
    const served = serve();
    const onSaved = vi.fn();
    mount(null, vi.fn(), onSaved);
    expect(screen.getByTestId('cloud-pr-credential-button')).toHaveTextContent('Pushes and pull requests: choose a credential');
    await userEvent.click(screen.getByTestId('cloud-pr-credential-button'));
    await userEvent.click(await screen.findByLabelText('team-gitlab (GitLab)'));
    await userEvent.click(screen.getByTestId('cloud-pr-credential-save'));
    await waitFor(() => expect(served.bodies).toEqual([{ credential: { type: 'gitlab', elitea_title: 'team-gitlab', private: false } }]));
    await waitFor(() => expect(onSaved).toHaveBeenCalledOnce());
  });

  it('offers the owner’s own credentials and clears the choice with None', async () => {
    const served = serve();
    mount({ type: 'github', elitea_title: 'team-github', private: false });
    expect(screen.getByTestId('cloud-pr-credential-button')).toHaveTextContent('Pushes and pull requests: team-github (GitHub)');
    await userEvent.click(screen.getByTestId('cloud-pr-credential-button'));
    expect(await screen.findByLabelText('my-pat (GitHub, yours)')).toBeInTheDocument();
    await userEvent.click(screen.getByLabelText('None'));
    await userEvent.click(screen.getByTestId('cloud-pr-credential-save'));
    await waitFor(() => expect(served.bodies).toEqual([{ credential: null }]));
  });

  it('says when the project turned pushes and pull requests off', async () => {
    serve(403);
    const onError = mount(null);
    await userEvent.click(screen.getByTestId('cloud-pr-credential-button'));
    await userEvent.click(await screen.findByLabelText('org-github (GitHub)'));
    await userEvent.click(screen.getByTestId('cloud-pr-credential-save'));
    await waitFor(() => expect(onError).toHaveBeenCalledWith('Pushes and pull requests from the cloud are turned off for this project.'));
  });
});
