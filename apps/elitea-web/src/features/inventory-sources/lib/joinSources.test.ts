/**
 * The rows the sources table renders, joined from two documents that share no
 * key.
 *
 * Both directions of the join are load-bearing and both fail SILENTLY — the
 * screen reads perfectly and says something false. Drop a configured source
 * that has no status and the "Add source" the user just did appears to have
 * failed. Drop a reported status that no configured source claims and the user
 * is left with entities in the graph they cannot account for and no control
 * that removes them.
 *
 * The count fields carry the same trap: `null` means "the provider did not
 * say", and a row that reported 0 instead would describe an ingested source as
 * having found nothing.
 */
import { describe, expect, it } from 'vitest';

import { joinSources, type ConfiguredSource, type ReportedStatus } from './joinSources';

function configured(overrides: Partial<ConfiguredSource> = {}): ConfiguredSource {
  return {
    toolkitId: '9010',
    name: 'Checkout repo',
    type: 'github',
    branch: 'main',
    filePatterns: '**/*.py',
    excludePatterns: '',
    preset: 'code',
    ...overrides,
  };
}

function reported(overrides: Partial<ReportedStatus> = {}): ReportedStatus {
  return {
    source: 'github:9010',
    status: 'completed',
    entityCount: 4,
    relationCount: 3,
    lastUpdated: '2026-09-01T10:00:00Z',
    errorMessage: '',
    ...overrides,
  };
}

describe('joinSources', () => {
  it('joins a configured source to the status reported under a different label', () => {
    const [row] = joinSources([configured()], [reported()]);
    expect(row).toEqual({
      toolkitId: '9010',
      name: 'Checkout repo',
      type: 'github',
      branch: 'main',
      filePatterns: '**/*.py',
      excludePatterns: '',
      preset: 'code',
      status: 'completed',
      entityCount: 4,
      relationCount: 3,
      lastUpdated: '2026-09-01T10:00:00Z',
      errorMessage: '',
    });
  });

  it('keeps a configured source that has never been ingested', () => {
    // The state a user is in immediately after adding one. Its status is ''
    // and the screen reads that as "not ingested yet".
    const [row] = joinSources([configured()], []);
    expect(row).toMatchObject({
      toolkitId: '9010',
      status: '',
      entityCount: null,
      relationCount: null,
      lastUpdated: '',
      errorMessage: '',
    });
  });

  it('keeps a reported status that no configured source claims', () => {
    // Its entities are still in the graph, and `remove_source_entities` is the
    // only thing that takes them out.
    const rows = joinSources([], [reported({ source: 'docs' })]);
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({ toolkitId: '', name: 'docs', type: '', status: 'completed' });
  });

  it('does not render a joined status twice', () => {
    const rows = joinSources([configured()], [reported()]);
    expect(rows).toHaveLength(1);
  });

  it('orders the configured sources first, in the order they were saved', () => {
    const rows = joinSources(
      [configured({ toolkitId: '9011', name: 'Docs' }), configured()],
      [reported({ source: 'orphan' })],
    );
    expect(rows.map((row) => row.name)).toEqual(['Docs', 'Checkout repo', 'orphan']);
  });

  it('names a source by its id when the toolkit listing has no label for it', () => {
    // A row a user cannot name is still a row they must be able to act on.
    const [row] = joinSources([configured({ name: '' })], []);
    expect(row?.name).toBe('9010');
  });

  it('carries an ingestion failure onto the row that failed', () => {
    const [row] = joinSources(
      [configured()],
      [reported({ status: 'error', errorMessage: 'branch not found', entityCount: null })],
    );
    expect(row).toMatchObject({ status: 'error', errorMessage: 'branch not found', entityCount: null });
  });

  it('renders nothing for a toolkit with no sources and no statuses', () => {
    expect(joinSources([], [])).toEqual([]);
  });
});
