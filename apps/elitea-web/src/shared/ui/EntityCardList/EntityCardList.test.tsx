import { readFileSync } from 'node:fs';
import { basename, resolve } from 'node:path';

import { act, fireEvent } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '../lib/testTheme';
import { EntityCard } from './EntityCard';
import { EntityCardList } from './EntityCardList';
import { EntityEmptyState } from './EntityEmptyState';
import type { EmptyStateArt } from './EntityEmptyState';
import { EntityListPagination } from './EntityListPagination';
import type { EntityListItem } from './model';

const ITEMS: readonly EntityListItem[] = [
  { id: '1', name: 'Meeting Prep Assistant', description: 'Prepares a call', createdAt: '2026-03-14T10:00:00Z' },
  { id: '2', name: 'Test Case Generator', description: 'Writes test cases', createdAt: '2026-03-15T10:00:00Z' },
];

describe('EntityCard', () => {
  it('renders the icon tile, the title, the author avatars and the tag chips', () => {
    const { getByTestId, getAllByTestId } = renderWithTheme(
      <EntityCard
        item={{
          id: '1',
          name: 'super-pipe',
          icon: <svg data-testid="glyph" />,
          authors: [{ id: 'a', name: 'Alexander Kharkevich' }],
          tags: [{ name: 'tag' }, { name: 'no1tag' }],
        }}
      />,
    );
    expect(getByTestId('entity-card-icon')).toBeInTheDocument();
    expect(getByTestId('entity-card-name')).toHaveTextContent('super-pipe');
    expect(getAllByTestId('entity-card-author-avatar')).toHaveLength(1);
    expect(getAllByTestId('entity-card-tag-chip').map((chip) => chip.textContent)).toEqual(['tag', 'no1tag']);
  });

  it('separates the author cluster from the tag strip with a divider, and drops it when there are no tags', () => {
    const withTags = renderWithTheme(
      <EntityCard item={{ id: '1', name: 'a', authors: [{ name: 'A' }], tags: [{ name: 't' }] }} />,
    );
    expect(withTags.getAllByTestId('entity-card-section-divider')).toHaveLength(1);
    withTags.unmount();

    const withoutTags = renderWithTheme(<EntityCard item={{ id: '2', name: 'b', authors: [{ name: 'A' }], tags: [] }} />);
    expect(withoutTags.queryAllByTestId('entity-card-section-divider')).toHaveLength(0);
  });

  it('caps the tag strip at two chips and counts the rest', () => {
    const { getAllByTestId, getByTestId } = renderWithTheme(
      <EntityCard item={{ id: '1', name: 'a', tags: [{ name: 'one' }, { name: 'two' }, { name: 'three' }] }} />,
    );
    expect(getAllByTestId('entity-card-tag-chip')).toHaveLength(2);
    expect(getByTestId('entity-card-tag-overflow')).toHaveTextContent('+1');
  });

  it('activates on click and on Enter', () => {
    const onClick = vi.fn();
    const { getByTestId } = renderWithTheme(<EntityCard item={{ id: '1', name: 'a', onClick }} />);
    fireEvent.click(getByTestId('entity-card'));
    fireEvent.keyDown(getByTestId('entity-card'), { key: 'Enter' });
    expect(onClick).toHaveBeenCalledTimes(2);
  });
});

describe('EntityCardList', () => {
  it('renders one card per item in the grid view', () => {
    const { getAllByTestId } = renderWithTheme(
      <EntityCardList
        items={ITEMS}
        view="cards"
      />,
    );
    expect(getAllByTestId('entity-card')).toHaveLength(2);
  });

  it('renders a real table (not a role-annotated div soup) in the table view', () => {
    const { getAllByTestId, container } = renderWithTheme(
      <EntityCardList
        items={ITEMS}
        view="table"
      />,
    );
    expect(container.querySelector('table')).toBeInTheDocument();
    expect(getAllByTestId('entity-list-row')).toHaveLength(2);
  });

  /**
   * DEFECT this closes. With no `view` prop the grid reads `?view=` off the
   * URL, and it used to read it at RENDER TIME only — so the value moved and
   * nothing re-read it. The toggle lives in the page header and subscribes to
   * the router; the list body does not, so on the agents and pipelines list
   * pages the button took `aria-pressed="true"`, the address bar carried
   * `view=table`, and the panel below kept drawing cards for as long as E2E
   * waited (J14e, J16d).
   *
   * Nothing else changes here: no prop, no parent state, no router. A history
   * change alone has to reach the rendering, because a history change alone is
   * what the product does.
   */
  it('follows `?view=` when the URL moves under it, with no other change', async () => {
    const initial = window.location.href;
    try {
      const { getAllByTestId, queryAllByTestId, findAllByTestId } = renderWithTheme(<EntityCardList items={ITEMS} />);
      expect(getAllByTestId('entity-card')).toHaveLength(2);

      act(() => {
        window.history.pushState(null, '', '?view=table');
      });

      expect(await findAllByTestId('entity-list-row')).toHaveLength(2);
      expect(queryAllByTestId('entity-card')).toHaveLength(0);

      act(() => {
        window.history.pushState(null, '', '?view=cards');
      });

      expect(await findAllByTestId('entity-card')).toHaveLength(2);
      expect(queryAllByTestId('entity-list-row')).toHaveLength(0);
    } finally {
      window.history.replaceState(null, '', initial);
    }
  });

  it('shows the empty state only when the list is genuinely empty — never while loading or erroring', () => {
    const empty = (
      <EntityEmptyState
        title="No agents yet"
        description="Create your first agent."
      />
    );
    const loading = renderWithTheme(
      <EntityCardList
        items={[]}
        isLoading
        emptyState={empty}
      />,
    );
    expect(loading.container.querySelector('[data-testid="entity-empty-state"]')).toBeNull();
    expect(loading.getAllByTestId('entity-card-skeleton').length).toBeGreaterThan(0);
    loading.unmount();

    const failed = renderWithTheme(
      <EntityCardList
        items={[]}
        isError
        errorMessage="Boom"
        emptyState={empty}
      />,
    );
    expect(failed.container.querySelector('[data-testid="entity-empty-state"]')).toBeNull();
    expect(failed.getByRole('alert')).toHaveTextContent('Boom');
    failed.unmount();

    const blank = renderWithTheme(
      <EntityCardList
        items={[]}
        emptyState={empty}
      />,
    );
    expect(blank.getByTestId('entity-empty-state')).toBeInTheDocument();
  });

  it('renders the pagination footer only once there are rows', () => {
    const pagination = { page: 0, pageSize: 20, total: 40, onPageChange: vi.fn(), onPageSizeChange: vi.fn() };
    const withRows = renderWithTheme(
      <EntityCardList
        items={ITEMS}
        pagination={pagination}
      />,
    );
    expect(withRows.getByTestId('entity-list-page-info')).toHaveTextContent('1 - 20 of 40');
    withRows.unmount();

    const withoutRows = renderWithTheme(
      <EntityCardList
        items={[]}
        pagination={pagination}
      />,
    );
    expect(withoutRows.container.querySelector('[data-testid="entity-list-pagination"]')).toBeNull();
  });
});

describe('EntityListPagination', () => {
  it('renders nothing at all when there is nothing to page through', () => {
    const { queryByTestId } = renderWithTheme(
      <EntityListPagination
        page={0}
        pageSize={20}
        total={0}
        onPageChange={vi.fn()}
        onPageSizeChange={vi.fn()}
      />,
    );
    expect(queryByTestId('entity-list-pagination')).not.toBeInTheDocument();
  });

  it('disables the arrow that would leave the range, and steps the page otherwise', () => {
    const onPageChange = vi.fn();
    const { getByRole } = renderWithTheme(
      <EntityListPagination
        page={0}
        pageSize={20}
        total={40}
        onPageChange={onPageChange}
        onPageSizeChange={vi.fn()}
      />,
    );
    expect(getByRole('button', { name: 'Previous page' })).toBeDisabled();
    fireEvent.click(getByRole('button', { name: 'Next page' }));
    expect(onPageChange).toHaveBeenCalledWith(1);
  });
});

describe('EntityEmptyState', () => {
  it('paints both illustration variants so the theme, not JavaScript, picks one', () => {
    const { container } = renderWithTheme(
      <EntityEmptyState
        art="skills"
        title="No skills yet"
        description="Create your first skill."
      />,
    );
    expect(container.querySelectorAll('img')).toHaveLength(2);
  });

  it('shows the Create CTA only when the caller can act on it', () => {
    const onCreateClick = vi.fn();
    const withCta = renderWithTheme(
      <EntityEmptyState
        title="No skills yet"
        description="…"
        onCreateClick={onCreateClick}
      />,
    );
    fireEvent.click(withCta.getByRole('button', { name: 'Create' }));
    expect(onCreateClick).toHaveBeenCalledTimes(1);
    withCta.unmount();

    const withoutCta = renderWithTheme(
      <EntityEmptyState
        title="No skills yet"
        description="…"
      />,
    );
    expect(withoutCta.container.querySelector('button')).toBeNull();
  });
});

const EMPTY_STATE_ARTS: readonly EmptyStateArt[] = ['applications', 'skills', 'credentials'];

/** The `<img>` pair (light first, dark second) an empty state paints for one art. */
function renderArt(art: EmptyStateArt): readonly HTMLImageElement[] {
  const { container } = renderWithTheme(
    <EntityEmptyState
      art={art}
      title="Nothing yet"
      description="Create the first one."
    />,
  );
  return [...container.querySelectorAll('img')];
}

/** Width and height out of a WebP header — VP8X, VP8 (lossy) and VP8L (lossless). */
function readWebpSize(file: string): { width: number; height: number } {
  const bytes = readFileSync(file);
  const chunk = bytes.toString('ascii', 12, 16);
  if (chunk === 'VP8X') return { width: bytes.readUIntLE(24, 3) + 1, height: bytes.readUIntLE(27, 3) + 1 };
  if (chunk === 'VP8 ') return { width: bytes.readUInt16LE(26) & 0x3fff, height: bytes.readUInt16LE(28) & 0x3fff };
  const bits = bytes.readUInt32LE(21);
  return { width: (bits & 0x3fff) + 1, height: ((bits >> 14) & 0x3fff) + 1 };
}

/**
 * Issue #819 — the empty-state illustration paints on the DEVICE grid.
 *
 * The illustrations were painted `width: 15rem; height: auto`, so the box took
 * its height from the source bitmap's ratio: 240 * 512 / 720 = 170.65625px.
 * Every box under it inherited that fraction — measured in the pinned visual
 * container, the heading reported `getBoundingClientRect().top = 361.40625`
 * and the description 401.40625, which at the suite's `deviceScaleFactor: 2`
 * are device rows 722.8125 and 802.8125. Layout was reproducible; how a
 * renderer rounds a 14px/400 glyph run onto a device row eight tenths away is
 * not, and `pipelines-list-empty` flipped between two captures that differ
 * ONLY over that text.
 *
 * jsdom runs no layout, so these assert the DECLARATION rather than the paint:
 * a whole-pixel box in both axes, stated as HTML attributes (which reserve it
 * before the WebP decodes) and matched to the source within half a pixel (a
 * re-encode that moved a source would otherwise letterbox silently).
 */
describe('EntityEmptyState — device grid', () => {
  it.each(EMPTY_STATE_ARTS)('declares an integer painted size for both %s variants', (art) => {
    const images = renderArt(art);
    expect(images).toHaveLength(2);
    for (const image of images) {
      const width = Number(image.getAttribute('width'));
      const height = Number(image.getAttribute('height'));
      expect(Number.isInteger(width)).toBe(true);
      expect(Number.isInteger(height)).toBe(true);
      // A whole CSS pixel is a whole device row at any integer scale factor.
      expect(width).toBe(240);
      expect(height).toBeGreaterThan(0);
    }
  });

  it.each(EMPTY_STATE_ARTS)('keeps the declared %s height within half a pixel of its source ratio', (art) => {
    for (const image of renderArt(art)) {
      const file = basename(new URL(image.src, 'http://localhost').pathname);
      const source = readWebpSize(resolve('src/assets/empty-states', file));
      const natural = (Number(image.getAttribute('width')) * source.height) / source.width;
      expect(Math.abs(Number(image.getAttribute('height')) - natural)).toBeLessThanOrEqual(0.5);
    }
  });
});
