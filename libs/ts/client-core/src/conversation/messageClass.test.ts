import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import { MESSAGE_CLASS, PARTICIPANT_KIND, classifyMessage, participantKindOf, type ParticipantKind } from './messageClass';

const roster = new Map<number, ParticipantKind>([
  [1, PARTICIPANT_KIND.Human],
  [2, PARTICIPANT_KIND.Agent],
  [3, PARTICIPANT_KIND.Agent],
  [4, PARTICIPANT_KIND.EphemeralAgent],
]);
const kindOfParticipant = (id: number) => roster.get(id);

describe('classifyMessage', () => {
  it('classes each author kind', () => {
    assert.equal(classifyMessage({ authorKind: 'human' }), MESSAGE_CLASS.Human);
    assert.equal(classifyMessage({ authorKind: 'agent', addressedTo: [] }), MESSAGE_CLASS.AgentToHumans);
    assert.equal(classifyMessage({ authorKind: 'ephemeral_agent' }), MESSAGE_CLASS.Ephemeral);
    assert.equal(classifyMessage({ authorKind: 'system' }), MESSAGE_CLASS.System);
  });

  it('classes a row without author_kind from the role the client derived', () => {
    assert.equal(classifyMessage({ fallbackRole: 'user' }), MESSAGE_CLASS.Human);
    assert.equal(classifyMessage({ fallbackRole: 'assistant' }), MESSAGE_CLASS.AgentToHumans);
    assert.equal(classifyMessage({}), MESSAGE_CLASS.AgentToHumans);
    assert.equal(classifyMessage({ authorKind: 'robot', fallbackRole: 'user' }), MESSAGE_CLASS.Human);
  });

  it('a side message is agent-to-agent talk', () => {
    assert.equal(classifyMessage({ authorKind: 'agent', visibility: 'side', addressedTo: [3] }), MESSAGE_CLASS.AgentToAgent);
  });

  it('an agent message to agents only is agent-to-agent talk, to anyone else it is not', () => {
    assert.equal(classifyMessage({ authorKind: 'agent', addressedTo: [3, 4], kindOfParticipant }), MESSAGE_CLASS.AgentToAgent);
    assert.equal(classifyMessage({ authorKind: 'agent', addressedTo: [1, 3], kindOfParticipant }), MESSAGE_CLASS.AgentToHumans);
    assert.equal(classifyMessage({ authorKind: 'agent', addressedTo: [9], kindOfParticipant }), MESSAGE_CLASS.AgentToHumans);
    // Without a roster the addressees cannot be read.
    assert.equal(classifyMessage({ authorKind: 'agent', addressedTo: [3] }), MESSAGE_CLASS.AgentToHumans);
  });

  it('a heartbeat answer to people is a heartbeat; side talk outranks it', () => {
    assert.equal(classifyMessage({ authorKind: 'agent', cause: 'heartbeat' }), MESSAGE_CLASS.Heartbeat);
    assert.equal(classifyMessage({ authorKind: 'agent', cause: 'heartbeat', visibility: 'side' }), MESSAGE_CLASS.AgentToAgent);
  });

  it('ignores junk addressees', () => {
    assert.equal(classifyMessage({ authorKind: 'agent', addressedTo: ['3', -1, 0, 1.5, null], kindOfParticipant }), MESSAGE_CLASS.AgentToHumans);
  });
});

describe('participantKindOf', () => {
  it('reads the four kinds only', () => {
    for (const kind of Object.values(PARTICIPANT_KIND)) assert.equal(participantKindOf(kind), kind);
    assert.equal(participantKindOf('application'), undefined);
    assert.equal(participantKindOf(undefined), undefined);
  });
});
