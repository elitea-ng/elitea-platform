import { parseSurfaceLink, routeForSurfaceLink, surfaceUrl } from '@/features/surfaces/surfaceLinks';
import { rewriteSystemPath } from '@/linking/nativeIntent';

const WS = 'aaaaaaaaaaaa';

describe('surface links', () => {
  it('builds elitea://surface links that name the workspace slot, never the host', () => {
    expect(surfaceUrl({ kind: 'notifications' }, WS)).toBe('elitea://surface/notifications?ws=aaaaaaaaaaaa');
    expect(surfaceUrl({ kind: 'conversation', ref: '0b9c5d2e-1f3a-4c6b-8d7e-9f0a1b2c3d4e' }, WS)).toBe(
      'elitea://surface/c/0b9c5d2e-1f3a-4c6b-8d7e-9f0a1b2c3d4e?ws=aaaaaaaaaaaa',
    );
    // A ref that is not a route reference falls back to the start.
    expect(surfaceUrl({ kind: 'conversation', ref: '../settings' }, WS)).toBe('elitea://surface/home?ws=aaaaaaaaaaaa');
  });

  it('reads the full URL and the path form expo-router hands over', () => {
    expect(parseSurfaceLink('elitea://surface/notifications?ws=aaaaaaaaaaaa')).toEqual({ href: '/notifications', workspaceId: WS, projectId: null });
    expect(parseSurfaceLink('/surface/c/42?ws=aaaaaaaaaaaa')).toEqual({ href: '/c/42', workspaceId: WS, projectId: null });
    expect(parseSurfaceLink('elitea://surface/new')).toEqual({ href: '/new', workspaceId: null, projectId: null });
    expect(parseSurfaceLink('elitea://surface/home?ws=aaaaaaaaaaaa')).toEqual({ href: '/', workspaceId: WS, projectId: null });
  });

  it('opens nothing it does not know: unknown targets go to the start, other links are not surface links', () => {
    expect(parseSurfaceLink('elitea://surface/settings?ws=aaaaaaaaaaaa')?.href).toBe('/');
    expect(parseSurfaceLink('elitea://surface/c/..%2Fsettings')?.href).toBe('/');
    expect(parseSurfaceLink('elitea://connect?server=https%3A%2F%2Fai.example.com')).toBeNull();
    expect(parseSurfaceLink('https://evil.example/surface/notifications')).toBeNull();
  });

  it('a link built for another workspace opens the start of the active one', () => {
    const link = parseSurfaceLink('elitea://surface/notifications?ws=bbbbbbbbbbbb')!;
    expect(routeForSurfaceLink(link, WS, true)).toBe('/');
    expect(routeForSurfaceLink(link, 'bbbbbbbbbbbb', true)).toBe('/notifications');
    // Cold launch: the session is not restored yet; the screen opens (behind the lock).
    expect(routeForSurfaceLink(link, null, false)).toBe('/notifications');
  });
});

describe('surface links name the project of a conversation', () => {
  const REF = '0b9c5d2e-1f3a-4c6b-8d7e-9f0a1b2c3d4e';
  it('builds and reads the project', () => {
    const url = surfaceUrl({ kind: 'conversation', ref: REF, projectId: 7 }, WS);
    expect(url).toBe(`elitea://surface/c/${REF}?ws=aaaaaaaaaaaa&p=7`);
    expect(parseSurfaceLink(url)).toEqual({ href: `/c/${REF}`, workspaceId: WS, projectId: 7 });
    expect(parseSurfaceLink(`elitea://surface/c/${REF}?ws=aaaaaaaaaaaa&p=x7`)?.projectId).toBeNull();
  });

  it('a conversation link of another project (a project change since) opens the start', () => {
    const link = parseSurfaceLink(surfaceUrl({ kind: 'conversation', ref: REF, projectId: 7 }, WS))!;
    expect(routeForSurfaceLink(link, WS, true, 7)).toBe(`/c/${REF}`);
    expect(routeForSurfaceLink(link, WS, true, 9)).toBe('/');
    expect(routeForSurfaceLink(link, WS, true, null)).toBe('/');
    const session = () => ({ workspaceId: WS, hydrated: true, projectId: 9 });
    expect(rewriteSystemPath(`elitea://surface/c/${REF}?ws=aaaaaaaaaaaa&p=7`, false, session)).toBe('/');
  });
});

describe('rewriteSystemPath with surface links', () => {
  it('routes a widget tap to its screen for the active workspace', () => {
    const session = () => ({ workspaceId: WS, hydrated: true });
    expect(rewriteSystemPath('elitea://surface/notifications?ws=aaaaaaaaaaaa', false, session)).toBe('/notifications');
    expect(rewriteSystemPath('elitea://surface/c/42?ws=bbbbbbbbbbbb', false, session)).toBe('/');
  });
});
