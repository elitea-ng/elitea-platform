/**
 * A saved team as the runner of a new Local work thread (#3053): the picker's team list (gated on
 * `models.teams.list`), picking a team (its lead on the team's version as the composer's agent, the other members as
 * companions on this computer, a new thread), and the thread creation that writes the lead as `meta.floor`.
 */
import { act, render, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';

import { server } from '../../test/setup';

import { useAgentSelection, type AgentSelection } from './useAgentSelection';
import { useEnsureConversation, type EnsureConversationInput, type EnsuredConversation } from './useEnsureConversation';

const BASE = '/api/v2';
const globals = globalThis as unknown as Record<string, unknown>;

const member = (applicationId: number, versionId: number, name: string, projectId = 42) => ({ application_id: applicationId, project_id: projectId, version_id: versionId, name });
const TEAMS = {
  rows: [
    { id: 3, project_id: 42, name: 'Review crew', description: '', floor_policy: 'lead', lead_position: 2, members: [member(5, 9, 'Coder'), member(6, 10, 'Reviewer')], created_by: 3, created_at: '2026-11-20T08:00:00Z', updated_at: '2026-11-20T08:00:00Z' },
    { id: 4, project_id: 42, name: 'Mixed crew', description: '', floor_policy: 'addressed', lead_position: null, members: [member(5, 9, 'Coder'), member(900, 901, 'Helper', 1)], created_by: 3, created_at: '2026-11-20T08:00:00Z', updated_at: '2026-11-20T08:00:00Z' },
  ],
  total: 2,
};

function serveProject(permissions: readonly string[]): void {
  server.use(
    http.get(`${BASE}/auth/permissions/prompt_lib/42`, () => HttpResponse.json(permissions.map((name) => ({ name, enabled: true })))),
    http.get(`${BASE}/elitea_core/applications/prompt_lib/42`, ({ request }) =>
      HttpResponse.json(new URL(request.url).searchParams.get('agents_type') === 'pipeline' ? { rows: [], total: 0 } : { rows: [{ id: '5', name: 'Coder' }, { id: '6', name: 'Reviewer' }], total: 2 }),
    ),
    http.get(`${BASE}/elitea_core/application/prompt_lib/42/:id`, ({ params }) =>
      HttpResponse.json(
        String(params['id']) === '6'
          ? { id: '6', name: 'Reviewer', versions: [{ id: '12', name: 'draft', agent_type: 'openai' }, { id: '10', name: 'base', agent_type: 'openai' }] }
          : { id: '5', name: 'Coder', versions: [{ id: '9', name: 'base', agent_type: 'openai' }] },
      ),
    ),
    http.get(`${BASE}/elitea_core/conversations/prompt_lib/42`, () => HttpResponse.json({ rows: [], total: 0 })),
    http.get(`${BASE}/elitea_core/agent_teams/prompt_lib/42`, () => HttpResponse.json(TEAMS)),
  );
}

beforeEach(() => {
  resetConfigForTests();
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: '1' };
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
  resetConfigForTests();
  delete globals['elitea_ui_config'];
});

function mountSelection(): { current: () => AgentSelection } {
  let latest: AgentSelection | undefined;
  function Probe(): null {
    latest = useAgentSelection('w-teams', 42);
    return null;
  }
  render(
    <AppProviders>
      <Probe />
    </AppProviders>,
  );
  return { current: () => latest as AgentSelection };
}

describe('useAgentSelection teams (#3053)', () => {
  it('lists the project\'s teams, the runnable ones with their lead, and picks one for a new thread', async () => {
    serveProject(['models.teams.list']);
    const hook = mountSelection();
    await waitFor(() => expect(hook.current().teams).toHaveLength(2));
    expect(hook.current().teams?.[1]).toEqual({ id: 4, name: 'Mixed crew', unavailable: 'other_project' });
    await waitFor(() => expect(hook.current().agentId).toBe('5'));

    act(() => hook.current().selectTeam?.(3));
    // The lead is the composer's agent on the team's version (not its first), the other member a local companion.
    await waitFor(() => expect(hook.current().versionId).toBe('10'));
    expect(hook.current().agentId).toBe('6');
    expect(hook.current().conversationId).toBe('');
    expect(hook.current().team).toMatchObject({ id: 3, lead: true });
    expect(hook.current().companions).toEqual([{ agentId: '5', name: 'Coder', versionId: 9, agentType: '', cloud: false }]);

    // Another agent: the thread is no longer the team's.
    act(() => hook.current().selectAgent('5'));
    expect(hook.current().team).toBeUndefined();
  });

  it('leaving the team\'s new thread drops the team and the members it brought', async () => {
    serveProject(['models.teams.list']);
    const hook = mountSelection();
    await waitFor(() => expect(hook.current().teams).toHaveLength(2));
    const pick = async () => {
      act(() => hook.current().selectTeam?.(3));
      await waitFor(() => expect(hook.current().versionId).toBe('10'));
    };
    const dropped = () => {
      expect(hook.current().team).toBeUndefined();
      expect(hook.current().companions).toEqual([]);
    };

    // An existing thread has its own agents: the team is not added to it.
    await pick();
    act(() => hook.current().selectConversation('11111111-1111-4111-8111-111111111111'));
    dropped();

    // The thread the team's first send created keeps it.
    await pick();
    act(() => hook.current().markCreated?.('22222222-2222-4222-8222-222222222222'));
    act(() => hook.current().selectConversation('22222222-2222-4222-8222-222222222222'));
    expect(hook.current().team).toMatchObject({ id: 3 });
    expect(hook.current().companions).toHaveLength(1);

    // Another lead agent: the team's other members go with the team.
    act(() => hook.current().selectConversation(''));
    await pick();
    act(() => hook.current().selectAgent('6'));
    dropped();

    // Another version of the lead is not the saved team: the members stay, the team's name and lead go.
    await pick();
    act(() => hook.current().selectVersion('9'));
    expect(hook.current().team).toBeUndefined();
    expect(hook.current().companions).toHaveLength(1);
  });

  it('a team that cannot run here is not picked', async () => {
    serveProject(['models.teams.list']);
    const hook = mountSelection();
    await waitFor(() => expect(hook.current().teams).toHaveLength(2));
    act(() => hook.current().selectTeam?.(4));
    expect(hook.current().team).toBeUndefined();
  });

  it('offers no team without models.teams.list', async () => {
    serveProject([]);
    const hook = mountSelection();
    await waitFor(() => expect(hook.current().agentId).toBe('5'));
    expect(hook.current().teams).toEqual([]);
  });
});

describe('useEnsureConversation for a team thread (#3053)', () => {
  it('adds the members as Teams → Start adds them, then writes the lead keeping the thread\'s meta', async () => {
    const calls = { participants: [] as unknown[], edits: [] as unknown[] };
    server.use(
      http.get(`${BASE}/social/author`, () => HttpResponse.json({ id: 3, name: 'Me' })),
      http.post(`${BASE}/elitea_core/conversations/prompt_lib/42`, () => HttpResponse.json({ id: 77, uuid: 'c0ffee00-0000-4000-8000-000000000077', name: 'fix it' })),
      http.post(`${BASE}/elitea_core/participants/prompt_lib/42/77`, async ({ request }) => {
        calls.participants.push(await request.json());
        return HttpResponse.json([
          { id: 7, entity_name: 'user' },
          { id: 88, entity_name: 'application', entity_meta: { id: 6 } },
          { id: 77, entity_name: 'application', entity_meta: { id: 5 } },
        ]);
      }),
      http.get(`${BASE}/elitea_core/conversation/prompt_lib/42/77`, () => HttpResponse.json({ id: 77, meta: { local_work: { folder_name: 'repo' } }, participants: [] })),
      http.put(`${BASE}/elitea_core/conversation/prompt_lib/42/77`, async ({ request }) => {
        calls.edits.push(await request.json());
        return HttpResponse.json({ id: 77 });
      }),
    );
    let ensure: ((input: EnsureConversationInput) => Promise<EnsuredConversation>) | undefined;
    let ready = false;
    function Probe(): null {
      ensure = useEnsureConversation();
      ready = useGetCurrentAuthor().isSuccess;
      return null;
    }
    render(
      <AppProviders>
        <Probe />
      </AppProviders>,
    );
    await waitFor(() => expect(ready).toBe(true));
    let ensured: EnsuredConversation | undefined;
    await act(async () => {
      ensured = await ensure?.({
        projectId: 42,
        conversationId: '',
        prompt: 'fix it',
        applicationId: 6,
        applicationName: 'Reviewer',
        versionId: 10,
        agentType: 'openai',
        folderName: 'repo',
        companions: [{ applicationId: 5, applicationName: 'Coder', versionId: 9, agentType: '', cloud: false }],
        lead: true,
      });
    });
    const floor = { policy: 'lead', lead_participant_id: 88 };
    expect(ensured).toMatchObject({ id: '77', meta: { local_work: { folder_name: 'repo' }, floor } });
    expect(calls.participants).toEqual([
      [
        { entity_name: 'user', entity_meta: { id: 3 } },
        { entity_name: 'application', entity_meta: { id: 6, name: 'Reviewer', project_id: 42 }, entity_settings: { version_id: 10, agent_type: 'openai', variables: [], icon_meta: {} } },
        { entity_name: 'application', entity_meta: { id: 5, name: 'Coder', project_id: 42 }, entity_settings: { version_id: 9, variables: [], icon_meta: {} } },
      ],
    ]);
    expect(calls.edits).toEqual([{ meta: { local_work: { folder_name: 'repo' }, floor } }]);
  });
});
