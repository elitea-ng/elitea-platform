/**
 * A Local work thread's Participants tab (ADR-0061 decision 2a, issue 3055): the chat's participants panel in the
 * inspector, agents only, first with two or more agents and after the others with one; Local/Cloud badges and the
 * Lead chip; the row menu writes the lead (keeping the other meta keys), where an agent runs (its whole
 * `entity_settings`) and Remove (a lead's removal clears the floor first).
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { replayView } from '@/features/workspace';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { SidePanelHarness } from '@/test/sidePanelHarness';
import { server } from '@/test/setup';

import type { AgentModels } from './agentModels';
import { ParticipantsTab } from './ParticipantsTab';
import { executorSettings } from './useThreadTeam';

const BASE = '/api/v2';
const DETAILS = `${BASE}/elitea_core/conversation/prompt_lib/42/77`;
const globals = globalThis as unknown as Record<string, unknown>;

const coder = { id: 11, entity_name: 'application', entity_meta: { id: 5, name: 'Coder' }, entity_settings: { version_id: 9 } };
const reviewer = { id: 12, entity_name: 'application', entity_meta: { id: 6, name: 'Reviewer' }, entity_settings: { version_id: 10, executor: 'cloud', llm_settings: { model: 'm' } } };
const toolkit = { id: 13, entity_name: 'toolkit', entity_meta: { id: 4, name: 'Jira' }, entity_settings: { toolkit_type: 'jira' } };
const META = { local_work: { folder_name: 'repo' }, steps_limit: 25, floor: { policy: 'lead', lead_participant_id: 11 } };

interface Calls { meta: unknown[]; settings: { id: string; body: unknown }[]; deleted: string[]; order: string[] }

function serve(participants: readonly unknown[], meta: Record<string, unknown> = META): Calls {
  const calls: Calls = { meta: [], settings: [], deleted: [], order: [] };
  server.use(
    http.get(`${BASE}/auth/permissions/prompt_lib/42`, () => HttpResponse.json([])),
    http.get(`${BASE}/social/author`, () => HttpResponse.json({ id: 3, name: 'Me' })),
    http.get(`${BASE}/elitea_core/application/prompt_lib/42/:id`, ({ params }) => HttpResponse.json({ id: params['id'], name: 'Agent', versions: [] })),
    http.get(`${BASE}/elitea_core/tool/prompt_lib/42/:id`, ({ params }) => HttpResponse.json({ id: params['id'], name: 'Jira', type: 'jira', settings: {} })),
    http.get(DETAILS, () => HttpResponse.json({ id: 77, uuid: 'c0ffee00-0000-4000-8000-000000000077', name: 'fix the build', source: 'local_work', meta, participants })),
    http.put(DETAILS, async ({ request }) => {
      calls.order.push('meta');
      const body = await request.json();
      calls.meta.push(body);
      return HttpResponse.json({ id: 77, meta: (body as { meta?: unknown }).meta });
    }),
    http.put(`${BASE}/elitea_core/entity_settings/prompt_lib/42/77/:id`, async ({ params, request }) => {
      const body = await request.json();
      calls.settings.push({ id: String(params['id']), body });
      return HttpResponse.json({ entity_settings: body });
    }),
    http.delete(`${BASE}/elitea_core/participant/prompt_lib/42/77/:id`, ({ params }) => {
      calls.order.push('delete');
      calls.deleted.push(String(params['id']));
      return new HttpResponse(null, { status: 204 });
    }),
  );
  return calls;
}

function mount(onAddress = vi.fn(), models?: AgentModels): void {
  const turn = { turnId: null, view: replayView([]) };
  const ipc = {
    stopExchange: vi.fn(),
    skipInExchange: vi.fn(),
    threadInstructionsStatus: vi.fn(() => Promise.resolve({ files: ['AGENTS.md'], respect: false })),
    setThreadRespectAgentsMd: vi.fn(() => Promise.resolve()),
  };
  const Page = () => (
    <SidePanelHarness>
      <ParticipantsTab workspaceId="ws-1" projectId={42} conversationId="77" subject="fix the build" executorLocked={false} onAddress={onAddress} turn={turn} ipc={ipc} models={models} />
    </SidePanelHarness>
  );
  const rootRoute = createRootRoute({ component: Page });
  const router = createRouter({ routeTree: rootRoute.addChildren([createRoute({ getParentRoute: () => rootRoute, path: '/agents/$tab/$agentId', component: () => null })]), history: createMemoryHistory({ initialEntries: ['/'] }) });
  render(
    <AppProviders>
      <RouterProvider router={router} />
    </AppProviders>,
  );
}

async function openMenu(participantId: number): Promise<void> {
  const row = await waitFor(() => {
    const found = screen.getAllByTestId('participant-lead').find((node) => node.getAttribute('data-participant-id') === String(participantId));
    expect(found).toBeDefined();
    return found!;
  });
  fireEvent.click(within(row).getByTestId('participant-lead-menu'));
}

beforeEach(() => {
  resetConfigForTests();
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: '1' };
  configureGeneratedClient({ baseUrl: BASE });
  vi.stubGlobal('matchMedia', (query: string) => ({ matches: false, media: query, addEventListener: () => {}, removeEventListener: () => {}, addListener: () => {}, removeListener: () => {}, onchange: null, dispatchEvent: () => false }));
});

afterEach(() => {
  resetGeneratedClient();
  resetConfigForTests();
  delete globals['elitea_ui_config'];
  vi.unstubAllGlobals();
});

describe("a Local work thread's Participants tab (issue 3055)", () => {
  it('states the thread\'s project instructions once, below the agents, with its Respect project instructions choice (issue 3130)', async () => {
    serve([coder, reviewer]);
    mount();
    const tab = await screen.findByTestId('side-panel-tab-team');
    const section = await within(tab).findByTestId('project-instructions');
    expect(within(section).getByTestId('project-instructions-file')).toHaveTextContent('AGENTS.md');
    await waitFor(() => expect(within(section).getByRole('switch', { name: 'Respect project instructions' })).not.toBeChecked());
    expect(within(section).getByTestId('agents-md-ignored')).toHaveTextContent('Project instructions ignored in this thread');
  });

  it('leads the tabs with two agents, with the Lead chip and Local/Cloud badges, and lists no toolkit', async () => {
    serve([coder, reviewer, toolkit]);
    mount();
    const tab = await screen.findByTestId('side-panel-tab-team');
    expect(tab).toHaveAttribute('aria-label', 'Participants');
    await waitFor(() => expect(within(tab).getAllByTestId('participant-executor')).toHaveLength(2));
    expect(within(tab).getAllByTestId('participant-executor').map((chip) => chip.getAttribute('data-executor'))).toEqual(['local', 'cloud']);
    expect(within(tab).getAllByTestId('participant-lead-badge')).toHaveLength(1);
    expect([...tab.querySelectorAll('[data-testid^="participants-section-"]')].map((node) => node.getAttribute('data-testid'))).toEqual(['participants-section-Agents']);
    expect(screen.queryByTestId('side-panel-tab-participants')).toBeNull();
  });

  it('shows each agent\'s logo or avatar pack in the row\'s avatar place, the default avatar without either, and no second avatar (#3131, #3145)', async () => {
    const media = (sha: string, ext: string) => `/api/v2/agent_media/42/${sha.repeat(32)}.${ext}`;
    const packed = { ...coder, persona: { display_name: 'Coder', avatar_pack_v1: { schema: 'elitea.avatar_pack/v1', slots: { still: { url: media('ab', 'png') }, working: { url: media('cd', 'mp4'), poster_url: media('ab', 'png') } } } } };
    const logo = { ...reviewer, entity_settings: { ...reviewer.entity_settings, icon_meta: { url: '/icons/42/reviewer.png' } } };
    const plain = { id: 14, entity_name: 'application', entity_meta: { id: 7, name: 'Dev Coordinator' }, entity_settings: { version_id: 11 } };
    serve([packed, logo, plain]);
    mount();
    const tab = await screen.findByTestId('side-panel-tab-team');
    await waitFor(() => expect(within(tab).getAllByTestId('participant-avatar')).toHaveLength(3));
    const avatarOf = (id: number) => within(tab).getAllByTestId('participant-avatar').find((node) => node.getAttribute('data-participant-id') === String(id));
    // On the deployment's origin (#3132): the page is served from another one here.
    expect(avatarOf(11)?.querySelector('img')?.getAttribute('src')).toBe(`https://elitea.example${media('ab', 'png')}`);
    expect(avatarOf(12)?.querySelector('img')?.getAttribute('src')).toBe('https://elitea.example/icons/42/reviewer.png');
    // The agent with neither has the shared avatar too (its brand mark), so its live state animates (#3145).
    expect(avatarOf(14)?.querySelector('[data-testid="chat-message-avatar"]')).not.toBeNull();
    expect(avatarOf(14)?.querySelector('img')).toBeNull();
    expect(within(tab).queryByText('D')).toBeNull();
    // The right side keeps the chips and the menu, with no second avatar.
    for (const controls of within(tab).getAllByTestId('participant-lead')) {
      expect(within(controls).queryByTestId('chat-message-avatar')).toBeNull();
      expect(within(controls).getByTestId('participant-executor')).toBeInTheDocument();
    }
  });

  it('follows the other tabs with one agent, which has no lead to make', async () => {
    serve([coder], { local_work: { folder_name: 'repo' } });
    mount();
    const tab = await screen.findByTestId('side-panel-tab-participants');
    await openMenu(11);
    expect(screen.queryByTestId('participant-lead-make')).toBeNull();
    expect(screen.getByTestId('participant-address')).toBeInTheDocument();
    expect(screen.queryByTestId('side-panel-tab-team')).toBeNull();
    expect(tab).toBeInTheDocument();
  });

  it('makes another agent the lead keeping the other meta keys, and clears it', async () => {
    const calls = serve([coder, reviewer]);
    mount();
    await openMenu(12);
    fireEvent.click(await screen.findByTestId('participant-lead-make'));
    await waitFor(() => expect(calls.meta).toHaveLength(1));
    expect(calls.meta[0]).toEqual({ meta: { local_work: { folder_name: 'repo' }, steps_limit: 25, floor: { policy: 'lead', lead_participant_id: 12 } } });

    // The lead is Reviewer now: its menu clears it.
    await openMenu(12);
    fireEvent.click(await screen.findByTestId('participant-lead-clear'));
    await waitFor(() => expect(calls.meta).toHaveLength(2));
    expect(calls.meta[1]).toEqual({ meta: { local_work: { folder_name: 'repo' }, steps_limit: 25 } });
  });

  it('runs an agent in the cloud with its whole entity_settings', async () => {
    const calls = serve([coder, reviewer]);
    mount();
    await openMenu(11);
    fireEvent.click(await screen.findByTestId('participant-run-cloud'));
    await waitFor(() => expect(calls.settings).toEqual([{ id: '11', body: { version_id: 9, executor: 'cloud' } }]));
  });

  it('clears the lead before it removes the lead from the thread', async () => {
    const calls = serve([coder, reviewer]);
    mount();
    await openMenu(11);
    fireEvent.click(await screen.findByTestId('participant-remove'));
    await waitFor(() => expect(calls.deleted).toEqual(['11']));
    expect(calls.order).toEqual(['meta', 'delete']);
    expect(calls.meta[0]).toEqual({ meta: { local_work: { folder_name: 'repo' }, steps_limit: 25 } });
  });

  it('addresses an agent in the composer', async () => {
    serve([coder, reviewer]);
    const onAddress = vi.fn();
    mount(onAddress);
    await openMenu(12);
    fireEvent.click(await screen.findByTestId('participant-address'));
    expect(onAddress).toHaveBeenCalledWith('Reviewer');
  });
});

describe('where an agent runs (issue 3055)', () => {
  it('sets or drops executor: cloud, keeping every other setting', () => {
    expect(executorSettings(reviewer, false)).toEqual({ version_id: 10, llm_settings: { model: 'm' } });
    expect(executorSettings(coder, true)).toEqual({ version_id: 9, executor: 'cloud' });
    expect(executorSettings(null, true)).toEqual({ executor: 'cloud' });
  });
});

describe('each agent\'s model in the thread (#3145)', () => {
  it('opens a local agent\'s model from its row, offers none for a cloud agent, and badges only a chosen model', async () => {
    serve([coder, reviewer]);
    const edit = vi.fn();
    const models: AgentModels = {
      overrideOf: (applicationId) => (applicationId === 5 ? { name: 'gpt-5' } : undefined),
      choose: vi.fn(),
      entries: () => [],
      handed: () => [],
      editing: null,
      edit,
      dropPending: vi.fn(),
    };
    mount(vi.fn(), models);
    await screen.findByTestId('side-panel-tab-team');
    await waitFor(() => expect(screen.getAllByTestId('participant-model-badge')).toHaveLength(1));
    expect(screen.getByTestId('participant-model-badge')).toHaveTextContent('gpt-5');
    expect(screen.getByTestId('participant-model-badge')).toHaveAccessibleName('Model in this thread: gpt-5');
    await openMenu(11);
    fireEvent.click(await screen.findByTestId('participant-model'));
    expect(edit).toHaveBeenCalledWith(5);
    // Reviewer runs in the cloud, on its own version's model.
    await openMenu(12);
    expect(screen.queryByTestId('participant-model')).toBeNull();
    // Thread settings has its place in the tab, above the project instructions.
    expect(screen.getByRole('heading', { name: 'Thread settings', hidden: true })).toBeInTheDocument();
  });
});
