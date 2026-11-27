import { describe, expect, it } from 'vitest';

import type { Application } from '@/shared/api/generated/model';

import { toApplicationRow } from './applicationRows';

function application(meta: Application['meta']): Application {
  return {
    id: '7',
    name: 'Dev Coordinator',
    tags: [],
    created_at: '2026-11-01T00:00:00Z',
    updated_at: '2026-11-20T10:00:00Z',
    owner_id: '1',
    is_forked: false,
    has_interrupt: false,
    meta,
  };
}

describe('toApplicationRow (issue 3133)', () => {
  it('carries the persona role, the avatar pack and the last change the list row surfaces on meta', () => {
    const pack = { schema: 'elitea.avatar_pack/v1', still: { url: '/media/still.webp' } };
    const row = toApplicationRow(application({ persona_role: 'Coordinator', avatar_pack: pack, icon_meta: { url: '/media/icon.png' } }), () => {});
    expect(row.personaRole).toBe('Coordinator');
    expect(row.avatarPack).toEqual(pack);
    expect(row.iconUrl).toBe('/media/icon.png');
    expect(row.updatedAt).toBe('2026-11-20T10:00:00Z');
  });

  it('leaves out a blank role and a pack that is not an object', () => {
    const row = toApplicationRow(application({ persona_role: '  ', avatar_pack: 'nope' }), () => {});
    expect(row).not.toHaveProperty('personaRole');
    expect(row).not.toHaveProperty('avatarPack');
    expect(toApplicationRow(application(null), () => {})).not.toHaveProperty('personaRole');
  });
});
