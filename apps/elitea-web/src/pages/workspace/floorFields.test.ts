import { describe, expect, it } from 'vitest';

import { companionParticipant, executorFixes, threadAgentsOf, type ThreadAgent } from './useEnsureConversation';
import { floorFields, routedRequest } from './useSendPrompt';

const coder: ThreadAgent = { participantId: 77, applicationId: 5, name: 'Coder', versionId: 9, cloud: false };
const reviewer: ThreadAgent = { participantId: 88, applicationId: 6, name: 'Reviewer', versionId: 10, cloud: true };

describe('floorFields (ADR-0050 decision 7f)', () => {
  it('addresses the agents a message names, and starts on the first one named', () => {
    expect(floorFields('@Reviewer then @Coder: check the diff', [coder, reviewer])).toEqual({ addressed_to: [88, 77], application_id: 6, version_id: 10, cloud_agent: true });
    expect(floorFields('@Coder and @Reviewer', [coder, reviewer])).toEqual({ addressed_to: [77, 88], application_id: 5, version_id: 9 });
  });

  it('changes nothing for a thread of one agent, or a message that names none', () => {
    expect(floorFields('@Coder fix it', [coder])).toEqual({});
    expect(floorFields('fix it', [coder, reviewer])).toEqual({});
    expect(floorFields('mail me@Coderhouse', [coder, reviewer])).toEqual({});
  });
});

describe('companionParticipant', () => {
  it('marks a cloud agent’s mapping executor cloud, and a local one not at all', () => {
    const cloud = companionParticipant(42, { applicationId: 6, applicationName: 'Reviewer', versionId: 10, agentType: 'openai', cloud: true });
    expect(cloud.entity_settings).toMatchObject({ version_id: 10, executor: 'cloud' });
    expect(cloud.entity_meta).toEqual({ id: 6, name: 'Reviewer', project_id: 42 });
    const local = companionParticipant(42, { applicationId: 5, applicationName: 'Coder', versionId: 9, agentType: 'openai', cloud: false });
    expect(local.entity_settings).not.toHaveProperty('executor');
  });
});

describe('routedRequest', () => {
  const base = { workspace_id: 'w1', project_id: 42, conversation_id: '77', application_id: 5, version_id: 9, prompt: 'p', plan_mode: false, mentions: [], skills: [] };
  const grant = { grant_id: 'g', continue_step_limit: true, continue_tool_call_cap: true, hook_asks: false, remote_confirmations: false };
  const model = { name: 'gpt-test' };

  it('drops the run-to-completion grant from a routed message or a cloud agent’s turn, and the model from a cloud agent’s turn', () => {
    const routed = routedRequest({ ...base, unattended: grant, model, addressed_to: [88, 77], cloud_agent: true });
    expect(routed).not.toHaveProperty('unattended');
    expect(routed).not.toHaveProperty('model');
    const local = routedRequest({ ...base, unattended: grant, model, addressed_to: [77, 88] });
    expect(local).not.toHaveProperty('unattended');
    expect(local.model).toBe(model);
    expect(routedRequest({ ...base, unattended: grant, cloud_agent: true })).not.toHaveProperty('unattended');
  });

  it('drops plan mode and bypass from a cloud agent’s turn, and keeps them on a routed local one', () => {
    const cloud = routedRequest({ ...base, plan_mode: true, bypass_approvals: true, cloud_agent: true });
    expect(cloud.plan_mode).toBe(false);
    expect(cloud).not.toHaveProperty('bypass_approvals');
    const local = routedRequest({ ...base, plan_mode: false, bypass_approvals: true, addressed_to: [77, 88] });
    expect(local.bypass_approvals).toBe(true);
    expect(routedRequest({ ...base, plan_mode: true, addressed_to: [77, 88] }).plan_mode).toBe(true);
  });

  it('keeps a one-agent message as it was', () => {
    expect(routedRequest({ ...base, unattended: grant, model })).toEqual({ ...base, unattended: grant, model });
  });
});

describe('threadAgentsOf', () => {
  it('reads a reopened thread’s agents from its participants, a cloud one marked', () => {
    const agents = threadAgentsOf([
      { id: 70, entity_name: 'user', entity_meta: { id: 1 } },
      { id: 77, entity_name: 'application', entity_meta: { id: 5, name: 'Coder' }, entity_settings: { version_id: 9 } },
      { id: 88, entity_name: 'application', entity_meta: { id: 6 }, entity_settings: { version_id: 10, executor: 'cloud' }, meta: { name: 'Reviewer' } },
      // No version: not an agent a turn can start on.
      { id: 99, entity_name: 'application', entity_meta: { id: 7, name: 'Broken' } },
    ]);
    expect(agents).toEqual([
      { participantId: 77, applicationId: 5, name: 'Coder', versionId: 9, cloud: false },
      { participantId: 88, applicationId: 6, name: 'Reviewer', versionId: 10, cloud: true },
    ]);
    expect(threadAgentsOf(undefined)).toEqual([]);
  });
});

describe('executorFixes', () => {
  const agent = (participantId: number, cloud: boolean): ThreadAgent => ({ participantId, applicationId: participantId, name: `A${participantId}`, versionId: 1, cloud });
  const participants = [
    { id: 77, entity_name: 'application', entity_settings: { version_id: 1, agent_type: 'openai' } },
    { id: 88, entity_name: 'application', entity_settings: { version_id: 1, executor: 'cloud' } },
  ];

  it('rewrites a kept mapping whose executor the person changed, keeping its other settings', () => {
    expect(executorFixes(participants, [agent(77, true), agent(88, false)])).toEqual([
      { participantId: 77, settings: { version_id: 1, agent_type: 'openai', executor: 'cloud' } },
      { participantId: 88, settings: { version_id: 1 } },
    ]);
  });

  it('leaves a mapping that already says where the agent runs, and one the detail does not list', () => {
    expect(executorFixes(participants, [agent(77, false), agent(88, true), agent(99, true)])).toEqual([]);
  });
});
