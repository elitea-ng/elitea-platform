/**
 * A thread run by a saved team (#3053): the lead a new thread's first send writes, the first message routed to it, and
 * a cloud thread's message routed as the chat routes it.
 */
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';

import { server } from '../../test/setup';

import { cloudRoute } from './useCloudChat';
import { companionParticipant, teamLeadOf, writeThreadLead, type ThreadAgent } from './useEnsureConversation';
import { floorFields, leadsTheTeam } from './useSendPrompt';
import type { WorkTeam } from './workTeams';

const BASE = '/api/v2';
const globals = globalThis as unknown as Record<string, unknown>;

const reviewer: ThreadAgent = { participantId: 88, applicationId: 6, name: 'Reviewer', versionId: 10, cloud: false };
const coder: ThreadAgent = { participantId: 77, applicationId: 5, name: 'Coder', versionId: 9, cloud: false };
const crew: WorkTeam = {
  id: 3,
  name: 'Review crew',
  lead: true,
  primary: { applicationId: 6, versionId: 10, name: 'Reviewer' },
  others: [{ applicationId: 5, versionId: 9, name: 'Coder' }],
};

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

describe('a team thread is created with its lead', () => {
  it('the composer agent leads only a lead team whose lead it is', () => {
    expect(leadsTheTeam({ team: crew }, '6')).toBe(true);
    expect(leadsTheTeam({ team: crew }, '5')).toBe(false);
    expect(leadsTheTeam({ team: { ...crew, lead: false } }, '6')).toBe(false);
    expect(leadsTheTeam({}, '6')).toBe(false);
  });

  it('writes the lead in a thread it creates or completes, never on a later send', () => {
    const created = { lead: true, conversationId: '', applicationId: 6 };
    expect(teamLeadOf(created, [reviewer, coder])).toBe(88);
    expect(teamLeadOf({ ...created, conversationId: '77', completeSetup: true }, [reviewer, coder])).toBe(88);
    expect(teamLeadOf({ ...created, conversationId: '77' }, [reviewer, coder])).toBeUndefined();
    expect(teamLeadOf({ ...created, lead: false }, [reviewer, coder])).toBeUndefined();
    // One agent answers anyway; an add that did not answer the composer's agent first names no lead.
    expect(teamLeadOf(created, [reviewer])).toBeUndefined();
    expect(teamLeadOf(created, [coder, reviewer])).toBeUndefined();
  });

  it('writes the floor keeping every other meta key, as Teams → Start writes it', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/conversation/prompt_lib/42/77`, () =>
        HttpResponse.json({ id: 77, name: 'fix it', meta: { local_work: { folder_name: 'repo' }, internal_tools: ['planner'] }, participants: [] }),
      ),
    );
    const edits: unknown[] = [];
    const meta = await writeThreadLead(42, '77', 88, (params) => {
      edits.push(params);
      return Promise.resolve();
    });
    const written = { local_work: { folder_name: 'repo' }, internal_tools: ['planner'], floor: { policy: 'lead', lead_participant_id: 88 } };
    expect(meta).toEqual(written);
    expect(edits).toEqual([{ projectId: 42, id: '77', meta: written }]);
  });

  it('adds a member without a type of its own, as Teams → Start adds it', () => {
    const settings = companionParticipant(42, { applicationId: 5, applicationName: 'Coder', versionId: 9, agentType: '', cloud: false }).entity_settings;
    expect(settings).toEqual({ version_id: 9, variables: [], icon_meta: {} });
  });

  it('routes the new thread\'s first message to the lead it wrote', () => {
    const meta = { floor: { policy: 'lead', lead_participant_id: 88 } };
    expect(floorFields('fix the flaky test', [reviewer, coder], meta)).toEqual({ application_id: 6, version_id: 10, participant_id: 88 });
    // Without the written floor the start named nobody (Main then chose the composer's agent).
    expect(floorFields('fix the flaky test', [reviewer, coder], undefined)).toEqual({});
  });
});

describe('cloudRoute: a cloud work thread routes as the chat does', () => {
  const participants = [
    { id: 7, entity_name: 'user', entity_meta: { id: 3 } },
    { id: 77, entity_name: 'application', entity_meta: { id: 5, name: 'Coder' }, entity_settings: { version_id: 9 } },
    { id: 88, entity_name: 'application', entity_meta: { id: 6, name: 'Reviewer' }, entity_settings: { version_id: 10 } },
  ];
  const lead = { floor: { policy: 'lead', lead_participant_id: 88 } };

  it('an unaddressed message goes to the lead', () => {
    expect(cloudRoute('carry on', participants, lead, 77)).toEqual({ participant_id: 88 });
  });

  it('a message naming agents is addressed to them, the first named answering first', () => {
    expect(cloudRoute('@Coder fix it, then @Reviewer check it', participants, lead, 77)).toEqual({ participant_id: 77, addressed_to: [77, 88] });
  });

  it('with no lead an unaddressed message goes to the thread\'s agent', () => {
    expect(cloudRoute('carry on', participants, {}, 77)).toEqual({ participant_id: 77 });
  });

  it('one agent: as before, no address', () => {
    expect(cloudRoute('@Coder carry on', participants.slice(0, 2), lead, 77)).toEqual({ participant_id: 77 });
  });
});
