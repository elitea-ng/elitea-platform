import { describe, expect, it } from 'vitest';

import { ChatParticipantType } from '../model/constants';

import {
  canParticipantBeActiveInChat,
  getChatParticipantUniqueId,
  getParticipantName,
  isParticipantOKForChat,
  isParticipantsEqual,
  isSkippedContainerParticipant,
  transformParticipant,
} from './helpers';

describe('getChatParticipantUniqueId', () => {
  it('returns empty string for undefined', () => {
    expect(getChatParticipantUniqueId(undefined)).toBe('');
  });

  it('keys models by model_name and integration_uid', () => {
    const p = { entity_name: 'llm', entity_meta: { model_name: 'gpt-4', integration_uid: 'uid1', project_id: 'p1' } };
    expect(getChatParticipantUniqueId(p)).toBe('llm_gpt-4-uid1_p1');
  });

  it('keys applications by entity_meta.id', () => {
    const p = { entity_name: 'application', entity_meta: { id: 'a1', project_id: 'p2' } };
    expect(getChatParticipantUniqueId(p)).toBe('application_a1_p2');
  });

  it('reclassifies pipeline-type applications', () => {
    const p = { entity_name: 'application', entity_settings: { agent_type: 'pipeline' }, entity_meta: { id: 'x', project_id: 'p' } };
    expect(getChatParticipantUniqueId(p)).toBe('pipeline_x_p');
  });

  it('handles missing entity_meta gracefully', () => {
    const p = { entity_name: 'toolkit' };
    expect(getChatParticipantUniqueId(p)).toBe('toolkit__');
  });
});

describe('getParticipantName', () => {
  it('resolves application name from entity_meta', () => {
    expect(getParticipantName({ entity_name: 'application', entity_meta: { name: 'Bot' } }, 'System')).toBe('Bot');
  });

  it('resolves model name from entity_meta.model_name', () => {
    expect(getParticipantName({ entity_name: 'llm', entity_meta: { model_name: 'claude-3' } }, '')).toBe('claude-3');
  });

  it('resolves user name from meta.user_name', () => {
    expect(getParticipantName({ entity_name: 'user', meta: { user_name: 'Alice' } }, '')).toBe('Alice');
  });

  it('resolves dummy to systemSenderName', () => {
    expect(getParticipantName({ entity_name: 'dummy' }, 'Elitea')).toBe('Elitea');
  });

  it('returns empty for unknown entity type', () => {
    expect(getParticipantName({ entity_name: 'unknown_thing' }, 'X')).toBe('');
  });

  it('returns empty for undefined participant', () => {
    expect(getParticipantName(undefined, 'X')).toBe('');
  });
});

describe('isParticipantOKForChat', () => {
  it.each(['user', 'toolkit', 'application', 'pipeline'])('%s is OK', (name) => {
    expect(isParticipantOKForChat({ entity_name: name })).toBe(true);
  });

  it('models are not OK', () => {
    expect(isParticipantOKForChat({ entity_name: 'llm' })).toBe(false);
  });
});

describe('canParticipantBeActiveInChat', () => {
  it.each(['user', 'application', 'pipeline'])('%s can be active', (name) => {
    expect(canParticipantBeActiveInChat({ entity_name: name })).toBe(true);
  });

  it('toolkits cannot be active', () => {
    expect(canParticipantBeActiveInChat({ entity_name: 'toolkit' })).toBe(false);
  });
});

describe('transformParticipant', () => {
  it('builds model participant with defaults', () => {
    const result = transformParticipant(ChatParticipantType.Models, { model_name: 'gpt', integration_uid: 'u1' });
    expect(result.entity_name).toBe('llm');
    expect(result.entity_meta).toEqual({ model_name: 'gpt', integration_uid: 'u1' });
    expect(result.entity_settings?.max_tokens).toBe(4096);
    expect(result.entity_settings?.temperature).toBe(0.7);
  });

  it('builds application participant', () => {
    const result = transformParticipant(ChatParticipantType.Applications, { id: 'a1', name: 'Agent', project_id: 'p1' });
    expect(result.entity_name).toBe('application');
    expect(result.entity_meta).toEqual({ id: 'a1', name: 'Agent', project_id: 'p1' });
  });

  it('reclassifies pipeline-type agent_type to applications entity_name', () => {
    const result = transformParticipant(ChatParticipantType.Pipelines, { id: 'pp1', name: 'Pipe', agent_type: 'pipeline' });
    expect(result.entity_name).toBe('application');
  });

  it('passes through provided variables', () => {
    const vars = [{ key: 'x', value: '1' }];
    const result = transformParticipant(ChatParticipantType.Applications, { id: '1' }, vars);
    expect(result.entity_settings?.variables).toBe(vars);
  });

  it('extracts icon_meta (empty for toolkits)', () => {
    const result = transformParticipant(ChatParticipantType.Toolkits, { id: '1', meta: { icon_meta: { url: 'x' } } });
    expect(result.entity_settings?.icon_meta).toEqual({});
  });
});

describe('isParticipantsEqual', () => {
  it('compares models by entity_name, model_name, and integration_uid', () => {
    const a = { entity_name: 'llm', entity_meta: { model_name: 'gpt', integration_uid: 'u1' } };
    const b = transformParticipant(ChatParticipantType.Models, { model_name: 'gpt', integration_uid: 'u1' });
    expect(isParticipantsEqual(a, b, ChatParticipantType.Models, 'model_name')).toBe(true);
  });

  it('returns false for different model names', () => {
    const a = { entity_name: 'llm', entity_meta: { model_name: 'gpt', integration_uid: 'u1' } };
    const b = transformParticipant(ChatParticipantType.Models, { model_name: 'claude', integration_uid: 'u1' });
    expect(isParticipantsEqual(a, b, ChatParticipantType.Models, 'model_name')).toBe(false);
  });

  it('compares non-models by unique id', () => {
    const a = { entity_name: 'application', entity_meta: { id: 'x', project_id: 'p1' } };
    const b = transformParticipant(ChatParticipantType.Applications, { id: 'x', project_id: 'p1' });
    expect(isParticipantsEqual(a, b, ChatParticipantType.Applications, 'id')).toBe(true);
  });
});

describe('transformParticipant version resolution', () => {
  const detailRow = {
    id: 'a1',
    name: 'RustProbe',
    project_id: 'p1',
    version_details: { id: 34, name: 'latest', variables: [{ name: 'topic' }] },
    versions: [{ id: 33, name: 'v1' }, { id: 34, name: 'latest' }],
  };

  it('takes version_id from the detail row version_details for an application', () => {
    const result = transformParticipant(ChatParticipantType.Applications, detailRow);
    expect(result.entity_settings?.version_id).toBe(34);
  });

  it('takes version_id from the detail row version_details for a pipeline', () => {
    const result = transformParticipant(ChatParticipantType.Pipelines, { ...detailRow, agent_type: 'pipeline' });
    expect(result.entity_settings?.version_id).toBe(34);
  });

  it('prefers an explicit entity_settings.version_id over version_details', () => {
    const result = transformParticipant(ChatParticipantType.Applications, { ...detailRow, entity_settings: { version_id: '7' } });
    expect(result.entity_settings?.version_id).toBe('7');
  });

  it('leaves version_id absent on a list row, which carries no version at all', () => {
    const listRow = { id: 'a1', name: 'RustProbe', project_id: 'p1', agent_type: 'openai' };
    expect(transformParticipant(ChatParticipantType.Applications, listRow).entity_settings).not.toHaveProperty('version_id');
  });
});

// EliteaUI participants.helpers.test.js (#5778), on Main's participant meta (FU-244, PLAN §8.8).
describe('isSkippedContainerParticipant', () => {
  const agent = (meta: Record<string, unknown>, entityName = ChatParticipantType.Applications) => ({
    entity_name: entityName,
    entity_meta: { id: 7 },
    meta: { is_container: true, ...meta },
  });

  it('offers an agent whose subtree fits one level below the chat model', () => {
    expect(isSkippedContainerParticipant(agent({ agent_subtree_tiers: 2, max_agent_nesting_tiers: 3 }))).toBe(false);
  });

  it('flags an agent at the nesting limit', () => {
    expect(isSkippedContainerParticipant(agent({ agent_subtree_tiers: 3, max_agent_nesting_tiers: 3 }))).toBe(true);
  });

  it('never flags a participant that is not a container or not an agent', () => {
    expect(isSkippedContainerParticipant(agent({ is_container: false, agent_subtree_tiers: 3, max_agent_nesting_tiers: 3 }))).toBe(false);
    expect(isSkippedContainerParticipant(agent({}, ChatParticipantType.Toolkits))).toBe(false);
    expect(isSkippedContainerParticipant(undefined)).toBe(false);
  });

  it('falls back to the container ban without the tier fields', () => {
    expect(isSkippedContainerParticipant(agent({ agent_subtree_tiers: 2 }))).toBe(true);
    expect(isSkippedContainerParticipant(agent({}))).toBe(true);
  });
});
