import { describe, expect, it } from 'vitest';

import { isPersonalProjectName, isPublicProject, isSuspendedProject, sortProjectsByName } from './selectors';
import type { Project } from './types';

const project = (id: number, name: string, suspended = false): Project => ({
  id,
  name,
  suspended,
});

describe('isPublicProject', () => {
  it('is true when the ids match as strings', () => {
    expect(isPublicProject(0, 0)).toBe(true);
    expect(isPublicProject('0', 0)).toBe(true);
  });

  it('is false when the ids differ', () => {
    expect(isPublicProject(5, 0)).toBe(false);
  });
});

/**
 * THE DEFECT this covers. The selector used to read
 * `project.status === 'suspended' || project.suspended`, and the spec's
 * `Project` schema declared `status` as a required `active | suspended` enum.
 * internal/api/v2/projects/handler.go:134-143 never emits a `status` field.
 * handler_test.go:145-150 fails the build if the body carries one. The
 * first arm therefore compared `undefined` on every real project. `suspended` is the
 * only suspension signal on the wire.
 */
describe('isSuspendedProject', () => {
  it('is true when the suspended flag is set', () => {
    expect(isSuspendedProject(project(1, 'p', true))).toBe(true);
  });

  it('is false for a project that is not suspended', () => {
    expect(isSuspendedProject(project(1, 'p'))).toBe(false);
  });

  it('reads only `suspended`, so an extra wire field cannot flip it', () => {
    // The server sends `suspended: false` and no `status`. A stale reader
    // that still branched on a `status` key would answer true here.
    const wireProject = { id: 1, name: 'p', suspended: false, status: 'suspended' } as Project;
    expect(isSuspendedProject(wireProject)).toBe(false);
  });
});

describe('sortProjectsByName', () => {
  it('sorts case-insensitively', () => {
    const projects = [project(1, 'zeta'), project(2, 'Alpha')];
    expect(sortProjectsByName(projects).map((p) => p.id)).toEqual([2, 1]);
  });

  it('does not mutate the input', () => {
    const projects = [project(1, 'b'), project(2, 'a')];
    const copy = [...projects];
    sortProjectsByName(projects);
    expect(projects).toEqual(copy);
  });
});

describe('isPersonalProjectName', () => {
  it('accepts the reserved storage name', () => {
    expect(isPersonalProjectName('project_user_1')).toBe(true);
    expect(isPersonalProjectName('project_user_90101')).toBe(true);
  });

  /*
   * The case this predicate exists for: `GET /social/author` answers
   * `personal_project_id` with an ORDINARY project id whenever the caller has
   * no `project_user_<uid>` row (`resolvePersonalProjectID`'s third branch).
   * A reader that trusted the id alone would call this project personal.
   */
  it('rejects an ordinary project that merely holds the personal id', () => {
    expect(isPersonalProjectName('Default Project')).toBe(false);
    expect(isPersonalProjectName('promptlib_public')).toBe(false);
  });

  it('is anchored, so a name that only contains the prefix does not match', () => {
    expect(isPersonalProjectName('shared project_user_2')).toBe(false);
    expect(isPersonalProjectName('project_user_2 archive')).toBe(false);
    expect(isPersonalProjectName('project_user_two')).toBe(false);
    expect(isPersonalProjectName('project_user_')).toBe(false);
  });
});
