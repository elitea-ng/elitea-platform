/**
 * What the chat page puts in the composer's place for each state of a thread
 * (ADR-0034): Local work (read-only, as before), in the cloud (composer
 * works, notice above it), returning (notice replaces the composer), and the
 * regenerate rule for desktop-executed answers.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '../../test/setup';

import { handoffExtensions, type HandoffExtensionsInput } from './handoffExtensions';

const BASE = '/api/v2';
const UUID = '0b1d6f4e-3c52-4d3a-9a54-5f1c9a0e7c11';

const INDICATOR = <span data-testid="indicator">context</span>;

function input(overrides: Partial<HandoffExtensionsInput> = {}): HandoffExtensionsInput {
  return {
    localWork: undefined,
    handoff: { executor: 'local', uuid: UUID, authorId: '3', lastAnswerOnDesktop: false },
    projectId: '42',
    conversationId: '77',
    userId: '3',
    contextIndicator: INDICATOR,
    ...overrides,
  };
}

function show(node: React.ReactNode): void {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithTheme(<QueryClientProvider client={client}>{node}</QueryClientProvider>);
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/auth/permissions/prompt_lib/42`, () => HttpResponse.json([])),
    http.get(`${BASE}/elitea_core/workspace_handoff/prompt_lib/42/${UUID}`, () => HttpResponse.json({ state: 'CLOUD_IDLE', epoch: 2 })),
  );
});
afterEach(() => resetGeneratedClient());

describe('handoffExtensions', () => {
  it('leaves an ordinary thread alone', () => {
    expect(handoffExtensions(input())).toEqual({ contextIndicator: INDICATOR });
  });

  it('keeps a Local work thread running on the desktop read-only, with its notice in place of the composer', () => {
    const extensions = handoffExtensions(input({ localWork: { folderName: 'api' } }));
    expect(extensions.readOnlyNotice).toBeDefined();
    expect(extensions.contextIndicator).toBeUndefined();
    show(extensions.readOnlyNotice);
    expect(screen.getByTestId('local-work-notice')).toHaveTextContent('in api');
  });

  it('keeps the composer for a thread in the cloud and shows the Cloud executor notice above it', async () => {
    const extensions = handoffExtensions(input({ localWork: { folderName: 'api' }, handoff: { executor: 'cloud', uuid: UUID, authorId: '3', lastAnswerOnDesktop: false } }));
    // No read-only notice: the composer stays.
    expect(extensions.readOnlyNotice).toBeUndefined();
    show(extensions.contextIndicator);
    expect(await screen.findByTestId('cloud-executor-badge')).toHaveTextContent('Cloud executor');
    // The context indicator is still there, under it.
    expect(screen.getByTestId('indicator')).toBeInTheDocument();
    expect(screen.queryByTestId('local-work-notice')).toBeNull();
  });

  it('replaces the composer with the notice while the thread returns to the desktop', async () => {
    const extensions = handoffExtensions(input({ handoff: { executor: 'returning', uuid: UUID, authorId: '3', lastAnswerOnDesktop: false } }));
    expect(extensions.readOnlyNotice).toBeDefined();
    show(extensions.readOnlyNotice);
    expect(await screen.findByTestId('cloud-executor-badge')).toHaveTextContent('Returning to desktop');
  });

  it('only offers Take back to the conversation\'s owner', async () => {
    const stranger = handoffExtensions(input({ userId: '99', handoff: { executor: 'cloud', uuid: UUID, authorId: '3', lastAnswerOnDesktop: false } }));
    show(stranger.contextIndicator);
    await screen.findByTestId('cloud-executor-badge');
    expect(screen.queryByRole('button', { name: 'Take back' })).toBeNull();
  });

  it('falls back to the Local work notice while the conversation\'s UUID is not known', () => {
    const extensions = handoffExtensions(input({ localWork: { folderName: undefined }, handoff: { executor: 'cloud', uuid: undefined, authorId: '3', lastAnswerOnDesktop: false } }));
    expect(extensions.readOnlyNotice).toBeDefined();
  });

  it('drops regenerate when the newest answer ran on a desktop, whatever the state', () => {
    expect(handoffExtensions(input({ handoff: { executor: 'local', uuid: UUID, authorId: '3', lastAnswerOnDesktop: true } }))).toMatchObject({ noRegenerate: true });
    expect(handoffExtensions(input({ handoff: { executor: 'cloud', uuid: UUID, authorId: '3', lastAnswerOnDesktop: true } }))).toMatchObject({ noRegenerate: true });
    expect(handoffExtensions(input())).not.toHaveProperty('noRegenerate');
  });

  it('adds nothing for a thread that does not exist yet', () => {
    expect(handoffExtensions(input({ conversationId: undefined }))).toEqual({ contextIndicator: INDICATOR });
    render(<div />);
  });
});
