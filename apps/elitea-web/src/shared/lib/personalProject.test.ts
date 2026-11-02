import { describe, expect, it } from 'vitest';

import { personalProjectIdOf, selectPersonalProjectId } from './personalProject';

describe('personalProjectIdOf', () => {
  it.each([
    [{ personal_project_id: 7 }, '7'],
    [{ personal_project_id: '7' }, '7'],
    [{ personal_project_id: '' }, undefined],
    [{ personal_project_id: Number.NaN }, undefined],
    [{ personal_project_id: null }, undefined],
    [{}, undefined],
    [undefined, undefined],
    [null, undefined],
  ])('reads %j as %j', (user, expected) => {
    expect(personalProjectIdOf(user)).toBe(expected);
  });
});

describe('selectPersonalProjectId', () => {
  it('reads the session user off the router context', () => {
    expect(selectPersonalProjectId({ auth: { getUser: () => ({ personal_project_id: 12 }) } })).toBe('12');
  });

  it('is undefined without a context, an auth or a user', () => {
    expect(selectPersonalProjectId(undefined)).toBeUndefined();
    expect(selectPersonalProjectId({})).toBeUndefined();
    expect(selectPersonalProjectId({ auth: {} })).toBeUndefined();
    expect(selectPersonalProjectId({ auth: { getUser: () => undefined } })).toBeUndefined();
  });
});
