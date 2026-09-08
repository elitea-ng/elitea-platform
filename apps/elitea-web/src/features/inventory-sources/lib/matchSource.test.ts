/**
 * Joining a configured source to the status the provider reports for it.
 *
 * The two sides share NO key. The toolkit names a source by its toolkit id;
 * the provider reports under whichever of three labels the ingestion ran with —
 * `github:9010`, the bare `9010`, or the SDK toolkit's short name `code`, which
 * is what the graph's citations carry. Match only one and every source shows as
 * never ingested on a graph full of entities, with nothing on the screen to say
 * that the JOIN rather than the ingestion is what failed.
 *
 * The `{type}:{id}` half-match is the other trap: comparing the whole label
 * against the type would give every github source in a project one shared
 * status.
 */
import { describe, expect, it } from 'vitest';

import { labelNamesSource, statusForSource } from './matchSource';

const SOURCE = { toolkitId: '9010', name: 'code', type: 'github' };

describe('labelNamesSource', () => {
  it('matches the bare id, from an ingestion that ran before the facade expanded anything', () => {
    expect(labelNamesSource('9010', SOURCE)).toBe(true);
  });

  it('matches the ID HALF of a {type}:{id} label', () => {
    expect(labelNamesSource('github:9010', SOURCE)).toBe(true);
  });

  it('matches the citation name, which is what the entity rows carry', () => {
    expect(labelNamesSource('code', SOURCE)).toBe(true);
  });

  it('does NOT match a different id of the same type', () => {
    // Otherwise every github source in the project shares one status.
    expect(labelNamesSource('github:9011', SOURCE)).toBe(false);
  });

  it('does not match the type on its own', () => {
    expect(labelNamesSource('github', SOURCE)).toBe(false);
  });

  it('ignores case and surrounding space, which the labels arrive with', () => {
    expect(labelNamesSource(' GitHub:9010 ', SOURCE)).toBe(true);
    expect(labelNamesSource('CODE', SOURCE)).toBe(true);
  });

  it('never matches on an empty name, which would claim every unnamed label', () => {
    expect(labelNamesSource('', SOURCE)).toBe(false);
    expect(labelNamesSource('   ', { toolkitId: '9010', name: '', type: '' })).toBe(false);
    expect(labelNamesSource('anything', { toolkitId: '9010', name: '', type: '' })).toBe(false);
  });

  it('reads the LAST colon, so a namespaced id still matches', () => {
    expect(labelNamesSource('github:acme:9010', SOURCE)).toBe(true);
  });
});

describe('statusForSource', () => {
  const statuses = [
    { source: 'github:9011', status: 'error' },
    { source: 'github:9010', status: 'completed' },
    { source: 'code', status: 'pending' },
  ];

  it('answers the status reported for that source', () => {
    expect(statusForSource(statuses, SOURCE)?.status).toBe('completed');
  });

  it('takes the FIRST match, in the provider own order', () => {
    // A source ingested twice has one row — the provider rewrites it — so a
    // second match is a provider defect, and preferring the last one hides it.
    expect(statusForSource(statuses, SOURCE)?.source).toBe('github:9010');
  });

  it('answers undefined for a source that has never been ingested', () => {
    // Which is not an error: it is the state a user is in right after adding
    // one, and the row still has to be rendered.
    expect(statusForSource(statuses, { toolkitId: '9012', name: 'docs', type: 'github' })).toBeUndefined();
    expect(statusForSource([], SOURCE)).toBeUndefined();
  });
});
