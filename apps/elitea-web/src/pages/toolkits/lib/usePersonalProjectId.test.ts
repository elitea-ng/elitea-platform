import { describe, expect, it } from 'vitest';

import { selectIsTeamProject, selectPersonalProjectId } from './usePersonalProjectId';

describe('selectPersonalProjectId', () => {
  it('returns undefined for a non-object context', () => {
    expect(selectPersonalProjectId(undefined)).toBeUndefined();
    expect(selectPersonalProjectId(null)).toBeUndefined();
    expect(selectPersonalProjectId('nope')).toBeUndefined();
  });

  it('returns undefined when auth.getUser is absent', () => {
    expect(selectPersonalProjectId({})).toBeUndefined();
    expect(selectPersonalProjectId({ auth: {} })).toBeUndefined();
  });

  it('returns undefined when auth.getUser() resolves no user', () => {
    expect(selectPersonalProjectId({ auth: { getUser: () => undefined } })).toBeUndefined();
  });

  it('returns undefined when the user carries no personal project', () => {
    expect(selectPersonalProjectId({ auth: { getUser: () => ({}) } })).toBeUndefined();
  });

  it('returns the id the user carries', () => {
    expect(selectPersonalProjectId({ auth: { getUser: () => ({ personal_project_id: '7' }) } })).toBe('7');
  });
});

/**
 * #902/ELITEA-0726: the three-valued scope. `useIsTeamProject`'s `false` for
 * an unknown id is right for a warning modal and wrong for a visible label,
 * which is why this second reading exists.
 */
describe('selectIsTeamProject', () => {
  it('answers undefined while either id is unknown, never a guessed scope', () => {
    expect(selectIsTeamProject(undefined, 'p1')).toBeUndefined();
    expect(selectIsTeamProject('', 'p1')).toBeUndefined();
    expect(selectIsTeamProject('proj-9', undefined)).toBeUndefined();
  });

  it('answers true for a project that is not the personal one, false for the personal one', () => {
    expect(selectIsTeamProject('proj-9', 'p1')).toBe(true);
    expect(selectIsTeamProject('p1', 'p1')).toBe(false);
  });
});
