import { parseSurfaceLink, routeForSurfaceLink } from '@/features/surfaces/surfaceLinks';
import { buildSnapshot, sameContent, signedOutSnapshot, type SnapshotInput } from '@/features/surfaces/surfaceSnapshot';

const NOW = Date.UTC(2026, 9, 6, 12);
const input = (over: Partial<SnapshotInput> = {}): SnapshotInput => ({
  workspaceId: 'aaaaaaaaaaaa',
  projectId: 90501,
  origin: 'https://acme-legal.elitea.example',
  allowTitles: false,
  approvals: [
    { ref: '0b9c5d2e-1f3a-4c6b-8d7e-9f0a1b2c3d4e', name: 'Contract review: Acme v. Globex' },
    { ref: '42', name: 'Quarterly numbers' },
  ],
  unread: 3,
  running: 1,
  now: NOW,
  ...over,
});

describe('buildSnapshot', () => {
  it('carries COUNTS only by default: no title, no host (allow_system_surfaces off)', () => {
    const snap = buildSnapshot(input());
    expect(snap).toMatchObject({ signedIn: true, approvals: 2, unread: 3, running: 1, host: '', titles: [] });
    expect(JSON.stringify(snap)).not.toMatch(/Acme|Quarterly|acme-legal/);
  });

  it('names the waiting conversations and the host only when the policy allows titles', () => {
    const snap = buildSnapshot(input({ allowTitles: true }));
    expect(snap.titles).toEqual(['Contract review: Acme v. Globex', 'Quarterly numbers']);
    expect(snap.host).toBe('acme-legal.elitea.example');
  });

  it('a single waiting conversation is opened directly; several open the list', () => {
    expect(buildSnapshot(input({ approvals: [{ ref: '42', name: 'x' }] })).approvalsUrl).toBe('agentzefir://surface/c/42?ws=aaaaaaaaaaaa&p=90501');
    expect(buildSnapshot(input()).approvalsUrl).toBe('agentzefir://surface/home?ws=aaaaaaaaaaaa');
    expect(buildSnapshot(input()).notificationsUrl).toBe('agentzefir://surface/notifications?ws=aaaaaaaaaaaa');
  });

  it("the waiting conversation's link names its project: after a project change it opens the start, never another project's thread", () => {
    // The widget keeps showing the last snapshot until the next project's store is read (and
    // WidgetKit redraws later still); a numeric conversation id means another conversation there.
    const url = buildSnapshot(input({ approvals: [{ ref: '7', name: 'x' }] })).approvalsUrl;
    const link = parseSurfaceLink(url)!;
    expect(routeForSurfaceLink(link, 'aaaaaaaaaaaa', true, 90501)).toBe('/c/7');
    expect(routeForSurfaceLink(link, 'aaaaaaaaaaaa', true, 90504)).toBe('/');
  });

  it('clamps counts and trims titles to one bounded line', () => {
    const snap = buildSnapshot(input({ unread: -4, running: Number.NaN, allowTitles: true, approvals: [{ ref: '1', name: `  ${'a'.repeat(80)}\n x ` }, { ref: '2', name: '   ' }] }));
    expect(snap.unread).toBe(0);
    expect(snap.running).toBe(0);
    expect(snap.titles).toHaveLength(1);
    expect(snap.titles[0]).toHaveLength(48);
  });

  it('the signed-out snapshot holds nothing of a workspace', () => {
    const out = signedOutSnapshot(NOW);
    expect(out).toMatchObject({ signedIn: false, approvals: 0, unread: 0, running: 0, host: '', titles: [] });
    expect(JSON.stringify(out)).not.toContain('ws=');
  });

  it('sameContent ignores the timestamp only', () => {
    expect(sameContent(buildSnapshot(input()), buildSnapshot(input({ now: NOW + 5000 })))).toBe(true);
    expect(sameContent(buildSnapshot(input()), buildSnapshot(input({ unread: 4 })))).toBe(false);
    expect(sameContent(null, buildSnapshot(input()))).toBe(false);
  });

  it('holds no null anywhere: UserDefaults stores none and the native widget module refuses one', () => {
    const hasNull = (v: unknown): boolean =>
      v === null || (typeof v === 'object' && Object.values(v as object).some(hasNull));
    for (const snap of [buildSnapshot(input()), buildSnapshot(input({ allowTitles: true, origin: null })), signedOutSnapshot(NOW)]) {
      expect(hasNull(snap)).toBe(false);
    }
  });
});
