/**
 * The latest target's details win. A chat with nothing picked resolves its
 * target twice (one agent, then the agent its history last addressed), so two
 * reads are in flight at once; the earlier one answering LAST used to stick,
 * and the attachments gate then read the wrong agent's version (the
 * ChatBox.stickyTarget flake on a loaded coverage shard, PR 1890).
 */
import { useState, type ReactElement } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { act, render, screen, waitFor } from '@testing-library/react';
import { delay, http, HttpResponse } from 'msw';
import { afterEach, beforeEach, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { useActiveParticipantDetails } from './useActiveParticipantDetails';

const AGENT_A = { entity_name: 'application', entity_meta: { id: 143, project_id: '2' }, entity_settings: { version_id: '164' } };
const AGENT_B = { entity_name: 'application', entity_meta: { id: 144, project_id: '2' }, entity_settings: { version_id: '165' } };

function application(id: number, versionId: string) {
  return { id, name: `Agent ${String(id)}`, versions: [{ id: versionId, name: 'base' }], version_details: { id: versionId, meta: {} } };
}

let switchToB: () => void = () => {};

function Probe(): ReactElement {
  const [participant, setParticipant] = useState<Record<string, unknown>>(AGENT_A);
  switchToB = () => setParticipant(AGENT_B);
  const { activeParticipantDetails, isLoadingDetails, isDetailsFailed } = useActiveParticipantDetails({ activeParticipant: participant });
  const id = activeParticipantDetails['id'];
  return <output data-testid="details" data-loading={String(isLoadingDetails)} data-failed={String(isDetailsFailed)}>{typeof id === 'number' ? String(id) : ''}</output>;
}

function renderProbe(): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const router = createRouter({
    routeTree: createRootRoute({ component: () => <QueryClientProvider client={queryClient}><Probe /></QueryClientProvider> }),
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => '2' } },
  });
  render(<RouterProvider router={router} />);
}

beforeEach(() => configureGeneratedClient({ baseUrl: '/api/v2' }));
afterEach(() => resetGeneratedClient());

it('keeps the latest target\'s details when the earlier read answers last', async () => {
  let aAnswered = false;
  server.use(
    http.get('*/elitea_core/application/prompt_lib/2/:id', async ({ params }) => {
      // A, asked first, answers well after B.
      if (params['id'] === '143') {
        await delay(150);
        aAnswered = true;
      }
      return HttpResponse.json(params['id'] === '143' ? application(143, '164') : application(144, '165'));
    }),
  );
  renderProbe();
  await screen.findByTestId('details');
  act(() => { switchToB(); });

  await waitFor(() => expect(screen.getByTestId('details')).toHaveTextContent('144'));
  await waitFor(() => expect(aAnswered).toBe(true));
  // Past A's answer and its state update: it must not overwrite B's.
  await new Promise((resolve) => { setTimeout(resolve, 50); });
  expect(screen.getByTestId('details')).toHaveTextContent('144');
  expect(screen.getByTestId('details')).toHaveAttribute('data-loading', 'false');
});

// The participant is bound to version 165, but carries the id as a NUMBER while
// the version list holds strings: a strict comparison fell back to the default
// version (164), so the chat showed ITS starters and welcome message (#2032).
function BoundProbe(): ReactElement {
  const { activeParticipantDetails } = useActiveParticipantDetails({
    activeParticipant: { entity_name: 'application', entity_meta: { id: 143, project_id: '2' }, entity_settings: { version_id: 165 } },
  });
  const details = activeParticipantDetails['version_details'] as { welcome_message?: string } | undefined;
  return <output data-testid="bound">{details?.welcome_message ?? ''}</output>;
}

it('reads the bound version, not the default one, when the participant carries a numeric version id', async () => {
  server.use(
    http.get('*/elitea_core/application/prompt_lib/2/:id', () =>
      HttpResponse.json({
        id: 143,
        name: 'Agent',
        versions: [{ id: '164', name: 'base' }, { id: '165', name: 'v2' }],
        version_details: { id: '164', welcome_message: 'default welcome' },
      }),
    ),
    http.get('*/elitea_core/version/prompt_lib/2/143/165', () =>
      HttpResponse.json({ id: '165', application_id: '143', name: 'v2', status: 'draft', welcome_message: 'bound welcome' }),
    ),
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const router = createRouter({
    routeTree: createRootRoute({ component: () => <QueryClientProvider client={queryClient}><BoundProbe /></QueryClientProvider> }),
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => '2' } },
  });
  render(<RouterProvider router={router} />);
  await waitFor(() => expect(screen.getByTestId('bound')).toHaveTextContent('bound welcome'));
});

it('never hands the previous agent\'s details to the next participant while its read is in flight (issue 2053)', async () => {
  server.use(
    http.get('*/elitea_core/application/prompt_lib/2/:id', async ({ params }) => {
      if (params['id'] === '144') await delay(200);
      return HttpResponse.json(params['id'] === '143' ? application(143, '164') : application(144, '165'));
    }),
  );
  renderProbe();
  await waitFor(() => expect(screen.getByTestId('details')).toHaveTextContent('143'));
  act(() => { switchToB(); });
  // B's read is still pending: A's version list must not be offered as B's.
  expect(screen.getByTestId('details')).toHaveTextContent('');
  await waitFor(() => expect(screen.getByTestId('details')).toHaveTextContent('144'));
});

it('shows nothing for participant B, and says the read failed, when its fetch fails (issue 2053)', async () => {
  server.use(
    http.get('*/elitea_core/application/prompt_lib/2/:id', ({ params }) =>
      params['id'] === '143' ? HttpResponse.json(application(143, '164')) : HttpResponse.json({ error: 'boom' }, { status: 500 }),
    ),
  );
  renderProbe();
  await waitFor(() => expect(screen.getByTestId('details')).toHaveTextContent('143'));
  act(() => { switchToB(); });
  await waitFor(() => expect(screen.getByTestId('details')).toHaveAttribute('data-failed', 'true'));
  // A's details never stand in for B's, and the guard has no version list to act on.
  expect(screen.getByTestId('details')).toHaveTextContent('');
});
