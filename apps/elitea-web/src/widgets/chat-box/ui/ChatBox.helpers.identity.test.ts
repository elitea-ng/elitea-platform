import { describe, expect, it, vi } from 'vitest';

import { toParticipant } from './ChatBox.helpers';

describe('participant identity wire fields', () => {
  it('normalizes numeric Main identities without coercing names', () => {
    const participant = toParticipant({
      id: 13, entity_name: 'user',
      entity_meta: { id: 3, project_id: 1, name: 99 },
      meta: { user_name: 'E2E Admin' },
    });

    expect(participant?.entityMeta).toEqual({ id: '3', projectId: '1' });
    expect(participant?.meta?.userName).toBe('E2E Admin');
  });

  it('retains string identities and names exactly', () => {
    const participant = toParticipant({
      id: '13', entity_name: 'user',
      entity_meta: { id: '003', project_id: 'project-uuid', name: 'Exact name' },
    });

    expect(participant?.entityMeta).toEqual({ id: '003', projectId: 'project-uuid', name: 'Exact name' });
  });

  it.each([NaN, Infinity, -Infinity, Number.MAX_SAFE_INTEGER + 1, 3.5, true, null, ['3']])(
    'refuses nonidentity primitives and unsafe numeric identity %j', (identity) => {
      const participant = toParticipant({
        id: '13', entity_name: 'user', entity_meta: { id: identity, project_id: identity },
      });

      expect(participant?.entityMeta).toEqual({});
    },
  );

  it('never calls object coercion for identity or name fields', () => {
    const toString = vi.fn(() => '3');
    const identity = { toString };
    const participant = toParticipant({
      id: '13', entity_name: 'user', entity_meta: { id: identity, project_id: identity, name: identity },
    });

    expect(participant?.entityMeta).toEqual({});
    expect(toString).not.toHaveBeenCalled();
  });
});
