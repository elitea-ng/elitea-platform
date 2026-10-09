import { describe, expect, it } from 'vitest';

import type { Participant } from '@/entities/participant';

import type { CreatedConversation, ParticipantInput, ParticipantSelectionRuntime } from './useAddEntityParticipant.helpers';
import { applyParticipantSelection } from './useAddEntityParticipant.helpers';
import { findSelectedConversationParticipant, selectedParticipantInput } from './useAddEntityParticipant';

describe('selectedParticipantInput', () => {
  it('converts an OpenAPI toolkit catalog row into the participant API shape', () => {
    expect(selectedParticipantInput({
      id: '20',
      name: 'rust_openapi_echo',
      participantType: 'toolkit',
      project_id: '2',
      type: 'openapi',
    })).toMatchObject({
      entity_name: 'toolkit',
      entity_meta: { id: '20', name: 'rust_openapi_echo', project_id: '2' },
      entity_settings: { toolkit_type: 'openapi' },
    });
  });

  it('rejects a row without a supported participant type', () => {
    expect(selectedParticipantInput({ id: '20', name: 'rust_openapi_echo' })).toBeUndefined();
  });

  it('finds an attached toolkit by entity and project identity', () => {
    const participant = {
      id: '31',
      entityName: 'toolkit' as const,
      entityMeta: { id: '20', name: 'rust_openapi_echo', projectId: '2' },
      entitySettings: { toolkitType: 'openapi' },
    };

    expect(findSelectedConversationParticipant({
      id: '20',
      participantType: 'toolkit',
      project_id: '2',
      type: 'openapi',
    }, [participant])).toBe(participant);
  });

  it('matches a pipeline stored as an application with pipeline settings', () => {
    const participant = {
      id: '41',
      entityName: 'application' as const,
      entityMeta: { id: '9', projectId: '2' },
      entitySettings: { agentType: 'pipeline' },
    };

    expect(findSelectedConversationParticipant({
      id: '9',
      participantType: 'pipeline',
      project_id: '2',
      agent_type: 'pipeline',
    }, [participant])).toBe(participant);
  });

  it('does not treat a pipeline as the same plain application', () => {
    const participant = {
      id: '41',
      entityName: 'application' as const,
      entityMeta: { id: '9', projectId: '2' },
      entitySettings: { agentType: 'pipeline' },
    };

    expect(findSelectedConversationParticipant({
      id: '9',
      participantType: 'application',
      project_id: '2',
    }, [participant])).toBeUndefined();
  });
});

// ---------------------------------------------------------------------------
// applyParticipantSelection — the async half, with no React tree involved.
// ---------------------------------------------------------------------------

/** The exact row `GET /elitea_core/applications/prompt_lib/{projectId}?agents_type=classic` returns: no version in any form. */
const AGENT_LIST_ROW = {
  // Ids are strings on this wire: "Numeric id serialized as string"
  // (`shared/api/generated/model/applicationVersionDetail.zod.ts:54`).
  id: '12',
  project_id: '2',
  name: 'RustProbe',
  agent_type: 'openai',
  participantType: 'application',
  meta: {},
};

/** The detail row, which is the only place `version_details` comes from. */
const AGENT_DETAIL_ROW = {
  id: '12',
  project_id: '2',
  name: 'RustProbe',
  agent_type: 'openai',
  version_details: { id: '34', name: 'latest' },
  versions: [{ id: '33', name: 'v1' }, { id: '34', name: 'latest' }],
};

function participantRow(id: string): Participant {
  return { id, entityName: 'application', entityMeta: { id: '12', projectId: '2' }, entitySettings: {} };
}

interface RecordedSelection {
  readonly runtime: ParticipantSelectionRuntime;
  readonly added: { projectId: string | number; conversationId: string; participants: readonly ParticipantInput[] }[];
  readonly detached: { id: string; conversationId: string }[];
  readonly announced: CreatedConversation[];
  readonly activated: unknown[];
  readonly order: string[];
}

function selectionRuntime(overrides: Partial<ParticipantSelectionRuntime> = {}): RecordedSelection {
  const added: RecordedSelection['added'] = [];
  const detached: RecordedSelection['detached'] = [];
  const announced: CreatedConversation[] = [];
  const activated: unknown[] = [];
  const order: string[] = [];
  const runtime: ParticipantSelectionRuntime = {
    projectId: 2,
    conversationId: 55,
    onChangeParticipant: (participant) => { activated.push(participant); },
    addParticipant: (input) => { order.push('add'); added.push(input); return Promise.resolve([participantRow('101')]); },
    deleteParticipant: (input) => { detached.push(input); return Promise.resolve(); },
    fetchDetails: () => { order.push('fetch'); return Promise.resolve(AGENT_DETAIL_ROW); },
    createConversation: () => { order.push('create'); return Promise.resolve({ id: 77, uuid: 'u-77' }); },
    onConversationCreated: (conversation) => { order.push('announce'); announced.push(conversation); },
    ...overrides,
  };
  return { runtime, added, detached, announced, activated, order };
}

describe('applyParticipantSelection', () => {
  it('creates the participant with the version_id the detail response marks as current', async () => {
    const { runtime, added } = selectionRuntime();

    await applyParticipantSelection(AGENT_LIST_ROW, undefined, runtime);

    expect(added).toHaveLength(1);
    expect(added[0]?.conversationId).toBe('55');
    expect(added[0]?.participants[0]?.entity_settings['version_id']).toBe('34');
  });

  it('resolves the version for a pipeline as well as an agent', async () => {
    const { runtime, added } = selectionRuntime({
      fetchDetails: () => Promise.resolve({ ...AGENT_DETAIL_ROW, agent_type: 'pipeline' }),
    });

    await applyParticipantSelection({ ...AGENT_LIST_ROW, participantType: 'pipeline', agent_type: 'pipeline' }, undefined, runtime);

    expect(added[0]?.participants[0]?.entity_settings['version_id']).toBe('34');
  });

  it('attaches the row as it stands when the detail response is empty', async () => {
    // `fetchOriginalDetails` answers `{}` rather than null on every miss
    // (`result?.data || {}`, and the toolkit scan's own `{}`), so the empty
    // OBJECT is the only shape the merge has to defend against — an agent with
    // no version still gets attached, just without a `version_id`.
    const { runtime, added } = selectionRuntime({ fetchDetails: () => Promise.resolve({}) });

    await applyParticipantSelection(AGENT_LIST_ROW, undefined, runtime);

    expect(added).toHaveLength(1);
    expect(added[0]?.participants[0]?.entity_meta['id']).toBe('12');
    expect(added[0]?.participants[0]?.entity_settings['version_id']).toBeUndefined();
  });

  it('does not look up a version for a toolkit, which has none', async () => {
    const { runtime, added, order } = selectionRuntime();

    await applyParticipantSelection({ id: '20', project_id: '2', participantType: 'toolkit', type: 'openapi' }, undefined, runtime);

    expect(order).not.toContain('fetch');
    expect(added).toHaveLength(1);
  });

  it('creates a conversation when the chat has none, and announces it only once the agent is on it', async () => {
    const { runtime, added, announced, activated, order } = selectionRuntime({ conversationId: undefined });

    await applyParticipantSelection(AGENT_LIST_ROW, undefined, runtime);

    expect(order).toEqual(['fetch', 'create', 'add', 'announce']);
    expect(added[0]?.conversationId).toBe('77');
    expect(announced).toEqual([{ id: 77, uuid: 'u-77', activeParticipantId: '101' }]);
    expect(activated).toEqual([participantRow('101')]);
  });

  it('leaves no empty conversation behind when the version lookup fails', async () => {
    const { runtime, order } = selectionRuntime({
      conversationId: undefined,
      fetchDetails: () => Promise.reject(new Error('boom')),
    });

    await expect(applyParticipantSelection(AGENT_LIST_ROW, undefined, runtime)).rejects.toThrow('boom');
    expect(order).not.toContain('create');
  });

  it('detaches an already attached toolkit instead of adding it twice', async () => {
    const { runtime, added, detached } = selectionRuntime();
    const attached: Participant = { id: '31', entityName: 'toolkit', entityMeta: { id: '20', projectId: '2' } };

    await applyParticipantSelection({ id: '20', project_id: '2', participantType: 'toolkit' }, attached, runtime);

    expect(detached).toEqual([{ projectId: 2, conversationId: '55', id: '31' }]);
    expect(added).toHaveLength(0);
  });

  it('does nothing without a project, which is half the route', async () => {
    const { runtime, order } = selectionRuntime({ projectId: undefined, conversationId: undefined });

    await applyParticipantSelection(AGENT_LIST_ROW, undefined, runtime);

    expect(order).toEqual([]);
  });
});
