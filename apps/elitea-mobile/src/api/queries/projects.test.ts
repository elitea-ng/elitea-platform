import { createApiClient } from '@/api/client';
import type { BaseFetch } from '@/api/middleware';
import { fetchProjects, groupProjects, resolveProjectSelection, selectableProjects, type Project } from '@/api/queries/projects';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const ORIGIN = 'https://ai.example.com';
const P = (id: number, suspended = false): Project => ({ id, name: `p${id}`, suspended });

describe('resolveProjectSelection', () => {
  const projects = [P(3, true), P(5), P(7)];

  it('keeps a valid selection', () => {
    expect(resolveProjectSelection(projects, 7)).toBe(7);
  });

  it('falls back to the first non-suspended project when the id is unknown', () => {
    expect(resolveProjectSelection(projects, 99)).toBe(5);
  });

  it('falls back when the selected project is suspended', () => {
    expect(resolveProjectSelection(projects, 3)).toBe(5);
  });

  it('falls back when nothing is selected', () => {
    expect(resolveProjectSelection(projects, null)).toBe(5);
  });

  it('returns null when no project is usable', () => {
    expect(resolveProjectSelection([P(3, true)], 3)).toBeNull();
    expect(resolveProjectSelection([], null)).toBeNull();
  });

  it('falls back to the personal project before the first one', () => {
    expect(resolveProjectSelection(projects, null, 7)).toBe(7);
    expect(resolveProjectSelection(projects, 99, 7)).toBe(7);
    // A kept selection wins over the personal project.
    expect(resolveProjectSelection(projects, 5, 7)).toBe(5);
    // A personal project not in the list (or suspended) is not chosen.
    expect(resolveProjectSelection(projects, null, 3)).toBe(5);
    expect(resolveProjectSelection(projects, null, 42)).toBe(5);
  });

  it('hides suspended projects from the switcher', () => {
    expect(selectableProjects(projects).map((p) => p.id)).toEqual([5, 7]);
  });
});

describe('groupProjects', () => {
  // listProjects rows (ProjectWithGroups, client contract 1.2) after fetchProjects, with the
  // personal project named by getCurrentAuthor's personal_project_id.
  const list = [P(1), P(7), P(12), P(13, true)];

  it('puts the personal project first and the usable team projects after it', () => {
    expect(groupProjects(list, 7)).toEqual({ personal: P(7), team: [P(1), P(12)] });
  });

  it('lists everything as team projects until the personal project is known', () => {
    expect(groupProjects(list, null)).toEqual({ personal: null, team: [P(1), P(7), P(12)] });
    expect(groupProjects(list, 99)).toEqual({ personal: null, team: [P(1), P(7), P(12)] });
  });
});

describe('fetchProjects', () => {
  beforeEach(() => {
    __resetSessionForTests();
    setSession({ hydrated: true, serverOrigin: ORIGIN, projectId: 1 });
    setCredential({ kind: 'pat', token: 't' });
  });

  it('reads the list route and keeps id, name and suspended only', async () => {
    const urls: string[] = [];
    const fetch: BaseFetch = async (input) => {
      urls.push(input.url);
      return new Response(
        JSON.stringify([
          { id: 2, name: 'Alpha', suspended: false, owner_id: 1, plugins: [], groups: [] },
          { id: 0, name: 'bad id' },
          { id: 4, name: 'Beta', suspended: true },
        ]),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      );
    };
    const client = createApiClient({ origin: ORIGIN, auth: { kind: 'session' }, fetch });
    await expect(fetchProjects(client)).resolves.toEqual([
      { id: 2, name: 'Alpha', suspended: false },
      { id: 4, name: 'Beta', suspended: true },
    ]);
    expect(urls).toEqual([`${ORIGIN}/api/v2/projects/project/default/1`]);
  });
});
