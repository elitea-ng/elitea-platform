import { describe, expect, it } from 'vitest';

import { applicationParticipant, editorTestIdentityKey } from './editorTestConversation';

describe('editorTestIdentityKey', () => {
  it('joins positive decimal ids', () => {
    expect(editorTestIdentityKey(['6', '9', '42', '7'])).toBe('6:9:42:7');
  });

  it.each([[['6', undefined, '42']], [['6', '0', '42']], [['6', '09', '42']], [['6', 'x', '42']], [['6', '-1', '42']]])(
    'is undefined for %j',
    (parts) => {
      expect(editorTestIdentityKey(parts)).toBeUndefined();
    },
  );
});

describe('applicationParticipant', () => {
  it('pins the version and carries the entity project', () => {
    expect(applicationParticipant({ applicationId: '42', projectId: '9', name: 'My Agent', versionId: '7', agentType: 'openai' })).toEqual({
      entity_name: 'application',
      entity_meta: { id: '42', name: 'My Agent', project_id: '9' },
      entity_settings: { version_id: '7', agent_type: 'openai', variables: [], icon_meta: {} },
    });
  });

  it('leaves the name off when there is none', () => {
    const row = applicationParticipant({ applicationId: '42', projectId: '9', versionId: '7', agentType: 'pipeline' });
    expect(row['entity_meta']).toEqual({ id: '42', project_id: '9' });
  });
});
