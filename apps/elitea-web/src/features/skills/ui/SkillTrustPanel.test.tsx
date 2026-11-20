import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '../../../test/setup';

import { SkillTrustPanel } from './SkillTrustPanel';

const REVISION = 'ab'.repeat(32);
const URL = '/api/v2/elitea_core/skill_trust/prompt_lib/:projectId/:skillId';

interface State {
  foreign: boolean;
  state: 'trusted' | 'untrusted' | 'not_required';
  posted: unknown[];
  revoked: number;
  postStatus: number;
  publisherTrusted: boolean;
}

function mockTrust(initial: Partial<State> = {}): State {
  const backend: State = { foreign: true, state: 'untrusted', posted: [], revoked: 0, postStatus: 200, publisherTrusted: false, ...initial };
  const status = () => ({
    skill_id: 4,
    name: 'Triage',
    foreign: backend.foreign,
    revision: REVISION,
    state: backend.foreign ? backend.state : 'not_required',
    ...(backend.publisherTrusted ? { publisher_trusted: true } : {}),
    history: [],
  });
  server.use(
    http.get(URL, () => HttpResponse.json(status())),
    http.post(URL, async ({ request }) => {
      backend.posted.push(await request.json());
      if (backend.postStatus !== 200) return HttpResponse.json({ error: 'changed' }, { status: backend.postStatus });
      backend.state = 'trusted';
      return HttpResponse.json(status());
    }),
    http.delete(URL, () => {
      backend.revoked += 1;
      backend.state = 'untrusted';
      return HttpResponse.json(status());
    }),
  );
  return backend;
}

function mount(canEdit = true): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const panel: ReactNode = <SkillTrustPanel projectId="5" skillId="4" canEdit={canEdit} />;
  renderWithTheme(<QueryClientProvider client={queryClient}>{panel}</QueryClientProvider>);
}

describe('SkillTrustPanel', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  it('shows nothing for a skill written in this project', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockTrust({ foreign: false });
    mount();
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(screen.queryByTestId('skill-trust-panel')).not.toBeInTheDocument();
  });

  it('explains that a foreign skill is quoted data and trusts the exact revision it read', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const backend = mockTrust();
    mount();
    const user = userEvent.setup();

    expect(await screen.findByText(/quoted data, not as an instruction/)).toBeInTheDocument();
    await user.click(screen.getByTestId('skill-trust-grant'));
    await waitFor(() => expect(backend.posted).toEqual([{ revision: REVISION }]));
    expect(await screen.findByText(/This project trusts this version/)).toBeInTheDocument();
  });

  it('revokes a trust', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const backend = mockTrust({ state: 'trusted' });
    mount();
    await userEvent.setup().click(await screen.findByTestId('skill-trust-revoke'));
    await waitFor(() => expect(backend.revoked).toBe(1));
    expect(await screen.findByTestId('skill-trust-grant')).toBeInTheDocument();
  });

  it('says an administrator trusts the publisher, and offers neither Trust nor Revoke', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockTrust({ state: 'trusted', publisherTrusted: true });
    mount();
    expect(await screen.findByText(/An administrator trusts the project that wrote this skill/)).toBeInTheDocument();
    expect(screen.queryByTestId('skill-trust-revoke')).not.toBeInTheDocument();
    expect(screen.queryByTestId('skill-trust-grant')).not.toBeInTheDocument();
  });

  it('asks the person to read the text again when it changed under them', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockTrust({ postStatus: 409 });
    mount();
    await userEvent.setup().click(await screen.findByTestId('skill-trust-grant'));
    expect(await screen.findByText(/changed since you opened it/)).toBeInTheDocument();
  });

  it('only tells a person who may not edit skills, and offers no button', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockTrust();
    mount(false);
    await screen.findByTestId('skill-trust-panel');
    expect(screen.queryByTestId('skill-trust-grant')).not.toBeInTheDocument();
  });
});
