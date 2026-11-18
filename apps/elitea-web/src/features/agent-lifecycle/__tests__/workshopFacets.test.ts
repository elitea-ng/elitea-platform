import { describe, expect, it } from 'vitest';

import type { Application } from '@/shared/api/generated/model';

import { originBadgeLabel, statusBadgeLabel, workshopFacets, workshopRowBadge } from '../lib/workshopFacets';

describe('workshopFacets', () => {
  it('offers only the values that occur, with the server counts', () => {
    const [status, origin] = workshopFacets({ statuses: { draft: 4, published: 0 }, origins: { own: 4 } });
    expect(status?.options).toEqual([{ value: 'draft', label: 'Draft', count: 4 }]);
    expect(origin?.options).toEqual([{ value: 'own', label: 'Own', count: 4 }]);
  });

  it('offers nothing before the list has answered, and never the three statuses no agent can hold', () => {
    for (const facet of workshopFacets(undefined)) expect(facet.options).toEqual([]);
    const [status] = workshopFacets({ statuses: { draft: 1, published: 2 }, origins: {} });
    expect(status?.options.map((option) => option.value)).toEqual(['draft', 'published']);
  });

  it('keeps a status the server sends that this build does not know, by its own name', () => {
    const [status] = workshopFacets({ statuses: { retired: 2, draft: 1 }, origins: {} });
    expect(status?.options.map((option) => option.label)).toEqual(['Draft', 'retired']);
  });

  it('names the status filter param and its array shape', () => {
    const [status, origin] = workshopFacets({ statuses: {}, origins: {} });
    expect(status).toMatchObject({ param: 'statuses', array: true });
    expect(origin?.param).toBe('origin');
  });
});

describe('statusBadgeLabel', () => {
  it('says the status as a word, and nothing for a row without one', () => {
    expect(statusBadgeLabel('published')).toBe('Published');
    expect(statusBadgeLabel('')).toBeUndefined();
    expect(statusBadgeLabel(undefined)).toBeUndefined();
  });
});

function row(id: string, name: string, meta: Record<string, unknown>): Application {
  return { id, name, status: 'draft', meta } as unknown as Application;
}

describe('installed origin and the update quick filter', () => {
  it('offers Installed in order and Update available only when one exists', () => {
    const [, origin, updates] = workshopFacets({ statuses: {}, origins: { forked: 1, installed: 2, own: 3 }, update_available: 2 });
    expect(origin?.options.map((option) => option.label)).toEqual(['Own', 'Installed', 'Forked']);
    expect(updates).toMatchObject({ param: 'update_available' });
    expect(updates?.options).toEqual([{ value: 'true', label: 'Update available', count: 2 }]);
    const [, , none] = workshopFacets({ statuses: {}, origins: {}, update_available: 0 });
    expect(none?.options).toEqual([]);
  });

  it('words the origin badge and the second hire of one listing', () => {
    const first = row('1', 'Changelog Writer', { origin_kind: 'installed', origin_listing_id: '7', origin_version_name: 'v2.3' });
    const second = row('2', 'Changelog Writer 2', { origin_kind: 'installed', origin_listing_id: '7', origin_version_name: 'v2.4' });
    const fork = row('3', 'Notes', { origin_kind: 'forked', origin_listing_name: 'Triage Helper', origin_version_name: 'v1.2' });
    expect(originBadgeLabel(first)).toBe('Installed v2.3');
    expect(originBadgeLabel(fork)).toBe('Forked from Triage Helper v1.2');
    expect(originBadgeLabel(row('4', 'Own', {}))).toBe('');
    const rows = [first, second, fork];
    expect(workshopRowBadge(first, rows)).toBe('Draft · Installed v2.3');
    expect(workshopRowBadge(second, rows)).toBe('Draft · Installed v2.4 · also hired as Changelog Writer');
  });

  it('says on the row that an automatic hire is on probation (#2091)', () => {
    const hired = row('1', 'Changelog Writer', { origin_kind: 'installed', origin_version_name: 'v2.3', on_probation: true, probation_until: '2026-11-15T00:00:00Z' });
    expect(workshopRowBadge(hired, [hired])).toBe('Draft · Installed v2.3 · On probation');
    const kept = row('2', 'Kept', { origin_kind: 'installed', origin_version_name: 'v2.3' });
    expect(workshopRowBadge(kept, [kept])).toBe('Draft · Installed v2.3');
  });
});
